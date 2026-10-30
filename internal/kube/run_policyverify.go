package kube

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

var _ OwnedPolicyVerifier = (*RunResources)(nil)

// Preflight returns the namespace preflight the adapter itself uses, wired to
// verify owned run policies against their records (CheckRun).
func (r *RunResources) Preflight() *Preflight { return r.pre }

// VerifyOwnedPolicy decides whether a NetworkPolicy that carries the run-resource
// owner label is exactly one of the policies the committed spec of its resource
// renders: the resource's record must exist and hold a spec whose fingerprint is
// the annotated one, the policy's name must be one the pinned policy body and the
// spec render (the network's policies, or a service's ingress policy), it must
// carry the resource's instance anchor, the UID recorded for it when it was
// created and equal the rendering field by field. Anything else, a policy that
// merely copies our labels and annotations included, is refused (ErrPreflight):
// network policies are additive, so an unrecognized one would silently widen the
// boundary. An unreadable record or policy is ErrUnavailable.
func (r *RunResources) VerifyOwnedPolicy(ctx context.Context, tenant domain.TenantID, p kubernetes.NetworkPolicy) error {
	ann := p.Metadata.Annotations
	no := func(why string) error { return ownedDeny(why) }
	kind := execution.ResourceKind(p.Metadata.Labels[LabelResourceKind])
	if ann[AnnTenant] != string(tenant) || (kind != execution.KindNetwork && kind != execution.KindService) {
		return no("it does not name a network or service resource of the tenant")
	}
	h := execution.ResourceHandle{
		Scope: domain.Scope{TenantID: tenant, ProjectID: domain.ProjectID(ann[AnnProject])}, RunID: domain.RunID(ann[AnnRun]),
		Kind: kind, Key: ann[AnnResourceKey], Name: ann[AnnResourceName],
	}
	if h.Validate() != nil {
		return no("it does not name a resource by its deterministic name")
	}
	tgt, err := r.cfg.Tenant(tenant)
	if err != nil {
		return err
	}
	a, err := readRec(ctx, r.cl, tgt.Namespace, identOf(h), ownerLabels(h.Scope, h.RunID, h.Kind), h.Name)
	switch {
	case err != nil && errors.Is(err, ErrForeignWorkload):
		return no("its resource record is not the resource's")
	case err != nil:
		return err
	case a == nil || len(a.secret.Data[KeyResourceSpec]) == 0:
		return no("its resource has no record")
	}
	spec, err := a.spec()
	if err != nil {
		return no("its resource record is unreadable")
	}
	x, err := r.render(ctx, spec)
	switch {
	case errors.Is(err, ErrPolicyUnavailable), errors.Is(err, ErrUnavailable):
		return err
	case err != nil:
		return no("its resource cannot be rendered under the pinned policy")
	}
	if x.fp != a.secret.Metadata.Annotations[AnnFingerprint] || x.spec.Handle("") != execution.ResourceHandle(h) {
		return no("its resource record does not match its spec")
	}
	var want []kubernetes.NetworkPolicy
	anchor := ""
	switch kind {
	case execution.KindNetwork:
		anchor = networkAnchor(a)
		if want, err = x.networkPolicies(anchor, storedResolver(a.secret.Data)); err != nil {
			return no("its resource cannot be rendered")
		}
	default:
		if a.rec.UID == "" {
			return no("its service is not bound")
		}
		anchor = a.rec.UID
		want = []kubernetes.NetworkPolicy{x.ingressPolicy(x.body.Network.Isolation)}
	}
	for _, w := range want {
		if w.Metadata.Name != p.Metadata.Name {
			continue
		}
		if err := checkOwned(p.Metadata, tgt.Namespace, w.Metadata.Name, x.want("")); err != nil {
			return no("its ownership marks are not the resource's")
		}
		if ann[AnnResourceUID] != anchor {
			return no("it belongs to another instance of its resource")
		}
		if rec := a.rec.Companions[w.Metadata.Name]; rec != "" && rec != p.Metadata.UID {
			return no("it is not the instance its resource created")
		}
		if err := verifyDoc("network policy", w, p.Raw, benignNone); err != nil {
			return no("it differs from the policy its resource renders")
		}
		return nil
	}
	return no("it is not a policy its resource renders")
}
