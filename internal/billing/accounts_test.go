package billing_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/billing"
	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

type sentMail struct {
	mu   sync.Mutex
	msgs []mail.Message
}

func (m *sentMail) Send(_ context.Context, msg mail.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, msg)
	return nil
}

// reset forgets what was sent (a new contact's confirmation link).
func (m *sentMail) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = nil
}

// all is a copy of what was sent: mail goes out from background work (a
// deposit pays what is owed on its own), so tests read it under the lock.
func (m *sentMail) all() []mail.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mail.Message(nil), m.msgs...)
}

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

// fixedRate prices a dollar at ₦1,500, as of the test clock: the rate source
// of a service with no wallet provider.
type fixedRate struct{ clk *clock }

func (r fixedRate) Rate(_ context.Context, _, _ string, _ int64) (billing.FXRate, error) {
	return billing.FXRate{From: "USD", To: "NGN", Market: billing.D("1500"), Effective: billing.D("1500"), FetchedAt: r.clk.Now(), Sources: []string{"test"}}, nil
}

func newService(t *testing.T) (*billing.Service, *pgxpool.Pool, *clock, *sentMail) {
	t.Helper()
	// The older tests count in naira.
	return newServiceWith(t, billing.DefaultPricesNGN)
}

// newServiceWith is a billing service whose first price book is first().
func newServiceWith(t *testing.T, first func() billing.Prices) (*billing.Service, *pgxpool.Pool, *clock, *sentMail) {
	t.Helper()
	db := storetest.New(t)
	m := &sentMail{}
	clk := &clock{t: time.Date(2026, 10, 13, 15, 0, 0, 0, time.UTC)}
	s := billing.New(db, m, "https://xenibase.example", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.Now = clk.Now
	s.SetRateSource(fixedRate{clk})
	s.SetDefaultPrices(first)
	if err := s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, db, clk, m
}

func quotaPlan(t *testing.T, db *pgxpool.Pool, org uuid.UUID) string {
	t.Helper()
	n, err := store.New(db).OrgQuotaPlanName(context.Background(), org)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAccountsAndPlanChanges(t *testing.T) {
	s, db, clk, _ := newService(t)
	ctx := context.Background()
	org := newOrg(t, db, "acme")

	a, err := s.Account(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	if a.Plan != billing.PlanFree || a.PriceBookVersion != 1 || a.Mode != billing.ModePostpaid {
		t.Fatalf("new account %+v", a)
	}

	// Free → Pro on the 13th: the rest of October, at once.
	dry, err := s.ChangePlan(ctx, org, billing.PlanRequest{Plan: billing.PlanPro, DryRun: true})
	if err != nil || !dry.Applied || !dry.Upgrade || dry.Total != 919355 {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	if a, _ := s.Account(ctx, org); a.Plan != billing.PlanFree {
		t.Fatal("a dry run changed the plan")
	}
	if _, err := s.ChangePlan(ctx, org, billing.PlanRequest{Plan: billing.PlanPro}); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Account(ctx, org); a.Plan != billing.PlanPro || a.PaymentTermsDays != 7 {
		t.Errorf("after the upgrade: %+v", a)
	}
	if q := quotaPlan(t, db, org); q != "Pro" {
		t.Errorf("quota plan %s, want Pro", q)
	}
	if _, err := s.ChangePlan(ctx, org, billing.PlanRequest{Plan: billing.PlanPro}); !errors.Is(err, billing.ErrConflict) {
		t.Errorf("same plan again: %v", err)
	}

	// Pro → Team the same day: credit the unused Pro, charge Team.
	up, err := s.ChangePlan(ctx, org, billing.PlanRequest{Plan: billing.PlanTeam})
	if err != nil || up.Total != 3677419-919355 || len(up.Lines) != 2 {
		t.Fatalf("upgrade: %+v %v", up, err)
	}

	// Team → Pro is a downgrade: from 1 November.
	down, err := s.ChangePlan(ctx, org, billing.PlanRequest{Plan: billing.PlanPro})
	if err != nil || down.Applied || len(down.Lines) != 0 || !down.EffectiveAt.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("downgrade: %+v %v", down, err)
	}
	if a, _ := s.Account(ctx, org); a.Plan != billing.PlanTeam {
		t.Error("the downgrade applied early")
	}
	// Choosing Team again cancels it; choosing Pro schedules it again.
	if c, err := s.ChangePlan(ctx, org, billing.PlanRequest{Plan: billing.PlanTeam}); err != nil || !c.Applied || len(c.Lines) != 0 {
		t.Fatalf("keep team: %+v %v", c, err)
	}
	if _, err := store.New(db).PendingPlanChange(ctx, org); err == nil {
		t.Fatal("the downgrade is still scheduled")
	}
	if _, err := s.ChangePlan(ctx, org, billing.PlanRequest{Plan: billing.PlanPro}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ApplyDueChanges(ctx); err != nil || n != 0 {
		t.Fatalf("early sweep: %d %v", n, err)
	}
	clk.t = time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC)
	if n, err := s.ApplyDueChanges(ctx); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	if a, _ := s.Account(ctx, org); a.Plan != billing.PlanPro {
		t.Errorf("after the sweep: %s", a.Plan)
	}
	if q := quotaPlan(t, db, org); q != "Pro" {
		t.Errorf("quota plan %s", q)
	}

	// An admin's custom quota plan survives plan changes.
	if _, err := db.Exec(ctx, `UPDATE organizations SET plan_id = (SELECT id FROM quota_plans WHERE name = 'Unlimited') WHERE id = $1`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangePlan(ctx, org, billing.PlanRequest{Plan: billing.PlanTeam}); err != nil {
		t.Fatal(err)
	}
	if q := quotaPlan(t, db, org); q != "Unlimited" {
		t.Errorf("quota plan %s, want the admin's Unlimited", q)
	}

	// Annual: paid at once, renewed when the term ends.
	ann, err := s.ChangePlan(ctx, org, billing.PlanRequest{Plan: billing.PlanTeam, Term: billing.TermAnnual})
	if err != nil || !ann.Applied || ann.TermEndsAt == nil {
		t.Fatalf("annual: %+v %v", ann, err)
	}
	if _, err := s.ChangePlan(ctx, org, billing.PlanRequest{Plan: billing.PlanFree, Term: billing.TermAnnual}); err == nil {
		t.Error("free has no annual term")
	}
	clk.t = ann.TermEndsAt.Add(time.Hour)
	if n, err := s.ApplyDueChanges(ctx); err != nil || n != 1 {
		t.Fatalf("renewal: %d %v", n, err)
	}
	a, _ = s.Account(ctx, org)
	if a.TermEndsAt == nil || !a.TermEndsAt.Equal(ann.TermEndsAt.AddDate(1, 0, 0)) {
		t.Errorf("renewed term ends %v", a.TermEndsAt)
	}
}

func TestPriceBookPublishing(t *testing.T) {
	s, db, clk, sent := newService(t)
	ctx := context.Background()
	orgs := map[string]uuid.UUID{}
	for _, slug := range []string{"monthly", "annual", "grandfathered", "free"} {
		orgs[slug] = newOrg(t, db, slug)
		if _, err := s.Account(ctx, orgs[slug]); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddContact(ctx, orgs[slug], "Finance@"+slug+".example", nil); err != nil {
			t.Fatal(err)
		}
	}
	sent.reset()
	for slug, term := range map[string]string{"monthly": billing.TermMonthly, "annual": billing.TermAnnual, "grandfathered": billing.TermMonthly} {
		if _, err := s.ChangePlan(ctx, orgs[slug], billing.PlanRequest{Plan: billing.PlanPro, Term: term}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(ctx, `UPDATE billing_accounts SET grandfathered = true WHERE org_id = $1`, orgs["grandfathered"]); err != nil {
		t.Fatal(err)
	}

	p := billing.DefaultPricesNGN()
	pro := p.Plans[billing.PlanPro]
	pro.MonthlyMinor, pro.AnnualMinor = 2_000_000, 20_000_000
	p.Plans[billing.PlanPro] = pro
	soon := clk.t.Add(10 * 24 * time.Hour)
	draft, err := s.CreateDraft(ctx, p, soon, nil)
	if err != nil || draft.Version != 2 || draft.PublishedAt != nil {
		t.Fatalf("draft: %+v %v", draft, err)
	}
	if _, _, err := s.Publish(ctx, draft.Version, nil); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("publishing 10 days ahead: %v", err)
	}
	later := clk.t.Add(31 * 24 * time.Hour)
	if _, err := s.UpdateDraft(ctx, draft.Version, p, later, nil); err != nil {
		t.Fatal(err)
	}
	pub, notified, err := s.Publish(ctx, draft.Version, nil)
	if err != nil || pub.PublishedAt == nil {
		t.Fatalf("publish: %v", err)
	}
	// The monthly and annual Pro orgs hear about it; not the grandfathered
	// org nor Free (whose prices don't change).
	if notified != 2 || len(sent.all()) != 2 {
		t.Fatalf("notified %d: %+v", notified, sent.all())
	}
	for _, m := range sent.all() {
		if !strings.Contains(m.Body, "NGN 15,000.00 → NGN 20,000.00") && !strings.Contains(m.Body, "NGN 150,000.00 → NGN 200,000.00") {
			t.Errorf("notice body: %s", m.Body)
		}
	}
	if _, err := s.UpdateDraft(ctx, draft.Version, p, later, nil); !errors.Is(err, billing.ErrConflict) {
		t.Errorf("editing a published book: %v", err)
	}

	// Before the date nothing moves; after it, all but grandfathered and
	// running annual terms do.
	if n, err := s.Reprice(ctx); err != nil || n != 0 {
		t.Fatalf("early reprice: %d %v", n, err)
	}
	clk.t = later.Add(time.Hour)
	if n, err := s.Reprice(ctx); err != nil || n != 2 {
		t.Fatalf("reprice: %d %v", n, err)
	}
	for slug, want := range map[string]int32{"monthly": 2, "free": 2, "annual": 1, "grandfathered": 1} {
		if a, _ := s.Account(ctx, orgs[slug]); a.PriceBookVersion != want {
			t.Errorf("%s on version %d, want %d", slug, a.PriceBookVersion, want)
		}
	}
	// A new org gets the current book.
	if a, _ := s.Account(ctx, newOrg(t, db, "newcomer")); a.PriceBookVersion != 2 {
		t.Errorf("new org on version %d", a.PriceBookVersion)
	}
}

func TestContacts(t *testing.T) {
	s, db, _, _ := newService(t)
	ctx := context.Background()
	org := newOrg(t, db, "acme")
	if _, err := s.AddContact(ctx, org, "not an email", nil); !errors.Is(err, billing.ErrInvalid) {
		t.Errorf("bad address: %v", err)
	}
	if _, err := s.AddContact(ctx, org, "Ada <ada@example.com>", nil); !errors.Is(err, billing.ErrInvalid) {
		t.Errorf("named address: %v", err)
	}
	if c, err := s.AddContact(ctx, org, "AP@Example.com", nil); err != nil || c.Email != "ap@example.com" {
		t.Fatalf("add: %+v %v", c, err)
	}
	if err := s.RemoveContact(ctx, org, "ap@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveContact(ctx, org, "ap@example.com"); !errors.Is(err, billing.ErrNotFound) {
		t.Errorf("remove twice: %v", err)
	}
}

// TestContactVerification: a billing contact routes support email to the
// organisation only once verified (F2). A member's verified address is at
// once; any other gets a confirmation link, and billing email meanwhile.
func TestContactVerification(t *testing.T) {
	s, db, clk, sent := newService(t)
	ctx := context.Background()
	org := newOrg(t, db, "acme")
	if _, err := db.Exec(ctx, `WITH u AS (INSERT INTO users (email, password_hash, email_verified_at) VALUES ('fin@acme.example', 'x', now()) RETURNING id)
		INSERT INTO org_members (org_id, user_id, role) SELECT $1, id, 'billing' FROM u`, org); err != nil {
		t.Fatal(err)
	}
	if c, err := s.AddContact(ctx, org, "Fin@acme.example", nil); err != nil || c.VerifiedAt == nil {
		t.Fatalf("a member's address: %+v %v", c, err)
	}
	if len(sent.all()) != 0 {
		t.Error("a member's address was sent a confirmation")
	}

	c, err := s.AddContact(ctx, org, "victim@b.example", nil)
	if err != nil || c.VerifiedAt != nil {
		t.Fatalf("an outside address: %+v %v", c, err)
	}
	msgs := sent.all()
	if len(msgs) != 1 || msgs[0].To[0] != "victim@b.example" || !strings.Contains(msgs[0].Subject, "Confirm") {
		t.Fatalf("confirmation: %+v", msgs)
	}
	m := regexp.MustCompile(`/confirm-contact\?token=([A-Za-z0-9_-]+)`).FindStringSubmatch(msgs[0].Body)
	if m == nil {
		t.Fatalf("no link in %q", msgs[0].Body)
	}
	// Adding it again straight away doesn't send another.
	if _, err := s.AddContact(ctx, org, "victim@b.example", nil); err != nil || len(sent.all()) != 1 {
		t.Fatalf("again: %d %v", len(sent.all()), err)
	}
	// Billing email still goes to it.
	to, _ := store.New(db).BillingRecipients(ctx, org)
	if !slices.Contains(to, "victim@b.example") {
		t.Errorf("billing recipients: %v", to)
	}
	if _, _, err := s.ConfirmContact(ctx, "not-the-token"); !errors.Is(err, billing.ErrInvalid) {
		t.Errorf("a wrong token: %v", err)
	}
	got, name, err := s.ConfirmContact(ctx, m[1])
	if err != nil || got.VerifiedAt == nil || got.Email != "victim@b.example" || name != "acme" {
		t.Fatalf("confirm: %+v %q %v", got, name, err)
	}
	if _, _, err := s.ConfirmContact(ctx, m[1]); !errors.Is(err, billing.ErrInvalid) {
		t.Errorf("a used token: %v", err)
	}

	// A link past its week does nothing.
	if _, err := s.AddContact(ctx, org, "late@c.example", nil); err != nil {
		t.Fatal(err)
	}
	msgs = sent.all()
	late := regexp.MustCompile(`/confirm-contact\?token=([A-Za-z0-9_-]+)`).FindStringSubmatch(msgs[len(msgs)-1].Body)
	clk.t = clk.t.Add(billing.ContactConfirmTTL + time.Minute)
	if _, _, err := s.ConfirmContact(ctx, late[1]); !errors.Is(err, billing.ErrInvalid) {
		t.Errorf("an expired token: %v", err)
	}
}
