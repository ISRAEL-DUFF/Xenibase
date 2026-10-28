package agentsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/floatip"
	"github.com/israel-duff/xenibase/internal/pooler"
)

// PoolerConfig turns the agent into a pooler host's agent (V3 §2.1): it
// receives the PgBouncer configuration, tells keepalived whether this host
// may hold the floating IP, and claims the IP when keepalived makes it
// MASTER.
type PoolerConfig struct {
	// Dir is where the PgBouncers read databases.ini, userlist.txt and the
	// TLS pair. Empty disables pooler mode.
	Dir string
	// FileMode for the written files (PgBouncer must be able to read them).
	FileMode os.FileMode
	// SessionAddr and PooledAddr are the local PgBouncers, checked for
	// readiness.
	SessionAddr, PooledAddr string
	// ServerID is the provider's ID for this machine.
	ServerID string
	// FloatIP assigns the floating IP to ServerID on becoming MASTER.
	FloatIP floatip.Provider
	// PidFiles hold the local PgBouncers' PIDs: they get SIGHUP after each
	// new configuration.
	PidFiles []string
}

const poolerStateFile = ".xenibase-pooler.json"

// poolerState is persisted next to the files so a restart keeps what
// generation is served and what generation xenibase-server expects.
type poolerState struct {
	Generation int64  `json:"generation"`
	Hash       string `json:"hash"`
	Expected   int64  `json:"expected"`
}

type poolerHost struct {
	cfg PoolerConfig
	log *slog.Logger

	mu         sync.Mutex
	st         poolerState
	vrrp       string
	claimErr   string
	claimAbort context.CancelFunc
}

