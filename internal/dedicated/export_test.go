package dedicated

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
