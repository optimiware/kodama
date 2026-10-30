package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

func jsonb(v any) ([]byte, error) { return json.Marshal(v) }

func (s *Store) insertRun(ctx context.Context, tx pgx.Tx, r *execution.Run) error {
	inputs, err := jsonb(r.Snapshot)
	if err != nil {
		return err
	}
	var spec any
	if r.Specification != nil {
		b, err := jsonb(r.Specification)
		if err != nil {
			return err
		}
		spec = b
	}
	var rev any
	if r.ContentRevision != 0 {
		rev = int64(r.ContentRevision)
	}
	var shared any
	if r.Shared != nil {
		b, err := jsonb(r.Shared)
		if err != nil {
			return err
		}
		shared = b
	}
	_, err = tx.Exec(ctx, `INSERT INTO runs
		(tenant_id, project_id, id, purpose, work_item_id, workspace_id, deployment_id, definition_id, definition_digest, content_revision,
		 specification, source_commit, inputs, snapshot_digest, source_retention, retained_ref_id, requested_by_type, requested_by_id,
		 status, status_reason, cancel_requested_at, cancel_reason, failure_intent_at, failure_reason, version, created_at, updated_at, started_at, finished_at,
		 run_shared, deadline_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31)`,
		string(r.Scope.TenantID), string(r.Scope.ProjectID), string(r.ID), string(r.Purpose), nstr(string(r.WorkItemID)), nstr(r.WorkspaceID),
		nstr(r.DeploymentID), string(r.DefinitionID), r.DefinitionDigest, rev, spec, nstr(r.SourceCommit), inputs, r.SnapshotDigest,
		string(r.SourceRetention), nstr(r.RetainedRefID), string(r.RequestedBy.Type), r.RequestedBy.ID, string(r.Status), string(r.Reason),
		ntm(r.CancelRequestedAt), nstr(string(r.CancelReason)), ntm(r.FailureIntentAt), nstr(string(r.FailureReason)), int64(r.Version),
		micros(r.CreatedAt), micros(r.UpdatedAt), ntm(r.StartedAt), ntm(r.FinishedAt), shared, ntm(r.DeadlineAt))
	return err
}

func (s *Store) insertStep(ctx context.Context, tx pgx.Tx, scope domain.Scope, st *execution.StepRun) error {
	spec, err := execution.EncodeStep(st.Spec)
	if err != nil {
		return err
	}
	outputs, err := jsonb(nonNil(st.Outputs))
	if err != nil {
		return err
	}
	refs, err := jsonb(nonNil(st.ArtifactRefs))
	if err != nil {
		return err
	}
	retry := make([]string, 0, len(st.RetryOn))
	for _, c := range st.RetryOn {
		retry = append(retry, string(c))
	}
	_, err = tx.Exec(ctx, `INSERT INTO step_runs
		(tenant_id, project_id, run_id, step_id, ordinal, spec, action_key, status, reason, next_attempt_at, gate_ref, attempts_started,
		 max_attempts, backoff_seconds, retry_on, output_commit, outputs, artifact_refs, version, created_at, updated_at, finished_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`,
		string(scope.TenantID), string(scope.ProjectID), string(st.RunID), st.ID, st.Ordinal, spec, st.ActionKey, string(st.Status), string(st.Reason),
		ntm(st.NextAttemptAt), nstr(st.GateRef), st.AttemptsStarted, st.MaxAttempts, st.BackoffSeconds, retry, nstr(st.OutputCommit), outputs,
		refs, int64(st.Version), micros(st.CreatedAt), micros(st.UpdatedAt), ntm(st.FinishedAt))
	return err
}

func nonNil[K comparable, V any](m map[K]V) map[K]V {
	if m == nil {
		return map[K]V{}
	}
	return m
}

