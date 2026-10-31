package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

func nz(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nt(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func jsonOf(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("runstate/postgres: encode %T: %v", v, err)) // plain data; cannot fail
	}
	return string(b)
}

// runStatus is the runs.status column: the outcome once execution settled, else
// queued until a step moved and running afterwards.
func runStatus(r runtime.Run) string {
	if r.Settled() {
		return string(r.Outcome)
	}
	if r.Status() == runtime.StatusQueued {
		return "queued"
	}
	return "running"
}

// writeRun updates the lifecycle columns of the run row, guarded by the version
// the writer read. at is the time of the change: it stamps a requested
// cancellation and the end of execution once.
func writeRun(ctx context.Context, tx pgx.Tx, prev, next runtime.Run, at time.Time) error {
	if prev.Cleanup.MaxAttempts != next.Cleanup.MaxAttempts {
		return invalid("run %s cannot change its cleanup bound", next.ID)
	}
	retained := make([]retainedEntry, 0, len(next.Cleanup.Retained)+len(next.Cleanup.RetainedWorkloads))
	for i := range next.Cleanup.Retained {
		retained = append(retained, retainedEntry{Resource: &next.Cleanup.Retained[i]})
	}
	for i := range next.Cleanup.RetainedWorkloads {
		retained = append(retained, retainedEntry{Workload: &next.Cleanup.RetainedWorkloads[i]})
	}
	sc := next.Scope
	tag, err := tx.Exec(ctx, stmtUpdateRun, sc.TenantID, sc.ProjectID, next.ID, runStatus(next), nz(string(next.Reason)),
		next.CancelRequested, at, int64(next.Version), next.Settled(), cleanupToColumn[next.Cleanup.State],
		next.Cleanup.Attempts, nz(string(next.Cleanup.Reason)), jsonOf(retained), int64(prev.Version))
	if err != nil {
		return fmt.Errorf("runstate/postgres: update run: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return versionConflict("run %s is no longer at version %d", next.ID, prev.Version)
	}
	return nil
}

// waitColumns holds the wait_* columns of one step row, in the order of
// stmtUpdateStep. A step that is not a wait step has all of them null.
type waitColumns struct {
	assignment, correlation, request, input, outcomes, approving, onExpiry, deadline any
	state, outcome, answer, actor, decidedAt, signal, version, reason                any
}

// waitColumnsOf maps a wait to its columns. A pending wait carries only its
// identity's version and assignment; an opened one also what it read and its
// deadline; a decided one the decision. A wait cancelled before it opened has
// no opened content.
func waitColumnsOf(w *runtime.Wait) waitColumns {
	if w == nil {
		return waitColumns{}
	}
	c := waitColumns{assignment: nz(w.Assignment), version: int64(w.Version)}
	if w.State == runtime.WaitPending {
		return c
	}
	c.state = waitToColumn[w.State]
	if !w.OpenedAt.IsZero() {
		corr := w.Correlation
		if corr == nil {
			corr = map[string]string{}
		}
		c.correlation, c.request, c.input = jsonOf(corr), nz(w.Request), w.Spec.Input
		c.outcomes, c.approving = jsonOf(w.Spec.Outcomes), nz(w.Spec.Approving)
		c.onExpiry, c.deadline = string(w.Spec.OnExpiry.Effective()), w.Deadline
	}
	if w.State.Closed() {
		d := w.Decision
		c.outcome, c.answer, c.actor = nz(d.Outcome), nz(d.Answer), nz(d.Actor)
		c.decidedAt, c.signal, c.reason = d.At, nz(string(d.SignalID)), nz(string(d.Reason))
	}
	return c
}

// writeStep writes one step with its wait (nil for a container step). at stamps
// the start and the end of the step once. outputs, when set, are the step's
// declared outputs, recorded with the settlement that makes it succeeded.
func writeStep(ctx context.Context, tx pgx.Tx, scope domain.Scope, run domain.RunID, s runtime.Step, w *runtime.Wait, outputs *runtime.StepOutputs, at time.Time) error {
	var exit, resCode, resReason, whenResult, whenDigest, whenAt, outs any
	if s.Result != (runtime.Result{}) {
		exit, resCode, resReason = s.Result.ExitCode, string(s.Result.Code), nz(string(s.Result.Reason))
	}
	if p := s.Predicate; p.Evaluated {
		whenResult, whenDigest, whenAt = p.Result, p.InputDigest, p.At
	}
	if outputs != nil {
		outs = jsonOf(outputs.Values)
	}
	var started any
	if s.State == runtime.StepRunning || s.State == runtime.StepWaiting {
		started = at
	}
	c := waitColumnsOf(w)
	tag, err := tx.Exec(ctx, stmtUpdateStep, scope.TenantID, scope.ProjectID, run, s.Key, string(s.State), nz(string(s.Reason)),
		int64(s.Version), s.Attempt, started, s.State.Terminal(), at, exit, resCode, resReason,
		s.Predicate.Evaluated, whenResult, whenDigest, whenAt, outs,
		c.assignment, c.correlation, c.request, c.input, c.outcomes, c.approving, c.onExpiry, c.deadline, c.state, c.outcome,
		c.answer, c.actor, c.decidedAt, c.signal, c.version, c.reason)
	if err != nil {
		return fmt.Errorf("runstate/postgres: update step %s: %w", s.Key, err)
	}
	if tag.RowsAffected() != 1 {
		return notFound("step " + string(s.Key))
	}
	return nil
}

func insertCommand(ctx context.Context, tx pgx.Tx, c runtime.Command) error {
	var attempt, backend any
	if c.Kind.Workload() {
		attempt, backend = c.Binding.Attempt, c.Binding.Backend
	}
	var code, exit, reason any
	if c.Result != (runtime.Result{}) {
		code, exit, reason = string(c.Result.Code), c.Result.ExitCode, nz(string(c.Result.Reason))
	}
	_, err := tx.Exec(ctx, stmtInsertCommand, c.Scope.TenantID, c.Scope.ProjectID, c.RunID, c.ID, nz(string(c.StepKey)), string(c.Kind),
		c.Target, nz(c.SpecDigest), int64(c.Fence), string(c.State), nz(string(c.Reason)), code, exit, reason, attempt, backend, c.RecordedAt)
	if err != nil {
		return fmt.Errorf("runstate/postgres: insert command %s: %w", c.ID, err)
	}
	return nil
}

func updateCommand(ctx context.Context, tx pgx.Tx, c runtime.Command) error {
	var code, exit, reason any
	if c.Result != (runtime.Result{}) {
		code, exit, reason = string(c.Result.Code), c.Result.ExitCode, nz(string(c.Result.Reason))
	}
	tag, err := tx.Exec(ctx, stmtUpdateCommand, c.Scope.TenantID, c.Scope.ProjectID, c.RunID, c.ID, string(c.State),
		nz(string(c.Reason)), code, exit, reason)
	if err != nil {
		return fmt.Errorf("runstate/postgres: update command %s: %w", c.ID, err)
	}
	if tag.RowsAffected() != 1 {
		return notFound("command " + c.ID)
	}
	return nil
}

func workloadResult(o runtime.WorkloadObservation) (code, exit, reason any) {
	if o.Result != (runtime.Result{}) {
		return string(o.Result.Code), o.Result.ExitCode, nz(string(o.Result.Reason))
	}
	return nil, nil, nil
}

func insertWorkload(ctx context.Context, tx pgx.Tx, o runtime.WorkloadObservation) error {
	b := o.Binding
	code, exit, reason := workloadResult(o)
	_, err := tx.Exec(ctx, stmtInsertWorkload, b.Scope.TenantID, b.Scope.ProjectID, b.RunID, b.StepKey, b.Attempt, b.WorkloadID,
		b.SpecDigest, b.Backend, int64(b.Fence), string(o.Phase), code, exit, reason, o.ObservedAt)
	if err != nil {
		return fmt.Errorf("runstate/postgres: insert workload observation %s: %w", b.WorkloadID, err)
	}
	return nil
}

func updateWorkload(ctx context.Context, tx pgx.Tx, o runtime.WorkloadObservation) error {
	b := o.Binding
	code, exit, reason := workloadResult(o)
	tag, err := tx.Exec(ctx, stmtUpdateWorkload, b.Scope.TenantID, b.Scope.ProjectID, b.RunID, b.StepKey, b.Attempt,
		int64(b.Fence), string(o.Phase), code, exit, reason, o.ObservedAt)
	if err != nil {
		return fmt.Errorf("runstate/postgres: update workload observation %s: %w", b.WorkloadID, err)
	}
	if tag.RowsAffected() != 1 {
		return notFound("workload observation " + b.WorkloadID)
	}
	return nil
}

func insertResource(ctx context.Context, tx pgx.Tx, run domain.RunID, o runtime.ResourceObservation) error {
	h := o.Handle
	_, err := tx.Exec(ctx, stmtInsertResource, h.Scope.TenantID, h.Scope.ProjectID, run, string(h.Kind), h.Name, h.Identity,
		nz(string(h.Owner.ProjectID)), string(h.Lifetime), nz(string(h.RunID)), int64(h.Fence), string(o.State),
		nz(string(o.Reason)), o.ObservedAt)
	if err != nil {
		return fmt.Errorf("runstate/postgres: insert resource observation %s: %w", h.Name, err)
	}
	return nil
}

func updateResource(ctx context.Context, tx pgx.Tx, run domain.RunID, o runtime.ResourceObservation) error {
	h := o.Handle
	tag, err := tx.Exec(ctx, stmtUpdateResource, h.Scope.TenantID, h.Scope.ProjectID, run, string(h.Kind), h.Name, h.Identity,
		int64(h.Fence), string(o.State), nz(string(o.Reason)), o.ObservedAt)
	if err != nil {
		return fmt.Errorf("runstate/postgres: update resource observation %s: %w", h.Name, err)
	}
	if tag.RowsAffected() != 1 {
		return notFound("resource observation " + h.Name)
	}
	return nil
}

func insertMaterialization(ctx context.Context, tx pgx.Tx, m runtime.MaterializationRecord) error {
	r := m.Ref
	_, err := tx.Exec(ctx, stmtInsertMaterialization, r.Scope.TenantID, r.Scope.ProjectID, r.RunID, r.StepKey, r.Attempt, r.ID, m.IssuedAt)
	if err != nil {
		return fmt.Errorf("runstate/postgres: insert materialization %s: %w", r.ID, err)
	}
	return nil
}

func revokeMaterialization(ctx context.Context, tx pgx.Tx, m runtime.MaterializationRecord, at time.Time) error {
	r := m.Ref
	tag, err := tx.Exec(ctx, stmtRevokeMaterialization, r.Scope.TenantID, r.Scope.ProjectID, r.RunID, r.StepKey, r.Attempt, at)
	if err != nil {
		return fmt.Errorf("runstate/postgres: revoke materialization %s: %w", r.ID, err)
	}
	if tag.RowsAffected() != 1 {
		return notFound("materialization " + r.ID)
	}
	return nil
}

// waitOf returns the wait of a step in the record, or nil.
func waitOf(waits []runtime.Wait, key domain.StepID) *runtime.Wait {
	if i := slices.IndexFunc(waits, func(w runtime.Wait) bool { return w.StepKey == key }); i >= 0 {
		return &waits[i]
	}
	return nil
}
