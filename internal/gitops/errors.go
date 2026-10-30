package gitops

import "errors"

var (
	// ErrNotConfigured: a required collaborator was not injected (fail closed).
	ErrNotConfigured = errors.New("gitops: required capability is not configured")
	// ErrUnavailable marks a dependency outage; callers retry.
	ErrUnavailable = errors.New("gitops: unavailable")
	// ErrRetention refuses a release or deletion whose retention has not elapsed
	// (or could not be established).
	ErrRetention = errors.New("gitops: retention does not allow this yet")
	// ErrEmptyRepository is what Objects.CreateBundle returns for a repository
	// without refs: there is nothing to bundle.
	ErrEmptyRepository = errors.New("gitops: the repository has no refs")
	// ErrBackupNotFound is what a BackupStore returns for an absent key.
	ErrBackupNotFound = errors.New("gitops: backup object not found")
	// ErrBackupCorrupt marks a manifest or bundle that does not verify.
	ErrBackupCorrupt = errors.New("gitops: backup does not verify")
	// ErrNotRestoring refuses Reconcile for a tenant whose access is not closed
	// for a restore: reconciliation reopens access and lowers journal heads, so it
	// only runs inside the restore procedure.
	ErrNotRestoring = errors.New("gitops: the tenant is not being restored")
	// ErrNotOffboarding refuses offboarding work for a tenant the lifecycle has
	// not offboarded.
	ErrNotOffboarding = errors.New("gitops: the tenant is not offboarded")
	// ErrBackupInProgress: another backup of the tenant holds the lease.
	ErrBackupInProgress = errors.New("gitops: a backup of the tenant is in progress")
	// ErrLeaseLost: the backup run was abandoned by another run.
	ErrLeaseLost = errors.New("gitops: the backup run's lease was lost")
	// ErrRecoveryActive refuses a second, different recovery while one is active.
	ErrRecoveryActive = errors.New("gitops: another recovery of the tenant is active")
	// ErrNotDiscrepancy refuses to resolve a difference the volume does not show.
	ErrNotDiscrepancy = errors.New("gitops: the volume does not show that discrepancy")
	// ErrTenantClosed is the retryable refusal of an operation that must not run
	// while the tenant has an active recovery or its Git access is closed (restore
	// or offboarding): a backup would bundle a half-restored volume and publish it
	// as complete, a prune could delete a branch the recovery point expects.
	ErrTenantClosed = errors.New("gitops: the tenant has an active recovery or closed Git access; try again later")
	// ErrPruneBusy: another pruner holds a live reservation of the ref.
	ErrPruneBusy = errors.New("gitops: another pruner holds a live reservation of the ref")
)
