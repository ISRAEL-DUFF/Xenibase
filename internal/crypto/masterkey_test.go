package crypto

import (
	"bytes"
	"errors"
	"testing"
)

func mustKey(t *testing.T) []byte {
	t.Helper()
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func mustRing(t *testing.T, primary []byte, previous ...[]byte) *Keyring {
	t.Helper()
	r, err := NewKeyring(primary, previous...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRoundTrip(t *testing.T) {
	r := mustRing(t, mustKey(t))
	aad := []byte("storage_targets.credentials:1")
	for _, pt := range [][]byte{nil, []byte("s3cret"), bytes.Repeat([]byte{7}, 4096)} {
		ct, err := r.Encrypt(pt, aad)
		if err != nil {
			t.Fatal(err)
		}
		got, err := r.Decrypt(ct, aad)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, pt) {
			t.Fatalf("got %q want %q", got, pt)
		}
	}
}

func TestNonceIsRandom(t *testing.T) {
	r := mustRing(t, mustKey(t))
	a, _ := r.Encrypt([]byte("x"), nil)
	b, _ := r.Encrypt([]byte("x"), nil)
	if bytes.Equal(a, b) {
		t.Fatal("two encryptions of the same plaintext are identical")
	}
}

func TestWrongAADFails(t *testing.T) {
	r := mustRing(t, mustKey(t))
	ct, _ := r.Encrypt([]byte("x"), []byte("row:1"))
	if _, err := r.Decrypt(ct, []byte("row:2")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("got %v", err)
	}
}

func TestTamperingFails(t *testing.T) {
	r := mustRing(t, mustKey(t))
	ct, _ := r.Encrypt([]byte("hello"), nil)
	for i := range ct {
		bad := bytes.Clone(ct)
		bad[i] ^= 0x01
		if _, err := r.Decrypt(bad, nil); err == nil {
			t.Fatalf("flipping byte %d went undetected", i)
		}
	}
	if _, err := r.Decrypt(ct[:5], nil); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("truncated: got %v", err)
	}
}

func TestWrongKeyFails(t *testing.T) {
	ct, _ := mustRing(t, mustKey(t)).Encrypt([]byte("x"), nil)
	if _, err := mustRing(t, mustKey(t)).Decrypt(ct, nil); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("got %v", err)
	}
}

func TestRotation(t *testing.T) {
	oldKey, newKey := mustKey(t), mustKey(t)
	aad := []byte("settings:backup_key")

	ct, _ := mustRing(t, oldKey).Encrypt([]byte("payload"), aad)

	rotating := mustRing(t, newKey, oldKey)
	if !rotating.NeedsRotation(ct) {
		t.Fatal("old ciphertext should need rotation")
	}
	re, err := rotating.Reencrypt(ct, aad)
	if err != nil {
		t.Fatal(err)
	}
	if rotating.NeedsRotation(re) {
		t.Fatal("re-encrypted ciphertext should not need rotation")
	}

	// After dropping the old key, only re-encrypted data is readable.
	rotated := mustRing(t, newKey)
	if got, err := rotated.Decrypt(re, aad); err != nil || string(got) != "payload" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := rotated.Decrypt(ct, aad); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("got %v", err)
	}
}

func TestParseKey(t *testing.T) {
	k := mustKey(t)
	for _, s := range []string{EncodeKey(k), " " + EncodeKey(k) + "\n"} {
		got, err := ParseKey(s)
		if err != nil || !bytes.Equal(got, k) {
			t.Fatalf("ParseKey(%q) = %x, %v", s, got, err)
		}
	}
	for _, s := range []string{"", "not base64!!", EncodeKey(k[:16])} {
		if _, err := ParseKey(s); err == nil {
			t.Errorf("ParseKey(%q) should fail", s)
		}
	}
	if _, err := NewKeyring(k[:10]); err == nil {
		t.Error("short key accepted")
	}
}

func TestDerive(t *testing.T) {
	k1, k2 := mustKey(t), mustKey(t)
	a := mustRing(t, k1).Derive("salt:xenibase", 16)
	if len(a) != 16 || !bytes.Equal(a, mustRing(t, k1).Derive("salt:xenibase", 16)) {
		t.Fatal("derive is not deterministic")
	}
	if bytes.Equal(a, mustRing(t, k1).Derive("salt:other", 16)) || bytes.Equal(a, mustRing(t, k2).Derive("salt:xenibase", 16)) {
		t.Fatal("derive does not depend on label and key")
	}
}

// TestDeriveSeed: with a seed, Derive depends on the seed and not on the
// master key, so two keyrings with different primaries and one seed
// derive the same values; a seed equal to the old primary key reproduces
// what that key derived without one (the upgrade path).
func TestDeriveSeed(t *testing.T) {
	oldKey, newKey := mustKey(t), mustKey(t)
	before := mustRing(t, oldKey).Derive("xenibase edge role p_edge", 32)

	oldRing, newRing := mustRing(t, oldKey), mustRing(t, newKey)
	for _, r := range []*Keyring{oldRing, newRing} {
		if err := r.SetDeriveSeed(oldKey); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(oldRing.Derive("xenibase edge role p_edge", 32), before) {
		t.Fatal("a seed equal to the old key changed what it derives")
	}
	if !bytes.Equal(newRing.Derive("xenibase edge role p_edge", 32), before) {
		t.Fatal("a new primary key changed what the same seed derives")
	}
	other := mustRing(t, newKey)
	if err := other.SetDeriveSeed(mustKey(t)); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(other.Derive("xenibase edge role p_edge", 32), before) {
		t.Fatal("derive does not depend on the seed")
	}
	if err := other.SetDeriveSeed(oldKey[:16]); err == nil {
		t.Fatal("short seed accepted")
	}
}
