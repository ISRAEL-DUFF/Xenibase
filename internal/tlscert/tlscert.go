// Package tlscert keeps the edge poolers' TLS certificate for the DB
// hostname current (spec §3.3): self-signed, from files, or from an ACME CA
// such as Let's Encrypt via HTTP-01. The certificate and key are written
// into the pooler config directory, then the poolers are reloaded.
package tlscert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"

	"github.com/israel-duff/xenibase/internal/pooler"
)

// File names in the pooler config directory, as pgbouncer.ini expects.
const (
	CertFile = "server.crt"
	KeyFile  = "server.key"
)

// Mode selects where the certificate comes from.
type Mode string

// Modes.
const (
	ModeSelfSigned Mode = "self-signed"
	ModeACME       Mode = "acme"
	ModeFiles      Mode = "files"
	ModeOff        Mode = "off"
)

// ParseMode validates a mode name.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case ModeSelfSigned, ModeACME, ModeFiles, ModeOff:
		return m, nil
	}
	return "", fmt.Errorf("unknown pooler TLS mode %q (want self-signed, acme, files, or off)", s)
}

// Config configures a Manager.
type Config struct {
	Mode Mode
	// Dir and FileMode are where and how the cert and key are written.
	Dir      string
	FileMode os.FileMode
	// Reload makes the poolers re-read the certificate.
	Reload func(context.Context) error

	// ACME: DataDir holds certmagic's storage (account, certificates);
	// CA defaults to Let's Encrypt; Roots trusts a private CA's directory
	// (tests use Pebble).
	DataDir   string
	ACMEEmail string
	ACMECA    string
	ACMERoots *x509.CertPool
	// ChallengePort is where HTTPChallengeHandler is served (xenibase-server's
	// own listen port). certmagic finds it in use and leaves solving to that
	// handler instead of binding :80 itself. Run must start after the
	// server is listening.
	ChallengePort int

	// Files mode: operator-supplied PEM paths.
	SourceCert, SourceKey string

	Log *slog.Logger
}

// Status describes the current certificate.
type Status struct {
	Mode     Mode
	State    string // ok, pending, error, off
	Host     string
	Issuer   string
	NotAfter time.Time
	Err      string
}

// Manager keeps the poolers' certificate in step with the DB hostname.
type Manager struct {
	cfg     Config
	trigger chan struct{}

	magic  *certmagic.Config
	issuer *certmagic.ACMEIssuer
	cache  *certmagic.Cache

	mu     sync.Mutex
	host   string
	status Status
}

// New returns a Manager for host.
func New(cfg Config, host string) (*Manager, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.FileMode == 0 {
		cfg.FileMode = 0o640
	}
	m := &Manager{cfg: cfg, trigger: make(chan struct{}, 1), host: host,
		status: Status{Mode: cfg.Mode, State: "pending", Host: host}}
	switch cfg.Mode {
	case ModeOff:
		m.status.State = "off"
	case ModeFiles:
		if cfg.SourceCert == "" || cfg.SourceKey == "" {
			return nil, errors.New("pooler TLS mode files needs a certificate and key file")
		}
	case ModeACME:
		if cfg.DataDir == "" {
			return nil, errors.New("pooler TLS mode acme needs a data directory")
		}
		if cfg.ChallengePort <= 0 {
			return nil, errors.New("pooler TLS mode acme needs the port that serves HTTP-01 challenges")
		}
		m.setupACME()
	}
	return m, nil
}

func (m *Manager) setupACME() {
	var magic *certmagic.Config
	m.cache = certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) { return magic, nil },
		Logger:           zap.NewNop(),
	})
	magic = certmagic.New(m.cache, certmagic.Config{
		Storage: &certmagic.FileStorage{Path: filepath.Join(m.cfg.DataDir, "certmagic")},
		Logger:  zap.NewNop(),
		OnEvent: func(_ context.Context, event string, data map[string]any) error {
			switch event {
			case "cert_obtained":
				m.cfg.Log.Info("pooler certificate obtained", "host", data["identifier"], "renewal", data["renewal"])
				m.Refresh() // export the new certificate
			case "cert_failed":
				m.cfg.Log.Warn("pooler certificate request failed", "host", data["identifier"], "error", data["error"])
			}
			return nil
		},
	})
	ca := m.cfg.ACMECA
	if ca == "" {
		ca = certmagic.LetsEncryptProductionCA
	}
	m.issuer = certmagic.NewACMEIssuer(magic, certmagic.ACMEIssuer{
		CA: ca, Email: m.cfg.ACMEEmail, Agreed: true, TrustedRoots: m.cfg.ACMERoots,
		// HTTP-01 only: :80 reaches us through Caddy; :443 is Caddy's.
		DisableTLSALPNChallenge: true,
		AltHTTPPort:             m.cfg.ChallengePort,
		Logger:                  zap.NewNop(),
	})
	magic.Issuers = []certmagic.Issuer{m.issuer}
	m.magic = magic
}

