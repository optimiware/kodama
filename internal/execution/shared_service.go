package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

// SharedConfig is the explicit opt-in of the run_shared model. Admission of the
// new format is closed unless an installation composes BOTH ports here and the
// pinned policy opens it; there is no default, no permissive constructor and no
// production fallback. The selected backend must offer every required resource
// capability, including run_network_isolation and egress_default_deny even for a
// service-free run. Fakes are injected by tests only.
type SharedConfig struct {
	// Resources is the companion port of the SAME backend as Dependencies.Runtime.
	Resources RunResources
	// Policies supplies the policy new admissions pin.
	Policies Policies
}

func (c *SharedConfig) validate(rt Runtime) error {
	if c == nil {
		return nil
	}
	missing := func(name string) error {
		return fmt.Errorf("%w: %s is required for run_shared", ErrNotConfigured, name)
	}
	switch {
	case c.Resources == nil:
		return missing("the run resource port")
	case c.Policies == nil:
		return missing("the execution policy source")
	case rt == nil:
		return missing("the attempt runtime")
	}
	return nil
}

// SharedEnv is implemented by a transaction environment that can reserve the
// run-level capacity of a run_shared run. It is separate from Env so existing
// Env implementations are unaffected; a store that cannot reserve refuses to
// start a run_shared run (fail closed).
type SharedEnv interface {
	// ReserveRunCapacity records the run's reservation once (idempotent) and
	// reports ErrQuotaExceeded when the tenant or project quantities or the
	// concurrency entitlement would be exceeded. The reservation is the run's
	// quantities AND one tenant/project active-attempt slot, taken atomically
	// under the quota scope locks and shared by every successive attempt of the
	// run without a second charge. It requires the quota scope locks.
	ReserveRunCapacity(ctx context.Context, run *Run, units CapacityUnits, lim Limit) error
	// ReleaseRunCapacity releases it exactly once (idempotent).
	ReleaseRunCapacity(ctx context.Context, run *Run) error
}

// SharedStore is implemented by a store that persists run_shared state and can
// list runs whose resources still need cleanup.
type SharedStore interface {
	// UnfinishedResources lists TERMINAL runs that still have a resource not
	// verified removed (nonterminal runs are reconciled with the active scan),
	// oldest first, after the reference.
	UnfinishedResources(ctx context.Context, tenant domain.TenantID, after RunRef, limit int) ([]RunRef, error)
}

func sharedEnvOf(env Env) (SharedEnv, error) {
	se, ok := env.(SharedEnv)
	if !ok {
		return nil, fmt.Errorf("%w: the store cannot reserve run_shared capacity", ErrNotConfigured)
	}
	return se, nil
}

// ---- admission ----

// planShared derives the pins of a run_shared run at admission. It runs outside
// any transaction. It refuses (never degrades) when the selected runtime and
// resource port do not offer every required capability, when the policy is
// unavailable or closed, or a ceiling is exceeded. A source-free run (an
// internal deployment cleanup run) has no repository, no checkout credential and
// no preparation: it needs no source information.
func (s *Service) planShared(ctx context.Context, scope domain.Scope, runID domain.RunID, doc pipeline.Document, sourceBacked bool, src SourceInfo, snap Snapshot) (*RunShared, *RuntimeBinding, error) {
	if s.d.Shared == nil || s.d.Runtime == nil {
		return nil, nil, Refusedf("the pipeline definition version is not supported for execution")
	}
	pol, err := s.d.Shared.Policies.Current(ctx)
	if err != nil {
		return nil, nil, Refusedf("%s", ReasonPolicyUnavailable)
	}
	if err := pol.Validate(); err != nil || !pol.RunSharedOpen() {
		return nil, nil, Refusedf("%s: run_shared admission is not enabled", ReasonPolicyUnavailable)
	}
	pin, err := pol.Pin()
	if err != nil {
		return nil, nil, Refusedf("%s", ReasonPolicyUnavailable)
	}
	rt := s.d.Runtime.Capabilities()
	rc := s.d.Shared.Resources.Capabilities(ctx)
	if rc.Backend == "" || rc.Backend != rt.Backend {
		return nil, nil, Refusedf("%s: the resource port is not the runtime's backend", ReasonCapabilityUnsupported)
	}
	required := RunGuarantees(doc, s.requirements)
	if missing := rt.Guarantees.Missing(required); missing != 0 {
		return nil, nil, Refusedf("%s: the %s runtime does not provide %s", ReasonCapabilityUnsupported, rt.Backend, missing)
	}
	needs := RequiredResourceCapabilities(doc, sourceBacked, pol)
	if missing := rc.Set.Missing(needs); missing != 0 {
		return nil, nil, Refusedf("%s: the %s backend lacks %s", ReasonCapabilityUnsupported, rt.Backend, missing)
	}
	shared, err := planSharedPins(sharedInput{
		Scope: scope, RunID: runID, Doc: doc, Policy: pol, PolicyPin: pin, Backend: rt.Backend, Requires: required,
		ResourceReq: needs, SourceBacked: sourceBacked, RepositoryURL: src.RepositoryURL, Snapshot: snap,
	})
	if err != nil {
		return nil, nil, err
	}
	if err := shared.Verify(); err != nil {
		return nil, nil, Refusedf("%s", ReasonPolicyUnavailable)
	}
	return shared, &RuntimeBinding{Backend: rt.Backend, Requires: shared.Requires, ResourceRequires: needs}, nil
}

