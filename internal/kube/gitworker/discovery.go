package gitworker

import (
	"context"

	"github.com/optimiware/kodama/internal/domain"
)

// Endpoint is where the control plane reaches one tenant's worker: the base URL of
// the worker's Service, to which the worker route family (gitv1.WorkerBasePath) is
// appended. It is a cache hint: callers tolerate a replaced worker.
type Endpoint struct {
	BaseURL string
}

// Endpoint resolves the tenant's worker (the worker discovery port of
// contracts/git/README.md). It answers only for a worker the last reconciliation
// found ready, and only while that observation is fresh; a tenant that has no
// worker, a worker being replaced, a blocked tenant and a stale observation are
// all ErrWorkerUnavailable, which callers treat as retryable. The endpoint is
// computed from the tenant's own namespace; it can never be another tenant's.
func (r *Reconciler) Endpoint(_ context.Context, tenant domain.TenantID) (Endpoint, error) {
	if err := tenant.Validate(); err != nil {
		return Endpoint{}, wrap(ErrWorkerUnavailable, "the tenant is not valid")
	}
	r.mu.Lock()
	st, ok := r.status[tenant]
	r.mu.Unlock()
	switch {
	case !ok:
		return Endpoint{}, wrap(ErrWorkerUnavailable, "the tenant has no reconciled worker")
	case !st.Ready:
		return Endpoint{}, wrap(ErrWorkerUnavailable, "the tenant's worker is not ready")
	case st.Tenant != tenant || st.Namespace != NamespaceOf(tenant):
		return Endpoint{}, wrap(ErrWorkerUnavailable, "the recorded worker is not the tenant's")
	case r.opts.Clock().Sub(st.ObservedAt) > r.opts.StatusTTL:
		return Endpoint{}, wrap(ErrWorkerUnavailable, "the tenant's worker was not observed recently")
	}
	return Endpoint{BaseURL: st.Endpoint}, nil
}
