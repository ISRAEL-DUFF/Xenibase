package services

import (
	"context"
	"net/netip"
	"testing"

	"github.com/israel-duff/xenibase/internal/pgsource/pgsourcetest"
)

func TestConnectSupabaseThroughAPoolerThatIgnoresFewStartupParameters(t *testing.T) {
	srv := pgsourcetest.Start(t, "default_transaction_read_only")
	conn, _, err := connectSupabase(context.Background(), "postgres://u:p@"+srv.Addr+"/db?sslmode=disable",
		func(_ context.Context, host string, _ int) (netip.Addr, error) { return netip.ParseAddr(host) })
	if err != nil {
		t.Fatalf("a pooler in front of the source refused the connection: %v", err)
	}
	defer conn.Close(context.Background())
	if q := srv.Queries(); len(q) != 1 || q[0] != "SET default_transaction_read_only = on" {
		t.Fatalf("the source session must be made read-only: %q", q)
	}
}
