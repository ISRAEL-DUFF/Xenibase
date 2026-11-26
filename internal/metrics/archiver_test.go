package metrics

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// The archiver sample reads the test server (which needs the superuser
// for pg_ls_archive_statusdir): with archiving off, as on the test server
// and a shared cluster, it records nothing; with it on, all four metrics.
func TestCollectArchiver(t *testing.T) {
	url := os.Getenv(storetest.EnvVar)
	if url == "" {
		t.Skipf("%s not set", storetest.EnvVar)
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var mode string
	if err := conn.QueryRow(ctx, `SHOW archive_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	var b batch
	if err := collectArchiver(ctx, conn, uuid.New(), &b); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range b.mets {
		got[m] = true
	}
	if mode == "off" {
		if len(b.mets) != 0 {
			t.Fatalf("archiving is off, but recorded %v", b.mets)
		}
		return
	}
	for _, m := range []string{WALArchiveFailed, WALArchivePending, WALArchivePendingAge} {
		if !got[m] {
			t.Errorf("%s not recorded (got %v)", m, b.mets)
		}
	}
}
