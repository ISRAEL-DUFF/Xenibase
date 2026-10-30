package jobs_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// partitionProxy forwards TCP to the metadata database until cut, when it
// closes every connection and refuses new ones: a worker partitioned from
// the metadata database.
type partitionProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	cut    bool
	conns  []net.Conn
}

func newPartitionProxy(t *testing.T, target string) *partitionProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &partitionProxy{ln: ln, target: target}
	t.Cleanup(func() { _ = ln.Close(); p.setCut(true) })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			if p.cut {
				p.mu.Unlock()
				_ = c.Close()
				continue
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				p.mu.Unlock()
				_ = c.Close()
				continue
			}
			p.conns = append(p.conns, c, up)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
			go func() { _, _ = io.Copy(c, up); _ = c.Close() }()
		}
	}()
	return p
}

func (p *partitionProxy) setCut(cut bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cut = cut
	if cut {
		for _, c := range p.conns {
			_ = c.Close()
		}
		p.conns = nil
	}
}

// proxiedPool is a pool on the same database as pool, through p.
func proxiedPool(t *testing.T, pool *pgxpool.Pool) (*pgxpool.Pool, *partitionProxy) {
	t.Helper()
	cfg := pool.Config().Copy()
	p := newPartitionProxy(t, net.JoinHostPort(cfg.ConnConfig.Host, strconv.Itoa(int(cfg.ConnConfig.Port))))
	addr := p.ln.Addr().(*net.TCPAddr)
	cfg.ConnConfig.Host, cfg.ConnConfig.Port = addr.IP.String(), uint16(addr.Port)
	cfg.ConnConfig.ConnectTimeout = time.Second
	pp, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pp.Close)
	return pp, p
}

// A worker cut off from the metadata database stops its attempt before the
// lease runs out, and once the operation was reclaimed (here by the same
// worker), the old attempt can write nothing: not a log line, not a result.
func TestPartitionedAttemptStopsAndIsFenced(t *testing.T) {
	pool := storetest.New(t)
	viaProxy, proxy := proxiedPool(t, pool)
	cfg := fastConfig("w")

	started1, started2 := make(chan struct{}), make(chan struct{})
	release1, release2 := make(chan struct{}), make(chan struct{})
	type firstResult struct {
		cancelled bool
		logErr    error
	}
	first := make(chan firstResult, 1)
	kinds := map[string]jobs.Kind{"work": {MaxAttempts: 3, Timeout: 20 * time.Second,
		Handler: func(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
			switch op.Attempts {
			case 1:
				close(started1)
				var res firstResult
				select {
				case <-ctx.Done():
					res.cancelled = true
				case <-release1:
				}
				<-release1
				res.logErr = log.Info(context.Background(), "late", "attempt 1 still writing")
				first <- res
				return nil
			default:
				close(started2)
				<-release2
				return nil
			}
		}}}
	start(t, viaProxy, cfg, kinds)
	var once1, once2 sync.Once
	free1 := func() { once1.Do(func() { close(release1) }) }
	free2 := func() { once2.Do(func() { close(release2) }) }
	t.Cleanup(func() { free1(); free2() }) // runs before the runner stops

	op := enqueue(t, pool, "work", nil)
	<-started1
	proxy.setCut(true)
	time.Sleep(cfg.LeaseTTL + 200*time.Millisecond)
	// Another worker's reclaim loop, on the other side of the partition.
	if _, err := store.New(pool).ReclaimStaleOperations(context.Background(), cfg.LeaseTTL.Seconds()); err != nil {
		t.Fatal(err)
	}
	proxy.setCut(false)

	select {
	case <-started2:
	case <-time.After(10 * time.Second):
		t.Fatal("the reclaimed operation was not run again")
	}
	free1()
	res := <-first
	if !res.cancelled {
		t.Error("attempt 1 kept running after its heartbeats failed for longer than the lease")
	}
	if !errors.Is(res.logErr, jobs.ErrLeaseLost) {
		t.Errorf("attempt 1 wrote to the log after losing its lease: %v", res.logErr)
	}
	time.Sleep(200 * time.Millisecond)
	got, err := jobs.Get(context.Background(), pool, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != jobs.StatusRunning || got.Attempts != 2 {
		t.Fatalf("attempt 1 finished the operation attempt 2 is running: status=%s attempts=%d", got.Status, got.Attempts)
	}
	free2()
	if done := waitTerminal(t, pool, op.ID); done.Status != jobs.StatusSucceeded || done.Attempts != 2 {
		t.Fatalf("unexpected result: status=%s attempts=%d", done.Status, done.Attempts)
	}
}