// SetHost switches the certificate to a new DB hostname.
func (m *Manager) SetHost(host string) {
	m.mu.Lock()
	m.host = host
	m.status.Host = host
	m.mu.Unlock()
	m.Refresh()
}

// Refresh schedules a check of the certificate.
func (m *Manager) Refresh() {
	select {
	case m.trigger <- struct{}{}:
	default:
	}
}

// Status returns the current status.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// HTTPChallengeHandler answers ACME HTTP-01 challenges ahead of next. Caddy
// forwards /.well-known/acme-challenge/ for the DB hostname to xenibase-server.
func (m *Manager) HTTPChallengeHandler(next http.Handler) http.Handler {
	if m.issuer == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.handleChallenge(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleChallenge answers r if it is one of this Manager's HTTP-01
// challenges.
func (m *Manager) handleChallenge(w http.ResponseWriter, r *http.Request) bool {
	return m.issuer != nil && certmagic.LooksLikeHTTPChallenge(r) && m.issuer.HandleHTTPChallenge(w, r)
}

// Bootstrap makes sure some certificate is in place, so the poolers can
// start: in ACME mode a self-signed one stands in until the real one
// arrives. It does not reload the poolers.
func (m *Manager) Bootstrap() error {
	if m.cfg.Mode == ModeOff {
		return nil
	}
	if m.cfg.Mode == ModeFiles {
		_, err := m.ensureFiles()
		return err
	}
	if _, err := m.currentCert(); err == nil {
		return nil
	}
	_, err := m.writeSelfSigned(m.Host())
	return err
}

// Host returns the current DB hostname.
func (m *Manager) Host() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.host
}

// Run keeps the certificate current until ctx ends: on Refresh, after a
// failure (with backoff), and twice a day.
func (m *Manager) Run(ctx context.Context) {
	if m.cache != nil {
		defer m.cache.Stop()
	}
	m.Refresh()
	retry := time.Minute
	tick := time.NewTicker(12 * time.Hour)
	defer tick.Stop()
	var retryC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.trigger:
		case <-tick.C:
		case <-retryC:
		}
		retryC = nil
		if err := m.ensure(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			m.fail(err)
			m.cfg.Log.Warn("pooler TLS certificate", "err", err, "retry_in", retry)
			retryC = time.After(retry)
			retry = min(retry*2, time.Hour)
			continue
		}
		retry = time.Minute
	}
}

// EnsureNow runs one check synchronously (tests and startup).
func (m *Manager) EnsureNow(ctx context.Context) error {
	err := m.ensure(ctx)
	if err != nil {
		m.fail(err)
	}
	return err
}

func (m *Manager) fail(err error) {
	m.mu.Lock()
	m.status.State = "error"
	m.status.Err = err.Error()
	m.mu.Unlock()
}

func (m *Manager) ensure(ctx context.Context) error {
	host := m.Host()
	var changed bool
	var err error
	switch m.cfg.Mode {
	case ModeOff:
		return nil
	case ModeFiles:
		changed, err = m.ensureFiles()
	case ModeSelfSigned:
		changed, err = m.ensureSelfSigned(host)
	case ModeACME:
		if !acmeable(host) {
			// Public CAs only issue for public DNS names.
			if changed, err = m.ensureSelfSigned(host); err == nil {
				err = fmt.Errorf("%q is not a public DNS name; serving a self-signed certificate until the DB hostname is set", host)
			}
			break
		}
		changed, err = m.ensureACME(ctx, host)
	}
	if changed && m.cfg.Reload != nil {
		if rerr := m.cfg.Reload(ctx); rerr != nil {
			err = errors.Join(err, fmt.Errorf("reload poolers: %w", rerr))
		}
	}
	m.recordCurrent(err)
	return err
}