// checkVersion is the execution guard on the pipeline document version. The
// original executor guard is checkDefinitionVersion and is unchanged: it admits
// exactly kodama/v1alpha1. kodama/v1alpha2 passes only when the installation
// explicitly composed SharedConfig, for an ordinary work run and for an internal
// deployment cleanup run alike; an unconfigured composition refuses both.
func (s *Service) checkVersion(version string, allowShared bool) error {
	if allowShared && s.d.Shared != nil && version == sharedAPIVersion {
		return nil
	}
	return checkDefinitionVersion(version)
}

const sharedAPIVersion = "kodama/v1alpha2"

// ownsModel is the pre-mutation guard of every entry that could change a
// persisted run_shared run (reconciliation, results, begin, receipts, attempt
// observations): the run is touched only by a controller that implements its
// pinned run model. It reads no peer and so runs inside a transaction.
func (s *Service) ownsModel(run *Run) error {
	if run.Shared == nil {
		return nil
	}
	if s.d.Shared == nil {
		return fmt.Errorf("%w: run_shared is not configured", ErrNotConfigured)
	}
	if err := run.Shared.Supported(); err != nil {
		return fmt.Errorf("%w: %v", ErrNotConfigured, err)
	}
	return nil
}

// sharedCleanup returns the runtime and companion port that may STOP, OBSERVE
// and REMOVE the committed, owned resources of a run_shared run. This narrow
// authority needs only that this controller implements the run model and that
// the companion port is the run's pinned backend: it deliberately does not
// need the current or even the retained policy, a pin that verifies or any
// capability, so a policy loss, a capability loss or a corrupt pin never strands
// a resource. A run of an unsupported model or of another backend is untouched.
func (s *Service) sharedCleanup(ctx context.Context, run *Run) (Runtime, RunResources, error) {
	if run.Shared == nil {
		return nil, nil, invalidf("not a run_shared run")
	}
	if s.d.Shared == nil {
		return nil, nil, fmt.Errorf("%w: run_shared is not configured", ErrNotConfigured)
	}
	if err := run.Shared.Supported(); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrNotConfigured, err)
	}
	if s.d.Runtime == nil || run.Snapshot.Runtime == nil {
		return nil, nil, fmt.Errorf("%w: the run has no runtime binding", ErrNotConfigured)
	}
	if s.d.Runtime.Capabilities().Backend != run.Shared.Backend {
		return nil, nil, fmt.Errorf("%w: the run is bound to the %s backend", ErrNotConfigured, run.Shared.Backend)
	}
	caps := s.d.Shared.Resources.Capabilities(ctx)
	if caps.Backend != run.Shared.Backend {
		return nil, nil, fmt.Errorf("%w: the run is bound to the %s backend", ErrNotConfigured, run.Shared.Backend)
	}
	return s.d.Runtime, s.d.Shared.Resources, nil
}

