package edge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/projauth"
)

// The admin API (V4 §4.3), for the project's servers with the secret key:
// users listed, made, changed, banned, deleted and signed out; invitations
// and links.

type adminUser struct {
	Email        *string         `json:"email"`
	Password     *string         `json:"password"`
	EmailConfirm *bool           `json:"email_confirm"`
	UserMetadata json.RawMessage `json:"user_metadata"`
	AppMetadata  json.RawMessage `json:"app_metadata"`
	BanDuration  *string         `json:"ban_duration"`
}

func (e *Edge) admin(c *call, path string) {
	m := c.r.Method
	segs := strings.Split(path, "/")
	switch {
	case path == "users" && m == http.MethodGet:
		e.adminList(c)
	case path == "users" && m == http.MethodPost:
		e.adminCreate(c)
	case len(segs) == 2 && segs[0] == "users":
		id, err := uuid.Parse(segs[1])
		if err != nil {
			c.fail(http.StatusNotFound, "user_not_found", "no such user")
			return
		}
		switch m {
		case http.MethodGet:
			e.adminGet(c, id)
		case http.MethodPatch, http.MethodPut:
			e.adminUpdate(c, id)
		case http.MethodDelete:
			e.adminDelete(c, id)
		default:
			c.fail(http.StatusMethodNotAllowed, "method_not_allowed", "GET, PATCH or DELETE a user")
		}
	case len(segs) == 3 && segs[0] == "users" && segs[2] == "signout" && m == http.MethodPost:
		id, err := uuid.Parse(segs[1])
		if err != nil {
			c.fail(http.StatusNotFound, "user_not_found", "no such user")
			return
		}
		e.adminSignOut(c, id)
	case path == "invite" && m == http.MethodPost:
		e.adminInvite(c)
	case path == "generate-link" && m == http.MethodPost:
		e.adminGenerateLink(c)
	default:
		c.fail(http.StatusNotFound, "no_such_endpoint", "no such admin endpoint")
	}
}

