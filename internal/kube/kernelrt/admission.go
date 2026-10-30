package kernelrt

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// admit decides, before any effect, whether this cluster can satisfy a request
// for scope: the required capabilities are declared, the scope has a target,
// the namespace is live and holds the service account, and, when network
// isolation is required, the namespace has a default-deny ingress policy. Every
// refusal wraps runtime.ErrNotReached and runtime.ErrCapability: nothing was
// created, and retrying without a cluster change cannot help.
func (c *core) admit(ctx context.Context, scope domain.Scope, required runtime.CapabilitySet) (Target, error) {
	if err := c.capabilities().Satisfies(required); err != nil {
		return Target{}, notReached(err)
	}
	t, err := c.targets.Target(ctx, scope)
	if err != nil {
		return Target{}, notReached(fmt.Errorf("%w: no target for the scope: %w", runtime.ErrCapability, err))
	}
	if !nameRE.MatchString(t.Namespace) || !nameRE.MatchString(t.ServiceAccount) {
		return Target{}, notReached(fmt.Errorf("%w: the scope's target is malformed", runtime.ErrCapability))
	}
	ns, err := c.cluster.GetNamespace(ctx, t.Namespace)
	switch {
	case errors.Is(err, kubernetes.ErrNotFound):
		return Target{}, unsupported("namespace %s does not exist", t.Namespace)
	case err != nil:
		return Target{}, notReached(fmt.Errorf("reading the namespace: %w", err))
	case ns.Status.Phase == "Terminating" || ns.Metadata.DeletionTimestamp != "":
		return Target{}, unsupported("namespace %s is terminating", t.Namespace)
	}
	if _, err := c.cluster.GetServiceAccount(ctx, t.Namespace, t.ServiceAccount); err != nil {
		if errors.Is(err, kubernetes.ErrNotFound) {
			return Target{}, unsupported("service account %s does not exist", t.ServiceAccount)
		}
		return Target{}, notReached(fmt.Errorf("reading the service account: %w", err))
	}
	if required.Has(runtime.CapNetworkIsolation) {
		if err := c.requireDefaultDeny(ctx, t.Namespace); err != nil {
			return Target{}, err
		}
	}
	return t, nil
}

// requireDefaultDeny verifies that some policy selects every Pod of the
// namespace, governs ingress and egress, and allows no ingress. Egress
// allowances are the operator's; this check does not judge them.
func (c *core) requireDefaultDeny(ctx context.Context, ns string) error {
	policies, err := c.cluster.ListNetworkPolicies(ctx, ns)
	if err != nil {
		return notReached(fmt.Errorf("listing network policies: %w", err))
	}
	for _, p := range policies {
		s := p.Spec
		if s.PodSelector.Empty() && slices.Contains(s.PolicyTypes, "Ingress") && slices.Contains(s.PolicyTypes, "Egress") && len(s.Ingress) == 0 {
			return nil
		}
	}
	return unsupported("namespace %s has no default-deny network policy", ns)
}

// checkStorage verifies the workspace storage class exists and is usable for a
// run claim, and reports whether it binds only on first use.
func (c *core) storageClass(ctx context.Context) (kubernetes.StorageClass, error) {
	sc, err := c.cluster.GetStorageClass(ctx, c.cfg.StorageClass)
	switch {
	case errors.Is(err, kubernetes.ErrNotFound):
		return sc, unsupported("storage class %s does not exist", c.cfg.StorageClass)
	case err != nil:
		return sc, notReached(fmt.Errorf("reading the storage class: %w", err))
	}
	return sc, nil
}
