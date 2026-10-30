package nodes

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"sync"
	"time"

	"github.com/israel-duff/xenibase/internal/agentca"
)

// clientCertRenewBefore is how long before it expires this server's client
// certificate (agentca.ServerClientCertValidity, a day) is reissued: at
// half-life, so a renewal that fails has twelve hours of retries (every
// handshake and every pass of Run) before agents refuse the server.
const clientCertRenewBefore = agentca.ServerClientCertValidity / 2

// clientCertRegisterTimeout bounds recording a new serial in the metadata
// database, which happens inside a TLS handshake when the current
// certificate is due.
const clientCertRegisterTimeout = 15 * time.Second

// clientCerts holds this server's client certificate and reissues it when
// half of it is left, or when it was revoked. A new certificate is
// registered (its serial recorded, raising the allow list's generation)
// before it is handed out, and Agent.do pushes the list naming it to each
// agent before the first call with it. It is safe for concurrent use:
// every handshake with an agent asks it for the certificate.
type clientCerts struct {
	issue func(now time.Time) (tls.Certificate, error)
	// register records a new certificate's serial; after runs once it is
	// in use (refreshing the allow list and closing idle connections made
	// with the old one). Either may be nil.
	register func(ctx context.Context, leaf *x509.Certificate) error
	after    func(ctx context.Context)
	now      func() time.Time
	log      *slog.Logger

	mu    sync.Mutex
	cert  *tls.Certificate
	stale bool // revoked: reissue at the next get
}

func newClientCerts(issue func(time.Time) (tls.Certificate, error), now func() time.Time, log *slog.Logger) *clientCerts {
	return &clientCerts{issue: issue, now: now, log: log}
}

// get returns the current certificate, reissued first when no more than
// clientCertRenewBefore is left or it was revoked. A failed reissue keeps
// the current one while it is still valid (the next handshake tries
// again).
func (c *clientCerts) get() (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.cert != nil && !c.stale && c.cert.Leaf.NotAfter.Sub(now) > clientCertRenewBefore {
		return c.cert, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), clientCertRegisterTimeout)
	defer cancel()
	cert, err := c.issue(now)
	if err == nil && c.register != nil {
		err = c.register(ctx, cert.Leaf)
	}
	if err != nil {
		if c.cert != nil && now.Before(c.cert.Leaf.NotAfter) {
			c.log.Warn("renewing the agent client certificate failed; keeping the current one", "expires", c.cert.Leaf.NotAfter, "revoked", c.stale, "err", err)
			return c.cert, nil
		}
		return nil, err
	}
	if c.cert != nil {
		c.log.Info("agent client certificate renewed", "serial", agentca.Serial(cert.Leaf), "expires", cert.Leaf.NotAfter, "revoked", c.stale)
	}
	c.cert, c.stale = &cert, false
	if c.after != nil {
		c.after(ctx)
	}
	return c.cert, nil
}

// serial is the current certificate's serial, or "".
func (c *clientCerts) serial() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cert == nil {
		return ""
	}
	return agentca.Serial(c.cert.Leaf)
}

// markStale has the next get reissue the certificate: its serial is no
// longer live (revoked by an admin).
func (c *clientCerts) markStale(serial string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cert != nil && agentca.Serial(c.cert.Leaf) == serial {
		c.stale = true
	}
}

// getClientCertificate is tls.Config.GetClientCertificate.
func (c *clientCerts) getClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return c.get()
}

// ClientCertExpiry renews the server's agent client certificate if it is
// due and returns when the current one expires (for the alert that fires
// when renewing keeps failing).
func (s *Service) ClientCertExpiry() time.Time {
	if cert, err := s.certs.get(); err == nil {
		return cert.Leaf.NotAfter
	}
	s.certs.mu.Lock()
	defer s.certs.mu.Unlock()
	if s.certs.cert == nil {
		return time.Time{}
	}
	return s.certs.cert.Leaf.NotAfter
}
