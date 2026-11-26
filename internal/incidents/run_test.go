package incidents

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// Without a status page, Run still ends announced maintenance whose
// window is over.
func TestRunWithoutStatusPageEndsMaintenance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := storetest.New(t)
	q := store.New(db)
	end := time.Now().Add(-time.Minute)
	m, err := q.InsertMaintenance(ctx, store.InsertMaintenanceParams{
		Title: "Kernel updates", Components: []string{ComponentBackups}, ScheduledStart: end.Add(-time.Hour), ScheduledEnd: &end,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := New(db, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx, time.Minute) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var status string
		if err := db.QueryRow(ctx, `SELECT status FROM incidents WHERE id = $1`, m.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "resolved" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("maintenance still %s", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
}
