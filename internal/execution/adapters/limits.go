package adapters

import (
	"context"
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// StaticLimits is an explicit, validated execution policy: the same limits for
// every scope unless overridden per tenant. Merged project settings do not yet
// define typed concurrency or startup-budget fields, so the composition root
// (T-027) injects the values; nothing is read from the environment here and an
// unconfigured scope is denied.
type StaticLimits struct {
	Default execution.Limit
	// PerTenant overrides Default for specific tenants.
	PerTenant map[domain.TenantID]execution.Limit
}

var _ execution.Limits = StaticLimits{}

// Validate checks every configured limit.
func (l StaticLimits) Validate() error {
	if err := l.Default.Validate(); err != nil {
		return fmt.Errorf("default limits: %w", err)
	}
	for t, lim := range l.PerTenant {
		if err := lim.Validate(); err != nil {
			return fmt.Errorf("limits of tenant %s: %w", t, err)
		}
	}
	return nil
}

// Limits implements execution.Limits.
func (l StaticLimits) Limits(_ context.Context, scope domain.Scope) (execution.Limit, error) {
	lim := l.Default
	if o, ok := l.PerTenant[scope.TenantID]; ok {
		lim = o
	}
	if err := lim.Validate(); err != nil {
		return execution.Limit{}, execution.Unavailablef(err, "execution limits are not configured")
	}
	return lim, nil
}
