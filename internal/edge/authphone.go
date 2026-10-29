package edge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/messaging"
	"github.com/israel-duff/xenibase/internal/projauth"
)

// Phone sign-in (V4 §4.1): codes by SMS or WhatsApp to numbers in the
// project's allowed countries. xenibase-server applies the per-number and
// daily caps when it queues a code.

// phoneEvery is how often one number may be sent a code from here (the
// server's per-number hourly limit is the other half).
const phoneEvery = 60 * time.Second

// phoneIn normalises raw and checks the project allows its country; false
// means c was answered.
func phoneIn(c *call, raw string) (string, bool) {
	phone, err := messaging.Normalize(raw)
	if err != nil {
		c.fail(http.StatusBadRequest, "invalid_phone", err.Error())
		return "", false
	}
	if !countryAllowed(c.p.cfg.Auth, phone) {
		c.fail(http.StatusForbidden, "phone_country_not_allowed", "this project doesn't send codes to numbers in this country")
		return "", false
	}
	return phone, true
}

func countryAllowed(a edgeapi.AuthConfig, phone string) bool {
	countries := a.PhoneCountries
	if len(countries) == 0 {
		countries = []string{"NG"}
	}
	return slices.Contains(countries, "*") || slices.Contains(countries, messaging.Country(phone))
}

// channelIn is the channel a code goes by (sms by default), if the
// project has it on; false means c was answered.
func channelIn(c *call, ch string) (string, bool) {
	if ch == "" {
		ch = edgeapi.ChannelSMS
	}
	if ch != edgeapi.ChannelSMS && ch != edgeapi.ChannelWhatsApp {
		c.fail(http.StatusBadRequest, "invalid_channel", "channel is sms or whatsapp")
		return "", false
	}
	if !c.p.cfg.Auth.HasChannel(ch) {
		c.fail(http.StatusForbidden, "phone_provider_disabled", fmt.Sprintf("sign-in by %s isn't turned on for this project", ch))
		return "", false
	}
	return ch, true
}

// phoneOn reports whether phone sign-in is on at all.
func phoneOn(c *call) bool {
	if len(c.p.cfg.Auth.PhoneChannels) == 0 {
		c.fail(http.StatusForbidden, "phone_provider_disabled", "phone sign-in isn't turned on for this project")
		return false
	}
	return true
}

// sendPhone asks xenibase-server to send code to phone by channel; its
// refusals come back as *apiErr, so the transaction that made the code
// rolls back.
func (e *Edge) sendPhone(ctx context.Context, p *project, channel, kind, phone string, code projauth.Code) error {
	err := e.client.SendAuthMessage(ctx, edgeapi.AuthMessage{Ref: p.cfg.Ref, Channel: channel, Kind: kind, To: phone, Code: code.Code})
	var se *edgeapi.StatusError
	if !errors.As(err, &se) {
		return err
	}
	var body struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal([]byte(se.Body), &body)
	switch se.Status {
	case http.StatusTooManyRequests:
		a := refuse(http.StatusTooManyRequests, "over_sms_send_rate_limit", "too many codes sent; try again later")
		switch body.Message {
		case "number":
			a.msg = "too many codes were sent to this number; try again in an hour"
		case "daily":
			a.msg = "the project's daily limit on codes is reached; try again tomorrow"
		}
		a.retryAfter = 3600
		return a
	case http.StatusForbidden:
		return refuse(http.StatusForbidden, "phone_not_allowed", body.Message)
	case http.StatusServiceUnavailable:
		return refuse(http.StatusServiceUnavailable, "sms_send_failed", body.Message)
	}
	return err
}

