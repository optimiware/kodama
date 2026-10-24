package kube

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// Backend is the identity of the Kubernetes runtime.
const Backend = "kubernetes"

// Runtime implements execution.Runtime with one restricted Job per attempt:
// crash-safe, idempotent creation, observation, conservative termination and
// retention-aware removal. It acts only on committed attempts, outside any SQL
// transaction, and re-reads the committed attempt from the durable store at
// every decision point: nothing it decides rests on memory a restart loses.
//
// The attempts it starts must have been planned by NewPlanner over the same
// policy: Start refuses a spec whose fingerprint is not the policy's recipe
// fingerprint (ErrPolicyDrift) and never translates one.
type Runtime struct {
	*core
	logs PodLogs
}

var _ execution.Runtime = (*Runtime)(nil)

// NewRuntime validates the dependencies. The registrar is required.
func NewRuntime(d Dependencies, o Options) (*Runtime, error) {
	c, err := newCore(d, o, true)
	if err != nil {
		return nil, err
	}
	return &Runtime{core: c, logs: d.Logs}, nil
}

// Capabilities implements execution.Runtime. Every guarantee is enforced: the
// pinned image runs in a restricted Pod under the policy's resource bounds,
// namespace preflight requires the tenant's NetworkPolicy and ResourceQuota,
// and admission Secrets carry the recovery record. It does not advertise
// ProvesNeverStarted (O-25): the absence of the Secret, Job and Pods cannot tell
// no create request from an issued create whose outcome was lost, so a stop
// without concrete evidence reports LaunchUnknown. That is launch-history proof
// only and leaves the container guarantees above untouched.
func (e *Runtime) Capabilities() execution.Capabilities {
	return execution.Capabilities{Backend: Backend, Guarantees: execution.AllGuarantees, ProvesNeverStarted: false}
}

// checkIntent verifies every dispatch field against committed attempt data. The
// intent is a request; the committed record is the truth.
func checkIntent(in execution.AttemptSpec, rec AttemptRecord) error {
	if in.Scope != rec.Scope || in.RunID != rec.RunID || in.StepID != rec.StepID || in.AttemptID != rec.AttemptID ||
		in.WorkloadName != rec.JobName || in.SpecFingerprint != rec.SpecFingerprint || in.ContextDigest != rec.ContextDigest ||
		in.Access != rec.Access || in.ActionKey != rec.ActionKey {
		return wrap(ErrNotDispatchable, "the intent does not match the committed attempt")
	}
	return nil
}

// Start implements execution.Runtime: it creates or adopts the attempt's Job
// and returns it with the verified Job UID. It succeeds only for a verified
// workload that is owned and still authorized; every other outcome is an error
// and leaves the attempt unconfirmed for redrive with the same identity.
//
// Order: committed re-read -> expectation (policy drift check) -> admission
// Secret (credential generated once) -> registration (same credential) ->
// read-before-create -> recorded create intent -> liveness re-check -> create ->
// read-back verification -> adoption -> liveness re-check (a late-created
// workload of a cancelled attempt is deleted, never granted authority).
//
// A workload that must not (re)start (its attempt cancelled, revoked or
// terminal, its record deleting or gone, or its Job deleted and not recreated)
// is execution.ErrConflict, as for every runtime; the error still satisfies
// errors.Is(err, ErrNotDispatchable) and names the cause.
func (e *Runtime) Start(ctx context.Context, in execution.AttemptSpec) (execution.Workload, error) {
	w, err := e.start(ctx, in)
	if err != nil && errors.Is(err, ErrNotDispatchable) && !errors.Is(err, execution.ErrConflict) {
		// The committed or recorded state forbids the workload (stopped, deleted,
		// cancelled, revoked or terminal): execution's contract calls that a
		// conflict. The Kubernetes cause stays in the chain.
		err = fmt.Errorf("%w: %w", execution.ErrConflict, err)
	}
	return w, err
}

