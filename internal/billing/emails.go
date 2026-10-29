package billing

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/store"
)

// The billing emails, written for people who run a business and for the
// developers who run its systems: what happened, what to do, by when, and
// the amounts in a table. Subjects keep their key phrases ("overdue balance",
// "wallet couldn't pay") because tests, filters and people search for them.

func (s *Service) billingURL(orgID uuid.UUID) string {
	return fmt.Sprintf("%s/org/billing?org=%s", s.publicURL, orgID)
}

func billingNote(org string) string {
	return "You're getting this because you're a billing contact for " + mail.Inert(org) + "."
}

// notifyContent emails an organisation's billing contacts a message written as
// content.
func (s *Service) notifyContent(ctx context.Context, orgID uuid.UUID, event, subject string, c mail.Content) {
	if s.mail == nil {
		return
	}
	to, err := store.New(s.db).BillingRecipients(ctx, orgID)
	if err != nil || len(to) == 0 {
		return
	}
	if c.Note == "" {
		c.Note = billingNote(s.orgName(ctx, orgID))
	}
	m := mail.Compose(to, subject, c)
	m.Headers = map[string]string{"X-Xenibase-Event": event}
	if err := s.mail.Send(ctx, m); err != nil {
		s.log.Warn("billing email", "org", orgID, "event", event, "err", err)
	}
}

// walletShortEmail is the first debit that didn't go through.
func (s *Service) walletShortEmail(org string, orgID uuid.UUID, why string) (string, mail.Content) {
	return fmt.Sprintf("%s's wallet couldn't pay its invoice", org), mail.Content{
		Preheader: "Add money to the wallet and we'll pay the invoice straight away.",
		Title:     "We couldn't pay your invoice from the wallet",
		Blocks: []mail.Block{
			mail.P("Hello,"),
			mail.P(fmt.Sprintf("We tried to pay %s's invoice from its wallet, but it didn't go through (%s).", org, why)),
			mail.P("To sort it out, add money to the wallet by bank transfer and we'll pay the invoice at once. We'll also try again in 3, 5 and 7 days."),
			mail.Button("Open billing", s.billingURL(orgID)),
		},
		SignOff: mail.SignOff,
	}
}

// walletShortAgainEmail is a retry that failed too.
func (s *Service) walletShortAgainEmail(org string, orgID uuid.UUID, day int) (string, mail.Content) {
	return fmt.Sprintf("%s's wallet couldn't pay its invoice again", org), mail.Content{
		Preheader: "Please add money to the wallet, or pay another way.",
		Title:     "The wallet still couldn't pay the invoice",
		Blocks: []mail.Block{
			mail.P("Hello,"),
			mail.P(fmt.Sprintf("We tried again on day %d, and the payment from %s's wallet still didn't go through.", day, org)),
			mail.P("Please add money to the wallet by bank transfer, or pay the invoice another way."),
			mail.Button("Open billing", s.billingURL(orgID)),
		},
		SignOff: mail.SignOff,
	}
}

// dunningEmail is one step of the overdue ladder.
func (s *Service) dunningEmail(step, org string, link string, deletion string) (string, mail.Content) {
	var subject, title, preheader string
	var body []string
	switch step {
	case "overdue":
		subject, title = org+" has an overdue balance", "A payment is overdue"
		preheader = "Paying now keeps everything running smoothly."
		body = []string{fmt.Sprintf("A payment for %s is overdue. Paying it now keeps everything running smoothly.", org)}
	case "restricted":
		subject, title = org+" can't create new resources until it pays", "New resources are paused until payment arrives"
		preheader = "Your existing databases keep running as normal."
		body = []string{"Payment is 3 days overdue, so creating projects, branches and dedicated instances is paused until it arrives.", "Your existing databases keep running as normal."}
	case "final_notice":
		subject, title = org+": final notice before suspension", "Final notice before suspension"
		preheader = "Payment is 7 days overdue."
		body = []string{"Payment is 7 days overdue. If it hasn't arrived within 3 days, we'll have to suspend the organisation: its databases go offline, and your data is kept safe."}
	case "suspended":
		subject, title = org+" is suspended for non-payment", "The organisation is suspended"
		preheader = "Paying reinstates everything at once."
		body = []string{"Payment is 10 days overdue, so the organisation is suspended. Its databases are offline, and its data is kept, with a final backup.", "Paying reinstates everything at once."}
	default: // deletion_scheduled
		subject, title = fmt.Sprintf("%s's paid resources will be deleted on %s", org, deletion), "Paid resources will be deleted on "+deletion
		preheader = "Paying before then cancels this."
		body = []string{fmt.Sprintf("Payment is 40 days overdue. On %s, the organisation's resources beyond the Free plan (its dedicated projects) will be deleted. Final backups are kept for 30 days.", deletion), "Paying before then cancels this."}
	}
	c := mail.Content{Preheader: preheader, Title: title, SignOff: mail.SignOff}
	c.Blocks = append(c.Blocks, mail.P("Hello,"))
	for _, p := range body {
		c.Blocks = append(c.Blocks, mail.P(p))
	}
	c.Blocks = append(c.Blocks, mail.Button("Pay now", link))
	return subject, c
}

func dunningClearedEmail(org string, reinstated bool) (string, mail.Content) {
	msg := "Your payment has arrived, and the restrictions are lifted."
	if reinstated {
		msg = "Your payment has arrived, the restrictions are lifted, and the organisation is reinstated."
	}
	return fmt.Sprintf("Thank you: %s is paid up", org), mail.Content{
		Preheader: msg,
		Title:     "Thank you, you're all paid up",
		Blocks:    []mail.Block{mail.P("Hello,"), mail.P(msg), mail.P("Thanks for sorting that out so quickly.")},
		SignOff:   mail.SignOff,
	}
}

