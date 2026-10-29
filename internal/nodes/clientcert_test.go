package nodes

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/agentca"
)

// SetClientCertIssuedAt replaces the server's client certificate with one
// issued at t (for TestClientCertRenewed in the external test package).
func SetClientCertIssuedAt(s *Service, t time.Time) error {
	cert, err := s.ca.ServerClientCertAt(t)
	if err != nil {
		return err
	}
	s.certs.mu.Lock()
	s.certs.cert = &cert
	s.certs.mu.Unlock()
	return nil
}

func TestClientCertRenewal(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	var mu sync.Mutex
	issued := 0
	fail := false
	issue := func(at time.Time) (tls.Certificate, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return tls.Certificate{}, errors.New("CA unavailable")
		}
		issued++
		return fakeCert(at)
	}
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	c, err := newClientCerts(issue, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	set := func(d time.Duration) { mu.Lock(); now = start.Add(d); mu.Unlock() }
	day := 24 * time.Hour

	first, _ := c.get()
	set(22 * day) // eight days left: kept
	if got, _ := c.get(); got != first || issued != 1 {
		t.Fatalf("renewed with eight days left (issued %d)", issued)
	}
	set(24 * day) // six days left: renewed
	second, err := c.get()
	if err != nil || second == first || issued != 2 || !second.Leaf.NotAfter.Equal(start.Add(24*day+agentca.ServerClientCertValidity)) {
		t.Fatalf("not renewed with six days left: issued %d, err %v", issued, err)
	}

	// A failed renewal keeps the current certificate while it is valid,
	// and is retried.
	set(24*day + agentca.ServerClientCertValidity - 3*day)
	mu.Lock()
	fail = true
	mu.Unlock()
	if got, err := c.get(); err != nil || got != second {
		t.Fatalf("failed renewal: %v", err)
	}
	set(24*day + agentca.ServerClientCertValidity + time.Minute) // expired
	if _, err := c.get(); err == nil {
		t.Fatal("an expired certificate was handed out")
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if got, err := c.get(); err != nil || got == second {
		t.Fatalf("not renewed after the CA came back: %v", err)
	}

	// Handshakes ask concurrently.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.getClientCertificate(nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

// fakeCert stands in for agentca's: only its validity matters here.
func fakeCert(at time.Time) (tls.Certificate, error) {
	return tls.Certificate{Leaf: &x509.Certificate{NotBefore: at.Add(-time.Hour), NotAfter: at.Add(agentca.ServerClientCertValidity)}}, nil
}