func (e *Runtime) start(ctx context.Context, in execution.AttemptSpec) (execution.Workload, error) {
	rec, err := e.attemptOf(ctx, in.Scope, in.AttemptID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return execution.Workload{}, wrap(ErrNotDispatchable, "the attempt does not exist")
		}
		return execution.Workload{}, err
	}
	if err := checkIntent(in, rec); err != nil {
		return execution.Workload{}, fmt.Errorf("%w: %w", execution.ErrConflict, err)
	}
	if !rec.Live {
		return execution.Workload{}, wrap(ErrNotDispatchable, "the attempt is cancelled, revoked or terminal")
	}
	exp, err := e.policy.Expect(rec)
	if err != nil {
		return execution.Workload{}, err
	}
	for i := 0; i < maxRecordRetries; i++ {
		var uid string
		uid, err = e.dispatchOnce(ctx, exp)
		if err == nil {
			return in.Workload(uid), nil
		}
		if !errors.Is(err, errRecordMoved) {
			return execution.Workload{}, conflict(err)
		}
	}
	return execution.Workload{}, wrap(ErrUnavailable, "the workload record kept changing")
}

// dispatchOnce creates or adopts the Job and returns its verified UID.
func (e *Runtime) dispatchOnce(ctx context.Context, exp Expectation) (string, error) {
	id := exp.Identity
	// Read before any effect: an existing Job is verified first, so a collision
	// with a foreign object creates no Secret and registers no credential.
	job, found, err := e.readJob(ctx, id.Namespace, id.JobName)
	if err != nil {
		return "", err
	}
	if found {
		v, verr := VerifyJob(exp, job)
		if verr != nil {
			return "", verr
		}
		a, err := e.loadAdmission(ctx, id)
		if err != nil {
			return "", err
		}
		if a == nil {
			return "", wrap(ErrCredentialConflict, "the admission Secret of an existing workload is gone")
		}
		if err := e.checkCA(exp, a); err != nil {
			return "", err
		}
		return e.adopt(ctx, exp, a, job, v)
	}
	a, err := e.loadAdmission(ctx, id)
	if err != nil {
		return "", err
	}
	if a != nil {
		if err := e.checkCA(exp, a); err != nil {
			return "", err
		}
	}
	if a == nil {
		if err := e.preflight.Check(ctx, id.Scope.TenantID); err != nil {
			return "", err
		}
		if a, err = e.createAdmission(ctx, exp); err != nil {
			return "", err
		}
	}
	// A workload already gone or deleting is never recreated.
	if a.wl.Phase == PhaseDeleting || a.wl.Phase == PhaseGone {
		return "", wrap(ErrNotDispatchable, "the workload was deleted and is not recreated")
	}
	if a.wl.Phase == PhaseCredentialed {
		cred, err := a.credential()
		if err != nil {
			return "", err
		}
		if err := e.register(ctx, exp, cred); err != nil {
			return "", err
		}
		if err := a.wl.Registered(); err != nil {
			return "", err
		}
		if err := e.save(ctx, a); err != nil {
			return "", err
		}
	}
	switch a.wl.Phase {
	case PhaseOwned:
		// The absence read above predates this record: another dispatcher may
		// have created and adopted the Job in between. Re-read after observing
		// ownership, verifying the same UID and current authorization as usual.
		job, found, err = e.readJob(ctx, id.Namespace, id.JobName)
		if err != nil {
			return "", err
		}
		if found {
			return e.adoptRead(ctx, exp, a, job)
		}
		return "", wrap(ErrNotDispatchable, "a confirmed workload disappeared and is not recreated")
	case PhaseCreateIssued:
		// An issued create with no Job is not permission for another one.
		_ = a.wl.CreateUnknown()
		_ = e.save(ctx, a)
		return "", wrap(ErrCreateUncertain, "a Job create was issued and no Job is visible")
	}
	// Phase registered: re-register idempotently (cheap, repairs a lost
	// registration whose record update was lost), then create.
	if cred, cerr := a.credential(); cerr == nil {
		if err := e.register(ctx, exp, cred); err != nil {
			return "", err
		}
	}
	if err := e.preflight.Check(ctx, id.Scope.TenantID); err != nil {
		return "", err
	}
	return e.create(ctx, exp, a)
}

