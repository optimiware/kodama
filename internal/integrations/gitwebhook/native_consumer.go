package gitwebhook

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync/atomic"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// DefaultInboxLease bounds how long a consumer's attempt excludes another
// consumer's attempt at the same key. Admission itself is idempotent on the run
// ID, so the lease only limits contention.
const DefaultInboxLease = 2 * time.Minute

// ConsumerDependencies are the collaborators of PushConsumer and TaskRouter.
// Work is required by TaskRouter only, Pushes by PushConsumer only.
type ConsumerDependencies struct {
	Store  NativeStore
	Pushes PushRecords
	Work   WorkItems
	Runs   SourceRuns
	// Holder identifies this consumer instance in inbox leases; it must be unique
	// per process (the composition root supplies one).
	Holder string
	Lease  time.Duration
	Now    func() time.Time
}

func (d *ConsumerDependencies) defaults() {
	if d.Lease <= 0 {
		d.Lease = DefaultInboxLease
	}
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
}

// Admission is the result of one (trigger, push, ref) or one task-ready key.
type Admission struct {
	// Source is the trigger ID (push) or the project (task route).
	Source string
	// Ref is the pushed ref; empty for a task route.
	Ref     string
	Outcome Outcome
	Reason  string
	RunID   domain.RunID
	// Replay is true when the key was already decided by an earlier delivery.
	Replay bool
}

// ---- shared admission procedure ----

type admitter struct {
	d ConsumerDependencies
}

// attemptSeq makes every admission attempt its own lease holder, so two
// attempts of one process (a redelivery racing a slow first attempt) exclude
// each other exactly like attempts of two processes.
var attemptSeq atomic.Uint64

// refusalCoder is implemented by a refusal that carries a stable, sanitized
// reason code (adapters.NativeRuns).
type refusalCoder interface{ RefusalCode() string }

var refusalCodeSyntax = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// RefusalCode is the stable reason code recorded for a permanent admission
// refusal: the code the refusal carries when it is a well-formed code (never a
// message, name, identifier or secret), otherwise ReasonAdmissionRefused.
func RefusalCode(err error) string {
	var c refusalCoder
	if errors.As(err, &c) && refusalCodeSyntax.MatchString(c.RefusalCode()) {
		return c.RefusalCode()
	}
	return ReasonAdmissionRefused
}

// buildFunc produces the admission request after the key is claimed, or an
// ignore reason when the key must not start a run. ErrNotFound from it is a
// recorded ignore; any other error is an outage.
type buildFunc func(ctx context.Context) (req SourceRunRequest, ignore string, err error)

