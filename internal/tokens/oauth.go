package tokens

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/store"
)

// OAuth 2.0 for third-party apps (RFC 6749 §4.1, the authorization code
// grant, with PKCE, RFC 7636, required of every client): a platform admin
// registers a client; a signed-in user consents on the web, choosing the
// organisation, scopes, projects and expiry as they would for a device
// login; the app exchanges the single-use code for an ordinary API token.
// Everything downstream (authz, rate limits, revocation, the Account
// page) is the token machinery above.

const (
	// ClientIDPrefix starts every client id.
	ClientIDPrefix = "xbapp_"
	// ClientSecretPrefix starts every client secret, so secret scanners can
	// find leaked ones.
	ClientSecretPrefix = "xbc_"
	// CodeExpiry is how long an authorization code may wait for its
	// exchange (RFC 6749 §4.1.2 recommends at most 10 minutes).
	CodeExpiry = 10 * time.Minute
	// MaxRedirectURIs a client may register.
	MaxRedirectURIs = 10

	// CodeChallengeMethod is the only PKCE method accepted.
	CodeChallengeMethod = "S256"
)

// Token endpoint errors (RFC 6749 §5.2).
var (
	ErrInvalidRequest = errors.New("invalid_request")
	ErrInvalidClient  = errors.New("invalid_client")
	ErrInvalidGrant   = errors.New("invalid_grant")
)

// verifierRe is RFC 7636 §4.1's code_verifier alphabet; a challenge is the
// base64url of a SHA-256, so it fits the same alphabet (43 characters).
var verifierRe = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// ---- Clients ------------------------------------------------------------------

// ClientParams registers a client.
type ClientParams struct {
	Name         string
	RedirectURIs []string
	// Scopes is the most the client may ask for; default read and write.
	Scopes []string
	// Confidential clients get a secret; public ones rely on PKCE alone.
	Confidential bool
	CreatedBy    *uuid.UUID
}

// ClientCreated is a new client: Secret is shown once (empty for a public
// client).
type ClientCreated struct {
	Secret string
	Row    store.OauthClient
}

// ValidateRedirectURI checks a registered redirect URI: absolute, https
// (http only on the loopback address, RFC 8252 §7.3), no fragment (RFC
// 6749 §3.1.2).
func ValidateRedirectURI(raw string) error {
	if raw == "" || len(raw) > 2000 {
		return fmt.Errorf("%w: a redirect URI is up to 2000 characters", ErrInvalid)
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("%w: %q is not an absolute URL", ErrInvalid, raw)
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return fmt.Errorf("%w: a redirect URI can't have a fragment", ErrInvalid)
	}
	host := u.Hostname()
	loopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && loopback:
	default:
		return fmt.Errorf("%w: %q must be https (http only on localhost)", ErrInvalid, raw)
	}
	return nil
}

// CreateClient registers a client.
func (s *Service) CreateClient(ctx context.Context, p ClientParams) (ClientCreated, error) {
	name := strings.TrimSpace(p.Name)
	if name == "" || len(name) > 100 {
		return ClientCreated{}, fmt.Errorf("%w: give the app a name of up to 100 characters", ErrInvalid)
	}
	if len(p.RedirectURIs) == 0 || len(p.RedirectURIs) > MaxRedirectURIs {
		return ClientCreated{}, fmt.Errorf("%w: give 1 to %d redirect URIs", ErrInvalid, MaxRedirectURIs)
	}
	uris := make([]string, 0, len(p.RedirectURIs))
	for _, u := range p.RedirectURIs {
		u = strings.TrimSpace(u)
		if err := ValidateRedirectURI(u); err != nil {
			return ClientCreated{}, err
		}
		if !slices.Contains(uris, u) {
			uris = append(uris, u)
		}
	}
	scopes := p.Scopes
	if len(scopes) == 0 {
		scopes = []string{ScopeRead, ScopeWrite}
	}
	scopes, err := NormalizeScopes(scopes)
	if err != nil {
		return ClientCreated{}, err
	}
	id, err := randomBase62(ClientIDPrefix, 20)
	if err != nil {
		return ClientCreated{}, err
	}
	params := store.InsertOAuthClientParams{ClientID: id, Name: name, RedirectUris: uris, ScopesAllowed: scopes, CreatedBy: p.CreatedBy}
	var secret string
	if p.Confidential {
		if secret, err = randomBase62(ClientSecretPrefix, 43); err != nil {
			return ClientCreated{}, err
		}
		h := Hash(secret)
		params.ClientSecretHash = &h
	}
	row, err := store.New(s.db).InsertOAuthClient(ctx, params)
	if err != nil {
		return ClientCreated{}, err
	}
	return ClientCreated{Secret: secret, Row: row}, nil
}

