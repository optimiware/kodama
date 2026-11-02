package gitops

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Offboarding stages, in order.
const (
	StageFenced    = "fenced"    // Git access closed, grants revoked, pushes settled
	StageSettling  = "settling"  // pushes still in flight or grants still active: try again
	StageRetaining = "retaining" // settled; waiting for the retention period or a hold
	StageDeleted   = "deleted"   // the storage provider deleted the volume
)

// OffboardingResult is where an offboarding stands.
type OffboardingResult struct {
	Stage       string
	RetainUntil time.Time
	Hold        bool
	// InFlight counts push entries not settled yet; Unknown the entries left for
	// an operator. Unknown entries do not delay deletion (the data is being
	// deleted), they are recorded in the audit event.
	InFlight, Unknown int
	ActiveGrants      int
}

// OffboardingConfig holds the collaborators; all but Clock are required.
type OffboardingConfig struct {
	Lifecycle Lifecycle
	Catalog   Catalog
	Fence     AccessFence
	Grants    Grants
	Pushes    Pushes
	Storage   StorageOffboarder
	Audit     Auditor
	Clock     func() time.Time
}

// Offboarding is the retention-gated offboarding of a tenant's Git storage
// (GR-14): revoke grants, settle pushes, and delete the volume only after
// retention. Git storage is never deleted by run cleanup; this is the only path.
type Offboarding struct{ c OffboardingConfig }

// NewOffboarding validates the configuration.
func NewOffboarding(c OffboardingConfig) (*Offboarding, error) {
	if c.Lifecycle == nil || c.Catalog == nil || c.Fence == nil || c.Grants == nil || c.Pushes == nil || c.Storage == nil || c.Audit == nil {
		return nil, fmt.Errorf("%w: a lifecycle, catalog, fence, grants, pushes, storage and audit are required", ErrNotConfigured)
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	return &Offboarding{c: c}, nil
}

func (o *Offboarding) now() time.Time { return o.c.Clock().UTC() }

// settle reconciles every repository's pushes and counts what remains.
func (o *Offboarding) settle(ctx context.Context, tenant domain.TenantID) (inFlight, unknown int, err error) {
	var errs []error
	err = repositories(ctx, o.c.Catalog, tenant, func(r Repository) error {
		if rerr := o.c.Pushes.Reconcile(ctx, tenant, r.ID); rerr != nil {
			errs = append(errs, fmt.Errorf("repository %s: %w", r.ID, rerr))
		}
		st, serr := o.c.Pushes.Settlement(ctx, tenant, r.ID)
		if serr != nil {
			errs = append(errs, fmt.Errorf("repository %s: %w", r.ID, serr))
			inFlight++ // unknown settlement is not settled
			return nil
		}
		inFlight += st.InFlight
		unknown += st.Unknown
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}
	return inFlight, unknown, errors.Join(errs...)
}

// Offboard advances the tenant's offboarding as far as it can and reports the
// stage; it is idempotent and meant to be repeated (by the lifecycle's
// reconciler) until StageDeleted. It never deletes before every condition holds:
//
//  1. the lifecycle says the tenant is offboarded (ErrNotOffboarding otherwise);
//  2. Git access is closed (a fence that no restore reopens) and every grant is
//     revoked: no new push can start, and a push in flight is settled
//     by the push records' own rules;
//  3. no push entry is in flight (unknown entries are counted and audited);
//  4. the retention period elapsed and no hold applies;
//  5. the deletion is recorded in the audit trail; if recording fails nothing is
//     deleted, and only then does the storage provider delete.
//
// The storage provider's own retention gate (MayDeleteVolume, implemented by
// this type) is consulted again by it.
func (o *Offboarding) Offboard(ctx context.Context, tenant domain.TenantID) (OffboardingResult, error) {
	st, err := o.state(ctx, tenant)
	if err != nil {
		return OffboardingResult{}, err
	}
	res := OffboardingResult{RetainUntil: st.RetainUntil, Hold: st.Hold}
	if err := o.c.Fence.Close(ctx, tenant, FenceOffboarding); err != nil {
		return res, fmt.Errorf("%w: closing Git access: %v", ErrUnavailable, err)
	}
	if err := o.c.Grants.RevokeAll(ctx, tenant); err != nil {
		return res, fmt.Errorf("%w: revoking grants: %v", ErrUnavailable, err)
	}
	active, err := o.c.Grants.Active(ctx, tenant)
	if err != nil {
		return res, fmt.Errorf("%w: counting grants: %v", ErrUnavailable, err)
	}
	res.ActiveGrants = active
	res.InFlight, res.Unknown, err = o.settle(ctx, tenant)
	if err != nil {
		res.Stage = StageSettling
		return res, err
	}
	if active > 0 || res.InFlight > 0 {
		res.Stage = StageSettling
		return res, nil
	}
	if !o.retentionElapsed(st) {
		res.Stage = StageRetaining
		return res, nil
	}
	if err := o.c.Audit.Record(ctx, AuditEvent{
		ID: operationID("aud_offb", string(tenant), "delete"), Tenant: tenant, Action: "git.offboarding.delete_storage",
		Metadata: map[string]any{"retainUntil": st.RetainUntil.UTC().Format(time.RFC3339), "unknownEntries": res.Unknown},
	}); err != nil {
		return res, fmt.Errorf("%w: recording the deletion: %v", ErrUnavailable, err)
	}
	if err := o.c.Storage.OffboardTenant(ctx, tenant); err != nil {
		return res, fmt.Errorf("deleting the tenant's Git storage: %w", err)
	}
	res.Stage = StageDeleted
	return res, nil
}

func (o *Offboarding) state(ctx context.Context, tenant domain.TenantID) (OffboardingState, error) {
	if err := tenant.Validate(); err != nil {
		return OffboardingState{}, err
	}
	st, err := o.c.Lifecycle.Offboarding(ctx, tenant)
	if err != nil {
		return OffboardingState{}, fmt.Errorf("%w: the tenant's offboarding state: %v", ErrUnavailable, err)
	}
	if !st.Requested {
		return OffboardingState{}, ErrNotOffboarding
	}
	return st, nil
}

func (o *Offboarding) retentionElapsed(st OffboardingState) bool {
	return !st.Hold && !st.RetainUntil.IsZero() && !o.now().Before(st.RetainUntil)
}

// MayDeleteVolume implements T-129's gitworker.Retention: true only when the
// tenant is offboarded, retention elapsed with no hold, every grant is revoked
// and no push entry is in flight. Any error is an answer of no. It changes
// nothing.
func (o *Offboarding) MayDeleteVolume(ctx context.Context, tenant domain.TenantID) (bool, error) {
	st, err := o.state(ctx, tenant)
	if err != nil {
		return false, err
	}
	if !o.retentionElapsed(st) {
		return false, nil
	}
	closed, err := o.c.Fence.ClosedFor(ctx, tenant, FenceOffboarding)
	if err != nil || !closed {
		return false, err
	}
	active, err := o.c.Grants.Active(ctx, tenant)
	if err != nil || active > 0 {
		return false, err
	}
	inFlight := 0
	err = repositories(ctx, o.c.Catalog, tenant, func(r Repository) error {
		s, err := o.c.Pushes.Settlement(ctx, tenant, r.ID)
		inFlight += s.InFlight
		return err
	})
	if err != nil {
		return false, err
	}
	return inFlight == 0, nil
}
