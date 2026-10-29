// Package schedjobs runs a project's scheduled jobs (V2 §9.2): SQL as the
// project owner, or an HTTP call through the outbound client, on a cron
// schedule in a time zone. One xenibase-server schedules (an advisory lock);
// missed runs are not caught up.
package schedjobs

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/outbound"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/tenancy"
)

var (
	// ErrInvalid is a bad request.
	ErrInvalid = errors.New("invalid job")
	// ErrConflict is a duplicate name.
	ErrConflict = errors.New("conflict")
)

// Job kinds, overlap policies, and run statuses.
const (
	KindSQL  = "sql"
	KindHTTP = "http"

	OverlapSkip  = "skip"
	OverlapQueue = "queue"

	RunQueued    = "queued"
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
	RunTimedOut  = "timed_out"
	RunSkipped   = "skipped"
)

// Defaults (V2 §9.2).
const (
	DefaultSQLTimeout  = 5 * time.Minute
	DefaultHTTPTimeout = 30 * time.Second
	MaxTimeout         = time.Hour
	MinInterval        = time.Minute
	// AlertAfter consecutive failures email the project's admins.
	AlertAfter = 3
)

const schedulerLockKey int64 = 0x7067646f636b06 // "xenibase\x06"

// Limits are an organisation's plan limits (the tenancy service).
type Limits interface {
	Limits(ctx context.Context, orgID uuid.UUID) (store.Limits, store.OrgWithPlanRow, error)
}

// SQLRunner runs a SQL job's script as the project's owner (the console
// service: through a login that holds no privileges of its own, so the
// script can't RESET ROLE to the superuser).
type SQLRunner interface {
	RunJob(ctx context.Context, p store.Project, sqlText string, timeout time.Duration) (int64, error)
}

// Config tunes the scheduler.
type Config struct {
	// Tick is how often due jobs are looked for.
	Tick time.Duration
	// Now is the clock.
	Now func() time.Time
	// PublicURL links emails to the UI.
	PublicURL string
}

// Service manages and runs scheduled jobs.
type Service struct {
	db       *pgxpool.Pool
	keyring  *crypto.Keyring
	projects *provision.Service
	sql      SQLRunner
	out      *outbound.Service
	limits   Limits
	mail     *mail.Service
	cfg      Config
	log      *slog.Logger

	wg sync.WaitGroup
	// life is the running scheduler's context: runs end with it (a server
	// stopping) rather than holding the shutdown for up to an hour.
	lifeMu sync.Mutex
	life   context.Context
}

func (s *Service) lifetime() context.Context {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.life != nil {
		return s.life
	}
	return context.Background()
}

