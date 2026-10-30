package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/memdb"
)

// The guards below are the Go counterpart of the PostgreSQL triggers the
// execution tables carry: a terminal row never changes, pinned columns are
// written once, a version moves one step at a time and an attempt is created
// only when every earlier attempt of its step is settled. Updates copy only the
// mutable columns onto the stored row, so a mutation cannot rewrite a pin even
// by accident.

func immutablef(format string, args ...any) error {
	return fmt.Errorf("%w: %s", execution.ErrInvalidTransition, fmt.Sprintf(format, args...))
}

func (s *Store) insertRun(tx memdb.Tx, r *execution.Run) error {
	if _, dup := runsT.Get(tx, runKey(r.Scope, r.ID)); dup {
		return fmt.Errorf("%w: the run exists", execution.ErrConflict)
	}
	cp := cloneRun(*r)
	cp.MarkClean()
	return runsT.Put(tx, cp)
}

func (s *Store) updateRun(tx memdb.Tx, r *execution.Run) error {
	cur, ok := runsT.Get(tx, runKey(r.Scope, r.ID))
	if !ok {
		return execution.ErrNotFound
	}
	if cur.Status.Terminal() {
		return immutablef("a terminal run is immutable")
	}
	if r.Version != cur.Version+1 {
		return execution.ErrStale
	}
	cur.Status, cur.Reason = r.Status, r.Reason
	cur.CancelRequestedAt, cur.CancelReason = cloneTime(r.CancelRequestedAt), r.CancelReason
	cur.FailureIntentAt, cur.FailureReason = cloneTime(r.FailureIntentAt), r.FailureReason
	cur.SourceRetention = r.SourceRetention
	if cur.DeadlineAt != nil && (r.DeadlineAt == nil || !r.DeadlineAt.Equal(*cur.DeadlineAt)) {
		return immutablef("the run deadline is pinned once")
	}
	cur.DeadlineAt = cloneTime(r.DeadlineAt)
	cur.Version, cur.UpdatedAt, cur.StartedAt, cur.FinishedAt = r.Version, r.UpdatedAt, cloneTime(r.StartedAt), cloneTime(r.FinishedAt)
	return runsT.Put(tx, cur)
}

func (s *Store) insertStep(tx memdb.Tx, scope domain.Scope, st *execution.StepRun) error {
	cp, err := cloneStep(*st)
	if err != nil {
		return err
	}
	cp.MarkClean()
	return stepsT.Put(tx, stepRow{Project: scope.ProjectID, Step: cp})
}

func (s *Store) updateStep(tx memdb.Tx, scope domain.Scope, st *execution.StepRun) error {
	row, ok := stepsT.Get(tx, key(string(scope.ProjectID), string(st.RunID), st.ID))
	if !ok {
		return execution.ErrNotFound
	}
	cur := row.Step
	if cur.Status.Terminal() {
		return immutablef("a terminal step run is immutable")
	}
	if st.Version != cur.Version+1 {
		return execution.ErrStale
	}
	if st.AttemptsStarted < cur.AttemptsStarted || st.AttemptsStarted > cur.AttemptsStarted+1 {
		return immutablef("attempts are counted one at a time")
	}
	cur.Status, cur.Reason, cur.NextAttemptAt, cur.GateRef = st.Status, st.Reason, cloneTime(st.NextAttemptAt), st.GateRef
	cur.AttemptsStarted, cur.OutputCommit, cur.Outputs = st.AttemptsStarted, st.OutputCommit, maps.Clone(st.Outputs)
	cur.Version, cur.UpdatedAt, cur.FinishedAt = st.Version, st.UpdatedAt, cloneTime(st.FinishedAt)
	return stepsT.Put(tx, stepRow{Project: scope.ProjectID, Step: cur})
}

func (s *Store) insertAttempt(tx memdb.Tx, a *execution.Attempt) error {
	if _, dup := attemptsT.Get(tx, key(string(a.Scope.ProjectID), string(a.ID))); dup {
		return fmt.Errorf("%w: the attempt exists", execution.ErrConflict)
	}
	if err := attemptShape(a); err != nil {
		return err
	}
	// No attempt is created while an earlier attempt of the same step is
	// unsettled: an uncertain or unterminated attempt blocks replay of its action.
	for _, prev := range collect(tx, attemptsT, func(p execution.Attempt) bool {
		return p.Scope == a.Scope && p.RunID == a.RunID && p.StepID == a.StepID
	}) {
		c, ok := controlsT.Get(tx, key(string(a.Scope.ProjectID), string(prev.ID)))
		if !ok || c.Control.SettledAt == nil {
			return immutablef("an earlier attempt of this step is unsettled")
		}
	}
	cp := cloneAttempt(*a)
	cp.MarkClean()
	return attemptsT.Put(tx, cp)
}