// create issues the create under a recorded intent and resolves its outcome.
func (e *Runtime) create(ctx context.Context, exp Expectation, a *admission) (string, error) {
	id := exp.Identity
	if err := a.wl.IssueCreate(e.nowUTC()); err != nil {
		return "", err
	}
	if err := e.save(ctx, a); err != nil {
		return "", err
	}
	// The create is not sent until cancellation has been rechecked after the
	// intent was recorded: a cancellation committed from here on is handled by
	// the post-create check and, as a backstop, by orphan cleanup.
	rec, err := e.attemptOf(ctx, id.Scope, id.AttemptID)
	if err != nil || !rec.Live {
		// Nothing was sent: the create definitely did not happen.
		_ = a.wl.CreateRefused()
		_ = e.save(ctx, a)
		if err != nil {
			return "", err
		}
		return "", wrap(ErrNotDispatchable, "the attempt was cancelled before the create")
	}
	created, err := e.cluster.CreateJob(ctx, id.Namespace, exp.Job)
	switch {
	case err == nil:
		return e.adoptRead(ctx, exp, a, created)
	case errors.Is(err, kubernetes.ErrAlreadyExists):
		// Someone else holds the name; our create was not applied.
		job, found, rerr := e.readJob(ctx, id.Namespace, id.JobName)
		if rerr != nil {
			_ = a.wl.CreateUnknown()
			_ = e.save(ctx, a)
			return "", rerr
		}
		if !found {
			_ = a.wl.CreateUnknown()
			_ = e.save(ctx, a)
			return "", wrap(ErrCreateUncertain, "the Job exists and vanished")
		}
		return e.adoptRead(ctx, exp, a, job)
	case errors.Is(err, kubernetes.ErrUnknownOutcome):
		_ = a.wl.CreateUnknown()
		_ = e.save(ctx, a)
		job, found, rerr := e.readJob(ctx, id.Namespace, id.JobName)
		if rerr != nil {
			return "", rerr
		}
		if !found {
			return "", wrap(ErrCreateUncertain, "the Job create outcome is unknown")
		}
		return e.adoptRead(ctx, exp, a, job)
	case kubernetes.Definite(err):
		// The API server refused the create; another one is safe.
		_ = a.wl.CreateRefused()
		if serr := e.save(ctx, a); serr != nil {
			return "", serr
		}
		kind := ErrUnavailable
		if errors.Is(err, kubernetes.ErrInvalid) || errors.Is(err, kubernetes.ErrForbidden) {
			kind = ErrPreflight
		}
		return "", wrapCause(kind, err, "the Job create was refused")
	}
	_ = a.wl.CreateUnknown()
	_ = e.save(ctx, a)
	return "", wrapCause(ErrCreateUncertain, err, "the Job create outcome is unknown")
}

// adoptRead verifies a Job found while a create was in flight. A same-name
// object that is not ours is a collision: it is never adopted, modified or
// deleted, and our issued create was refused, so the record returns to
// registered.
func (e *Runtime) adoptRead(ctx context.Context, exp Expectation, a *admission, job kubernetes.Job) (string, error) {
	v, err := VerifyJob(exp, job)
	if err != nil {
		if a.wl.Phase == PhaseCreateIssued && a.wl.JobUID == "" {
			_ = a.wl.CreateRefused()
			_ = e.save(ctx, a)
		}
		return "", err
	}
	return e.adopt(ctx, exp, a, job, v)
}

// adopt fixes the UID of a verified Job in the record, re-checks
// authorization and returns the UID.
func (e *Runtime) adopt(ctx context.Context, exp Expectation, a *admission, job kubernetes.Job, v Verification) (string, error) {
	id := exp.Identity
	if a.wl.Phase == PhaseDeleting || a.wl.Phase == PhaseGone {
		// Termination won the race with the create: the workload is being deleted
		// and is never adopted.
		return "", wrap(ErrNotDispatchable, "the workload is being terminated")
	}
	if err := a.wl.Adopt(v); err != nil {
		return "", err
	}
	if err := e.save(ctx, a); err != nil {
		return "", err
	}
	rec, err := e.attemptOf(ctx, id.Scope, id.AttemptID)
	if err != nil {
		return "", err
	}
	if !rec.Live {
		// Cancelled while the create was in flight: the workload is terminated like
		// any other (verified UID, launch evidence captured before its Pods go) and
		// receives no authority: its admission is denied by the committed state.
		if _, terr := e.terminate(ctx, rec); terr != nil {
			e.log.Warn("late workload termination pending", "attempt", string(id.AttemptID), "error", terr)
		}
		return "", wrap(ErrNotDispatchable, "the attempt was cancelled during the create")
	}
	return v.uid, nil
}

