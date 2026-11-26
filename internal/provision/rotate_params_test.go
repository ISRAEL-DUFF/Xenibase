package provision

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// wireRotate is where a rotation's params are expected, written down apart
// from rotateParams.
type wireRotate struct {
	Secrets struct {
		Password         string `json:"password"`
		Verifier         string `json:"verifier"`
		PreviousVerifier string `json:"previous_verifier"`
	} `json:"secrets"`
}

// A rotation's SCRAM verifiers travel in params.secrets, so they go when
// the operation ends: they used to stay in params for good, and the API
// returns params to anyone who can view the project.
func TestRotateKeepsVerifiersInSecrets(t *testing.T) {
	db := storetest.New(t)
	ctx := context.Background()
	k, _ := crypto.GenerateKey()
	kr, _ := crypto.NewKeyring(k)
	s := &Service{db: db, keyring: kr}
	old, _ := crypto.SCRAMVerifier("the-old-password")
	project := insertProject(t, db, old)

	r, err := s.Rotate(ctx, project, nil)
	if err != nil {
		t.Fatal(err)
	}
	params := operationParams(t, db, r.Operation.ID)
	if _, ok := params["verifier"]; ok {
		t.Errorf("a verifier outside secrets: %v", params)
	}
	if _, ok := params["previous_verifier"]; ok {
		t.Errorf("a verifier outside secrets: %v", params)
	}
	var p wireRotate
	b, _ := json.Marshal(params)
	if err := json.Unmarshal(b, &p); err != nil || p.Secrets.PreviousVerifier != old || !crypto.IsSCRAMVerifier(p.Secrets.Verifier) || p.Secrets.Password == "" {
		t.Fatalf("the rotation's secrets: %+v %v", p.Secrets, err)
	}

	// The operation ends: nothing of the verifiers is left.
	if _, err := db.Exec(ctx, `UPDATE operations SET status = 'running', locked_by = 'w' WHERE id = $1`, r.Operation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.New(db).SucceedOperation(ctx, store.SucceedOperationParams{ID: r.Operation.ID, Worker: "w"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(operationParams(t, db, r.Operation.ID))
	if strings.Contains(string(raw), old) || strings.Contains(string(raw), p.Secrets.Verifier) {
		t.Fatalf("a finished rotation keeps a verifier: %s", raw)
	}
}

// Migration 00062 scrubs the verifiers finished operations kept, and moves
// those of operations still running into secrets.
func TestMigrationScrubsOperationVerifiers(t *testing.T) {
	db := storetest.NewEmpty(t)
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.MigrateTo(ctx, db, 61, quiet); err != nil {
		t.Fatal(err)
	}
	done, running := uuid.New(), uuid.New()
	legacy := `{"verifier": "SCRAM-SHA-256$new", "previous_verifier": "SCRAM-SHA-256$old", "secrets": {"password": "sealed"}}`
	if err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO operations (id, kind, project_id, status, params) VALUES
			($1, 'rotate', gen_random_uuid(), 'succeeded', '{"verifier": "SCRAM-SHA-256$new", "previous_verifier": "SCRAM-SHA-256$old"}'),
			($2, 'rotate', gen_random_uuid(), 'running', $3)`, done, running, legacy)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, db, quiet); err != nil {
		t.Fatal(err)
	}
	if p := operationParams(t, db, done); len(p) != 0 {
		t.Errorf("a finished rotation keeps %v", p)
	}
	b, _ := json.Marshal(operationParams(t, db, running))
	var p wireRotate
	var top map[string]any
	_ = json.Unmarshal(b, &p)
	_ = json.Unmarshal(b, &top)
	if top["verifier"] != nil || top["previous_verifier"] != nil || p.Secrets.Verifier != "SCRAM-SHA-256$new" ||
		p.Secrets.PreviousVerifier != "SCRAM-SHA-256$old" || p.Secrets.Password != "sealed" {
		t.Errorf("a running rotation after the migration: %s", b)
	}
}

func insertProject(t *testing.T, db *pgxpool.Pool, verifier string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pgx.BeginFunc(context.Background(), db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(), `INSERT INTO projects (id, name, slug, db_name, owner_role, scram_verifier, tier, instance_id, status, org_id, region)
			VALUES ($1, 'p', 'p', 'p', 'p', $2, 'shared', gen_random_uuid(), 'active', gen_random_uuid(), 'eu-central')`, id, verifier)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func operationParams(t *testing.T, db *pgxpool.Pool, id uuid.UUID) map[string]any {
	t.Helper()
	var raw []byte
	if err := db.QueryRow(context.Background(), `SELECT params FROM operations WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
