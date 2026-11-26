// Package crypto holds Xenibase's cryptographic helpers. This file covers
// master-key envelope encryption of secrets stored in the metadata DB
// (spec §7.3); SCRAM verifiers and the agent CA arrive in later milestones.
package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the master key length in bytes (AES-256).
const KeySize = 32

const (
	formatV1  byte = 1
	keyIDSize      = 8
	nonceSize      = 12
	headerLen      = 1 + keyIDSize + nonceSize
)

var (
	// ErrUnknownKey means the ciphertext was sealed with a key the keyring
	// does not hold (for example, a rotated-out key that was dropped).
	ErrUnknownKey = errors.New("crypto: ciphertext was encrypted with an unknown master key")
	// ErrDecrypt means the ciphertext is malformed, was tampered with, or
	// was sealed with different associated data.
	ErrDecrypt = errors.New("crypto: decryption failed")
)

// KeyID identifies a master key without revealing it.
type KeyID [keyIDSize]byte

func (id KeyID) String() string { return fmt.Sprintf("%x", id[:]) }

type key struct {
	id   KeyID
	aead cipher.AEAD
	raw  []byte
}

func newKey(raw []byte) (key, error) {
	if len(raw) != KeySize {
		return key{}, fmt.Errorf("crypto: master key must be %d bytes, got %d", KeySize, len(raw))
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return key{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return key{}, err
	}
	sum := sha256.Sum256(append([]byte("xenibase master key id\x00"), raw...))
	var id KeyID
	copy(id[:], sum[:keyIDSize])
	return key{id: id, aead: aead, raw: append([]byte(nil), raw...)}, nil
}

// Keyring encrypts with a primary master key and decrypts with the primary
// or any previous key, which is what makes master-key rotation possible:
// load the new key as primary and the old one as previous, re-encrypt every
// stored secret with Reencrypt, then drop the old key.
//
// Ciphertext layout: version (1) | key ID (8) | nonce (12) | AES-256-GCM
// sealed data. The version byte and key ID are authenticated along with the
// caller's associated data.
type Keyring struct {
	primary key
	byID    map[KeyID]key
	// seed keys Derive when set (SetDeriveSeed): a stored secret of its
	// own, so derived values outlive a rotation of the master key.
	seed []byte
}

// NewKeyring builds a keyring from a primary key and optional previous keys.
func NewKeyring(primary []byte, previous ...[]byte) (*Keyring, error) {
	p, err := newKey(primary)
	if err != nil {
		return nil, err
	}
	k := &Keyring{primary: p, byID: map[KeyID]key{p.id: p}}
	for _, raw := range previous {
		old, err := newKey(raw)
		if err != nil {
			return nil, fmt.Errorf("previous key: %w", err)
		}
		if _, dup := k.byID[old.id]; !dup {
			k.byID[old.id] = old
		}
	}
	return k, nil
}

// PrimaryID returns the ID of the key new ciphertexts are sealed with.
func (k *Keyring) PrimaryID() KeyID { return k.primary.id }

// Encrypt seals plaintext with the primary key. aad binds the ciphertext to
// its context (for example "storage_targets.credentials:<id>") so a blob
// copied into another row fails to decrypt.
func (k *Keyring) Encrypt(plaintext, aad []byte) ([]byte, error) {
	out := make([]byte, headerLen, headerLen+len(plaintext)+k.primary.aead.Overhead())
	out[0] = formatV1
	copy(out[1:], k.primary.id[:])
	if _, err := rand.Read(out[1+keyIDSize : headerLen]); err != nil {
		return nil, err
	}
	nonce := out[1+keyIDSize : headerLen]
	return k.primary.aead.Seal(out, nonce, plaintext, additionalData(out[:1+keyIDSize], aad)), nil
}

// Decrypt opens a ciphertext produced by Encrypt with the same aad.
func (k *Keyring) Decrypt(ciphertext, aad []byte) ([]byte, error) {
	kk, err := k.keyFor(ciphertext)
	if err != nil {
		return nil, err
	}
	nonce := ciphertext[1+keyIDSize : headerLen]
	pt, err := kk.aead.Open(nil, nonce, ciphertext[headerLen:], additionalData(ciphertext[:1+keyIDSize], aad))
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// NeedsRotation reports whether ciphertext was sealed with a key other than
// the primary.
func (k *Keyring) NeedsRotation(ciphertext []byte) bool {
	if len(ciphertext) < headerLen {
		return false
	}
	return !bytes.Equal(ciphertext[1:1+keyIDSize], k.primary.id[:])
}

// Reencrypt decrypts ciphertext with whichever key sealed it and seals it
// again with the primary key.
func (k *Keyring) Reencrypt(ciphertext, aad []byte) ([]byte, error) {
	pt, err := k.Decrypt(ciphertext, aad)
	if err != nil {
		return nil, err
	}
	return k.Encrypt(pt, aad)
}

func (k *Keyring) keyFor(ciphertext []byte) (key, error) {
	if len(ciphertext) < headerLen+k.primary.aead.Overhead() || ciphertext[0] != formatV1 {
		return key{}, ErrDecrypt
	}
	var id KeyID
	copy(id[:], ciphertext[1:1+keyIDSize])
	kk, ok := k.byID[id]
	if !ok {
		return key{}, ErrUnknownKey
	}
	return kk, nil
}

func additionalData(header, aad []byte) []byte {
	out := make([]byte, 0, len(header)+len(aad))
	out = append(out, header...)
	return append(out, aad...)
}

// SetDeriveSeed makes Derive key on seed instead of the primary key. The
// server sets it once at startup, before the keyring is shared, from the
// sealed settings row rotate.DeriveSeed keeps; a rotation re-seals that row
// like any other secret, so what Derive returns does not change with the
// master key.
func (k *Keyring) SetDeriveSeed(seed []byte) error {
	if len(seed) != KeySize {
		return fmt.Errorf("crypto: derive seed must be %d bytes, got %d", KeySize, len(seed))
	}
	k.seed = append([]byte(nil), seed...)
	return nil
}

// Derive returns n bytes (at most 32) deterministically derived for label,
// for values that must be stable and secret, such as a SCRAM salt or a
// password that is never stored. They are keyed on the derive seed
// (SetDeriveSeed), which survives a rotation; a keyring without one (tests,
// and tools that derive nothing) keys on the primary key, which is what the
// seed of an install from before the seed existed holds.
func (k *Keyring) Derive(label string, n int) []byte {
	secret := k.seed
	if secret == nil {
		secret = k.primary.raw
	}
	h := hmac.New(sha256.New, secret)
	h.Write([]byte("xenibase derive\x00" + label))
	return h.Sum(nil)[:min(n, sha256.Size)]
}

// GenerateKey returns a new random master key.
func GenerateKey() ([]byte, error) {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, nil
}

// EncodeKey renders a key in the format ParseKey accepts.
func EncodeKey(k []byte) string { return base64.StdEncoding.EncodeToString(k) }

// ParseKey decodes a base64 (standard or URL alphabet, padded or not) master
// key and checks its length.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			if len(b) != KeySize {
				return nil, fmt.Errorf("crypto: master key must decode to %d bytes, got %d", KeySize, len(b))
			}
			return b, nil
		}
	}
	return nil, errors.New("crypto: master key is not valid base64")
}
