package gitworker

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// OffboardTenant removes a tenant's service namespace, and with it the worker, its
// claim and its Secret. It is the only operation of this package that can lose a
// volume claim, and it is explicit: reconciliation, worker replacement, run
// cleanup and pipeline completion never reach it.
//
//   - The retention gate must answer yes; a refusal or a failure to answer is
//     ErrRetention and nothing is touched.
//   - Only a namespace that verifies as this tenant's is deleted (labels, tenant
//     annotation, fingerprint) and only under its UID precondition; another
//     object holding the name is ErrForeign and is left alone.
//   - A tenant without a namespace is already offboarded: success.
//
// The StorageClass requires Retain, so the volume behind a deleted claim stays on
// the storage backend until the storage administrator releases it: the data is
// removed in a second, deliberate step outside Kodama.
func (r *Reconciler) OffboardTenant(ctx context.Context, tenant domain.TenantID) error {
	if err := tenant.Validate(); err != nil {
		return invalid("the tenant ID is not valid")
	}
	if r.deps.Retention == nil {
		return wrap(ErrRetention, "no retention policy is configured")
	}
	defer r.lock(tenant)()
	allowed, err := r.deps.Retention.MayDeleteVolume(ctx, tenant)
	if err != nil {
		return wrap(ErrRetention, "retention could not be established")
	}
	if !allowed {
		return wrap(ErrRetention, "the tenant's retention has not elapsed or a hold applies")
	}
	ns := NamespaceOf(tenant)
	live, err := r.deps.Cluster.GetNamespace(ctx, ns)
	switch {
	case errors.Is(err, kubernetes.ErrNotFound):
		r.forget(tenant)
		return nil
	case err != nil:
		return unavailable(err, "reading the namespace")
	}
	c := chain{tenant: tenant, key: TenantKey(tenant), ns: ns}
	if err := c.verifyMeta(live.Metadata, ns, RoleNamespace, false); err != nil {
		if errors.Is(err, ErrUnavailable) { // already terminating: the deletion is under way
			r.forget(tenant)
			return nil
		}
		return err
	}
	err = r.deps.Cluster.DeleteNamespace(ctx, ns, kubernetes.DeleteOptions{UID: live.Metadata.UID})
	switch {
	case err == nil, errors.Is(err, kubernetes.ErrNotFound):
		r.forget(tenant)
		return nil
	case errors.Is(err, kubernetes.ErrConflict):
		return wrap(ErrForeign, "the namespace was replaced by another instance")
	}
	return unavailable(err, "deleting the namespace")
}

func (r *Reconciler) forget(t domain.TenantID) {
	r.mu.Lock()
	delete(r.status, t)
	r.mu.Unlock()
}
