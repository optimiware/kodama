package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

// StartShared starts resource holding of a queued run_shared run in one step:
// it pins the deadline, creates the resource intents (durable before any
// runtime call) and moves the run to running. Capacity is reserved by the
// caller in the same transaction; a refusal rolls the whole step back and the
// run stays queued. It enforces: the run is run_shared, queued, not draining
// and has no resources yet.
func (a *Aggregate) StartShared(specs []ResourceSpec, deadline, now time.Time) error {
	run := a.Run
	if run.Shared == nil {
		return invalidf("not a run_shared run")
	}
	if err := run.Shared.Verify(); err != nil {
		return err
	}
	if run.Status != RunQueued || run.Draining() || len(a.Resources) != 0 {
		return transitionf("a run_shared run starts holding resources once, from queued")
	}
	if run.SourceRetention != RetentionConfirmed && run.SourceRetention != RetentionNotRequired {
		return ErrSourceNotReady
	}
	fromRun := run.Status
	if _, err := run.markRunning(now); err != nil {
		return err
	}
	d := deadline.UTC()
	run.DeadlineAt = &d
	run.touch(now)
	a.recordRun(fromRun, now, ReasonRunning)
	for i, sp := range specs {
		r, err := NewRunResource(sp, i, now)
		if err != nil {
			return err
		}
		a.Resources = append(a.Resources, r)
		a.NewResources = append(a.NewResources, r)
		a.Transitions = append(a.Transitions, Transition{Subject: SubjectResource, ID: r.Key, From: "", To: string(r.Phase), Reason: ReasonCreated, At: now.UTC(),
			FromPhase: "none", ToPhase: string(r.Phase)})
	}
	return nil
}

// CheckRunDeadline fails the run when its pinned total deadline (which covers
// gate waits) has passed: it records the failure intent, drains attempts and
// stops resources. It reports whether it recorded the intent now.
func (a *Aggregate) CheckRunDeadline(now time.Time) bool {
	if a.Run.Shared == nil || a.Run.DeadlineAt == nil || a.Run.Status.Terminal() || a.Run.FailureIntentAt != nil || a.Run.CancelRequestedAt != nil {
		return false
	}
	if now.Before(*a.Run.DeadlineAt) {
		return false
	}
	return a.RecordFailure(Reason(ReasonRunDeadlineExceeded), "", now)
}

// ObservationAnswer is the adapter's answer to one observation request: an
// observation, or the affirmative statement that the object at the name is not
// the resource's (Collision).
type ObservationAnswer struct {
	Obs       ResourceObservation
	Collision bool
	// Fenced is the adapter's statement that the resource exists stopped or
	// removing and is never started again (an Ensure refused as fenced).
	Fenced bool
}

// ApplyObservation applies the answer to request generation gen, issued at
// requestedAt, to the resource. A generation that is not newer than an applied
// one is discarded and changes nothing (applied false): a delayed older
// "ready" can never overwrite a newer unknown or pending observation. A UID or
// fingerprint mismatch is a collision (never adopted); an affirmative loss of
// a resource in use, an unsolicited stop, an expired startup budget or a
// collision records the run's failure intent with the resource's safe reason
// (stopping the resources). Unknown observations never count as loss: they
// block dispatch until the run deadline.
func (a *Aggregate) ApplyObservation(key string, gen int64, requestedAt time.Time, ans ObservationAnswer, now time.Time) (bool, error) {
	r := a.Resource(key)
	if r == nil {
		return false, ErrNotFound
	}
	from := r.state()
	if ans.Collision || ans.Fenced {
		// Every outcome of a request is applied through the generation fence, the
		// terminal ones included: an older answer never supersedes a newer one.
		if gen <= r.AppliedGen {
			return false, nil
		}
		r.AppliedGen = gen
		reason := ReasonResourceCollision
		if ans.Fenced {
			reason = ReasonResourceFenced
		}
		r.Reason = reason
		r.touch(now)
		a.recordResource(r, from, now)
		a.RecordFailure(Reason(reason), "", now)
		return true, nil
	}
	applied, err := r.observeAt(gen, requestedAt, ans.Obs, now)
	switch {
	case errors.Is(err, ErrResourceCollision):
		r.Reason = ReasonResourceCollision
		a.recordResource(r, from, now)
		a.RecordFailure(Reason(ReasonResourceCollision), "", now)
		return true, nil
	case err != nil:
		return false, err
	case !applied:
		return false, nil
	}
	a.recordResource(r, from, now)
	if reason, lost := r.Lost(now); lost {
		a.RecordFailure(Reason(reason), "", now)
	} else if reason, expired := r.StartupExpired(now); expired {
		// Only a service that became ready is exempt (StartupExpired never expires
		// one); a preparation completes with its trusted receipt, never with a
		// generic ready phase.
		a.RecordFailure(Reason(reason), "", now)
	}
	return true, nil
}

