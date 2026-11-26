package backup

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/provision"
)

// An import's source URL travels sealed in its operation, so any server
// can run it, not only the one that took the request; it is never stored
// in the clear, and is gone once the operation's secrets are removed.
func TestImportSourceReachesAnyServer(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	other := NewService(h.db, h.s.keyring, h.s.nodes, provision.NewService(h.db, h.s.keyring, nil, provision.Config{}, quiet), Config{}, quiet)
	const url = "postgres://app:s3cret@db.example.com:5432/shop"
	ref := uuid.New()
	sealed, err := SealOpSecret(h.s.keyring, importSourcePurpose, ref, []byte(url))
	if err != nil {
		t.Fatal(err)
	}
	// As provision.Create writes it: the password handoff and the import's
	// secrets side by side.
	op, err := jobs.Enqueue(ctx, h.db, jobs.EnqueueParams{Kind: KindImport, Params: map[string]any{
		"import":  importParams{SourceRef: ref, Source: "postgres://app:****@db.example.com:5432/shop"},
		"secrets": map[string]string{"password": "sealed-password", "source_url": sealed},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(op.Params), "s3cret") {
		t.Fatalf("source URL stored in the clear: %s", op.Params)
	}
	if got, err := other.importSource(op, ref); err != nil || got != url {
		t.Fatalf("another server read %q, %v", got, err)
	}
	if _, err := other.importSource(op, uuid.New()); err == nil {
		t.Fatal("the sealed URL opened under another import's reference")
	}
	if _, err := h.db.Exec(ctx, `UPDATE operations SET params = params - 'secrets' WHERE id = $1`, op.ID); err != nil {
		t.Fatal(err)
	}
	op.Params = []byte(`{"import": {}}`)
	if _, err := other.importSource(op, ref); err == nil {
		t.Fatal("the source URL outlived the operation's secrets")
	}
}
