package alerts

import (
	"fmt"
	"time"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/store"
)

// stuckCondition is project_stuck for a live project in a status other
// than active with no operation to move it on.
func stuckCondition(p store.Project) condition {
	detail := map[string]any{"status": p.Status}
	switch p.Status {
	case "moving", "promoting", "demoting", "upgrading":
		// RepairStuckMoves retries these every minute.
		detail["repaired_automatically"] = true
		return condition{KindProjectStuck, SeverityCritical, "project", p.ID.String(), p.Name,
			fmt.Sprintf("%s is still %s after its operation failed: the rollback cannot give its logins back until its instance answers", p.Name, p.Status),
			detail}
	case "error":
		return condition{KindProjectStuck, SeverityWarning, "project", p.ID.String(), p.Name,
			fmt.Sprintf("%s is in error and nothing is running on it: read its last operation's log, then restore or delete it", p.Name),
			detail}
	default:
		// provisioning, restoring, deleting: the operation ended without
		// setting the project's final status, and nothing repairs it.
		return condition{KindProjectStuck, SeverityCritical, "project", p.ID.String(), p.Name,
			fmt.Sprintf("%s is still %s but no operation is queued or running for it: its last operation ended without finishing it, and it needs repair by hand (docs/operations.md)", p.Name, p.Status),
			detail}
	}
}

// queueAgeConditions is queue_age for each class whose oldest runnable
// operation has waited longer than queueAgeAfter allows.
func queueAgeConditions(rows []store.QueuedOperationAgesRow, classOf func(string) jobs.Class) []condition {
	type class struct {
		queued     int64
		waited     float64
		oldestKind string
	}
	byClass := map[jobs.Class]*class{}
	for _, r := range rows {
		c := jobs.ClassInteractive
		if classOf != nil {
			c = classOf(r.Kind)
		}
		b := byClass[c]
		if b == nil {
			b = &class{}
			byClass[c] = b
		}
		b.queued += r.Queued
		if r.WaitedSeconds > b.waited {
			b.waited, b.oldestKind = r.WaitedSeconds, r.Kind
		}
	}
	var out []condition
	for _, c := range jobs.Classes {
		b := byClass[c]
		after := queueAgeAfter[c]
		if b == nil || b.waited < after.Seconds() {
			continue
		}
		severity := SeverityWarning
		if c == jobs.ClassInteractive {
			severity = SeverityCritical
		}
		waited := (time.Duration(b.waited) * time.Second).Round(time.Minute)
		out = append(out, condition{KindQueueAge, severity, "control_plane", "queue:" + string(c), string(c) + " operations",
			fmt.Sprintf("The oldest runnable %s operation (%s) has waited %s for a worker, with %d %s queued: every slot that may run it is busy, or no server runs its kind",
				c, b.oldestKind, waited, b.queued, plural(b.queued, "operation")),
			map[string]any{"class": string(c), "queued": b.queued, "waited_seconds": b.waited, "oldest_kind": b.oldestKind,
				"threshold_seconds": after.Seconds()}})
	}
	return out
}

func plural(n int64, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
