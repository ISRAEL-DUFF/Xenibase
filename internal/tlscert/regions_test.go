package tlscert

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRegionCertificates: a region with its own pooler hostname gets a
// certificate for it in its own directory (not the home pair, which names
// only the home hostname), follows a hostname change, and is left alone
// once it has no hostname of its own.
func TestRegionCertificates(t *testing.T) {
	root := t.TempDir()
	dir := func(region string) string { return filepath.Join(root, "regions", region) }
	var mu sync.Mutex
	hosts := map[string]string{"ng-lagos": "db.ng.example.com"}
	var reloads atomic.Int32
	rs := NewRegions(Config{Mode: ModeSelfSigned, Dir: root, Log: quiet,
		Reload: func(context.Context) error { reloads.Add(1); return nil }},
		dir, func(context.Context) (map[string]string, error) {
			mu.Lock()
			defer mu.Unlock()
			out := map[string]string{}
			for k, v := range hosts {
				out[k] = v
			}
			return out, nil
		})
	ctx := context.Background()
	if err := rs.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if c := servedCert(t, dir("ng-lagos")); c.VerifyHostname("db.ng.example.com") != nil {
		t.Fatalf("the region's certificate names %v", c.DNSNames)
	}
	if st := rs.Status()["ng-lagos"]; st.Host != "db.ng.example.com" {
		t.Fatalf("status: %+v", st)
	}

	// Run keeps it current: a new hostname gets a new certificate.
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); rs.Run(runCtx, 20*time.Millisecond) }()
	mu.Lock()
	hosts["ng-lagos"] = "db.lagos.example.com"
	mu.Unlock()
	deadline := time.Now().Add(10 * time.Second)
	for servedCert(t, dir("ng-lagos")).VerifyHostname("db.lagos.example.com") != nil || reloads.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the region's certificate did not follow its new hostname, or the poolers were not reloaded")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// No hostname of its own any more: its manager stops.
	mu.Lock()
	delete(hosts, "ng-lagos")
	mu.Unlock()
	for len(rs.Status()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the region's manager kept running")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	// Files mode: the operator's pair is everyone's; nothing is managed.
	files := NewRegions(Config{Mode: ModeFiles, Log: quiet}, dir, func(context.Context) (map[string]string, error) {
		return map[string]string{"za": "db.za.example.com"}, nil
	})
	if err := files.Sync(ctx); err != nil || len(files.Status()) != 0 {
		t.Fatalf("files mode: %v %v", err, files.Status())
	}
}

// servedCert reads the certificate in dir (alone: while a manager
// replaces the pair, the key may already be the next one's).
func servedCert(t *testing.T, dir string) *x509.Certificate {
	t.Helper()
	certPEM, err := os.ReadFile(filepath.Join(dir, CertFile))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode(certPEM)
	if b == nil {
		t.Fatalf("no certificate in %s", dir)
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
