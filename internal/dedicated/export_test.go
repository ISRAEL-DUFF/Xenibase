package dedicated

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// RollbackSharedCluster exposes rollbackSharedCluster to the tests.
var RollbackSharedCluster = (*Service).rollbackSharedCluster

// ForgetUnlisted exposes the base backups' reconciliation with WAL-G.
var ForgetUnlisted = (*Service).forgetUnlisted

// DemotedSettings and Resets expose the demotion's settings rules.
var (
	DemotedSettings = demotedSettings
	Resets          = resets
)

// The rollbacks of the move-like operations.
var (
	FailMove    = (*Service).failMove
	FailPromote = (*Service).failPromote
	FailDemote  = (*Service).failDemote
)

// The handlers of the move-like operations.
var (
	RunMove    = (*Service).runMove
	RunPromote = (*Service).runPromote
	RunDemote  = (*Service).runDemote
)

// The postgresql.conf of a dedicated instance and of a shared cluster.
var (
	Settings       = settings
	SharedSettings = sharedSettings
)

// PlaceArchive exposes placeArchive.
var PlaceArchive = (*Service).placeArchive

// InsertUpgradeTarget exposes the major upgrade's target creation.
func InsertUpgradeTarget(s *Service, ctx context.Context, src store.Instance, version int) (uuid.UUID, error) {
	return s.insertUpgradeTarget(ctx, store.New(s.db), src, version)
}

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
