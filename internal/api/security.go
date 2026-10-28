package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/auth"
	"github.com/israel-duff/xenibase/internal/authz"
	"github.com/israel-duff/xenibase/internal/store"
)

// SecurityOptions configures cookies and proxy trust.
type SecurityOptions struct {
	// SecureCookies sets the Secure flag and the __Host- name prefix. Turn
	// it off only for plain-HTTP development.
	SecureCookies bool
	// TrustedProxies are the addresses whose X-Forwarded-For is believed
	// (e.g. Caddy). Empty trusts no one: the TCP peer is the client.
	TrustedProxies []netip.Prefix
}

const (
	csrfHeader   = "X-CSRF-Token"
	sessionName  = "xenibase_session"
	csrfName     = "xenibase_csrf"
	secureprefix = "__Host-"
)

func (s *Server) cookieName(base string) string {
	if s.sec.SecureCookies {
		return secureprefix + base
	}
	return base
}

type ctxKey int

const (
	keyIP ctxKey = iota
	keySession
	keyAudit
	keyAccess
)

// clientIP returns the request's client address, honouring
// X-Forwarded-For only from trusted proxies (rightmost untrusted hop).
func (s *Server) clientIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	peer = peer.Unmap()
	if !s.trusted(peer) {
		return peer
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		a = a.Unmap()
		if !s.trusted(a) {
			return a
		}
		peer = a
	}
	return peer
}