func (s *Service) receiptEmail(org string, orgID uuid.UUID, p PaymentIn) (string, mail.Content) {
	return fmt.Sprintf("We've received your payment of %s", Naira(p.AmountMinor)), mail.Content{
		Preheader: "Thank you. Your receipt is in billing.",
		Title:     "Thank you, we've received your payment",
		Blocks: []mail.Block{
			mail.P("Hello,"),
			mail.P("Your payment has arrived."),
			mail.Facts(
				mail.Fact{Label: "Amount", Value: Naira(p.AmountMinor)},
				mail.Fact{Label: "Organisation", Value: org},
				mail.Fact{Label: "Received", Value: p.ReceivedAt.UTC().Format("2 January 2006")},
				mail.Fact{Label: "Reference", Value: p.ProviderRef},
			),
			mail.Button("View the receipt", s.billingURL(orgID)),
		},
		SignOff: mail.SignOff,
	}
}

func (s *Service) walletPaidEmail(org string, orgID uuid.UUID, invoice string, amount int64) (string, mail.Content) {
	return fmt.Sprintf("Invoice %s was paid from your wallet", invoice), mail.Content{
		Preheader: "Nothing more to do. Your receipt is in billing.",
		Title:     "Your invoice is paid",
		Blocks: []mail.Block{
			mail.P("Hello,"),
			mail.P(fmt.Sprintf("We paid %s's invoice from its wallet. There's nothing more for you to do.", org)),
			mail.Facts(
				mail.Fact{Label: "Invoice", Value: invoice},
				mail.Fact{Label: "Taken from the wallet", Value: Naira(amount)},
			),
			mail.Button("View the receipt", s.billingURL(orgID)),
		},
		SignOff: mail.SignOff,
	}
}

func (s *Service) prepaidEmptyEmail(org string, orgID uuid.UUID, bal int64) (string, mail.Content) {
	return fmt.Sprintf("%s's prepaid balance has run out", org), mail.Content{
		Preheader: fmt.Sprintf("Top up within %d days to avoid restrictions.", GraceDays),
		Title:     "The prepaid balance has run out",
		Blocks: []mail.Block{
			mail.P("Hello,"),
			mail.P(fmt.Sprintf("%s's prepaid balance is now %s. Please top it up within %d days to avoid restrictions. Your projects keep running in the meantime.", org, Naira(bal), GraceDays)),
			mail.Button("Top up", s.billingURL(orgID)),
		},
		SignOff: mail.SignOff,
	}
}

func (s *Service) prepaidLowEmail(org string, orgID uuid.UUID, bal int64, why string) (string, mail.Content) {
	return fmt.Sprintf("%s's prepaid balance is low", org), mail.Content{
		Preheader: "A quick top-up keeps things running.",
		Title:     "The prepaid balance is running low",
		Blocks: []mail.Block{
			mail.P("Hello,"),
			mail.P(fmt.Sprintf("%s's prepaid balance of %s covers %s.", org, Naira(bal), why)),
			mail.Button("Top up", s.billingURL(orgID)),
		},
		SignOff: mail.SignOff,
	}
}

func (s *Service) budgetEmail(org string, orgID uuid.UUID, month string, spend int64, pct int, budget int64) (string, mail.Content) {
	return fmt.Sprintf("%s's spend is forecast at %d%% of its budget", org, pct), mail.Content{
		Preheader: "A heads-up, so there are no surprises on the invoice.",
		Title:     fmt.Sprintf("Spend is forecast at %d%% of the budget", pct),
		Blocks: []mail.Block{
			mail.P("Hello,"),
			mail.P(fmt.Sprintf("A heads-up on %s's spend for %s, so there are no surprises on the invoice.", org, month)),
			mail.Facts(
				mail.Fact{Label: "Forecast (before VAT)", Value: Naira(spend)},
				mail.Fact{Label: "Monthly budget", Value: Naira(budget)},
				mail.Fact{Label: "Share of the budget", Value: fmt.Sprintf("%d%%", pct)},
			),
			mail.Button("See the forecast", s.billingURL(orgID)),
		},
		SignOff: mail.SignOff,
	}
}

func (s *Service) spendCapEmail(org string, orgID uuid.UUID, month string, usage, limit int64) (string, mail.Content) {
	return fmt.Sprintf("%s has reached its spend cap", org), mail.Content{
		Preheader: "Some new resources are paused. Your databases stay connected.",
		Title:     "The spend cap has been reached",
		Blocks: []mail.Block{
			mail.P("Hello,"),
			mail.P(fmt.Sprintf("Usage charges for %s in %s are forecast to be at or above the spend cap you set.", org, month)),
			mail.Facts(
				mail.Fact{Label: "Forecast usage", Value: Naira(usage)},
				mail.Fact{Label: "Spend cap", Value: Naira(limit)},
			),
			mail.P("Until you raise the cap, or next month starts:"),
			mail.List(
				"new branches, dedicated instances and HA are refused",
				"webhook deliveries queue, and scheduled jobs are skipped",
				"backend services slow down: data and storage API rate limits are tightened, new image transforms pause and new realtime connections are refused",
			),
			mail.P("Sign-in and existing connections carry on, your databases stay connected, and no data is deleted."),
			mail.Button("Change the cap", s.billingURL(orgID)),
		},
		SignOff: mail.SignOff,
	}
}
