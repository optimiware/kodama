package podman

import "github.com/optimiware/kodama/internal/domain"

// Names of the engine objects of an identity, for tests that plant or inspect
// objects at the names the kernel adapters derive.
var (
	WorkloadObjectName = func(scope domain.Scope, workloadID string) string { return objectName("kw-", scope, workloadID) }
	ServiceObjectName  = func(scope domain.Scope, identity string) string { return objectName("ks-", scope, identity) }
	VolumeObjectName   = func(scope domain.Scope, identity string) string { return objectName("kv-", scope, identity) }
	NetworkObjectName  = networkName
)
