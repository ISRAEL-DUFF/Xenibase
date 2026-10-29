package nodes

import (
	"crypto/tls"
	"log/slog"
	"sync"
	"time"
)

// clientCertRenewBefore is how long before it expires xenibase-server's
// client certificate (agentca.ServerClientCertValidity, 30 days) is
// reissued. Agents check its expiry on every handshake, so a server that
// kept the one it issued at start lost every agent after 30 days.
const clientCertRenewBefore = 7 * 24 * time.Hour

// clientCerts holds the server's client certificate and reissues it when
// little of it is left. It is safe for concurrent use: every handshake
// with an agent asks it for the certificate.
type clientCerts struct {
	issue func(now time.Time) (tls.Certificate, error)
	now   func() time.Time
	log   *slog.Logger

	mu   sync.Mutex
	cert *tls.Certificate
}

func newClientCerts(issue func(time.Time) (tls.Certificate, error), now func() time.Time, log *slog.Logger) (*clientCerts, error) {
	c := &clientCerts{issue: issue, now: now, log: log}
	if _, err := c.get(); err != nil {
		return nil, err
	}
	return c, nil
}

// get returns the current certificate, reissued first when less than
// clientCertRenewBefore is left. A failed reissue keeps the current one
// while it is still valid (the next handshake tries again).
func (c *clientCerts) get() (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.cert != nil && c.cert.Leaf.NotAfter.Sub(now) > clientCertRenewBefore {
		return c.cert, nil
	}
	cert, err := c.issue(now)
	if err != nil {
		if c.cert != nil && now.Before(c.cert.Leaf.NotAfter) {
			c.log.Warn("renewing the agent client certificate failed; keeping the current one", "expires", c.cert.Leaf.NotAfter, "err", err)
			return c.cert, nil
		}
		return nil, err
	}
	if c.cert != nil {
		c.log.Info("agent client certificate renewed", "expires", cert.Leaf.NotAfter)
	}
	c.cert = &cert
	return c.cert, nil
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
	return s.certs.cert.Leaf.NotAfter
}
