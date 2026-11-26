package services

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/rotate"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// TestDerivedSurviveRotation: a project with backend services keeps
// working across a rotation of the master key. The edge and request
// roles' passwords, the console login's password and the file URL key
// are derived from the sealed seed, so a server restarted on the new key
// alone derives what the old one did, and the verifiers stored at enable
// time from those passwords still match. Before the seed, all of them
// changed.
func TestDerivedSurviveRotation(t *testing.T) {
	db := storetest.New(t)
	ctx := context.Background()
	// An install upgraded with a project: the seed is the old key.
	if err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO projects (id, name, slug, db_name, owner_role, scram_verifier, tier, instance_id, status, org_id, region)
			VALUES ($1, 'p', 'p', 'p_abc', 'p_abc', 'x', 'shared', gen_random_uuid(), 'active', gen_random_uuid(), 'eu-central')`, uuid.New())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	oldKey, _ := crypto.GenerateKey()
	newKey, _ := crypto.GenerateKey()

	type values struct {
		passwords []string
		urlKey    []byte
		console   string
	}
	server := func(primary []byte, previous ...[]byte) values {
		t.Helper()
		kr, err := crypto.NewKeyring(primary, previous...)
		if err != nil {
			t.Fatal(err)
		}
		if err := rotate.LoadDeriveSeed(ctx, db, kr, primary); err != nil {
			t.Fatal(err)
		}
		s := &Service{db: db, keyring: kr, log: log}
		var v values
		for _, r := range store.ServiceRoles("p_abc") {
			v.passwords = append(v.passwords, s.edgePassword(r))
		}
		v.urlKey = s.storageSecret("abcdefgh")
		v.console = provision.NewService(db, kr, nil, provision.Config{}, log).ConsolePassword("p_abc_console")
		return v
	}
	before := server(oldKey)
	if err := rotate.PrepareDeriveSeed(ctx, db, [][]byte{oldKey}); err != nil {
		t.Fatal(err)
	}
	both, _ := crypto.NewKeyring(newKey, oldKey)
	if _, err := rotate.Run(ctx, db, both); err != nil {
		t.Fatal(err)
	}
	after := server(newKey)
	for i, r := range store.ServiceRoles("p_abc") {
		if before.passwords[i] != after.passwords[i] {
			t.Errorf("%s: the derived password changed with the master key", r)
		}
	}
	if !bytes.Equal(before.urlKey, after.urlKey) {
		t.Error("the file URL key changed with the master key")
	}
	if before.console != after.console {
		t.Error("the console password changed with the master key")
	}
}
