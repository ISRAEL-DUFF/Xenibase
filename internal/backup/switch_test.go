package backup

import (
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/store"
)

// An archive a storage or key switch retires stays restorable for the
// project's whole point-in-time window, the 14 or 30 days it pays for, not
// only the standard 7.
func TestRetiredArchiveKeptForThePaidWindow(t *testing.T) {
	day := 24 * time.Hour
	for days, want := range map[int32]time.Duration{0: 7 * day, 7: 7 * day, 14: 14 * day, 30: 30 * day} {
		if got := retiredArchiveKept(store.Instance{PitrDays: days}); got != want {
			t.Errorf("pitr_days %d: kept %s, want %s", days, got, want)
		}
	}
}