// sharedLaunch returns the ports a run_shared run may LAUNCH, ADOPT or REDRIVE
// with: the cleanup authority plus verified pins (the retained policy body
// matches its digest), the runtime still providing the pinned guarantees and
// the companion port still offering every pinned capability. The reason is the
// safe code of the first failed check.
func (s *Service) sharedLaunch(ctx context.Context, run *Run) (Runtime, RunResources, ResourceReason, error) {
	rt, res, err := s.sharedCleanup(ctx, run)
	if err != nil {
		return nil, nil, ReasonCapabilityUnsupported, err
	}
	if err := run.Shared.Verify(); err != nil {
		return nil, nil, ReasonPolicyUnavailable, fmt.Errorf("%w: %s", ErrNotConfigured, ReasonPolicyUnavailable)
	}
	caps := rt.Capabilities()
	if missing := caps.Guarantees.Missing(run.Snapshot.Runtime.Requires | run.Shared.Requires); missing != 0 {
		return nil, nil, ReasonCapabilityUnsupported, fmt.Errorf("%w: the %s runtime no longer provides %s", ErrNotConfigured, caps.Backend, missing)
	}
	rc := res.Capabilities(ctx)
	if missing := rc.Set.Missing(run.Shared.ResourceRequires); missing != 0 {
		return nil, nil, ReasonCapabilityUnsupported, fmt.Errorf("%w: the %s backend no longer provides %s", ErrNotConfigured, rc.Backend, missing)
	}
	return rt, res, "", nil
}

// ---- start ----

// startSharedMutation moves a queued run_shared run to running: it reserves the
// run's capacity and active-attempt slot, creates the resource intents and pins
// the deadline, all in one transaction before any runtime call. Insufficient
// capacity rolls it back and leaves the run queued. A run whose retained pins no
// longer verify is failed with policy_unavailable instead (nothing exists to
// clean up).
func (s *Service) startSharedMutation(lim Limit) Mutation {
	return func(ctx context.Context, env Env, agg *Aggregate) (Effects, error) {
		run := agg.Run
		now := env.Now()
		if run.Purpose == PurposeWork {
			if _, err := env.Work(); err != nil {
				if errors.Is(err, ErrWorkCancelled) {
					agg.RequestCancel(ReasonWorkCancelled, now)
					after, aerr := advance(env, agg)
					return Effects{After: after}, aerr
				}
				return Effects{}, err
			}
			if err := env.Authorize(ctx, run.RequestedBy, run.Scope, PermRunManage); err != nil {
				return Effects{}, normalizeAuth(err)
			}
		} else if run.DeploymentID == "" {
			return Effects{}, ErrDenied
		}
		if run.Status != RunQueued || len(agg.Resources) != 0 || run.Draining() {
			return Effects{}, nil
		}
		if err := run.Shared.Verify(); err != nil {
			agg.RecordFailure(Reason(ReasonPolicyUnavailable), "", now)
			after, aerr := advance(env, agg)
			return Effects{After: after}, aerr
		}
		deadline := now.Add(time.Duration(run.Shared.RunSeconds) * time.Second)
		specs, err := BuildResourceSpecs(run, deadline)
		if err != nil {
			return Effects{}, err
		}
		if err := agg.StartShared(specs, deadline, now); err != nil {
			return Effects{}, err
		}
		units := run.Shared.Capacity
		reserve := func(ctx context.Context, env Env) error {
			se, err := sharedEnvOf(env)
			if err != nil {
				return err
			}
			return se.ReserveRunCapacity(ctx, run, units, lim)
		}
		return Effects{After: []func(context.Context, Env) error{reserve}}, nil
	}
}

// StartShared starts resource holding of a queued run_shared run. It is
// idempotent and safe to repeat from any controller. It only starts a run this
// controller may launch.
func (s *Service) StartShared(ctx context.Context, ref RunRef) error {
	view, err := s.d.Store.Get(ctx, ref.Scope, ref.ID)
	if err != nil {
		return err
	}
	if view.Run.Shared == nil || view.Run.Status != RunQueued {
		return nil
	}
	if _, _, _, err := s.sharedLaunch(ctx, view.Run); err != nil {
		return err
	}
	lim, err := s.limits(ctx, ref.Scope)
	if err != nil {
		return err
	}
	opts := UpdateOptions{Quota: true, LockWork: view.Run.Purpose == PurposeWork, SkipLocked: true}
	_, _, err = s.d.Store.Update(ctx, ref.Scope, ref.ID, opts, s.startSharedMutation(lim))
	return err
}

