package environmentpinningruntime_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	execadapters "github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/test/integration/delivery/stack"

	"github.com/jackc/pgx/v5"
)

// faultyReader is the transaction-bound delivery reader of the admission with an
// armed failure: an outage of the retained-snapshot read.
type faultyReader struct {
	execadapters.TxDelivery
	snapshotErr *atomic.Pointer[error]
	calls       *atomic.Int64
}

func (f faultyReader) Snapshot(ctx context.Context, scope domain.Scope, env domain.EnvironmentID, digest runtimev1.Digest) (delivery.ConfigurationSnapshot, error) {
	f.calls.Add(1)
	if err := f.snapshotErr.Load(); err != nil {
		return delivery.ConfigurationSnapshot{}, *err
	}
	return f.TxDelivery.Snapshot(ctx, scope, env, digest)
}

// failingEvents fails the n-th execution event append inside the admission
// transaction, after the run, its steps and the credential pins were written.
type failingEvents struct {
	execpg.EventAppender
	failAt *atomic.Int64
	seen   *atomic.Int64
}

func (f failingEvents) Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error) {
	n := f.seen.Add(1)
	if at := f.failAt.Load(); at != 0 && n == at {
		return eventspg.AppendResult{}, errors.New("event bus unavailable")
	}
	return f.EventAppender.Append(ctx, tx, env)
}

// Test_environment_pin_rollback — environment_pin_rollback (DQ-15/PX-01): every
// failure of the admission transaction after the environment and credential pins
// were prepared — credential revoked, authority revoked, an altered or missing
// retained snapshot, a delivery outage, a failing event append after the pins
// were written — leaves no run, step, history, credential pin, retained
// source reference, audit row or event behind; the identical admission then
// succeeds exactly once.
func Test_environment_pin_rollback(t *testing.T) {
	ctx := context.Background()
	var (
		snapshotErr atomic.Pointer[error]
		binderCalls atomic.Int64
		failAt      atomic.Int64
		seen        atomic.Int64
	)
	w := newWorld(t, stack.Options{
		WrapBinder: func(r execadapters.TxDelivery) execadapters.TxDelivery {
			return faultyReader{TxDelivery: r, snapshotErr: &snapshotErr, calls: &binderCalls}
		},
		WrapExecAppender: func(a execpg.EventAppender) execpg.EventAppender {
			return failingEvents{EventAppender: a, failAt: &failAt, seen: &seen}
		},
	})

	attempt := func(t *testing.T, what string, prepare func(plan *execution.AdmissionPlan), want error) {
		t.Helper()
		plan := w.prepare(t)
		if prepare != nil {
			prepare(plan)
		}
		before := w.counts()
		_, _, err := w.ExecStore.Admit(ctx, plan)
		if err == nil || !errors.Is(err, want) {
			t.Fatalf("%s: got %v, want %v", what, err, want)
		}
		t.Logf("%s: %v", what, err)
		w.requireUnchanged(t, before, what)
	}

	t.Run("a_credential_revoked_between_preparation_and_commit", func(t *testing.T) {
		attempt(t, "a revoked credential", func(*execution.AdmissionPlan) {
			if err := w.X.RevokeSecret(scopeA, w.secret.ID(), 1); err != nil {
				t.Fatal(err)
			}
		}, execution.ErrAdmissionRefused)
		// With the only current version revoked a fresh preparation is refused
		// before any transaction; a new version restores admission.
		if _, err := w.Exec.Prepare(ctx, caller(), w.command()); err == nil {
			t.Fatal("a revoked credential was resolved for a new admission")
		}
		w.X.RotateSecret(scopeA, w.secret.ID(), kubeconfigV2)
	})

	t.Run("the_actors_authority_revoked_between_preparation_and_commit", func(t *testing.T) {
		plan := w.prepare(t)
		before := w.counts()
		w.X.Revoke(scopeA, executiontest.Engineer)
		_, _, err := w.ExecStore.Admit(ctx, plan)
		if !errors.Is(err, execution.ErrDenied) && !errors.Is(err, execution.ErrHidden) {
			t.Fatalf("an admission by a revoked actor: %v", err)
		}
		w.requireUnchanged(t, before, "a revoked actor")
		w.X.Grant(scopeA, executiontest.Engineer, identity.RoleEngineer)
		w.X.Grant(scopeA2, executiontest.Engineer, identity.RoleEngineer)
	})

	t.Run("an_altered_or_unretained_snapshot_pin", func(t *testing.T) {
		attempt(t, "an unretained snapshot", func(plan *execution.AdmissionPlan) {
			forged := runtimev1.Digest("sha256:" + strings.Repeat("b", 64))
			plan.Aggregate.Run.Snapshot.Environments[0].Pin.ConfigDigest = forged
			plan.Environments[0].Pin.ConfigDigest = forged
		}, execution.ErrAdmissionRefused)
		attempt(t, "other credential bindings than the retained snapshot's", func(plan *execution.AdmissionPlan) {
			plan.Environments[0].Secrets[0].Version = 99
		}, execution.ErrAdmissionRefused)
	})

	t.Run("a_delivery_outage_inside_the_transaction_is_unavailable_never_access", func(t *testing.T) {
		outage := errors.New("delivery store unreachable")
		snapshotErr.Store(&outage)
		defer snapshotErr.Store(nil)
		binderCalls.Store(0)
		attempt(t, "a delivery outage", nil, execution.ErrUnavailable)
		if binderCalls.Load() == 0 {
			t.Fatal("the admission transaction never consulted delivery")
		}
	})

	t.Run("a_failing_event_append_after_the_pins_were_written", func(t *testing.T) {
		seen.Store(0)
		failAt.Store(1)
		defer failAt.Store(0)
		plan := w.prepare(t)
		before := w.counts()
		_, _, err := w.ExecStore.Admit(ctx, plan)
		if err == nil {
			t.Fatal("an admission whose event append failed committed")
		}
		t.Logf("a failed event append: %v", err)
		w.requireUnchanged(t, before, "a failed event append")
		if n := w.Count(`SELECT count(*) FROM secret_pins WHERE released_at IS NULL AND consumer_id LIKE 'run_%'`); n != 0 {
			t.Fatalf("%d credential pins survived the rolled-back admission", n)
		}
		failAt.Store(0)
		// The same plan, repeated, now commits once.
		run, created, err := w.ExecStore.Admit(ctx, plan)
		if err != nil || !created || run == nil {
			t.Fatalf("the repeated admission: %v %v", created, err)
		}
		if n := w.runPins(run.ID, w.currentVersion(t)); n != 1 {
			t.Fatalf("the committed run has %d credential pins", n)
		}
	})

	t.Run("the_identical_admission_succeeds_exactly_once", func(t *testing.T) {
		before := w.Count(`SELECT count(*) FROM runs WHERE purpose = 'work'`)
		run := w.admit(t)
		if after := w.Count(`SELECT count(*) FROM runs WHERE purpose = 'work'`); after != before+1 {
			t.Fatalf("runs: %d -> %d", before, after)
		}
		pin := pinOf(t, run)
		if _, err := w.Svc.RetainedSnapshot(ctx, scopeA, pin.ID, pin.ConfigDigest); err != nil {
			t.Fatal(err)
		}
	})
}

func (w *world) currentVersion(t *testing.T) int64 {
	t.Helper()
	var v int64
	w.Scalar(&v, `SELECT max(version) FROM secret_versions WHERE tenant_id = $1 AND secret_id = $2`, string(scopeA.TenantID), w.secret.ID())
	return v
}
