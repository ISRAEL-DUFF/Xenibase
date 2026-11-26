package backup

import (
	"encoding/base64"
	"errors"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/crypto"
)

// Secrets an operation needs only while it runs (an import's source
// connection string, a Supabase migration's credentials) travel in its
// params.secrets, sealed with the master key: any server may claim the
// operation and open them, the job runner removes them when the operation
// succeeds or fails, and the API never returns them (spec §6.8 notes).
// Each is bound to its purpose and the operation's reference, so a sealed
// value can't be opened as another.

func opSecretAAD(purpose string, ref uuid.UUID) []byte {
	return []byte("operations.secrets." + purpose + ":" + ref.String())
}

// SealOpSecret seals value for an operation's params.secrets.
func SealOpSecret(k *crypto.Keyring, purpose string, ref uuid.UUID, value []byte) (string, error) {
	ct, err := k.Encrypt(value, opSecretAAD(purpose, ref))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ct), nil
}

// OpenOpSecret opens SealOpSecret's output.
func OpenOpSecret(k *crypto.Keyring, purpose string, ref uuid.UUID, sealed string) ([]byte, error) {
	if sealed == "" {
		return nil, errors.New("the operation's secrets are gone (it has ended)")
	}
	ct, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return nil, err
	}
	return k.Decrypt(ct, opSecretAAD(purpose, ref))
}