func (s *Server) trusted(a netip.Addr) bool {
	for _, p := range s.sec.TrustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func ipFrom(ctx context.Context) *netip.Addr {
	if a, ok := ctx.Value(keyIP).(netip.Addr); ok && a.IsValid() {
		return &a
	}
	return nil
}

func ipString(ctx context.Context) string {
	if a := ipFrom(ctx); a != nil {
		return a.String()
	}
	return "unknown"
}

func sessionFrom(ctx context.Context) (auth.Session, bool) {
	s, ok := ctx.Value(keySession).(auth.Session)
	return s, ok
}

// userID returns the signed-in user, for created_by columns.
func userID(ctx context.Context) *uuid.UUID {
	if s, ok := sessionFrom(ctx); ok {
		id := s.UserID
		return &id
	}
	return nil
}

// ---- Audit -----------------------------------------------------------------

// auditInfo is filled in by handlers while a mutating request runs.
type auditInfo struct {
	skip bool
	// breakGlass marks a platform admin acting through a break-glass
	// session: the row shows in the org's log and the platform's (V2 §2.4).
	breakGlass bool
	// tokenID is the API token that made the request (actor kind token).
	tokenID    *uuid.UUID
	userID     *uuid.UUID
	orgID      uuid.UUID
	projectID  uuid.UUID
	targetType string
	targetID   string
	detail     map[string]any
}

func auditFrom(ctx context.Context) *auditInfo {
	if a, ok := ctx.Value(keyAudit).(*auditInfo); ok {
		return a
	}
	return &auditInfo{} // tests calling handlers directly
}

func (a *auditInfo) target(typ, id string) { a.targetType, a.targetID = typ, id }

func (a *auditInfo) set(k string, v any) {
	if a.detail == nil {
		a.detail = map[string]any{}
	}
	a.detail[k] = v
}

// auditActions names mutating routes. Unlisted ones fall back to
// "<METHOD> <pattern>".
var auditActions = map[string]string{
	"POST /api/v1/auth/login":                                      "auth.login",
	"POST /api/v1/auth/totp":                                       "auth.totp",
	"POST /api/v1/auth/reauth":                                     "auth.reauth",
	"POST /api/v1/auth/logout":                                     "auth.logout",
	"POST /api/v1/setup/begin":                                     "setup.begin",
	"POST /api/v1/setup/complete":                                  "setup.complete",
	"POST /api/v1/projects":                                        "project.create",
	"DELETE /api/v1/projects/{id}":                                 "project.delete",
	"PATCH /api/v1/projects/{id}/settings":                         "project.update",
	"POST /api/v1/projects/{id}/rotate-password":                   "project.rotate_password",
	"PUT /api/v1/settings/db-host":                                 "settings.db_host",
	"POST /api/v1/dev/operations":                                  "dev.operation",
	"POST /api/v1/settings/db-host/check":                          "", // read-only check
	"POST /api/v1/payments/webhooks/{provider}":                    "", // the payment event log records them
	"POST /api/v1/support/inbound/email":                           "", // the ticket records it
	"POST /api/v1/support/whatsapp":                                "", // likewise
	"POST /api/v1/orgs/{org}/support/tickets":                      "support.ticket_open",
	"POST /api/v1/orgs/{org}/support/tickets/{ticket_id}/messages": "support.ticket_reply",
	"POST /api/v1/orgs/{org}/support/phones":                       "support.phone_add",
	"DELETE /api/v1/orgs/{org}/support/phones/{phone}":             "support.phone_remove",
	"PATCH /api/v1/admin/support/tickets/{ticket_id}":              "support.ticket_update",
	"POST /api/v1/admin/support/tickets/{ticket_id}/messages":      "support.staff_reply",
	"POST /api/v1/orgs/{org}/legal/{document_id}/accept":           "legal.accept",
	"POST /api/v1/admin/legal":                                     "legal.publish",
	"POST /api/v1/admin/orgs/{org}/order-form":                     "legal.order_form",
	"POST /api/v1/projects/{id}/backups":                           "backup.create",
	"POST /api/v1/backups/{id}/restore":                            "backup.restore",
	"POST /api/v1/restore-tests":                                   "backup.restore_test",
	"PUT /api/v1/settings/storage":                                 "settings.storage",
	"POST /api/v1/settings/storage/test":                           "settings.storage_test",
	"POST /api/v1/settings/backup-key":                             "settings.backup_key.generate",
	"POST /api/v1/settings/backup-key/export":                      "settings.backup_key.export",
	"POST /api/v1/settings/backup-key/confirm":                     "settings.backup_key.confirm",
	"POST /api/v1/imports/preflight":                               "import.preflight",
	"POST /api/v1/imports":                                         "import.create",
	"POST /api/v1/nodes/{id}/registration-token":                   "node.registration_token",
	"POST /api/v1/agent/register":                                  "node.agent_register",
	"POST /api/v1/nodes":                                           "node.create",
	"DELETE /api/v1/nodes/{id}":                                    "node.remove",
	"PATCH /api/v1/nodes/{id}":                                     "node.update",
	"PUT /api/v1/admin/capacity/settings":                          "capacity.settings",
	"PUT /api/v1/admin/regions/{region_id}":                        "region.save",
	"PUT /api/v1/projects/{id}/residency":                          "project.residency",
	"PUT /api/v1/projects/{id}/manage-roles":                       "project.manage_roles",
	"PUT /api/v1/projects/{id}/network":                            "project.network",
	"POST /api/v1/admin/capacity/evaluate":                         "capacity.evaluate",
	"POST /api/v1/admin/capacity/proposals/{proposal_id}/approve":  "capacity.approve",
	"POST /api/v1/admin/capacity/proposals/{proposal_id}/reject":   "capacity.reject",
	"POST /api/v1/admin/capacity/rebalance":                        "capacity.rebalance",
	"POST /api/v1/admin/capacity/batches/{batch_id}":               "capacity.batch",
	"POST /api/v1/nodes/{id}/drain":                                "node.drain",
	"DELETE /api/v1/nodes/{id}/drain":                              "node.drain_stop",
	"PUT /api/v1/nodes/{id}/cost":                                  "node.cost",
	"POST /api/v1/admin/costs/attribute":                           "costs.attribute",
	"PUT /api/v1/admin/costs/settings":                             "costs.settings",
	"POST /api/v1/admin/fx-rates":                                  "costs.fx_rate",
	"POST /api/v1/nodes/{id}/shared-cluster":                       "node.shared_cluster",
	"POST /api/v1/projects/{id}/pitr":                              "backup.pitr",
	"POST /api/v1/projects/{id}/instance":                          "project.instance",
	"POST /api/v1/projects/{id}/promote":                           "project.promote",
	"POST /api/v1/projects/{id}/sql":                               "project.console",
	"POST /api/v1/projects/{id}/sql/cancel":                        "project.console_cancel",
	"POST /api/v1/projects/{id}/extensions":                        "project.extension",
	"POST /api/v1/security/isolation-checks":                       "security.isolation_check",
	"PUT /api/v1/settings/alerts":                                  "settings.alerts",
	"POST /api/v1/settings/alerts/test":                            "settings.alerts_test",

	"POST /api/v1/auth/signup":                                   "auth.signup",
	"POST /api/v1/auth/verify-email":                             "auth.verify_email",
	"POST /api/v1/auth/verify-email/resend":                      "auth.verify_email_resend",
	"POST /api/v1/auth/password-reset":                           "auth.password_reset_request",
	"POST /api/v1/auth/password-reset/confirm":                   "auth.password_reset",
	"POST /api/v1/invitations/preview":                           "", // read-only
	"POST /api/v1/invitations/accept":                            "invitation.accept",
	"PATCH /api/v1/me":                                           "account.update",
	"POST /api/v1/me/password":                                   "account.password",
	"DELETE /api/v1/me/sessions/{session_id}":                    "account.session_revoke",
	"POST /api/v1/me/recovery-codes":                             "account.recovery_codes",
	"POST /api/v1/me/totp/setup":                                 "account.totp_setup",
	"POST /api/v1/me/totp/enable":                                "account.totp_enable",
	"POST /api/v1/me/totp/disable":                               "account.totp_disable",
	"POST /api/v1/me/terms/accept":                               "account.terms_accept",
	"POST /api/v1/me/invitations/{invitation_id}/accept":         "invitation.accept",
	"POST /api/v1/orgs":                                          "org.create",
	"PATCH /api/v1/orgs/{org}":                                   "org.update",
	"POST /api/v1/orgs/{org}/members":                            "org.member.invite",
	"PATCH /api/v1/orgs/{org}/members/{user}":                    "org.member.role",
	"DELETE /api/v1/orgs/{org}/members/{user}":                   "org.member.remove",
	"POST /api/v1/orgs/{org}/leave":                              "org.member.leave",
	"POST /api/v1/orgs/{org}/transfer-ownership":                 "org.transfer_ownership",
	"DELETE /api/v1/orgs/{org}/invitations/{invitation_id}":      "org.invitation.revoke",
	"POST /api/v1/projects/{id}/members":                         "project.member.add",
	"PATCH /api/v1/projects/{id}/members/{user}":                 "project.member.role",
	"DELETE /api/v1/projects/{id}/members/{user}":                "project.member.remove",
	"POST /api/v1/projects/{id}/credentials":                     "project.credentials",
	"POST /api/v1/projects/{id}/transfer":                        "project.transfer",
	"PATCH /api/v1/admin/users/{user}":                           "admin.user.update",
	"POST /api/v1/admin/users/{user}/reset-2fa":                  "admin.user.reset_2fa",
	"POST /api/v1/admin/invitations":                             "admin.invitation.create",
	"DELETE /api/v1/admin/invitations/{invitation_id}":           "admin.invitation.revoke",
	"PUT /api/v1/admin/settings/signup":                          "admin.settings.signup",
	"PUT /api/v1/admin/settings/mail":                            "admin.settings.mail",
	"POST /api/v1/admin/settings/terms":                          "admin.settings.terms",
	"POST /api/v1/tokens":                                        "token.create",
	"DELETE /api/v1/tokens/{token_id}":                           "token.revoke",
	"DELETE /api/v1/orgs/{org}/tokens/{token_id}":                "org.token.revoke",
	"POST /api/v1/auth/device":                                   "",
	"POST /api/v1/auth/device/token":                             "",
	"POST /api/v1/auth/device/approve":                           "token.device_approve",
	"POST /api/v1/oauth/authorize":                               "oauth.authorize",
	"POST /api/v1/oauth/token":                                   "oauth.token_issue",
	"POST /api/v1/oauth/revoke":                                  "oauth.token_revoke",
	"POST /api/v1/admin/oauth/clients":                           "admin.oauth_client.create",
	"DELETE /api/v1/admin/oauth/clients/{client_id}":             "admin.oauth_client.disable",
	"POST /api/v1/admin/projects/{project_id}/roles":             "admin.project.integrator_roles",
	"PUT /api/v1/admin/settings/tokens":                          "admin.settings.tokens",
	"POST /api/v1/projects/{id}/tables/{schema}/{table}/changes": "project.rows.save",
	"POST /api/v1/projects/{id}/schema/apply":                    "project.schema.change",
	"POST /api/v1/projects/{id}/schema/preview":                  "", // read-only
	"POST /api/v1/projects/{id}/schema/migration":                "", // read-only
	"POST /api/v1/orgs/{org}/storage-targets":                    "org.storage_target.create",
	"PATCH /api/v1/orgs/{org}/storage-targets/{target_id}":       "org.storage_target.update",
	"DELETE /api/v1/orgs/{org}/storage-targets/{target_id}":      "org.storage_target.delete",
	"POST /api/v1/storage-targets/test":                          "", // read-only check
	"POST /api/v1/admin/storage-targets":                         "admin.storage_target.create",
	"PATCH /api/v1/admin/storage-targets/{target_id}":            "admin.storage_target.update",
	"DELETE /api/v1/admin/storage-targets/{target_id}":           "admin.storage_target.delete",
	"PUT /api/v1/projects/{id}/storage-target":                   "project.storage_target",
	"POST /api/v1/projects/{id}/backup-key":                      "project.backup_key.enable",
	"GET /api/v1/projects/{id}/backup-key/download":              "project.backup_key.download",
	"GET /api/v1/backups/{id}/download":                          "backup.download",
	"POST /api/v1/projects/{id}/branches":                        "branch.create",
	"POST /api/v1/projects/{id}/reset":                           "branch.reset",
	"POST /api/v1/projects/{id}/detach":                          "branch.detach",

	// Webhooks and scheduled jobs (V2 §9).
	"POST /api/v1/projects/{id}/webhooks":                            "webhook.create",
	"PATCH /api/v1/projects/{id}/webhooks/{webhook_id}":              "webhook.update",
	"DELETE /api/v1/projects/{id}/webhooks/{webhook_id}":             "webhook.delete",
	"POST /api/v1/projects/{id}/webhooks/{webhook_id}/test":          "webhook.test",
	"POST /api/v1/projects/{id}/webhooks/{webhook_id}/rotate-secret": "webhook.rotate_secret",
	"POST /api/v1/projects/{id}/webhooks/{webhook_id}/replay":        "webhook.replay",
	"POST /api/v1/projects/{id}/jobs":                                "job.create",
	"PATCH /api/v1/projects/{id}/jobs/{job_id}":                      "job.update",
	"DELETE /api/v1/projects/{id}/jobs/{job_id}":                     "job.delete",
	"POST /api/v1/projects/{id}/jobs/{job_id}/run":                   "job.run",
	"PUT /api/v1/admin/orgs/{org}/outbound":                          "admin.org_outbound_allowlist",

	// Demotion (V2 §5).
	"POST /api/v1/projects/{id}/demote/preflight": "project.demote_preflight",
	"POST /api/v1/projects/{id}/demote":           "project.demote",
}

// auditedReads are GET routes audited like mutations: handing out a
// secret is an event (V2 §6: backup key downloads are audited).
var auditedReads = map[string]bool{
	"GET /api/v1/projects/{id}/backup-key/download": true,
	"GET /api/v1/backups/{id}/download":             true,
}

func outcomeFor(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests:
		return "denied"
	case status >= 400:
		return "failure"
	default:
		return "success"
	}
}