// admit runs one key to its terminal outcome: claim the key, recover a run that
// already exists, otherwise admit it, and record the write-once outcome with its
// audit. A retryable failure releases the claim and returns ErrUnavailable; the
// redelivered event resumes the same key, and the stable run ID makes the
// admission itself a replay, never a second run.
func (a *admitter) admit(ctx context.Context, key InboxKey, audit auditNames, meta map[string]string, build buildFunc) (Admission, error) {
	out := Admission{Source: key.Source, Ref: key.Ref}
	if err := key.Validate(); err != nil {
		return out, err
	}
	now := a.d.Now().UTC()
	holder := fmt.Sprintf("%s#%d", a.d.Holder, attemptSeq.Add(1))
	rec, acquired, err := a.d.Store.ClaimInbox(ctx, key, holder, now, a.d.Lease)
	if err != nil {
		return out, nativeStoreError(err)
	}
	if rec.Done {
		return fromRecord(out, rec, true), nil
	}
	if !acquired {
		return out, fmt.Errorf("%w: another consumer is admitting this key", ErrAdmissionBusy)
	}
	runID := key.RunID()
	finish := func(f InboxFinish, action string, reason string) (Admission, error) {
		f.Holder = holder
		m := map[string]string{}
		for k, v := range meta {
			m[k] = v
		}
		if f.RunID != "" {
			m["run"] = string(f.RunID)
		}
		rec, err := a.d.Store.FinishInbox(ctx, key, f, a.d.Now().UTC(), AuditRecord{
			Action: action, Scope: key.Scope, TriggerID: TriggerID(key.Source), Reason: reason, Metadata: m, At: a.d.Now().UTC(),
		})
		if errors.Is(err, ErrConflict) {
			// The store fenced this attempt: its lease expired (or was taken
			// over) before it could record a non-started outcome. The attempt
			// that holds the key decides it; a redelivery replays that decision.
			a.release(ctx, key, holder)
			return out, fmt.Errorf("%w: the lease on this key was lost before its outcome was recorded", ErrAdmissionBusy)
		}
		if err != nil {
			a.release(ctx, key, holder)
			return out, nativeStoreError(err)
		}
		return fromRecord(out, rec, false), nil
	}
	started := func() (Admission, error) {
		return finish(InboxFinish{Outcome: OutcomeStarted, RunID: runID}, audit.started, "")
	}
	// A run admitted by an earlier attempt whose result was lost: recover it
	// without any authority or source re-check.
	found, err := a.d.Runs.Find(ctx, key.Scope, runID)
	if err != nil {
		a.release(ctx, key, holder)
		return out, unavailable("run lookup", err)
	}
	if found {
		return started()
	}
	req, ignore, err := build(ctx)
	switch {
	case errors.Is(err, ErrNotFound):
		return finish(InboxFinish{Outcome: OutcomeIgnored, Reason: ReasonNotFoundTarget}, audit.ignored, ReasonNotFoundTarget)
	case err != nil:
		a.release(ctx, key, holder)
		return out, err
	case ignore != "":
		return finish(InboxFinish{Outcome: OutcomeIgnored, Reason: ignore}, audit.ignored, ignore)
	}
	req.Scope, req.RunID = key.Scope, runID
	ref, err := a.d.Runs.Admit(ctx, req)
	switch {
	case err == nil:
		if ref.ID != runID {
			// Admission must honor the requested identity; anything else would
			// break the at-most-one guarantee, so it is never recorded as started.
			a.release(ctx, key, holder)
			return out, unavailable("run admission", fmt.Errorf("admission returned run %q for %q", ref.ID, runID))
		}
		return started()
	case errors.Is(err, ErrRunRefused):
		// A refusal is recorded only if no run exists for the key: the run's own
		// existence wins over a refusal observed by a stalled attempt.
		if found, ferr := a.d.Runs.Find(ctx, key.Scope, runID); ferr != nil {
			a.release(ctx, key, holder)
			return out, unavailable("run lookup", ferr)
		} else if found {
			return started()
		}
		code := RefusalCode(err)
		return finish(InboxFinish{Outcome: OutcomeRejected, Reason: code}, audit.refused, code)
	default:
		a.release(ctx, key, holder)
		if errors.Is(err, ErrUnavailable) {
			return out, err
		}
		return out, unavailable("run admission", err)
	}
}

// release drops the claim after a retryable failure; the lease would also
// expire on its own.
func (a *admitter) release(ctx context.Context, key InboxKey, holder string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = a.d.Store.ReleaseInbox(rctx, key, holder)
}

func fromRecord(a Admission, rec InboxRecord, replay bool) Admission {
	a.Outcome, a.Reason, a.RunID, a.Replay = rec.Outcome, rec.Reason, rec.RunID, replay
	return a
}

type auditNames struct{ started, refused, ignored string }

// ReasonNotFoundTarget is recorded when the object a key names (a work item)
// no longer exists in the scope.
const ReasonNotFoundTarget = "target_not_found"

// ---- push consumer ----

// PushConsumer starts runs from applied pushes (GR-10, GR-11, PX-24, GR-A07).
// It consumes the reference-only git.refs_updated event: the event names a push
// record and says whether a run made it; the record, reloaded by ID, says which
// refs were applied to which commits.
type PushConsumer struct {
	d ConsumerDependencies
	a admitter
}

// NewPushConsumer requires the store, the push record reload and run admission.
func NewPushConsumer(d ConsumerDependencies) (*PushConsumer, error) {
	if d.Store == nil || d.Pushes == nil || d.Runs == nil || d.Holder == "" {
		return nil, errors.New("gitwebhook: native store, push records, run admission and a holder identity are required")
	}
	d.defaults()
	return &PushConsumer{d: d, a: admitter{d: d}}, nil
}

