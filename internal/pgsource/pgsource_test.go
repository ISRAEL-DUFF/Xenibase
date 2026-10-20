package pgsource_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/pgsource"
	"github.com/israel-duff/xenibase/internal/pgsource/pgsourcetest"
)

const refused = "default_transaction_read_only"

func TestReadOnlyWorksThroughAPoolerThatRefusesTheStartupParameter(t *testing.T) {
	ctx := context.Background()
	srv := pgsourcetest.Start(t, refused)
	url := "postgres://u:p@" + srv.Addr + "/db?sslmode=disable"

	// What the importer used to do: the pooler turns the connection away.
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams[refused] = "on"
	if _, err := pgx.ConnectConfig(ctx, cfg); err == nil || !strings.Contains(err.Error(), "unsupported startup parameter") {
		t.Fatalf("a startup parameter should be refused by the fake pooler: %v", err)
	}

	// What it does now: connect plainly, then SET.
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if err := pgsource.ReadOnly(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if q := srv.Queries(); len(q) != 1 || q[0] != "SET default_transaction_read_only = on" {
		t.Fatalf("queries: %q", q)
	}
}
