package pgversions

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// A notice whose mail fails is sent again by the next sweep, and two
// servers sweeping at once send it once.
func TestNoticeIsResentAfterAFailedSend(t *testing.T) {
	db := storetest.New(t)
	ctx := context.Background()
	q := store.New(db)
	if err := q.EnsurePgVersion(ctx, 17); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	retires := now.Add(200 * 24 * time.Hour)
	if _, err := q.UpdatePgVersion(ctx, store.UpdatePgVersionParams{Major: 17, Status: provision.VersionDeprecated, DeprecatedAt: &now, RetiresAt: &retires}); err != nil {
		t.Fatal(err)
	}
	n, err := q.UpsertNode(ctx, store.UpsertNodeParams{Name: "node1", PrivateAddr: "127.0.0.1", Role: "both"})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := q.InsertInstance(ctx, store.InsertInstanceParams{ID: uuid.New(), NodeID: n.ID, Kind: "shared", PgVersion: 17})
	if err != nil {
		t.Fatal(err)
	}
	var org uuid.UUID
	if err := db.QueryRow(ctx, `INSERT INTO organizations (name, slug, plan_id)
		SELECT 'o', 'o', id FROM quota_plans WHERE name = 'Personal' RETURNING id`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	u, err := q.InsertUser(ctx, store.InsertUserParams{Email: "owner@example.com", PasswordHash: "x", PlatformRole: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.InsertOrgMember(ctx, store.InsertOrgMemberParams{OrgID: org, UserID: u.ID, Role: "owner"}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertProject(ctx, store.InsertProjectParams{
		Region: "eu-central", ID: uuid.New(), OrgID: org, Name: "p", Slug: "p", DbName: "p", OwnerRole: "p_owner",
		ScramVerifier: "SCRAM-SHA-256$4096:x$y:z", Tier: "shared", InstanceID: inst.ID, Settings: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	var mu sync.Mutex
	var sent []mail.Message
	fail := true
	send := func(_ context.Context, m mail.Message) error {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return errors.New("smtp: connection refused")
		}
		time.Sleep(50 * time.Millisecond) // a slow server, while the other sweeps
		sent = append(sent, m)
		return nil
	}
	server := func() *Service {
		s := New(db, nil, nil, Config{}, quiet)
		s.send = send
		return s
	}
	a, b := server(), server()
	if err := a.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 0 {
		t.Fatalf("sent %d with the mail failing", len(sent))
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range []*Service{a, b} {
		wg.Add(1)
		go func(s *Service) { defer wg.Done(); _ = s.Sweep(ctx) }(s)
	}
	wg.Wait()
	if len(sent) != 1 || sent[0].To[0] != "owner@example.com" {
		t.Fatalf("after the mail recovered, two servers sent %+v", sent)
	}
	if err := a.Sweep(ctx); err != nil || len(sent) != 1 {
		t.Fatalf("a later sweep sent it again: %d %v", len(sent), err)
	}
}