func newPoolerHost(cfg PoolerConfig, log *slog.Logger) (*poolerHost, error) {
	if cfg.FileMode == 0 {
		cfg.FileMode = 0o640
	}
	if cfg.FloatIP == nil {
		cfg.FloatIP = floatip.None{}
	}
	if err := os.MkdirAll(cfg.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("pooler dir: %w", err)
	}
	p := &poolerHost{cfg: cfg, log: log}
	b, err := os.ReadFile(filepath.Join(cfg.Dir, poolerStateFile))
	if err == nil {
		if err := json.Unmarshal(b, &p.st); err != nil {
			return nil, fmt.Errorf("pooler state: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return p, nil
}

func (p *poolerHost) saveLocked() error {
	b, _ := json.Marshal(p.st)
	_, err := pooler.WriteFileAtomic(filepath.Join(p.cfg.Dir, poolerStateFile), b, 0o600)
	return err
}

// errOlder means a push carried a generation older than the one served:
// two xenibase-server processes raced, and the newer one already won.
var errOlder = errors.New("bundle is older than the configuration already served")

func (p *poolerHost) apply(b agentapi.PoolerBundle) error {
	for name := range b.Files {
		if !slices.Contains(agentapi.PoolerFiles, name) {
			return fmt.Errorf("unexpected file %q in the bundle", name)
		}
	}
	for _, name := range agentapi.PoolerFiles[:2] {
		if _, ok := b.Files[name]; !ok {
			return fmt.Errorf("the bundle has no %s", name)
		}
	}
	if got := agentapi.PoolerHash(b.Files); got != b.Hash {
		return fmt.Errorf("bundle hash %s does not match its content (%s)", b.Hash, got)
	}
	if b.Generation <= 0 {
		return errors.New("bundle generation must be positive")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if b.Generation < p.st.Generation {
		return errOlder
	}
	// The auth file first: a new route is only useful once its user can log
	// in. The key before the certificate, so the pair never mismatches in
	// the direction PgBouncer can't load.
	for _, name := range []string{"userlist.txt", "hba.conf", "server.key", "server.crt", "databases.ini"} {
		data, ok := b.Files[name]
		if !ok {
			continue
		}
		if _, err := pooler.WriteFileAtomic(filepath.Join(p.cfg.Dir, name), data, p.cfg.FileMode); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	p.st.Generation, p.st.Hash = b.Generation, b.Hash
	if p.st.Expected < b.Generation {
		p.st.Expected = b.Generation
	}
	return p.saveLocked()
}

func (p *poolerHost) expect(e agentapi.PoolerExpected) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e.Generation <= p.st.Expected {
		return nil
	}
	p.st.Expected = e.Generation
	return p.saveLocked()
}

func listening(addr string) bool {
	if addr == "" {
		return false
	}
	c, err := net.DialTimeout("tcp", addr, 700*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func (p *poolerHost) status() agentapi.PoolerStatus {
	p.mu.Lock()
	st := agentapi.PoolerStatus{
		Generation: p.st.Generation, Hash: p.st.Hash, Expected: p.st.Expected,
		VRRPState: p.vrrp, ServerID: p.cfg.ServerID, ClaimError: p.claimErr,
	}
	p.mu.Unlock()
	st.Session = listening(p.cfg.SessionAddr)
	st.Transaction = listening(p.cfg.PooledAddr)
	st.Stale = st.Expected > st.Generation
	var why []string
	if st.Generation == 0 {
		why = append(why, "no configuration received yet")
	}
	if st.Stale {
		why = append(why, fmt.Sprintf("serving generation %d, xenibase-server has %d", st.Generation, st.Expected))
	}
	if !st.Session {
		why = append(why, "the session PgBouncer is not accepting connections")
	}
	if !st.Transaction {
		why = append(why, "the transaction PgBouncer is not accepting connections")
	}
	st.Ready = len(why) == 0
	st.Reason = strings.Join(why, "; ")
	return st
}

// vrrpStates are keepalived's notify states.
var vrrpStates = map[string]bool{"MASTER": true, "BACKUP": true, "FAULT": true, "STOP": true}

// setVRRP records keepalived's state. On MASTER it claims the floating IP
// in the background, retrying until it succeeds or the state changes.
func (p *poolerHost) setVRRP(state string) {
	p.mu.Lock()
	prev := p.vrrp
	p.vrrp = state
	if p.claimAbort != nil {
		p.claimAbort()
		p.claimAbort = nil
	}
	if state != "MASTER" {
		p.mu.Unlock()
		if prev != state {
			p.log.Info("keepalived state", "state", state, "was", prev)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	p.claimAbort = cancel
	p.mu.Unlock()
	p.log.Info("keepalived made this host MASTER; claiming the floating IP", "was", prev, "server", p.cfg.ServerID)
	go p.claim(ctx)
}

func (p *poolerHost) claim(ctx context.Context) {
	start := time.Now()
	delay := 250 * time.Millisecond
	for {
		actx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := p.cfg.FloatIP.Assign(actx, p.cfg.ServerID)
		cancel()
		p.mu.Lock()
		if err == nil {
			p.claimErr = ""
		} else {
			p.claimErr = err.Error()
		}
		p.mu.Unlock()
		if err == nil {
			p.log.Info("floating IP assigned to this host", "ms", time.Since(start).Milliseconds())
			return
		}
		p.log.Warn("could not assign the floating IP; retrying", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 5*time.Second)
	}
}

// Pooler handlers on the mTLS API.

func (s *Service) poolerConfig(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		fail(w, http.StatusNotFound, errors.New("this agent is not a pooler host"))
		return
	}
	var b agentapi.PoolerBundle
	// Bundles carry every user's verifier: allow far more than other calls.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&b); err != nil {
		fail(w, http.StatusBadRequest, fmt.Errorf("bad request: %w", err))
		return
	}
	switch err := s.pool.apply(b); {
	case errors.Is(err, errOlder):
		fail(w, http.StatusConflict, err)
		return
	case err != nil:
		s.log.Warn("pooler bundle refused", "generation", b.Generation, "err", err)
		fail(w, http.StatusUnprocessableEntity, err)
		return
	}
	if len(s.pool.cfg.PidFiles) > 0 {
		if err := signalPgBouncers(s.pool.cfg.PidFiles); err != nil {
			s.log.Warn("could not signal the PgBouncers to reload", "err", err)
		}
	}
	s.log.Info("pooler configuration written", "generation", b.Generation, "hash", b.Hash[:12])
	writeJSON(w, http.StatusOK, s.pool.status())
}

func (s *Service) poolerExpected(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		fail(w, http.StatusNotFound, errors.New("this agent is not a pooler host"))
		return
	}
	var e agentapi.PoolerExpected
	if !decode(w, r, &e) {
		return
	}
	if err := s.pool.expect(e); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, s.pool.status())
}

func (s *Service) poolerStatus(w http.ResponseWriter, _ *http.Request) {
	if s.pool == nil {
		fail(w, http.StatusNotFound, errors.New("this agent is not a pooler host"))
		return
	}
	writeJSON(w, http.StatusOK, s.pool.status())
}

// LocalHandler is the unauthenticated API keepalived calls on 127.0.0.1:
//
//	GET  /ready          200 when this host may hold the floating IP (check script)
//	POST /vrrp/{state}   keepalived's notify (MASTER, BACKUP, FAULT, STOP)
//
// It must only ever listen on a loopback address.
func (s *Service) LocalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, _ *http.Request) {
		if s.pool == nil {
			http.Error(w, "not a pooler host", http.StatusNotFound)
			return
		}
		st := s.pool.status()
		if !st.Ready {
			http.Error(w, st.Reason, http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ready\n")
	})
	mux.HandleFunc("POST /vrrp/{state}", func(w http.ResponseWriter, r *http.Request) {
		if s.pool == nil {
			http.Error(w, "not a pooler host", http.StatusNotFound)
			return
		}
		state := strings.ToUpper(r.PathValue("state"))
		if !vrrpStates[state] {
			http.Error(w, "unknown state", http.StatusBadRequest)
			return
		}
		s.pool.setVRRP(state)
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

// ServeLocal runs LocalHandler on addr, which must be a loopback address.
func (s *Service) ServeLocal(ctx context.Context, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("the local pooler API must listen on a loopback address, not %q", addr)
	}
	srv := &http.Server{Addr: addr, Handler: s.LocalHandler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
