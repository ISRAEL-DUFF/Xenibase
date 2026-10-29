package services

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// TestRevokeSigningKeyWithdrawsItAtOnce: a revoked key is retired at once
// (it leaves the keys the edges verify with), not after the day rotation
// keeps it for; revoking the active key signs with a new one.
func TestRevokeSigningKeyWithdrawsItAtOnce(t *testing.T) {
	db := storetest.New(t)
	ctx := context.Background()
	k, _ := crypto.GenerateKey()
	kr, _ := crypto.NewKeyring(k)
	s := &Service{db: db, keyring: kr, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	pid := uuid.New()
	if err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO projects (id, name, slug, db_name, owner_role, scram_verifier, tier, instance_id, status, org_id, region)
			VALUES ($1, 'keys', 'keys', 'keys', 'keys', 'x', 'shared', gen_random_uuid(), 'active', gen_random_uuid(), 'eu-central')`, pid); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO project_services (project_id, ref, enabled) VALUES ($1, 'keysref1', true)`, pid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	first, err := s.RotateSigningKey(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.RotateSigningKey(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	verifying := func() map[string]bool {
		t.Helper()
		ks, err := store.New(db).EdgeJWTKeys(ctx, []uuid.UUID{pid})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, k := range ks {
			out[k.Kid] = true
		}
		return out
	}
	if v := verifying(); len(v) != 2 {
		t.Fatalf("after rotating: %v", v)
	}

	// The rotated-out key: retired now, not in a day.
	if n, err := s.RevokeSigningKey(ctx, pid, first.ID); err != nil || n != nil {
		t.Fatalf("revoke the verifying key: %v %v", n, err)
	}
	if v := verifying(); len(v) != 1 || !v[second.Kid] {
		t.Fatalf("after revoking the verifying key: %v", v)
	}
	// The active key: a new one signs, and the old one verifies nothing.
	n, err := s.RevokeSigningKey(ctx, pid, second.ID)
	if err != nil || n == nil || n.Status != "active" || n.Kid == second.Kid {
		t.Fatalf("revoke the active key: %+v %v", n, err)
	}
	if v := verifying(); len(v) != 1 || !v[n.Kid] {
		t.Fatalf("after revoking the active key: %v", v)
	}
	active, err := store.New(db).EdgeSigningKeys(ctx, []uuid.UUID{pid})
	if err != nil || len(active) != 1 || active[0].Kid != n.Kid {
		t.Fatalf("the key the edges sign with: %+v %v", active, err)
	}
	if _, err := s.RevokeSigningKey(ctx, pid, second.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("revoking a retired key: %v", err)
	}
	if _, err := s.RevokeSigningKey(ctx, uuid.New(), n.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another project's key: %v", err)
	}
}
