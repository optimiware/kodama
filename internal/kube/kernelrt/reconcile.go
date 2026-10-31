package kernelrt

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// Inventory is what committed run state still references for one scope: the
// stable identities of its workloads (WorkloadID) and resources (Identity). The
// engine builds it from its retained records; the adapter never decides what a
// run needs.
type Inventory struct {
	Workloads map[string]bool
	Resources map[string]bool
}

// Retained is an orphan the reconciler could not remove. The identity stays
// recorded in the cluster and the next pass retries it.
type Retained struct {
	Kind     string         `json:"kind"`
	Identity string         `json:"identity"`
	Reason   runtime.Reason `json:"reason"`
	// Cause is the adapter's error text, which carries no values.
	Cause string `json:"cause,omitempty"`
}

// ReconcileReport is the outcome of one pass.
type ReconcileReport struct {
	// Removed are the identities confirmed removed.
	Removed []string `json:"removed,omitempty"`
	// Retained are orphans still present or not confirmed absent.
	Retained []Retained `json:"retained,omitempty"`
	// Pruned counts leftover step secrets deleted. Fence records are never pruned.
	Pruned int `json:"pruned,omitempty"`
	// Spared counts unreferenced identities younger than the grace period.
	Spared int `json:"spared,omitempty"`
}

// Reconcile removes the objects of one scope that no committed state
// references. It acts only on identities with a record created by this adapter,
// spares young ones (a create may be racing its own commit), removes the rest
// with the record's own fence, and reports every orphan it could not remove.
// It lists by the scope's label, so another scope's objects are never seen.
func (a *Adapter) Reconcile(ctx context.Context, scope domain.Scope, inv Inventory) (ReconcileReport, error) {
	var rep ReconcileReport
	if err := scope.Validate(); err != nil {
		return rep, err
	}
	t, err := a.targets.Target(ctx, scope)
	if err != nil {
		return rep, fmt.Errorf("kernelrt: no target for the scope: %w", err)
	}
	secrets, err := a.cluster.ListSecrets(ctx, t.Namespace, labelManaged+"="+valueManaged+","+labelScope+"="+scopeHash(scope))
	if err != nil {
		return rep, fmt.Errorf("kernelrt: listing records: %w", err)
	}
	now := a.opts.Clock()
	records := map[string]bool{}
	for _, s := range secrets {
		if s.Metadata.Labels[labelRole] == roleRecord {
			records[s.Metadata.Name] = true
		}
	}
	for _, s := range secrets {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		ann := s.Metadata.Annotations
		if ann[annTenant] != string(scope.TenantID) || ann[annProject] != string(scope.ProjectID) {
			continue
		}
		created, hasAge := parseTime(s.Metadata.CreationTimestamp)
		old := hasAge && now.Sub(created) >= a.opts.OrphanGrace
		switch s.Metadata.Labels[labelRole] {
		case roleMaterial:
			if records[strings.TrimSuffix(s.Metadata.Name, "-sec")+"-rec"] {
				continue
			}
			if !old {
				rep.Spared++
				continue
			}
			err := a.cluster.DeleteSecret(ctx, t.Namespace, s.Metadata.Name, kubernetes.DeleteOptions{UID: s.Metadata.UID})
			if err != nil && !errors.Is(err, kubernetes.ErrNotFound) {
				rep.Retained = append(rep.Retained, Retained{Kind: "secret", Identity: ann[annIdentity], Reason: runtime.ReasonCleanupFailed, Cause: err.Error()})
				continue
			}
			rep.Pruned++
		case roleRecord:
			a.reconcileRecord(ctx, t, scope, s, inv, old, &rep)
		}
	}
	return rep, nil
}

func (a *Adapter) reconcileRecord(ctx context.Context, t Target, scope domain.Scope, s kubernetes.Secret, inv Inventory, old bool, rep *ReconcileReport) {
	kind := objKind(s.Metadata.Labels[labelKind])
	ann := s.Metadata.Annotations
	known := inv.Resources[ann[annIdentity]]
	if kind == kindWorkload {
		known = inv.Workloads[ann[annIdentity]]
	}
	retain := func(err error) {
		rep.Retained = append(rep.Retained, Retained{Kind: string(kind), Identity: ann[annIdentity], Reason: runtime.ReasonCleanupFailed, Cause: err.Error()})
	}
	if known {
		return
	}
	id, err := identFromAnnotations(kind, ann)
	if err != nil || id.scope != scope {
		retain(errors.New("the record has no valid identity for this scope"))
		return
	}
	rec, err := decodeRecord(s)
	if err != nil {
		retain(err)
		return
	}
	if rec.state == recRemoved {
		// Closure evidence is durable: the record of a removed identity and its
		// highest fence are never deleted, because they are what rejects a delayed
		// create from an older writer at any later time.
		return
	}
	if !old {
		rep.Spared++
		return
	}
	var removed bool
	switch kind {
	case kindWorkload:
		b := runtime.RuntimeBinding{Scope: id.scope, RunID: id.run, StepKey: id.step, Attempt: id.attempt, WorkloadID: id.id, SpecDigest: id.digest, Backend: id.backend, Fence: rec.fence}
		var obs runtime.WorkloadObservation
		obs, err = a.removeWorkload(ctx, t.Namespace, id, b)
		removed = err == nil && obs.Phase == runtime.PhaseAbsent
	case kindWorkspace, kindService:
		h := a.handleOf(id, rec.fence)
		var obs runtime.ResourceObservation
		obs, err = a.removeResource(ctx, t.Namespace, id, h)
		removed = err == nil && obs.State == runtime.ResourceRemoved
	default:
		err = fmt.Errorf("kernelrt: record kind %q", kind)
	}
	switch {
	case err != nil:
		retain(err)
	case removed:
		rep.Removed = append(rep.Removed, id.id)
	default:
		retain(errors.New("removal is not yet confirmed"))
	}
}
