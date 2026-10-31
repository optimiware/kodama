package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// Column vocabularies that differ from the kernel's.
var (
	cleanupToColumn = map[runtime.CleanupState]string{
		runtime.CleanupNotStarted: "none", runtime.CleanupPending: "pending",
		runtime.CleanupComplete: "done", runtime.CleanupExhausted: "retained",
	}
	cleanupFromColumn = invert(cleanupToColumn)

	// A pending wait is a wait step that has not opened: its wait_state is null.
	waitToColumn = map[runtime.WaitState]string{
		runtime.WaitOpen: "waiting", runtime.WaitResolved: "resolved", runtime.WaitExpired: "expired", runtime.WaitCancelled: "cancelled",
	}
	waitFromColumn = invert(waitToColumn)

	decisionKindOf = map[runtime.WaitState]runtime.DecisionKind{
		runtime.WaitResolved: runtime.DecisionResolved, runtime.WaitExpired: runtime.DecisionExpired, runtime.WaitCancelled: runtime.DecisionCancelled,
	}
)

func invert[K, V comparable](m map[K]V) map[V]K {
	out := make(map[V]K, len(m))
	for k, v := range m {
		out[v] = k
	}
	return out
}

// retainedEntry is one entry of runs.retained_resources: the array holds both
// the retained resources and the retained workloads, each tagged.
type retainedEntry struct {
	Resource *runtime.ResourceHandle `json:"resource,omitempty"`
	Workload *runtime.RuntimeBinding `json:"workload,omitempty"`
}

func utc(t time.Time) time.Time { return t.UTC() }