// ---- observation and logs ----

// Observe implements execution.Runtime with the reconciler's assessment of
// the verified Job and its Pods. The committed attempt, not the handle, names
// what is expected: a Job that fails verification or carries another UID than
// the recorded ones is ErrConflict, and a definitely absent Job ErrNotFound.
// Job completion is never success: only an accepted result is.
func (e *Runtime) Observe(ctx context.Context, w execution.Workload) (execution.Observation, error) {
	rec, err := e.recordOf(ctx, w)
	if err != nil {
		return execution.Observation{}, err
	}
	exp, err := e.policy.Expect(rec)
	if err != nil {
		return execution.Observation{}, err
	}
	v, err := e.view(ctx, rec, exp)
	switch {
	case errors.Is(err, errJobAbsent):
		return execution.Observation{}, fmt.Errorf("%w: %w", execution.ErrNotFound, err)
	case err != nil:
		return execution.Observation{}, conflict(err)
	}
	obs := execution.Observation{
		Kind: execution.ObservedDispatched, WorkloadName: rec.JobName, SpecFingerprint: rec.SpecFingerprint,
		WorkloadUID: v.uid, ObservedAt: e.now(),
	}
	switch {
	case v.Failure != "":
		obs.Kind, obs.Class, obs.Reason, obs.Launch = execution.ObservedFailed, execution.ClassInfrastructure, v.Failure, v.Launch
	case v.Started:
		obs.Kind, obs.Launch = execution.ObservedStarted, execution.LaunchStarted
	}
	return obs, nil
}

// Logs implements execution.Runtime: it copies the primary container log of
// the verified Job's Pod. It needs the optional PodLogs dependency.
func (e *Runtime) Logs(ctx context.Context, w execution.Workload, dst io.Writer) error {
	if e.logs == nil {
		return fmt.Errorf("%w: no Pod log reader is configured", execution.ErrNotConfigured)
	}
	rec, err := e.recordOf(ctx, w)
	if err != nil {
		return err
	}
	exp, err := e.policy.Expect(rec)
	if err != nil {
		return err
	}
	v, err := e.view(ctx, rec, exp)
	switch {
	case errors.Is(err, errJobAbsent):
		return fmt.Errorf("%w: %w", execution.ErrNotFound, err)
	case err != nil:
		return conflict(err)
	case len(v.Pods) == 0:
		return fmt.Errorf("%w: the Job has no Pod yet", execution.ErrNotFound)
	}
	pod := v.Pods[len(v.Pods)-1]
	log, err := e.logs.PodLog(ctx, exp.Identity.Namespace, pod.Metadata.Name, PrimaryContainer)
	if err != nil {
		return wrapCause(ErrUnavailable, err, "reading the Pod log")
	}
	_, err = dst.Write(log)
	return err
}

// ---- removal ----

// Remove implements execution.Runtime for a settled attempt. The Job is kept
// for CompletedRetention after settlement (ErrBusy until then), then deleted
// like a stopped workload; once no Job or Pod of a committed UID remains, the
// admission Secret, the recovery record that proves those Pods ours, is
// deleted by its UID. Nothing is ever removed for an unsettled attempt.
func (e *Runtime) Remove(ctx context.Context, w execution.Workload) error {
	rec, err := e.recordOf(ctx, w)
	if err != nil {
		return err
	}
	if !rec.Settled || rec.SettledAt == nil {
		return fmt.Errorf("%w: the attempt is not settled", execution.ErrConflict)
	}
	if e.now().Sub(*rec.SettledAt) < e.opts.CompletedRetention {
		return execution.ErrBusy
	}
	ev, err := e.terminate(ctx, rec)
	if err != nil {
		return err
	}
	if !ev.Stopped {
		return execution.ErrBusy
	}
	return e.deleteAdmission(ctx, rec)
}