// attemptShape is the Go counterpart of step_attempts_workspace_shape: the
// workspace binding of an attempt is all-or-nothing. No access carries no
// workspace, input commit or expected tip; read carries a workspace and an
// input commit; write also carries the expected tip. An attempt of a source-set
// run (runtime context v3) whose run has no workspace binds none of the three:
// its sources are in its pinned context.
func attemptShape(a *execution.Attempt) error {
	ok := false
	switch a.Access {
	case execution.AccessNone:
		ok = a.WorkspaceID == "" && a.InputCommit == "" && a.ExpectedTip == ""
	case execution.AccessRead:
		ok = (a.WorkspaceID != "" && a.InputCommit != "" && a.ExpectedTip == "") || (a.WorkspaceID == "" && a.InputCommit == "" && a.ExpectedTip == "")
	case execution.AccessWrite:
		ok = (a.WorkspaceID != "" && a.InputCommit != "" && a.ExpectedTip != "") || (a.WorkspaceID == "" && a.InputCommit == "" && a.ExpectedTip == "")
	}
	if !ok {
		return fmt.Errorf("%w: the attempt's workspace binding does not match its access %q", execution.ErrInvalid, a.Access)
	}
	return nil
}

func (s *Store) updateAttempt(tx memdb.Tx, a *execution.Attempt) error {
	cur, ok := attemptsT.Get(tx, key(string(a.Scope.ProjectID), string(a.ID)))
	if !ok {
		return execution.ErrNotFound
	}
	if cur.Status != execution.AttemptRunning {
		return immutablef("a terminal attempt is immutable; late evidence is recorded separately")
	}
	if a.Version != cur.Version+1 {
		return execution.ErrStale
	}
	if cur.WorkloadUID != "" && a.WorkloadUID != cur.WorkloadUID {
		return immutablef("the observed workload UID is immutable")
	}
	if cur.StartedAt != nil && (a.StartedAt == nil || !a.StartedAt.Equal(*cur.StartedAt)) {
		return immutablef("the actual start is immutable")
	}
	cur.Status, cur.Reason, cur.Class, cur.WorkloadUID = a.Status, a.Reason, a.Class, a.WorkloadUID
	cur.ExecutionDeadline, cur.StartedAt, cur.FinishedAt = cloneTime(a.ExecutionDeadline), cloneTime(a.StartedAt), cloneTime(a.FinishedAt)
	cur.ResultHash, cur.Result, cur.AuthorityRevision = a.ResultHash, cloneResult(a.Result), a.AuthorityRevision
	cur.Commits = slices.Clone(a.Commits)
	cur.Version, cur.UpdatedAt = a.Version, a.UpdatedAt
	return attemptsT.Put(tx, cur)
}