// phoneTooSoon refuses a code for phone if one went out within phoneEvery.
func phoneTooSoon(ctx context.Context, tx pgx.Tx, phone string, kinds []string) (*apiErr, error) {
	last, err := projauth.LastCodeAt(ctx, tx, phone, kinds)
	if err != nil || last == nil {
		return nil, err
	}
	if wait := phoneEvery - time.Since(*last); wait > 0 {
		a := refuse(http.StatusTooManyRequests, "over_sms_send_rate_limit",
			fmt.Sprintf("a code was just sent to this number; wait %d seconds", int(wait.Seconds())+1))
		a.retryAfter = int(wait.Seconds()) + 1
		return a, nil
	}
	return nil, nil
}

// newPhoneCode makes and sends a code of kind to phone for user uid;
// passwordHash is the password the request set ("" for none).
func (e *Edge) newPhoneCode(ctx context.Context, c *call, tx pgx.Tx, uid uuid.UUID, kind, channel, phone, passwordHash string) error {
	var code projauth.Code
	var err error
	if passwordHash != "" {
		code, err = projauth.NewPasswordCode(ctx, tx, uid, kind, phone, projauth.CodeTTL, passwordHash)
	} else {
		code, err = projauth.NewCode(ctx, tx, uid, kind, phone, projauth.CodeTTL)
	}
	if err != nil {
		return err
	}
	msgKind := edgeapi.PhoneCode
	if kind == projauth.CodePhoneChange {
		msgKind = edgeapi.PhoneChange
	}
	return e.sendPhone(ctx, c.p, channel, msgKind, phone, code)
}

