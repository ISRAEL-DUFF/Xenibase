package rotate

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/crypto"
)

// The derive seed is the secret crypto.Keyring.Derive keys on: the edge and
// request roles' passwords, the console logins' passwords, the keys that
// sign file URLs and the pooler admin's salt. It is 32 bytes sealed under
// the master key in the settings row derive_seed, so a rotation re-seals it
// (settingBlobs) and the values derived from it stay as they were.
const (
	deriveSeedKey = "derive_seed"
	deriveSeedAAD = "settings.derive_seed"
)

type storedSeed struct {
	Sealed []byte `json:"sealed"`
}

// DeriveSeed returns the derive seed, creating it on first use sealed under
// k's primary key. legacy is what Derive keyed on before the seed existed,
// the primary master key of the servers that ran until now: an install that
// already has projects gets it as its seed, so nothing derived changes on
// upgrade; a new install gets random bytes.
func DeriveSeed(ctx context.Context, db *pgxpool.Pool, k *crypto.Keyring, legacy []byte) ([]byte, error) {
	var seed []byte
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		var err error
		seed, err = deriveSeed(ctx, tx, k, legacy)
		return err
	})
	return seed, err
}

// LoadDeriveSeed sets the derive seed on k (DeriveSeed with k's primary key
// as the legacy one), before k is shared.
func LoadDeriveSeed(ctx context.Context, db *pgxpool.Pool, k *crypto.Keyring, primary []byte) error {
	seed, err := DeriveSeed(ctx, db, k, primary)
	if err != nil {
		return fmt.Errorf("derive seed: %w", err)
	}
	return k.SetDeriveSeed(seed)
}

// PrepareDeriveSeed runs before a rotation's Run. An install that upgraded
// and rotates before any server created the seed would otherwise create it
// from the new key; it is created here from the old key (the one previous
// key), sealed under it, so the rotation re-seals it with everything else
// and a rotation that rolls back leaves a seed the old key still opens.
func PrepareDeriveSeed(ctx context.Context, db *pgxpool.Pool, previous [][]byte) error {
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if err := lockSeed(ctx, tx); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM settings WHERE key = $1)`, deriveSeedKey).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		if len(previous) != 1 {
			return errors.New("there is no derive seed yet and several previous keys: start xenibase-server once with the old key, or put only the old key in XENIBASE_MASTER_KEY_PREVIOUS")
		}
		old, err := crypto.NewKeyring(previous[0])
		if err != nil {
			return err
		}
		_, err = deriveSeed(ctx, tx, old, previous[0])
		return err
	})
}

func lockSeed(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('xenibase_derive_seed'))`)
	return err
}

func deriveSeed(ctx context.Context, tx pgx.Tx, k *crypto.Keyring, legacy []byte) ([]byte, error) {
	if err := lockSeed(ctx, tx); err != nil {
		return nil, err
	}
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, deriveSeedKey).Scan(&raw)
	if err == nil {
		var s storedSeed
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		seed, err := k.Decrypt(s.Sealed, []byte(deriveSeedAAD))
		if err != nil {
			return nil, fmt.Errorf("open: %w", err)
		}
		return seed, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	// Platform-wide: whether any organisation has a project yet.
	var upgrade bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM projects)`).Scan(&upgrade); err != nil {
		return nil, err
	}
	seed := make([]byte, crypto.KeySize)
	if upgrade && legacy != nil {
		if len(legacy) != crypto.KeySize {
			return nil, fmt.Errorf("legacy key must be %d bytes", crypto.KeySize)
		}
		copy(seed, legacy)
	} else if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	sealed, err := k.Encrypt(seed, []byte(deriveSeedAAD))
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(storedSeed{Sealed: sealed})
	if _, err := tx.Exec(ctx, `INSERT INTO settings (key, value) VALUES ($1, $2)`, deriveSeedKey, b); err != nil {
		return nil, err
	}
	return seed, nil
}