// RecordStopEvidence records verified (or unconfirmed) stop evidence.
func (a *Aggregate) RecordStopEvidence(key string, ev ResourceStopEvidence, now time.Time) error {
	r := a.Resource(key)
	if r == nil {
		return ErrNotFound
	}
	from := r.state()
	r.requestStop(ReasonStopUnconfirmed, now)
	if r.recordStop(ev, now) {
		a.recordResource(r, from, now)
	}
	return nil
}

// RecordRemoveEvidence records verified (or unconfirmed) removal.
func (a *Aggregate) RecordRemoveEvidence(key string, ev ResourceRemoveEvidence, now time.Time) error {
	r := a.Resource(key)
	if r == nil {
		return ErrNotFound
	}
	from := r.state()
	changed, err := r.recordRemove(ev, now)
	if err != nil {
		return err
	}
	if changed {
		a.recordResource(r, from, now)
	}
	if r.Kind == KindWorkspace && r.Removed() {
		// The volume is gone: no holder can remain.
		for _, h := range a.Holders {
			if h.ReleasedAt == nil {
				t := now.UTC()
				h.ReleasedAt, h.dirty = &t, true
			}
		}
	}
	return nil
}

// ---- preparation receipt ----

// RecordPreparationReceipt records the trusted checkout receipt of the
// preparation resource: bound to the run, the resource key and the workspace's
// bound UID in its canonical digest. It is refused unless both resources have
// their UIDs bound (resource_unbound), the preparation is not stopping or past
// the deadline (resource_revoked), the commit equals the run's pinned input
// commit and the safety fields hold. An identical repeat returns the original;
// a different receipt is a conflict. The preparation's workload must be
// observed stopped before the workspace is ready; that is a separate barrier.
func (a *Aggregate) RecordPreparationReceipt(req runtimev2.PreparationReceiptRequest, now time.Time) (digest string, replay bool, err error) {
	prep, ws := a.Resource(KeyPreparation), a.Resource(KeyWorkspace)
	if a.Run.Shared == nil || prep == nil || ws == nil {
		return "", false, ErrNotFound
	}
	if err := req.Validate(); err != nil {
		return "", false, invalidf("%v", err)
	}
	if req.Commit != runtimev1.GitCommit(a.Run.SourceCommit) {
		return "", false, invalidf("the receipt commit is not the run's pinned input commit")
	}
	if req.Attempts > a.Run.Shared.PolicyBody.Limits.PreparationAttempts {
		return "", false, invalidf("the receipt reports more tries than the policy allows")
	}
	obj := PreparationReceiptObject{Request: req, RunID: a.Run.ID, ResourceKey: KeyPreparation, WorkspaceUID: ws.UID}
	if prep.Receipt != nil {
		if prep.ReceiptDigest == "" || ws.UID == "" {
			return "", false, ErrReceiptConflict
		}
		d, derr := obj.Digest()
		if derr != nil || d != prep.ReceiptDigest || *prep.Receipt != req {
			return "", false, ErrReceiptConflict
		}
		return d, true, nil
	}
	if !prep.Bound() || !ws.Bound() {
		return "", false, ErrResourceUnbound
	}
	if prep.Stopping() || prep.Stopped() || a.Run.Draining() || a.Run.Status.Terminal() ||
		(a.Run.DeadlineAt != nil && !now.Before(*a.Run.DeadlineAt)) {
		return "", false, ErrResourceRevoked
	}
	d, derr := obj.Digest()
	if derr != nil {
		return "", false, derr
	}
	from := prep.state()
	r := req
	t := now.UTC()
	prep.Receipt, prep.ReceiptDigest, prep.ReceiptAt = &r, d, &t
	prep.touch(now)
	a.recordResource(prep, from, now)
	return d, false, nil
}

