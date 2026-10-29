package dedicated

import (
	"context"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/store"
)

// RollbackSharedCluster exposes rollbackSharedCluster to the tests.
var RollbackSharedCluster = (*Service).rollbackSharedCluster

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