func (s *Service) limits(ctx context.Context, scope domain.Scope) (Limit, error) {
	lim, err := s.d.Limits.Limits(ctx, scope)
	if err != nil {
		return Limit{}, Unavailablef(err, "execution limits are unavailable")
	}
	if err := lim.Validate(); err != nil {
		return Limit{}, Unavailablef(err, "execution limits are invalid")
	}
	return lim, nil
}

// ---- dispatch ----

func (s *Service) claimShared(ctx context.Context, ref RunRef, view *Aggregate) (*AttemptSpec, error) {
	if _, _, _, err := s.sharedLaunch(ctx, view.Run); err != nil {
		// Reconciliation records the failure and tears the run down; a claim only
		// declines.
		s.log(ctx, "claim", err, "run", string(ref.ID))
		return nil, nil
	}
	if view.Run.Status == RunQueued {
		if err := s.StartShared(ctx, ref); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if len(view.Resources) == 0 {
		return nil, nil
	}
	// Recheck stale observations immediately before dispatch: an observation of
	// ready older than one reconciliation period is not trusted.
	if err := s.refreshResources(ctx, ref, s.o.ResourceRecheck); err != nil {
		return nil, err
	}
	view, err := s.d.Store.Get(ctx, ref.Scope, ref.ID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	step := view.NextSharedRunnable(now, s.d.Hooks)
	if step == nil {
		return nil, nil
	}
	if _, err := s.launchable(view.Run, step); err != nil {
		s.log(ctx, "claim", err, "run", string(ref.ID), "step", step.ID)
		return nil, nil
	}
	lim, err := s.limits(ctx, ref.Scope)
	if err != nil {
		return nil, err
	}
	var src SourceInfo
	if access := view.Run.Shared.Access[step.ID]; access != AccessNone {
		src, err = s.d.Sources.Describe(ctx, ref.Scope, view.Run.WorkspaceID, access == AccessWrite)
		if err != nil {
			return nil, mapLookup(err, "source")
		}
	}
	opts := UpdateOptions{Quota: true, LockWork: view.Run.Purpose == PurposeWork, SkipLocked: true}
	_, res, err := s.d.Store.Update(ctx, ref.Scope, ref.ID, opts, s.claimSharedMutation(step.ID, src, lim))
	if err != nil {
		return nil, err
	}
	if out, ok := res.(claimOutcome); ok {
		return out.Intent, nil
	}
	return nil, nil
}

// claimSharedMutation is the run_shared dispatch decision, rechecked under
// every lock: authorization, work cancellation, cancellation/failure intent,
// the total deadline, the pinned policy, the provisioning barrier (network,
// workspace and every service verified ready and fresh), the single workspace
// holder, the deterministic order and its predecessor's settlement, retry
// safety and the logical writer. The run's quantities and its active-attempt
// slot were reserved at start and are consumed by every attempt without a second
// charge, so a run holding services can never wait for quota for its next
// attempt.
func (s *Service) claimSharedMutation(stepID string, src SourceInfo, lim Limit) Mutation {
	return func(ctx context.Context, env Env, agg *Aggregate) (Effects, error) {
		now := env.Now()
		run := agg.Run
		none := func() (Effects, error) {
			after, err := advance(env, agg)
			return Effects{After: after, Result: claimOutcome{}}, err
		}
		if run.Purpose == PurposeWork {
			if _, err := env.Work(); err != nil {
				if errors.Is(err, ErrWorkCancelled) {
					agg.RequestCancel(ReasonWorkCancelled, now)
					return none()
				}
				return Effects{}, err
			}
			if err := env.Authorize(ctx, run.RequestedBy, run.Scope, PermRunManage); err != nil {
				return Effects{}, normalizeAuth(err)
			}
		} else if run.DeploymentID == "" {
			return Effects{}, ErrDenied
		}
		if err := run.Shared.Verify(); err != nil {
			return Effects{}, err
		}
		if agg.CheckRunDeadline(now) {
			return none()
		}
		if run.Status != RunRunning || !agg.BarrierOpen(now, s.o.ResourceFreshness) {
			return Effects{Result: claimOutcome{}}, nil
		}
		if agg.ActiveHolder() != nil {
			return Effects{Result: claimOutcome{}}, nil
		}
		step := agg.NextSharedRunnable(now, s.d.Hooks)
		if step == nil || step.ID != stepID {
			return none()
		}
		if agg.StepBegan(step.ID) {
			// A step whose earlier attempt began may have changed the scratch: it is
			// never dispatched again automatically.
			return Effects{Result: claimOutcome{}}, nil
		}
		attemptID := domain.AttemptID(env.NewID("att"))
		rs, err := agg.Resolve(step)
		if err != nil {
			return s.failStep(env, agg, step, ReasonInvalidRef)
		}
		c, err := BuildContextV2(agg, step, attemptID, rs, src, lim)
		if err != nil {
			if errors.Is(err, ErrAdmissionRefused) {
				return s.failStep(env, agg, step, ReasonStepFailed)
			}
			return Effects{}, err
		}
		plan, err := planSharedAttempt(agg, c)
		if err != nil {
			return Effects{}, err
		}
		access := run.Shared.Access[step.ID]
		input, tip := "", ""
		workspaceID := ""
		if access != AccessNone {
			input, workspaceID = agg.HeadCommit(), run.WorkspaceID
		}
		if access == AccessWrite {
			tip = src.AcceptedTip
		}
		timeout := time.Duration(c.Limits.TimeoutSeconds) * time.Second
		at, err := agg.Claim(ClaimParams{
			StepID: step.ID, AttemptID: attemptID, Plan: plan, Access: access, Workspace: workspaceID, Input: input, Tip: tip,
			Timeout: timeout, Startup: lim.StartupBudget, Lease: s.o.ClaimLease,
		}, now)
		if err != nil {
			return Effects{}, err
		}
		if err := agg.acquireHolder(at, now); err != nil {
			return Effects{}, err
		}
		var after []func(context.Context, Env) error
		if at.Writer() {
			after = append(after, func(ctx context.Context, env Env) error { return env.ReserveWriter(ctx, at) })
		}
		spec := at.spec(run.Purpose, false)
		return Effects{After: after, Result: claimOutcome{Intent: &spec}}, nil
	}
}

// sharedDriveCheck verifies, outside any transaction and immediately before the
// runtime is asked to create or adopt an attempt's workload, that the run
// model, backend, pins, policy, capabilities and the provisioning barrier all
// still hold and that the attempt still has live command authority. Any failure
// means nothing is started; the attempt stays unconfirmed for a later pass, and
// cancellation and deadlines still end it.
func (s *Service) sharedDriveCheck(ctx context.Context, spec AttemptSpec) (*SharedLaunch, error) {
	if s.d.Shared == nil {
		// No run_shared composition: no run_shared attempt can have been claimed by
		// this controller, and launchable already refuses one found by redrive.
		return nil, nil
	}
	ref := RunRef{Scope: spec.Scope, ID: spec.RunID}
	view, err := s.d.Store.Get(ctx, ref.Scope, ref.ID)
	if err != nil {
		return nil, err
	}
	if view.Run.Shared == nil {
		return nil, nil
	}
	if _, _, _, err := s.sharedLaunch(ctx, view.Run); err != nil {
		return nil, err
	}
	if err := s.refreshResources(ctx, ref, s.o.ResourceRecheck); err != nil {
		return nil, err
	}
	view, err = s.d.Store.Get(ctx, ref.Scope, ref.ID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	at := view.Attempt(spec.AttemptID)
	if at == nil || !view.CommandAuthorized(at, now) || !view.BarrierOpen(now, s.o.ResourceFreshness) {
		return nil, fmt.Errorf("%w: the run_shared launch barrier is not open", ErrNotConfigured)
	}
	launch, err := SharedLaunchOf(view, spec.StepID)
	if err != nil {
		return nil, err
	}
	if err := launch.Validate(spec); err != nil {
		return nil, err
	}
	return launch, nil
}

// SharedLaunchOf derives the launch description of a run_shared step from the
// run's verified pins and its workspace's committed identity. The workspace must
// be bound and not stopping; the role is the pinned policy body's step role.
func SharedLaunchOf(a *Aggregate, stepID string) (*SharedLaunch, error) {
	sh := a.Run.Shared
	if sh == nil {
		return nil, invalidf("not a run_shared run")
	}
	ws := a.Resource(KeyWorkspace)
	if ws == nil || !ws.Bound() || ws.Stopping() || ws.Removed() {
		return nil, fmt.Errorf("%w: the run workspace is not bound", ErrNotConfigured)
	}
	role, ok := sh.PolicyBody.Roles[RoleStep]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotConfigured, ReasonPolicyUnavailable)
	}
	digest, err := sh.PolicyBody.RoleDigest(RoleStep)
	if err != nil {
		return nil, err
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
	return &SharedLaunch{
		Policy: sh.Policy, Role: role, RoleDigest: digest, PodSecurity: sh.PolicyBody.PodSecurity,
		Workspace: ws.Handle(), Network: sh.PolicyBody.Network, Egress: egress, EgressDigests: digests,
	}, nil
}

// ---- receipt and begin ----

// GrantClaim names the runtime session a durable mutation is performed under: the
// session and signing generation of the verified authorization. The runtime
// authority has already verified the bearer; this is what the mutation hands to
// its own transaction so the grant can be checked THERE (see GrantFence).
type GrantClaim struct {
	Session    string
	Generation int64
}

// Validate refuses a claim that names no session.
func (c GrantClaim) Validate() error {
	if c.Session == "" || c.Generation < 1 {
		return fmt.Errorf("%w: no runtime session is named", ErrInvalid)
	}
	return nil
}

// GrantFence is the transaction-bound check of a runtime grant, implemented by the
// Env of a store whose database carries the runtime grants. A mutation that must
// be atomic with revocation calls it AFTER the run row is locked (lock order:
// run -> grants) and in the transaction that records its result: the grant row is
// read under a share lock, so a revocation (a single UPDATE of the grant row,
// outside any execution transaction) either committed before and is seen here, or
// waits for this transaction. A grant that is revoked, missing, or not the
// claim's session and generation is no authority: ErrAttemptRevoked for an
// attempt's grant and ErrResourceRevoked for a resource's.
type GrantFence interface {
	AttemptGrantLive(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, claim GrantClaim) error
	ResourceGrantLive(ctx context.Context, scope domain.Scope, run domain.RunID, key string, claim GrantClaim) error
}

// fenceOf returns the grant fence of a mutation's transaction, or ErrNotConfigured:
// recording on a runtime session without being able to check its grant in the
// same transaction is refused, never skipped.
func fenceOf(env Env) (GrantFence, error) {
	f, ok := env.(GrantFence)
	if !ok {
		return nil, fmt.Errorf("%w: the store has no runtime grant fence", ErrNotConfigured)
	}
	return f, nil
}

// ReceiptAck acknowledges a recorded preparation receipt.
type ReceiptAck struct {
	Digest     string
	RecordedAt time.Time
	Replayed   bool
}

// RecordPreparationReceipt records the trusted checkout receipt of a run's
// preparation. The caller (the resource-subject route, P3) has verified the
// bearer token against committed state; this use case rechecks everything under
// the run lock and binds the digest to the workspace's committed UID.
//
// The claim names the runtime session the receipt is recorded under; its grant is
// checked in this same transaction, after the run lock (GrantFence): a revocation
// committed after the caller's own check still stops the record, and a store
// without the fence refuses.
func (s *Service) RecordPreparationReceipt(ctx context.Context, scope domain.Scope, run domain.RunID, claim GrantClaim, req runtimev2.PreparationReceiptRequest) (ReceiptAck, error) {
	if err := claim.Validate(); err != nil {
		return ReceiptAck{}, err
	}
	var ack ReceiptAck
	_, _, err := s.d.Store.Update(ctx, scope, run, UpdateOptions{}, func(ctx context.Context, env Env, agg *Aggregate) (Effects, error) {
		if err := s.ownsModel(agg.Run); err != nil {
			return Effects{}, err
		}
		fence, err := fenceOf(env)
		if err != nil {
			return Effects{}, err
		}
		if err := fence.ResourceGrantLive(ctx, scope, run, KeyPreparation, claim); err != nil {
			return Effects{}, err
		}
		d, replay, err := agg.RecordPreparationReceipt(req, env.Now())
		if err != nil {
			return Effects{}, err
		}
		at := env.Now()
		if prep := agg.Resource(KeyPreparation); prep != nil && prep.ReceiptAt != nil {
			at = *prep.ReceiptAt
		}
		ack = ReceiptAck{Digest: d, RecordedAt: at, Replayed: replay}
		return Effects{}, nil
	})
	return ack, err
}

// BeginAck is the durable acknowledgment of a command-begin.
type BeginAck struct {
	AttemptID      domain.AttemptID
	BeganAt        time.Time
	WorkspaceState runtimev2.WorkspaceState
	IgnoredPresent bool
	Replayed       bool
}

// RecordBegin journals the command-begin authorization of an attempt durably,
// before the entrypoint starts the command. It linearizes against cancellation
// under the run lock. A lost acknowledgment leaves the record in place: the
// attempt is treated as begun and never retried automatically.
//
// The claim names the runtime session the begin is recorded under; its grant is
// checked in this same transaction, after the run lock (GrantFence), so a
// revocation committed after the caller's own check records nothing. A store
// without the fence refuses.
func (s *Service) RecordBegin(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, claim GrantClaim, req runtimev2.BeginRequest) (BeginAck, error) {
	if err := firstOf(scope.Validate(), attempt.Validate()); err != nil {
		return BeginAck{}, ErrNotFound
	}
	if err := claim.Validate(); err != nil {
		return BeginAck{}, err
	}
	run, err := s.d.Store.RunOf(ctx, scope, attempt)
	if err != nil {
		return BeginAck{}, err
	}
	var ack BeginAck
	_, res, err := s.d.Store.Update(ctx, scope, run, UpdateOptions{}, func(ctx context.Context, env Env, agg *Aggregate) (Effects, error) {
		ack = BeginAck{}
		if err := s.ownsModel(agg.Run); err != nil {
			return Effects{}, err
		}
		at := agg.Attempt(attempt)
		if at == nil {
			return Effects{}, ErrNotFound
		}
		pinned, err := runtimev2.DecodeRequest[runtimev2.ContextV2](at.Context)
		if err != nil {
			return Effects{}, Unavailablef(err, "the pinned context is unreadable")
		}
		if agg.CheckRunDeadline(env.Now()) {
			// The total deadline passed: persist the failure and the revocation, refuse
			// the begin. The record of an earlier begin stays.
			after, aerr := advance(env, agg)
			return Effects{After: after, Result: beginRevoked{}}, aerr
		}
		fence, err := fenceOf(env)
		if err != nil {
			return Effects{}, err
		}
		if err := fence.AttemptGrantLive(ctx, scope, attempt, claim); err != nil {
			return Effects{}, err
		}
		rec, replay, err := agg.RecordBegin(BeginInput{Attempt: at, Request: req, Context: pinned}, env.Now())
		if err != nil {
			return Effects{}, err
		}
		ack = BeginAck{AttemptID: rec.AttemptID, BeganAt: rec.BeganAt, WorkspaceState: rec.WorkspaceState, IgnoredPresent: rec.IgnoredPresent, Replayed: replay}
		return Effects{}, nil
	})
	if _, revoked := res.(beginRevoked); err == nil && revoked {
		return BeginAck{}, ErrAttemptRevoked
	}
	return ack, err
}

// decodeAttemptContext decodes an attempt's pinned context into the v1
// projection result checking needs: outputs, the effective access and the
// ownership fields, for either context version.
func decodeAttemptContext(at *Attempt) (runtimev1.Context, error) {
	if isContextV2(at.Context) {
		c, err := runtimev2.DecodeRequest[runtimev2.ContextV2](at.Context)
		if err != nil {
			return runtimev1.Context{}, Unavailablef(err, "the pinned context is unreadable")
		}
		return runtimev1.Context{
			TenantID: c.TenantID, ProjectID: c.ProjectID, RunID: c.RunID, StepID: c.StepID, AttemptID: c.AttemptID, ActionKey: c.ActionKey,
			Definition: c.Definition, WorkItem: c.WorkItem, DesignBaseline: c.DesignBaseline, Profile: c.Profile, Command: c.Command,
			Workspace: runtimev1.Workspace{Access: c.Workspace.Access}, Inputs: c.Inputs, Outputs: c.Outputs, Secrets: c.Secrets,
			Environment: c.Environment, Limits: c.Limits,
		}, nil
	}
	c, err := runtimev1.DecodeRequest[runtimev1.Context](at.Context)
	if err != nil {
		return runtimev1.Context{}, Unavailablef(err, "the pinned context is unreadable")
	}
	return c, nil
}

func isContextV2(raw []byte) bool {
	var v struct {
		ContextVersion int64 `json:"contextVersion"`
	}
	return json.Unmarshal(raw, &v) == nil && v.ContextVersion == SharedContextVersion
}

// beginRevoked is the Result of a begin refused by the total deadline after its
// failure was persisted.
type beginRevoked struct{}
