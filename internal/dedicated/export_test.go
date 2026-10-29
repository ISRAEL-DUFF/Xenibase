package dedicated

// RollbackSharedCluster exposes rollbackSharedCluster to the tests.
var RollbackSharedCluster = (*Service).rollbackSharedCluster

// ForgetUnlisted exposes the base backups' reconciliation with WAL-G.
var ForgetUnlisted = (*Service).forgetUnlisted

// DemotedSettings and Resets expose the demotion's settings rules.
var (
	DemotedSettings = demotedSettings
	Resets          = resets
)