func (s *Server) writeAudit(r *http.Request, action string, status int, info *auditInfo) {
	if s.db == nil || action == "" || info.skip {
		return
	}
	uid := info.userID
	if uid == nil {
		uid = userID(r.Context())
	}
	detail := map[string]any{"status": status, "request_id": middleware.GetReqID(r.Context())}
	for k, v := range info.detail {
		detail[k] = v
	}
	b, _ := json.Marshal(detail)
	ua := r.UserAgent()
	if len(ua) > 512 {
		ua = ua[:512]
	}
	params := store.InsertAuditParams{
		UserID: uid, Action: action, Detail: b, Ip: ipFrom(r.Context()),
		Outcome: outcomeFor(status), ActorKind: authz.ActorSession, BreakGlass: info.breakGlass,
	}
	if info.tokenID != nil {
		params.ActorKind, params.TokenID = authz.ActorToken, info.tokenID
	}
	if info.orgID != uuid.Nil {
		params.OrgID = &info.orgID
	}
	if info.projectID != uuid.Nil {
		params.ProjectID = &info.projectID
	}
	if info.targetType != "" {
		params.TargetType = &info.targetType
	}
	if info.targetID != "" {
		params.TargetID = &info.targetID
	}
	if ua != "" {
		params.UserAgent = &ua
	}
	// The audit row must outlive a client that hung up.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if err := store.New(s.db).InsertAudit(ctx, params); err != nil {
		s.log.Error("write audit log", "action", action, "err", err)
	}
}