func utcp(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

func str[T ~string](p *string) T {
	if p == nil {
		return ""
	}
	return T(*p)
}

// loadRecord reads the whole run record of (scope, id). With lock it takes the
// run's row lock first, which every writer of the run does, so the rest of the
// record is read after any concurrent writer committed and no writer can change
// it until this transaction ends. A run in another tenant is invisible (row
// level security); a run in another project of the tenant has another project
// predicate: both are not-found.
func loadRecord(ctx context.Context, tx pgx.Tx, scope domain.Scope, id domain.RunID, lock bool) (runtime.RunRecord, error) {
	var rec runtime.RunRecord
	var (
		planJSON, retainedJSON       []byte
		pipeline, status, cleanupSt  string
		reason, cleanupReason, owner *string
		taskID                       *string
		purpose                      *string
		cancelled, expires           *time.Time
		version, fence               int64
		attempts, maxAttempts        int
	)
	q := stmtGetRun
	if lock {
		q = stmtLockRun
	}
	err := tx.QueryRow(ctx, q, scope.TenantID, scope.ProjectID, id).
		Scan(&pipeline, &planJSON, &status, &reason, &cancelled, &version, &owner, &purpose, &fence, &expires,
			&cleanupSt, &attempts, &maxAttempts, &cleanupReason, &retainedJSON, &taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return rec, notFound("run")
	}
	if err != nil {
		return rec, fmt.Errorf("runstate/postgres: read run: %w", err)
	}
	if err := json.Unmarshal(planJSON, &rec.Plan); err != nil {
		return rec, fmt.Errorf("runstate/postgres: decode the plan of run %s: %w", id, err)
	}
	if taskID != nil {
		rec.Task = &runtime.TaskRef{Scope: scope, ID: domain.TaskID(*taskID)}
	}
	run := runtime.Run{Scope: scope, ID: id, PipelineID: domain.PipelineID(pipeline), Version: domain.Version(version),
		CancelRequested: cancelled != nil, Reason: str[runtime.Reason](reason)}
	if o := runtime.Outcome(status); o.Decided() {
		run.Outcome = o
	}
	state, ok := cleanupFromColumn[cleanupSt]
	if !ok {
		return rec, fmt.Errorf("runstate/postgres: run %s has cleanup state %q", id, cleanupSt)
	}
	run.Cleanup = runtime.Cleanup{State: state, Attempts: attempts, MaxAttempts: maxAttempts, Reason: str[runtime.Reason](cleanupReason)}
	var retained []retainedEntry
	if err := json.Unmarshal(retainedJSON, &retained); err != nil {
		return rec, fmt.Errorf("runstate/postgres: decode the retained resources of run %s: %w", id, err)
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
		rec.Claim = runtime.Claim{Scope: scope, RunID: id, Owner: *owner, Purpose: str[runtime.ClaimPurpose](purpose),
			Fence: runtime.Fence(fence), Expires: utcp(expires)}
	}

	if err := loadSteps(ctx, tx, scope, id, &rec, &run); err != nil {
		return rec, err
	}
	rec.Run = run
	if rec.Commands, err = loadCommands(ctx, tx, scope, id); err != nil {
		return rec, err
	}
	if rec.Workloads, err = loadWorkloads(ctx, tx, scope, id); err != nil {
		return rec, err
	}
	if rec.Resources, err = loadResources(ctx, tx, scope, id); err != nil {
		return rec, err
	}
	if rec.Materializations, err = loadMaterializations(ctx, tx, scope, id); err != nil {
		return rec, err
	}
	return rec, nil
}

func loadSteps(ctx context.Context, tx pgx.Tx, scope domain.Scope, id domain.RunID, rec *runtime.RunRecord, run *runtime.Run) error {
	rows, err := tx.Query(ctx, stmtGetSteps, scope.TenantID, scope.ProjectID, id)
	if err != nil {
		return fmt.Errorf("runstate/postgres: read steps: %w", err)
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
			outputsJSON, corr                                              []byte
			waitID, assignment, request, waitState, outcome, answer, actor *string
			signal, waitReason                                             *string
			waitVersion                                                    *int64
			deadline, decidedAt                                            *time.Time
		)
		if err := rows.Scan(&key, &kind, &st, &reason, &stepVersion, &attempt, &exit, &resCode, &resReason,
			&whenEvaluated, &whenResult, &whenDigest, &whenAt, &outputsJSON,
			&waitID, &assignment, &corr, &request, &waitState, &waitVersion, &deadline, &outcome, &answer, &actor, &decidedAt,
			&signal, &waitReason); err != nil {
			return fmt.Errorf("runstate/postgres: scan step: %w", err)
		}
		s := runtime.Step{Key: domain.StepID(key), Kind: runtime.StepKind(kind), State: runtime.StepState(st),
			Reason: str[runtime.Reason](reason), Version: domain.Version(stepVersion), Attempt: attempt}
		if resCode != nil {
			s.Result = runtime.Result{Code: runtime.ResultCode(*resCode), Reason: str[runtime.Reason](resReason)}
			if exit != nil {
				s.Result.ExitCode = *exit
			}
		}
		if whenEvaluated {
			if whenResult == nil || whenDigest == nil || whenAt == nil {
				return fmt.Errorf("runstate/postgres: step %s has an incomplete when evaluation", key)
			}
			s.Predicate = runtime.PredicateEvaluation{Evaluated: true, Result: *whenResult, InputDigest: *whenDigest, At: utcp(whenAt)}
		}
		run.Steps = append(run.Steps, s)

		var values map[string]string
		if err := json.Unmarshal(outputsJSON, &values); err != nil {
			return fmt.Errorf("runstate/postgres: decode the outputs of step %s: %w", key, err)
		}
		if len(values) > 0 {
			rec.Outputs = append(rec.Outputs, runtime.StepOutputs{StepKey: s.Key, Attempt: s.Attempt, Values: values})
		}

		if waitID == nil {
			continue
		}
		pi := slices.IndexFunc(rec.Plan.Steps, func(p runtime.PlanStep) bool { return p.Key == s.Key })
		if pi < 0 || rec.Plan.Steps[pi].Wait == nil || waitVersion == nil {
			return fmt.Errorf("runstate/postgres: wait of step %s does not match the plan", key)
		}
		w := runtime.Wait{Scope: scope, ID: runtime.WaitID(*waitID), RunID: id, StepKey: s.Key, Spec: cloneSpec(*rec.Plan.Steps[pi].Wait),
			Assignment: str[string](assignment), State: runtime.WaitPending, Version: domain.Version(*waitVersion)}
		if waitState != nil {
			state, ok := waitFromColumn[*waitState]
			if !ok {
				return fmt.Errorf("runstate/postgres: wait of step %s has state %q", key, *waitState)
			}
			w.State = state
		}
		if deadline != nil {
			w.Deadline = utcp(deadline)
			w.OpenedAt = w.Deadline.Add(-w.Spec.Timeout)
			if err := json.Unmarshal(corr, &w.Correlation); err != nil {
				return fmt.Errorf("runstate/postgres: decode the correlation of wait %s: %w", *waitID, err)
			}
			if len(w.Correlation) == 0 {
				w.Correlation = nil
			}
			w.Request = str[string](request)
		}
		if w.State.Closed() {
			if decidedAt == nil {
				return fmt.Errorf("runstate/postgres: decided wait %s records no decision time", *waitID)
			}
			w.Decision = runtime.Decision{Kind: decisionKindOf[w.State], Version: w.Version, At: utcp(decidedAt),
				Outcome: str[string](outcome), Answer: str[string](answer), Actor: str[string](actor),
				SignalID: str[runtime.SignalID](signal), Reason: str[runtime.Reason](waitReason)}
		}
		rec.Waits = append(rec.Waits, w)
	}
	return rows.Err()
}