// deleteAdmission deletes the attempt's admission Secret once it validates as
// the attempt's own record. A missing Secret is already removed; a foreign one
// is left untouched.
func (e *Runtime) deleteAdmission(ctx context.Context, rec AttemptRecord) error {
	tgt, err := e.policy.Tenant(rec.Scope.TenantID)
	if err != nil {
		return err
	}
	id := Identity{
		Scope: rec.Scope, RunID: rec.RunID, StepID: rec.StepID, AttemptID: rec.AttemptID, JobName: rec.JobName,
		Namespace: tgt.Namespace, ContextDigest: rec.ContextDigest, Fingerprint: rec.SpecFingerprint, Access: rec.Access,
	}
	a, err := e.loadAdmission(ctx, id)
	if err != nil {
		return conflict(err)
	}
	if a == nil {
		return nil
	}
	err = e.cluster.DeleteSecret(ctx, tgt.Namespace, a.secret.Metadata.Name, kubernetes.DeleteOptions{UID: a.secret.Metadata.UID})
	if err != nil && !errors.Is(err, kubernetes.ErrNotFound) && !errors.Is(err, kubernetes.ErrConflict) {
		return wrapCause(ErrUnavailable, err, "deleting the admission Secret")
	}
	return nil
}

// ---- termination ----

// Stop implements execution.Runtime: it deletes the attempt's workload and
// reports Stopped only with affirmative evidence: the Job is absent, no Pod
// controlled by it remains, and any create that might still land has had its
// quiescence. It deletes only verified objects, with UID preconditions and
// foreground propagation, and never touches a same-name object that is not the
// attempt's. The launch evidence is LaunchUnknown unless the workload record or
// the Pods establish more. Deletion is asynchronous: callers repeat Stop.
func (e *Runtime) Stop(ctx context.Context, w execution.Workload) (execution.StopEvidence, error) {
	rec, err := e.recordOf(ctx, w)
	if err != nil {
		return execution.StopEvidence{}, err
	}
	return e.terminate(ctx, rec)
}

// recordOf reads the committed attempt a workload handle names and checks the
// handle against it.
func (e *Runtime) recordOf(ctx context.Context, w execution.Workload) (AttemptRecord, error) {
	if err := w.Validate(); err != nil {
		return AttemptRecord{}, err
	}
	rec, err := e.attemptOf(ctx, w.Scope, w.AttemptID)
	if err != nil {
		return AttemptRecord{}, err
	}
	if w.Name != rec.JobName || (w.UID != "" && rec.ObservedJobUID != "" && w.UID != rec.ObservedJobUID) {
		return AttemptRecord{}, fmt.Errorf("%w: the handle names another workload", execution.ErrConflict)
	}
	return rec, nil
}

func (e *Runtime) terminate(ctx context.Context, rec AttemptRecord) (execution.StopEvidence, error) {
	for i := 0; i < maxRecordRetries; i++ {
		res, err := e.terminateOnce(ctx, rec)
		if !errors.Is(err, errRecordMoved) {
			return res, err
		}
	}
	return execution.StopEvidence{}, wrap(ErrUnavailable, "the workload record kept changing")
}

