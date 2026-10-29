package billing

import (
	"context"
	"time"

	"github.com/israel-duff/xenibase/internal/statusapi"
	"github.com/israel-duff/xenibase/internal/store"
)

// outageWindow is how far back a payment provider's last automatic charge
// counts for the status page.
const outageWindow = 30 * time.Minute

// Health is the billing component of the status page (V4.1 §7.2):
// degraded while a payment provider's last automatic charge in the last
// half hour hit an outage, or when last month's invoices (or an earlier
// month's) haven't issued by the 2nd. The detail is public: it says what,
// not for whom.
func (s *Service) Health(ctx context.Context, now time.Time) (statusapi.ComponentState, error) {
	st := statusapi.ComponentState{ID: ComponentBilling, Status: statusapi.Operational}
	q := store.New(s.db)
	providers, err := q.ProviderUnavailableSince(ctx, now.Add(-outageWindow))
	if err != nil {
		return st, err
	}
	for _, p := range providers {
		if p.Unavailable {
			st.Status, st.Detail = statusapi.Degraded, "A payment provider is unreachable: automatic payments are delayed and retried."
			return st, nil
		}
	}
	set, err := s.Settings(ctx)
	if err != nil {
		return st, err
	}
	if now.UTC().Day() >= 2 && set.AutoIssue {
		prev := MonthStart(now.UTC()).AddDate(0, -1, 0)
		n, err := q.UnissuedDrafts(ctx, pgDate(prev))
		if err != nil {
			return st, err
		}
		if n > 0 {
			st.Status, st.Detail = statusapi.Degraded, "Last month's invoices are late."
		}
	}
	return st, nil
}

// ComponentBilling is the status page's billing component.
const ComponentBilling = "billing"
