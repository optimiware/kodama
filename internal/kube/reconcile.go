package kube

import (
	"context"
	"errors"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// Report counts what one reconciliation pass observed and repaired.
type Report struct {
	Tenants        int
	Attempts       int
	Started        int
	Failed         int
	JobsDeleted    int
	PodsDeleted    int
	SecretsDeleted int
	Skipped        int
	Errors         int
}

func (r *Report) add(o Report) {
	r.Tenants += o.Tenants
	r.Attempts += o.Attempts
	r.Started += o.Started
	r.Failed += o.Failed
	r.JobsDeleted += o.JobsDeleted
	r.PodsDeleted += o.PodsDeleted
	r.SecretsDeleted += o.SecretsDeleted
	r.Skipped += o.Skipped
	r.Errors += o.Errors
}

// Reconciler observes the cluster and reports verified evidence to execution.
// Polling is the recovery mechanism: a pass reads durable desired state and the
// managed cluster resources only, tolerates duplicate and concurrent runs, and
// never infers an outcome from absence of evidence. It reports started and
// failed workloads through Observations; execution decides deadlines, retries
// and settlement. Termination of revoked attempts and removal of settled ones
// are requested by execution through Runtime.Stop and Runtime.Remove; the
// managed-resource scan below is the backstop for late creates and leftovers,
// and it also covers attempts of terminal runs.
type Reconciler struct{ *core }

// NewReconciler validates the dependencies. Observations and Tenants are
// required.
func NewReconciler(d Dependencies, o Options) (*Reconciler, error) {
	c, err := newCore(d, o, false)
	if err != nil {
		return nil, err
	}
	if d.Observations == nil || d.Tenants == nil {
		return nil, wrap(ErrPolicy, "the observation sink and tenant enumeration are required")
	}
	return &Reconciler{c}, nil
}

// Pass runs one bounded, context-aware reconciliation over every tenant the
// policy covers. It continues past per-item failures and returns the first.
func (r *Reconciler) Pass(ctx context.Context) (Report, error) {
	var total Report
	ids, err := r.tenants.TenantIDs(ctx)
	if err != nil {
		return total, wrapCause(ErrUnavailable, err, "tenant enumeration")
	}
	var first error
	for _, t := range ids {
		if _, perr := r.policy.Tenant(t); perr != nil {
			continue // no execution target: nothing of this tenant is managed here
		}
		rep, err := r.PassTenant(ctx, t)
		total.add(rep)
		if err != nil && first == nil {
			first = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return total, first
}

// PassTenant reconciles one tenant.
func (r *Reconciler) PassTenant(ctx context.Context, tenant domain.TenantID) (Report, error) {
	rep := Report{Tenants: 1}
	var first error
	note := func(err error) {
		if err != nil {
			rep.Errors++
			if first == nil {
				first = err
			}
		}
	}
	after := ""
	// known maps deterministic Job names to the unsettled attempts read in this
	// pass: committed evidence the orphan scan may corroborate a claim with.
	known := map[string]AttemptRecord{}
	for {
		page, next, err := r.attempts.Unsettled(ctx, tenant, after, r.opts.Batch)
		if err != nil {
			note(wrapCause(ErrUnavailable, err, "listing attempts"))
			break
		}
		for _, rec := range page {
			if ctx.Err() != nil {
				note(ctx.Err())
				return rep, first
			}
			rep.Attempts++
			known[rec.JobName] = rec
			if err := r.observe(ctx, rec, &rep); err != nil {
				r.log.Warn("attempt observation failed", "attempt", string(rec.AttemptID), "error", err)
				note(err)
			}
		}
		if next == "" {
			break
		}
		after = next
	}
	if err := r.scan(ctx, tenant, known, &rep); err != nil {
		note(err)
	}
	return rep, first
}

// observe reports started and failed evidence for one running, authorized
// attempt. Job completion and exit code zero are never success: only an accepted
// result is.
func (r *Reconciler) observe(ctx context.Context, rec AttemptRecord, rep *Report) error {
	if rec.Status != execution.AttemptRunning || !rec.Live {
		return nil
	}
	exp, err := r.policy.Expect(rec)
	if err != nil {
		rep.Skipped++
		return err
	}
	v, err := r.view(ctx, rec, exp)
	switch {
	case errors.Is(err, errJobAbsent):
		// Absence before a confirmed dispatch is the runtime's redrive to
		// resolve. Absence after confirmation is a lost workload: it is reported
		// (never recreated), with launch evidence only from what is known.
		if !rec.DispatchConfirmed && rec.ObservedJobUID == "" {
			return nil
		}
		launch := execution.LaunchUnknown
		if everStarted(rec) {
			launch = execution.LaunchStarted
		}
		return r.fail(ctx, rec, ReasonWorkloadLost, launch, rep)
	case errors.Is(err, ErrForeignWorkload):
		rep.Skipped++
		return err // a foreign object is neither observed nor adopted
	case err != nil:
		return err
	}
	if v.Started && rec.StartedAt == nil {
		err := r.obs.ObserveAttempt(ctx, rec.Scope, rec.AttemptID, execution.Observation{
			Kind: execution.ObservedStarted, WorkloadName: rec.JobName, SpecFingerprint: rec.SpecFingerprint, WorkloadUID: v.uid,
			ObservedAt: r.now(), Launch: execution.LaunchStarted,
		})
		if err != nil {
			return wrapCause(ErrUnavailable, err, "reporting the start")
		}
		rep.Started++
	}
	if v.Failure != "" {
		return r.fail(ctx, rec, v.Failure, v.Launch, rep)
	}
	return nil
}

// errJobAbsent: the attempt's Job is definitely absent.
var errJobAbsent = errors.New("kube: the Job is absent")

// workloadView is one verified read of an attempt's Job and the Pods it
// controls.
type workloadView struct {
	Assessment
	uid string
	job kubernetes.Job
}

// view reads the attempt's Job and its Pods. It returns errJobAbsent for a
// definitely absent Job and ErrForeignWorkload when the object holding the
// name is not the attempt's workload: it fails verification, or its UID
// conflicts with a recorded one (execution's bound UID or the admission
// record's confirmed UID), which makes it a same-name replacement. Only an
// initial create with no recorded UID accepts the live, verified Job.
func (c *core) view(ctx context.Context, rec AttemptRecord, exp Expectation) (workloadView, error) {
	ns := exp.Identity.Namespace
	job, found, err := c.readJob(ctx, ns, rec.JobName)
	if err != nil {
		return workloadView{}, err
	}
	if !found {
		return workloadView{}, errJobAbsent
	}
	v, err := VerifyJob(exp, job)
	if err != nil {
		return workloadView{}, err
	}
	if rec.ObservedJobUID != "" && rec.ObservedJobUID != v.uid {
		return workloadView{}, wrap(ErrForeignWorkload, "the Job's UID conflicts with the attempt's observed workload")
	}
	adm, err := c.loadAdmission(ctx, exp.Identity)
	if err != nil {
		return workloadView{}, err
	}
	if adm != nil && adm.wl.JobUID != "" && adm.wl.JobUID != v.uid {
		return workloadView{}, wrap(ErrForeignWorkload, "the Job's UID conflicts with the confirmed workload")
	}
	pods, err := c.podsOf(ctx, ns, rec.JobName)
	if err != nil {
		return workloadView{}, err
	}
	var ours []kubernetes.Pod
	for _, p := range pods {
		if controlledBy(p, rec.JobName, v.uid) {
			ours = append(ours, p)
		}
	}
	return workloadView{Assessment: assess(job, ours, everStarted(rec)), uid: v.uid, job: job}, nil
}

// everStarted reports the started evidence execution already committed.
func everStarted(rec AttemptRecord) bool {
	return rec.StartedAt != nil || rec.Launch == execution.LaunchStarted
}

func (r *Reconciler) fail(ctx context.Context, rec AttemptRecord, reason execution.Reason, launch execution.LaunchEvidence, rep *Report) error {
	err := r.obs.ObserveAttempt(ctx, rec.Scope, rec.AttemptID, execution.Observation{
		Kind: execution.ObservedFailed, WorkloadName: rec.JobName, SpecFingerprint: rec.SpecFingerprint,
		ObservedAt: r.now(), Class: execution.ClassInfrastructure, Reason: reason, Launch: launch,
	})
	if err != nil {
		return wrapCause(ErrUnavailable, err, "reporting the failure")
	}
	rep.Failed++
	return nil
}

// ---- managed-resource scan (late creates, leftovers, orphans) ----

const managedSelector = LabelManagedBy + "=" + ValueManagedBy

func (r *Reconciler) scan(ctx context.Context, tenant domain.TenantID, known map[string]AttemptRecord, rep *Report) error {
	tgt, err := r.policy.Tenant(tenant)
	if err != nil {
		return err
	}
	var first error
	note := func(err error) {
		if err != nil {
			rep.Errors++
			if first == nil {
				first = err
			}
		}
	}
	jobs, err := r.cluster.ListJobs(ctx, tgt.Namespace, managedSelector)
	if err != nil {
		note(wrapCause(ErrUnavailable, err, "listing managed Jobs"))
	}
	for _, j := range jobs {
		note(r.scanJob(ctx, tenant, tgt, j, rep))
	}
	pods, err := r.cluster.ListPods(ctx, tgt.Namespace, managedSelector)
	if err != nil {
		note(wrapCause(ErrUnavailable, err, "listing managed Pods"))
	}
	for _, p := range pods {
		note(r.scanPod(ctx, tenant, tgt, p, known, rep))
	}
	secrets, err := r.cluster.ListSecrets(ctx, tgt.Namespace, managedSelector)
	if err != nil {
		note(wrapCause(ErrUnavailable, err, "listing managed Secrets"))
	}
	for _, s := range secrets {
		note(r.scanSecret(ctx, tenant, tgt, s, rep))
	}
	return first
}

// ownedIdentity extracts the claimed attempt of a managed object and proves the
// claim is internally consistent: the annotation tenant is the namespace's
// tenant, and the name is the deterministic name of the claimed attempt. A
// claim is only a candidate; committed data decides.
func ownedIdentity(tenant domain.TenantID, meta kubernetes.ObjectMeta, secret bool) (domain.Scope, domain.AttemptID, string, bool) {
	if meta.Annotations[AnnTenant] != string(tenant) || meta.Labels[LabelManagedBy] != ValueManagedBy {
		return domain.Scope{}, "", "", false
	}
	scope := domain.Scope{TenantID: tenant, ProjectID: domain.ProjectID(meta.Annotations[AnnProject])}
	attempt := domain.AttemptID(meta.Annotations[AnnAttempt])
	if scope.Validate() != nil || attempt.Validate() != nil {
		return domain.Scope{}, "", "", false
	}
	job := execution.WorkloadName(tenant, attempt)
	name := job
	if secret {
		name = SecretName(job)
	}
	if meta.Name != name {
		return domain.Scope{}, "", "", false
	}
	return scope, attempt, job, true
}

func (r *Reconciler) aged(meta kubernetes.ObjectMeta) bool {
	t, err := time.Parse(time.RFC3339, meta.CreationTimestamp)
	return err == nil && r.now().Sub(t) >= r.opts.OrphanGrace
}

// lookup reads the committed attempt of a managed object. found=false only for
// an authoritative NotFound; an outage is an error and never orphan evidence.
func (r *Reconciler) lookup(ctx context.Context, scope domain.Scope, id domain.AttemptID) (AttemptRecord, bool, error) {
	rec, err := r.attempts.Attempt(ctx, scope, id)
	switch {
	case err == nil:
		return rec, true, nil
	case errors.Is(err, ErrNotFound):
		return AttemptRecord{}, false, nil
	}
	return AttemptRecord{}, false, wrapCause(ErrUnavailable, err, "reading the attempt")
}

func (r *Reconciler) scanJob(ctx context.Context, tenant domain.TenantID, tgt TenantTarget, j kubernetes.Job, rep *Report) error {
	scope, id, jobName, ok := ownedIdentity(tenant, j.Metadata, false)
	if !ok || len(j.Metadata.OwnerReferences) != 0 {
		rep.Skipped++
		return nil // not provably the executor's: neither adopted nor deleted
	}
	rec, found, err := r.lookup(ctx, scope, id)
	if err != nil {
		return err
	}
	if !found {
		// An authoritative absence of the attempt makes the Job a candidate orphan
		// (for example after a restore), not a proven one: names, labels,
		// annotations and a claimed fingerprint are all copyable. Only the
		// executor's own recovery record, which previously confirmed this very UID,
		// corroborates the claim. Young objects are spared.
		if !r.aged(j.Metadata) || j.Metadata.UID == "" {
			return nil
		}
		a, err := r.claimedAdmission(ctx, tgt, j.Metadata, scope, id, jobName)
		if err != nil {
			return err
		}
		if a == nil || a.wl.JobUID == "" || a.wl.JobUID != j.Metadata.UID {
			r.unverified(rep, "Job", j.Metadata.Name)
			return nil
		}
		return r.removeJob(ctx, tgt, jobName, j.Metadata.UID, rep)
	}
	if rec.Live {
		return nil
	}
	exp, expErr := r.policy.Expect(rec)
	aid := Identity{
		Scope: rec.Scope, RunID: rec.RunID, StepID: rec.StepID, AttemptID: rec.AttemptID, JobName: rec.JobName,
		Namespace: tgt.Namespace, ContextDigest: rec.ContextDigest, Fingerprint: rec.SpecFingerprint, Access: rec.Access, Shared: rec.Shared != nil,
	}
	a, err := r.loadAdmission(ctx, aid)
	if err != nil {
		return err
	}
	// Committed evidence decides: the executor's own record and execution's
	// observed UID both name the attempt's workload, and either survives the loss
	// of the other. A same-name Job with another UID is a replacement, never a late
	// create, even when it passes full verification and the Secret is gone. Only
	// when no UID was ever committed does a Job that verifies against the committed
	// recipe count as a late create.
	uid := r.deletionEligible(aid, exp, expErr, committedUIDs(a, rec), j)
	if uid == "" {
		rep.Skipped++
		return nil
	}
	switch {
	case rec.Status == execution.AttemptSucceeded:
		// Retained until the accepted result is settled and the retention window
		// passes: no TTL controller races result or source finalization.
		if !rec.Settled || rec.SettledAt == nil || r.now().Sub(*rec.SettledAt) < r.opts.CompletedRetention {
			return nil
		}
	default:
		// A failed or cancelled attempt's Job is deleted by execution's termination
		// request. Anything still here after termination was confirmed (or the
		// attempt settled) is a late create or a leftover.
		if !rec.TerminationConfirmed && !rec.Settled {
			return nil
		}
	}
	// A late create was never adopted, so no record names its UID yet. Record it
	// before the Job goes: its Pods may outlive it, and only the recorded UID lets
	// a later pass prove them ours. Execution's observed UID already names one, in
	// which case the eligible UID is that one.
	if a != nil && a.wl.JobUID == "" {
		if err := a.wl.BeginDelete(Verification{attempt: rec.AttemptID, jobName: rec.JobName, uid: uid, ok: true}); err == nil {
			if err := r.save(ctx, a); err != nil {
				return err // retried by the next pass with the fresh record
			}
		}
	}
	return r.removeJob(ctx, tgt, jobName, uid, rep)
}

// unverified reports a candidate whose ownership could not be corroborated: it
// is left untouched and surfaced for investigation.
func (r *Reconciler) unverified(rep *Report, kind, name string) {
	rep.Skipped++
	r.log.Warn("managed-looking object is not corroborated by a recovery record and was left untouched", "kind", kind, "name", name)
}

// claimedIdentity builds the identity an object claims through its annotations.
// It is only a claim: the admission Secret is checked against it, so Secret and
// object must agree, and the record's confirmed UID must match the object's.
func claimedIdentity(tgt TenantTarget, meta kubernetes.ObjectMeta, scope domain.Scope, id domain.AttemptID, jobName string) Identity {
	return Identity{
		Scope: scope, RunID: domain.RunID(meta.Annotations[AnnRun]), StepID: meta.Annotations[AnnStep], AttemptID: id, JobName: jobName,
		Namespace: tgt.Namespace, ContextDigest: meta.Annotations[AnnContextDigest], Fingerprint: meta.Annotations[AnnFingerprint],
	}
}

// claimedAdmission reads the attempt's admission Secret for an object whose
// committed attempt is absent. A missing or inconsistent Secret yields nil: the
// object stays unverified.
func (r *Reconciler) claimedAdmission(ctx context.Context, tgt TenantTarget, meta kubernetes.ObjectMeta, scope domain.Scope, id domain.AttemptID, jobName string) (*admission, error) {
	a, err := r.loadAdmission(ctx, claimedIdentity(tgt, meta, scope, id, jobName))
	if err != nil {
		if errors.Is(err, ErrForeignWorkload) {
			return nil, nil
		}
		return nil, err
	}
	return a, nil
}

func (r *Reconciler) removeJob(ctx context.Context, tgt TenantTarget, name, uid string, rep *Report) error {
	if err := r.deleteJob(ctx, tgt.Namespace, name, uid); err != nil {
		return err
	}
	rep.JobsDeleted++
	return nil
}

func (r *Reconciler) scanPod(ctx context.Context, tenant domain.TenantID, tgt TenantTarget, p kubernetes.Pod, known map[string]AttemptRecord, rep *Report) error {
	// A Pod is a candidate only when the Job-controller owner reference names the
	// deterministic Job its Job label claims.
	label := p.Metadata.Labels[LabelJob]
	var owner kubernetes.OwnerReference
	for _, o := range p.Metadata.OwnerReferences {
		if o.Controlled() {
			owner = o
		}
	}
	if label == "" || owner.Kind != "Job" || owner.Name != label || owner.UID == "" {
		rep.Skipped++
		return nil
	}
	// Pods of their live Job go with the Job. Only Pods whose controlling Job UID
	// no longer exists are leftovers; a same-name Job with another UID does not
	// own them.
	job, found, err := r.readJob(ctx, tgt.Namespace, label)
	if err != nil {
		return err
	}
	if found && job.Metadata.UID == owner.UID {
		return nil
	}
	if !r.aged(p.Metadata) || p.Metadata.UID == "" {
		return nil
	}
	// Labels, an owner reference and age are copyable and prove nothing: the Pod
	// is deleted only when committed data names its controlling Job UID.
	rec, hasRec, verified, err := r.podOwner(ctx, tenant, tgt, label, owner.UID, known)
	if err != nil {
		return err
	}
	if !verified {
		r.unverified(rep, "Pod", p.Metadata.Name)
		return nil
	}
	if hasRec && rec.Live {
		return nil // execution revokes the attempt and requests termination
	}
	err = r.cluster.DeletePod(ctx, tgt.Namespace, p.Metadata.Name, kubernetes.DeleteOptions{UID: p.Metadata.UID, Propagation: "Background"})
	if err != nil && !errors.Is(err, kubernetes.ErrNotFound) && !errors.Is(err, kubernetes.ErrConflict) {
		return wrapCause(ErrUnavailable, err, "deleting a leftover Pod")
	}
	rep.PodsDeleted++
	return nil
}

// podOwner decides whether committed data names ownerUID as the controlling Job
// of the attempt behind jobName: execution's observed UID, or the confirmed UID
// in the attempt's validated recovery record. A failed lookup is an error and
// never evidence.
func (r *Reconciler) podOwner(ctx context.Context, tenant domain.TenantID, tgt TenantTarget, jobName, ownerUID string, known map[string]AttemptRecord) (AttemptRecord, bool, bool, error) {
	if rec, ok := known[jobName]; ok && rec.ObservedJobUID == ownerUID && execution.WorkloadName(tenant, rec.AttemptID) == jobName {
		return rec, true, true, nil
	}
	s, err := r.cluster.GetSecret(ctx, tgt.Namespace, SecretName(jobName))
	switch {
	case errors.Is(err, kubernetes.ErrNotFound):
		return AttemptRecord{}, false, false, nil
	case err != nil:
		return AttemptRecord{}, false, false, wrapCause(ErrUnavailable, err, "reading the admission Secret")
	}
	scope, id, name, ok := ownedIdentity(tenant, s.Metadata, true)
	if !ok || name != jobName || len(s.Metadata.OwnerReferences) != 0 {
		return AttemptRecord{}, false, false, nil
	}
	rec, hasRec, err := r.lookup(ctx, scope, id)
	if err != nil {
		return AttemptRecord{}, false, false, err
	}
	ident := claimedIdentity(tgt, s.Metadata, scope, id, jobName)
	if hasRec {
		ident = Identity{
			Scope: rec.Scope, RunID: rec.RunID, StepID: rec.StepID, AttemptID: rec.AttemptID, JobName: rec.JobName,
			Namespace: tgt.Namespace, ContextDigest: rec.ContextDigest, Fingerprint: rec.SpecFingerprint, Access: rec.Access, Shared: rec.Shared != nil,
		}
	}
	a, err := r.admissionOf(ident, s)
	if err != nil {
		if errors.Is(err, ErrForeignWorkload) {
			return AttemptRecord{}, false, false, nil
		}
		return AttemptRecord{}, false, false, err
	}
	for _, u := range committedUIDs(a, rec) {
		if u == ownerUID {
			return rec, hasRec, true, nil
		}
	}
	return AttemptRecord{}, false, false, nil
}

func (r *Reconciler) scanSecret(ctx context.Context, tenant domain.TenantID, tgt TenantTarget, s kubernetes.Secret, rep *Report) error {
	scope, id, jobName, ok := ownedIdentity(tenant, s.Metadata, true)
	if !ok || len(s.Metadata.OwnerReferences) != 0 {
		rep.Skipped++
		return nil
	}
	rec, found, err := r.lookup(ctx, scope, id)
	if err != nil {
		return err
	}
	// The Job must be definitely absent: an outage proves nothing.
	_, jobFound, err := r.readJob(ctx, tgt.Namespace, jobName)
	if err != nil {
		return err
	}
	if jobFound {
		return nil
	}
	if found {
		// The Secret holds the create-recovery record: it stays while the attempt
		// is unsettled, because a create may still land or be redriven.
		if rec.Live || !rec.Settled {
			return nil
		}
	} else if !r.aged(s.Metadata) {
		return nil
	}
	if s.Metadata.UID == "" {
		return nil
	}
	// The record is the only proof that Pods outliving their Job are the
	// attempt's: it stays until none controlled by a committed UID remain. The
	// Secret is deleted only once it validates as this attempt's own recovery
	// record (labels, annotations, fingerprint, content and record): a foreign,
	// forged or malformed candidate is left untouched and surfaced, never treated
	// as absence of the record.
	ident := claimedIdentity(tgt, s.Metadata, scope, id, jobName)
	if found {
		ident = Identity{
			Scope: rec.Scope, RunID: rec.RunID, StepID: rec.StepID, AttemptID: rec.AttemptID, JobName: rec.JobName,
			Namespace: tgt.Namespace, ContextDigest: rec.ContextDigest, Fingerprint: rec.SpecFingerprint, Access: rec.Access, Shared: rec.Shared != nil,
		}
	}
	a, aerr := r.admissionOf(ident, s)
	if aerr != nil {
		r.unverified(rep, "Secret", s.Metadata.Name)
		return nil
	}
	pods, err := r.podsOf(ctx, tgt.Namespace, jobName)
	if err != nil {
		return err
	}
	for _, p := range pods {
		for _, u := range committedUIDs(a, rec) {
			if controlledBy(p, jobName, u) {
				return nil
			}
		}
	}
	err = r.cluster.DeleteSecret(ctx, tgt.Namespace, s.Metadata.Name, kubernetes.DeleteOptions{UID: s.Metadata.UID})
	if err != nil && !errors.Is(err, kubernetes.ErrNotFound) && !errors.Is(err, kubernetes.ErrConflict) {
		return wrapCause(ErrUnavailable, err, "deleting the admission Secret")
	}
	rep.SecretsDeleted++
	return nil
}