func (e *Runtime) terminateOnce(ctx context.Context, rec AttemptRecord) (execution.StopEvidence, error) {
	tgt, err := e.policy.Tenant(rec.Scope.TenantID)
	if err != nil {
		return execution.StopEvidence{}, err
	}
	// The expectation may be unavailable (policy drift): the recorded, verified
	// UID still identifies what the executor created.
	exp, expErr := e.policy.Expect(rec)
	if expErr != nil && !errors.Is(expErr, ErrPolicyDrift) {
		return execution.StopEvidence{}, expErr
	}
	id := Identity{
		Scope: rec.Scope, RunID: rec.RunID, StepID: rec.StepID, AttemptID: rec.AttemptID, JobName: rec.JobName,
		Namespace: tgt.Namespace, ContextDigest: rec.ContextDigest, Fingerprint: rec.SpecFingerprint, Access: rec.Access,
	}
	a, err := e.loadAdmission(ctx, id)
	if err != nil {
		return execution.StopEvidence{}, err
	}
	job, found, err := e.readJob(ctx, tgt.Namespace, rec.JobName)
	if err != nil {
		return execution.StopEvidence{}, err
	}
	// Committed UIDs: what the executor's own record and execution's observed
	// start name as the attempt's workload. Either survives the loss of the other
	// (a lost Secret, a record update that never landed).
	uids := committedUIDs(a, rec)
	var ownedUID string
	if found {
		ownedUID = e.deletionEligible(id, exp, expErr, uids, job)
	}
	pods, err := e.podsOf(ctx, tgt.Namespace, rec.JobName)
	if err != nil {
		return execution.StopEvidence{}, err
	}
	// Our Pods are those controlled by our Job's UID: the verified live one or a
	// committed one (the Job may already be gone while its Pods terminate).
	if ownedUID != "" {
		uids = appendUID(uids, ownedUID)
	}
	var ours []kubernetes.Pod
	for _, p := range pods {
		for _, u := range uids {
			if controlledBy(p, rec.JobName, u) {
				ours = append(ours, p)
				break
			}
		}
	}
	// Launch evidence only ever becomes more informative. Committed evidence from
	// execution and from the workload record is never discarded because an object
	// (the Secret, the Pods) is missing now.
	launch := mergeLaunch(committedLaunch(rec), recordLaunch(a))
	if l := launchOf(ours); l != execution.LaunchUnknown {
		launch = mergeLaunch(launch, l)
	}
	// Persist launch evidence before any Pod disappears: it cannot be re-read
	// afterwards.
	if a != nil && launch != execution.LaunchUnknown && a.wl.NoteLaunch(launch) {
		if err := e.save(ctx, a); err != nil {
			return execution.StopEvidence{}, err
		}
	}
	if ownedUID != "" {
		if err := e.deleteJob(ctx, tgt.Namespace, rec.JobName, ownedUID); err != nil {
			return execution.StopEvidence{}, err
		}
		if a != nil {
			v := Verification{attempt: rec.AttemptID, jobName: rec.JobName, uid: ownedUID, ok: true}
			if err := a.wl.BeginDelete(v); err == nil {
				_ = e.save(ctx, a)
			}
		}
		// Foreground deletion is asynchronous: report Stopped only once the Job
		// and its Pods have been read as absent.
		return execution.StopEvidence{Launch: launch}, nil
	}
	if found && ownedUID == "" && len(uids) > 0 {
		// UID replacement: the recorded workload is gone; the object now
		// holding the name is someone else's and stays untouched.
		e.log.Warn("a foreign object holds the Job name", "attempt", string(rec.AttemptID))
	}
	if len(ours) > 0 {
		// Our Pods still exist: a deleting Job's dependents, or Pods outliving a
		// Job that is gone. A Pod controlled by the verified or recorded Job UID
		// is ours to delete (by its own UID); the cluster's garbage collector
		// would remove it eventually, but Stopped is not reported on that hope.
		for _, p := range ours {
			err := e.cluster.DeletePod(ctx, tgt.Namespace, p.Metadata.Name, kubernetes.DeleteOptions{UID: p.Metadata.UID, Propagation: "Background"})
			if err != nil && !errors.Is(err, kubernetes.ErrNotFound) && !errors.Is(err, kubernetes.ErrConflict) {
				return execution.StopEvidence{}, wrapCause(ErrUnavailable, err, "deleting a Pod")
			}
		}
		return execution.StopEvidence{Launch: launch}, nil
	}
	quiescent := true
	if a != nil && a.wl.Phase == PhaseCreateIssued {
		quiescent = !a.wl.IssuedAt.IsZero() && e.now().Sub(a.wl.IssuedAt) >= e.opts.CreateQuiescence
	}
	if a != nil {
		// Affirmative evidence must be captured before ObserveGone moves the
		// record to gone: a record that never issued a create can never hold a
		// workload. It is persisted with the transition, so retries and a
		// restarted controller read it back instead of falling to unknown.
		neverCreated := a.wl.NeverCreated()
		if err := a.wl.ObserveGone(quiescent); err != nil {
			if errors.Is(err, ErrCreateUncertain) {
				return execution.StopEvidence{Launch: launch}, nil
			}
			return execution.StopEvidence{}, err
		}
		if launch == execution.LaunchUnknown && neverCreated {
			launch = execution.LaunchNotStarted
			a.wl.NoteLaunch(launch)
		}
		// A moved record is retried by terminate with the fresh state: evidence is
		// claimed only once it is persisted.
		if err := e.save(ctx, a); err != nil {
			return execution.StopEvidence{}, err
		}
	}
	// With no admission Secret the absence of the Job and its Pods is all that is
	// established: a missing record is not evidence that no create was ever
	// issued, so the launch evidence stays what execution and the Pods showed
	// (possibly unknown). Execution reconciles an unknown launch.
	return execution.StopEvidence{Stopped: true, Launch: launch}, nil
}

