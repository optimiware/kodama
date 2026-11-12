package installation

import "context"

// BackupPort performs one backup of the configured durable scope. It never
// includes disposable workspaces.
type BackupPort interface {
	Backup(context.Context, BackupPlan) (BackupResult, error)
}

// RestorePort reads one backup's preflight evidence, performs one fenced
// restore and reconciles external effects. It never enables dispatch;
// DispatchGate owns that transition.
type RestorePort interface {
	Preflight(context.Context, BackupIdentity) (BackupEvidence, error)
	Restore(context.Context, RestorePlan) (RestoreResult, error)
	Reconcile(context.Context) (ReconcileResult, error)
}

// FencePort fences old controllers, enforcing services and consumers before a
// restore can make changes. Fencing failure blocks the restore.
type FencePort interface {
	Fence(context.Context) error
}

// DispatchGate enables dispatch only after fencing and reconciliation. It is
// separate from RestorePort so no restore path can skip either step.
type DispatchGate interface {
	EnableDispatch(context.Context) error
}