// HandleRefsUpdated processes one git.refs_updated envelope.
//
// A nil error means every start the event can cause is decided (started,
// rejected or ignored) and the delivery may be acknowledged; the results list
// each decision. An error wrapping ErrUnavailable is retryable: redelivery
// resumes the undecided keys and replays the decided ones. An error wrapping
// ErrInvalid or ErrNotFound is a malformed or dangling event that redelivery
// cannot fix.
//
// Per trigger and ref the sequence is: the trigger's repository and ref
// patterns select entries; a push made by a run is excluded from the event
// alone when the trigger excludes origin runs, before anything is reloaded;
// only applied entries that are not deletes start runs, each at most once per
// (trigger, push record, ref) under a run ID derived from that key, and the run
// pins the entry's `new` commit from the record, never a branch tip.
func (c *PushConsumer) HandleRefsUpdated(ctx context.Context, env eventsv1.Envelope) ([]Admission, error) {
	if err := env.Validate(); err != nil {
		return nil, invalidf("event: %v", err)
	}
	if env.Type != eventsv1.TypeGitRefsUpdated {
		return nil, invalidf("event: %s is not %s", env.Type, eventsv1.TypeGitRefsUpdated)
	}
	data, err := eventsv1.DecodeData(env)
	if err != nil {
		return nil, invalidf("event: %v", err)
	}
	ev, ok := data.(*eventsv1.GitRefsUpdated)
	if !ok {
		return nil, invalidf("event: unexpected payload")
	}
	scope := domain.Scope{TenantID: env.TenantID, ProjectID: env.ProjectID}
	if err := scope.Validate(); err != nil {
		return nil, invalidf("event: %v", err)
	}
	triggers, err := c.d.Store.EnabledPushTriggers(ctx, scope, ev.RepositoryID)
	if err != nil {
		return nil, nativeStoreError(err)
	}
	var results []Admission
	candidates := triggers[:0:0]
	for _, t := range triggers {
		switch {
		case t.Config.RepositoryID != ev.RepositoryID || t.Scope != scope:
			continue // a store that returned another repository's or scope's trigger is never obeyed
		case env.OccurredAt.Before(t.EnabledAt):
			// The push settled before the trigger existed or was re-enabled: a late
			// redelivery must not start runs for pushes it was not watching.
			results = append(results, Admission{Source: t.ID(), Outcome: OutcomeIgnored, Reason: ReasonBeforeEnabled})
		case ev.OriginRunID != nil && t.Excludes():
			results = append(results, Admission{Source: t.ID(), Outcome: OutcomeIgnored, Reason: ReasonOriginRunExcluded})
		default:
			candidates = append(candidates, t)
		}
	}
	if len(candidates) == 0 {
		return results, nil
	}
	rec, err := c.d.Pushes.Reload(ctx, scope, ev.RepositoryID, ev.ReceiveID)
	switch {
	case errors.Is(err, ErrNotFound):
		return results, fmt.Errorf("%w: the push record of the event", ErrNotFound)
	case err != nil:
		return results, unavailable("push record", err)
	case rec.RepositoryID != ev.RepositoryID || rec.ReceiveID != ev.ReceiveID:
		return results, invalidf("the reloaded push record is not the event's")
	case rec.Status != gitv1.PushApplied:
		return results, invalidf("the push record is %s, not applied", rec.Status)
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].ID() < candidates[j].ID() })
	var firstErr error
	for _, t := range candidates {
		if rec.OriginRunID != nil && t.Excludes() {
			// The record is authoritative about who pushed: a run's push is
			// excluded even if the event named no run.
			results = append(results, Admission{Source: t.ID(), Outcome: OutcomeIgnored, Reason: ReasonOriginRunExcluded})
			continue
		}
		for _, entry := range rec.Entries {
			if !t.Config.Matches(entry.Ref) {
				continue
			}
			res, err := c.admitEntry(ctx, scope, t, rec, entry, env)
			results = append(results, res)
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return results, firstErr
}

func (c *PushConsumer) admitEntry(ctx context.Context, scope domain.Scope, t PushTrigger, rec gitv1.PushRecord, entry gitv1.PushEntry, env eventsv1.Envelope) (Admission, error) {
	out := Admission{Source: t.ID(), Ref: entry.Ref, Outcome: OutcomeIgnored}
	switch {
	case entry.State != gitv1.EntryApplied:
		out.Reason = ReasonEntryNotApplied
		return out, nil
	case entry.Kind == gitv1.KindDelete || gitv1.IsZeroObjectID(entry.New):
		out.Reason = ReasonRefDeleted
		return out, nil
	}
	commit, err := gitv1.PinnedCommit(rec, entry.Ref)
	if err != nil {
		out.Reason = ReasonEntryNotApplied
		return out, nil
	}
	key := InboxKey{Scope: scope, Kind: InboxPush, Source: t.ID(), Subject: rec.PushID, Ref: entry.Ref}
	meta := map[string]string{"trigger": t.ID(), "push": rec.PushID, "ref": entry.Ref, "commit": commit, "repository": rec.RepositoryID}
	return c.a.admit(ctx, key, auditNames{started: AuditPushRunStarted, refused: AuditPushRunRefused, ignored: AuditPushRunIgnored}, meta,
		func(context.Context) (SourceRunRequest, string, error) {
			return SourceRunRequest{
				DefinitionID:   domain.DefinitionID(t.Config.DefinitionID),
				Origin:         RunOrigin{PushID: rec.PushID, RepositoryID: rec.RepositoryID, Ref: entry.Ref},
				ExpectedCommit: commit,
				Principal:      t.ConfiguredBy,
				RequestID:      env.ID,
			}, "", nil
		})
}

// ---- task routing ----

// TaskRouter starts a work_item run when an item becomes ready, from the
// project's configured route, through the same source-set admission as every
// other origin. Admission is where current authority, quotas, cancellation and
// run idempotency are enforced; the router bypasses none of them and never
// consumes business WIP itself (WIP is the work module's rule on the
// ready-to-active edge).
type TaskRouter struct {
	d ConsumerDependencies
	a admitter
}

// NewTaskRouter requires the store, work item reads and run admission.
func NewTaskRouter(d ConsumerDependencies) (*TaskRouter, error) {
	if d.Store == nil || d.Work == nil || d.Runs == nil || d.Holder == "" {
		return nil, errors.New("gitwebhook: native store, work items, run admission and a holder identity are required")
	}
	d.defaults()
	return &TaskRouter{d: d, a: admitter{d: d}}, nil
}

// StateReady is the work item state that triggers a route.
const StateReady = "ready"

// HandleWorkItemEvent processes a work_item.transitioned (to ready) or
// work_item.created (in ready) envelope. It returns nil, nil for an event that
// does not make an item ready or when the project has no enabled route. Errors
// are classified as in HandleRefsUpdated.
//
// The key is (project route, the readiness transition): an item that leaves and
// re-enters ready is a new transition and so a new run, while a redelivery of
// one transition is a replay. The item's *current* state decides at admission:
// an item that is no longer ready, is blocked, cancelled or not actionable
// starts nothing, and that decision is recorded.
func (r *TaskRouter) HandleWorkItemEvent(ctx context.Context, env eventsv1.Envelope) (*Admission, error) {
	if err := env.Validate(); err != nil {
		return nil, invalidf("event: %v", err)
	}
	var subject string
	switch env.Type {
	case eventsv1.TypeWorkItemTransitioned:
		data, err := eventsv1.DecodeData(env)
		if err != nil {
			return nil, invalidf("event: %v", err)
		}
		p, ok := data.(*eventsv1.WorkItemTransitioned)
		if !ok {
			return nil, invalidf("event: unexpected payload")
		}
		if p.To != StateReady {
			return nil, nil
		}
		subject = "transition:" + p.TransitionID
	case eventsv1.TypeWorkItemCreated:
		data, err := eventsv1.DecodeData(env)
		if err != nil {
			return nil, invalidf("event: %v", err)
		}
		p, ok := data.(*eventsv1.WorkItemCreated)
		if !ok {
			return nil, invalidf("event: unexpected payload")
		}
		if p.State != StateReady {
			return nil, nil
		}
		subject = "created"
	default:
		return nil, nil
	}
	scope := domain.Scope{TenantID: env.TenantID, ProjectID: env.ProjectID}
	if err := scope.Validate(); err != nil {
		return nil, invalidf("event: %v", err)
	}
	item := domain.WorkItemID(env.Resource.ID)
	if err := item.Validate(); err != nil {
		return nil, invalidf("event: %v", err)
	}
	route, err := r.d.Store.TaskRoute(ctx, scope)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, nativeStoreError(err)
	case !route.Enabled || route.Scope != scope:
		return nil, nil
	case env.OccurredAt.Before(route.EnabledAt):
		return &Admission{Source: string(scope.ProjectID), Outcome: OutcomeIgnored, Reason: ReasonBeforeEnabled}, nil
	}
	key := InboxKey{Scope: scope, Kind: InboxTaskReady, Source: string(scope.ProjectID), Subject: string(item) + "/" + subject}
	meta := map[string]string{"work_item": string(item), "definition": string(route.DefinitionID)}
	out, err := r.a.admit(ctx, key, auditNames{started: AuditTaskRunStarted, refused: AuditTaskRunRefused, ignored: AuditTaskRunIgnored}, meta,
		func(ctx context.Context) (SourceRunRequest, string, error) {
			facts, err := r.d.Work.Facts(ctx, scope, item)
			switch {
			case errors.Is(err, ErrNotFound):
				return SourceRunRequest{}, "", ErrNotFound
			case err != nil:
				return SourceRunRequest{}, "", unavailable("work item", err)
			case facts.Cancelled:
				return SourceRunRequest{}, ReasonWorkCancelled, nil
			case facts.State != StateReady:
				return SourceRunRequest{}, ReasonNotReady, nil
			case !facts.Actionable:
				return SourceRunRequest{}, ReasonNotActionable, nil
			case facts.BlockedReason != "":
				return SourceRunRequest{}, ReasonWorkBlocked, nil
			}
			return SourceRunRequest{
				DefinitionID:    route.DefinitionID,
				Origin:          RunOrigin{WorkItemID: item},
				ContentRevision: facts.ContentRevision,
				Principal:       route.ConfiguredBy,
				RequestID:       env.ID,
			}, "", nil
		})
	return &out, err
}
