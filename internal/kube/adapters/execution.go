// Package adapters bridges the Kubernetes executor to the execution and
// runtimeauth modules without changing either. The bridges are thin: they read
// committed data through each module's own scoped methods, and perform no
// Kubernetes call and no write.
package adapters

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
)

// Attempts implements kube.Attempts over execution's store. Every read is scoped
// by tenant and project through the store; a foreign or unknown attempt is
// kube.ErrNotFound.
type Attempts struct{ store execution.Store }

var _ kube.Attempts = (*Attempts)(nil)

// NewAttempts requires the execution store.
func NewAttempts(store execution.Store) (*Attempts, error) {
	if store == nil {
		return nil, errors.New("kube/adapters: an execution store is required")
	}
	return &Attempts{store: store}, nil
}

// Attempt implements kube.Attempts.
func (a *Attempts) Attempt(ctx context.Context, scope domain.Scope, id domain.AttemptID) (kube.AttemptRecord, error) {
	if scope.Validate() != nil || id.Validate() != nil {
		return kube.AttemptRecord{}, kube.ErrNotFound
	}
	run, err := a.store.RunOf(ctx, scope, id)
	if err != nil {
		return kube.AttemptRecord{}, mapNotFound(err)
	}
	agg, err := a.store.Get(ctx, scope, run)
	if err != nil {
		return kube.AttemptRecord{}, mapNotFound(err)
	}
	at := agg.Attempt(id)
	if at == nil {
		return kube.AttemptRecord{}, kube.ErrNotFound
	}
	return record(agg, at), nil
}

func record(agg *execution.Aggregate, at *execution.Attempt) kube.AttemptRecord {
	c := agg.Control(at.ID)
	return kube.AttemptRecord{
		Shared: sharedLaunch(agg, at.StepID),
		Scope:  at.Scope, RunID: at.RunID, StepID: at.StepID, AttemptID: at.ID, ActionKey: at.ActionKey,
		Status: at.Status, Live: at.Status == execution.AttemptRunning && c.Live(),
		WorkspaceID: at.WorkspaceID, JobName: at.WorkloadName, SpecFingerprint: at.SpecFingerprint, ContextDigest: at.ContextDigest, Context: at.Context, Access: at.Access,
		ObservedJobUID: at.WorkloadUID, StartedAt: at.StartedAt, DispatchConfirmed: c.DispatchConfirmedAt != nil,
		TerminationRequested: c.TerminationRequestedAt != nil, TerminationConfirmed: c.TerminationConfirmedAt != nil,
		Settled: c.Settled(), SettledAt: c.SettledAt, Launch: c.Launch,
	}
}

// Unsettled implements kube.Attempts: the unsettled attempts of the tenant's
// nonterminal runs. The continuation key is "<project>|<run>".
func (a *Attempts) Unsettled(ctx context.Context, tenant domain.TenantID, after string, limit int) ([]kube.AttemptRecord, string, error) {
	var ref execution.RunRef
	if after != "" {
		project, run, ok := strings.Cut(after, "|")
		if !ok {
			return nil, "", errors.New("kube/adapters: malformed continuation")
		}
		ref = execution.RunRef{Scope: domain.Scope{TenantID: tenant, ProjectID: domain.ProjectID(project)}, ID: domain.RunID(run)}
	}
	refs, err := a.store.Active(ctx, tenant, ref, limit)
	if err != nil {
		return nil, "", err
	}
	var out []kube.AttemptRecord
	for _, r := range refs {
		agg, err := a.store.Get(ctx, r.Scope, r.ID)
		if err != nil {
			if errors.Is(err, execution.ErrNotFound) {
				continue
			}
			return nil, "", err
		}
		for _, at := range agg.Unsettled() {
			out = append(out, record(agg, at))
		}
	}
	next := ""
	if len(refs) == limit && limit > 0 {
		last := refs[len(refs)-1]
		next = string(last.Scope.ProjectID) + "|" + string(last.ID)
	}
	return out, next, nil
}

func mapNotFound(err error) error {
	if errors.Is(err, execution.ErrNotFound) {
		return kube.ErrNotFound
	}
	return err
}

// sharedLaunch derives the committed run_shared launch of a step from the run's
// immutable pins and its workspace's bound identity: the pinned step role, the
// bound claim, the pinned network model and the step's profile digests. Unlike
// execution's launch derivation it does not refuse a workspace that is stopping or
// removed: the expectation of an attempt's Job (observation, termination,
// removal) must stay rebuildable for as long as the Job exists. It is nil for an
// attempt_private run and for a workspace that was never bound.
func sharedLaunch(agg *execution.Aggregate, stepID string) *execution.SharedLaunch {
	sh := agg.Run.Shared
	if sh == nil {
		return nil
	}
	ws := agg.Resource(execution.KeyWorkspace)
	if ws == nil || !ws.Bound() {
		return nil
	}
	role, ok := sh.PolicyBody.Roles[execution.RoleStep]
	if !ok {
		return nil
	}
	digest, err := sh.PolicyBody.RoleDigest(execution.RoleStep)
	if err != nil {
		return nil
	}
	egress := append([]string{}, sh.Egress[stepID]...)
	digests := map[string]string{}
	for _, name := range egress {
		if d, ok := sh.EgressDigests[name]; ok {
			digests[name] = d
		}
	}
	// slices.Clone keeps nil as nil: the copy must hash to the pinned digest.
	role.CapabilitiesAdd = slices.Clone(role.CapabilitiesAdd)
	role.Devices = slices.Clone(role.Devices)
	return &execution.SharedLaunch{
		Policy: sh.Policy, Role: role, RoleDigest: digest, PodSecurity: sh.PolicyBody.PodSecurity,
		Workspace: ws.Handle(), Network: sh.PolicyBody.Network, Egress: egress, EgressDigests: digests,
	}
}
