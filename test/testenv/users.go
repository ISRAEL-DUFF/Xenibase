package testenv

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/auth"
)

// UserPassword is the password the harness gives every account it creates.
const UserPassword = "member test password"

// Client is another user's browser: its own cookies, CSRF token, and
// authenticator.
type Client struct {
	e      *Env
	client *http.Client
	csrf   string
	totp   string
	Email  string
	UserID uuid.UUID
	// OrgID is the user's personal organisation.
	OrgID uuid.UUID
}

func newClient(e *Env) *Client {
	jar, _ := cookiejar.New(nil)
	c := &Client{e: e, client: &http.Client{Jar: jar}}
	var st gen.SessionState
	if code := c.Do("GET", "/api/v1/session", nil, &st); code != http.StatusOK {
		e.t.Fatalf("session: %d", code)
	}
	c.csrf = st.CsrfToken
	return c
}

// Do sends a JSON request as this user; see Env.Do.
func (c *Client) Do(method, path string, body, out any) int {
	c.e.t.Helper()
	code, raw := c.DoRaw(method, path, body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			c.e.t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
		}
	}
	return code
}

// DoRaw is Do returning the body.
func (c *Client) DoRaw(method, path string, body any) (int, []byte) {
	c.e.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.e.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.e.URL+path, r)
	if err != nil {
		c.e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.csrf != "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	res, err := c.client.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

// TOTP returns a fresh code from the user's authenticator.
func (c *Client) TOTP() string {
	c.e.t.Helper()
	c.e.clock.advance()
	code, err := auth.TOTPCode(c.totp, c.e.clock.Now())
	if err != nil {
		c.e.t.Fatal(err)
	}
	return code
}

// Reauth performs step-up authentication as this user.
func (c *Client) Reauth() {
	c.e.t.Helper()
	if code := c.Do("POST", "/api/v1/auth/reauth", map[string]string{"password": UserPassword, "code": c.TOTP()}, nil); code != http.StatusNoContent {
		c.e.t.Fatalf("reauth as %s: %d", c.Email, code)
	}
}

// CreateProject creates a project in orgID as this user and waits for it.
func (c *Client) CreateProject(name string, orgID uuid.UUID) gen.ProjectCredentials {
	c.e.t.Helper()
	var pc gen.ProjectCredentials
	if code := c.Do("POST", "/api/v1/projects", map[string]any{"name": name, "org_id": orgID}, &pc); code != http.StatusAccepted {
		c.e.t.Fatalf("create %q as %s: status %d", name, c.Email, code)
	}
	if op := c.WaitOperation(pc.Operation.Id); op.Status != gen.OperationStatusSucceeded {
		c.e.t.Fatalf("create %q: %s\n%s", name, op.Status, FormatLog(op))
	}
	return pc
}

// WaitOperation is Env.WaitOperation as this user.
func (c *Client) WaitOperation(id uuid.UUID) gen.Operation {
	c.e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", c.e.URL+"/api/v1/operations/"+id.String()+"/stream", nil)
	res, err := c.client.Do(req)
	if err != nil {
		c.e.t.Fatalf("stream operation %s: %v", id, err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	var op gen.Operation
	if code := c.Do("GET", "/api/v1/operations/"+id.String(), nil, &op); code != http.StatusOK {
		c.e.t.Fatalf("get operation %s as %s: status %d", id, c.Email, code)
	}
	return op
}

var tokenRe = regexp.MustCompile(`token=([A-Za-z0-9_-]+)`)

// MailToken returns the link token in the latest email to addr whose body
// contains want.
func (e *Env) MailToken(addr, want string) string {
	e.t.Helper()
	msgs := e.SMTP.Mail()
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		for _, to := range m.To {
			if strings.EqualFold(to, addr) && strings.Contains(m.Data, want) {
				if t := tokenRe.FindStringSubmatch(m.Data); t != nil {
					return t[1]
				}
			}
		}
	}
	e.t.Fatalf("no email to %s mentioning %q", addr, want)
	return ""
}

// AcceptInvitation opens an invitation link as a new visitor, creates the
// account it invites, enrols an authenticator, and returns the signed-in
// user.
func (e *Env) AcceptInvitation(token, email string) *Client {
	e.t.Helper()
	c := newClient(e)
	c.Email = email
	var prev gen.InvitationPreview
	if code := c.Do("POST", "/api/v1/invitations/preview", map[string]string{"token": token}, &prev); code != http.StatusOK || prev.Email != email {
		e.t.Fatalf("preview invitation for %s: %d %+v", email, code, prev)
	}
	var terms gen.Terms
	if code := c.Do("GET", "/api/v1/terms", nil, &terms); code != http.StatusOK {
		e.t.Fatalf("terms: %d", code)
	}
	var res gen.AcceptInvitationResult
	if code := c.Do("POST", "/api/v1/invitations/accept", map[string]any{
		"token": token, "name": strings.Split(email, "@")[0], "password": UserPassword, "terms_version": terms.Version,
	}, &res); code != http.StatusOK || res.Login == nil || res.Login.Session == nil || res.Login.Session.User == nil {
		e.t.Fatalf("accept invitation for %s: %d %+v", email, code, res)
	}
	// An ordinary account signs in with a password alone; turn two-factor on the
	// way a user does (Account → Security), so the rest of the suite can step up with TOTP.
	var setup gen.TotpSetup
	if code := c.Do("POST", "/api/v1/me/totp/setup", map[string]string{"password": UserPassword}, &setup); code != http.StatusOK {
		e.t.Fatalf("two-factor setup for %s: %d", email, code)
	}
	c.totp = setup.TotpSecret
	var rc gen.RecoveryCodes
	if code := c.Do("POST", "/api/v1/me/totp/enable", map[string]string{"setup_token": setup.SetupToken, "code": c.TOTP()}, &rc); code != http.StatusOK || len(rc.Codes) != 10 {
		e.t.Fatalf("enable two-factor for %s: %d %+v", email, code, rc)
	}
	st := *res.Login.Session
	c.UserID = st.User.Id
	var list gen.OrgList
	if code := c.Do("GET", "/api/v1/orgs", nil, &list); code != http.StatusOK || len(list.Items) == 0 || !list.Items[0].Personal {
		e.t.Fatalf("orgs of %s: %d %+v", email, code, list)
	}
	c.OrgID = list.Items[0].Id
	return c
}

// InviteUser has the platform admin invite email to the platform and
// returns them signed in, with their personal organisation.
func (e *Env) InviteUser(email string) *Client {
	e.t.Helper()
	var in gen.InvitationCreated
	if code := e.Do("POST", "/api/v1/admin/invitations", map[string]string{"email": email}, &in); code != http.StatusCreated || !in.EmailSent {
		e.t.Fatalf("platform invitation for %s: %d %+v", email, code, in)
	}
	return e.AcceptInvitation(e.MailToken(email, "invitation"), email)
}

// SignIn signs c in again (password and TOTP), after its sessions ended
// (a platform role change ends them), returning the new client.
func (e *Env) SignIn(c *Client) *Client {
	e.t.Helper()
	n := newClient(e)
	n.Email, n.UserID, n.OrgID, n.totp = c.Email, c.UserID, c.OrgID, c.totp
	var ch gen.LoginChallenge
	if code := n.Do("POST", "/api/v1/auth/login", map[string]string{"email": c.Email, "password": UserPassword}, &ch); code != http.StatusOK {
		e.t.Fatalf("sign in %s: %d", c.Email, code)
	}
	var st gen.SessionState
	if code := n.Do("POST", "/api/v1/auth/totp", map[string]string{"challenge_id": *ch.ChallengeId, "code": n.TOTP()}, &st); code != http.StatusOK || !st.Authenticated {
		e.t.Fatalf("TOTP for %s: %d %+v", c.Email, code, st)
	}
	return n
}