// ---- command begin ----

// BeginInput is the verified begin request of an attempt: the pinned context
// (decoded from the attempt) and the entrypoint's workspace observation.
type BeginInput struct {
	Attempt *Attempt
	Request runtimev2.BeginRequest
	Context runtimev2.ContextV2
}

// RecordBegin is the durable command-begin authorization. It is idempotent for
// the same body, a conflict for a different body, and refused for a revoked,
// cancelled, terminal or past-deadline attempt: command authorization
// linearizes against cancellation under the run lock. The derived state is
// server-side (never client-asserted).
func (a *Aggregate) RecordBegin(in BeginInput, now time.Time) (*BeginRecord, bool, error) {
	at := in.Attempt
	if a.Run.Shared == nil || at == nil || a.Attempt(at.ID) == nil {
		return nil, false, ErrNotFound
	}
	state, ignored, err := runtimev2.DeriveBegin(in.Context, in.Request)
	if err != nil {
		return nil, false, invalidf("%v", err)
	}
	raw, err := json.Marshal(in.Request)
	if err != nil {
		return nil, false, err
	}
	sum := sha256.Sum256(raw)
	body := "sha256:" + hex.EncodeToString(sum[:])
	// The LIVE command authority is rechecked under the run lock before any
	// acknowledgment, an identical replay included: a begin that committed
	// before a cancellation, failure or the deadline is not authority to start
	// the command afterwards. The recorded begin stays, so the attempt is still
	// treated as begun by the retry and effect barriers.
	if !a.CommandAuthorized(at, now) {
		return nil, false, ErrAttemptRevoked
	}
	for _, b := range a.Begins {
		if b.AttemptID == at.ID {
			if b.BodyDigest != body {
				return nil, false, ErrBeginConflict
			}
			return b, true, nil
		}
	}
	rec := &BeginRecord{
		Scope: a.Run.Scope, RunID: a.Run.ID, AttemptID: at.ID, StepID: at.StepID, Access: at.Access, BeganAt: now.UTC(),
		WorkspaceState: state, IgnoredPresent: ignored, BodyDigest: body,
	}
	a.Begins = append(a.Begins, rec)
	a.NewBegins = append(a.NewBegins, rec)
	return rec, false, nil
}

// CommandAuthorized reports whether the attempt may still be authorized to
// start its command: running with live authority, the run neither draining nor
// terminal and before the pinned total deadline.
func (a *Aggregate) CommandAuthorized(at *Attempt, now time.Time) bool {
	if at.Status != AttemptRunning || !a.control(at.ID).Live() || a.Run.Draining() || a.Run.Status.Terminal() {
		return false
	}
	return a.Run.DeadlineAt == nil || now.Before(*a.Run.DeadlineAt)
}

// ---- order and dispatch selection ----

// NextSharedRunnable returns the step to dispatch under the run's pinned
// deterministic execution order: the first step of the order that is not
// terminal. It is runnable only when it is a pending, due, dependency-satisfied
// executable step whose kind is enabled and that has no unsettled attempt. A
// gate occupies its position: nothing after it dispatches until it resolves. A
// step in retry backoff keeps its slot. Everything before it is terminal, so
// its predecessor is settled.
func (a *Aggregate) NextSharedRunnable(now time.Time, hooks Hooks) *StepRun {
	if a.Run.Shared == nil || !a.Run.AcceptsDispatch() {
		return nil
	}
	a.Propagate(now)
	for _, id := range a.Run.Shared.Order {
		s := a.Step(id)
		if s == nil {
			return nil
		}
		if s.Status.Terminal() {
			if !a.stepSettled(s) {
				return nil
			}
			continue
		}
		if a.Runnable(s, now, hooks) {
			return s
		}
		return nil
	}
	return nil
}

// stepSettled reports that a terminal step's attempts are all settled, so its
// workload and effects no longer hold the workspace.
func (a *Aggregate) stepSettled(s *StepRun) bool {
	for _, at := range a.AttemptsOf(s.ID) {
		if !a.control(at.ID).Settled() {
			return false
		}
	}
	return true
}