func (e *Edge) adminList(c *call) {
	q := c.r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	per, _ := strconv.Atoi(q.Get("per_page"))
	if page < 1 {
		page = 1
	}
	if per < 1 || per > 1000 {
		per = 50
	}
	ctx := c.r.Context()
	var users []*projauth.User
	var total int
	err := e.withAuth(ctx, c.p, func(tx pgx.Tx) error {
		var err error
		users, total, err = projauth.ListUsers(ctx, tx, q.Get("q"), per, (page-1)*per)
		return err
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	if users == nil {
		users = []*projauth.User{}
	}
	c.json(http.StatusOK, map[string]any{"users": users, "total": total, "page": page, "per_page": per})
}

func (e *Edge) adminGet(c *call, id uuid.UUID) {
	ctx := c.r.Context()
	var u *projauth.User
	var ids []projauth.Identity
	err := e.withAuth(ctx, c.p, func(tx pgx.Tx) error {
		var err error
		if u, err = projauth.GetUser(ctx, tx, id, false); err != nil {
			return err
		}
		ids, err = projauth.Identities(ctx, tx, id)
		return err
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	c.json(http.StatusOK, map[string]any{"user": u, "identities": ids})
}

// adminFields checks a create or update body; hash is the new password's.
func (e *Edge) adminFields(c *call, in adminUser) (hash *string, ban *time.Time, ok bool) {
	if in.Email != nil && !validEmail(projauth.NormalizeEmail(*in.Email)) {
		c.fail(http.StatusBadRequest, "invalid_email", "a valid email address is required")
		return nil, nil, false
	}
	for _, b := range []json.RawMessage{in.UserMetadata, in.AppMetadata} {
		if b != nil && !strings.HasPrefix(strings.TrimSpace(string(b)), "{") {
			c.fail(http.StatusBadRequest, "invalid_body", "user_metadata and app_metadata are JSON objects")
			return nil, nil, false
		}
	}
	if in.BanDuration != nil {
		t, err := projauth.ParseBan(*in.BanDuration)
		if err != nil {
			c.fail(http.StatusBadRequest, "invalid_ban_duration", err.Error())
			return nil, nil, false
		}
		ban = &t
	}
	if in.Password != nil {
		if msg := passwordProblem(c.p.cfg.Auth, *in.Password); msg != "" {
			c.fail(http.StatusUnprocessableEntity, "weak_password", msg)
			return nil, nil, false
		}
		var h string
		var err error
		e.hash(func() { h, err = projauth.HashPassword(*in.Password) })
		if err != nil {
			e.dbError(c, err)
			return nil, nil, false
		}
		hash = &h
	}
	return hash, ban, true
}

func (e *Edge) adminCreate(c *call) {
	var in adminUser
	if !authBody(c, &in) {
		return
	}
	if in.Email == nil {
		c.fail(http.StatusBadRequest, "invalid_email", "email is required (phone users come with phone sign-in)")
		return
	}
	hash, ban, ok := e.adminFields(c, in)
	if !ok {
		return
	}
	ctx := c.r.Context()
	var u *projauth.User
	err := e.withAuth(ctx, c.p, func(tx pgx.Tx) error {
		n := projauth.NewUser{Email: *in.Email, EmailConfirmed: in.EmailConfirm != nil && *in.EmailConfirm,
			UserMetadata: in.UserMetadata, AppMetadata: in.AppMetadata}
		if hash != nil {
			n.PasswordHash = *hash
		}
		var err error
		if u, err = projauth.CreateUser(ctx, tx, n); err != nil {
			return err
		}
		if ban != nil {
			if u, err = projauth.UpdateUser(ctx, tx, u.ID, projauth.Update{Ban: ban}); err != nil {
				return err
			}
		}
		if err := projauth.EnsureEmailIdentity(ctx, tx, u.ID, *in.Email); err != nil {
			return err
		}
		return projauth.Audit(ctx, tx, &u.ID, projauth.ActSignup, c.ip, map[string]any{"by": "admin"})
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	c.json(http.StatusCreated, u)
}

func (e *Edge) adminUpdate(c *call, id uuid.UUID) {
	var in adminUser
	if !authBody(c, &in) {
		return
	}
	hash, ban, ok := e.adminFields(c, in)
	if !ok {
		return
	}
	ctx := c.r.Context()
	var u *projauth.User
	err := e.withAuth(ctx, c.p, func(tx pgx.Tx) error {
		up := projauth.Update{Email: in.Email, PasswordHash: hash, ConfirmEmail: in.EmailConfirm != nil && *in.EmailConfirm,
			UserMetadata: in.UserMetadata, AppMetadata: in.AppMetadata, Ban: ban}
		var err error
		if u, err = projauth.UpdateUser(ctx, tx, id, up); err != nil {
			return err
		}
		if in.Email != nil {
			if err := projauth.EnsureEmailIdentity(ctx, tx, id, *in.Email); err != nil {
				return err
			}
		}
		action := projauth.ActUpdated
		if ban != nil {
			action = projauth.ActBanned
			if ban.IsZero() {
				action = projauth.ActUnbanned
			}
		}
		return projauth.Audit(ctx, tx, &id, action, c.ip, map[string]any{"by": "admin"})
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	c.json(http.StatusOK, u)
}

func (e *Edge) adminDelete(c *call, id uuid.UUID) {
	ctx := c.r.Context()
	err := e.withAuth(ctx, c.p, func(tx pgx.Tx) error {
		if err := projauth.DeleteUser(ctx, tx, id); err != nil {
			return err
		}
		return projauth.Audit(ctx, tx, &id, projauth.ActDeleted, c.ip, map[string]any{"by": "admin"})
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	c.w.WriteHeader(http.StatusNoContent)
}

func (e *Edge) adminSignOut(c *call, id uuid.UUID) {
	ctx := c.r.Context()
	var n int64
	err := e.withAuth(ctx, c.p, func(tx pgx.Tx) error {
		if _, err := projauth.GetUser(ctx, tx, id, false); err != nil {
			return err
		}
		var err error
		if n, err = projauth.EndSessions(ctx, tx, id, nil); err != nil {
			return err
		}
		return projauth.Audit(ctx, tx, &id, projauth.ActAdminSignOut, c.ip, map[string]any{"sessions": n})
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	c.json(http.StatusOK, map[string]any{"sessions_ended": n})
}

// linkFor makes a code of kind for email (creating an invited user when
// asked) and returns the user and the code. password is the hash of the
// password a sign-up link sets ("" for none): it replaces the password of
// an unconfirmed user, which whoever signed up first may have set.
func linkFor(ctx context.Context, tx pgx.Tx, kind, email string, createInvited bool, password string, data json.RawMessage) (*projauth.User, projauth.Code, error) {
	u, err := projauth.UserByEmail(ctx, tx, email, true)
	if errors.Is(err, projauth.ErrNotFound) && createInvited {
		u, err = projauth.CreateUser(ctx, tx, projauth.NewUser{Email: email, Invited: kind == projauth.CodeInvite, PasswordHash: password,
			UserMetadata: data})
		if err == nil {
			err = projauth.EnsureEmailIdentity(ctx, tx, u.ID, email)
		}
	} else if err == nil && password != "" && u.EmailConfirmedAt == nil && u.PhoneConfirmedAt == nil {
		if u, err = projauth.UpdateUser(ctx, tx, u.ID, projauth.Update{PasswordHash: &password}); err == nil {
			_, err = projauth.EndSessions(ctx, tx, u.ID, nil)
		}
	}
	if err != nil {
		return nil, projauth.Code{}, err
	}
	ttl := projauth.CodeTTL
	if kind == projauth.CodeInvite {
		ttl = projauth.InviteTTL
	}
	// An invitation (or a sign-up link) keeps only the password this
	// request set: one already on an unconfirmed row is cleared when the
	// link is used (useVerified).
	if password != "" {
		code, err := projauth.NewPasswordCode(ctx, tx, u.ID, kind, email, ttl, password)
		return u, code, err
	}
	code, err := projauth.NewCode(ctx, tx, u.ID, kind, email, ttl)
	return u, code, err
}

func (e *Edge) adminInvite(c *call) {
	var in struct {
		Email      string          `json:"email"`
		Data       json.RawMessage `json:"data"`
		RedirectTo string          `json:"redirect_to"`
	}
	if !authBody(c, &in) {
		return
	}
	email := projauth.NormalizeEmail(in.Email)
	if !validEmail(email) {
		c.fail(http.StatusBadRequest, "invalid_email", "a valid email address is required")
		return
	}
	redirect, ok := redirectFor(c.p.cfg.Auth, in.RedirectTo)
	if !ok {
		c.fail(http.StatusBadRequest, "redirect_not_allowed", "redirect_to isn't one of the project's redirect URLs")
		return
	}
	ctx := c.r.Context()
	var u *projauth.User
	err := e.withAuth(ctx, c.p, func(tx pgx.Tx) error {
		if existing, err := projauth.UserByEmail(ctx, tx, email, false); err == nil && existing.EmailConfirmedAt != nil {
			return projauth.ErrExists
		}
		var code projauth.Code
		var err error
		if u, code, err = linkFor(ctx, tx, projauth.CodeInvite, email, true, "", in.Data); err != nil {
			return err
		}
		if err := projauth.Audit(ctx, tx, &u.ID, projauth.ActInvited, c.ip, map[string]any{"by": "admin"}); err != nil {
			return err
		}
		return e.sendEmail(ctx, c.p, edgeapi.EmailInvite, "invite", email, code, redirect)
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	c.json(http.StatusOK, u)
}

// adminGenerateLink makes a link without sending it (V4 §4.3), for a
// project that sends its own emails.
func (e *Edge) adminGenerateLink(c *call) {
	var in struct {
		Type       string          `json:"type"`
		Email      string          `json:"email"`
		Password   string          `json:"password"`
		Data       json.RawMessage `json:"data"`
		RedirectTo string          `json:"redirect_to"`
	}
	if !authBody(c, &in) {
		return
	}
	email := projauth.NormalizeEmail(in.Email)
	if !validEmail(email) {
		c.fail(http.StatusBadRequest, "invalid_email", "a valid email address is required")
		return
	}
	kind := map[string]string{"magiclink": projauth.CodeMagicLink, "invite": projauth.CodeInvite, "recovery": projauth.CodeRecovery,
		"signup": projauth.CodeSignup}[in.Type]
	if kind == "" {
		c.fail(http.StatusBadRequest, "invalid_type", "type is magiclink, invite, recovery or signup")
		return
	}
	redirect, ok := redirectFor(c.p.cfg.Auth, in.RedirectTo)
	if !ok {
		c.fail(http.StatusBadRequest, "redirect_not_allowed", "redirect_to isn't one of the project's redirect URLs")
		return
	}
	var hash string
	if kind == projauth.CodeSignup {
		if msg := passwordProblem(c.p.cfg.Auth, in.Password); msg != "" {
			c.fail(http.StatusUnprocessableEntity, "weak_password", msg)
			return
		}
		var herr error
		e.hash(func() { hash, herr = projauth.HashPassword(in.Password) })
		if herr != nil {
			e.dbError(c, herr)
			return
		}
	}
	ctx := c.r.Context()
	var u *projauth.User
	var code projauth.Code
	err := e.withAuth(ctx, c.p, func(tx pgx.Tx) error {
		var err error
		u, code, err = linkFor(ctx, tx, kind, email, kind == projauth.CodeInvite || kind == projauth.CodeSignup, hash, in.Data)
		return err
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	q := url.Values{"token": {code.Token}, "type": {in.Type}}
	if redirect != "" {
		q.Set("redirect_to", redirect)
	}
	c.json(http.StatusOK, map[string]any{
		"action_link": "https://" + c.p.cfg.Ref + "." + e.cfg.Domain + "/auth/v1/verify?" + q.Encode(),
		"email_otp":   code.Code, "hashed_token": code.Token, "verification_type": in.Type, "redirect_to": redirect, "user": u,
	})
}