// committedUIDs lists the Job UIDs committed data names as the attempt's
// workload: the admission record's confirmed UID and execution's observed UID.
func committedUIDs(a *admission, rec AttemptRecord) []string {
	var out []string
	if a != nil {
		out = appendUID(out, a.wl.JobUID)
	}
	return appendUID(out, rec.ObservedJobUID)
}

func appendUID(list []string, uid string) []string {
	if uid == "" {
		return list
	}
	for _, u := range list {
		if u == uid {
			return list
		}
	}
	return append(list, uid)
}

// committedLaunch is the launch evidence execution committed for the attempt.
func committedLaunch(rec AttemptRecord) execution.LaunchEvidence {
	if rec.Launch == execution.LaunchStarted || rec.StartedAt != nil {
		return execution.LaunchStarted
	}
	if rec.Launch == execution.LaunchNotStarted {
		return execution.LaunchNotStarted
	}
	return execution.LaunchUnknown
}

func recordLaunch(a *admission) execution.LaunchEvidence {
	if a == nil || a.wl.Launch == "" {
		return execution.LaunchUnknown
	}
	return a.wl.Launch
}

// mergeLaunch combines two pieces of evidence conservatively: any evidence that
// the command may have started wins, a definite not_started needs no
// contradiction, and unknown yields to either.
func mergeLaunch(x, y execution.LaunchEvidence) execution.LaunchEvidence {
	switch {
	case x == execution.LaunchStarted || y == execution.LaunchStarted:
		return execution.LaunchStarted
	case x == execution.LaunchNotStarted || y == execution.LaunchNotStarted:
		return execution.LaunchNotStarted
	}
	return execution.LaunchUnknown
}

// deletionEligible returns the UID of a Job the executor may delete, or "".
// Eligible: the object's UID is one the committed data names (and it carries the
// attempt's name, namespace and attempt annotation), or, when no UID was ever
// committed, the object passes full verification against the committed
// expectation.
func (c *core) deletionEligible(id Identity, exp Expectation, expErr error, uids []string, job kubernetes.Job) string {
	if len(uids) > 0 {
		for _, u := range uids {
			if u == job.Metadata.UID && job.Metadata.Name == id.JobName && job.Metadata.Namespace == id.Namespace &&
				job.Metadata.Annotations[AnnAttempt] == string(id.AttemptID) {
				return u
			}
		}
		return ""
	}
	if expErr != nil {
		return ""
	}
	v, err := VerifyJob(exp, job)
	if err != nil {
		return ""
	}
	return v.uid
}

// deleteJob deletes a verified Job by UID with foreground propagation. Absence
// and a changed UID both mean nothing is left to delete under that UID.
func (c *core) deleteJob(ctx context.Context, ns, name, uid string) error {
	err := c.cluster.DeleteJob(ctx, ns, name, kubernetes.DeleteOptions{UID: uid, Propagation: "Foreground"})
	switch {
	case err == nil, errors.Is(err, kubernetes.ErrNotFound), errors.Is(err, kubernetes.ErrConflict):
		return nil
	}
	return wrapCause(ErrUnavailable, err, "deleting the Job")
}

// conflict marks a refusal to act on a foreign object as execution's
// conflict, keeping the Kubernetes classification.
func conflict(err error) error {
	if errors.Is(err, ErrForeignWorkload) {
		return fmt.Errorf("%w: %w", execution.ErrConflict, err)
	}
	return err
}
