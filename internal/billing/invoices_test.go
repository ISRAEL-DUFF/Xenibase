package billing_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/billing"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/tenancy"
)

// usage records qty of metric for project at every hour of [from, to).
func usage(t *testing.T, db *pgxpool.Pool, org, project uuid.UUID, metric string, from, to time.Time, perHour string) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `
		INSERT INTO usage_records (org_id, project_id, metric, granularity, period_start, quantity, plan_id)
		SELECT $1, $2, $3, 'hour', h, $6::numeric, (SELECT plan_id FROM organizations WHERE id = $1)
		FROM generate_series($4::timestamptz, $5::timestamptz - interval '1 hour', interval '1 hour') AS h`,
		org, project, metric, from, to, perHour); err != nil {
		t.Fatal(err)
	}
}

func day(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }

// TestMonthWithMidMonthUpgrade rates a month of usage across a mid-month
// upgrade (the M20 done-when): proration by the day, each part's
// allowance, dedicated and HA hours, and the next month in advance; then
// issues it and checks the ledger.
func TestMonthWithMidMonthUpgrade(t *testing.T) {
	s, db, clk, sent := newService(t)
	ctx := context.Background()
	org := newOrg(t, db, "acme")
	shared, ded := uuid.New(), uuid.New()
	if _, err := s.AddContact(ctx, org, "ap@acme.example", nil); err != nil {
		t.Fatal(err)
	}
	sent.reset() // the contact's confirmation link
	legal := "Acme Nigeria Ltd"
	if _, err := s.UpdateDetails(ctx, org, billing.Details{LegalName: &legal, DeductsWHT: true, VATRegistered: true}); err != nil {
		t.Fatal(err)
	}

	// Pro from 1 October, Team from the 13th.
	clk.t = day(1)
	if _, err := s.ChangePlan(ctx, org, billing.PlanRequest{Plan: billing.PlanPro}); err != nil {
		t.Fatal(err)
	}
	clk.t = day(13).Add(15 * time.Hour)
	if _, err := s.ChangePlan(ctx, org, billing.PlanRequest{Plan: billing.PlanTeam}); err != nil {
		t.Fatal(err)
	}

	// 20 GB of shared storage all month; 200,000 webhook deliveries on the
	// 5th (Pro) and 300,000 on the 20th (Team); a 2 vCPU / 4,096 MB / 40 GB
	// dedicated instance all month, with an HA standby and synchronous
	// replication from the 20th.
	usage(t, db, org, shared, tenancy.MetricSharedStorage, day(1), day(1).AddDate(0, 1, 0), "20")
	usage(t, db, org, shared, tenancy.MetricWebhookSent, day(5), day(5).Add(time.Hour), "200000")
	usage(t, db, org, shared, tenancy.MetricWebhookSent, day(20), day(20).Add(time.Hour), "300000")
	for m, q := range map[string]string{tenancy.MetricDedicatedCPU: "2", tenancy.MetricDedicatedRAM: "4.096", tenancy.MetricDedicatedDisk: "40"} {
		usage(t, db, org, ded, m, day(1), day(1).AddDate(0, 1, 0), q)
	}
	for m, q := range map[string]string{tenancy.MetricHACPU: "2", tenancy.MetricHARAM: "4.096", tenancy.MetricHADisk: "40", tenancy.MetricSyncReplication: "1"} {
		usage(t, db, org, ded, m, day(20), day(1).AddDate(0, 1, 0), q)
	}
	// Usage outside October doesn't count.
	usage(t, db, org, shared, tenancy.MetricSharedStorage, day(1).AddDate(0, 1, 0), day(1).AddDate(0, 1, 1), "1000")

	r, err := s.Rate(ctx, org, day(31))
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		desc   string
		amount int64
	}{
		{"Team plan, November 2026", 6_000_000},
		{"Pro (monthly), 31 of 31 days", 1_500_000}, // Free → Pro on the 1st
		{"Unused Pro (monthly), 19 of 31 days", -919_355},
		{"Team (monthly), 19 of 31 days", 3_677_419},
		// 12 days × 480 GB-hours = 5,760, less 7,300 × 12/31 included, at 34.25 kobo.
		{"Shared storage (GB-hours) above the Pro allowance", 100_496},
		// 200,000 less 100,000 × 12/31 included, at 2 kobo; Team's 300,000 fit its allowance.
		{"Webhook deliveries above the Pro allowance", 322_581},
		{"Dedicated " + ded.String() + ": vCPU-hours", 4_077_120},   // 1,488 × 2,740
		{"Dedicated " + ded.String() + ": RAM GB-hours", 2_087_485}, // 3,047.424 × 685
		{"Dedicated " + ded.String() + ": Disk GB-hours", 1_019_280},
		{"HA standby " + ded.String() + ": vCPU-hours", 1_578_240}, // 576 × 2,740
		{"HA standby " + ded.String() + ": RAM GB-hours", 808_059},
		{"HA standby " + ded.String() + ": Disk GB-hours", 394_560},
		{"HA premium " + ded.String(), 556_172},              // 20% of 2,780,859
		{"Synchronous replication " + ded.String(), 394_560}, // 288 h × 1,370
	}
	if len(r.Lines) != len(want) {
		for _, l := range r.Lines {
			t.Logf("%-70s %d", l.Description, l.Amount)
		}
		t.Fatalf("%d lines, want %d", len(r.Lines), len(want))
	}
	var subtotal int64
	for _, w := range want {
		found := false
		for _, l := range r.Lines {
			if strings.HasPrefix(l.Description, w.desc) {
				found = true
				if l.Amount != w.amount {
					t.Errorf("%s: %d, want %d", w.desc, l.Amount, w.amount)
				}
			}
		}
		if !found {
			t.Errorf("no line %q", w.desc)
		}
		subtotal += w.amount
	}
	if r.Subtotal != subtotal || r.VAT != billing.DecInt(subtotal).Mul(billing.D("0.075")).Round() || r.Total != r.Subtotal+r.VAT {
		t.Errorf("totals: subtotal %d (want %d), VAT %d, total %d", r.Subtotal, subtotal, r.VAT, r.Total)
	}
	if r.WHTExpected != billing.DecInt(subtotal).Mul(billing.D("0.05")).Round() {
		t.Errorf("WHT %d", r.WHTExpected)
	}

	// Draft, re-draft (idempotent), issue.
	clk.t = day(31).Add(20 * time.Hour)
	inv, err := s.Draft(ctx, org, day(31))
	if err != nil || inv == nil || inv.Status != billing.StatusDraft {
		t.Fatalf("draft: %+v %v", inv, err)
	}
	again, err := s.Draft(ctx, org, day(31))
	if err != nil || again.ID != inv.ID || again.TotalMinor != r.Total {
		t.Fatalf("re-draft: %+v %v", again, err)
	}
	q := store.New(db)
	if lines, _ := q.InvoiceLines(ctx, inv.ID); len(lines) != len(want) {
		t.Fatalf("draft has %d lines", len(lines))
	}
	if _, err := s.Hold(ctx, inv.ID, true, ptrTo("usage dispute")); err != nil {
		t.Fatal(err)
	}
	clk.t = time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	set, _ := s.Settings(ctx)
	set.AutoIssue = true
	if err := s.SetSettings(ctx, set); err != nil {
		t.Fatal(err)
	}
	if n, err := s.IssueDue(ctx, day(1)); err != nil || n != 0 {
		t.Fatalf("held drafts aren't issued: %d %v", n, err)
	}
	issued, err := s.Issue(ctx, inv.ID, nil) // the admin issues it
	if err != nil {
		t.Fatal(err)
	}
	if issued.Number == nil || *issued.Number != "XB-2026-000001" || issued.Status != billing.StatusIssued ||
		!issued.DueAt.Equal(clk.t.AddDate(0, 0, 14)) {
		t.Fatalf("issued: %+v", issued)
	}
	if _, err := s.Issue(ctx, inv.ID, nil); !errors.Is(err, billing.ErrConflict) {
		t.Errorf("issuing twice: %v", err)
	}
	if again, err := s.Draft(ctx, org, day(31)); err != nil || again.Status != billing.StatusIssued {
		t.Errorf("re-drafting an issued month: %+v %v", again, err)
	}
	if n := len(sent.all()); n != 1 || !strings.Contains(sent.all()[0].Subject, "XB-2026-000001") || sent.all()[0].To[0] != "ap@acme.example" {
		t.Errorf("invoice email: %+v", sent.all())
	}

	// The ledger: what the org owes is the total; revenue by product line.
	mustCheck(t, db)
	if b := balance(t, db, &org, billing.AccReceivable); b != r.Total {
		t.Errorf("receivable %d, want %d", b, r.Total)
	}
	if b := balance(t, db, nil, billing.AccVATPayable); b != -r.VAT {
		t.Errorf("VAT payable %d, want %d", b, -r.VAT)
	}
	if b := balance(t, db, nil, billing.RevenueHA); b != -(1_578_240 + 808_059 + 394_560 + 556_172 + 394_560) {
		t.Errorf("HA revenue %d", b)
	}

	// Credit notes: VAT at the invoice's rate, never more than the total.
	cn, err := s.CreditNote(ctx, inv.ID, 10_000, "Goodwill for the October outage", nil)
	if err != nil || cn.VatMinor != 750 || cn.Number != "XB-CN-2026-000001" {
		t.Fatalf("credit note: %+v %v", cn, err)
	}
	if b := balance(t, db, &org, billing.AccReceivable); b != r.Total-10_750 {
		t.Errorf("receivable after the credit note %d", b)
	}
	if _, err := s.CreditNote(ctx, inv.ID, r.Total, "too much", nil); !errors.Is(err, billing.ErrInvalid) {
		t.Errorf("over-credit: %v", err)
	}
	mustCheck(t, db)

	pdf, err := billing.InvoicePDF(issued, mustLines(t, q, inv.ID), []store.CreditNote{cn})
	if err != nil || !bytes.HasPrefix(pdf, []byte("%PDF")) || len(pdf) < 2000 {
		t.Fatalf("PDF: %d bytes, %v", len(pdf), err)
	}
}