// ---- Middleware --------------------------------------------------------------

// csrfExempt lists mutating routes called by programs, not browsers. They
// authenticate with a token in the body and ignore cookies, so CSRF does
// not apply.
var csrfExempt = map[string]bool{
	"POST /api/v1/agent/register":    true,
	"POST /api/v1/auth/device":       true,
	"POST /api/v1/auth/device/token": true,
	// Third-party apps authenticate with their client secret and PKCE.
	"POST /api/v1/oauth/token":  true,
	"POST /api/v1/oauth/revoke": true,
	// Signed by the provider; no cookies.
	"POST /api/v1/payments/webhooks/{provider}": true,
	"POST /api/v1/support/inbound/email":        true,
	"POST /api/v1/support/whatsapp":             true,
	// xenibase-edge, signed with the edge secret (V4 §2.1).
	"POST /api/v1/edge/report":        true,
	"POST /api/v1/edge/wake":          true,
	"POST /api/v1/edge/auth-message":  true,
	"POST /api/v1/edge/auth-hook":     true,
	"POST /api/v1/edge/storage-event": true,
}

// reauthRequired lists destructive routes needing a recent step-up auth
// (spec §7.2).
var reauthRequired = map[string]bool{
	"DELETE /api/v1/projects/{id}":            true,
	"POST /api/v1/settings/backup-key/export": true,
	"DELETE /api/v1/nodes/{id}":               true,
	// Spending money: a server, or the budget that lets Xenibase buy them.
	"POST /api/v1/admin/capacity/proposals/{proposal_id}/approve": true,
	"PUT /api/v1/admin/capacity/settings":                         true,
	"PUT /api/v1/projects/{id}/residency":                         true,
	"PUT /api/v1/projects/{id}/manage-roles":                      true,
	// Moving money or changing what is owed (M27 security review).
	"POST /api/v1/admin/payments/{payment_id}/refund":        true,
	"POST /api/v1/admin/payments":                            true,
	"POST /api/v1/admin/invoices/{invoice_id}/credit-notes":  true,
	"POST /api/v1/admin/payment-events/{event_id}/attribute": true,
	"POST /api/v1/admin/price-books/{version}/publish":       true,
	"PATCH /api/v1/admin/orgs/{org}/billing":                 true,
	"POST /api/v1/me/recovery-codes":                         true,
	"POST /api/v1/admin/users/{user}/reset-2fa":              true,
	"POST /api/v1/projects/{id}/transfer":                    true,
	"POST /api/v1/orgs/{org}/transfer-ownership":             true,
	"DELETE /api/v1/orgs/{org}":                              true,
	"POST /api/v1/admin/orgs/{org}/break-glass":              true,
	"POST /api/v1/admin/orgs/{org}/suspend":                  true,
	"GET /api/v1/projects/{id}/backup-key/download":          true,
	// Handing a third-party app a token, and creating roles on an instance.
	"POST /api/v1/oauth/authorize":                   true,
	"POST /api/v1/admin/projects/{project_id}/roles": true,
	// POST /api/v1/backups/{id}/restore checks it for mode in_place only.
}

