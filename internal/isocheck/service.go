package isocheck

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// Kind is the operation that checks one shared cluster.
const Kind = "isolation_check"

// Every is how often each shared cluster is checked: nightly in V2 (§10.4,
// the forbidden-capabilities audit), weekly in V1.
const Every = 24 * time.Hour

// Service schedules and runs isolation checks.
type Service struct {
	db       *pgxpool.Pool
	projects *provision.Service
	log      *slog.Logger
}

// New returns a Service.
func New(db *pgxpool.Pool, projects *provision.Service, log *slog.Logger) *Service {
	return &Service{db: db, projects: projects, log: log}
}

// Kinds registers the operation.
func (s *Service) Kinds() map[string]jobs.Kind {
	return map[string]jobs.Kind{Kind: {Handler: s.run, MaxAttempts: 2, Timeout: 30 * time.Minute, Class: jobs.ClassScheduled}}
}

type params struct {
	InstanceID uuid.UUID `json:"instance_id"`
}

func (s *Service) run(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	var p params
	if err := json.Unmarshal(op.Params, &p); err != nil {
		return jobs.Permanent(fmt.Errorf("invalid params: %w", err))
	}
	insts, err := store.New(s.db).ListLiveSharedInstances(ctx)
	if err != nil {
		return err
	}
	var inst *store.ListLiveSharedInstancesRow
	for i := range insts {
		if insts[i].ID == p.InstanceID {
			inst = &insts[i]
		}
	}
	if inst == nil {
		return jobs.Permanent(fmt.Errorf("shared cluster %s no longer exists", p.InstanceID))
	}
	if err := log.Info(ctx, "start", "checking the shared cluster on %s (spec §7.1)", inst.NodeName); err != nil {
		return err
	}
	findings, err := Instance(ctx, s.db, s.projects, *inst, func(format string, args ...any) {
		_ = log.Info(ctx, "check", format, args...)
	})
	if err != nil {
		return err
	}
	for _, f := range findings {
		if err := log.Error(ctx, "finding", "%s", f); err != nil {
			return err
		}
	}
	if len(findings) > 0 {
		msgs := make([]string, 0, 3)
		for _, f := range findings[:min(3, len(findings))] {
			msgs = append(msgs, f.String())
		}
		return jobs.Permanent(fmt.Errorf("%d isolation finding(s) on %s: %s", len(findings), inst.NodeName, strings.Join(msgs, "; ")))
	}
	return log.Info(ctx, "done", "every §7.1 check passed; the throwaway tenants are dropped")
}

// Enqueue queues a check of one shared cluster.
func (s *Service) Enqueue(ctx context.Context, instanceID uuid.UUID, by *uuid.UUID) (store.Operation, error) {
	return jobs.Enqueue(ctx, s.db, jobs.EnqueueParams{Kind: Kind, Params: params{InstanceID: instanceID}, CreatedBy: by})
}

// EnqueueAll queues a check of every shared cluster.
func (s *Service) EnqueueAll(ctx context.Context, by *uuid.UUID) ([]store.Operation, error) {
	insts, err := store.New(s.db).ListLiveSharedInstances(ctx)
	if err != nil {
		return nil, err
	}
	var ops []store.Operation
	for _, inst := range insts {
		if inst.Status != "running" {
			continue
		}
		op, err := s.Enqueue(ctx, inst.ID, by)
		if err != nil {
			return ops, err
		}
		ops = append(ops, op)
	}
	return ops, nil
}

// Due queues a check of each running shared cluster not checked (or
// being checked) within Every.
func (s *Service) Due(ctx context.Context) error {
	q := store.New(s.db)
	insts, err := q.ListLiveSharedInstances(ctx)
	if err != nil {
		return err
	}
	latest, err := q.LatestIsolationChecks(ctx)
	if err != nil {
		return err
	}
	last := map[uuid.UUID]store.LatestIsolationChecksRow{}
	for _, l := range latest {
		last[l.InstanceID] = l
	}
	for _, inst := range insts {
		if inst.Status != "running" {
			continue
		}
		if l, ok := last[inst.ID]; ok && (l.Status == "queued" || l.Status == "running" || time.Since(l.CreatedAt) < Every) {
			continue
		}
		op, err := s.Enqueue(ctx, inst.ID, nil)
		if err != nil {
			return err
		}
		s.log.Info("isolation check queued", "instance_id", inst.ID, "node", inst.NodeName, "operation_id", op.ID)
	}
	return nil
}

// Run queues due checks every hour until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := s.Due(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("schedule isolation checks", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Latest returns the newest check per shared cluster.
func (s *Service) Latest(ctx context.Context) ([]store.ListLiveSharedInstancesRow, map[uuid.UUID]store.LatestIsolationChecksRow, error) {
	q := store.New(s.db)
	insts, err := q.ListLiveSharedInstances(ctx)
	if err != nil {
		return nil, nil, err
	}
	latest, err := q.LatestIsolationChecks(ctx)
	if err != nil {
		return nil, nil, err
	}
	m := map[uuid.UUID]store.LatestIsolationChecksRow{}
	for _, l := range latest {
		m[l.InstanceID] = l
	}
	return insts, m, nil
}