// ListClients lists every client, live ones first.
func (s *Service) ListClients(ctx context.Context) ([]store.OauthClient, error) {
	return store.New(s.db).ListOAuthClients(ctx)
}

// DisableClient disables a client and revokes every token it was issued.
// Disabling twice is fine.
func (s *Service) DisableClient(ctx context.Context, clientID string, by uuid.UUID) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		c, err := q.GetOAuthClient(ctx, clientID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := q.DisableOAuthClient(ctx, clientID); err != nil {
			return err
		}
		_, err = q.RevokeOAuthClientTokens(ctx, store.RevokeOAuthClientTokensParams{OauthClientID: &c.ID, RevokedBy: &by})
		return err
	})
}

// authenticateClient resolves a client by id and, for a confidential
// client, its secret. A public client must send none.
func authenticateClient(ctx context.Context, q *store.Queries, clientID, secret string) (store.OauthClient, error) {
	if clientID == "" {
		return store.OauthClient{}, fmt.Errorf("%w: client_id is required", ErrInvalidClient)
	}
	c, err := q.GetOAuthClient(ctx, clientID)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.OauthClient{}, fmt.Errorf("%w: unknown client", ErrInvalidClient)
	}
	if err != nil {
		return store.OauthClient{}, err
	}
	if c.DisabledAt != nil {
		return store.OauthClient{}, fmt.Errorf("%w: the client is disabled", ErrInvalidClient)
	}
	switch {
	case c.ClientSecretHash == nil && secret != "":
		return store.OauthClient{}, fmt.Errorf("%w: a public client sends no secret", ErrInvalidClient)
	case c.ClientSecretHash != nil && (secret == "" || subtle.ConstantTimeCompare([]byte(Hash(secret)), []byte(*c.ClientSecretHash)) != 1):
		return store.OauthClient{}, fmt.Errorf("%w: wrong client secret", ErrInvalidClient)
	}
	return c, nil
}

// ---- Authorization -------------------------------------------------------------

// AuthorizeRequest is what an app sends the user to /oauth/authorize with
// (RFC 6749 §4.1.1, RFC 7636 §4.3).
type AuthorizeRequest struct {
	ClientID            string
	RedirectURI         string
	Scope               string // space-separated
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
}

// Authorization is a validated request.
type Authorization struct {
	Client store.OauthClient
	// Scopes the app asked for, normalized; the user may narrow them.
	Scopes []string
}

// ValidateAuthorize checks an authorize request against its client: a
// live client, an exact redirect URI match, scopes within what the client
// may ask for, and a PKCE S256 challenge.
func (s *Service) ValidateAuthorize(ctx context.Context, r AuthorizeRequest) (Authorization, error) {
	c, err := store.New(s.db).GetOAuthClient(ctx, r.ClientID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Authorization{}, fmt.Errorf("%w: unknown client_id", ErrInvalid)
	}
	if err != nil {
		return Authorization{}, err
	}
	if c.DisabledAt != nil {
		return Authorization{}, fmt.Errorf("%w: this app has been disabled", ErrInvalid)
	}
	if !slices.Contains(c.RedirectUris, r.RedirectURI) {
		return Authorization{}, fmt.Errorf("%w: redirect_uri is not one the app registered", ErrInvalid)
	}
	if len(r.State) > 512 {
		return Authorization{}, fmt.Errorf("%w: state is up to 512 characters", ErrInvalid)
	}
	if r.CodeChallengeMethod != CodeChallengeMethod {
		return Authorization{}, fmt.Errorf("%w: code_challenge_method must be %s (PKCE is required)", ErrInvalid, CodeChallengeMethod)
	}
	if !verifierRe.MatchString(r.CodeChallenge) {
		return Authorization{}, fmt.Errorf("%w: code_challenge must be the base64url SHA-256 of the verifier", ErrInvalid)
	}
	scopes := strings.Fields(r.Scope)
	if len(scopes) == 0 {
		scopes = c.ScopesAllowed
	}
	scopes, err = NormalizeScopes(scopes)
	if err != nil {
		return Authorization{}, err
	}
	for _, sc := range scopes {
		if !HasScope(c.ScopesAllowed, sc) {
			return Authorization{}, fmt.Errorf("%w: the app may not ask for the %s scope", ErrInvalid, sc)
		}
	}
	return Authorization{Client: c, Scopes: scopes}, nil
}