func isMutating(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// guard enforces CSRF, authentication, re-authentication, the terms, and
// authorization (routeRules) on /api, and audits every mutating API request.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), keyIP, s.clientIP(r))
		r = r.WithContext(ctx)
		if (!isAPIPath(r.URL.Path) && r.URL.Path != "/metrics") || s.auth == nil {
			next.ServeHTTP(w, r)
			return
		}

		mutating := isMutating(r.Method)
		// Resolve the route now: requests refused below never reach routing.
		pattern, params := matchRoute(r)
		key := r.Method + " " + pattern
		info := &auditInfo{}
		ctx = context.WithValue(ctx, keyAudit, info)
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		if mutating || auditedReads[key] {
			defer func() {
				action, ok := auditActions[key]
				if !ok {
					action = key
				}
				s.writeAudit(r, action, ww.Status(), info)
			}()
		}

		// An API token (V2 §7.2): no cookies are involved, so CSRF does not
		// apply, and the session cookie is ignored.
		if bearer, ok := bearerToken(r); ok && pattern != "/metrics" {
			sess, status, code, msg := s.tokenSession(ctx, bearer)
			if status != 0 {
				if status == http.StatusTooManyRequests {
					ww.Header().Set("Retry-After", "60")
				}
				writeError(ww, status, code, msg)
				return
			}
			ctx = context.WithValue(ctx, keySession, sess)
			info.tokenID = &sess.Token.ID
		} else if c, err := r.Cookie(s.cookieName(sessionName)); err == nil {
			sess, err := s.auth.Authenticate(ctx, c.Value)
			if err == nil {
				ctx = context.WithValue(ctx, keySession, sess)
			} else if !isNoSession(err) {
				s.log.Error("authenticate session", "err", err)
				writeError(ww, http.StatusInternalServerError, "internal", "internal error")
				return
			}
		}
		r = r.WithContext(ctx)

		if mutating && !csrfExempt[key] && info.tokenID == nil {
			if msg := s.checkCSRF(r); msg != "" {
				writeError(ww, http.StatusForbidden, "csrf", msg)
				return
			}
		}

		rl, declared := routeRules[key]
		if !declared {
			if pattern == r.URL.Path {
				// No such route: routing answers 404/405.
				next.ServeHTTP(ww, r)
				return
			}
			s.log.Error("route has no authorization rule", "route", key)
			writeError(ww, http.StatusForbidden, "forbidden", "this route is not available")
			return
		}
		if rl.scope != scopePublic {
			sess, ok := sessionFrom(ctx)
			if !ok {
				writeError(ww, http.StatusUnauthorized, "unauthenticated", "sign in to continue")
				return
			}
			if !rl.beforeTerms && sess.Token == nil {
				pending, version, err := s.auth.TermsOutstanding(ctx, sess.UserID)
				if err != nil {
					s.internalError(ww, "terms", err)
					return
				}
				if pending {
					writeError(ww, http.StatusForbidden, "terms_required",
						fmt.Sprintf("accept version %d of the terms of use to continue", version))
					return
				}
			}
			// Tokens skip step-up auth: destructive actions need the admin
			// scope and a typed confirm field instead (V2 §7.2).
			if reauthRequired[key] && sess.Token == nil && !s.auth.RecentlyReauthenticated(sess) {
				writeError(ww, http.StatusForbidden, "reauth_required", "confirm your password and code to continue")
				return
			}
			acc, status, err := s.authorize(ctx, sess, rl, params, r)
			if err != nil {
				s.internalError(ww, "authorize", err)
				return
			}
			if status != 0 {
				// The header lets tests tell the guard's refusals from handlers'.
				switch {
				case status == http.StatusNotFound:
					ww.Header().Set("X-Xenibase-Authz", "hidden")
					writeError(ww, status, "not_found", "not found")
				case acc.NeedScope != "":
					ww.Header().Set("X-Xenibase-Authz", "denied")
					writeScopeError(ww, acc.NeedScope)
				case acc.Frozen:
					ww.Header().Set("X-Xenibase-Authz", "denied")
					writeError(ww, status, "org_suspended", "the organisation is suspended or being deleted; only viewing works")
				default:
					ww.Header().Set("X-Xenibase-Authz", "denied")
					writeError(ww, status, "forbidden", "you don't have permission to do this")
				}
				return
			}
			info.orgID, info.projectID, info.breakGlass = acc.OrgID, acc.ProjectID, acc.BreakGlass
			ctx = context.WithValue(ctx, keyAccess, acc)
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(ww, r)
	})
}

