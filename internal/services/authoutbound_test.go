package services

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/outbound"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// TestProjectSMTPAndProviderGoThroughOutbound: a project's own SMTP server
// and SMS provider base_url are reached only at addresses the outbound
// rules allow (public, or a host the platform admin allow-listed), and
// the SMTP test tells nothing about what a connection met.
func TestProjectSMTPAndProviderGoThroughOutbound(t *testing.T) {
	db := storetest.New(t)
	ctx := context.Background()
	k, _ := crypto.GenerateKey()
	kr, _ := crypto.NewKeyring(k)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var allow atomic.Bool
	s := &Service{db: db, keyring: kr, log: log,
		Outbound: outbound.New(db, outbound.Config{Allowed: func(uuid.UUID, string) bool { return allow.Load() }}, log)}
	pid := uuid.New()
	if err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO projects (id, name, slug, db_name, owner_role, scram_verifier, tier, instance_id, status, org_id, region)
			VALUES ($1, 'smtp', 'smtp', 'smtp', 'smtp', 'x', 'shared', gen_random_uuid(), 'active', gen_random_uuid(), 'eu-central')`, pid)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// An SMTP "server" on loopback that hangs up at once.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var dialled atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			dialled.Add(1)
			_ = c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	smtp := &SMTPSettings{Host: "127.0.0.1", Port: port, From: "auth@example.com", TLS: "none"}

	err = s.TestSMTP(ctx, pid, smtp, "ada@example.com")
	if !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("a loopback SMTP server: %v", err)
	}
	if n := dialled.Load(); n != 0 {
		t.Fatalf("the refused server was dialled %d times", n)
	}
	allow.Store(true)
	err = s.TestSMTP(ctx, pid, smtp, "ada@example.com")
	if !errors.Is(err, ErrSMTPTest) || err.Error() != ErrSMTPTest.Error() {
		t.Fatalf("an allow-listed server that hangs up: %v", err)
	}
	if n := dialled.Load(); n != 1 {
		t.Fatalf("the allow-listed server was dialled %d times", n)
	}

	// A provider's base_url: refused on a private address, pinned when
	// allowed.
	allow.Store(false)
	prov := &PhoneProvider{Provider: "termii", APIKey: "k", SenderID: "S", BaseURL: "https://10.0.0.5:" + strconv.Itoa(port)}
	if _, err := s.providerClient(ctx, pid, prov); !errors.Is(err, ErrDestination) || !errors.Is(err, outbound.ErrRefused) {
		t.Fatalf("a private base_url: %v", err)
	}
	if c, err := s.providerClient(ctx, pid, &PhoneProvider{Provider: "termii"}); err != nil || c != nil {
		t.Fatalf("the provider's own API: %v %v", c, err)
	}
	allow.Store(true)
	if c, err := s.providerClient(ctx, pid, prov); err != nil || c == nil || c.CheckRedirect == nil {
		t.Fatalf("an allow-listed base_url: %v %v", c, err)
	}
}