func (s *Store) updateStep(ctx context.Context, tx pgx.Tx, scope domain.Scope, st *execution.StepRun) error {
	outputs, err := jsonb(nonNil(st.Outputs))
	if err != nil {
		return err
	}
	refs, err := jsonb(nonNil(st.ArtifactRefs))
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE step_runs SET status=$5, reason=$6, next_attempt_at=$7, gate_ref=$8, attempts_started=$9, output_commit=$10,
		outputs=$11, artifact_refs=$12, version=$13, updated_at=$14, finished_at=$15
		WHERE tenant_id=$1 AND project_id=$2 AND run_id=$3 AND step_id=$4 AND version=$13-1`,
		string(scope.TenantID), string(scope.ProjectID), string(st.RunID), st.ID, string(st.Status), string(st.Reason), ntm(st.NextAttemptAt),
		nstr(st.GateRef), st.AttemptsStarted, nstr(st.OutputCommit), outputs, refs, int64(st.Version), micros(st.UpdatedAt), ntm(st.FinishedAt))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return execution.ErrStale
	}
	return nil
}

func (s *Store) insertAttempt(ctx context.Context, tx pgx.Tx, a *execution.Attempt) error {
	_, err := tx.Exec(ctx, `INSERT INTO step_attempts
		(tenant_id, project_id, id, run_id, step_id, ordinal, action_key, status, reason, failure_class, job_name, spec_fingerprint, observed_job_uid,
		 workspace_id, access, input_commit, expected_tip, timeout_seconds, startup_deadline_at, execution_deadline_at, context_digest, context,
		 started_at, finished_at, result_hash, result, authority_revision, version, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30)`,
		string(a.Scope.TenantID), string(a.Scope.ProjectID), string(a.ID), string(a.RunID), a.StepID, a.Ordinal, a.ActionKey, string(a.Status),
		string(a.Reason), nstr(string(a.Class)), a.WorkloadName, a.SpecFingerprint, nstr(a.WorkloadUID), nstr(a.WorkspaceID), string(a.Access),
		nstr(a.InputCommit), nstr(a.ExpectedTip), a.TimeoutSeconds, micros(a.StartupDeadline), ntm(a.ExecutionDeadline), a.ContextDigest,
		string(a.Context), ntm(a.StartedAt), ntm(a.FinishedAt), nil, nil, a.AuthorityRevision, int64(a.Version), micros(a.CreatedAt), micros(a.UpdatedAt))
	return err
}

func (s *Store) updateAttempt(ctx context.Context, tx pgx.Tx, a *execution.Attempt) error {
	var result any
	if a.Result != nil {
		b, err := jsonb(a.Result)
		if err != nil {
			return err
		}
		result = b
	}
	tag, err := tx.Exec(ctx, `UPDATE step_attempts SET status=$4, reason=$5, failure_class=$6, observed_job_uid=$7, execution_deadline_at=$8,
		started_at=$9, finished_at=$10, result_hash=$11, result=$12, authority_revision=$13, version=$14, updated_at=$15
		WHERE tenant_id=$1 AND project_id=$2 AND id=$3 AND version=$14-1`,
		string(a.Scope.TenantID), string(a.Scope.ProjectID), string(a.ID), string(a.Status), string(a.Reason), nstr(string(a.Class)),
		nstr(a.WorkloadUID), ntm(a.ExecutionDeadline), ntm(a.StartedAt), ntm(a.FinishedAt), nstr(a.ResultHash), result, a.AuthorityRevision,
		int64(a.Version), micros(a.UpdatedAt))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return execution.ErrStale
	}
	return nil
}

func (s *Store) insertControl(ctx context.Context, tx pgx.Tx, scope domain.Scope, run domain.RunID, c *execution.Control) error {
	_, err := tx.Exec(ctx, `INSERT INTO attempt_controls
		(tenant_id, project_id, attempt_id, run_id, dispatch_confirmed_at, claim_token, claim_until, revoked_at, termination_requested_at,
		 termination_confirmed_at, launch, effect, reconciled_at, replay_safe, settled_at, step_resolved_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		string(scope.TenantID), string(scope.ProjectID), string(c.AttemptID), string(run), ntm(c.DispatchConfirmedAt), nstr(c.ClaimToken),
		ntm(c.ClaimUntil), ntm(c.RevokedAt), ntm(c.TerminationRequestedAt), ntm(c.TerminationConfirmedAt), string(c.Launch), string(c.Effect),
		ntm(c.ReconciledAt), c.ReplaySafe, ntm(c.SettledAt), ntm(c.StepResolvedAt), s.now())
	return err
}

