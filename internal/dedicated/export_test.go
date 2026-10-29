package dedicated

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/provision"
)

// RollbackSharedCluster exposes rollbackSharedCluster to the tests.
var RollbackSharedCluster = (*Service).rollbackSharedCluster

// DemotedSettings and Resets expose the demotion's settings rules.
var (
	DemotedSettings = demotedSettings
	Resets          = resets
)

// AgreedLeader exposes the watcher's reading of the members' views.
var AgreedLeader = agreedLeader

// SetHALockTiming shortens the watcher lock's retry and check (tests).
func SetHALockTiming(retry, check time.Duration) func() {
	r, c := haLockRetry, haLockCheck
	haLockRetry, haLockCheck = retry, check
	return func() { haLockRetry, haLockCheck = r, c }
}

// SetWatchResume replaces the RESUME a server sends when it gives up the
// watcher lock.
func (s *Service) SetWatchResume(f func(ctx context.Context, dbs ...string) error) {
	s.watch.mu.Lock()
	defer s.watch.mu.Unlock()
	s.watch.resume = f
}

// WatchPaused reports whether the watcher holds inst's poolers paused.
func (s *Service) WatchPaused(inst uuid.UUID) bool {
	st := s.watch.state(inst)
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.paused
}

// WatchOne polls one instance as the watcher does.
func (s *Service) WatchOne(ctx context.Context, inst uuid.UUID) error { return s.watchOne(ctx, inst) }

// LeadHA makes the watcher act as the lock holder without the lock.
func (s *Service) LeadHA() { s.watch.lead(true) }

// FailMinorUpgrade and RunMinorUpgrade expose the minor upgrade's OnFail
// and handler.
var (
	FailMinorUpgrade = (*Service).failMinorUpgrade
	RunMinorUpgrade  = (*Service).runMinorUpgrade
)

// Projects is the provision service the dedicated service uses.
func (s *Service) Projects() *provision.Service { return s.projects }

// BackupInstance exposes the instance a base backup came from.
var BackupInstance = (*Service).backupInstance
