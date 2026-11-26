package agentsvc

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/agentca"
)

// allowFile keeps the last allow list the agent was given, as pushed
// (signed), so a restarted agent enforces it before the next push.
const allowFile = "client-serials.json"

// LegacyClientCertWindow is how long after its first list an agent still
// accepts, by subject alone, a client certificate that names no server:
// one issued by a xenibase-server from before allow lists (30 days long),
// which registers no serial. It lets servers sharing a metadata database
// be upgraded one at a time; a server still that old after the window is
// refused.
const LegacyClientCertWindow = 31 * 24 * time.Hour

// savedAllow is allowFile's content.
type savedAllow struct {
	List  agentapi.ClientSerials `json:"list"`
	Since time.Time              `json:"since"` // the first list arrived
}

// ClientAllow is the set of xenibase-server client certificate serials the
// agent accepts. Until the first list arrives it accepts every client
// certificate the CA signed for xenibase-server (the subject check in
// TLSConfig), as agents always did, so an upgrade or an older server
// locks no one out. Once it has a list it accepts only the live serials
// on it, on every request, so revoking a serial also ends a connection
// that is already open. The one exception is LegacyClientCertWindow.
//
// The list is signed by the agent CA (agentca.SignList), and checked
// against the CA the agent pinned at registration. A client whose serial
// is not on the current list may still push one, but only a newer list
// that names it: that is how a server that has just issued itself a
// certificate (at start, or after its last one was revoked) gets it
// accepted.
type ClientAllow struct {
	path string // empty: not saved
	ca   *x509.Certificate
	now  func() time.Time
	log  *slog.Logger

	mu      sync.RWMutex
	have    bool
	since   time.Time // the first list arrived
	gen     int64
	serials map[string]time.Time // serial → expiry
}

// LoadClientAllow returns the allow list saved in dir, or an empty one
// (accepting by subject) when none was ever pushed. A saved list that no
// longer verifies is treated as an empty list that is enforced: nothing
// but a push from the control plane gets in, and the next push repairs it.
func LoadClientAllow(dir string, ca *x509.Certificate, log *slog.Logger) *ClientAllow {
	a := &ClientAllow{ca: ca, now: time.Now, log: log}
	if dir == "" {
		return a
	}
	a.path = filepath.Join(dir, allowFile)
	raw, err := os.ReadFile(a.path)
	if errors.Is(err, fs.ErrNotExist) {
		return a
	}
	var saved savedAllow
	if err == nil {
		err = json.Unmarshal(raw, &saved)
	}
	var list agentapi.ClientSerialList
	if err == nil {
		list, err = agentca.VerifyList(ca, saved.List)
	}
	if err != nil || saved.Since.IsZero() {
		log.Error("the saved client certificate allow list is unreadable; accepting only a new push from xenibase-server", "file", a.path, "err", err)
		a.have, a.since = true, time.Time{}
		a.serials = map[string]time.Time{}
		return a
	}
	a.since = saved.Since
	a.set(list)
	return a
}

func (a *ClientAllow) set(list agentapi.ClientSerialList) {
	m := make(map[string]time.Time, len(list.Serials))
	for _, s := range list.Serials {
		m[s.Serial] = s.ExpiresAt
	}
	a.have, a.gen, a.serials = true, list.Generation, m
}

// Enforcing reports whether the agent has a list (and so accepts only
// serials on it), and its generation.
func (a *ClientAllow) Enforcing() (bool, int64) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.have, a.gen
}

// Allowed reports whether a client certificate (already verified against
// the CA and the subject) may call the agent.
func (a *ClientAllow) Allowed(c *x509.Certificate) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.allowedLocked(c)
}

func (a *ClientAllow) allowedLocked(c *x509.Certificate) bool {
	if !a.have {
		return true
	}
	exp, ok := a.serials[agentca.Serial(c)]
	if ok && a.now().Before(exp) {
		return true
	}
	_, named := agentca.ServerNameFromCert(c)
	return !named && !a.since.IsZero() && a.now().Before(a.since.Add(LegacyClientCertWindow))
}

// errStale is a push older than the list the agent has.
var errStale = errors.New("allow list is older than the one this agent has")

// Update replaces the list with a pushed one, sent by a client presenting
// caller, and saves it. It returns the generation the agent then holds.
func (a *ClientAllow) Update(cs agentapi.ClientSerials, caller *x509.Certificate) (int64, int, error) {
	list, err := agentca.VerifyList(a.ca, cs)
	if err != nil {
		return 0, http.StatusBadRequest, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.have && list.Generation < a.gen {
		return a.gen, http.StatusConflict, errStale
	}
	if !a.allowedLocked(caller) {
		named := false
		for _, s := range list.Serials {
			named = named || s.Serial == agentca.Serial(caller)
		}
		if !named {
			return a.gen, http.StatusForbidden, errors.New(agentapi.ErrClientSerialNotAllowed)
		}
	}
	since := a.since
	if !a.have {
		since = a.now()
	}
	if a.path != "" {
		raw, _ := json.Marshal(savedAllow{List: cs, Since: since})
		tmp := a.path + ".tmp"
		if err := os.WriteFile(tmp, raw, 0o600); err != nil {
			return a.gen, http.StatusInternalServerError, fmt.Errorf("save allow list: %w", err)
		}
		if err := os.Rename(tmp, a.path); err != nil {
			return a.gen, http.StatusInternalServerError, fmt.Errorf("save allow list: %w", err)
		}
	}
	first := !a.have
	a.since = since
	a.set(list)
	if first {
		a.log.Info("client certificate allow list received: only its serials are accepted from now on", "generation", list.Generation, "serials", len(list.Serials))
	}
	return a.gen, http.StatusOK, nil
}

// peer is the request's verified client certificate, or nil (a handler
// served without TLS, in tests).
func peer(r *http.Request) *x509.Certificate {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return nil
	}
	return r.TLS.PeerCertificates[0]
}

// checkSerial refuses a request whose client certificate is not on the
// allow list. Pushing a list is checked by Update instead.
func (s *Service) checkSerial(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := peer(r)
		if s.cfg.ClientAllow == nil || c == nil || (r.Method == http.MethodPut && r.URL.Path == agentapi.PathClientSerials) {
			next.ServeHTTP(w, r)
			return
		}
		if !s.cfg.ClientAllow.Allowed(c) {
			s.log.Warn("refused a client certificate that is not on the allow list", "serial", agentca.Serial(c), "remote", r.RemoteAddr)
			fail(w, http.StatusForbidden, errors.New(agentapi.ErrClientSerialNotAllowed))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientSerials is PUT /v1/client-serials.
func (s *Service) clientSerials(w http.ResponseWriter, r *http.Request) {
	c := peer(r)
	if s.cfg.ClientAllow == nil || c == nil {
		fail(w, http.StatusNotFound, errors.New("this agent keeps no allow list"))
		return
	}
	var cs agentapi.ClientSerials
	if !decode(w, r, &cs) {
		return
	}
	gen, status, err := s.cfg.ClientAllow.Update(cs, c)
	if status == http.StatusConflict {
		writeJSON(w, status, agentapi.ClientSerialsAck{Generation: gen})
		return
	}
	if err != nil {
		fail(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, agentapi.ClientSerialsAck{Generation: gen})
}
