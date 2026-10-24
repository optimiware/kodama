package gatecomposition_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/adapters"
	"github.com/optimiware/kodama/internal/gates/gatestest"
	gatespg "github.com/optimiware/kodama/internal/gates/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

// fault is one way the composed transaction fails after the check or mutation
// it names has been applied.
type fault struct {
	name string
	arm  func(f *Faults)
}

var (
	afterAuthority = fault{"after the reviewer authority check", func(f *Faults) { f.Fail(PointAuthority) }}
	afterEvent     = fault{"after the event append", func(f *Faults) { f.Fail(PointEventAppend) }}
	atCommit       = fault{"at commit", func(f *Faults) { f.Commit(2) }}
)

// sameXid asserts that every hit ran in one database transaction and returns it.
func sameXid(t *testing.T, what string, hits []Hit) int64 {
	t.Helper()
	if len(hits) == 0 {
		t.Fatalf("%s: no seam was crossed", what)
	}
	for _, h := range hits {
		if h.Xid <= 0 || h.Xid != hits[0].Xid {
			t.Fatalf("%s: the seams ran in different transactions: %+v", what, hits)
		}
	}
	return hits[0].Xid
}

func has(hits []Hit, point string) bool {
	for _, h := range hits {
		if h.Point == point {
			return true
		}
	}
	return false
}

