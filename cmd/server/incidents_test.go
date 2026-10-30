package main

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// The incidents loop starts without a status page too: it is what ends
// announced maintenance.
func TestStartIncidentsWithoutStatusPage(t *testing.T) {
	for _, url := range []string{"", "https://status.example.com"} {
		ctx, cancel := context.WithCancel(context.Background())
		var bg sync.WaitGroup
		started := make(chan struct{})
		startIncidents(ctx, &bg, func(ctx context.Context, _ time.Duration) {
			close(started)
			<-ctx.Done()
		}, url, slog.New(slog.NewTextHandler(io.Discard, nil)))
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("status URL %q: the incidents loop did not start", url)
		}
		cancel()
		bg.Wait()
	}
}