// HeadCommit is the latest commit published in this run by a successful
// writer, or the run's pinned input commit before any.
func (a *Aggregate) HeadCommit() string {
	head := a.Run.SourceCommit
	for _, id := range a.Run.Shared.Order {
		s := a.Step(id)
		if s != nil && s.Status == StepSucceeded && s.Spec.IsWriter() && s.OutputCommit != "" {
			head = s.OutputCommit
		}
	}
	return head
}

// ---- runtime v2 context ----

// BuildContextV2 assembles the immutable runtime v2 context of an attempt from
// the run's pins, durable resource state, recorded begins and the selected
// source. Nothing in it comes from a workload or a client claim: the
// preparation pin is the recorded server receipt bound to the workspace UID.
func BuildContextV2(a *Aggregate, step *StepRun, attempt domain.AttemptID, rs ResolvedStep, src SourceInfo, lim Limit) (runtimev2.ContextV2, error) {
	run := a.Run
	sh := run.Shared
	if sh == nil || run.StartedAt == nil || run.DeadlineAt == nil {
		return runtimev2.ContextV2{}, invalidf("the run has not started holding resources")
	}
	environment, err := run.Snapshot.environmentOf(step)
	if err != nil {
		return runtimev2.ContextV2{}, err
	}
	timeout := time.Duration(step.Spec.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = lim.DefaultTimeout
	}
	if timeout > lim.MaxTimeout {
		return runtimev2.ContextV2{}, Refusedf("the step timeout exceeds the configured limit")
	}
	access := sh.Access[step.ID]
	c := runtimev2.ContextV2{
		ContextVersion: SharedContextVersion, RunModel: RunModelShared,
		TenantID: run.Scope.TenantID, ProjectID: run.Scope.ProjectID, RunID: run.ID, StepID: step.ID, AttemptID: attempt, ActionKey: step.ActionKey,
		Definition:    runtimev1.Definition{ID: run.DefinitionID, Digest: runtimev1.Digest(run.DefinitionDigest)},
		Command:       runtimev1.Command{Image: rs.Image, Command: rs.Command, Args: rs.Args, Env: rs.Env},
		Endpoints:     map[string]runtimev2.Endpoint{},
		Egress:        append([]string{}, sh.Egress[step.ID]...),
		Run:           runtimev2.RunWindow{StartedAt: stamp(*run.StartedAt), DeadlineAt: stamp(*run.DeadlineAt)},
		Policy:        runtimev2.PolicyPin{ID: sh.Policy.ID, Digest: runtimev1.Digest(sh.Policy.Digest)},
		Inputs:        run.Snapshot.Values(),
		Outputs:       rs.Outputs,
		Secrets:       run.Snapshot.SecretsOf(step.ID),
		Environment:   environment,
		State:         run.Snapshot.StatesOf(step.ID),
		ArtifactFiles: run.Snapshot.ArtifactsOf(step.ID),
		Limits: runtimev1.Limits{
			TimeoutSeconds: int64(timeout / time.Second), MaxOutputBytes: lim.MaxOutputBytes,
			MaxOutputFiles: lim.MaxOutputFiles, MaxDiagnosticBytes: lim.MaxDiagnosticBytes,
		},
		Workspace: runtimev2.WorkspaceV2{Access: runtimev1.WorkspaceAccess(access), MountPath: runtimev2.MountPath, Lineage: []runtimev2.LineageEntry{}},
	}
	if c.Secrets == nil {
		c.Secrets = []runtimev1.SecretBinding{}
	}
	for name, ep := range sh.Endpoints {
		ports := map[string]int64{}
		for k, v := range ep.Ports {
			ports[k] = v
		}
		c.Endpoints[name] = runtimev2.Endpoint{Host: ep.Host, Ports: ports}
	}
	if run.Specification != nil {
		crit := run.Specification.AcceptanceCriteria
		if crit == nil {
			crit = []runtimev1.AcceptanceCriterion{}
		}
		c.WorkItem = &runtimev1.WorkItem{ID: run.WorkItemID, ContentRevision: run.ContentRevision, Spec: run.Specification.Spec, AcceptanceCriteria: crit}
	}
	if sh.SourceBacked {
		prep := a.Resource(KeyPreparation)
		if prep == nil || prep.Receipt == nil || !a.ReceiptBound() {
			return runtimev2.ContextV2{}, Refusedf("the preparation receipt is not recorded")
		}
		repo := sh.RepositoryURL
		if repo == "" {
			repo = src.RepositoryURL
		}
		source := &runtimev2.WorkspaceSource{RepositoryURL: repo, InputCommit: runtimev1.GitCommit(run.SourceCommit), HeadCommit: runtimev1.GitCommit(a.HeadCommit())}
		if access == AccessWrite {
			br, tip := src.Branch, runtimev1.GitCommit(src.AcceptedTip)
			source.Branch, source.ExpectedTip = &br, &tip
		}
		c.Workspace.Source = source
		c.Workspace.Preparation = &runtimev2.PreparationPin{
			Commit: prep.Receipt.Commit, TreeID: prep.Receipt.TreeID, ReceiptDigest: runtimev1.Digest(prep.ReceiptDigest),
		}
	}
	begins := append([]*BeginRecord(nil), a.Begins...)
	sort.Slice(begins, func(i, j int) bool {
		if !begins[i].BeganAt.Equal(begins[j].BeganAt) {
			return begins[i].BeganAt.Before(begins[j].BeganAt)
		}
		return begins[i].AttemptID < begins[j].AttemptID
	})
	for _, b := range begins {
		e := runtimev2.LineageEntry{
			StepID: b.StepID, AttemptID: b.AttemptID, Access: runtimev1.WorkspaceAccess(b.Access), BeganAt: stamp(b.BeganAt),
			WorkspaceState: b.WorkspaceState, IgnoredPresent: b.IgnoredPresent,
		}
		if prior := a.Attempt(b.AttemptID); prior != nil && prior.Status == AttemptSucceeded && prior.Result != nil && prior.Result.SourceCommit != nil {
			cm := *prior.Result.SourceCommit
			e.PublishedCommit = &cm
		}
		c.Workspace.Lineage = append(c.Workspace.Lineage, e)
	}
	return c, nil
}