func (m *Manager) recordCurrent(err error) {
	cert, cerr := m.currentCert()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.Host = m.host
	if cerr == nil {
		m.status.Issuer = issuerName(cert)
		m.status.NotAfter = cert.NotAfter
	}
	switch {
	case err != nil:
		m.status.State, m.status.Err = "error", err.Error()
	case cerr != nil:
		m.status.State, m.status.Err = "error", cerr.Error()
	default:
		m.status.State, m.status.Err = "ok", ""
	}
}

func issuerName(c *x509.Certificate) string {
	if c.Issuer.CommonName != "" {
		return c.Issuer.CommonName
	}
	if len(c.Issuer.Organization) > 0 {
		return c.Issuer.Organization[0]
	}
	return c.Issuer.String()
}

func acmeable(host string) bool {
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	return strings.Contains(host, ".") && host != "localhost" && !strings.HasSuffix(host, ".localhost")
}

// currentCert parses the certificate the poolers currently serve.
func (m *Manager) currentCert() (*x509.Certificate, error) {
	certPEM, err := os.ReadFile(filepath.Join(m.cfg.Dir, CertFile))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(m.cfg.Dir, KeyFile))
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(pair.Certificate[0])
}

func (m *Manager) write(certPEM, keyPEM []byte) (bool, error) {
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return false, fmt.Errorf("certificate and key do not match: %w", err)
	}
	// Key first: a new certificate must never meet an old key.
	kc, err := pooler.WriteFileAtomic(filepath.Join(m.cfg.Dir, KeyFile), keyPEM, m.cfg.FileMode)
	if err != nil {
		return false, err
	}
	cc, err := pooler.WriteFileAtomic(filepath.Join(m.cfg.Dir, CertFile), certPEM, m.cfg.FileMode)
	return kc || cc, err
}

func (m *Manager) ensureFiles() (bool, error) {
	certPEM, err := os.ReadFile(m.cfg.SourceCert)
	if err != nil {
		return false, err
	}
	keyPEM, err := os.ReadFile(m.cfg.SourceKey)
	if err != nil {
		return false, err
	}
	return m.write(certPEM, keyPEM)
}

// coversHost reports whether c is valid for host for at least 30 days.
func coversHost(c *x509.Certificate, host string) bool {
	return c.VerifyHostname(host) == nil && time.Until(c.NotAfter) > 30*24*time.Hour
}

func (m *Manager) ensureSelfSigned(host string) (bool, error) {
	if c, err := m.currentCert(); err == nil && coversHost(c, host) {
		return false, nil
	}
	return m.writeSelfSigned(host)
}

func (m *Manager) writeSelfSigned(host string) (bool, error) {
	certPEM, keyPEM, err := SelfSigned(host, 365*24*time.Hour)
	if err != nil {
		return false, err
	}
	m.cfg.Log.Info("pooler TLS: self-signed certificate written", "host", host)
	return m.write(certPEM, keyPEM)
}

func (m *Manager) ensureACME(ctx context.Context, host string) (bool, error) {
	if c, err := m.currentCert(); err != nil || !m.isFromCA(c) || c.VerifyHostname(host) != nil {
		// Keep the poolers serving something while the CA answers; the
		// status stays "pending" until a CA-issued certificate is in place.
		if _, err := m.ensureSelfSigned(host); err != nil {
			return false, err
		}
		m.mu.Lock()
		m.status.State = "pending"
		m.mu.Unlock()
	}
	// Obtains if missing, renews if due, and keeps maintaining it.
	if err := m.magic.ManageSync(ctx, []string{host}); err != nil {
		return false, fmt.Errorf("obtain certificate for %s: %w", host, err)
	}
	creds, err := m.magic.ClientCredentials(ctx, []string{host})
	if err != nil || len(creds) == 0 {
		return false, fmt.Errorf("load certificate for %s: %w", host, err)
	}
	certPEM, keyPEM, err := encodePair(creds[0])
	if err != nil {
		return false, err
	}
	return m.write(certPEM, keyPEM)
}

// isFromCA reports whether c was issued by a CA rather than self-signed.
func (m *Manager) isFromCA(c *x509.Certificate) bool {
	return c.Issuer.String() != c.Subject.String()
}

func encodePair(c tls.Certificate) ([]byte, []byte, error) {
	var certPEM []byte
	for _, der := range c.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(c.PrivateKey)
	if err != nil {
		return nil, nil, err
	}
	return certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

// SelfSigned returns a self-signed ECDSA P-256 certificate for host (a DNS
// name or IP address), valid for validity.
func SelfSigned(host string, validity time.Duration) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host, Organization: []string{"Xenibase self-signed"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}