// CodeParams is a user's consent. The caller has checked that the user
// may act in OrgID and on every one of Projects (as for a token).
type CodeParams struct {
	Request  AuthorizeRequest
	UserID   uuid.UUID
	OrgID    uuid.UUID
	Scopes   []string    // may be narrowed from the request, never widened
	Projects []uuid.UUID // nil: unrestricted
	// ExpiresIn is the token's lifetime once exchanged; 0 for the default.
	ExpiresIn time.Duration
}

// IssueCode stores a consent and returns the single-use authorization
// code for the app.
func (s *Service) IssueCode(ctx context.Context, p CodeParams) (string, error) {
	a, err := s.ValidateAuthorize(ctx, p.Request)
	if err != nil {
		return "", err
	}
	scopes := p.Scopes
	if len(scopes) == 0 {
		scopes = a.Scopes
	}
	scopes, err = NormalizeScopes(scopes)
	if err != nil {
		return "", err
	}
	for _, sc := range scopes {
		if !HasScope(a.Scopes, sc) {
			return "", fmt.Errorf("%w: the app did not ask for the %s scope", ErrInvalid, sc)
		}
	}
	st, err := s.GetSettings(ctx)
	if err != nil {
		return "", err
	}
	var days *int32
	if p.ExpiresIn != 0 {
		if p.ExpiresIn < 24*time.Hour || p.ExpiresIn > time.Duration(st.MaxDays)*24*time.Hour {
			return "", fmt.Errorf("%w: the token must expire within 1 to %d days", ErrInvalid, st.MaxDays)
		}
		d := int32(p.ExpiresIn / (24 * time.Hour))
		days = &d
	}
	if p.Projects != nil && len(p.Projects) == 0 {
		return "", fmt.Errorf("%w: a project restriction needs at least one project", ErrInvalid)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	code := base64.RawURLEncoding.EncodeToString(b)
	err = store.New(s.db).InsertOAuthCode(ctx, store.InsertOAuthCodeParams{
		CodeHash: Hash(code), ClientID: a.Client.ClientID, UserID: p.UserID, OrgID: p.OrgID, ProjectIds: p.Projects,
		Scopes: scopes, ExpiresInDays: days, RedirectUri: p.Request.RedirectURI, CodeChallenge: p.Request.CodeChallenge,
		ExpiresAt: s.cfg.Now().Add(CodeExpiry),
	})
	if err != nil {
		return "", err
	}
	return code, nil
}

// ---- Exchange ------------------------------------------------------------------

// ExchangeParams is the app's token request (RFC 6749 §4.1.3, RFC 7636 §4.5).
type ExchangeParams struct {
	Code         string
	RedirectURI  string
	ClientID     string
	ClientSecret string
	CodeVerifier string
}

// PKCEChallenge is the S256 challenge of a verifier (RFC 7636 §4.2).
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// codeReused says a code was presented again; Exchange then revokes the
// token the first use issued, outside the rolled-back transaction.
type codeReused struct {
	tokenID *uuid.UUID
	orgID   uuid.UUID
}

func (codeReused) Error() string { return "authorization code reused" }

// Exchange turns a code into a token, once. A code presented again is
// refused and the token it issued is revoked (RFC 6749 §4.1.2).
func (s *Service) Exchange(ctx context.Context, p ExchangeParams) (Created, error) {
	if p.Code == "" || p.RedirectURI == "" {
		return Created{}, fmt.Errorf("%w: code and redirect_uri are required", ErrInvalidRequest)
	}
	if !verifierRe.MatchString(p.CodeVerifier) {
		return Created{}, fmt.Errorf("%w: code_verifier must be 43 to 128 characters of [A-Za-z0-9._~-]", ErrInvalidRequest)
	}
	out, err := s.exchange(ctx, p)
	var reused codeReused
	if errors.As(err, &reused) {
		if reused.tokenID != nil {
			if _, err := store.New(s.db).RevokeToken(ctx, store.RevokeTokenParams{ID: *reused.tokenID, OrgID: reused.orgID}); err != nil {
				return Created{}, err
			}
		}
		return Created{}, fmt.Errorf("%w: the code was already used; the token it issued is revoked", ErrInvalidGrant)
	}
	return out, err
}

func (s *Service) exchange(ctx context.Context, p ExchangeParams) (Created, error) {
	var out Created
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		c, err := authenticateClient(ctx, q, p.ClientID, p.ClientSecret)
		if err != nil {
			return err
		}
		row, err := q.GetOAuthCodeForUpdate(ctx, Hash(p.Code))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: unknown code", ErrInvalidGrant)
		}
		if err != nil {
			return err
		}
		if row.ClientID != c.ClientID {
			return fmt.Errorf("%w: the code was issued to another client", ErrInvalidGrant)
		}
		if row.UsedAt != nil {
			// RFC 6749 §4.1.2: deny, and revoke what the first use issued.
			return codeReused{tokenID: row.TokenID, orgID: row.OrgID}
		}
		if !s.cfg.Now().Before(row.ExpiresAt) {
			return fmt.Errorf("%w: the code expired; ask the user again", ErrInvalidGrant)
		}
		if row.RedirectUri != p.RedirectURI {
			return fmt.Errorf("%w: redirect_uri differs from the authorization request's", ErrInvalidGrant)
		}
		if subtle.ConstantTimeCompare([]byte(PKCEChallenge(p.CodeVerifier)), []byte(row.CodeChallenge)) != 1 {
			return fmt.Errorf("%w: code_verifier does not match the code_challenge", ErrInvalidGrant)
		}
		var exp time.Duration
		if row.ExpiresInDays != nil {
			exp = time.Duration(*row.ExpiresInDays) * 24 * time.Hour
		}
		created, err := s.create(ctx, q, CreateParams{
			UserID: row.UserID, OrgID: row.OrgID, Name: c.Name, Scopes: row.Scopes, Projects: row.ProjectIds,
			ExpiresIn: exp, Via: "oauth", OAuthClientID: &c.ID,
		})
		if err != nil {
			return err
		}
		n, err := q.UseOAuthCode(ctx, store.UseOAuthCodeParams{CodeHash: row.CodeHash, OrgID: row.OrgID, TokenID: &created.Row.ID})
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: the code was already used", ErrInvalidGrant)
		}
		out = created
		return nil
	})
	return out, err
}

// RevokeByClient revokes a token the client was issued (RFC 7009) and
// returns its id and whether this call revoked it. An unknown token, or
// one that is not the client's, is not an error.
func (s *Service) RevokeByClient(ctx context.Context, clientID, clientSecret, token string) (uuid.UUID, bool, error) {
	q := store.New(s.db)
	c, err := authenticateClient(ctx, q, clientID, clientSecret)
	if err != nil {
		return uuid.Nil, false, err
	}
	if !strings.HasPrefix(token, Prefix) || len(token) > 100 {
		return uuid.Nil, false, nil
	}
	row, err := q.GetOAuthClientToken(ctx, store.GetOAuthClientTokenParams{TokenHash: Hash(token), OauthClientID: &c.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	n, err := q.RevokeToken(ctx, store.RevokeTokenParams{ID: row.ID, OrgID: row.OrgID})
	return row.ID, n > 0, err
}