func stamp(t time.Time) runtimev1.Timestamp {
	return runtimev1.Timestamp(t.UTC().Format("2006-01-02T15:04:05Z"))
}

// SharedPlan is the workload plan of a run_shared attempt: its deterministic
// workload name, a fingerprint binding the context digest, image, action key,
// policy pin and runtime v2, and the canonical context.
func planSharedAttempt(a *Aggregate, c runtimev2.ContextV2) (WorkloadPlan, error) {
	if err := c.Validate(); err != nil {
		return WorkloadPlan{}, invalidf("pinned context: %v", err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return WorkloadPlan{}, err
	}
	canonical, err := runtimev1.Canonicalize(raw)
	if err != nil {
		return WorkloadPlan{}, invalidf("pinned context: %v", err)
	}
	sum := sha256.Sum256(canonical)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	name := WorkloadName(a.Run.Scope.TenantID, c.AttemptID)
	fp := sha256.Sum256([]byte("kodama/job-spec/v2|" + name + "|" + digest + "|" + c.Command.Image + "|" + c.ActionKey + "|" + a.Run.Shared.Policy.Digest))
	return WorkloadPlan{WorkloadName: name, SpecFingerprint: "sha256:" + hex.EncodeToString(fp[:]), ContextDigest: digest, Context: canonical}, nil
}

// DecodeContextV2 decodes the pinned runtime v2 context of an attempt spec.
func (s AttemptSpec) DecodeContextV2() (runtimev2.ContextV2, error) {
	if err := s.Validate(); err != nil {
		return runtimev2.ContextV2{}, err
	}
	c, err := runtimev2.DecodeRequest[runtimev2.ContextV2](s.Context)
	if err != nil {
		return runtimev2.ContextV2{}, invalidf("the attempt context is unreadable")
	}
	if (domain.Scope{TenantID: c.TenantID, ProjectID: c.ProjectID}) != s.Scope || c.RunID != s.RunID || c.StepID != s.StepID || c.AttemptID != s.AttemptID {
		return runtimev2.ContextV2{}, invalidf("the attempt context belongs to another attempt")
	}
	return c, nil
}

var _ = pipeline.KindContainer
