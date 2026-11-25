package pgpstream_test

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/israel-duff/xenibase/internal/pgpstream"
	"github.com/israel-duff/xenibase/internal/walg"
)

func key(t *testing.T) (priv, pub string) {
	t.Helper()
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	priv, err := walg.ProjectPGPKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	pub, err = walg.PublicKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return priv, pub
}

func encrypt(t *testing.T, pub string, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := pgpstream.Encrypt(&buf, pub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRoundTrip(t *testing.T) {
	priv, pub := key(t)
	plain := make([]byte, 3<<20)
	_, _ = rand.Read(plain)
	r, err := pgpstream.Decrypt(bytes.NewReader(encrypt(t, pub, plain)), priv)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("round trip: err %v, equal %v", err, bytes.Equal(got, plain))
	}
}

func TestTamperedFails(t *testing.T) {
	priv, pub := key(t)
	msg := encrypt(t, pub, bytes.Repeat([]byte("xenibase"), 10000))
	msg[len(msg)-40] ^= 1
	r, err := pgpstream.Decrypt(bytes.NewReader(msg), priv)
	if err == nil {
		_, err = io.ReadAll(r)
	}
	if err == nil {
		t.Fatal("a tampered message decrypted without error")
	}
}

func TestWrongKeyFails(t *testing.T) {
	_, pub := key(t)
	other, _ := key(t)
	if _, err := pgpstream.Decrypt(bytes.NewReader(encrypt(t, pub, []byte("x"))), other); err == nil {
		t.Fatal("decrypted with the wrong key")
	}
}

// TestGPGDecrypts is the "standard tools" half of V2 s6: gpg, given only the
// derived private key, opens a message the agent would write.
func TestGPGDecrypts(t *testing.T) {
	gpg, err := exec.LookPath("gpg")
	if err != nil {
		t.Skip("gpg not installed")
	}
	priv, pub := key(t)
	plain := []byte("PGDMP pretend archive\n")
	dir := t.TempDir()
	home := filepath.Join(dir, "gnupg")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "key.asc")
	msgFile := filepath.Join(dir, "backup.dump.gpg")
	_ = os.WriteFile(keyFile, []byte("README text before the key is ignored by gpg\n\n"+priv), 0o600)
	_ = os.WriteFile(msgFile, encrypt(t, pub, plain), 0o600)
	run := func(args ...string) ([]byte, string, error) {
		cmd := exec.Command(gpg, append([]string{"--homedir", home, "--batch", "--quiet"}, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		return out, stderr.String(), err
	}
	if _, stderr, err := run("--import", keyFile); err != nil {
		t.Fatalf("gpg --import: %v\n%s", err, stderr)
	}
	got, stderr, err := run("--decrypt", msgFile)
	if err != nil && strings.Contains(stderr, "Bad secret key") && libgcrypt(t, gpg) == "1.12.0" {
		// libgcrypt 1.12.0 (Ubuntu 26.04's) rejects valid Curve25519 secret
		// keys at random when gpg-agent first uses an imported one: its
		// secret-key check computes with MPIs short of limbs, and says the
		// curve's own base point isn't on the curve. Whether it fails
		// depends on the agent's memory (even GNUPGHOME's length); keys
		// GnuPG itself exports fail the same way, and 1.12.1 fixes it
		// (dev.gnupg.org/T8094). That our keys are well formed is checked
		// without gpg by walg's TestExportedSecretKeysAreConformant.
		t.Skipf("gpg's libgcrypt 1.12.0 rejected the key (T8094, fixed in 1.12.1):\n%s", stderr)
	}
	if err != nil {
		t.Fatalf("gpg --decrypt: %v\n%s", err, stderr)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("gpg decrypted %q", got)
	}
}

// libgcrypt is the version of libgcrypt gpg runs with, as gpg --version
// reports it ("libgcrypt 1.12.0").
func libgcrypt(t *testing.T, gpg string) string {
	t.Helper()
	out, err := exec.Command(gpg, "--version").Output()
	if err != nil {
		t.Fatalf("gpg --version: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "libgcrypt "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
