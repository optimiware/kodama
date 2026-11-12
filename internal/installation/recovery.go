package installation

// Backup and restore orchestration over injected ports. The runner never opens
// a database, broker, container runtime, cloud client or filesystem path: every
// effect belongs to a port, and every ordering decision is made from observed
// port outcomes rather than caller-supplied booleans.

import (
	"context"

	"github.com/optimiware/kodama/internal/fault"
)

// BackupIdentity names exactly one backup set.
type BackupIdentity struct {
	ID string
}

// PreservedState counts durable records a restore carries through.
type PreservedState struct {
	Receipts         int
	Decisions        int
	QueuedExecutions int
}

// BackupEvidence is the read-only preflight evidence for one backup.
type BackupEvidence struct {
	Identity          BackupIdentity
	Scope             BackupScope
	KeysPresent       bool
	Preserved         PreservedState
	MissingWorkspaces []string
}

// ReconcileResult is the evidence of one reconciliation pass.
type ReconcileResult struct {
	Reconciled bool
	Unresolved []string
	Services   []string
	Detail     string
}

// Recovery drives backup and restore over injected ports.
type Recovery struct {
	Backup   BackupPort
	Restore  RestorePort
	Fence    FencePort
	Dispatch DispatchGate
}

// RunBackup performs one backup exactly once. It refuses a cancelled context,
// an invalid scope and missing ports before any port call, and refuses a result
// that carries no identity or an invalid scope.
func (recovery Recovery) RunBackup(ctx context.Context, plan BackupPlan) (BackupResult, error) {
	if err := ctx.Err(); err != nil {
		return BackupResult{}, err
	}
	if recovery.Backup == nil {
		return BackupResult{}, fault.New(fault.InternalFailure, "backup port is not configured")
	}
	if err := plan.Validate(); err != nil {
		return BackupResult{}, err
	}

	result, err := recovery.Backup.Backup(ctx, plan)
	if err != nil {
		return BackupResult{}, err
	}
	if result.Identity.ID == "" {
		return BackupResult{}, fault.New(fault.InternalFailure, "backup result carries no backup identity")
	}
	if err := result.Scope.validate(); err != nil {
		return BackupResult{}, fault.New(fault.InternalFailure, "backup result carries an invalid scope")
	}
	return result, nil
}

// RunRestore drives fence -> restore -> reconcile -> dispatch over observed
// outcomes. Caller-provided RestorePlan booleans are never trusted. Any
// failure, cancellation, unresolved reconciliation evidence or missing backup
// key blocks dispatch and nothing is retried or replayed.
func (recovery Recovery) RunRestore(ctx context.Context, identity BackupIdentity, plan RestorePlan) (RestoreResult, error) {
	if err := ctx.Err(); err != nil {
		return RestoreResult{}, err
	}
	if recovery.Restore == nil {
		return RestoreResult{}, fault.New(fault.InternalFailure, "restore port is not configured")
	}
	if recovery.Fence == nil {
		return RestoreResult{}, fault.New(fault.InternalFailure, "fence port is not configured")
	}
	if recovery.Dispatch == nil {
		return RestoreResult{}, fault.New(fault.InternalFailure, "dispatch gate is not configured")
	}
	if identity.ID == "" {
		return RestoreResult{}, fault.New(fault.InvalidInput, "backup identity is empty")
	}
	if err := plan.Scope.validate(); err != nil {
		return RestoreResult{}, err
	}

	evidence, err := recovery.Restore.Preflight(ctx, identity)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return RestoreResult{}, err
	}
	if evidence.Identity != identity {
		return RestoreResult{}, fault.New(fault.InternalFailure, "preflight evidence names another backup")
	}
	if err := evidence.Scope.validate(); err != nil {
		return RestoreResult{}, fault.New(fault.InternalFailure, "preflight evidence carries an invalid scope")
	}
	if !evidence.KeysPresent {
		return RestoreResult{}, fault.New(fault.NotFound, "backup encryption keys are missing")
	}

	if err := recovery.Fence.Fence(ctx); err != nil {
		return RestoreResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return RestoreResult{}, err
	}

	restorePlan := RestorePlan{
		Scope:          evidence.Scope,
		Fenced:         true,
		Reconciled:     false,
		EnableDispatch: false,
	}
	if _, err := recovery.Restore.Restore(ctx, restorePlan); err != nil {
		return RestoreResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return RestoreResult{}, err
	}

	reconcile, err := recovery.Restore.Reconcile(ctx)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return RestoreResult{}, err
	}
	if !reconcile.Reconciled || len(reconcile.Unresolved) > 0 {
		return RestoreResult{}, fault.New(fault.UncertainExternalEffect, "reconciliation left unresolved external effects; dispatch stays blocked")
	}

	if err := recovery.Dispatch.EnableDispatch(ctx); err != nil {
		return RestoreResult{}, err
	}

	return RestoreResult{
		Fenced:                  true,
		Reconciled:              true,
		Detail:                  reconcile.Detail,
		Preserved:               evidence.Preserved,
		Services:                append([]string(nil), reconcile.Services...),
		UnrecoverableWorkspaces: append([]string(nil), evidence.MissingWorkspaces...),
	}, nil
}