func loadCommands(ctx context.Context, tx pgx.Tx, scope domain.Scope, id domain.RunID) ([]runtime.Command, error) {
	rows, err := tx.Query(ctx, stmtGetCommands, scope.TenantID, scope.ProjectID, id)
	if err != nil {
		return nil, fmt.Errorf("runstate/postgres: read commands: %w", err)
	}
	defer rows.Close()
	var out []runtime.Command
	for rows.Next() {
		var (
			cid, kind, target, state                  string
			step, digest, reason, code, resReason, be *string
			fence                                     int64
			exit, attempt                             *int
			at                                        time.Time
		)
		if err := rows.Scan(&cid, &step, &kind, &target, &digest, &fence, &state, &reason, &code, &exit, &resReason, &attempt, &be, &at); err != nil {
			return nil, fmt.Errorf("runstate/postgres: scan command: %w", err)
		}
		c := runtime.Command{ID: cid, Scope: scope, RunID: id, StepKey: str[domain.StepID](step), Kind: runtime.CommandKind(kind),
			Target: target, SpecDigest: str[string](digest), Fence: runtime.Fence(fence), State: runtime.CommandState(state),
			Reason: str[runtime.Reason](reason), RecordedAt: utc(at)}
		if code != nil {
			c.Result = runtime.Result{Code: runtime.ResultCode(*code), Reason: str[runtime.Reason](resReason)}
			if exit != nil {
				c.Result.ExitCode = *exit
			}
		}
		if c.Kind.Workload() {
			if attempt == nil || be == nil {
				return nil, fmt.Errorf("runstate/postgres: workload command %s has no binding", cid)
			}
			c.Binding = runtime.RuntimeBinding{Scope: scope, RunID: id, StepKey: c.StepKey, Attempt: *attempt, WorkloadID: target,
				SpecDigest: c.SpecDigest, Backend: *be, Fence: c.Fence}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func loadWorkloads(ctx context.Context, tx pgx.Tx, scope domain.Scope, id domain.RunID) ([]runtime.WorkloadObservation, error) {
	rows, err := tx.Query(ctx, stmtGetWorkloads, scope.TenantID, scope.ProjectID, id)
	if err != nil {
		return nil, fmt.Errorf("runstate/postgres: read workload observations: %w", err)
	}
	defer rows.Close()
	var out []runtime.WorkloadObservation
	for rows.Next() {
		var (
			step, wl, digest, backend, phase string
			code, resReason                  *string
			attempt                          int
			fence                            int64
			exit                             *int
			at                               time.Time
		)
		if err := rows.Scan(&step, &attempt, &wl, &digest, &backend, &fence, &phase, &code, &exit, &resReason, &at); err != nil {
			return nil, fmt.Errorf("runstate/postgres: scan workload observation: %w", err)
		}
		o := runtime.WorkloadObservation{Binding: runtime.RuntimeBinding{Scope: scope, RunID: id, StepKey: domain.StepID(step),
			Attempt: attempt, WorkloadID: wl, SpecDigest: digest, Backend: backend, Fence: runtime.Fence(fence)},
			Phase: runtime.WorkloadPhase(phase), ObservedAt: utc(at)}
		if code != nil {
			o.Result = runtime.Result{Code: runtime.ResultCode(*code), Reason: str[runtime.Reason](resReason)}
			if exit != nil {
				o.Result.ExitCode = *exit
			}
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func loadResources(ctx context.Context, tx pgx.Tx, scope domain.Scope, id domain.RunID) ([]runtime.ResourceObservation, error) {
	rows, err := tx.Query(ctx, stmtGetResources, scope.TenantID, scope.ProjectID, id)
	if err != nil {
		return nil, fmt.Errorf("runstate/postgres: read resource observations: %w", err)
	}
	defer rows.Close()
	var out []runtime.ResourceObservation
	for rows.Next() {
		var (
			kind, name, identity, lifetime, state string
			ownerProject, handleRun, reason       *string
			fence                                 int64
			at                                    time.Time
		)
		if err := rows.Scan(&kind, &name, &identity, &ownerProject, &lifetime, &handleRun, &fence, &state, &reason, &at); err != nil {
			return nil, fmt.Errorf("runstate/postgres: scan resource observation: %w", err)
		}
		h := runtime.ResourceHandle{Scope: scope, Owner: domain.Owner{TenantID: scope.TenantID, ProjectID: str[domain.ProjectID](ownerProject)},
			Lifetime: runtime.Lifetime(lifetime), RunID: str[domain.RunID](handleRun), Kind: runtime.ResourceKind(kind),
			Name: name, Identity: identity, Fence: runtime.Fence(fence)}
		out = append(out, runtime.ResourceObservation{Handle: h, State: runtime.ResourceState(state),
			Reason: str[runtime.Reason](reason), ObservedAt: utc(at)})
	}
	return out, rows.Err()
}

func loadMaterializations(ctx context.Context, tx pgx.Tx, scope domain.Scope, id domain.RunID) ([]runtime.MaterializationRecord, error) {
	rows, err := tx.Query(ctx, stmtGetMaterializations, scope.TenantID, scope.ProjectID, id)
	if err != nil {
		return nil, fmt.Errorf("runstate/postgres: read materializations: %w", err)
	}
	defer rows.Close()
	var out []runtime.MaterializationRecord
	for rows.Next() {
		var (
			step, state string
			attempt     int
			issued      time.Time
		)
		if err := rows.Scan(&step, &attempt, &state, &issued); err != nil {
			return nil, fmt.Errorf("runstate/postgres: scan materialization: %w", err)
		}
		// The identity is a pure function of the key (the table enforces it).
		ref, err := runtime.NewMaterializationRef(scope, id, domain.StepID(step), attempt)
		if err != nil {
			return nil, fmt.Errorf("runstate/postgres: materialization of step %s: %w", step, err)
		}
		out = append(out, runtime.MaterializationRecord{Ref: ref, State: runtime.MaterializationState(state), IssuedAt: utc(issued)})
	}
	return out, rows.Err()
}

func cloneSpec(s runtime.WaitSpec) runtime.WaitSpec {
	s.Correlate = maps.Clone(s.Correlate)
	s.Outcomes = slices.Clone(s.Outcomes)
	return s
}
