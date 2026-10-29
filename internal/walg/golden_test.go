package walg

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// The OpenPGP keys are never stored: they are derived again from the
// backup key or a project key whenever they are needed. A go-crypto that
// read its random source differently, or changed a default in the key or
// its self-signatures, would derive another key from the same secret, and
// every WAL-G archive and .dump.gpg object would stop opening, unnoticed
// until a restore. These are the keys this release derives from a fixed
// secret; if they change, existing backups no longer open. Do not update
// them: make the derivation produce them again.
var goldenKeys = []struct {
	name        string
	derive      func([]byte) (string, error)
	fingerprint string
	privateSHA  string // SHA-256 of the armored private key
	public      string
}{
	{"PGPKey", PGPKey, "CB9F07FAA99E1B7F9C86B1FC4027F327673FBD72", "93d6e57153fcb3050aa42eb1347928bc7bdf6788acd53fc2ed72070c72a69e85", `-----BEGIN PGP PUBLIC KEY BLOCK-----

xjMEaVW5ABYJKwYBBAHaRw8BAQdADyOwB1Sq1YFspHVp7LQKKv9XR9fsDkA0bKjL
FYx1FRDNJ1hlbmliYXNlIFdBTC1HIDx3YWwtZ0B4ZW5pYmFzZS5pbnZhbGlkPsK9
BBMWCABvBYJpVbkAAgsHCRBAJ/MnZz+9cjUUAAAAAAAcABBzYWx0QG5vdGF0aW9u
cy5vcGVucGdwanMub3JnPYfKcPe4MBOMt51alWDX+QIVCAIWAAIZAQKbAwIeARYh
BMufB/qpnht/nIax/EAn8ydnP71yAAClmAEA1whnsUtXI1mEr0IH/b6HHfARX3xn
nb/S8OiftagOtLQBAKsh8XJVq0XhnQ6DD3bzuM9xE+wtqS46X8aL19DVUBMAzjgE
aVW5ABIKKwYBBAGXVQEFAQEHQDir/BmzxXfdwH7tjJcBfQ27cUmB7rOjpaL5+eJ6
zfZOAwEKCcKuBBgWCABgBYJpVbkACRBAJ/MnZz+9cjUUAAAAAAAcABBzYWx0QG5v
dGF0aW9ucy5vcGVucGdwanMub3Jne5d/ENO2oeJDdspvehi6iwKbDBYhBMufB/qp
nht/nIax/EAn8ydnP71yAABCNQD9FGptWuwdCr8SEcVdMTXQc3mEFKU/7/kF6mdy
WD7LufYA/3EbFSRV3fQeZVbrr/4ZC2jTJGnTUfGPUShCscAUSGsP
=e9of
-----END PGP PUBLIC KEY BLOCK-----`},
	{"ProjectPGPKey", ProjectPGPKey, "CEC061B3F44BBE3A2A040DCF94A23957BB3F7D99", "0dab63d3f397931dfb3d43320329b81228bb2ac0fdf75356143ff6c998568848", `-----BEGIN PGP PUBLIC KEY BLOCK-----

xjMEaVW5ABYJKwYBBAHaRw8BAQdAQsxEgJDlitvHvKU1lSKwH6flyaKXr8lr0KMm
+Hjww/zNMlhlbmliYXNlIHByb2plY3QgYmFja3VwIDxiYWNrdXBzQHhlbmliYXNl
LmludmFsaWQ+wr0EExYIAG8FgmlVuQACCwcJEJSiOVe7P32ZNRQAAAAAABwAEHNh
bHRAbm90YXRpb25zLm9wZW5wZ3Bqcy5vcmfTQb9BbXtuOtK4ROrMjGEnAhUIAhYA
AhkBApsDAh4BFiEEzsBhs/RLvjoqBA3PlKI5V7s/fZkAAO7gAP0b6UFX0XCFUUdh
v1X0XtcOsJNKZFOLNPdK2VJSrgay2wD+IQ4DVZz6CL6oMzrC3h5Z6p6H5a0+sTC5
DhaGxCtjMg7OOARpVbkAEgorBgEEAZdVAQUBAQdAWjjGWeM8ny32S7GuJMPTSc1u
SJFx5b4ey5HOzH9N6FADAQoJwq4EGBYIAGAFgmlVuQAJEJSiOVe7P32ZNRQAAAAA
ABwAEHNhbHRAbm90YXRpb25zLm9wZW5wZ3Bqcy5vcmeGQ9I6m825Vn20X8jDGU0A
ApsMFiEEzsBhs/RLvjoqBA3PlKI5V7s/fZkAAKQBAP9VK+o/GF0/OtfWXIXve0Jw
VcVNpht6jnSAv/QOT1zaugEAoBHYvPgarf2dCCMhkjNRWTAnPavxPClLYmcbQmUI
NQc=
=4jiF
-----END PGP PUBLIC KEY BLOCK-----`},
}

func TestDerivedKeysArePinned(t *testing.T) {
	secret := bytes.Repeat([]byte{7}, 32)
	for _, g := range goldenKeys {
		priv, err := g.derive(secret)
		if err != nil {
			t.Fatalf("%s: %v", g.name, err)
		}
		if fp, err := Fingerprint(priv); err != nil || fp != g.fingerprint {
			t.Errorf("%s: fingerprint %s (%v), want %s: existing backups would no longer open", g.name, fp, err, g.fingerprint)
		}
		if pub, err := PublicKey(priv); err != nil || pub != g.public {
			t.Errorf("%s: public key changed (%v):\n%s", g.name, err, pub)
		}
		if sum := sha256.Sum256([]byte(priv)); hex.EncodeToString(sum[:]) != g.privateSHA {
			t.Errorf("%s: armored private key changed (SHA-256 %x, want %s)", g.name, sum, g.privateSHA)
		}
	}
}
