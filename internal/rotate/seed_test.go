package rotate_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/rotate"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// labels are the Derive labels the platform uses (edgePassword,
// ConsolePassword, storageSecret, the pooler admin salt).
var labels = []string{"xenibase edge role p1_edge", "xenibase edge role p1_anon", "xenibase console role p1_console",
	"xenibase storage urls abcdefgh", "pooler admin scram salt:xenibase"}

func derived(k *crypto.Keyring) [][]byte {
	var out [][]byte
	for _, l := range labels {
		out = append(out, k.Derive(l, 32))
	}
	return out
}

func sameDerived(a, b [][]byte) bool {
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

func addProject(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		return insertRow(ctx, tx, "projects", map[string]any{"id": uuid.New(), "tier": "shared", "status": "active"})
	}); err != nil {
		t.Fatal(err)
	}
}

func ring(t *testing.T, primary []byte, previous ...[]byte) *crypto.Keyring {
	t.Helper()
	k, err := crypto.NewKeyring(primary, previous...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestDeriveSeedSurvivesRotation: an install upgraded with projects gets
// the old primary key as its seed, so nothing derived changes on upgrade,
// and after a rotation the new key alone opens the seed and derives the
// same values as before it.
func TestDeriveSeedSurvivesRotation(t *testing.T) {
	db := storetest.New(t)
	ctx := context.Background()
	addProject(t, db)
	oldKey, _ := crypto.GenerateKey()
	newKey, _ := crypto.GenerateKey()
	before := derived(ring(t, oldKey)) // what 4.2.4 derived

	running := ring(t, oldKey)
	if err := rotate.LoadDeriveSeed(ctx, db, running, oldKey); err != nil {
		t.Fatal(err)
	}
	if !sameDerived(derived(running), before) {
		t.Fatal("the upgrade changed what the old key derives")
	}

	if err := rotate.PrepareDeriveSeed(ctx, db, [][]byte{oldKey}); err != nil {
		t.Fatal(err)
	}
	res, err := rotate.Run(ctx, db, ring(t, newKey, oldKey))
	if err != nil {
		t.Fatal(err)
	}
	if res.Rewrapped["derive seed"] != 1 {
		t.Fatalf("derive seed rewrapped %d times", res.Rewrapped["derive seed"])
	}
	only := ring(t, newKey)
	if _, err := rotate.Verify(ctx, db, only); err != nil {
		t.Fatal(err)
	}
	if err := rotate.LoadDeriveSeed(ctx, db, only, newKey); err != nil {
		t.Fatal(err)
	}
	if !sameDerived(derived(only), before) {
		t.Fatal("the rotation changed the derived passwords and URL keys")
	}
	if sameDerived(derived(ring(t, newKey)), before) {
		t.Fatal("the test proves nothing: the new key derives the same without a seed")
	}
}

// TestPrepareDeriveSeed: an install that rotates before any server made
// the seed gets one from the old key, sealed under it, so the rotation
// keeps today's values; with several previous keys it cannot tell which
// one the servers used and refuses.
func TestPrepareDeriveSeed(t *testing.T) {
	db := storetest.New(t)
	ctx := context.Background()
	addProject(t, db)
	oldKey, _ := crypto.GenerateKey()
	older, _ := crypto.GenerateKey()
	newKey, _ := crypto.GenerateKey()
	before := derived(ring(t, oldKey))

	if _, err := rotate.Verify(ctx, db, ring(t, oldKey)); err == nil {
		t.Fatal("verify passed with projects and no derive seed")
	}
	if err := rotate.PrepareDeriveSeed(ctx, db, [][]byte{oldKey, older}); err == nil {
		t.Fatal("prepared a seed without knowing which previous key was in use")
	}
	if err := rotate.PrepareDeriveSeed(ctx, db, [][]byte{oldKey}); err != nil {
		t.Fatal(err)
	}
	// A rotation that rolled back here leaves a seed the old key opens.
	stay := ring(t, oldKey)
	if err := rotate.LoadDeriveSeed(ctx, db, stay, oldKey); err != nil {
		t.Fatal(err)
	}
	if !sameDerived(derived(stay), before) {
		t.Fatal("the prepared seed changes what the old key derived")
	}
	if _, err := rotate.Run(ctx, db, ring(t, newKey, oldKey)); err != nil {
		t.Fatal(err)
	}
	only := ring(t, newKey)
	if err := rotate.LoadDeriveSeed(ctx, db, only, newKey); err != nil {
		t.Fatal(err)
	}
	if !sameDerived(derived(only), before) {
		t.Fatal("rotation changed the derived values")
	}
}

// TestDeriveSeedNewInstall: an install without projects gets a random
// seed, not its master key.
func TestDeriveSeedNewInstall(t *testing.T) {
	db := storetest.New(t)
	ctx := context.Background()
	key, _ := crypto.GenerateKey()
	seed, err := rotate.DeriveSeed(ctx, db, ring(t, key), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(seed) != crypto.KeySize || bytes.Equal(seed, key) {
		t.Fatal("a new install's seed is its master key")
	}
	again, err := rotate.DeriveSeed(ctx, db, ring(t, key), key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(seed, again) {
		t.Fatal("the seed is not stable")
	}
}
