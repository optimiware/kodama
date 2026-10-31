package storage_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// This file is a test-only mapping between runtime.RunRecord and the durable
// schema. It is not the PostgreSQL adapter (that is T-182); it exists so the
// real-database evidence can show that every field of the record round-trips
// through the tables the migration adds, written and read as a non-owner role.

var cleanupToColumn = map[runtime.CleanupState]string{
	runtime.CleanupNotStarted: "none", runtime.CleanupPending: "pending",
	runtime.CleanupComplete: "done", runtime.CleanupExhausted: "retained",
}

var waitToColumn = map[runtime.WaitState]string{
	runtime.WaitOpen: "waiting", runtime.WaitResolved: "resolved", runtime.WaitExpired: "expired", runtime.WaitCancelled: "cancelled",
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nilTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// retainedEntry is one entry of runs.retained_resources: the array holds both the
// retained resources and the retained workloads, each tagged.
type retainedEntry struct {
	Resource *runtime.ResourceHandle `json:"resource,omitempty"`
	Workload *runtime.RuntimeBinding `json:"workload,omitempty"`
}

func stepRowID(key domain.StepID) string { return "s-" + string(key) }

func saveRecord(ctx context.Context, tx pgx.Tx, rec runtime.RunRecord) error {
	run, sc := rec.Run, rec.Run.Scope
	status := string(run.Outcome)
	if !run.Settled() {
		status = "running"
		if run.Status() == runtime.StatusQueued {
			status = "queued"
		}
	}
	var finished any
	if run.Settled() {
		finished = time.Now()
	}
	var cancelled any
	if run.CancelRequested {
		cancelled = time.Now()
	}
	var retained []retainedEntry
	for i := range run.Cleanup.Retained {
		retained = append(retained, retainedEntry{Resource: &run.Cleanup.Retained[i]})
	}
	for i := range run.Cleanup.RetainedWorkloads {
		retained = append(retained, retainedEntry{Workload: &run.Cleanup.RetainedWorkloads[i]})
	}
	if retained == nil {
		retained = []retainedEntry{}
	}
	var owner, purpose, expires any
	if c := rec.Claim; c != (runtime.Claim{}) {
		owner, purpose, expires = c.Owner, string(c.Purpose), c.Expires
	}
	if _, err := tx.Exec(ctx, `INSERT INTO runs (tenant_id, project_id, id, pipeline_id, definition_snapshot, status, reason,
  cancel_requested_at, version, finished_at, lease_owner, lease_purpose, lease_fence, lease_expires_at,
  cleanup_state, cleanup_attempts, cleanup_max_attempts, cleanup_reason, retained_resources)
VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19::jsonb)`,
		sc.TenantID, sc.ProjectID, run.ID, run.PipelineID, mustJSON(rec.Plan), status, nilIfEmpty(string(run.Reason)),
		cancelled, int64(run.Version), finished, owner, purpose, int64(rec.Claim.Fence), expires,
		cleanupToColumn[run.Cleanup.State], run.Cleanup.Attempts, run.Cleanup.MaxAttempts, nilIfEmpty(string(run.Cleanup.Reason)),
		mustJSON(retained)); err != nil {
		return fmt.Errorf("insert run: %w", err)
	}

	waits := map[domain.StepID]runtime.Wait{}
	for _, w := range rec.Waits {
		waits[w.StepKey] = w
	}
	for i, s := range run.Steps {
		var image any
		if s.Kind == runtime.StepContainer {
			image = rec.Plan.Steps[i].Workload.Image
		}
		var finishedAt, exit, resCode, resReason, whenResult, whenDigest, whenAt any
		if s.State.Terminal() {
			finishedAt = time.Now()
		}
		if s.Result != (runtime.Result{}) {
			exit, resCode, resReason = s.Result.ExitCode, string(s.Result.Code), nilIfEmpty(string(s.Result.Reason))
		}
		if p := s.Predicate; p.Evaluated {
			whenResult, whenDigest, whenAt = p.Result, p.InputDigest, p.At
		}
		// A wait step is inserted pending and moved together with its wait columns.
		stored := s
		if s.Kind == runtime.StepWait {
			stored.State, stored.Reason, stored.Version = runtime.StepPending, runtime.ReasonNone, 1
			finishedAt = nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO run_steps (tenant_id, project_id, run_id, id, step_key, ordinal, image, step_kind,
  status, reason, version, attempt, finished_at, exit_code, result_code, result_reason,
  when_evaluated, when_result, when_input_digest, when_evaluated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)`,
			sc.TenantID, sc.ProjectID, run.ID, stepRowID(s.Key), s.Key, i, image, string(s.Kind), string(stored.State),
			nilIfEmpty(string(stored.Reason)), int64(stored.Version), s.Attempt, finishedAt, exit, resCode, resReason,
			s.Predicate.Evaluated, whenResult, whenDigest, whenAt); err != nil {
			return fmt.Errorf("insert step %s: %w", s.Key, err)
		}
		w, ok := waits[s.Key]
		if !ok {
			continue
		}
		if err := saveWait(ctx, tx, s, w); err != nil {
			return err
		}
	}

	for _, c := range rec.Commands {
		var attempt, backend any
		if c.Kind.Workload() {
			attempt, backend = c.Binding.Attempt, c.Binding.Backend
		}
		var code, exit, reason any
		if c.Result != (runtime.Result{}) {
			code, exit, reason = string(c.Result.Code), c.Result.ExitCode, nilIfEmpty(string(c.Result.Reason))
		}
		if _, err := tx.Exec(ctx, `INSERT INTO run_commands (tenant_id, project_id, run_id, command_id, step_key, kind, target, spec_digest,
  fence, state, reason, result_code, result_exit_code, result_reason, binding_attempt, binding_backend, recorded_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
			sc.TenantID, sc.ProjectID, run.ID, c.ID, nilIfEmpty(string(c.StepKey)), string(c.Kind), c.Target, nilIfEmpty(c.SpecDigest),
			int64(c.Fence), string(c.State), nilIfEmpty(string(c.Reason)), code, exit, reason, attempt, backend, c.RecordedAt); err != nil {
			return fmt.Errorf("insert command %s: %w", c.ID, err)
		}
	}
	for _, o := range rec.Workloads {
		b := o.Binding
		var code, exit, reason any
		if o.Result != (runtime.Result{}) {
			code, exit, reason = string(o.Result.Code), o.Result.ExitCode, nilIfEmpty(string(o.Result.Reason))
		}
		if _, err := tx.Exec(ctx, `INSERT INTO run_workload_observations (tenant_id, project_id, run_id, step_key, attempt, workload_id,
  spec_digest, backend, fence, phase, result_code, result_exit_code, result_reason, observed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			sc.TenantID, sc.ProjectID, run.ID, b.StepKey, b.Attempt, b.WorkloadID, b.SpecDigest, b.Backend, int64(b.Fence),
			string(o.Phase), code, exit, reason, o.ObservedAt); err != nil {
			return fmt.Errorf("insert workload observation %s: %w", b.WorkloadID, err)
		}
	}
	for _, o := range rec.Resources {
		h := o.Handle
		var ownerProject, handleRun any
		if h.Owner.ProjectID != "" {
			ownerProject = string(h.Owner.ProjectID)
		}
		if h.RunID != "" {
			handleRun = string(h.RunID)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO run_resource_observations (tenant_id, project_id, run_id, kind, name, identity,
  owner_project_id, lifetime, handle_run_id, fence, state, reason, observed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			sc.TenantID, sc.ProjectID, run.ID, string(h.Kind), h.Name, h.Identity, ownerProject, string(h.Lifetime), handleRun,
			int64(h.Fence), string(o.State), nilIfEmpty(string(o.Reason)), o.ObservedAt); err != nil {
			return fmt.Errorf("insert resource observation %s: %w", h.Name, err)
		}
	}
	return nil
}

// saveWait replays the wait's lifecycle as the database sees it: its identity,
// version and assignment are recorded while it is unopened, it opens (step
// waiting), and its decision follows. The version rises at each step.
func saveWait(ctx context.Context, tx pgx.Tx, s runtime.Step, w runtime.Wait) error {
	sc := w.Scope
	update := func(args map[string]any) error {
		keys := make([]string, 0, len(args))
		for k := range args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		vals := []any{sc.TenantID, sc.ProjectID, w.RunID, stepRowID(s.Key)}
		var sets []string
		for _, k := range keys {
			vals = append(vals, args[k])
			cast := ""
			if k == "wait_correlation" || k == "wait_outcomes" {
				cast = "::jsonb"
			}
			sets = append(sets, fmt.Sprintf("%s = $%d%s", k, len(vals), cast))
		}
		if _, err := tx.Exec(ctx, "UPDATE run_steps SET "+joinComma(sets)+" WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 AND id = $4", vals...); err != nil {
			return fmt.Errorf("record wait %s: %w", w.ID, err)
		}
		return nil
	}
	settled := func(args map[string]any) map[string]any {
		args["status"], args["reason"], args["version"] = string(s.State), nilIfEmpty(string(s.Reason)), int64(s.Version)
		if s.State.Terminal() {
			args["finished_at"] = time.Now()
		}
		return args
	}
	decision := func(args map[string]any) map[string]any {
		d := w.Decision
		args["wait_state"] = waitToColumn[w.State]
		args["wait_version"] = int64(w.Version)
		args["wait_outcome"], args["wait_answer"] = nilIfEmpty(d.Outcome), nilIfEmpty(d.Answer)
		args["wait_actor_ref"], args["wait_decided_at"] = nilIfEmpty(d.Actor), d.At
		args["wait_consumed_signal_id"], args["wait_reason"] = nilIfEmpty(string(d.SignalID)), nilIfEmpty(string(d.Reason))
		return args
	}
	if err := update(map[string]any{"wait_id": string(w.ID), "wait_version": int64(1), "wait_assignment": nilIfEmpty(w.Assignment)}); err != nil {
		return err
	}
	if w.State == runtime.WaitResolved {
		d := w.Decision
		if _, err := tx.Exec(ctx, `INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape,
  request_digest, applied, target_run_id, target_step_id, target_wait_id, decision_kind, decision_outcome, decision_answer,
  decision_actor_ref, decision_version, decided_at)
VALUES ($1, $2, 'user-alice', $3, $4, 'decision', repeat('a', 64), true, $5, $6, $7, 'resolved', $8, $9, $10, $11, $12)`,
			sc.TenantID, sc.ProjectID, "key-"+string(w.ID), string(d.SignalID), w.RunID, stepRowID(s.Key), w.ID, d.Outcome,
			nilIfEmpty(d.Answer), d.Actor, int64(d.Version), d.At); err != nil {
			return fmt.Errorf("insert signal record: %w", err)
		}
	}
	switch {
	case w.State == runtime.WaitPending:
		if s.State != runtime.StepPending {
			return update(settled(map[string]any{}))
		}
		return nil
	case w.OpenedAt.IsZero():
		// Cancelled before it opened: no values were ever read.
		return update(settled(decision(map[string]any{})))
	}
	corr := w.Correlation
	if corr == nil {
		corr = map[string]string{}
	}
	open := map[string]any{"status": "waiting", "wait_state": "waiting", "wait_correlation": mustJSON(corr), "wait_request": nilIfEmpty(w.Request),
		"wait_input": w.Spec.Input, "wait_outcomes": mustJSON(w.Spec.Outcomes), "wait_approving": nilIfEmpty(w.Spec.Approving),
		"wait_on_expiry": string(w.Spec.OnExpiry.Effective()), "wait_deadline_at": w.Deadline, "wait_version": int64(w.Version)}
	if !w.State.Closed() {
		return update(open)
	}
	open["wait_version"] = int64(w.Version) - 1
	if err := update(open); err != nil {
		return err
	}
	return update(settled(decision(map[string]any{})))
}

func joinComma(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ", "
		}
		out += v
	}
	return out
}

func loadRecord(ctx context.Context, tx pgx.Tx, sc domain.Scope, id domain.RunID) (runtime.RunRecord, error) {
	var rec runtime.RunRecord
	var (
		planJSON, retainedJSON       []byte
		status, cleanupState         string
		reason, cleanupReason, owner *string
		purpose                      *string
		cancelled, expires           *time.Time
		version, fence               int64
		pipeline                     string
		attempts, maxAttempts        int
	)
	if err := tx.QueryRow(ctx, `SELECT pipeline_id, definition_snapshot, status, reason, cancel_requested_at, version,
  lease_owner, lease_purpose, lease_fence, lease_expires_at, cleanup_state, cleanup_attempts, cleanup_max_attempts,
  cleanup_reason, retained_resources
FROM runs WHERE tenant_id = $1 AND project_id = $2 AND id = $3`, sc.TenantID, sc.ProjectID, id).
		Scan(&pipeline, &planJSON, &status, &reason, &cancelled, &version, &owner, &purpose, &fence, &expires,
			&cleanupState, &attempts, &maxAttempts, &cleanupReason, &retainedJSON); err != nil {
		return rec, err
	}
	if err := json.Unmarshal(planJSON, &rec.Plan); err != nil {
		return rec, err
	}
	run := runtime.Run{Scope: sc, ID: id, PipelineID: domain.PipelineID(pipeline), Version: domain.Version(version),
		CancelRequested: cancelled != nil}
	if o := runtime.Outcome(status); o.Decided() {
		run.Outcome = o
	}
	if reason != nil {
		run.Reason = runtime.Reason(*reason)
	}
	for k, v := range cleanupToColumn {
		if v == cleanupState {
			run.Cleanup.State = k
		}
	}
	run.Cleanup.Attempts, run.Cleanup.MaxAttempts = attempts, maxAttempts
	if cleanupReason != nil {
		run.Cleanup.Reason = runtime.Reason(*cleanupReason)
	}
	var retained []retainedEntry
	if err := json.Unmarshal(retainedJSON, &retained); err != nil {
		return rec, err
	}
	for _, e := range retained {
		if e.Resource != nil {
			run.Cleanup.Retained = append(run.Cleanup.Retained, *e.Resource)
		}
		if e.Workload != nil {
			run.Cleanup.RetainedWorkloads = append(run.Cleanup.RetainedWorkloads, *e.Workload)
		}
	}
	if owner != nil {
		rec.Claim = runtime.Claim{Scope: sc, RunID: id, Owner: *owner, Purpose: runtime.ClaimPurpose(*purpose),
			Fence: runtime.Fence(fence), Expires: expires.UTC()}
	}

	rows, err := tx.Query(ctx, `SELECT step_key, step_kind, status, reason, version, attempt, exit_code, result_code, result_reason,
  when_evaluated, when_result, when_input_digest, when_evaluated_at,
  wait_id, wait_assignment, wait_correlation, wait_request, wait_state, wait_version, wait_deadline_at, wait_outcome,
  wait_answer, wait_actor_ref, wait_decided_at, wait_consumed_signal_id, wait_reason
FROM run_steps WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 ORDER BY ordinal`, sc.TenantID, sc.ProjectID, id)
	if err != nil {
		return rec, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			key, kind, st                                                  string
			reason, resCode, resReason, whenDigest                         *string
			stepVersion                                                    int64
			attempt                                                        int
			exit                                                           *int
			whenEvaluated                                                  bool
			whenResult                                                     *bool
			whenAt                                                         *time.Time
			waitID, assignment, request, waitState, outcome, answer, actor *string
			signal, waitReason                                             *string
			corr                                                           []byte
			waitVersion                                                    *int64
			deadline, decidedAt                                            *time.Time
		)
		if err := rows.Scan(&key, &kind, &st, &reason, &stepVersion, &attempt, &exit, &resCode, &resReason,
			&whenEvaluated, &whenResult, &whenDigest, &whenAt,
			&waitID, &assignment, &corr, &request, &waitState, &waitVersion, &deadline, &outcome, &answer, &actor, &decidedAt, &signal, &waitReason); err != nil {
			return rec, err
		}
		s := runtime.Step{Key: domain.StepID(key), Kind: runtime.StepKind(kind), State: runtime.StepState(st),
			Version: domain.Version(stepVersion), Attempt: attempt}
		if reason != nil {
			s.Reason = runtime.Reason(*reason)
		}
		if resCode != nil {
			s.Result = runtime.Result{Code: runtime.ResultCode(*resCode)}
			if exit != nil {
				s.Result.ExitCode = *exit
			}
			if resReason != nil {
				s.Result.Reason = runtime.Reason(*resReason)
			}
		}
		if whenEvaluated {
			s.Predicate = runtime.PredicateEvaluation{Evaluated: true, Result: *whenResult, InputDigest: *whenDigest, At: whenAt.UTC()}
		}
		run.Steps = append(run.Steps, s)
		if waitID == nil {
			continue
		}
		w := runtime.Wait{Scope: sc, ID: runtime.WaitID(*waitID), RunID: id, StepKey: s.Key, State: runtime.WaitPending,
			Version: domain.Version(*waitVersion)}
		i := slices.IndexFunc(rec.Plan.Steps, func(p runtime.PlanStep) bool { return p.Key == s.Key })
		w.Spec = *rec.Plan.Steps[i].Wait
		if assignment != nil {
			w.Assignment = *assignment
		}
		if waitState != nil {
			for k, v := range waitToColumn {
				if v == *waitState {
					w.State = k
				}
			}
		}
		if deadline != nil {
			w.Deadline = deadline.UTC()
			w.OpenedAt = w.Deadline.Add(-w.Spec.Timeout)
			if err := json.Unmarshal(corr, &w.Correlation); err != nil {
				return rec, err
			}
			if len(w.Correlation) == 0 {
				w.Correlation = nil
			}
			if request != nil {
				w.Request = *request
			}
		}
		if w.State.Closed() {
			d := runtime.Decision{Kind: map[runtime.WaitState]runtime.DecisionKind{runtime.WaitResolved: runtime.DecisionResolved,
				runtime.WaitExpired: runtime.DecisionExpired, runtime.WaitCancelled: runtime.DecisionCancelled}[w.State],
				Version: w.Version, At: decidedAt.UTC()}
			if outcome != nil {
				d.Outcome = *outcome
			}
			if answer != nil {
				d.Answer = *answer
			}
			if actor != nil {
				d.Actor = *actor
			}
			if signal != nil {
				d.SignalID = runtime.SignalID(*signal)
			}
			if waitReason != nil {
				d.Reason = runtime.Reason(*waitReason)
			}
			w.Decision = d
		}
		rec.Waits = append(rec.Waits, w)
	}
	if err := rows.Err(); err != nil {
		return rec, err
	}
	rows.Close()
	rec.Run = run

	crows, err := tx.Query(ctx, `SELECT command_id, step_key, kind, target, spec_digest, fence, state, reason, result_code,
  result_exit_code, result_reason, binding_attempt, binding_backend, recorded_at
FROM run_commands WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 ORDER BY command_id`, sc.TenantID, sc.ProjectID, id)
	if err != nil {
		return rec, err
	}
	defer crows.Close()
	for crows.Next() {
		var (
			cid, kind, target, state                  string
			step, digest, reason, code, resReason, be *string
			fence                                     int64
			exit, attempt                             *int
			at                                        time.Time
		)
		if err := crows.Scan(&cid, &step, &kind, &target, &digest, &fence, &state, &reason, &code, &exit, &resReason, &attempt, &be, &at); err != nil {
			return rec, err
		}
		c := runtime.Command{ID: cid, Scope: sc, RunID: id, Kind: runtime.CommandKind(kind), Target: target, Fence: runtime.Fence(fence),
			State: runtime.CommandState(state), RecordedAt: at.UTC()}
		if step != nil {
			c.StepKey = domain.StepID(*step)
		}
		if digest != nil {
			c.SpecDigest = *digest
		}
		if reason != nil {
			c.Reason = runtime.Reason(*reason)
		}
		if code != nil {
			c.Result = runtime.Result{Code: runtime.ResultCode(*code)}
			if exit != nil {
				c.Result.ExitCode = *exit
			}
			if resReason != nil {
				c.Result.Reason = runtime.Reason(*resReason)
			}
		}
		if c.Kind.Workload() {
			c.Binding = runtime.RuntimeBinding{Scope: sc, RunID: id, StepKey: c.StepKey, Attempt: *attempt, WorkloadID: target,
				SpecDigest: c.SpecDigest, Backend: *be, Fence: c.Fence}
		}
		rec.Commands = append(rec.Commands, c)
	}
	if err := crows.Err(); err != nil {
		return rec, err
	}
	crows.Close()

	wrows, err := tx.Query(ctx, `SELECT step_key, attempt, workload_id, spec_digest, backend, fence, phase, result_code, result_exit_code,
  result_reason, observed_at
FROM run_workload_observations WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 ORDER BY step_key, attempt`, sc.TenantID, sc.ProjectID, id)
	if err != nil {
		return rec, err
	}
	defer wrows.Close()
	for wrows.Next() {
		var (
			step, wl, digest, backend, phase string
			code, resReason                  *string
			attempt                          int
			fence                            int64
			exit                             *int
			at                               time.Time
		)
		if err := wrows.Scan(&step, &attempt, &wl, &digest, &backend, &fence, &phase, &code, &exit, &resReason, &at); err != nil {
			return rec, err
		}
		o := runtime.WorkloadObservation{Binding: runtime.RuntimeBinding{Scope: sc, RunID: id, StepKey: domain.StepID(step), Attempt: attempt,
			WorkloadID: wl, SpecDigest: digest, Backend: backend, Fence: runtime.Fence(fence)},
			Phase: runtime.WorkloadPhase(phase), ObservedAt: at.UTC()}
		if code != nil {
			o.Result = runtime.Result{Code: runtime.ResultCode(*code)}
			if exit != nil {
				o.Result.ExitCode = *exit
			}
			if resReason != nil {
				o.Result.Reason = runtime.Reason(*resReason)
			}
		}
		rec.Workloads = append(rec.Workloads, o)
	}
	if err := wrows.Err(); err != nil {
		return rec, err
	}
	wrows.Close()

	rrows, err := tx.Query(ctx, `SELECT kind, name, identity, owner_project_id, lifetime, handle_run_id, fence, state, reason, observed_at
FROM run_resource_observations WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 ORDER BY kind, name, identity`, sc.TenantID, sc.ProjectID, id)
	if err != nil {
		return rec, err
	}
	defer rrows.Close()
	for rrows.Next() {
		var (
			kind, name, identity, lifetime, state string
			ownerProject, handleRun, reason       *string
			fence                                 int64
			at                                    time.Time
		)
		if err := rrows.Scan(&kind, &name, &identity, &ownerProject, &lifetime, &handleRun, &fence, &state, &reason, &at); err != nil {
			return rec, err
		}
		h := runtime.ResourceHandle{Scope: sc, Owner: domain.Owner{TenantID: sc.TenantID}, Lifetime: runtime.Lifetime(lifetime),
			Kind: runtime.ResourceKind(kind), Name: name, Identity: identity, Fence: runtime.Fence(fence)}
		if ownerProject != nil {
			h.Owner.ProjectID = domain.ProjectID(*ownerProject)
		}
		if handleRun != nil {
			h.RunID = domain.RunID(*handleRun)
		}
		o := runtime.ResourceObservation{Handle: h, State: runtime.ResourceState(state), ObservedAt: at.UTC()}
		if reason != nil {
			o.Reason = runtime.Reason(*reason)
		}
		rec.Resources = append(rec.Resources, o)
	}
	return rec, rrows.Err()
}

// canonical returns the record with its collections in a fixed order, so two
// records compare equal by their JSON encoding whatever order a store reads in.
func canonical(rec runtime.RunRecord) runtime.RunRecord {
	rec.Run = rec.Run.Clone()
	rec.Waits = slices.Clone(rec.Waits)
	rec.Commands = slices.Clone(rec.Commands)
	rec.Workloads = slices.Clone(rec.Workloads)
	rec.Resources = slices.Clone(rec.Resources)
	order := map[domain.StepID]int{}
	for i, s := range rec.Run.Steps {
		order[s.Key] = i
	}
	slices.SortFunc(rec.Waits, func(a, b runtime.Wait) int { return order[a.StepKey] - order[b.StepKey] })
	slices.SortFunc(rec.Commands, func(a, b runtime.Command) int {
		if a.ID < b.ID {
			return -1
		}
		return 1
	})
	slices.SortFunc(rec.Workloads, func(a, b runtime.WorkloadObservation) int {
		if a.Binding.StepKey != b.Binding.StepKey {
			if a.Binding.StepKey < b.Binding.StepKey {
				return -1
			}
			return 1
		}
		return a.Binding.Attempt - b.Binding.Attempt
	})
	slices.SortFunc(rec.Resources, func(a, b runtime.ResourceObservation) int {
		ka := string(a.Handle.Kind) + "/" + a.Handle.Name + "/" + a.Handle.Identity
		kb := string(b.Handle.Kind) + "/" + b.Handle.Name + "/" + b.Handle.Identity
		if ka < kb {
			return -1
		}
		return 1
	})
	return rec
}
