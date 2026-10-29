package services

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// A Supabase migration step's credentials travel sealed in the operation,
// so a server other than the one that queued it can run it; once the
// operation ends and its secrets are removed, they are gone.
func TestSupabaseStepSecretsReachAnyServer(t *testing.T) {
	db := storetest.New(t)
	ctx := context.Background()
	k, _ := crypto.GenerateKey()
	kr, _ := crypto.NewKeyring(k)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	queuing := &Service{db: db, keyring: kr, log: quiet}
	running := &Service{db: db, keyring: kr, log: quiet}

	id := uuid.New()
	if err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO projects (id, name, slug, db_name, owner_role, scram_verifier, tier, instance_id, status, org_id, region)
			VALUES ($1, 'app', 'app', 'p_app', 'p_app_owner', 'x', 'shared', gen_random_uuid(), 'active', gen_random_uuid(), 'eu-central')`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	const url = "postgres://postgres:s3cret@db.example.supabase.co:5432/postgres"
	params := supabaseParams{Source: "postgres://postgres:****@db.example.supabase.co:5432/postgres"}
	if err := queuing.sealSecrets(&params, supabaseSecrets{SourceURL: url, S3: &SupabaseS3{Endpoint: "https://s3", AccessKey: "a", SecretKey: "s3-secret"}}); err != nil {
		t.Fatal(err)
	}
	op, err := jobs.Enqueue(ctx, db, jobs.EnqueueParams{Kind: KindMigrateSupabaseStorage, ProjectID: &id, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(op.Params), "s3cret") || strings.Contains(string(op.Params), "s3-secret") {
		t.Fatalf("credentials stored in the clear: %s", op.Params)
	}

	_, sec, err := running.migrationStart(ctx, op, true)
	if err != nil || sec.SourceURL != url || sec.S3 == nil || sec.S3.SecretKey != "s3-secret" {
		t.Fatalf("another server opened %+v, %v", sec, err)
	}

	// The runner removes params.secrets when the operation ends.
	if _, err := db.Exec(ctx, `UPDATE operations SET params = params - 'secrets' WHERE id = $1`, op.ID); err != nil {
		t.Fatal(err)
	}
	if op, err = store.New(db).GetOperation(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := running.migrationStart(ctx, op, true); err == nil || !strings.Contains(err.Error(), "secrets are gone") {
		t.Fatalf("after the operation ended: %v", err)
	}
}
