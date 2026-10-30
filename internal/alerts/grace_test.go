package alerts

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/pooler"
)

// The agent client certificate alert fires only when renewal (due at
// twelve hours left of a day) has left it with less than six hours.
func TestClientCertCondition(t *testing.T) {
	exp := time.Now().Add(11 * time.Hour)
	s := New(nil, nil, Config{ClientCertExpiry: func() time.Time { return exp }}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if c := s.clientCertConditions(); len(c) != 0 {
		t.Fatalf("fired with 11 hours left, just after renewal was due: %+v", c)
	}
	exp = time.Now().Add(5 * time.Hour)
	if c := s.clientCertConditions(); len(c) != 1 || c[0].kind != KindAgentClientCert {
		t.Fatalf("with five hours left: %+v", c)
	}
}

// A pooler counts as down only after failing for the grace period.
func TestPoolerGrace(t *testing.T) {
	ghost, err := pooler.NewAdmin("ghost", "127.0.0.1:1", "xenibase", "x", "disable")
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, nil, Config{Poolers: []*pooler.Admin{ghost}, PoolerGrace: 200 * time.Millisecond}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	if c, err := s.poolerConditions(ctx); err != nil || len(c) != 0 {
		t.Fatalf("down on the first failure: %+v", c)
	}
	time.Sleep(250 * time.Millisecond)
	if c, err := s.poolerConditions(ctx); err != nil || len(c) != 1 || c[0].kind != KindPoolerDown || c[0].targetID != "ghost" {
		t.Fatalf("after the grace period: %+v", c)
	}
}
