package edge

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestPoolerURLVerifiesWithAPlatformCA: with verify-full and a root
// certificate the edge's pooler connections check the pooler's
// certificate and hostname against that CA.
func TestPoolerURLVerifiesWithAPlatformCA(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "xenibase test CA"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}

	e := New(Config{PoolerSSLMode: "verify-full", PoolerRootCert: ca})
	cfg, err := pgx.ParseConfig(e.poolerURL("p_x_edge", "pw", "db.ng.example.com", 6543, "p_x", "xenibase-edge"))
	if err != nil {
		t.Fatal(err)
	}
	tc := cfg.TLSConfig
	if tc == nil || tc.InsecureSkipVerify || tc.ServerName != "db.ng.example.com" || tc.RootCAs == nil {
		t.Fatalf("TLS config: %+v", tc)
	}
	if len(cfg.Fallbacks) != 0 {
		t.Fatalf("verify-full must not fall back to plain text: %d fallbacks", len(cfg.Fallbacks))
	}

	// The default, require, encrypts without checking.
	cfg, err = pgx.ParseConfig(New(Config{}).poolerURL("u", "p", "h", 6543, "d", "a"))
	if err != nil || cfg.TLSConfig == nil || len(cfg.Fallbacks) != 0 {
		t.Fatalf("require: %+v %v", cfg, err)
	}
}