func (s *Store) updateControl(ctx context.Context, tx pgx.Tx, scope domain.Scope, c *execution.Control) error {
	tag, err := tx.Exec(ctx, `UPDATE attempt_controls SET dispatch_confirmed_at=$4, claim_token=$5, claim_until=$6, revoked_at=$7,
		termination_requested_at=$8, termination_confirmed_at=$9, launch=$10, effect=$11, reconciled_at=$12, replay_safe=$13, settled_at=$14,
		step_resolved_at=$15, updated_at=$16
		WHERE tenant_id=$1 AND project_id=$2 AND attempt_id=$3`,
		string(scope.TenantID), string(scope.ProjectID), string(c.AttemptID), ntm(c.DispatchConfirmedAt), nstr(c.ClaimToken), ntm(c.ClaimUntil),
		ntm(c.RevokedAt), ntm(c.TerminationRequestedAt), ntm(c.TerminationConfirmedAt), string(c.Launch), string(c.Effect), ntm(c.ReconciledAt),
		c.ReplaySafe, ntm(c.SettledAt), ntm(c.StepResolvedAt), s.now())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return execution.ErrStale
	}
	return nil
}

func (s *Store) updateRun(ctx context.Context, tx pgx.Tx, r *execution.Run) error {
	tag, err := tx.Exec(ctx, `UPDATE runs SET status=$4, status_reason=$5, cancel_requested_at=$6, cancel_reason=$7, failure_intent_at=$8,
		failure_reason=$9, source_retention=$10, version=$11, updated_at=$12, started_at=$13, finished_at=$14, deadline_at=$15
		WHERE tenant_id=$1 AND project_id=$2 AND id=$3 AND version=$11-1`,
		string(r.Scope.TenantID), string(r.Scope.ProjectID), string(r.ID), string(r.Status), string(r.Reason), ntm(r.CancelRequestedAt),
		nstr(string(r.CancelReason)), ntm(r.FailureIntentAt), nstr(string(r.FailureReason)), string(r.SourceRetention), int64(r.Version),
		micros(r.UpdatedAt), ntm(r.StartedAt), ntm(r.FinishedAt), ntm(r.DeadlineAt))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return execution.ErrStale
	}
	return nil
}