// TestInvoicingCycle runs the monthly cycle: nothing before usage is in,
// then drafts and issues for the past month, numbered in order.
func TestInvoicingCycle(t *testing.T) {
	s, db, clk, _ := newService(t)
	ctx := context.Background()
	q := store.New(db)
	set, _ := s.Settings(ctx)
	set.AutoIssue = true
	if err := s.SetSettings(ctx, set); err != nil {
		t.Fatal(err)
	}
	var orgs []uuid.UUID
	for _, slug := range []string{"one", "two", "idle"} {
		org := newOrg(t, db, slug)
		orgs = append(orgs, org)
		if slug != "idle" {
			clk.t = day(2)
			if _, err := s.ChangePlan(ctx, org, billing.PlanRequest{Plan: billing.PlanPro}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// September's usage, from before billing started, is never invoiced.
	usage(t, db, orgs[0], uuid.New(), tenancy.MetricDedicatedCPU, day(1).AddDate(0, -1, 0), day(1), "4")

	// The last day of October: drafts.
	clk.t = day(31).Add(22 * time.Hour)
	if err := s.Invoicing(ctx); err != nil {
		t.Fatal(err)
	}
	drafts, _ := q.DraftsForPeriod(ctx, pgDate(day(1)))
	if len(drafts) != 2 {
		t.Fatalf("%d drafts on the 31st, want 2 (the idle Free org has nothing)", len(drafts))
	}
	// 1 November, before usage recording has caught up: nothing issued.
	clk.t = time.Date(2026, 11, 1, 0, 30, 0, 0, time.UTC)
	putWatermark(t, db, time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC))
	if err := s.Invoicing(ctx); err != nil {
		t.Fatal(err)
	}
	if drafts, _ := q.DraftsForPeriod(ctx, pgDate(day(1))); len(drafts) != 2 {
		t.Fatalf("issued before usage was in: %d drafts left", len(drafts))
	}
	putWatermark(t, db, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if err := s.Invoicing(ctx); err != nil {
		t.Fatal(err)
	}
	if drafts, _ := q.DraftsForPeriod(ctx, pgDate(day(1))); len(drafts) != 0 {
		t.Fatalf("%d drafts left after the 1st", len(drafts))
	}
	rows, _ := q.ListInvoices(ctx, store.ListInvoicesParams{Lim: 10})
	var numbers []string
	for _, r := range rows {
		numbers = append(numbers, *r.Number)
		if r.PeriodStart.Time.Month() != time.October {
			t.Errorf("invoice for %s", r.PeriodStart.Time)
		}
	}
	if strings.Join(numbers, ",") != "XB-2026-000001,XB-2026-000002" && strings.Join(numbers, ",") != "XB-2026-000002,XB-2026-000001" {
		t.Errorf("numbers %v", numbers)
	}
	// Running again changes nothing.
	if err := s.Invoicing(ctx); err != nil {
		t.Fatal(err)
	}
	if rows, _ := q.ListInvoices(ctx, store.ListInvoicesParams{Lim: 10}); len(rows) != 2 {
		t.Errorf("%d invoices after a second run", len(rows))
	}
	mustCheck(t, db)
}

func putWatermark(t *testing.T, db *pgxpool.Pool, at time.Time) {
	t.Helper()
	raw := []byte(`{"hour": "` + at.Format(time.RFC3339) + `", "day": "` + at.Truncate(24*time.Hour).Format(time.RFC3339) + `"}`)
	if err := store.New(db).PutSetting(context.Background(), store.PutSettingParams{Key: "usage.watermark", Value: raw}); err != nil {
		t.Fatal(err)
	}
}

func mustLines(t *testing.T, q *store.Queries, id uuid.UUID) []store.InvoiceLine {
	t.Helper()
	l, err := q.InvoiceLines(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func ptrTo[T any](v T) *T { return &v }

func pgDate(t time.Time) pgtype.Date { return pgtype.Date{Time: t, Valid: true} }

// TestBackendServicesRating rates V4's metrics (V4 §12): each above the
// plan's allowance at its unit price, and SMS and WhatsApp codes at their
// provider cost plus the margin; LineService groups the lines.
func TestBackendServicesRating(t *testing.T) {
	s, db, clk, _ := newService(t)
	ctx := context.Background()
	org := newOrg(t, db, "appco")
	app := uuid.New()
	clk.t = day(1)
	if _, err := s.ChangePlan(ctx, org, billing.PlanRequest{Plan: billing.PlanPro}); err != nil {
		t.Fatal(err)
	}
	once := func(metric, qty string) { usage(t, db, org, app, metric, day(10), day(10).Add(time.Hour), qty) }
	once(tenancy.MetricAPIRequests, "6000000")       // 1M over 5M
	once(tenancy.MetricAuthMAU, "51000")             // 1,000 over 50,000
	once(tenancy.MetricImageTransforms, "12000")     // 2,000 over 10,000
	once(tenancy.MetricRealtimeMessages, "11000000") // 1M over 10M
	once(tenancy.MetricAPIEgress, "100")             // within 250 GB
	once(tenancy.MetricMessagesSMS, "100")
	once(tenancy.MetricMessagesSMSCost, "40000") // ₦4 each
	once(tenancy.MetricMessagesWhatsApp, "10")
	once(tenancy.MetricMessagesWhatsAppCost, "10000")
	// 60 GB of files all month: 44,640 GB-hours, 8,140 over 36,500.
	usage(t, db, org, app, tenancy.MetricStorageGBHours, day(1), day(1).AddDate(0, 1, 0), "60")

	r, err := s.Rate(ctx, org, day(31))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		amount  int64
		service string
	}{
		"Data API requests above the Pro allowance":                  {300_000, "data_api"}, // 1M × 0.3 kobo
		"Monthly active users above the Pro allowance":               {500_000, "auth"},     // 1,000 × ₦5
		"Image transforms above the Pro allowance":                   {2_000, "storage"},
		"Realtime messages above the Pro allowance":                  {400_000, "realtime"},
		"File storage (GB-hours) above the Pro allowance":            {33_455, "storage"}, // 8,140 × 4.11
		"SMS codes: 100 sent, provider cost NGN 400.00 plus 20%":     {48_000, "messages"},
		"WhatsApp codes: 10 sent, provider cost NGN 100.00 plus 20%": {12_000, "messages"},
	}
	got := map[string]bool{}
	for _, l := range r.Lines {
		for prefix, w := range want {
			if strings.HasPrefix(l.Description, prefix) {
				got[prefix] = true
				if l.Amount != w.amount {
					t.Errorf("%s: %d, want %d", prefix, l.Amount, w.amount)
				}
				if svc := billing.LineService(l); svc != w.service {
					t.Errorf("%s: service %s, want %s", prefix, svc, w.service)
				}
			}
		}
		if strings.HasPrefix(l.Description, "Data API transfer") {
			t.Errorf("transfer within the allowance was charged: %s", l.Description)
		}
	}
	for prefix := range want {
		if !got[prefix] {
			for _, l := range r.Lines {
				t.Logf("%-70s %d", l.Description, l.Amount)
			}
			t.Fatalf("no line %q", prefix)
		}
	}
}