func (s *Store) putControl(tx memdb.Tx, scope domain.Scope, run domain.RunID, c *execution.Control, create bool) error {
	k := key(string(scope.ProjectID), string(c.AttemptID))
	cur, ok := controlsT.Get(tx, k)
	if create == ok {
		if create {
			return fmt.Errorf("%w: the attempt bookkeeping exists", execution.ErrConflict)
		}
		return execution.ErrStale
	}
	if ok {
		old := cur.Control
		if (old.RevokedAt != nil && !sameTime(old.RevokedAt, c.RevokedAt)) ||
			(old.SettledAt != nil && !sameTime(old.SettledAt, c.SettledAt)) ||
			(old.TerminationRequestedAt != nil && !sameTime(old.TerminationRequestedAt, c.TerminationRequestedAt)) ||
			(old.TerminationConfirmedAt != nil && !sameTime(old.TerminationConfirmedAt, c.TerminationConfirmedAt)) {
			return immutablef("revocation, termination and settlement are recorded once")
		}
		if old.Launch != execution.LaunchUnknown && c.Launch != old.Launch {
			return immutablef("launch evidence only becomes more informative")
		}
	}
	cp := cloneControl(*c)
	cp.MarkClean()
	return controlsT.Put(tx, controlRow{Project: scope.ProjectID, Run: run, Control: cp})
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// persist writes the aggregate's execution rows: run, steps, attempts and
// bookkeeping, then evidence. History, audit and events follow after the peer
// effects.
func (s *Store) persist(tx memdb.Tx, agg *execution.Aggregate) error {
	scope := agg.Run.Scope
	if agg.Run.Dirty() {
		if err := s.updateRun(tx, agg.Run); err != nil {
			return err
		}
	}
	for _, st := range agg.Steps {
		if st.Dirty() {
			if err := s.updateStep(tx, scope, st); err != nil {
				return err
			}
		}
	}
	created := map[domain.AttemptID]bool{}
	for _, a := range agg.Created {
		created[a.ID] = true
		if err := s.insertAttempt(tx, a); err != nil {
			return err
		}
	}
	for _, a := range agg.Attempts {
		if !created[a.ID] && a.Dirty() {
			if err := s.updateAttempt(tx, a); err != nil {
				return err
			}
		}
	}
	for id, c := range agg.Controls {
		if !c.Dirty() {
			continue
		}
		if err := s.putControl(tx, scope, agg.Run.ID, c, created[id]); err != nil {
			return err
		}
	}
	if err := s.persistShared(tx, agg); err != nil {
		return err
	}
	for _, ev := range agg.Evidence {
		seq, err := nextSeq(tx)
		if err != nil {
			return err
		}
		detail, err := cloneDetail(ev.Detail)
		if err != nil {
			return err
		}
		if err := evidenceT.Put(tx, Evidence{Seq: seq, Scope: scope, Run: agg.Run.ID, Attempt: ev.Attempt, Kind: ev.Kind, Detail: detail, At: ev.OccurredAt}); err != nil {
			return err
		}
	}
	return nil
}

func versionOf(agg *execution.Aggregate, t execution.Transition) domain.Version {
	switch t.Subject {
	case execution.SubjectStep:
		if st := agg.Step(t.StepID); st != nil {
			return st.Version
		}
	case execution.SubjectAttempt:
		if a := agg.Attempt(t.AttemptID); a != nil {
			return a.Version
		}
	case execution.SubjectResource:
		if r := agg.Resource(t.ID); r != nil {
			return r.Version
		}
	}
	return agg.Run.Version
}

func auditActorType(t execution.ActorType) execution.ActorType {
	if t == execution.ActorService {
		return "system"
	}
	return t
}

// writeTransitions records one history row, one audit row and (for runs and
// attempts) one catalog event per transition, in the saving transaction.
func (s *Store) writeTransitions(ctx context.Context, tx memdb.Tx, agg *execution.Aggregate) error {
	actor := execution.ActorFrom(ctx)
	reqID := execution.RequestID(ctx)
	scope := agg.Run.Scope
	for i, t := range agg.Transitions {
		t.Version = versionOf(agg, t)
		seq, err := nextSeq(tx)
		if err != nil {
			return err
		}
		if err := historyT.Put(tx, History{
			Seq: seq, Scope: scope, Run: agg.Run.ID, Subject: t.Subject, SubjectID: t.ID, StepID: t.StepID, From: t.From, To: t.To,
			Reason: t.Reason, RunVersion: agg.Run.Version, Actor: actor, At: t.At,
		}); err != nil {
			return err
		}
		if err := s.writeAudit(tx, agg, t, actor, reqID); err != nil {
			return err
		}
		if err := s.writeEvent(tx, agg, i, t, actor, reqID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) writeAudit(tx memdb.Tx, agg *execution.Aggregate, t execution.Transition, actor execution.Actor, reqID string) error {
	resType, resID, action := "run", string(agg.Run.ID), "run.state_changed"
	md := map[string]any{"from": t.From, "to": t.To, "reason": string(t.Reason)}
	switch t.Subject {
	case execution.SubjectStep:
		action = "run.step_state_changed"
		md["stepId"] = t.StepID
	case execution.SubjectAttempt:
		resType, resID, action = "step_attempt", string(t.AttemptID), "step_attempt.state_changed"
		md["runId"], md["stepId"] = string(agg.Run.ID), t.StepID
	case execution.SubjectResource:
		id, err := execution.ResourceAuditID(agg.Run.Scope, agg.Run.ID, t.ID)
		if err != nil {
			return err
		}
		resType, resID, action = eventsv1.ResourceTypeRunResource, id, eventsv1.TypeRunResourceStateChanged
		md = map[string]any{"runId": string(agg.Run.ID), "key": t.ID, "from": t.FromPhase, "to": t.ToPhase, "reason": string(t.Reason)}
		if r := agg.Resource(t.ID); r != nil {
			md["fingerprint"], md["uidBound"] = r.Fingerprint, r.UID != ""
		}
	}
	if len(reqID) > 256 {
		reqID = reqID[:256]
	}
	seq, err := nextSeq(tx)
	if err != nil {
		return err
	}
	metadata, err := cloneDetail(md)
	if err != nil {
		return err
	}
	return auditT.Put(tx, Audit{
		Seq: seq, Scope: agg.Run.Scope, Action: action, ResourceType: resType, ResourceID: resID, Version: t.Version,
		Actor: execution.Actor{Type: auditActorType(actor.Type), ID: actor.ID}, Reason: string(t.Reason), RequestID: reqID, Metadata: metadata, At: s.now(),
	})
}

func eventID(agg *execution.Aggregate, index int, t execution.Transition) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "execution/%s/%s/%s/%s/%s/%d/%d", agg.Run.Scope.TenantID, agg.Run.Scope.ProjectID, agg.Run.ID, t.Subject, t.ID, t.Version, index))
	return "evt_ex_" + hex.EncodeToString(sum[:20])
}