// Test_gate_join_rollback: SD-04, SD-10, SD-11, IT-03, DQ-03, DQ-04, DQ-11,
// PX-13 (acceptance `gate_join_rollback`). The real gate store, keyed
// decision helper and gate-aware execution store over the runtime-enabled
// execution store, with every participant (gate row, evidence references,
// execution binding, current authorization, deployment configuration, run and
// step history, audit and outbox) joined to one tenant transaction under the
// non-owner runtime role. Faults are injected after each check or mutation has
// been applied, between the decision/applicability checks and the progression
// they authorize; every required state, history, audit and outbox change must
// roll back, and a decision (durable intent) must survive a failed progression.
func Test_gate_join_rollback(t *testing.T) {
	e := New(t, Options{})
	e.GrantAll()
	ctx := context.Background()
	scope := ScopeA
	tenant := scope.TenantID

	t.Run("the participants join one transaction", func(t *testing.T) {
		// Request: gate row, evidence, binding, audit and event.
		s := e.ReadyRun(scope)
		e.F.Reset()
		if rep := e.Reconcile(tenant); rep.Requested != 1 {
			t.Fatalf("request: %+v", rep)
		}
		sameXid(t, "gate request", e.F.Hits(PointEventAppend))
		if !has(e.F.Trace(), PointEventAppend+":gate.requested") {
			t.Fatalf("the request crossed %+v", e.F.Trace())
		}
		g := e.GateOf(s.Run, "review")

		// Decision: authorization revision and gate event.
		e.F.Reset()
		e.MustApprove(g)
		hits := e.F.Trace()
		sameXid(t, "gate decision", hits)
		if !has(hits, PointAuthority) || !has(hits, PointEventAppend+":gate.decided") {
			t.Fatalf("the decision crossed %+v", hits)
		}

		// Consumption: gate lock, current facts, authorization and execution
		// progression (the run is not settled, so no execution event is due).
		e.F.Reset()
		if out, err := e.Consume(g); err != nil || !out.Completed {
			t.Fatalf("consume: %v %+v", err, out)
		}
		consume := e.F.Trace()
		xid := sameXid(t, "gate consumption", consume)
		if !has(consume, PointAuthority) {
			t.Fatalf("consumption never rechecked the reviewer: %+v", consume)
		}

		// A protected claim: capacity, writer slot, authority, gate recheck and
		// execution events all in the dispatch transaction.
		e.F.Reset()
		if _, err := e.ClaimErr(tenant); err != nil {
			t.Fatal(err)
		}
		claim := e.F.Trace()
		cx := sameXid(t, "protected claim", claim)
		if cx == xid {
			t.Fatal("two operations shared one transaction identifier")
		}
		for _, point := range []string{PointWorkspaceReserve, PointAuthority} {
			if !has(claim, point) {
				t.Fatalf("the protected claim never crossed %s: %+v", point, claim)
			}
		}

		// A rejection moves the step in its own transaction.
		r := e.Reviewed(scope)
		if _, err := e.Decide(r.Gate, gatestest.Reviewer, gates.DecisionReject); err != nil {
			t.Fatal(err)
		}
		e.F.Reset()
		if out, err := e.Store.RejectStep(ctx, scope, r.Gate.ID); err != nil || !out.Rejected {
			t.Fatalf("reject: %v %+v", err, out)
		}
		sameXid(t, "step rejection", e.F.Trace())
	})

	t.Run("constructors and bindings fail closed", func(t *testing.T) {
		cfg := gatespg.Config{DB: e.F.DB(), Events: e.F.Appender(e.X.Events), Execution: e.RT, Authority: e.F.Authority(adapters.BindAuthority)}
		for name, mutate := range map[string]func(*gatespg.Config){
			"no database":  func(c *gatespg.Config) { c.DB = nil },
			"no events":    func(c *gatespg.Config) { c.Events = nil },
			"no execution": func(c *gatespg.Config) { c.Execution = nil },
			"no authority": func(c *gatespg.Config) { c.Authority = nil },
		} {
			c := cfg
			mutate(&c)
			if _, err := gatespg.NewStore(c); !errors.Is(err, gates.ErrNotConfigured) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := gatespg.NewExecutionStore(e.F.DB(), nil, e.RT, e.Store); !errors.Is(err, gates.ErrNotConfigured) {
			t.Errorf("no inner execution store: %v", err)
		}
		if _, err := gatespg.NewExecutionStore(e.F.DB(), e.RT, nil, e.Store); !errors.Is(err, gates.ErrNotConfigured) {
			t.Errorf("no transaction binding: %v", err)
		}
		if _, err := gatespg.NewExecutionStore(e.F.DB(), e.RT, e.RT, nil); !errors.Is(err, gates.ErrNotConfigured) {
			t.Errorf("no gate store: %v", err)
		}
		if _, err := gates.NewService(gates.Dependencies{}, gates.Options{GateTTL: 1}); !errors.Is(err, gates.ErrNotConfigured) {
			t.Errorf("an empty gate service: %v", err)
		}
	})

	t.Run("a failed request rolls back the gate, its evidence and its binding; the pins are idempotent intent", func(t *testing.T) {
		for _, f := range []fault{
			{"gate event", func(f *Faults) { f.Fail(PointEventAppend + ":gate.requested") }},
			atCommit,
		} {
			s := e.ReadyRun(scope)
			before := e.Snapshot(s, "review")
			id := gates.GateIDFor(s.Run.Scope, s.Run.ID, "review")
			f.arm(e.F)
			rep, err := e.Gates.Reconcile(ctx, tenant)
			e.F.Clear()
			if err != nil || rep.Requested != 0 || rep.Failed == 0 {
				t.Fatalf("%s: reconcile during the fault: %+v %v", f.name, rep, err)
			}
			if after := e.Snapshot(s, "review"); after != before {
				t.Fatalf("%s: a failed request left effects:\n before %+v\n after  %+v", f.name, before, after)
			}
			// The evidence pins are made before the transaction and are idempotent,
			// keyed by the stable gate identity: durable intent, not a gate.
			pinned := e.X.Count(`SELECT count(*) FROM artifact_pins WHERE owner_kind = 'gate' AND owner_id = $1`, string(id))
			if pinned != 1 {
				t.Fatalf("%s: %d evidence pins of the intended gate, want 1", f.name, pinned)
			}
			if rep := e.Reconcile(tenant); rep.Requested != 1 {
				t.Fatalf("%s: after the fault: %+v", f.name, rep)
			}
			g := e.GateOf(s.Run, "review")
			if g.ID != id {
				t.Fatalf("%s: the retry created %s, want the stable identity %s", f.name, g.ID, id)
			}
			after := e.Snapshot(s, "review")
			if after.GateRows != 1 || after.GateBound != 1 || after.GateAudit != 1 || after.GateOutbox != 1 || after.GateEvidence != 1 || after.Pins != 1 {
				t.Fatalf("%s: the retry left %+v", f.name, after)
			}
			if after.Attempts != before.Attempts || after.Reservations != before.Reservations || after.WriterSlots != 0 {
				t.Fatalf("%s: a gate allocated compute: %+v (before %+v)", f.name, after, before)
			}
		}
	})

	t.Run("a lost commit acknowledgment of a request leaves one gate", func(t *testing.T) {
		s := e.ReadyRun(scope)
		e.F.Commit(1)
		rep, err := e.Gates.Reconcile(ctx, tenant)
		e.F.Clear()
		if err != nil || rep.Failed == 0 {
			t.Fatalf("reconcile with a lost acknowledgment: %+v %v", rep, err)
		}
		if n := e.Snapshot(s, "review"); n.GateRows != 1 || n.GateBound != 1 || n.GateAudit != 1 || n.GateOutbox != 1 {
			t.Fatalf("the committed request is not whole: %+v", n)
		}
		if rep := e.Reconcile(tenant); rep.Requested != 0 {
			t.Fatalf("the retry created a second gate: %+v", rep)
		}
		if n := e.Snapshot(s, "review"); n.GateRows != 1 || n.GateAudit != 1 || n.GateOutbox != 1 {
			t.Fatalf("the retry changed the committed request: %+v", n)
		}
	})

	t.Run("a failed decision rolls back state, audit and outbox", func(t *testing.T) {
		for _, f := range []fault{afterAuthority, afterEvent, atCommit} {
			s := e.Reviewed(scope)
			before := e.Snapshot(s, "review")
			f.arm(e.F)
			_, err := e.Approve(s.Gate)
			e.F.Clear()
			if err == nil {
				t.Fatalf("%s: the decision succeeded", f.name)
			}
			if after := e.Snapshot(s, "review"); after != before {
				t.Fatalf("%s: a failed decision left effects:\n before %+v\n after  %+v", f.name, before, after)
			}
			if cur := e.Current(s.Gate); cur.Status != gates.StatusPending || cur.Decision != nil || cur.Version != s.Gate.Version {
				t.Fatalf("%s: the decision survived: %+v", f.name, cur)
			}
			// The same command, same version, succeeds once the fault is gone.
			if _, err := e.Approve(s.Gate); err != nil {
				t.Fatalf("%s: retry: %v", f.name, err)
			}
			if n := e.Snapshot(s, "review"); n.GateAudit != before.GateAudit+1 || n.GateOutbox != before.GateOutbox+1 || n.GateVersion != before.GateVersion+1 {
				t.Fatalf("%s: a decision must leave one audit row and one event: %+v", f.name, n)
			}
		}
	})

	t.Run("a lost commit acknowledgment of a decision leaves exactly one decision", func(t *testing.T) {
		s := e.Reviewed(scope)
		before := e.Snapshot(s, "review")
		e.F.Commit(1)
		if _, err := e.Approve(s.Gate); err == nil {
			t.Fatal("the acknowledgment was lost but no error was reported")
		}
		e.F.Clear()
		if cur := e.Current(s.Gate); cur.Status != gates.StatusApproved || cur.Decision == nil {
			t.Fatalf("the committed decision is missing: %+v", cur)
		}
		// The retry observes whatever committed: it never decides twice.
		_, _ = e.Approve(s.Gate)
		if n := e.Snapshot(s, "review"); n.GateAudit != before.GateAudit+1 || n.GateOutbox != before.GateOutbox+1 || n.GateVersion != before.GateVersion+1 {
			t.Fatalf("the retry decided again: %+v", n)
		}
	})

	t.Run("a failed completion rolls back the step, run, history, audit and outbox; the approval is durable intent", func(t *testing.T) {
		// The gate is the last step: completing it settles the run, which appends
		// a run event after the step and run mutations were written.
		for _, f := range []fault{afterAuthority, afterEvent, atCommit} {
			s := e.FinalGate(scope)
			e.MustApprove(s.Gate)
			before := e.Snapshot(s, "review")
			f.arm(e.F)
			if _, err := e.Consume(s.Gate); err == nil {
				t.Fatalf("%s: the step completed", f.name)
			}
			e.F.Clear()
			if after := e.Snapshot(s, "review"); after != before {
				t.Fatalf("%s: a failed completion left effects:\n before %+v\n after  %+v", f.name, before, after)
			}
			if cur := e.Current(s.Gate); cur.Status != gates.StatusApproved || cur.Decision == nil || cur.Invalidation != nil {
				t.Fatalf("%s: the immutable approval did not survive intact: %+v", f.name, cur)
			}
			out, err := e.Consume(s.Gate)
			if err != nil || !out.Completed {
				t.Fatalf("%s: retry: %v %+v", f.name, err, out)
			}
			done := e.Snapshot(s, "review")
			if done.StepStatus != string(execution.StepSucceeded) || done.RunStatus != string(execution.RunSucceeded) || done.Attempts != 0 || done.Reservations != before.Reservations {
				t.Fatalf("%s: the completion left compute or the wrong state: %+v", f.name, done)
			}
			if n := e.Transitions(s.Run, "review"); n != 1 {
				t.Fatalf("%s: %d step transitions after the retry, want one", f.name, n)
			}
			_, _ = e.Consume(s.Gate)
			if again := e.Snapshot(s, "review"); again != done {
				t.Fatalf("%s: a duplicate completion changed state:\n %+v\n %+v", f.name, done, again)
			}
		}
	})

	t.Run("a failed completion of a step with a successor rolls back the same way", func(t *testing.T) {
		for _, f := range []fault{afterAuthority, atCommit} {
			s := e.Reviewed(scope)
			e.MustApprove(s.Gate)
			before := e.Snapshot(s, "review")
			f.arm(e.F)
			if _, err := e.Consume(s.Gate); err == nil {
				t.Fatalf("%s: the step completed", f.name)
			}
			e.F.Clear()
			if after := e.Snapshot(s, "review"); after != before {
				t.Fatalf("%s: a failed completion left effects:\n before %+v\n after  %+v", f.name, before, after)
			}
			if err := e.NothingRunnable(tenant); err != nil {
				t.Fatalf("%s: a successor ran before its gate step completed: %v", f.name, err)
			}
			if out, err := e.Consume(s.Gate); err != nil || !out.Completed {
				t.Fatalf("%s: retry: %v %+v", f.name, err, out)
			}
			if n := e.Transitions(s.Run, "review"); n != 1 {
				t.Fatalf("%s: %d transitions after the retry", f.name, n)
			}
		}
	})

	t.Run("a lost commit acknowledgment of a completion leaves one transition", func(t *testing.T) {
		s := e.FinalGate(scope)
		e.MustApprove(s.Gate)
		e.F.Commit(1)
		if _, err := e.Consume(s.Gate); err == nil {
			t.Fatal("the acknowledgment was lost but no error was reported")
		}
		e.F.Clear()
		if n := e.Transitions(s.Run, "review"); n != 1 {
			t.Fatalf("%d step transitions after the lost acknowledgment", n)
		}
		before := e.Snapshot(s, "review")
		_, _ = e.Consume(s.Gate)
		if again := e.Snapshot(s, "review"); again != before {
			t.Fatalf("the retry changed the committed completion:\n %+v\n %+v", before, again)
		}
	})

	t.Run("a failed rejection rolls back the step but not the rejection", func(t *testing.T) {
		for _, f := range []fault{afterEvent, atCommit} {
			s := e.FinalGate(scope)
			if _, err := e.Decide(s.Gate, gatestest.Reviewer, gates.DecisionReject); err != nil {
				t.Fatal(err)
			}
			before := e.Snapshot(s, "review")
			f.arm(e.F)
			if _, err := e.Store.RejectStep(ctx, scope, s.Gate.ID); err == nil {
				t.Fatalf("%s: the step was rejected", f.name)
			}
			e.F.Clear()
			if after := e.Snapshot(s, "review"); after != before {
				t.Fatalf("%s: a failed rejection left effects:\n before %+v\n after  %+v", f.name, before, after)
			}
			if cur := e.Current(s.Gate); cur.Status != gates.StatusRejected || cur.Decision == nil {
				t.Fatalf("%s: the immutable rejection did not survive: %+v", f.name, cur)
			}
			if out, err := e.Store.RejectStep(ctx, scope, s.Gate.ID); err != nil || !out.Rejected {
				t.Fatalf("%s: retry: %v %+v", f.name, err, out)
			}
			done := e.Snapshot(s, "review")
			if done.StepStatus != string(execution.StepFailed) || done.Attempts != 0 || done.Reservations != before.Reservations {
				t.Fatalf("%s: the rejection left compute or the wrong state: %+v", f.name, done)
			}
			if n := e.Transitions(s.Run, "review"); n != 1 {
				t.Fatalf("%s: %d transitions after the retry", f.name, n)
			}
		}
	})

	t.Run("a failed protected claim rolls back the attempt, ordinal, capacity and writer slot", func(t *testing.T) {
		for _, f := range []fault{
			{"after the workspace writer reservation", func(f *Faults) { f.Fail(PointWorkspaceReserve) }},
			afterAuthority,
			afterEvent,
			atCommit,
		} {
			s := e.Reviewed(scope)
			e.MustApprove(s.Gate)
			if out, err := e.Consume(s.Gate); err != nil || !out.Completed {
				t.Fatalf("consume: %v %+v", err, out)
			}
			before := e.Snapshot(s, "review")
			if before.Attempts != 0 || before.Started != 0 || before.WriterSlots != 0 {
				t.Fatalf("%s: setup: %+v", f.name, before)
			}
			f.arm(e.F)
			if _, err := e.ClaimErr(tenant); err == nil {
				t.Fatalf("%s: a claim committed through the fault", f.name)
			}
			e.F.Clear()
			if after := e.Snapshot(s, "review"); after != before {
				t.Fatalf("%s: a failed claim leaked:\n before %+v\n after  %+v", f.name, before, after)
			}
			in, err := e.ClaimErr(tenant)
			if err != nil || in.Ordinal != 1 || in.StepID != "deploy" {
				t.Fatalf("%s: claim after the fault: %v %+v", f.name, err, in)
			}
			after := e.Snapshot(s, "review")
			if after.Attempts != 1 || after.Started != 1 || after.WriterSlots != 1 || after.Reservations != before.Reservations+1 {
				t.Fatalf("%s: the claim after the fault is not whole: %+v", f.name, after)
			}
			// Positive control of the snapshot: a committed claim does leave attempt
			// audit rows and attempt events, so their absence after a failure is
			// evidence of rollback rather than of a blind query.
			if after.ExecAudit <= before.ExecAudit || after.ExecOutbox <= before.ExecOutbox || after.History <= before.History {
				t.Fatalf("%s: the snapshot did not see the committed claim's audit/outbox/history:\n before %+v\n after  %+v", f.name, before, after)
			}
			// Finish the attempt so the next iteration's claim is not blocked by a
			// shared quota: the run is cancelled by the cleanup.
		}
	})

	t.Run("a pending gate cannot be consumed and holds no compute", func(t *testing.T) {
		s := e.Reviewed(scope)
		if _, err := e.Consume(s.Gate); !errors.Is(err, gates.ErrInvalidTransition) {
			t.Fatalf("consuming a pending gate: %v", err)
		}
		if err := e.NothingRunnable(tenant); err != nil {
			t.Fatal(err)
		}
		if n := e.Snapshot(s, "review"); n.Attempts != 0 || n.Started != 0 || n.WriterSlots != 0 {
			t.Fatalf("a pending gate held compute: %+v", n)
		}
	})

	t.Run("the binder reads deployment configuration through the supplied transaction", func(t *testing.T) {
		dep := domain.Scope{TenantID: scope.TenantID, ProjectID: scope.ProjectID}
		const id = "dep_t073_vis"
		e.Deploy.Remove(id)
		bound := e.Deploy.Bind
		other := func() error {
			_, err := e.Deploy.Snapshot(ctx, dep, id)
			return err
		}
		err := storage.WithTenant(ctx, e.Pool, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			// A row written by this transaction and not committed: visible through
			// the transaction the binder was handed, to nobody else.
			if _, err := tx.Exec(ctx, `INSERT INTO t073_deployments (tenant_id, project_id, id, candidate, config_digest) VALUES ($1, $2, $3, $4, $5)`,
				string(scope.TenantID), string(scope.ProjectID), id, gatestest.CommitA, digestOf("uncommitted")); err != nil {
				return err
			}
			snap, err := bound(tx).Snapshot(ctx, dep, id)
			if err != nil || snap.ConfigDigest != digestOf("uncommitted") || snap.Scope != dep || snap.Candidate != gatestest.CommitA {
				t.Errorf("the binder did not read through the transaction: %+v %v", snap, err)
			}
			if err := other(); !errors.Is(err, gates.ErrNotFound) {
				t.Errorf("an uncommitted row is visible outside its transaction: %v", err)
			}
			// Another tenant's or project's scope never sees the row.
			for name, sc := range map[string]domain.Scope{
				"another project": {TenantID: scope.TenantID, ProjectID: ScopeA2.ProjectID},
				"another tenant":  ScopeB,
			} {
				if _, err := bound(tx).Snapshot(ctx, sc, id); !errors.Is(err, gates.ErrNotFound) {
					t.Errorf("%s: %v", name, err)
				}
			}
			return errors.New("roll back the fixture row")
		})
		if err == nil || other() == nil {
			t.Fatalf("the fixture row survived its rolled back transaction: %v", err)
		}
		// Committed, the row is visible to the unbound port as well.
		e.Deploy.Set(dep, id, gatestest.CommitA, digestOf("committed"))
		if snap, err := e.Deploy.Snapshot(ctx, dep, id); err != nil || snap.ConfigDigest != digestOf("committed") {
			t.Fatalf("committed row: %+v %v", snap, err)
		}
		// Another tenant's transaction cannot read it even with the right ids.
		err = storage.WithTenant(ctx, e.Pool, ScopeB.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM t073_deployments`).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Errorf("tenant B sees %d of tenant A's deployments", n)
			}
			return nil
		})
		must(t, err)
	})

	t.Run("a deployment configuration read fails after the read: decision, completion and claim roll back", func(t *testing.T) {
		s := e.DeploymentGated(scope, "rollback-deploy", "dep_t073_roll", "config-1")
		before := e.Snapshot(s, "review")
		// The decision reads the configuration through its own transaction.
		e.F.Reset()
		e.F.Fail(PointDeployment)
		if _, err := e.Approve(s.Gate); err == nil {
			t.Fatal("the decision succeeded through the fault")
		}
		e.F.Clear()
		if after := e.Snapshot(s, "review"); after != before {
			t.Fatalf("a failed decision left effects:\n before %+v\n after  %+v", before, after)
		}
		// The read statement's own transaction identifier equals the decision's
		// authorization and event participants'.
		if !has(e.F.Trace(), PointDeployment) {
			t.Fatalf("the configuration was not read in the decision transaction: %+v", e.F.Trace())
		}
		sameXid(t, "the deployment read of a decision", e.F.Trace())

		e.MustApprove(s.Gate)
		approved := e.Snapshot(s, "review")
		e.F.Reset()
		e.F.Fail(PointDeployment)
		if _, err := e.Consume(s.Gate); err == nil {
			t.Fatal("the completion succeeded through the fault")
		}
		e.F.Clear()
		if after := e.Snapshot(s, "review"); after != approved {
			t.Fatalf("a failed completion left effects:\n before %+v\n after  %+v", approved, after)
		}
		consume := e.F.Trace()
		sameXid(t, "deployment read of a completion", consume)
		if !has(consume, PointDeployment) {
			t.Fatalf("completion crossed %+v", consume)
		}

		if out, err := e.Consume(s.Gate); err != nil || !out.Completed {
			t.Fatalf("consume: %v %+v", err, out)
		}
		done := e.Snapshot(s, "review")
		e.F.Reset()
		e.F.Fail(PointDeployment)
		if _, err := e.ClaimErr(tenant); err == nil {
			t.Fatal("the claim succeeded through the fault")
		}
		e.F.Clear()
		if after := e.Snapshot(s, "review"); after != done {
			t.Fatalf("a failed claim leaked:\n before %+v\n after  %+v", done, after)
		}
		claim := e.F.Trace()
		sameXid(t, "deployment read of a protected claim", claim)
		if !has(claim, PointDeployment) {
			t.Fatalf("the claim crossed %+v", claim)
		}
		// The gate check is part of the mutation, before its effects (capacity
		// and writer slot) are applied: a refusal never reserves anything. The
		// successful claim reads the configuration, rechecks the reviewer and
		// reserves the writer slot in one transaction.
		e.F.Reset()
		if in, err := e.ClaimErr(tenant); err != nil || in.StepID != "promote" || in.Ordinal != 1 {
			t.Fatalf("claim after the fault: %v %+v", err, in)
		}
		ok := e.F.Trace()
		sameXid(t, "a claim over a deployment gate", ok)
		for _, point := range []string{PointDeployment, PointAuthority, PointWorkspaceReserve} {
			if !has(ok, point) {
				t.Fatalf("the claim never crossed %s: %+v", point, ok)
			}
		}
	})
}
