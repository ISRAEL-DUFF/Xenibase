package main

import (
	"bytes"
	"crypto/rand"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/israel-duff/xenibase/internal/backupfmt"
	"github.com/israel-duff/xenibase/internal/floatip"
)

func TestDecrypt(t *testing.T) {
	dir := t.TempDir()
	bk := make([]byte, 32)
	_, _ = rand.Read(bk)
	keyFile := filepath.Join(dir, "key.txt")
	if err := os.WriteFile(keyFile, []byte("# comment\n"+backupfmt.EncodeKey(bk)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plain := bytes.Repeat([]byte("PGDMP archive bytes "), 10000)
	fk, wrapped, err := backupfmt.NewFileKey(bk)
	if err != nil {
		t.Fatal(err)
	}
	var enc bytes.Buffer
	w, err := backupfmt.NewWriter(&enc, fk, wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(dir, "x.dump.enc")
	if err := os.WriteFile(in, enc.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "x.dump")
	if err := run([]string{"decrypt", "--key-file", keyFile, "--in", in, "--out", out}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, plain) {
		t.Fatal("decrypted bytes differ")
	}

	// The wrong key fails and leaves no partial output.
	_, _ = rand.Read(bk)
	_ = os.WriteFile(keyFile, []byte(backupfmt.EncodeKey(bk)), 0o600)
	out2 := filepath.Join(dir, "y.dump")
	if err := run([]string{"decrypt", "--key-file", keyFile, "--in", in, "--out", out2}); err == nil {
		t.Fatal("decrypted with the wrong key")
	}
	if _, err := os.Stat(out2); !os.IsNotExist(err) {
		t.Fatal("left a partial output behind")
	}
}

// A pooler host assigns the floating IP itself only when told to: by
// default it holds no Hetzner client, even with a token in its environment.
func TestFloatingIPAssignedOnlyWhenEnabled(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	o := &opts{hetznerToken: "full-access-token", floatingIP: "42", hetznerAPI: floatip.DefaultHetznerAPI}
	fip, err := floatingIP(o, "1001", quiet)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fip.(floatip.None); !ok {
		t.Fatalf("the agent assigns the floating IP by default: %T", fip)
	}
	o.assignIP = true
	if fip, err = floatingIP(o, "1001", quiet); err != nil {
		t.Fatal(err)
	}
	if _, ok := fip.(*floatip.Hetzner); !ok {
		t.Fatalf("--assign-floating-ip: %T", fip)
	}
	o.hetznerToken = ""
	if _, err := floatingIP(o, "1001", quiet); err == nil {
		t.Fatal("--assign-floating-ip without a token")
	}
}
