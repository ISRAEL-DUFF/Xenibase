package orgs_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/orgs"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

type outbox struct {
	mu   sync.Mutex
	msgs []mail.Message
}

func (o *outbox) Send(_ context.Context, m mail.Message) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.msgs = append(o.msgs, m)
	return nil
}

// TestInvitationsAreCapped: an organisation, and an inviter, may send only
// so many invitations a day; past that Invite says when to try again.
func TestInvitationsAreCapped(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	ann, ben, org, other := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(t, db, `INSERT INTO users (id, email, password_hash) VALUES ($1, 'ann@example.com', 'x'), ($2, 'ben@example.com', 'x')`, ann, ben)
	exec(t, db, `INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'Org', 'org', id FROM quota_plans ORDER BY name LIMIT 1`, org)
	exec(t, db, `INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'Other', 'other', id FROM quota_plans ORDER BY name LIMIT 1`, other)
	svc := orgs.New(db, nil, nil, &outbox{}, "https://xenibase.test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.SetInvitationLimits(3, 2)
	invite := func(org uuid.UUID, by uuid.UUID, n int) error {
		_, err := svc.Invite(ctx, orgs.InviteParams{OrgID: &org, Email: "guest" + string(rune('a'+n)) + "@example.com", OrgRole: "member", InvitedBy: by})
		return err
	}
	for i := range 2 {
		if err := invite(org, ann, i); err != nil {
			t.Fatalf("invitation %d: %v", i, err)
		}
	}
	// Ann's own cap counts every organisation.
	err := invite(other, ann, 2)
	var l orgs.LimitError
	if !errors.Is(err, orgs.ErrTooManyInvitations) || !errors.As(err, &l) || l.RetryAfter < 23*time.Hour || l.RetryAfter > 24*time.Hour {
		t.Fatalf("the inviter's third invitation: %v %+v", err, l)
	}
	if err := invite(org, ben, 3); err != nil {
		t.Fatalf("another inviter: %v", err)
	}
	// The organisation's cap is reached.
	if err := invite(org, ben, 4); !errors.Is(err, orgs.ErrTooManyInvitations) || !strings.Contains(err.Error(), "organisation") {
		t.Fatalf("the organisation's fourth invitation: %v", err)
	}
	// A day later the window has moved on.
	exec(t, db, `UPDATE invitations SET created_at = created_at - interval '25 hours'`)
	if err := invite(org, ann, 5); err != nil {
		t.Fatalf("the next day: %v", err)
	}
}

// TestInvitationNamesAreNotLinks: the organisation's and the inviter's
// names are theirs to choose and go to any address, so a web address in
// one is never made a link, in the HTML or the text.
func TestInvitationNamesAreNotLinks(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	by, org := uuid.New(), uuid.New()
	exec(t, db, `INSERT INTO users (id, email, password_hash) VALUES ($1, 'mallory@example.com', 'x')`, by)
	exec(t, db, `INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'Security team https://evil.test/reset', 'org', id FROM quota_plans ORDER BY name LIMIT 1`, org)
	box := &outbox{}
	svc := orgs.New(db, nil, nil, box, "https://xenibase.test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	in, err := svc.Invite(ctx, orgs.InviteParams{OrgID: &org, Email: "victim@example.com", OrgRole: "member", InvitedBy: by,
		InviterName: "IT desk evil.test/login"})
	if err != nil || in.EmailError != nil {
		t.Fatalf("invite: %v %v", err, in.EmailError)
	}
	if len(box.msgs) != 1 || box.msgs[0].Content == nil {
		t.Fatalf("emails: %+v", box.msgs)
	}
	m := box.msgs[0]
	html, text := m.Content.HTML(), m.Body
	for _, part := range []struct{ name, s string }{{"html", html}, {"text", text}, {"subject", m.Subject}} {
		for _, bad := range []string{"https://evil.test", "evil.test/"} {
			if strings.Contains(part.s, bad) {
				t.Errorf("%s carries %q as a link", part.name, bad)
			}
		}
	}
	if !strings.Contains(html, `href="`+in.URL+`"`) || !strings.Contains(text, in.URL) {
		t.Fatal("the invitation's own link is missing")
	}
	// The names still read the same.
	if plain := strings.ReplaceAll(text, "\u200b", ""); !strings.Contains(plain, "Security team https://evil.test/reset") || !strings.Contains(plain, "IT desk evil.test/login") {
		t.Fatalf("the names changed: %s", plain)
	}
}
