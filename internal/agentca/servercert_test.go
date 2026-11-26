package agentca

import (
	"crypto/ecdsa"
	"crypto/x509"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/agentapi"
)

// A server's client certificate lasts a day, has its own key, keeps the
// subject every agent checks, and names the server.
func TestServerClientCertNamed(t *testing.T) {
	ca, _ := New()
	now := time.Now()
	a, err := ca.ServerClientCertFor("host a/1", now)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ca.ServerClientCertFor("host a/1", now)
	if d := a.Leaf.NotAfter.Sub(now); d > 25*time.Hour || d < 23*time.Hour {
		t.Fatalf("valid for %s, want about a day", d)
	}
	if a.Leaf.Subject.CommonName != ServerCN || len(a.Leaf.ExtKeyUsage) != 1 || a.Leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatalf("subject %q, usage %v", a.Leaf.Subject.CommonName, a.Leaf.ExtKeyUsage)
	}
	if name, ok := ServerNameFromCert(a.Leaf); !ok || name != "host a/1" || a.Leaf.Subject.OrganizationalUnit[0] != "host a/1" {
		t.Fatalf("named %q (%v), OU %v", name, ok, a.Leaf.Subject.OrganizationalUnit)
	}
	if Serial(a.Leaf) == Serial(b.Leaf) || a.Leaf.PublicKey.(*ecdsa.PublicKey).Equal(b.Leaf.PublicKey) {
		t.Fatal("two certificates share a serial or a key")
	}
	if _, err := a.Leaf.Verify(x509.VerifyOptions{Roots: ca.Pool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := ServerNameFromCert(must(ca.ServerClientCert()).Leaf); ok {
		t.Fatal("an unnamed certificate has a name")
	}
}

// Allow lists verify only against the CA that signed them, unchanged.
func TestSignedList(t *testing.T) {
	ca, _ := New()
	other, _ := New()
	l := agentapi.ClientSerialList{Generation: 7, Serials: []agentapi.ClientSerial{{Serial: "ab", Server: "a", ExpiresAt: time.Now().Add(time.Hour)}}}
	cs, err := ca.SignList(l)
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifyList(ca.Cert(), cs)
	if err != nil || got.Generation != 7 || len(got.Serials) != 1 || got.Serials[0].Serial != "ab" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := VerifyList(other.Cert(), cs); err == nil {
		t.Fatal("verified against another CA")
	}
	tampered := cs
	tampered.List = append([]byte(nil), cs.List...)
	tampered.List[len(tampered.List)-3] ^= 1
	if _, err := VerifyList(ca.Cert(), tampered); err == nil {
		t.Fatal("a changed list verified")
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
