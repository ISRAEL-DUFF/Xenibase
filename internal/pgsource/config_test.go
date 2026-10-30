package pgsource_test

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/pgsource"
	"github.com/israel-duff/xenibase/internal/pgsource/pgsourcetest"
)

// TestConfigPinsTheCheckedAddress: the connection goes to the address the
// guard approved, whatever the name resolves to later (DNS rebinding), and
// the agent's PGConn carries it as hostaddr.
func TestConfigPinsTheCheckedAddress(t *testing.T) {
	srv := pgsourcetest.Start(t)
	_, port, _ := net.SplitHostPort(srv.Addr)
	var checked string
	guard := func(_ context.Context, host string, _ int) (netip.Addr, error) {
		checked = host
		return netip.MustParseAddr("127.0.0.1"), nil
	}
	// source.invalid never resolves: the pinned address is the only way in.
	cfg, pg, err := pgsource.Config(context.Background(), "postgres://u:p@source.invalid:"+port+"/db?sslmode=disable", guard)
	if err != nil {
		t.Fatal(err)
	}
	if checked != "source.invalid" || pg.Host != "source.invalid" || pg.HostAddr != "127.0.0.1" {
		t.Fatalf("checked %q, PGConn %+v", checked, pg)
	}
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect through the pinned address: %v", err)
	}
	_ = conn.Close(context.Background())

	for _, raw := range []string{
		"postgres://u:p@source.invalid:" + port + "/db?host=10.0.0.5",
		"postgres://u:p@source.invalid:" + port + "/db?host=/var/run/postgresql",
		"postgres://u:p@source.invalid:" + port + "/db?port=5433",
		"postgres://u:p@source.invalid:" + port + ",10.0.0.5:5432/db",
	} {
		if _, _, err := pgsource.Config(context.Background(), raw, guard); err == nil {
			t.Errorf("%s: accepted", raw)
		}
	}
	if _, _, err := pgsource.Config(context.Background(), "postgres://u:p@source.invalid/db", nil); err == nil {
		t.Error("no guard: accepted")
	}
}
