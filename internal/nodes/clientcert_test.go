package nodes

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/agentca"
)

// SetClientCertIssuedAt replaces the server's client certificate with a
// registered one issued at t (for the external test package).
func SetClientCertIssuedAt(s *Service, t time.Time) error {
	cert, err := s.ca.ServerClientCertFor(s.name, t)
	if err != nil {
		return err
	}
	if err := s.registerClientCert(context.Background(), cert.Leaf); err != nil {
		return err
	}
	s.certs.mu.Lock()
	s.certs.cert = &cert
	s.certs.mu.Unlock()
	s.agents.closeIdle() // the next call handshakes with it
	return nil
}

// CurrentClientCert is the server's client certificate in use (a test's "stolen
// copy").
func CurrentClientCert(s *Service) tls.Certificate {
	s.certs.mu.Lock()
	defer s.certs.mu.Unlock()
	return *s.certs.cert
}

// NewServiceNamed is NewService for a server with the given name (two
// servers sharing one metadata database in a test).
func NewServiceNamed(db *pgxpool.Pool, ca *agentca.CA, name string, log *slog.Logger) (*Service, error) {
	return newService(db, ca, "", name, log)
}

func TestClientCertRenewal(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	var mu sync.Mutex
	issued := 0
	fail := false
	var events []string // what happened, in order
	issue := func(at time.Time) (tls.Certificate, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return tls.Certificate{}, errors.New("CA unavailable")
		}
		issued++
		return fakeCert(at, issued)
	}
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	c := newClientCerts(issue, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.register = func(_ context.Context, leaf *x509.Certificate) error {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, "register "+leaf.SerialNumber.String())
		return nil
	}
	c.after = func(context.Context) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, "in use")
	}
	first, err := c.get()
	if err != nil {
		t.Fatal(err)
	}
	set := func(d time.Duration) { mu.Lock(); now = start.Add(d); mu.Unlock() }
	life := agentca.ServerClientCertValidity
	if life > 25*time.Hour {
		t.Fatalf("client certificates last %s: they should be short-lived", life)
	}

	set(life/2 - time.Minute) // just over half left: kept
	if got, _ := c.get(); got != first || issued != 1 {
		t.Fatalf("renewed with more than half its life left (issued %d)", issued)
	}
	set(life/2 + time.Minute) // under half left: renewed
	second, err := c.get()
	if err != nil || second == first || issued != 2 || !second.Leaf.NotAfter.Equal(start.Add(life/2+time.Minute+life)) {
		t.Fatalf("not renewed at half-life: issued %d, err %v", issued, err)
	}
	// Each new certificate is registered before it is handed out.
	if want := []string{"register 1", "in use", "register 2", "in use"}; !equal(events, want) {
		t.Fatalf("events %v, want %v", events, want)
	}

	// A failed renewal keeps the current certificate while it is valid,
	// and is retried; so does a failed registration.
	set(life/2 + time.Minute + life - time.Hour)
	mu.Lock()
	fail = true
	mu.Unlock()
	if got, err := c.get(); err != nil || got != second {
		t.Fatalf("failed renewal: %v", err)
	}
	set(life/2 + time.Minute + life + time.Minute) // expired
	if _, err := c.get(); err == nil {
		t.Fatal("an expired certificate was handed out")
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	c.register = func(context.Context, *x509.Certificate) error { return errors.New("database down") }
	if _, err := c.get(); err == nil {
		t.Fatal("an unregistered certificate was handed out")
	}
	c.register = nil
	third, err := c.get()
	if err != nil || third == second {
		t.Fatalf("not renewed after the CA came back: %v", err)
	}

	// A revoked certificate is replaced at the next get.
	c.markStale(agentca.Serial(third.Leaf))
	if got, err := c.get(); err != nil || got == third {
		t.Fatalf("revoked certificate kept: %v", err)
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

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fakeCert stands in for agentca's: only its validity and serial matter.
func fakeCert(at time.Time, serial int) (tls.Certificate, error) {
	return tls.Certificate{Leaf: &x509.Certificate{SerialNumber: big.NewInt(int64(serial)), NotBefore: at.Add(-time.Hour), NotAfter: at.Add(agentca.ServerClientCertValidity)}}, nil
}