func isNoSession(err error) bool { return errors.Is(err, auth.ErrNoSession) }

// checkCSRF implements double-submit: the X-CSRF-Token header must equal the
// csrf cookie, which only same-origin script can read. An Origin header, when
// sent, must also name this host.
func (s *Server) checkCSRF(r *http.Request) string {
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			return "cross-origin request refused"
		}
	}
	c, err := r.Cookie(s.cookieName(csrfName))
	h := r.Header.Get(csrfHeader)
	if err != nil || c.Value == "" || h == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(h)) != 1 {
		return "missing or invalid CSRF token; reload the page"
	}
	return ""
}

// ensureCSRF returns the request's CSRF token, issuing a cookie if needed.
func (s *Server) ensureCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(s.cookieName(csrfName)); err == nil && len(c.Value) >= 32 {
		return c.Value
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := base64.RawURLEncoding.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(csrfName), Value: tok, Path: "/",
		Secure: s.sec.SecureCookies, SameSite: http.SameSiteStrictMode,
		MaxAge: int((30 * 24 * time.Hour).Seconds()),
	})
	return tok
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(sessionName), Value: token, Path: "/",
		HttpOnly: true, Secure: s.sec.SecureCookies, SameSite: http.SameSiteStrictMode,
		MaxAge: int((7 * 24 * time.Hour).Seconds()),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(sessionName), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.sec.SecureCookies, SameSite: http.SameSiteStrictMode,
	})
}

// matchRoute finds the pattern r will be routed to, and its path
// parameters, before routing.
func matchRoute(r *http.Request) (string, map[string]string) {
	rc := chi.RouteContext(r.Context())
	if rc == nil || rc.Routes == nil {
		return r.URL.Path, nil
	}
	tctx := chi.NewRouteContext()
	if rc.Routes.Match(tctx, r.Method, r.URL.Path) {
		params := map[string]string{}
		for i, k := range tctx.URLParams.Keys {
			params[k] = tctx.URLParams.Values[i]
		}
		return tctx.RoutePattern(), params
	}
	return r.URL.Path, nil
}

// securityHeaders sets conservative browser security headers.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if !isAPIPath(r.URL.Path) {
			// Cloudflare Turnstile's script and frame, for the signup page's
			// challenge (V3 §7.4).
			h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
				"script-src 'self' https://challenges.cloudflare.com; frame-src https://challenges.cloudflare.com; "+
				"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		}
		next.ServeHTTP(w, r)
	})
}
