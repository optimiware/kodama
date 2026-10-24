package integration_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/gateway"
	"github.com/optimiware/kodama/internal/gateway/adapters"
	"github.com/optimiware/kodama/internal/gateway/gatewaytest"
)

// forwardByHand performs the admission and forwarding steps of a worker that
// then dies: the request is committed as forwarding and nothing settles it.
func forwardByHand(t *testing.T, e *gatewaytest.Env, r *gatewaytest.Replica, a gatewaytest.Attempt, begin bool) gateway.RequestID {
	t.Helper()
	ctx := context.Background()
	id, err := gateway.NewRequestID()
	if err != nil {
		t.Fatal(err)
	}
	adm, err := r.Store.Admit(ctx, gateway.AdmitCommand{Workload: a.Workload, RequestID: id, Model: "gpt-a", RequestBytes: 60, InputBound: 30, OutputMax: 64, Now: e.X.Clock.Now()})
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if begin {
		if err := r.Store.BeginForwarding(ctx, a.Workload, adm.Request.ID, e.X.Clock.Now()); err != nil {
			t.Fatalf("begin forwarding: %v", err)
		}
	}
	return id
}

func observed(in, out int64) gateway.Outcome {
	return gateway.Outcome{Result: gateway.StateSucceeded, Code: "completed", Settlement: gateway.SettlementObserved, Observed: &gateway.TokenUsage{Input: in, Output: out}}
}

func finalizeCmd(a gatewaytest.Attempt, id gateway.RequestID, o gateway.Outcome) gateway.FinalizeCommand {
	return gateway.FinalizeCommand{Scope: a.Scope, RunID: a.RunID, AttemptID: a.ID, RequestID: id, Outcome: o, Now: time.Now()}
}

