package backup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/pgsource/pgsourcetest"
	"github.com/israel-duff/xenibase/internal/provision"
)

func TestRelevantWarnings(t *testing.T) {
	lines := []string{
		`pg_restore: error: could not execute query: ERROR:  schema "public" already exists`,
		`Command was: CREATE SCHEMA public;`,
		`pg_restore: error: could not execute query: ERROR:  relation "auth.users" does not exist`,
		`Command was: ALTER TABLE ONLY public.profiles ADD CONSTRAINT profiles_id_fkey FOREIGN KEY (id) REFERENCES auth.users(id);`,
		`pg_restore: warning: errors ignored on restore: 2`,
	}
	got := relevantWarnings(lines, []string{"public"})
	want := []string{`ERROR:  relation "auth.users" does not exist (in: ALTER TABLE ONLY public.profiles ADD CONSTRAINT profiles_id_fkey FOREIGN KEY (id) REFERENCES auth.users(id);)`}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestConnectSourceThroughAPoolerThatIgnoresFewStartupParameters(t *testing.T) {
	srv := pgsourcetest.Start(t, "default_transaction_read_only")
	conn, _, err := connectSource(context.Background(), "postgres://u:p@"+srv.Addr+"/db?sslmode=disable", trustedSource)
	if err != nil {
		t.Fatalf("a Xenibase V2 pooler refused the import's connection: %v", err)
	}
	defer conn.Close(context.Background())
	if q := srv.Queries(); len(q) != 1 || q[0] != "SET default_transaction_read_only = on" {
		t.Fatalf("the source session must be made read-only: %q", q)
	}
}

// trustedSource and trustedCheck pass the fake source on loopback, which
// the real check refuses.
func trustedSource(_ context.Context, host string, _ int) (netip.Addr, error) {
	return netip.ParseAddr(host)
}

func trustedCheck(ctx context.Context, _ uuid.UUID, host string, port int) (netip.Addr, error) {
	return trustedSource(ctx, host, port)
}

// TestImportSourceAddressIsChecked: an import's source goes through the
// outbound rules before anything connects to it, so the server cannot be
// pointed at its own clusters (which trust its address, past a project's
// IP allowlist), loopback or the private network.
func TestImportSourceAddressIsChecked(t *testing.T) {
	srv := pgsourcetest.Start(t)
	s := NewService(nil, nil, nil, &provision.Service{}, Config{SourceInternal: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	for _, src := range []string{
		"postgres://u:secret@" + srv.Addr + "/db?sslmode=disable",  // loopback: the server's own
		"postgres://u:secret@10.0.0.5:5432/db?sslmode=disable",     // the nodes' network
		"postgres://u:secret@192.168.1.10:5432/db?sslmode=disable", // private
		"postgres://u:secret@169.254.169.254:5432/db",              // metadata
		"postgres://u:secret@93.184.216.34:5432/db?host=127.0.0.1", // host= behind a public authority
		"postgres://u:secret@93.184.216.34:5432,127.0.0.1:5432/db", // a second host
	} {
		_, err := s.Preflight(ctx, nil, src)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: preflight %v, want refused", src, err)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("the password leaked: %v", err)
		}
	}
	if n := srv.Connections(); n != 0 {
		t.Fatalf("the server connected to the refused source %d times", n)
	}
}

func TestPreflightTimeOutIsAnswerNotInternalError(t *testing.T) {
	srv := pgsourcetest.Start(t)
	srv.Hang = true
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := (&Service{CheckSource: trustedCheck}).Preflight(ctx, nil, "postgres://u:secret@"+srv.Addr+"/db?sslmode=disable")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "did not answer in time") {
		t.Fatalf("a source that never answers should be a clear request error, not %v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("the password leaked: %v", err)
	}
}