func (s *Store) writeEvent(tx memdb.Tx, agg *execution.Aggregate, index int, t execution.Transition, actor execution.Actor, reqID string) error {
	var typ string
	var res eventsv1.Resource
	var data any
	from := t.From
	if from == "" {
		from = "none"
	}
	switch t.Subject {
	case execution.SubjectRun:
		if t.From == "" && agg.Run.Purpose == execution.PurposeWork && agg.Run.WorkItemID != "" {
			typ = eventsv1.TypeRunCreated
			res = eventsv1.Resource{Type: "run", ID: string(agg.Run.ID), Version: int64(t.Version)}
			data = eventsv1.RunCreated{WorkItemID: string(agg.Run.WorkItemID), DefinitionID: string(agg.Run.DefinitionID), InputSnapshotDigest: agg.Run.SnapshotDigest}
			break
		}
		typ = eventsv1.TypeRunStateChanged
		res = eventsv1.Resource{Type: "run", ID: string(agg.Run.ID), Version: int64(t.Version)}
		data = eventsv1.RunStateChanged{From: from, To: t.To, Reason: string(t.Reason)}
	case execution.SubjectAttempt:
		typ = eventsv1.TypeStepAttemptStateChange
		res = eventsv1.Resource{Type: "step_attempt", ID: string(t.AttemptID), Version: int64(t.Version)}
		data = eventsv1.StepAttemptStateChanged{RunID: string(agg.Run.ID), StepID: t.StepID, From: from, To: t.To, Reason: string(t.Reason)}
	case execution.SubjectResource:
		r := agg.Resource(t.ID)
		if r == nil {
			return fmt.Errorf("execution: a resource transition names no resource")
		}
		id, err := execution.ResourceAuditID(agg.Run.Scope, agg.Run.ID, t.ID)
		if err != nil {
			return err
		}
		typ = eventsv1.TypeRunResourceStateChanged
		res = eventsv1.Resource{Type: eventsv1.ResourceTypeRunResource, ID: id, Version: int64(t.Version)}
		data = eventsv1.RunResourceStateChanged{
			RunID: string(agg.Run.ID), Kind: string(r.Kind), Key: r.Key, From: t.FromPhase, To: t.ToPhase, Reason: string(t.Reason),
			SpecFingerprint: r.Fingerprint, UIDBound: r.UID != "",
		}
	default:
		return nil // step transitions are history and audit only
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	corr := reqID
	if corr == "" {
		corr = s.opts.NewID("corr")
	}
	at := t.At
	if at.IsZero() {
		at = s.now()
	}
	env := eventsv1.Envelope{
		ID: eventID(agg, index, t), SchemaVersion: eventsv1.SchemaVersion, Type: typ, OccurredAt: at.UTC().Truncate(time.Microsecond),
		TenantID: agg.Run.Scope.TenantID, ProjectID: agg.Run.Scope.ProjectID, Resource: res,
		Actor:         eventsv1.Actor{Type: eventsv1.ActorType(actor.Type), ID: actor.ID},
		CorrelationID: corr, Data: raw,
	}
	if err := s.peers.Events.Append(tx, env); err != nil {
		return fmt.Errorf("execution: append event %s: %w", typ, err)
	}
	return nil
}
