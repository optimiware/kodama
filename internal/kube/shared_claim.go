package kube

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// verifyClaim confirms the run workspace a run_shared attempt mounts is the
// claim execution bound: it exists, carries the run's ownership marks, is not
// being deleted, has exactly the bound instance UID and its resource record is
// not fenced. A claim of the same name but another UID (a replaced or foreign
// claim) is never mounted, and a stopped workspace is never handed out again.
// Only a definite absence of the claim is "not found"; an outage concludes
// nothing.
func (c *core) verifyClaim(ctx context.Context, exp Expectation) error {
	l := exp.Shared
	if l == nil {
		return nil
	}
	if c.run == nil {
		return wrap(ErrPolicy, "the cluster cannot serve a run_shared attempt")
	}
	ns, h := exp.Identity.Namespace, l.Workspace
	pvc, err := c.run.GetPVC(ctx, ns, h.Name)
	switch {
	case errors.Is(err, kubernetes.ErrNotFound):
		return wrap(ErrNotDispatchable, "the run workspace claim does not exist")
	case err != nil:
		return wrapCause(ErrUnavailable, err, "reading the run workspace claim")
	}
	if err := checkOwned(pvc.Metadata, ns, h.Name, ownedWant{
		scope: h.Scope, run: h.RunID, kind: execution.KindWorkspace, key: execution.KeyWorkspace, uid: h.UID,
	}); err != nil {
		return err
	}
	if pvc.Metadata.DeletionTimestamp != "" {
		return wrap(ErrNotDispatchable, "the run workspace claim is being deleted")
	}
	ident := map[string]string{
		AnnTenant: string(h.Scope.TenantID), AnnProject: string(h.Scope.ProjectID), AnnRun: string(h.RunID),
		AnnResourceKind: string(execution.KindWorkspace), AnnResourceKey: execution.KeyWorkspace, AnnResourceName: h.Name,
	}
	rec, err := readRec(ctx, c.cluster, ns, ident, ownerLabels(h.Scope, h.RunID, execution.KindWorkspace), h.Name)
	switch {
	case err != nil:
		return err
	case rec == nil:
		return wrap(ErrForeignWorkload, "the run workspace has no resource record")
	case rec.rec.Fenced:
		return wrap(ErrNotDispatchable, "the run workspace is stopped")
	case rec.rec.UID != "" && rec.rec.UID != h.UID:
		return wrap(ErrForeignWorkload, "the run workspace record binds another claim")
	}
	return nil
}

// sameLaunch reports that two launches agree on everything an attempt's Job is
// built from: the pin, the role, the claim and the network model.
func sameLaunch(a, b execution.SharedLaunch) bool {
	return a.Policy == b.Policy && a.RoleDigest == b.RoleDigest && a.Workspace == b.Workspace && a.Network == b.Network &&
		a.PodSecurity == b.PodSecurity
}