// persist writes the aggregate's execution rows: run, steps, attempts and
// bookkeeping, then evidence. History, audit and events follow after peer
// effects (see writeTransitions), keeping the documented lock order.
func (s *Store) persist(ctx context.Context, tx pgx.Tx, agg *execution.Aggregate) error {
	scope := agg.Run.Scope
	if agg.Run.Dirty() {
		if err := s.updateRun(ctx, tx, agg.Run); err != nil {
			return err
		}
	}
	for _, st := range agg.Steps {
		if st.Dirty() {
			if err := s.updateStep(ctx, tx, scope, st); err != nil {
				return err
			}
		}
	}
	created := map[domain.AttemptID]bool{}
	for _, a := range agg.Created {
		created[a.ID] = true
		if err := s.insertAttempt(ctx, tx, a); err != nil {
			return err
		}
	}
	for _, a := range agg.Attempts {
		if !created[a.ID] && a.Dirty() {
			if err := s.updateAttempt(ctx, tx, a); err != nil {
				return err
			}
		}
	}
	for id, c := range agg.Controls {
		if !c.Dirty() {
			continue
		}
		if created[id] {
			if err := s.insertControl(ctx, tx, scope, agg.Run.ID, c); err != nil {
				return err
			}
			continue
		}
		if err := s.updateControl(ctx, tx, scope, c); err != nil {
			return err
		}
	}
	if err := s.persistShared(ctx, tx, agg); err != nil {
		return err
	}
	for _, ev := range agg.Evidence {
		detail, err := jsonb(nonNil(ev.Detail))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO attempt_evidence (tenant_id, project_id, id, run_id, attempt_id, kind, detail, occurred_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, string(scope.TenantID), string(scope.ProjectID), s.opts.NewID("evd"), string(agg.Run.ID),
			string(ev.Attempt), string(ev.Kind), detail, micros(ev.OccurredAt)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) markClean(agg *execution.Aggregate) {
	agg.Run.MarkClean()
	for _, st := range agg.Steps {
		st.MarkClean()
	}
	for _, a := range agg.Attempts {
		a.MarkClean()
	}
	for _, c := range agg.Controls {
		c.MarkClean()
	}
	agg.Created = nil
	markCleanShared(agg)
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

func auditActorType(t execution.ActorType) string {
	if t == execution.ActorService {
		return "system"
	}
	return string(t)
}

// writeTransitions records one history row, one audit row and (for runs and
// attempts) one catalog event per transition, in the saving transaction.
func (s *Store) writeTransitions(ctx context.Context, tx pgx.Tx, agg *execution.Aggregate) error {
	actor := execution.ActorFrom(ctx)
	reqID := execution.RequestID(ctx)
	scope := agg.Run.Scope
	for i, t := range agg.Transitions {
		t.Version = versionOf(agg, t)
		from := nstr(t.From)
		if _, err := tx.Exec(ctx, `INSERT INTO run_history (tenant_id, project_id, id, run_id, subject_kind, subject_id, step_id, from_status, to_status, reason,
			run_version, actor_type, actor_id, occurred_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			string(scope.TenantID), string(scope.ProjectID), s.opts.NewID("hst"), string(agg.Run.ID), string(t.Subject), t.ID, nstr(t.StepID), from,
			t.To, string(t.Reason), int64(agg.Run.Version), string(actor.Type), actor.ID, micros(t.At)); err != nil {
			return err
		}
		if err := s.writeAudit(ctx, tx, agg, t, actor, reqID); err != nil {
			return err
		}
		if err := s.writeEvent(ctx, tx, agg, i, t, actor, reqID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) writeAudit(ctx context.Context, tx pgx.Tx, agg *execution.Aggregate, t execution.Transition, actor execution.Actor, reqID string) error {
	scope := agg.Run.Scope
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
		id, err := execution.ResourceAuditID(scope, agg.Run.ID, t.ID)
		if err != nil {
			return err
		}
		resType, resID, action = eventsv1.ResourceTypeRunResource, id, eventsv1.TypeRunResourceStateChanged
		md = map[string]any{"runId": string(agg.Run.ID), "key": t.ID, "from": t.FromPhase, "to": t.ToPhase, "reason": string(t.Reason)}
		if r := agg.Resource(t.ID); r != nil {
			md["fingerprint"], md["uidBound"] = r.Fingerprint, r.UID != ""
		}
	}
	raw, err := json.Marshal(md)
	if err != nil {
		return err
	}
	if len(reqID) > 256 {
		reqID = reqID[:256]
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit (tenant_id, id, project_id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, resource_version, reason, request_id, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		string(scope.TenantID), s.opts.NewID("aud"), string(scope.ProjectID), s.now(), auditActorType(actor.Type), actor.ID, action, resType, resID,
		int64(t.Version), nstr(string(t.Reason)), nstr(reqID), raw)
	return err
}

func eventID(agg *execution.Aggregate, index int, t execution.Transition) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "execution/%s/%s/%s/%s/%s/%d/%d", agg.Run.Scope.TenantID, agg.Run.Scope.ProjectID, agg.Run.ID, t.Subject, t.ID, t.Version, index))
	return "evt_ex_" + hex.EncodeToString(sum[:20])
}

func (s *Store) writeEvent(ctx context.Context, tx pgx.Tx, agg *execution.Aggregate, index int, t execution.Transition, actor execution.Actor, reqID string) error {
	var typ string
	var res eventsv1.Resource
	var data any
	from := t.From
	if from == "" {
		from = "none"
	}
	switch t.Subject {
	case execution.SubjectRun:
		if t.From == "" && agg.Run.Purpose == execution.PurposeWork {
			// Creation: the catalog's run.created with the pinned snapshot digest.
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
	return s.appendEvent(ctx, tx, agg, eventID(agg, index, t), typ, res, data, actor, reqID, t.At)
}

func (s *Store) appendEvent(ctx context.Context, tx pgx.Tx, agg *execution.Aggregate, id, typ string, res eventsv1.Resource, data any, actor execution.Actor, reqID string, at time.Time) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	corr := reqID
	if corr == "" {
		corr = s.opts.NewID("corr")
	}
	if at.IsZero() {
		at = s.now()
	}
	env := eventsv1.Envelope{
		ID: id, SchemaVersion: eventsv1.SchemaVersion, Type: typ, OccurredAt: micros(at),
		TenantID: agg.Run.Scope.TenantID, ProjectID: agg.Run.Scope.ProjectID, Resource: res,
		Actor:         eventsv1.Actor{Type: eventsv1.ActorType(actor.Type), ID: actor.ID},
		CorrelationID: corr, Data: raw,
	}
	if _, err := s.events.Append(ctx, tx, env); err != nil {
		return fmt.Errorf("execution: append event %s: %w", typ, err)
	}
	return nil
}