// Test_gateway_recovery: GM-03, GM-04, GM-05, SD-A (recovery). Crashes around
// admission, send and finalization, lost commits, duplicate finalization, stale
// workers and a refused event write never duplicate a provider call, a charge
// or an event, and never reopen an unsafe slot.
func Test_gateway_recovery(t *testing.T) {
	t.Run("a lost admission commit sends nothing and is reconciled as unsent", func(t *testing.T) {
		e := newWorld(t)
		r := e.NewReplica(gatewaytest.ReplicaOptions{Timeouts: fastTimeouts(20 * time.Second)})
		a := e.NewAttempt(scopeA)

		r.DB.LoseNextCommits(1)
		if _, err := e.Invoke(r, a, body("x"), &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrUnavailable) {
			t.Fatalf("lost admission commit: %v", err)
		}
		if n := len(e.Provider.Calls()); n != 0 {
			t.Fatalf("provider calls %d: an admission with an unknown outcome must not send", n)
		}
		if st := reqState(t, e, a.RunID, 1); st[0] != "admitted" {
			t.Fatalf("request state %v", st)
		}
		// The request is real, so it holds the slot.
		if _, err := e.Invoke(r, a, body("x"), &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrBusy) {
			t.Fatalf("while the orphan holds the slot: %v", err)
		}
		e.X.Clock.Advance(time.Minute)
		rep, err := r.Svc.Reconcile(context.Background(), 2*time.Second)
		if err != nil || rep.Unsent != 1 || rep.Unknown != 0 {
			t.Fatalf("reconcile: %+v %v", rep, err)
		}
		if st := reqState(t, e, a.RunID, 1); st != [2]string{"failed", "unsent_abandoned"} {
			t.Fatalf("request state %v", st)
		}
		if adm, charged := e.Budget(a.RunID); adm != 1 || charged != 0 {
			t.Fatalf("admitted=%d charged=%d: an unsent request consumes an admission but no tokens", adm, charged)
		}
		if _, err := e.Invoke(r, a, body("x"), &gatewaytest.Sink{}); err != nil {
			t.Fatalf("after reconciliation: %v", err)
		}
		if adm, _ := e.Budget(a.RunID); adm != 2 {
			t.Fatalf("admitted=%d, want 2", adm)
		}
	})

	t.Run("a dead worker's forwarding request is charged, keeps the slot, and settles once", func(t *testing.T) {
		e := newWorld(t)
		r1 := e.NewReplica(gatewaytest.ReplicaOptions{Timeouts: fastTimeouts(20 * time.Second)})
		r2 := e.NewReplica(gatewaytest.ReplicaOptions{Timeouts: fastTimeouts(20 * time.Second)})
		a := e.NewAttempt(scopeA)
		id := forwardByHand(t, e, e.Replica, a, true)
		e.X.Clock.Advance(time.Minute)

		// Two replicas reconcile at once: exactly one effect.
		var wg sync.WaitGroup
		reports := make([]gateway.ReconcileReport, 2)
		for i, r := range []*gatewaytest.Replica{r1, r2} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var err error
				if reports[i], err = r.Svc.Reconcile(context.Background(), 2*time.Second); err != nil {
					t.Errorf("reconcile %d: %v", i, err)
				}
			}()
		}
		wg.Wait()
		if unknown := reports[0].Unknown + reports[1].Unknown; unknown != 1 {
			t.Fatalf("reports %+v: the abandoned request was settled %d times", reports, unknown)
		}
		u, err := e.Replica.Store.Usage(context.Background(), scopeA, id)
		if err != nil || u.Result != gateway.StateUnknown || u.Charged != 94 || u.Uncertainty != gateway.UncertaintyWorkerLost || u.Basis != gateway.BasisEstimated {
			t.Fatalf("usage %+v %v: want the bound (30+64) charged with worker_lost", u, err)
		}
		if n := e.Events(scopeA.TenantID, "usage.recorded"); n != 1 {
			t.Fatalf("usage events %d", n)
		}

		// A later pass changes nothing, and lease expiry never reopens the slot.
		e.X.Clock.Advance(2 * time.Minute) // lease expiry alone, within the token lifetime
		if rep, _ := r1.Svc.Reconcile(context.Background(), 2*time.Second); rep != (gateway.ReconcileReport{}) {
			t.Fatalf("a second pass acted: %+v", rep)
		}
		if _, err := e.Invoke(e.Replica, a, body("x"), &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrBusy) {
			t.Fatalf("an unknown request must keep blocking the run: %v", err)
		}
		if n := len(e.Provider.Calls()); n != 0 {
			t.Fatalf("provider calls %d", n)
		}

		// The stale worker comes back: it can neither record usage nor beat.
		if _, err := e.Replica.Store.Finalize(context.Background(), finalizeCmd(a, id, observed(10, 3))); !errors.Is(err, gateway.ErrSuperseded) {
			t.Fatalf("a stale finalization: %v", err)
		}
		if err := e.Replica.Store.Heartbeat(context.Background(), a.Workload, id, time.Now()); !errors.Is(err, gateway.ErrSuperseded) {
			t.Fatalf("a stale heartbeat: %v", err)
		}
		if _, charged := e.Budget(a.RunID); charged != 94 {
			t.Fatalf("charged %d after the stale worker", charged)
		}
		if n := e.Events(scopeA.TenantID, "usage.recorded"); n != 1 {
			t.Fatalf("usage events %d after the stale worker", n)
		}

		// Only trusted evidence with management authority releases the slot, and
		// the charge stays.
		ctx := context.Background()
		if err := r1.Svc.SettleUnknown(ctx, gateway.Actor{ID: gatewaytest.Viewer}, scopeA, id); !errors.Is(err, gateway.ErrDenied) {
			t.Fatalf("a viewer settling: %v", err)
		}
		if err := r1.Svc.SettleUnknown(ctx, gateway.Actor{ID: gatewaytest.Admin}, scopeA, id); err != nil {
			t.Fatal(err)
		}
		if st := reqState(t, e, a.RunID, 1); st[0] != "settled" {
			t.Fatalf("request state %v", st)
		}
		if err := r1.Svc.SettleUnknown(ctx, gateway.Actor{ID: gatewaytest.Admin}, scopeA, id); !errors.Is(err, gateway.ErrInvalidTransition) {
			t.Fatalf("settling twice: %v", err)
		}
		if _, err := e.Invoke(e.Replica, a, body("x"), &gatewaytest.Sink{}); err != nil {
			t.Fatalf("after settlement: %v", err)
		}
		if _, charged := e.Budget(a.RunID); charged != 94+13 {
			t.Fatalf("charged %d, want the unchanged 94 plus the new request's 13", charged)
		}
	})

	t.Run("manual settlement is authorized inside its own transaction and attributed to the human", func(t *testing.T) {
		const settledAudit = `SELECT count(*) FROM audit WHERE action = 'model.request.settled' AND resource_id = $1`
		// unknownRequest leaves one forwarding request abandoned and reconciled to
		// unknown: charged its bound, holding the run's slot, with one usage row.
		unknownRequest := func(t *testing.T) (*gatewaytest.Env, gatewaytest.Attempt, gateway.RequestID) {
			t.Helper()
			e := newWorld(t)
			a := e.NewAttempt(scopeA)
			id := forwardByHand(t, e, e.Replica, a, true)
			e.X.Clock.Advance(time.Minute)
			if rep, err := e.Replica.Svc.Reconcile(context.Background(), 2*time.Second); err != nil || rep.Unknown != 1 {
				t.Fatalf("reconcile: %+v %v", rep, err)
			}
			return e, a, id
		}
		// stillHeld asserts that nothing settled: the request is unknown, the slot
		// is held, the charge, usage record and event are exactly the abandonment's,
		// no manual-settlement audit exists and the provider was never called.
		stillHeld := func(t *testing.T, e *gatewaytest.Env, a gatewaytest.Attempt, id gateway.RequestID) {
			t.Helper()
			if st := reqState(t, e, a.RunID, 1); st[0] != "unknown" {
				t.Fatalf("request state %v, want unknown", st)
			}
			if adm, charged := e.Budget(a.RunID); adm != 1 || charged != 94 {
				t.Fatalf("admitted=%d charged=%d, want 1 and 94", adm, charged)
			}
			if _, err := e.Invoke(e.Replica, a, body("x"), &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrBusy) {
				t.Fatalf("the slot was released: %v", err)
			}
			if n := e.Count(`SELECT count(*) FROM model_usage WHERE run_id = $1`, string(a.RunID)); n != 1 {
				t.Fatalf("%d usage rows", n)
			}
			if n := e.Events(scopeA.TenantID, "usage.recorded"); n != 1 {
				t.Fatalf("%d usage events", n)
			}
			if n := e.Count(settledAudit, string(id)); n != 0 {
				t.Fatalf("%d settlement audit rows", n)
			}
			if n := len(e.Provider.Calls()); n != 0 {
				t.Fatalf("provider calls %d", n)
			}
		}
		refused := func(err error) bool { return errors.Is(err, gateway.ErrDenied) || errors.Is(err, gateway.ErrHidden) }

		t.Run("a permission revoked between the preliminary check and persistence", func(t *testing.T) {
			e, a, id := unknownRequest(t)
			var once sync.Once
			e.Replica.DB.OnBegin(func() { once.Do(func() { e.X.Revoke(scopeA, gatewaytest.Admin) }) })
			err := e.Replica.Svc.SettleUnknown(context.Background(), gateway.Actor{ID: gatewaytest.Admin}, scopeA, id)
			e.Replica.DB.OnBegin(nil)
			if !refused(err) {
				t.Fatalf("a settlement after revocation: %v", err)
			}
			stillHeld(t, e, a, id)
		})

		t.Run("a revocation racing the settlement is ordered with it", func(t *testing.T) {
			e, a, id := unknownRequest(t)
			ctx := context.Background()
			// An in-flight revocation holds the actor's authorization revision.
			rev, err := e.X.Migration.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rev.Rollback(ctx) }()
			if _, err := rev.Exec(ctx, `UPDATE identity_authz_revisions SET revision = revision + 1 WHERE tenant_id = $1 AND user_id = $2`,
				string(scopeA.TenantID), gatewaytest.Admin); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				done <- e.Replica.Store.Settle(ctx, gateway.SettleCommand{Actor: gateway.Actor{ID: gatewaytest.Admin}, Scope: scopeA, RequestID: id, Now: e.X.Clock.Now()})
			}()
			select {
			case err := <-done:
				t.Fatalf("the settlement did not wait for the revocation: %v", err)
			case <-time.After(300 * time.Millisecond):
			}
			if _, err := rev.Exec(ctx, `DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`, string(scopeA.TenantID), gatewaytest.Admin); err != nil {
				t.Fatal(err)
			}
			if err := rev.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !refused(err) {
				t.Fatalf("a settlement racing a revocation: %v", err)
			}
			stillHeld(t, e, a, id)
		})

		t.Run("a foreign scope and an actor without the permission settle nothing", func(t *testing.T) {
			e, a, id := unknownRequest(t)
			ctx := context.Background()
			if err := e.Replica.Svc.SettleUnknown(ctx, gateway.Actor{ID: gatewaytest.Admin}, executiontest.ScopeB, id); !errors.Is(err, gateway.ErrNotFound) {
				t.Fatalf("settling by another tenant's scope: %v", err)
			}
			if err := e.Replica.Svc.SettleUnknown(ctx, gateway.Actor{ID: gatewaytest.Viewer}, scopeA, id); !refused(err) {
				t.Fatalf("a viewer settling: %v", err)
			}
			if err := e.Replica.Store.Settle(ctx, gateway.SettleCommand{Actor: gateway.Actor{ID: gatewaytest.Viewer}, Scope: scopeA, RequestID: id, Now: e.X.Clock.Now()}); !refused(err) {
				t.Fatalf("a viewer reaching the store directly: %v", err)
			}
			if err := e.Replica.Store.Settle(ctx, gateway.SettleCommand{Actor: gateway.Actor{ID: "u032_nobody"}, Scope: scopeA, RequestID: id, Now: e.X.Clock.Now()}); !refused(err) {
				t.Fatalf("an unknown actor: %v", err)
			}
			if err := e.Replica.Store.Settle(ctx, gateway.SettleCommand{Scope: scopeA, RequestID: id, Now: e.X.Clock.Now()}); err == nil {
				t.Fatal("an anonymous settlement was accepted")
			}
			stillHeld(t, e, a, id)
		})

		t.Run("an audit failure rolls the settlement back and a retry succeeds once", func(t *testing.T) {
			e, a, id := unknownRequest(t)
			e.X.OwnerExec(`CREATE FUNCTION t032_refuse_settled_audit() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN
					IF NEW.action = 'model.request.settled' THEN RAISE EXCEPTION 't032 injected audit failure'; END IF;
					RETURN NEW;
				END $$`)
			e.X.OwnerExec(`CREATE TRIGGER t032_refuse_settled_audit BEFORE INSERT ON audit FOR EACH ROW EXECUTE FUNCTION t032_refuse_settled_audit()`)
			ctx := context.Background()
			err := e.Replica.Svc.SettleUnknown(ctx, gateway.Actor{ID: gatewaytest.Admin}, scopeA, id)
			if !errors.Is(err, gateway.ErrUnavailable) {
				t.Fatalf("a failed audit write: %v", err)
			}
			stillHeld(t, e, a, id)

			e.X.OwnerExec(`DROP TRIGGER t032_refuse_settled_audit ON audit`)
			if err := e.Replica.Svc.SettleUnknown(ctx, gateway.Actor{ID: gatewaytest.Admin}, scopeA, id); err != nil {
				t.Fatalf("the retry: %v", err)
			}
			if n := e.Count(settledAudit, string(id)); n != 1 {
				t.Fatalf("%d settlement audit rows after the retry", n)
			}
		})

		t.Run("success names the authorizing human, releases only the slot and keeps usage unchanged", func(t *testing.T) {
			e, a, id := unknownRequest(t)
			ctx := context.Background()
			before, err := e.Replica.Store.Usage(ctx, scopeA, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Replica.Svc.SettleUnknown(ctx, gateway.Actor{ID: gatewaytest.Admin}, scopeA, id); err != nil {
				t.Fatal(err)
			}
			var kind, actor string
			e.Scalar(&kind, `SELECT actor_type FROM audit WHERE action = 'model.request.settled' AND resource_id = $1`, string(id))
			e.Scalar(&actor, `SELECT actor_id FROM audit WHERE action = 'model.request.settled' AND resource_id = $1`, string(id))
			if kind != "user" || actor != gatewaytest.Admin {
				t.Fatalf("settlement attributed to %s %s, want the authorizing user", kind, actor)
			}
			if n := e.Count(`SELECT count(*) FROM audit WHERE actor_id = 'model-gateway' AND action = 'model.request.settled'`); n != 0 {
				t.Fatalf("%d settlements attributed to the service", n)
			}
			after, err := e.Replica.Store.Usage(ctx, scopeA, id)
			if err != nil || after != before {
				t.Fatalf("usage changed: %+v -> %+v %v", before, after, err)
			}
			if adm, charged := e.Budget(a.RunID); adm != 1 || charged != 94 {
				t.Fatalf("admitted=%d charged=%d, want the unchanged 1 and 94", adm, charged)
			}
			if n := e.Events(scopeA.TenantID, "usage.recorded"); n != 1 {
				t.Fatalf("%d usage events: settlement must not record usage again", n)
			}
			if err := e.Replica.Svc.SettleUnknown(ctx, gateway.Actor{ID: gatewaytest.Admin}, scopeA, id); !errors.Is(err, gateway.ErrInvalidTransition) {
				t.Fatalf("settling twice: %v", err)
			}
			if n := e.Count(settledAudit, string(id)); n != 1 {
				t.Fatalf("%d settlement audit rows after a duplicate", n)
			}
			if n := len(e.Provider.Calls()); n != 0 {
				t.Fatalf("provider calls %d", n)
			}
			if _, err := e.Invoke(e.Replica, a, body("x"), &gatewaytest.Sink{}); err != nil {
				t.Fatalf("after settlement: %v", err)
			}
		})
	})

	t.Run("a worker lost before forwarding cannot send afterwards", func(t *testing.T) {
		e := newWorld(t)
		r := e.NewReplica(gatewaytest.ReplicaOptions{Timeouts: fastTimeouts(20 * time.Second)})
		a := e.NewAttempt(scopeA)
		id := forwardByHand(t, e, e.Replica, a, false)
		e.X.Clock.Advance(time.Minute)
		if rep, err := r.Svc.Reconcile(context.Background(), 2*time.Second); err != nil || rep.Unsent != 1 {
			t.Fatalf("reconcile: %+v %v", rep, err)
		}
		// The slow worker resumes: its request is already finished, so it cannot
		// start forwarding (and so cannot send).
		if err := e.Replica.Store.BeginForwarding(context.Background(), a.Workload, id, e.X.Clock.Now()); !errors.Is(err, gateway.ErrInvalidTransition) {
			t.Fatalf("a stale begin-forwarding: %v", err)
		}
	})

	t.Run("a lost or failed finalization is recovered without repeating the call", func(t *testing.T) {
		for name, inject := range map[string]func(*gatewaytest.DB){
			"lost commit":   func(db *gatewaytest.DB) { db.LoseCommits(2, 1) },
			"failed begin":  func(db *gatewaytest.DB) { db.FailNthBegin(3) },
			"lost, retried": func(db *gatewaytest.DB) { db.LoseCommits(2, 2) },
		} {
			t.Run(name, func(t *testing.T) {
				e := newWorld(t)
				r := e.NewReplica(gatewaytest.ReplicaOptions{Timeouts: quietTimeouts()})
				a := e.NewAttempt(scopeA)
				inject(r.DB)
				sink := &gatewaytest.Sink{}
				res, err := e.Invoke(r, a, body("x"), sink)
				if err != nil || res.Usage == nil || res.Usage.Charged != 13 {
					t.Fatalf("result %+v err %v", res, err)
				}
				if _, fin, _ := sink.Snapshot(); !fin {
					t.Fatal("the stream was not completed")
				}
				if n := len(e.Provider.Calls()); n != 1 {
					t.Fatalf("provider calls %d: finalization recovery repeated the call", n)
				}
				adm, charged := e.Budget(a.RunID)
				if adm != 1 || charged != 13 {
					t.Fatalf("admitted=%d charged=%d", adm, charged)
				}
				if n := e.Count(`SELECT count(*) FROM model_usage WHERE run_id = $1`, string(a.RunID)); n != 1 {
					t.Fatalf("%d usage rows", n)
				}
				if n := e.Events(scopeA.TenantID, "usage.recorded"); n != 1 {
					t.Fatalf("%d usage events", n)
				}
			})
		}
	})

	t.Run("a lost finalization commit of a truncated response preserves its unknown receipt and slot", func(t *testing.T) {
		e := newWorld(t)
		r := e.NewReplica(gatewaytest.ReplicaOptions{Timeouts: quietTimeouts()})
		a := e.NewAttempt(scopeA)
		raw := body("truncated commit response")
		bound := inputBound(t, e, raw) + 64
		e.Provider.Script(gatewaytest.Truncated("gpt-a-2026", "partial"))
		// Admission and begin-forwarding commit normally; finalization commits
		// but its acknowledgment is lost. Its unknown request state is durable
		// finalization evidence, even though it still occupies the run's slot.
		r.DB.LoseCommits(2, 1)
		sink := &gatewaytest.Sink{}
		res, err := e.Invoke(r, a, raw, sink)
		if !errors.Is(err, gateway.ErrProvider) || res.State != gateway.StateUnknown || res.Usage == nil {
			t.Fatalf("the recorded truncation was not recovered: %+v %v", res, err)
		}
		if res.Usage.Charged != bound || res.Usage.Result != gateway.StateUnknown {
			t.Fatalf("recovered usage %+v, want unknown with charge %d", res.Usage, bound)
		}
		if _, finished, code := sink.Snapshot(); finished || code != "stream_truncated" {
			t.Fatalf("finished=%v code=%q, want the original truncation", finished, code)
		}
		if _, err := e.Invoke(e.Replica, a, raw, &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrBusy) {
			t.Fatalf("the recovered unknown request released its slot: %v", err)
		}
		if admitted, charged := e.Budget(a.RunID); admitted != 1 || charged != bound {
			t.Fatalf("admitted=%d charged=%d, want one charge of %d", admitted, charged, bound)
		}
		if len(e.Provider.Calls()) != 1 || e.Count(`SELECT count(*) FROM model_usage WHERE run_id = $1`, string(a.RunID)) != 1 || e.Events(scopeA.TenantID, "usage.recorded") != 1 {
			t.Fatal("receipt recovery repeated a provider call, usage record or event")
		}
	})

	t.Run("duplicate concurrent finalization settles exactly once", func(t *testing.T) {
		e := newWorld(t)
		a := e.NewAttempt(scopeA)
		id := forwardByHand(t, e, e.Replica, a, true)
		r2 := e.NewReplica(gatewaytest.ReplicaOptions{})
		const n = 6
		errs := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				store := e.Replica.Store
				if i%2 == 1 {
					store = r2.Store
				}
				_, errs[i] = store.Finalize(context.Background(), finalizeCmd(a, id, observed(10, 3)))
			}()
		}
		close(start)
		wg.Wait()
		wins := 0
		for i, err := range errs {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, gateway.ErrSuperseded):
			default:
				t.Errorf("finalizer %d: %v", i, err)
			}
		}
		if wins != 1 {
			t.Fatalf("%d finalizations won", wins)
		}
		if _, charged := e.Budget(a.RunID); charged != 13 {
			t.Fatalf("charged %d, want 13 once", charged)
		}
		if c := e.Events(scopeA.TenantID, "usage.recorded"); c != 1 {
			t.Fatalf("%d usage events", c)
		}
	})

	t.Run("a refused event write rolls the settlement back and reconciliation charges the bound once", func(t *testing.T) {
		e := newWorld(t)
		real := adapters.EventsFrom(e.X.Events)
		var mu sync.Mutex
		refuse := true
		refusing := func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
			mu.Lock()
			defer mu.Unlock()
			if refuse && env.Type == eventsv1.TypeUsageRecorded {
				return fmt.Errorf("%w: injected full backlog", gateway.ErrBackpressure)
			}
			return real(ctx, tx, env)
		}
		r := e.NewReplica(gatewaytest.ReplicaOptions{Events: refusing, Timeouts: fastTimeouts(20 * time.Second)})
		a := e.NewAttempt(scopeA)

		sink := &gatewaytest.Sink{}
		res, err := e.Invoke(r, a, body("x"), sink)
		if err == nil || res.Usage != nil {
			t.Fatalf("a refused settlement reported success: %+v %v", res, err)
		}
		if _, fin, failCode := sink.Snapshot(); fin || failCode != "unavailable" {
			t.Fatalf("finished=%v failCode=%q: the workload must not see a clean end", fin, failCode)
		}
		if n := len(e.Provider.Calls()); n != 1 {
			t.Fatalf("provider calls %d", n)
		}
		// Atomic: no usage, no event, no charge; the request still holds the slot.
		if n := e.Count(`SELECT count(*) FROM model_usage`); n != 0 {
			t.Fatalf("%d usage rows survived the rollback", n)
		}
		if n := e.Events(scopeA.TenantID, "usage.recorded"); n != 0 {
			t.Fatalf("%d usage events survived the rollback", n)
		}
		if n := e.Count(`SELECT count(*) FROM audit WHERE action LIKE 'model.request.%'`); n != 0 {
			t.Fatalf("%d audit rows survived the rollback: usage, charge, outcome, audit and event commit together", n)
		}
		if st := reqState(t, e, a.RunID, 1); st[0] != "forwarding" {
			t.Fatalf("request state %v", st)
		}
		if _, charged := e.Budget(a.RunID); charged != 0 {
			t.Fatalf("charged %d", charged)
		}

		mu.Lock()
		refuse = false
		mu.Unlock()
		e.X.Clock.Advance(time.Minute)
		rep, err := r.Svc.Reconcile(context.Background(), 2*time.Second)
		if err != nil || rep.Unknown != 1 {
			t.Fatalf("reconcile: %+v %v", rep, err)
		}
		if n := e.Count(`SELECT count(*) FROM model_usage`); n != 1 {
			t.Fatalf("%d usage rows", n)
		}
		if n := e.Events(scopeA.TenantID, "usage.recorded"); n != 1 {
			t.Fatalf("%d usage events", n)
		}
		if n := e.Count(`SELECT count(*) FROM audit WHERE action LIKE 'model.request.%'`); n != 1 {
			t.Fatalf("%d audit rows after reconciliation, want 1", n)
		}
		if n := len(e.Provider.Calls()); n != 1 {
			t.Fatalf("provider calls %d: reconciliation must never repeat provider traffic", n)
		}
		if _, err := e.Invoke(r, a, body("x"), &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrBusy) {
			t.Fatalf("the unknown request must keep the slot: %v", err)
		}
	})

	t.Run("a live worker's heartbeat keeps it from reconciliation", func(t *testing.T) {
		e := newWorld(t)
		a := e.NewAttempt(scopeA)
		started := make(chan struct{})
		release := make(chan struct{})
		e.Provider.Script(e.Provider.Hang("gpt-a-2026", started, release, "one"))
		done := make(chan error, 1)
		go func() {
			_, err := e.Invoke(e.Replica, a, body("x"), &gatewaytest.Sink{})
			done <- err
		}()
		<-started
		e.X.Clock.Advance(time.Hour)
		time.Sleep(700 * time.Millisecond) // several heartbeat intervals at the advanced time
		rep, err := e.Replica.Svc.Reconcile(context.Background(), 2*time.Second)
		if err != nil || rep != (gateway.ReconcileReport{}) {
			t.Fatalf("a live request was reconciled: %+v %v", rep, err)
		}
		close(release)
		// The stream has no usage chunk and no terminator: it ends with the
		// provider's close, as an incomplete stream.
		<-done
	})

	t.Run("reconciliation needs a sane quiet period and a tenant source", func(t *testing.T) {
		e := newWorld(t)
		if _, err := e.Replica.Svc.Reconcile(context.Background(), time.Millisecond); !errors.Is(err, gateway.ErrInvalid) {
			t.Fatalf("a quiet period shorter than the heartbeat: %v", err)
		}
		none := e.NewReplica(gatewaytest.ReplicaOptions{NoTenants: true})
		if _, err := none.Svc.Reconcile(context.Background(), time.Minute); err == nil {
			t.Fatal("reconciling without a tenant source succeeded")
		}
	})
}
