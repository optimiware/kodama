package postgres_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

var errInjected = errors.New("injected append failure")

// failingAppender fails the append of one event type.
type failingAppender struct {
	inner execpg.EventAppender
	fail  atomic.Value // string
}

func (f *failingAppender) Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error) {
	if t, _ := f.fail.Load().(string); t != "" && env.Type == t {
		return eventspg.AppendResult{}, errInjected
	}
	return f.inner.Append(ctx, tx, env)
}

func newFailing(t *testing.T) (*executiontest.Env, *failingAppender) {
	var fa *failingAppender
	e := executiontest.New(t, executiontest.Options{WrapAppender: func(in execpg.EventAppender) execpg.EventAppender {
		fa = &failingAppender{inner: in}
		return fa
	}})
	e.GrantAll()
	return e, fa
}

// Test_atomic_effect_rollback: PX-05, PX-11, PX-06. An audit/outbox/backlog
// failure rolls back state, secret pins, quota and the writer reservation
// together.
func Test_atomic_effect_rollback(t *testing.T) {
	const secretDoc = "env: {TOKEN: {secretRef: \"secret://project/token\"}}"

	t.Run("admission", func(t *testing.T) {
		e, fa := newFailing(t)
		e.Secret(executiontest.ScopeA, "token", "s3cr3t")
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.Doc(job("deploy", readRun, "timeoutSeconds: 60", secretDoc)))
		fa.fail.Store(eventsv1.TypeRunCreated)
		if _, err := e.Admit(p, def, nil); !errors.Is(err, errInjected) {
			t.Fatalf("got %v", err)
		}
		for table, n := range map[string]int{
			"runs": e.Count(`SELECT count(*) FROM runs`), "step_runs": e.Count(`SELECT count(*) FROM step_runs`),
			"secret_pins": e.Count(`SELECT count(*) FROM secret_pins`), "workspace_retained_refs": e.Count(`SELECT count(*) FROM workspace_retained_refs`),
			"run_history": e.Count(`SELECT count(*) FROM run_history`), "run audit": e.Count(`SELECT count(*) FROM audit WHERE resource_type = 'run'`),
		} {
			if n != 0 {
				t.Errorf("%s: %d rows survived a failed admission", table, n)
			}
		}
		fa.fail.Store("")
		run := e.MustAdmit(p, def, nil)
		if e.Count(`SELECT count(*) FROM secret_pins WHERE consumer_id = $1`, string(run.ID)) != 1 {
			t.Fatal("the retried admission did not register its pin")
		}
	})

	t.Run("claim with a writer reservation", func(t *testing.T) {
		e, fa := newFailing(t)
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.Doc(job("edit", writeRun, "timeoutSeconds: 60")))
		run := e.MustAdmit(p, def, nil)
		before := e.Get(run)
		fa.fail.Store(eventsv1.TypeStepAttemptStateChange)
		if _, err := e.Claim(p.Scope.TenantID); !errors.Is(err, errInjected) {
			t.Fatalf("got %v", err)
		}
		if attemptCount(e, run.ID) != 0 || reservationsHeld(e) != 0 {
			t.Fatal("attempt or capacity survived the failed claim")
		}
		var holder *string
		e.Scalar(&holder, `SELECT writer_attempt_id FROM workspaces WHERE id = $1`, string(p.Workspace))
		if holder != nil {
			t.Fatalf("the writer reservation survived: %s", *holder)
		}
		after := e.Get(run)
		if after.Run.Version != before.Run.Version || after.Run.Status != execution.RunQueued || after.Step("edit").AttemptsStarted != 0 {
			t.Fatalf("run %s v%d step attempts %d: state survived", after.Run.Status, after.Run.Version, after.Step("edit").AttemptsStarted)
		}
		if n := e.Count(`SELECT count(*) FROM audit WHERE resource_type = 'step_attempt'`); n != 0 {
			t.Fatalf("%d attempt audit rows survived", n)
		}
		fa.fail.Store("")
		if in := e.MustClaim(p.Scope.TenantID); in.Ordinal != 1 {
			t.Fatalf("ordinal %d: the failed claim consumed one", in.Ordinal)
		}
	})

	t.Run("a full event backlog rolls back the whole claim", func(t *testing.T) {
		opts := eventstest.DefaultOptions()
		opts.Backlog = events.BacklogLimits{MaxEvents: 2, MaxBytes: 1 << 20}
		e := executiontest.New(t, executiontest.Options{Events: &opts})
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.Doc(job("edit", writeRun, "timeoutSeconds: 60")))
		run := e.MustAdmit(p, def, nil) // run.created: 1 of 2
		if _, err := e.Claim(p.Scope.TenantID); err == nil {
			t.Fatal("the claim fit a full backlog")
		}
		if attemptCount(e, run.ID) != 0 || reservationsHeld(e) != 0 {
			t.Fatal("state survived the backlog failure")
		}
		var holder *string
		e.Scalar(&holder, `SELECT writer_attempt_id FROM workspaces WHERE id = $1`, string(p.Workspace))
		if holder != nil {
			t.Fatal("the writer reservation survived the backlog failure")
		}
		// Only the admission's own retention confirmation event exists; the claim's
		// run and attempt events rolled back with it.
		if n := e.CountTenant(p.Scope.TenantID, `SELECT count(*) FROM outbox WHERE event_type IN ('run.state_changed', 'step_attempt.state_changed')`); n != 1 {
			t.Fatalf("%d run/attempt events, want only the retention confirmation", n)
		}
	})

	t.Run("settlement and result commit roll back together", func(t *testing.T) {
		e, fa := newFailing(t)
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.Doc(job("edit", writeRun, "timeoutSeconds: 60")))
		run := e.MustAdmit(p, def, nil)
		in := e.MustClaim(p.Scope.TenantID)
		e.Start(in)
		fa.fail.Store(eventsv1.TypeRunStateChanged) // the run's completion event fails
		if _, err := e.Succeed(in, nil, executiontest.CommitA); !errors.Is(err, errInjected) {
			t.Fatalf("got %v", err)
		}
		agg := e.Get(run)
		if agg.Attempt(in.AttemptID).Status != execution.AttemptRunning || reservationsHeld(e) != 1 {
			t.Fatalf("attempt %s, %d held: a partially committed result", agg.Attempt(in.AttemptID).Status, reservationsHeld(e))
		}
		var holder *string
		e.Scalar(&holder, `SELECT writer_attempt_id FROM workspaces WHERE id = $1`, string(p.Workspace))
		if holder == nil {
			t.Fatal("the writer slot was released although the result rolled back")
		}
		fa.fail.Store("")
		if _, err := e.Succeed(in, nil, executiontest.CommitA); err != nil {
			t.Fatalf("retry after the fault: %v", err)
		}
	})
}