// New returns a Service.
func New(db *pgxpool.Pool, keyring *crypto.Keyring, ps *provision.Service, sql SQLRunner, out *outbound.Service, limits Limits, m *mail.Service, cfg Config, log *slog.Logger) *Service {
	if cfg.Tick <= 0 {
		cfg.Tick = 5 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{db: db, keyring: keyring, projects: ps, sql: sql, out: out, limits: limits, mail: m, cfg: cfg, log: log}
}

// HTTPSpec is an HTTP job's request.
type HTTPSpec struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

// spec is what spec_enc holds.
type spec struct {
	SQL    string    `json:"sql,omitempty"`
	HTTP   *HTTPSpec `json:"http,omitempty"`
	Secret string    `json:"secret,omitempty"` // signs HTTP requests
}

func specAAD(id uuid.UUID) []byte { return []byte("scheduled_jobs.spec:" + id.String()) }

// Spec opens a job's SQL or request.
func (s *Service) Spec(j store.ScheduledJob) (sql string, h *HTTPSpec, secret string, err error) {
	b, err := s.keyring.Decrypt(j.SpecEnc, specAAD(j.ID))
	if err != nil {
		return "", nil, "", err
	}
	var sp spec
	if err := json.Unmarshal(b, &sp); err != nil {
		return "", nil, "", err
	}
	return sp.SQL, sp.HTTP, sp.Secret, nil
}

// Params configure a job.
type Params struct {
	Name     string
	Cron     string
	Timezone string
	Kind     string
	SQL      string
	HTTP     *HTTPSpec
	Timeout  time.Duration
	Overlap  string
	Enabled  bool
}

var (
	nameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.-]{0,63}$`)
	headerRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	methods  = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
)

// validate checks in for project p and returns its schedule.
func (s *Service) validate(ctx context.Context, p store.Project, in *Params) (Schedule, error) {
	in.Name = strings.TrimSpace(in.Name)
	if !nameRe.MatchString(in.Name) {
		return Schedule{}, fmt.Errorf("%w: a name of 1 to 64 letters, digits, spaces, dots, dashes or underscores", ErrInvalid)
	}
	if in.Timezone == "" {
		in.Timezone = "UTC"
	}
	sched, err := Parse(in.Cron, in.Timezone)
	if err != nil {
		return sched, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if s.cfg.Now != nil && sched.Next(s.cfg.Now()).IsZero() {
		return sched, fmt.Errorf("%w: the schedule never runs", ErrInvalid)
	}
	minGap := MinInterval
	if s.limits != nil {
		if l, _, err := s.limits.Limits(ctx, p.OrgID); err == nil {
			if v, ok := l.Get(store.LimitJobMinIntervalS); ok {
				minGap = max(minGap, time.Duration(v)*time.Second)
			}
		}
	}
	if g := sched.MinGap(s.cfg.Now()); g < minGap {
		return sched, &tenancy.QuotaError{Limit: store.LimitJobMinIntervalS, Used: int64(g.Seconds()), Max: int64(minGap.Seconds())}
	}
	if in.Overlap == "" {
		in.Overlap = OverlapSkip
	}
	if in.Overlap != OverlapSkip && in.Overlap != OverlapQueue {
		return sched, fmt.Errorf("%w: overlap is skip or queue", ErrInvalid)
	}
	switch in.Kind {
	case KindSQL:
		if strings.TrimSpace(in.SQL) == "" || len(in.SQL) > 1<<20 {
			return sched, fmt.Errorf("%w: SQL of 1 byte to 1 MiB", ErrInvalid)
		}
		in.HTTP = nil
		if in.Timeout == 0 {
			in.Timeout = DefaultSQLTimeout
		}
	case KindHTTP:
		if in.HTTP == nil {
			return sched, fmt.Errorf("%w: an HTTP job needs a request", ErrInvalid)
		}
		in.SQL = ""
		in.HTTP.Method = strings.ToUpper(in.HTTP.Method)
		if in.HTTP.Method == "" {
			in.HTTP.Method = http.MethodPost
		}
		if !slices.Contains(methods, in.HTTP.Method) {
			return sched, fmt.Errorf("%w: method is GET, POST, PUT, PATCH or DELETE", ErrInvalid)
		}
		if len(in.HTTP.Body) > 1<<20 || len(in.HTTP.Headers) > 20 {
			return sched, fmt.Errorf("%w: at most 20 headers and a 1 MiB body", ErrInvalid)
		}
		for k, v := range in.HTTP.Headers {
			lk := strings.ToLower(k)
			if !headerRe.MatchString(k) || strings.HasPrefix(lk, "xenibase-") || lk == "host" || lk == "content-length" || strings.ContainsAny(v, "\r\n") {
				return sched, fmt.Errorf("%w: header %q is not allowed", ErrInvalid, k)
			}
		}
		if _, err := s.out.Check(ctx, p.OrgID, in.HTTP.URL); err != nil {
			if errors.Is(err, outbound.ErrRefused) {
				return sched, fmt.Errorf("%w: %w", ErrInvalid, err)
			}
			return sched, err
		}
		if in.Timeout == 0 {
			in.Timeout = DefaultHTTPTimeout
		}
	default:
		return sched, fmt.Errorf("%w: kind is sql or http", ErrInvalid)
	}
	if in.Timeout < time.Second || in.Timeout > MaxTimeout {
		return sched, fmt.Errorf("%w: the timeout is 1 second to 1 hour", ErrInvalid)
	}
	return sched, nil
}

func newSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return "jobsec_" + base64.RawURLEncoding.EncodeToString(b)
}

// Created is a new job; Secret (HTTP jobs) is shown once.
type Created struct {
	Job    store.ScheduledJob
	Secret string
}

// Create configures a job, within the plan's job count and interval.
func (s *Service) Create(ctx context.Context, p store.Project, in Params, by *uuid.UUID) (Created, error) {
	sched, err := s.validate(ctx, p, &in)
	if err != nil {
		return Created{}, err
	}
	if s.limits != nil {
		if l, _, err := s.limits.Limits(ctx, p.OrgID); err == nil {
			if limit, ok := l.Get(store.LimitScheduledJobs); ok {
				n, err := store.New(s.db).CountOrgJobs(ctx, p.OrgID)
				if err != nil {
					return Created{}, err
				}
				if int64(n) >= limit {
					return Created{}, &tenancy.QuotaError{Limit: store.LimitScheduledJobs, Used: int64(n), Max: limit}
				}
			}
		}
	}
	id := uuid.New()
	sp := spec{SQL: in.SQL, HTTP: in.HTTP}
	if in.Kind == KindHTTP {
		sp.Secret = newSecret()
	}
	enc, err := s.seal(id, sp)
	if err != nil {
		return Created{}, err
	}
	next := s.next(sched, in.Enabled)
	j, err := store.New(s.db).InsertJob(ctx, store.InsertJobParams{
		ID: id, ProjectID: p.ID, Name: in.Name, Cron: in.Cron, Timezone: in.Timezone, Kind: in.Kind, SpecEnc: enc,
		TimeoutS: int32(in.Timeout.Seconds()), Overlap: in.Overlap, Enabled: in.Enabled, NextRunAt: next, CreatedBy: by,
	})
	if err != nil {
		return Created{}, conflict(err, in.Name)
	}
	return Created{Job: j, Secret: sp.Secret}, nil
}

func conflict(err error, name string) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return fmt.Errorf("%w: the project already has a job named %q", ErrConflict, name)
	}
	return err
}

func (s *Service) seal(id uuid.UUID, sp spec) ([]byte, error) {
	raw, err := json.Marshal(sp)
	if err != nil {
		return nil, err
	}
	return s.keyring.Encrypt(raw, specAAD(id))
}

func (s *Service) next(sched Schedule, enabled bool) *time.Time {
	if !enabled {
		return nil
	}
	n := sched.Next(s.cfg.Now())
	return &n
}

// Update changes a job; an HTTP job keeps its signing secret.
func (s *Service) Update(ctx context.Context, p store.Project, j store.ScheduledJob, in Params) (store.ScheduledJob, error) {
	sched, err := s.validate(ctx, p, &in)
	if err != nil {
		return j, err
	}
	_, _, secret, err := s.Spec(j)
	if err != nil {
		return j, err
	}
	sp := spec{SQL: in.SQL, HTTP: in.HTTP}
	if in.Kind == KindHTTP {
		sp.Secret = secret
		if sp.Secret == "" {
			sp.Secret = newSecret()
		}
	}
	enc, err := s.seal(j.ID, sp)
	if err != nil {
		return j, err
	}
	out, err := store.New(s.db).UpdateJob(ctx, store.UpdateJobParams{
		ID: j.ID, Name: in.Name, Cron: in.Cron, Timezone: in.Timezone, Kind: in.Kind, SpecEnc: enc,
		TimeoutS: int32(in.Timeout.Seconds()), Overlap: in.Overlap, Enabled: in.Enabled, NextRunAt: s.next(sched, in.Enabled),
	})
	return out, conflict(err, in.Name)
}

// SetEnabled pauses or resumes a job.
func (s *Service) SetEnabled(ctx context.Context, p store.Project, j store.ScheduledJob, enabled bool) (store.ScheduledJob, error) {
	sql, h, _, err := s.Spec(j)
	if err != nil {
		return j, err
	}
	return s.Update(ctx, p, j, Params{Name: j.Name, Cron: j.Cron, Timezone: j.Timezone, Kind: j.Kind, SQL: sql, HTTP: h,
		Timeout: time.Duration(j.TimeoutS) * time.Second, Overlap: j.Overlap, Enabled: enabled})
}

// Upcoming is a job's next n run times.
func Upcoming(j store.ScheduledJob, now time.Time, n int) []time.Time {
	sched, err := Parse(j.Cron, j.Timezone)
	if err != nil || !j.Enabled {
		return nil
	}
	return sched.Upcoming(now, n)
}

// ---- Running ---------------------------------------------------------------

// Run schedules due jobs until ctx ends, while this server holds the
// scheduler lock.
func (s *Service) Run(ctx context.Context) {
	for ctx.Err() == nil {
		conn, err := s.db.Acquire(ctx)
		if err != nil {
			sleep(ctx, 10*time.Second)
			continue
		}
		var got bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, schedulerLockKey).Scan(&got); err != nil || !got {
			conn.Release()
			sleep(ctx, 10*time.Second)
			continue
		}
		s.lifeMu.Lock()
		s.life = ctx
		s.lifeMu.Unlock()
		// Runs left queued or running by a stopped server.
		if _, err := store.New(s.db).FailInterruptedRuns(ctx, time.Now()); err != nil {
			s.log.Warn("fail interrupted job runs", "err", err)
		}
		lastSweep := time.Time{}
		for ctx.Err() == nil {
			if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("scheduler", "err", err)
			}
			if time.Since(lastSweep) > time.Hour {
				if _, err := store.New(s.db).SweepJobRuns(ctx, s.cfg.Now().AddDate(0, 0, -30)); err != nil && ctx.Err() == nil {
					s.log.Warn("sweep job runs", "err", err)
				}
				lastSweep = time.Now()
			}
			sleep(ctx, s.cfg.Tick)
		}
		s.wg.Wait()
		s.lifeMu.Lock()
		s.life = nil
		s.lifeMu.Unlock()
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, schedulerLockKey)
		conn.Release()
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// Tick starts the jobs that are due. A run that was missed is not caught
// up: the next time is computed from now.
func (s *Service) Tick(ctx context.Context) error {
	now := s.cfg.Now()
	q := store.New(s.db)
	due, err := q.DueJobs(ctx, now)
	if err != nil {
		return err
	}
	for _, j := range due {
		sched, err := Parse(j.Cron, j.Timezone)
		var next *time.Time
		if err == nil {
			n := sched.Next(now)
			next = &n
		}
		if err := q.SetJobNextRun(ctx, store.SetJobNextRunParams{ID: j.ID, NextRunAt: next}); err != nil {
			return err
		}
		if _, err := s.dispatch(ctx, j, *j.NextRunAt, "schedule"); err != nil {
			s.log.Warn("start job", "job_id", j.ID, "err", err)
		}
	}
	return nil
}

// RunNow starts a manual run of j.
func (s *Service) RunNow(ctx context.Context, j store.ScheduledJob) (store.JobRun, error) {
	return s.dispatch(ctx, j, s.cfg.Now(), "manual")
}

// dispatch records a run of j for scheduledFor and starts it, or records
// why it was skipped or queued.
func (s *Service) dispatch(ctx context.Context, j store.ScheduledJob, scheduledFor time.Time, trigger string) (store.JobRun, error) {
	q := store.New(s.db)
	skipped := func(q *store.Queries, reason string) (store.JobRun, error) {
		now := s.cfg.Now()
		return q.InsertJobRun(ctx, store.InsertJobRunParams{JobID: j.ID, ScheduledFor: scheduledFor, StartedAt: &now, FinishedAt: &now,
			Status: RunSkipped, Error: &reason, Trigger: trigger})
	}
	skip := func(reason string) (store.JobRun, error) { return skipped(q, reason) }
	p, err := q.GetProject(ctx, j.ProjectID)
	if err != nil {
		return store.JobRun{}, err
	}
	o, err := q.GetOrg(ctx, p.OrgID)
	if err != nil {
		return store.JobRun{}, err
	}
	switch {
	case o.Status != tenancy.OrgActive:
		return skip("the organisation is " + o.Status)
	case p.Status != provision.StatusActive:
		// Promoting, demoting, restoring, resetting (V2 §9.2).
		return skip("the project is " + p.Status)
	case p.Lifecycle != "active":
		// Paused or archived for inactivity (V3 §4.2).
		return skip("the project is " + p.Lifecycle)
	case j.Kind == KindHTTP && o.OutboundDisabled:
		return skip("outbound traffic is disabled for the organisation")
	}
	if capped, err := q.OrgSpendCapped(ctx, p.OrgID); err != nil {
		return store.JobRun{}, err
	} else if capped {
		return skip("the organisation has reached its spend cap")
	}
	if j.Kind == KindHTTP && s.limits != nil {
		if l, _, err := s.limits.Limits(ctx, p.OrgID); err == nil {
			limit, _ := l.Get(store.LimitHTTPJobRunsPerHour)
			if !s.out.Take(p.OrgID, "http_job", limit, time.Hour) {
				return skip(fmt.Sprintf("over the organisation's %d HTTP job runs per hour", limit))
			}
		}
	}
	// The overlap check and the insert run under the job's row lock: the
	// scheduler and a "Run now" served by another server would otherwise
	// both find no active run and both start one.
	var r store.JobRun
	started := false
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := q.WithTx(tx)
		if _, err := q.LockJob(ctx, j.ID); err != nil {
			return err
		}
		active, err := q.ActiveJobRuns(ctx, j.ID)
		if err != nil {
			return err
		}
		if len(active) > 0 {
			queued := slices.ContainsFunc(active, func(r store.JobRun) bool { return r.Status == RunQueued })
			if j.Overlap == OverlapSkip || queued {
				r, err = skipped(q, "the previous run is still going")
				return err
			}
			// Queue one run; it starts when the current one ends.
			r, err = q.InsertJobRun(ctx, store.InsertJobRunParams{JobID: j.ID, ScheduledFor: scheduledFor, Status: RunQueued, Trigger: trigger})
			return err
		}
		now := s.cfg.Now()
		r, err = q.InsertJobRun(ctx, store.InsertJobRunParams{JobID: j.ID, ScheduledFor: scheduledFor, StartedAt: &now, Status: RunRunning, Trigger: trigger})
		started = err == nil
		return err
	})
	if err != nil {
		return store.JobRun{}, err
	}
	if started {
		s.start(j, p, r)
	}
	return r, nil
}

func (s *Service) start(j store.ScheduledJob, p store.Project, r store.JobRun) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx := s.lifetime()
		s.execute(ctx, j, p, r)
		if ctx.Err() != nil {
			return // stopping: a queued run waits for the next server
		}
		// A queued run starts now, under the job's lock as in dispatch, so a
		// run queued while this one finished is not left behind.
		var next store.JobRun
		err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
			q := store.New(tx)
			if _, err := q.LockJob(ctx, j.ID); err != nil {
				return err
			}
			active, err := q.ActiveJobRuns(ctx, j.ID)
			if err != nil || slices.ContainsFunc(active, func(r store.JobRun) bool { return r.Status == RunRunning }) {
				return err
			}
			for _, r := range active {
				if r.Status == RunQueued {
					now := s.cfg.Now()
					if err := q.StartJobRun(ctx, store.StartJobRunParams{ID: r.ID, StartedAt: &now}); err != nil {
						return err
					}
					next = r
					next.Status, next.StartedAt = RunRunning, &now
					return nil
				}
			}
			return nil
		})
		if err != nil || next.ID == 0 {
			return
		}
		if cur, err := store.New(s.db).GetJobByID(ctx, j.ID); err == nil {
			s.start(cur, p, next)
		}
	}()
}

// execute runs r and records the outcome.
func (s *Service) execute(ctx context.Context, j store.ScheduledJob, p store.Project, r store.JobRun) {
	timeout := time.Duration(j.TimeoutS) * time.Second
	finish := store.FinishJobRunParams{ID: r.ID, Status: RunSucceeded}
	sqlText, h, secret, err := s.Spec(j)
	if err == nil {
		switch j.Kind {
		case KindSQL:
			var n int64
			n, err = s.runSQL(ctx, p, sqlText, timeout)
			finish.RowsAffected = &n
		case KindHTTP:
			var code int
			code, err = s.runHTTP(ctx, j, p, h, secret, timeout)
			if code > 0 {
				c := int32(code)
				finish.StatusCode = &c
			}
		}
	}
	interrupted := ctx.Err() != nil
	if interrupted {
		err = errInterrupted
	}
	// The outcome is recorded even while the server stops.
	ctx = context.WithoutCancel(ctx)
	if err != nil {
		finish.Status = RunFailed
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "57014" || errors.Is(err, context.DeadlineExceeded) {
			finish.Status = RunTimedOut
		}
		msg := err.Error()
		if len(msg) > 4096 {
			msg = msg[:4096]
		}
		finish.Error = &msg
	}
	now := s.cfg.Now()
	finish.FinishedAt = &now
	q := store.New(s.db)
	if err := q.FinishJobRun(ctx, finish); err != nil {
		s.log.Warn("record job run", "job_id", j.ID, "err", err)
	}
	cur, err := q.GetJobByID(ctx, j.ID)
	if err != nil {
		return
	}
	fails := int32(0)
	switch {
	case interrupted:
		return // not the job's failure
	case finish.Status != RunSucceeded:
		fails = cur.ConsecutiveFailures + 1
	}
	if fails != cur.ConsecutiveFailures {
		_ = q.SetJobFailures(ctx, store.SetJobFailuresParams{ID: j.ID, ConsecutiveFailures: fails})
	}
	if fails == AlertAfter {
		s.notify(ctx, p, fmt.Sprintf("[Xenibase] Job %s of %s failed %d times in a row", j.Name, p.Name, fails),
			fmt.Sprintf("The scheduled job %s of %s has failed %d runs in a row. The last error:\n\n%s", j.Name, p.Name, fails, deref(finish.Error)))
	}
}

// errInterrupted ends a run the stopping server cut short (the scheduler
// records the same for runs a crashed server left).
var errInterrupted = errors.New("interrupted: the server stopped during the run")

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// runSQL runs a job's SQL as the project owner in one transaction, with
// the job's timeout as statement_timeout. The connection does not lift a
// storage soft lock, unlike Xenibase's own admin connections.
func (s *Service) runSQL(ctx context.Context, p store.Project, sqlText string, timeout time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout+10*time.Second)
	defer cancel()
	return s.sql.RunJob(ctx, p, sqlText, timeout)
}

// runHTTP calls the job's URL through the outbound client, signed, with a
// Xenibase-Job header.
func (s *Service) runHTTP(ctx context.Context, j store.ScheduledJob, p store.Project, h *HTTPSpec, secret string, timeout time.Duration) (int, error) {
	if h == nil {
		return 0, errors.New("the job has no request")
	}
	hdr := http.Header{}
	for k, v := range h.Headers {
		hdr.Set(k, v)
	}
	if h.Body != "" && hdr.Get("Content-Type") == "" {
		hdr.Set("Content-Type", "application/json")
	}
	hdr.Set("Xenibase-Job", j.ID.String())
	resp, err := s.out.Do(ctx, outbound.Request{OrgID: p.OrgID, Method: h.Method, URL: h.URL, Header: hdr, Body: []byte(h.Body), Timeout: timeout, Secret: secret})
	if err != nil {
		return 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(resp.Snippet))
	}
	return resp.StatusCode, nil
}

func (s *Service) notify(ctx context.Context, p store.Project, subject, body string) {
	if s.mail == nil {
		return
	}
	addrs, err := store.New(s.db).ProjectAdminEmails(ctx, store.ProjectAdminEmailsParams{OrgID: p.OrgID, ProjectID: p.ID})
	if err != nil || len(addrs) == 0 {
		return
	}
	link := strings.TrimRight(s.cfg.PublicURL, "/") + "/projects/" + p.ID.String() + "/jobs"
	if err := s.mail.Send(ctx, mail.Notice(addrs, subject, body, "Open the job in Xenibase", link)); err != nil {
		s.log.Warn("send job email", "err", err)
	}
}

// Wait waits for runs in progress (tests).
func (s *Service) Wait() { s.wg.Wait() }
