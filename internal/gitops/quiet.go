package gitops

import (
	"context"
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
)

// requireQuiet refuses (ErrTenantClosed, retryable) while the tenant has an
// active recovery or its Git access is closed for a restore or an offboarding,
// and fails closed when either cannot be read or was not configured. On success
// it returns the ID of the tenant's latest recovery ("" when it never had one):
// a caller whose result must not span a recovery compares it again at the end
// (Backups does before it publishes).
func requireQuiet(ctx context.Context, fence AccessFence, recoveries RecoveryStore, tenant domain.TenantID) (string, error) {
	if fence == nil || recoveries == nil {
		return "", fmt.Errorf("%w: the access fence and the recovery store are required", ErrNotConfigured)
	}
	rec, ok, err := recoveries.Current(ctx, tenant)
	if err != nil {
		return "", fmt.Errorf("%w: reading the recovery: %v", ErrUnavailable, err)
	}
	if ok && rec.Status == RecoveryActive {
		return "", fmt.Errorf("%w: recovery %s is active", ErrTenantClosed, rec.ID)
	}
	for _, reason := range []FenceReason{FenceRestore, FenceOffboarding} {
		closed, err := fence.ClosedFor(ctx, tenant, reason)
		if err != nil {
			return "", fmt.Errorf("%w: reading the access fence: %v", ErrUnavailable, err)
		}
		if closed {
			return "", fmt.Errorf("%w: Git access is closed for %s", ErrTenantClosed, reason)
		}
	}
	if !ok {
		return "", nil
	}
	return rec.ID, nil
}
