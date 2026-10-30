package statuspage

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The public page shows a probe's error class, never its text: a postgres
// probe's error names its user, database and the address it reached.
func TestProbeErrorsRedacted(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := l.Addr().String()
	_ = l.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer down.Close()
	ctx := context.Background()
	timeout := Probe{Kind: "tcp", Addr: "10.255.255.1:5432"}
	timeout.Timeout.Duration = time.Nanosecond
	tcp := Probe{Kind: "tcp", Addr: closed}
	tcp.Timeout.Duration = 5 * time.Second
	web := Probe{Kind: "http", URL: down.URL}
	web.Timeout.Duration = 5 * time.Second

	for _, c := range []struct {
		err  error
		want string
	}{
		{runProbe(ctx, tcp), "connection refused"},
		{runProbe(ctx, timeout), "timeout"},
		{runProbe(ctx, web), "HTTP 503"},
		{fmt.Errorf("failed to connect to `user=probe database=secretdb`: %w", &pgconn.PgError{Code: "28P01", Message: "password authentication failed for user \"probe\""}), "authentication failed"},
		{fmt.Errorf("%w: %w", errProbeQuery, &pgconn.PgError{Code: "42P01", Message: "relation \"secret_table\" does not exist"}), "query failed"},
		{errors.New("failed to connect to `user=probe database=secretdb`: 10.0.0.5:6432: something odd"), "check failed"},
	} {
		if c.err == nil {
			t.Fatalf("no error for %q", c.want)
		}
		if got := errClass(c.err); got != c.want {
			t.Errorf("errClass(%v) = %q, want %q", c.err, got, c.want)
		}
	}
	_, detail := combine([]probeResult{{Name: "pg", Err: errors.New("failed to connect to `user=probe database=secretdb`: 10.0.0.5:6432")}})
	if strings.Contains(detail, "secretdb") || strings.Contains(detail, "10.0.0.5") {
		t.Fatalf("the public detail shows the error text: %q", detail)
	}
	if detail != "pg: check failed" {
		t.Fatalf("detail %q", detail)
	}
}
