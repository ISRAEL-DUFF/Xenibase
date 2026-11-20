package walg

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

// The exported private keys are read by gpg, so their secret key packets
// must be what GnuPG and RFC 4880/6637 (and the crypto-refresh's legacy
// Curve25519 rules) expect, for every secret: a cv25519 subkey whose scalar
// is clamped and written as a big-endian MPI (the reverse of X25519's
// little-endian bytes) that matches the public point, and canonical MPIs
// with correct checksums throughout. (Ubuntu 26.04's libgcrypt 1.12.0
// rejects such keys at random, whoever made them: see TestGPGDecrypts.)
func TestExportedSecretKeysAreConformant(t *testing.T) {
	secrets := [][]byte{bytes.Repeat([]byte{7}, 32)} // the pinned derivation
	for range 300 {
		s := make([]byte, 32)
		_, _ = rand.Read(s)
		secrets = append(secrets, s)
	}
	for _, secret := range secrets {
		for name, derive := range map[string]func([]byte) (string, error){"PGPKey": PGPKey, "ProjectPGPKey": ProjectPGPKey} {
			priv, err := derive(secret)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkSecretKey(priv); err != nil {
				t.Fatalf("%s from %x: %v", name, secret, err)
			}
		}
	}
}

var (
	oidEd25519    = []byte{0x2b, 0x06, 0x01, 0x04, 0x01, 0xda, 0x47, 0x0f, 0x01}
	oidCurve25519 = []byte{0x2b, 0x06, 0x01, 0x04, 0x01, 0x97, 0x55, 0x01, 0x05, 0x01}
)

func checkSecretKey(armored string) error {
	blk, err := armor.Decode(strings.NewReader(armored))
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(blk.Body)
	if err != nil {
		return err
	}
	var primary, sub int
	for len(raw) > 0 {
		tag, body, rest, err := nextPacket(raw)
		if err != nil {
			return err
		}
		raw = rest
		switch tag {
		case 5:
			primary++
			if err := checkSecretPacket(body, 22, oidEd25519); err != nil {
				return fmt.Errorf("primary key: %w", err)
			}
		case 7:
			sub++
			if err := checkSecretPacket(body, 18, oidCurve25519); err != nil {
				return fmt.Errorf("subkey: %w", err)
			}
		}
	}
	if primary != 1 || sub != 1 {
		return fmt.Errorf("%d primary keys and %d subkeys", primary, sub)
	}
	return nil
}

// nextPacket splits off one new-format packet (all go-crypto writes).
func nextPacket(b []byte) (tag byte, body, rest []byte, err error) {
	if len(b) < 2 || b[0]&0xc0 != 0xc0 {
		return 0, nil, nil, fmt.Errorf("not a new-format packet header: % x", b[:min(len(b), 2)])
	}
	tag = b[0] & 0x3f
	var n, hl int
	switch l := int(b[1]); {
	case l < 192:
		n, hl = l, 2
	case l < 224:
		n, hl = (l-192)<<8+int(b[2])+192, 3
	case l == 255:
		n, hl = int(binary.BigEndian.Uint32(b[2:6])), 6
	default:
		return 0, nil, nil, fmt.Errorf("partial body length in a key packet")
	}
	if len(b) < hl+n {
		return 0, nil, nil, fmt.Errorf("truncated packet")
	}
	return tag, b[hl : hl+n], b[hl+n:], nil
}

// mpi reads a canonical MPI: its bit count is exactly that of its bytes.
func mpi(b []byte) (v, rest []byte, err error) {
	if len(b) < 2 {
		return nil, nil, fmt.Errorf("truncated MPI")
	}
	bits := int(binary.BigEndian.Uint16(b))
	n := (bits + 7) / 8
	if len(b) < 2+n || n == 0 {
		return nil, nil, fmt.Errorf("MPI of %d bits", bits)
	}
	v = b[2 : 2+n]
	if top := 8 - (n*8 - bits); v[0]>>(top-1) != 1 {
		return nil, nil, fmt.Errorf("MPI of %d bits starts with %#x", bits, v[0])
	}
	return v, b[2+n:], nil
}

func checkSecretPacket(b []byte, algo byte, oid []byte) error {
	if len(b) < 7 || b[0] != 4 || b[5] != algo {
		return fmt.Errorf("version %d algorithm %d, want 4 and %d", b[0], b[5], algo)
	}
	b = b[6:]
	if int(b[0]) != len(oid) || !bytes.Equal(b[1:1+len(oid)], oid) {
		return fmt.Errorf("curve OID % x", b[1:1+int(b[0])])
	}
	b = b[1+len(oid):]
	q, b, err := mpi(b)
	if err != nil {
		return fmt.Errorf("public point: %w", err)
	}
	if len(q) != 33 || q[0] != 0x40 {
		return fmt.Errorf("public point of %d bytes, prefix %#x", len(q), q[0])
	}
	if algo == 18 { // ECDH: the KDF parameters
		if len(b) < 4 || b[0] != 3 || b[1] != 1 {
			return fmt.Errorf("KDF parameters % x", b[:min(len(b), 4)])
		}
		b = b[4:]
	}
	if len(b) < 1 {
		return fmt.Errorf("no secret key material")
	}
	if b[0] != 0 {
		return fmt.Errorf("S2K usage %d, want an unprotected key", b[0])
	}
	secret := b[1:]
	d, rest, err := mpi(secret)
	if err != nil {
		return fmt.Errorf("secret: %w", err)
	}
	if len(rest) != 2 {
		return fmt.Errorf("%d bytes after the secret, want a 2-byte checksum", len(rest))
	}
	var sum uint16
	for _, c := range secret[:len(secret)-2] {
		sum += uint16(c)
	}
	if got := binary.BigEndian.Uint16(rest); got != sum {
		return fmt.Errorf("checksum %#x, want %#x", got, sum)
	}
	if algo != 18 {
		return nil
	}
	// cv25519: 255 bits, clamped, big-endian; the reversed bytes are the
	// X25519 private key whose public key is the point.
	if len(d) != 32 || d[0]&0xc0 != 0x40 || d[31]&7 != 0 {
		return fmt.Errorf("cv25519 scalar not clamped as a big-endian MPI: % x", d)
	}
	native := make([]byte, 32)
	for i := range d {
		native[i] = d[31-i]
	}
	k, err := ecdh.X25519().NewPrivateKey(native)
	if err != nil {
		return err
	}
	if !bytes.Equal(k.PublicKey().Bytes(), q[1:]) {
		return fmt.Errorf("the cv25519 scalar doesn't match the public point")
	}
	return nil
}