// signupPhone is sign-up with a number and password: with confirmation on,
// a code goes to the number and the answer is the same for a new or known
// number.
func (e *Edge) signupPhone(c *call, raw, channel, password string, data json.RawMessage) {
	if !phoneOn(c) {
		return
	}
	phone, ok := phoneIn(c, raw)
	if !ok {
		return
	}
	a := c.p.cfg.Auth
	if a.PhoneConfirm {
		if channel, ok = channelIn(c, channel); !ok {
			return
		}
	}
	if msg := passwordProblem(a, password); msg != "" {
		c.fail(http.StatusUnprocessableEntity, "weak_password", msg)
		return
	}
	var hash string
	var herr error
	e.hash(func() { hash, herr = projauth.HashPassword(password) })
	if herr != nil {
		e.dbError(c, herr)
		return
	}
	ctx := c.r.Context()
	var out *tokenResponse
	err := e.authTx(c, func(tx pgx.Tx) error {
		existing, err := projauth.UserByPhone(ctx, tx, phone, true)
		if err != nil && !errors.Is(err, projauth.ErrNotFound) {
			return err
		}
		if existing != nil {
			if !a.PhoneConfirm {
				return refuse(http.StatusUnprocessableEntity, "phone_exists", "a user with this phone number already exists")
			}
			if existing.PhoneConfirmedAt != nil {
				return nil
			}
			// Unconfirmed: as for an email address, this request's password
			// replaces whoever's signed up with the number before.
			set := ""
			if existing.EmailConfirmedAt == nil {
				if _, err := projauth.UpdateUser(ctx, tx, existing.ID, projauth.Update{PasswordHash: &hash}); err != nil {
					return err
				}
				if _, err := projauth.EndSessions(ctx, tx, existing.ID, nil); err != nil {
					return err
				}
				set = hash
			}
			if soon, err := phoneTooSoon(ctx, tx, phone, []string{projauth.CodePhoneSignup}); err != nil || soon != nil {
				return err
			}
			return e.newPhoneCode(ctx, c, tx, existing.ID, projauth.CodePhoneSignup, channel, phone, set)
		}
		u, err := e.createUser(ctx, c, tx, projauth.NewUser{Phone: phone, PasswordHash: hash, PhoneConfirmed: !a.PhoneConfirm,
			UserMetadata: data}, "phone")
		if err != nil {
			return err
		}
		if !a.PhoneConfirm {
			if err := projauth.EnsurePhoneIdentity(ctx, tx, u.ID, phone); err != nil {
				return err
			}
			t, err := e.startSession(ctx, c, tx, u, "password")
			out = &t
			return err
		}
		return e.newPhoneCode(ctx, c, tx, u.ID, projauth.CodePhoneSignup, channel, phone, hash)
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	if out != nil {
		c.json(http.StatusOK, out)
		return
	}
	c.json(http.StatusOK, map[string]any{"confirmation_sent": true})
}

// signinPhoneOTP sends a sign-in code to a number, creating the user when
// allowed; the answer is the same either way.
func (e *Edge) signinPhoneOTP(c *call, raw, channel string, create bool, data json.RawMessage) {
	if !phoneOn(c) {
		return
	}
	phone, ok := phoneIn(c, raw)
	if !ok {
		return
	}
	if channel, ok = channelIn(c, channel); !ok {
		return
	}
	a := c.p.cfg.Auth
	ctx := c.r.Context()
	err := e.authTx(c, func(tx pgx.Tx) error {
		if soon, err := phoneTooSoon(ctx, tx, phone, []string{projauth.CodePhone}); err != nil || soon != nil {
			if soon != nil {
				return soon
			}
			return err
		}
		u, err := projauth.UserByPhone(ctx, tx, phone, true)
		if errors.Is(err, projauth.ErrNotFound) {
			if !create || !a.SignupEnabled {
				return nil
			}
			if u, err = e.createUser(ctx, c, tx, projauth.NewUser{Phone: phone, UserMetadata: data}, "otp"); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if u.Banned(time.Now()) {
			return nil
		}
		return e.newPhoneCode(ctx, c, tx, u.ID, projauth.CodePhone, channel, phone, "")
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	c.json(http.StatusOK, map[string]any{"sent": true})
}

// usePhoneVerified signs in the user a phone code was for, applying what
// it proved (the number, or a new number).
func (e *Edge) usePhoneVerified(ctx context.Context, c *call, tx pgx.Tx, u *projauth.User, v projauth.Verified) (tokenResponse, *apiErr, error) {
	up := projauth.Update{ConfirmPhone: true}
	action := projauth.ActConfirmed
	if v.Kind == projauth.CodePhoneChange {
		up.Phone, up.NotAnonymous, action = &v.Target, true, projauth.ActPhoneChanged
	} else {
		if u.Phone == nil || *u.Phone != v.Target {
			return tokenResponse{}, refuse(http.StatusForbidden, "otp_expired", "the code is invalid or has expired"), nil
		}
		// A sign-in code proves the number for the first time: a password
		// set by whoever signed up with it unconfirmed doesn't come with it.
		// A sign-up code keeps only the password its own request set.
		drop := v.Kind == projauth.CodePhone || (v.Kind == projauth.CodePhoneSignup && !v.KeepsPassword(u))
		if drop && u.PhoneConfirmedAt == nil && u.HasPassword() && u.EmailConfirmedAt == nil {
			none := ""
			up.PasswordHash = &none
			if _, err := projauth.EndSessions(ctx, tx, u.ID, nil); err != nil {
				return tokenResponse{}, nil, err
			}
		}
	}
	u, err := projauth.UpdateUser(ctx, tx, u.ID, up)
	if errors.Is(err, projauth.ErrExists) {
		return tokenResponse{}, refuse(http.StatusUnprocessableEntity, "phone_exists", "another user has this phone number now"), nil
	}
	if err != nil {
		return tokenResponse{}, nil, err
	}
	if err := projauth.EnsurePhoneIdentity(ctx, tx, u.ID, v.Target); err != nil {
		return tokenResponse{}, nil, err
	}
	if err := projauth.Audit(ctx, tx, &u.ID, action, c.ip, map[string]any{"via": v.Kind}); err != nil {
		return tokenResponse{}, nil, err
	}
	method := "otp"
	if v.Kind == projauth.CodePhoneChange {
		method = "phone_change"
	}
	t, err := e.startSession(ctx, c, tx, u, method)
	return t, nil, err
}
