package gatecomposition_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/storage"
)

func staleReason(t *testing.T, what string, err error, reason string) {
	t.Helper()
	var se *gates.StaleError
	if !errors.As(err, &se) || se.Reason != reason || !errors.Is(err, gates.ErrStaleSubject) {
		t.Fatalf("%s: %v, want a stale subject (%s)", what, err, reason)
	}
}

// noCompute asserts a refused or rolled back progression allocated no attempt,
// ordinal, capacity reservation or writer slot beyond the earlier snapshot.
func noCompute(t *testing.T, what string, before, after Snap) {
	t.Helper()
	if after.Attempts != before.Attempts || after.Started != before.Started || after.Reservations != before.Reservations || after.WriterSlots != before.WriterSlots {
		t.Fatalf("%s: compute was allocated:\n before %+v\n after  %+v", what, before, after)
	}
}

// Test_gate_join_revocation: SD-04, SD-10, SD-11, IT-03, DQ-03, DQ-04, DQ-11,
// DQ-14, PX-13 (acceptance `gate_join_revocation`). Current authorization and
// the current subject (specification, source, configuration, evidence) are
// rechecked inside the very transaction that decides, consumes or claims,
// including when a revocation or a change commits while that transaction waits
// on a lock; a refusal leaves the decision and the earlier history intact,
// records one invalidation for a definitive change, and allocates no
// attempt, ordinal, capacity or writer slot. Duplicate decisions converge, and
// one pooled connection reused across tenants carries no tenant context.
func Test_gate_join_revocation(t *testing.T) {
	e := New(t, Options{})
	e.GrantAll()
	ctx := context.Background()
	scope := ScopeA
	tenant := scope.TenantID

	grantTemp := func(user string) {
		e.X.Grant(scope, user, identity.RoleReviewer)
	}
	decideAs := func(g *gates.Gate, user string) (*gates.Gate, error) {
		return e.Decide(g, user, gates.DecisionApprove)
	}

	t.Run("a reviewer revoked while the decision waits on the gate lock", func(t *testing.T) {
		s := e.Reviewed(scope)
		before := e.Snapshot(s, "review")
		grantTemp("u073_racer")
		xid, release := e.HoldGate(s.Gate)
		done := make(chan error, 1)
		go func() { _, err := decideAs(s.Gate, "u073_racer"); done <- err }()
		e.WaitBlocked(xid)
		e.X.Revoke(scope, "u073_racer")
		release()
		if err := <-done; !errors.Is(err, gates.ErrHidden) && !errors.Is(err, gates.ErrDenied) {
			t.Fatalf("the decision used the authority it had before the lock wait: %v", err)
		}
		if after := e.Snapshot(s, "review"); after != before {
			t.Fatalf("a revoked reviewer's decision left effects:\n before %+v\n after  %+v", before, after)
		}
	})

	t.Run("a revocation that commits while consumption waits on the gate lock", func(t *testing.T) {
		s := e.Reviewed(scope)
		grantTemp("u073_waiter")
		if _, err := decideAs(s.Gate, "u073_waiter"); err != nil {
			t.Fatal(err)
		}
		before := e.Snapshot(s, "review")
		xid, release := e.HoldGate(s.Gate)
		done := make(chan error, 1)
		go func() { _, err := e.Consume(s.Gate); done <- err }()
		e.WaitBlocked(xid)
		e.X.Revoke(scope, "u073_waiter")
		release()
		if err := <-done; !errors.Is(err, gates.ErrReviewerAuthority) {
			t.Fatalf("consumption used the authority it had before the lock wait: %v", err)
		}
		if after := e.Snapshot(s, "review"); after != before {
			t.Fatalf("a refused consumption left effects:\n before %+v\n after  %+v", before, after)
		}
		if cur := e.Current(s.Gate); cur.Status != gates.StatusApproved || cur.Invalidation != nil {
			t.Fatalf("an authority check must not rewrite or invalidate the decision: %+v", cur)
		}
		if err := e.NothingRunnable(tenant); err != nil {
			t.Fatal(err)
		}
		// Current authority is the rule: restoring the reviewer restores it.
		grantTemp("u073_waiter")
		if rep := e.Reconcile(tenant); rep.Completed != 1 {
			t.Fatalf("after the reviewer was restored: %+v", rep)
		}
	})

	t.Run("a revocation committing after the authorization revision wait is seen by consumption", func(t *testing.T) {
		s := e.Reviewed(scope)
		grantTemp("u073_revision")
		if _, err := decideAs(s.Gate, "u073_revision"); err != nil {
			t.Fatal(err)
		}
		before := e.Snapshot(s, "review")
		// The revocation transaction holds the reviewer's authorization revision
		// row; the consumption reaches it only after the run and gate locks.
		xid, release := e.HoldRevocation(scope, "u073_revision")
		done := make(chan error, 1)
		go func() { _, err := e.Consume(s.Gate); done <- err }()
		e.WaitBlocked(xid)
		release()
		if err := <-done; !errors.Is(err, gates.ErrReviewerAuthority) {
			t.Fatalf("consumption missed the revocation committed during its wait: %v", err)
		}
		if after := e.Snapshot(s, "review"); after != before {
			t.Fatalf("a refused consumption left effects:\n before %+v\n after  %+v", before, after)
		}
	})

	t.Run("a revocation that commits while a protected claim waits on the gate lock", func(t *testing.T) {
		s := e.Reviewed(scope)
		grantTemp("u073_claimer")
		if _, err := decideAs(s.Gate, "u073_claimer"); err != nil {
			t.Fatal(err)
		}
		if out, err := e.Consume(s.Gate); err != nil || !out.Completed {
			t.Fatalf("consume: %v %+v", err, out)
		}
		before := e.Snapshot(s, "review")
		xid, release := e.HoldGate(s.Gate)
		res := make(chan error, 1)
		go func() { _, err := e.ClaimErr(tenant); res <- err }()
		e.WaitBlocked(xid)
		e.X.Revoke(scope, "u073_claimer")
		release()
		if err := <-res; !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("the claim used the authority it had before the wait: %v", err)
		}
		after := e.Snapshot(s, "review")
		if after != before {
			t.Fatalf("a refused claim left effects:\n before %+v\n after  %+v", before, after)
		}
		noCompute(t, "the revoked reviewer's claim", before, after)
		grantTemp("u073_claimer")
		if in, err := e.ClaimErr(tenant); err != nil || in.Ordinal != 1 || in.StepID != "deploy" {
			t.Fatalf("after the reviewer was restored: %v %+v", err, in)
		}
	})

	// A change to the subject between approval and progression: the decision
	// (durable intent) stays, one invalidation is recorded, nothing progresses.
	type change struct {
		name   string
		reason string
		// setup builds the scenario; mutate changes the fact.
		setup  func(e *Env, name string) Scenario
		mutate func(e *Env, s Scenario)
	}
	changes := []change{
		{
			name: "specification", reason: gates.ReasonContentChanged,
			setup:  func(e *Env, _ string) Scenario { return e.Reviewed(scope) },
			mutate: func(e *Env, s Scenario) { e.X.EditSpec(scope, s.P.Item, "# Plan\nA changed requirement.") },
		},
		{
			name: "evidence", reason: gates.ReasonEvidenceChanged,
			setup: func(e *Env, _ string) Scenario { return e.Reviewed(scope) },
			mutate: func(e *Env, s Scenario) {
				e.X.OwnerExec(`UPDATE artifacts SET disposition = 'removed', version = version + 1 WHERE run_id = $1 AND name = 'report'`, string(s.Run.ID))
			},
		},
		{
			name: "deployment configuration", reason: gates.ReasonDeploymentChanged,
			setup: func(e *Env, name string) Scenario {
				return e.DeploymentGated(scope, "revoke-"+name, "dep_t073_"+name, "config-1")
			},
			mutate: func(e *Env, s Scenario) {
				e.Deploy.Set(scope, s.Dep, gatestest.CommitA, digestOf("config-2"))
			},
		},
	}
	for _, c := range changes {
		for _, point := range []string{"decision", "consumption", "protected claim"} {
			c, point := c, point
			t.Run("changed "+c.name+" before the "+point, func(t *testing.T) {
				name := c.name[:3] + point[:3]
				s := c.setup(e, name)
				switch point {
				case "decision":
					before := e.Snapshot(s, "review")
					c.mutate(e, s)
					_, err := e.Approve(s.Gate)
					staleReason(t, "the decision", err, c.reason)
					after := e.Snapshot(s, "review")
					if cur := e.Current(s.Gate); cur.Status != gates.StatusPending || cur.Decision != nil || cur.Invalidation == nil || cur.Invalidation.Reason != c.reason {
						t.Fatalf("a stale decision was recorded or the staleness was not: %+v", cur)
					}
					if after.GateAudit != before.GateAudit+1 || after.GateOutbox != before.GateOutbox+1 || after.Invalidations != 1 {
						t.Fatalf("the definitive staleness is one audit row and one event:\n before %+v\n after  %+v", before, after)
					}
					if _, err := e.Approve(e.Current(s.Gate)); !errors.Is(err, gates.ErrStaleSubject) {
						t.Fatalf("approving an invalidated gate: %v", err)
					}
					e.Reconcile(tenant)
					if again := e.Snapshot(s, "review"); again != after {
						t.Fatalf("a repeat or a reconciliation changed the recorded staleness:\n %+v\n %+v", after, again)
					}
					noCompute(t, "the stale decision", before, after)
				case "consumption":
					e.MustApprove(s.Gate)
					before := e.Snapshot(s, "review")
					c.mutate(e, s)
					rep := e.Reconcile(tenant)
					if rep.Completed != 0 || rep.Invalidated != 1 {
						t.Fatalf("reconcile: %+v", rep)
					}
					after := e.Snapshot(s, "review")
					if after.StepStatus != string(execution.StepPending) || after.Invalidations != 1 || after.GateAudit != before.GateAudit+1 || after.GateOutbox != before.GateOutbox+1 {
						t.Fatalf("a stale approval was consumed or the staleness mis-recorded:\n before %+v\n after  %+v", before, after)
					}
					if cur := e.Current(s.Gate); cur.Status != gates.StatusApproved || cur.Decision == nil || cur.Invalidation.Reason != c.reason {
						t.Fatalf("the decision must stay with a separate invalidation: %+v", cur)
					}
					if _, err := e.Consume(s.Gate); err == nil {
						t.Fatal("a direct consumption of a stale approval succeeded")
					}
					if rep := e.Reconcile(tenant); rep.Invalidated != 0 || rep.Completed != 0 {
						t.Fatalf("the staleness was recorded twice: %+v", rep)
					}
					if err := e.NothingRunnable(tenant); err != nil {
						t.Fatal(err)
					}
					noCompute(t, "the stale consumption", before, e.Snapshot(s, "review"))
				case "protected claim":
					e.MustApprove(s.Gate)
					if out, err := e.Consume(s.Gate); err != nil || !out.Completed {
						t.Fatalf("consume: %v %+v", err, out)
					}
					decision := *e.Current(s.Gate).Decision
					before := e.Snapshot(s, "review")
					c.mutate(e, s)
					if err := e.NothingRunnable(tenant); err != nil {
						t.Fatalf("a protected successor was claimed over a changed %s: %v", c.name, err)
					}
					noCompute(t, "the stale claim", before, e.Snapshot(s, "review"))
					if after := e.Snapshot(s, "review"); after != before {
						t.Fatalf("a refused claim left effects:\n before %+v\n after  %+v", before, after)
					}
					if rep := e.Reconcile(tenant); rep.Invalidated != 1 {
						t.Fatalf("reconcile: %+v", rep)
					}
					cur := e.Current(s.Gate)
					if cur.Invalidation == nil || cur.Invalidation.Reason != c.reason || *cur.Decision != decision || cur.Status != gates.StatusApproved {
						t.Fatalf("history must stay intact with the invalidation beside it: %+v", cur)
					}
					if got := e.Snapshot(s, "review").StepStatus; got != string(execution.StepSucceeded) {
						t.Fatalf("the gate step's historical result changed: %s", got)
					}
					if rep := e.Reconcile(tenant); rep.Invalidated != 0 {
						t.Fatalf("the staleness was recorded twice: %+v", rep)
					}
				}
			})
		}
	}

	t.Run("a deployment gate fails closed without the transaction-bound binder", func(t *testing.T) {
		// The state of the merged code: the gate service can request a deployment
		// gate through its own port, but no binder reads the configuration inside
		// the decision or progression transaction. It must be undecidable and
		// unconsumable, never permissive.
		undecided := e.DeploymentGated(scope, "nobinder-a", "dep_t073_nobinder_a", "config-1")
		approved := e.DeploymentGated(scope, "nobinder-b", "dep_t073_nobinder_b", "config-1")
		e.Keep = true
		defer func() { e.Keep = false }()
		e.MustApprove(approved.Gate)

		e.opts.NoDeploymentBinder = true
		e.Restart() // the same database, a store without the binder
		defer func() { e.opts.NoDeploymentBinder = false; e.Restart() }()

		before := e.Snapshot(undecided, "review")
		if _, err := e.Approve(undecided.Gate); !errors.Is(err, gates.ErrNotConfigured) {
			t.Fatalf("a decision without a deployment binder: %v", err)
		}
		if after := e.Snapshot(undecided, "review"); after != before {
			t.Fatalf("a refused decision left effects:\n before %+v\n after  %+v", before, after)
		}
		// An approval recorded earlier is not consumable without the binder.
		before = e.Snapshot(approved, "review")
		if rep := e.Reconcile(tenant); rep.Completed != 0 {
			t.Fatalf("an approval was consumed without the binder: %+v", rep)
		}
		if _, err := e.Consume(approved.Gate); !errors.Is(err, gates.ErrNotConfigured) {
			t.Fatalf("consumption without a deployment binder: %v", err)
		}
		if err := e.NothingRunnable(tenant); err != nil {
			t.Fatal(err)
		}
		after := e.Snapshot(approved, "review")
		if after != before {
			t.Fatalf("an unbound consumption left effects:\n before %+v\n after  %+v", before, after)
		}
		noCompute(t, "the unbound consumption", before, after)
	})

	t.Run("a source change: a write after the approval needs a new gate", func(t *testing.T) {
		e.Quiesce()
		p := e.X.NewProject(scope)
		def := e.Define(scope,
			pipelinetest.Container("implement", writeRun),
			gatestest.Gate("review", []string{"implement"}),
			pipelinetest.Container("polish", "needs: [review]", "workspace: {access: write, from: implement}"),
			pipelinetest.Container("ship", "needs: [polish]", "workspace: {access: read, from: polish}"))
		run := e.MustAdmit(p, def, nil)
		e.cancelAtEnd(run)
		e.X.Sources.History[gatestest.CommitC] = true

		e.Finish(e.MustClaim(tenant), gatestest.CommitB)
		g := e.RequestGate(run, "review")
		e.MustApprove(g)
		if rep := e.Reconcile(tenant); rep.Completed != 1 {
			t.Fatalf("consume: %+v", rep)
		}
		if got := e.Current(g); got.Subject.CandidateCommit() != gatestest.CommitB {
			t.Fatalf("candidate %s: the writer's published commit is the candidate", got.Subject.CandidateCommit())
		}
		// The writer itself acts on the approved candidate.
		e.X.Sources.Tip = gatestest.CommitB
		defer func() { e.X.Sources.Tip = gatestest.CommitA }()
		in := e.MustClaim(tenant)
		if in.StepID != "polish" {
			t.Fatalf("claimed %s", in.StepID)
		}
		e.Finish(in, gatestest.CommitC)
		before := e.X.Count(`SELECT count(*) FROM step_attempts WHERE run_id = $1 AND step_id = 'ship'`, string(run.ID))
		reservations := e.X.Count(`SELECT count(*) FROM execution_reservations WHERE run_id = $1`, string(run.ID))
		// The commit it produced was never approved: no gate covers it.
		if err := e.NothingRunnable(tenant); err != nil {
			t.Fatalf("a successor ran on a commit nobody approved: %v", err)
		}
		if n := e.X.Count(`SELECT count(*) FROM step_attempts WHERE run_id = $1 AND step_id = 'ship'`, string(run.ID)); n != before {
			t.Fatalf("the refused claim leaked an attempt: %d", n)
		}
		if n := e.X.Count(`SELECT count(*) FROM execution_reservations WHERE run_id = $1`, string(run.ID)); n != reservations {
			t.Fatalf("the refused claim leaked a reservation: %d -> %d", reservations, n)
		}
		if cur := e.Current(g); cur.Invalidation != nil || cur.Status != gates.StatusApproved {
			t.Fatalf("the earlier approval is history and stays untouched: %+v", cur)
		}
	})

	t.Run("an approval is never consumed after its run was cancelled", func(t *testing.T) {
		s := e.Reviewed(scope)
		e.MustApprove(s.Gate)
		before := e.Snapshot(s, "review")
		if _, err := e.Svc.Cancel(ctx, execution.Caller{PrincipalID: gatestest.Engineer, RequestID: "req_t073"}, scope, s.Run.ID); err != nil {
			t.Fatal(err)
		}
		out, err := e.Consume(s.Gate)
		if err != nil && !errors.Is(err, gates.ErrInvalidTransition) {
			t.Fatalf("consuming an approval of a cancelled run: %v", err)
		}
		if out.Completed {
			t.Fatal("an approval completed the gate step of a cancelled run")
		}
		if err := e.NothingRunnable(tenant); err != nil {
			t.Fatal(err)
		}
		after := e.Snapshot(s, "review")
		if after.StepStatus == string(execution.StepSucceeded) {
			t.Fatalf("the gate step succeeded after cancellation: %+v", after)
		}
		noCompute(t, "the cancelled run's approval", before, after)
		if cur := e.Current(s.Gate); cur.Status != gates.StatusApproved || cur.Decision == nil {
			t.Fatalf("the cancelled run's decision changed: %+v", cur)
		}
	})

	t.Run("changed conformance evidence", func(t *testing.T) {
		base := gatestest.NewBaseline(t, requiredChecks...)
		for _, point := range []string{"consumption", "protected claim"} {
			e.SetBaseline(&base)
			s, report := e.AgentGate(scope, base)
			e.MustApprove(s.Gate)
			if got := e.Current(s.Gate).Required.Conformance; got == nil || got.Status != gates.ConformanceVerified {
				t.Fatalf("%s: conformance %+v", point, got)
			}
			if point == "protected claim" {
				if out, err := e.Consume(s.Gate); err != nil || !out.Completed {
					t.Fatalf("consume: %v %+v", err, out)
				}
			}
			before := e.Snapshot(s, "review")
			// The report is replaced: the artifact the subject bound is gone.
			e.X.OwnerExec(`UPDATE artifacts SET disposition = 'removed', version = version + 1 WHERE id = $1`, string(report))
			switch point {
			case "consumption":
				if rep := e.Reconcile(tenant); rep.Completed != 0 || rep.Invalidated != 1 {
					t.Fatalf("%s: reconcile %+v", point, rep)
				}
			case "protected claim":
				if err := e.NothingRunnable(tenant); err != nil {
					t.Fatalf("%s: %v", point, err)
				}
				if rep := e.Reconcile(tenant); rep.Invalidated != 1 {
					t.Fatalf("%s: reconcile %+v", point, rep)
				}
			}
			cur := e.Current(s.Gate)
			if cur.Invalidation == nil || cur.Invalidation.Reason != gates.ReasonEvidenceChanged || cur.Decision == nil {
				t.Fatalf("%s: the invalidation of changed conformance evidence: %+v", point, cur)
			}
			noCompute(t, point, before, e.Snapshot(s, "review"))
		}
	})

	t.Run("duplicate decisions converge on one decision", func(t *testing.T) {
		race := func(t *testing.T, s Scenario, decisions ...gates.Decision) []error {
			t.Helper()
			var wg sync.WaitGroup
			errs := make([]error, len(decisions))
			start := make(chan struct{})
			for i, d := range decisions {
				i, d := i, d
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, errs[i] = e.Decide(s.Gate, gatestest.Reviewer, d)
				}()
			}
			close(start)
			wg.Wait()
			return errs
		}
		t.Run("identical approvals", func(t *testing.T) {
			s := e.Reviewed(scope)
			before := e.Snapshot(s, "review")
			errs := race(t, s, gates.DecisionApprove, gates.DecisionApprove, gates.DecisionApprove, gates.DecisionApprove)
			wins := 0
			for _, err := range errs {
				switch {
				case err == nil:
					wins++
				case errors.Is(err, gates.ErrVersionConflict), errors.Is(err, gates.ErrInvalidTransition), errors.Is(err, gates.ErrConflict):
				default:
					t.Fatalf("a duplicate decision: %v", err)
				}
			}
			after := e.Snapshot(s, "review")
			if wins < 1 || after.GateAudit != before.GateAudit+1 || after.GateOutbox != before.GateOutbox+1 || after.GateVersion != before.GateVersion+1 {
				t.Fatalf("%d winners; duplicates must leave one audit row and one event:\n before %+v\n after  %+v", wins, before, after)
			}
		})
		t.Run("approve against reject", func(t *testing.T) {
			s := e.Reviewed(scope)
			before := e.Snapshot(s, "review")
			errs := race(t, s, gates.DecisionApprove, gates.DecisionReject)
			if (errs[0] == nil) == (errs[1] == nil) {
				t.Fatalf("exactly one decision may win: %v / %v", errs[0], errs[1])
			}
			after := e.Snapshot(s, "review")
			cur := e.Current(s.Gate)
			want := gates.StatusApproved
			if errs[1] == nil {
				want = gates.StatusRejected
			}
			if cur.Status != want || after.GateAudit != before.GateAudit+1 || after.GateOutbox != before.GateOutbox+1 {
				t.Fatalf("the loser changed the winner's record: %+v\n before %+v\n after  %+v", cur, before, after)
			}
			// An immutable decision is never flipped by a later one.
			if _, err := e.Decide(cur, gatestest.Reviewer, gates.DecisionApprove); err == nil && want == gates.StatusRejected {
				t.Fatal("a rejected gate became approved")
			}
		})
		t.Run("keyed duplicate replays the stored response", func(t *testing.T) {
			s := e.Reviewed(scope)
			before := e.Snapshot(s, "review")
			cmd := DecideCmd(s.Gate, gates.DecisionApprove)
			first, err := e.decideKeyed(ctx, s.Gate, gatestest.Reviewer, "key-t073-1", `{"x":1}`, cmd)
			if err != nil || first.Replayed {
				t.Fatalf("first: %+v %v", first, err)
			}
			second, err := e.decideKeyed(ctx, s.Gate, gatestest.Reviewer, "key-t073-1", `{"x":1}`, cmd)
			if err != nil || !second.Replayed || string(second.Body) != string(first.Body) {
				t.Fatalf("replay: %+v %v", second, err)
			}
			after := e.Snapshot(s, "review")
			if after.GateAudit != before.GateAudit+1 || after.GateOutbox != before.GateOutbox+1 || after.GateVersion != before.GateVersion+1 {
				t.Fatalf("the replay decided again:\n before %+v\n after  %+v", before, after)
			}
			// A revoked reviewer is not given the stored response.
			e.X.Revoke(scope, gatestest.Reviewer)
			defer func() {
				e.X.Grant(scope, gatestest.Reviewer, identity.RoleReviewer)
				e.X.Grant(ScopeA2, gatestest.Reviewer, identity.RoleReviewer)
			}()
			if r, err := e.decideKeyed(ctx, s.Gate, gatestest.Reviewer, "key-t073-1", `{"x":1}`, cmd); err == nil || len(r.Body) != 0 {
				t.Fatalf("the stored response was released to a revoked reviewer: %+v %v", r, err)
			}
		})
		t.Run("duplicate consumption and duplicate claims", func(t *testing.T) {
			s := e.Reviewed(scope)
			e.MustApprove(s.Gate)
			var wg sync.WaitGroup
			start := make(chan struct{})
			outs := make([]gates.StepOutcome, 4)
			for i := range outs {
				i := i
				wg.Add(1)
				go func() { defer wg.Done(); <-start; outs[i], _ = e.Consume(s.Gate) }()
			}
			close(start)
			wg.Wait()
			if n := e.Transitions(s.Run, "review"); n != 1 {
				t.Fatalf("%d step transitions after racing consumptions", n)
			}
			before := e.Snapshot(s, "review")
			noCompute(t, "racing consumptions", Snap{Reservations: before.Reservations}, before)
			var claims []*execution.AttemptSpec
			var mu sync.Mutex
			start = make(chan struct{})
			for i := 0; i < 3; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					if in, err := e.ClaimErr(tenant); err == nil {
						mu.Lock()
						claims = append(claims, in)
						mu.Unlock()
					} else if !errors.Is(err, execution.ErrNothingRunnable) {
						t.Errorf("claim: %v", err)
					}
				}()
			}
			close(start)
			wg.Wait()
			after := e.Snapshot(s, "review")
			if len(claims) != 1 || after.Attempts != 1 || after.Started != 1 || after.WriterSlots != 1 || after.Reservations != before.Reservations+1 {
				t.Fatalf("%d claims; one winner expected: %+v", len(claims), after)
			}
		})
	})

	t.Run("pooled tenant reuse over one connection", func(t *testing.T) {
		f := New(t, Options{SingleConn: true})
		f.GrantAll()
		f.X.Grant(ScopeB, gatestest.Engineer, identity.RoleEngineer)
		if f.Pool.Stat().MaxConns() != 1 {
			t.Fatalf("the composition pool has %d connections", f.Pool.Stat().MaxConns())
		}
		clean := func(step string) {
			t.Helper()
			if setting, visible := f.PoolContext(); setting != "" || visible != 0 {
				t.Fatalf("after %s the pooled connection carries tenant %q and shows %d gates", step, setting, visible)
			}
		}
		clean("setup")
		reviewerB := "u034_reviewer_b"

		// Tenant A: request, decision, consumption, claim: every join runs on the
		// one connection, so none of them opened a second transaction.
		a := f.Reviewed(ScopeA)
		clean("A's request")
		f.MustApprove(a.Gate)
		clean("A's decision")
		if out, err := f.Consume(a.Gate); err != nil || !out.Completed {
			t.Fatalf("A consume: %v %+v", err, out)
		}
		clean("A's consumption")
		if in, err := f.ClaimErr(ScopeA.TenantID); err != nil || in.Ordinal != 1 {
			t.Fatalf("A claim: %v %+v", err, in)
		}
		clean("A's claim")

		// Tenant B on the same connection: a rolled back decision, then a success.
		b := f.Reviewed(ScopeB)
		clean("B's request")
		f.F.Commit(2)
		if _, err := f.Decide(b.Gate, reviewerB, gates.DecisionApprove); err == nil {
			t.Fatal("B's decision committed through a failing commit")
		}
		f.F.Clear()
		clean("B's rolled back decision")
		if _, err := f.Decide(b.Gate, reviewerB, gates.DecisionApprove); err != nil {
			t.Fatalf("B's retry: %v", err)
		}
		clean("B's decision")
		f.F.Fail(PointAuthority)
		if _, err := f.Consume(b.Gate); err == nil {
			t.Fatal("B's consumption succeeded through the fault")
		}
		f.F.Clear()
		clean("B's rolled back consumption")
		if out, err := f.Consume(b.Gate); err != nil || !out.Completed {
			t.Fatalf("B consume: %v %+v", err, out)
		}
		if in, err := f.ClaimErr(ScopeB.TenantID); err != nil || in.Ordinal != 1 {
			t.Fatalf("B claim: %v %+v", err, in)
		}
		clean("B's claim")

		// A deployment-bearing gate: the binder's SQL read runs on the one
		// connection inside the decision, consumption and claim transactions; a
		// read that opened its own transaction would wait for the connection its
		// caller holds.
		d := f.DeploymentGated(ScopeA, "pooled-dep", "dep_t073_pooled", "config-1")
		clean("A's deployment gate request")
		f.F.Reset()
		f.MustApprove(d.Gate)
		sameXid(t, "the deployment decision", f.F.Trace())
		if !has(f.F.Trace(), PointDeployment) {
			t.Fatalf("the decision never read the configuration: %+v", f.F.Trace())
		}
		clean("A's deployment decision")
		f.F.Reset()
		if out, err := f.Consume(d.Gate); err != nil || !out.Completed {
			t.Fatalf("deployment consume: %v %+v", err, out)
		}
		sameXid(t, "the deployment consumption", f.F.Trace())
		clean("A's deployment consumption")
		f.F.Reset()
		if in, err := f.ClaimErr(ScopeA.TenantID); err != nil || in.StepID != "promote" {
			t.Fatalf("deployment claim: %v %+v", err, in)
		}
		sameXid(t, "the deployment claim", f.F.Trace())
		clean("A's deployment claim")

		// Authority and visibility never cross tenants.
		if _, err := f.Decide(a.Gate, reviewerB, gates.DecisionApprove); err == nil {
			t.Fatal("tenant B's reviewer decided tenant A's gate")
		}
		if _, err := f.Store.Get(ctx, ScopeB, a.Gate.ID); !errors.Is(err, gates.ErrNotFound) {
			t.Fatalf("A's gate under B's scope: %v", err)
		}
		if _, err := f.Store.Get(ctx, domain.Scope{TenantID: ScopeA.TenantID, ProjectID: ScopeB.ProjectID}, a.Gate.ID); !errors.Is(err, gates.ErrNotFound) {
			t.Fatalf("A's gate under another project: %v", err)
		}
		if _, err := f.Store.CompleteStep(ctx, ScopeB, a.Gate.ID); !errors.Is(err, gates.ErrNotFound) {
			t.Fatalf("B's scope completed A's gate step: %v", err)
		}
		clean("the foreign attempts")

		for _, tn := range []domain.TenantID{ScopeA.TenantID, ScopeB.TenantID} {
			err := storage.WithTenant(ctx, f.Pool, tn, func(ctx context.Context, tx pgx.Tx) error {
				for _, table := range []string{"gates", "gate_evidence", "runs", "step_runs", "step_attempts", "run_history", "execution_reservations", "outbox", "audit", "artifact_pins"} {
					var n, foreign int
					if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE tenant_id <> $1) FROM `+table, string(tn)).Scan(&n, &foreign); err != nil {
						return err
					}
					if foreign != 0 {
						t.Errorf("tenant %s sees %d foreign rows of %s", tn, foreign, table)
					}
					if (table == "gates" || table == "runs") && n == 0 {
						t.Errorf("tenant %s sees no %s of its own", tn, table)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		var super, bypass bool
		var owned int
		must(t, f.Pool.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass))
		must(t, f.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tableowner = current_user`).Scan(&owned))
		if super || bypass || owned != 0 {
			t.Fatalf("the runtime role is superuser=%v bypassrls=%v and owns %d tables", super, bypass, owned)
		}
		var forced int
		f.X.Scalar(&forced, `SELECT count(*) FROM pg_class WHERE relname IN ('gates', 'gate_evidence', 'runs', 'step_runs', 'step_attempts', 'run_history') AND relrowsecurity AND relforcerowsecurity`)
		if forced != 6 {
			t.Fatalf("%d of 6 tables force row-level security", forced)
		}
	})
}

var requiredChecks = []string{"module-boundaries", "tenant-authority"}

func passedChecks(support []domain.ArtifactID) []runtimev1.ConformanceCheck {
	var out []runtimev1.ConformanceCheck
	for i, id := range requiredChecks {
		var ev []domain.ArtifactID
		if i < len(support) {
			ev = []domain.ArtifactID{support[i]}
		}
		if len(ev) == 0 {
			ev = []domain.ArtifactID{"art_none"}
		}
		out = append(out, runtimev1.ConformanceCheck{ID: id, Outcome: runtimev1.CheckPassed, EvidenceArtifactIDs: ev})
	}
	return out
}

// AgentGate is implement(agent writer, declared conformance report) ->
// review(gate) -> deploy, up to a requested gate with verified conformance.
func (e *Env) AgentGate(scope domain.Scope, base runtimev1.DesignBaseline) (Scenario, domain.ArtifactID) {
	e.T.Helper()
	e.quiesceUnlessKept()
	p := e.X.NewProject(scope)
	def := e.Define(scope,
		gatestest.Agent("implement", "outputs: {conformance: {type: artifact, path: /kodama/outputs/conformance.json}}"),
		gatestest.Gate("review", []string{"implement"}),
		pipelinetest.Container("deploy", "needs: [review]", "workspace: {access: read, from: implement}"))
	run := e.MustAdmit(p, def, nil)
	e.cancelAtEnd(run)
	e.X.Sources.History[gatestest.CommitB] = true
	in := e.MustClaim(scope.TenantID)
	var support []domain.ArtifactID
	for i := 0; i < 2; i++ {
		support = append(support, e.Artifact(in, "notes-"+string(rune('a'+i)), []byte("review notes "+string(rune('a'+i))), artifacts.RoleOutput))
	}
	data, err := json.Marshal(gatestest.Report(base, gatestest.CommitB, passedChecks(support)...))
	must(e.T, err)
	ids := e.Finish(in, gatestest.CommitB, gatestest.Output{Name: "conformance", Data: data})
	return Scenario{P: p, Run: run, Pre: "implement", Gate: e.RequestGate(run, "review")}, ids["conformance"]
}

type keyedResult struct {
	Replayed bool
	Body     []byte
	Gate     *gates.Gate
}

// decideKeyed runs one decision through the keyed (idempotent) transaction the
// HTTP handler uses.
func (e *Env) decideKeyed(ctx context.Context, g *gates.Gate, principal, key, body string, cmd gates.DecideCommand) (keyedResult, error) {
	caller := gatestest.Caller(principal)
	plan, err := e.Gates.PrepareDecision(ctx, caller, g.Scope, g.ID, cmd)
	if err != nil {
		return keyedResult{}, err
	}
	sum := sha256.Sum256([]byte(body))
	id := httpapi.RequestIdentity{
		Principal: httpapi.Principal{ID: principal}, TenantID: g.Scope.TenantID, ProjectID: g.Scope.ProjectID, Method: "POST",
		Route: "/api/v1/tenants/" + string(g.Scope.TenantID) + "/projects/" + string(g.Scope.ProjectID) + "/gates/" + string(g.ID) + "/decisions",
		Key:   httpapi.ClientKey(key), Hash: httpapi.RequestHash(sum),
	}
	authorize := func(ctx context.Context, _ httpapi.Reference) error {
		return e.Gates.Authorize(ctx, caller, g.Scope, gates.PermDecide)
	}
	render := func(g *gates.Gate) (httpapi.Outcome, error) {
		raw, _ := json.Marshal(map[string]any{"id": g.ID, "status": g.Status, "version": g.Version})
		return httpapi.Outcome{Response: httpapi.StoredResponse{Status: 200, Body: raw}, Target: httpapi.Reference{Kind: "gate", ID: string(g.ID)}}, nil
	}
	resp, replayed, decided, err := e.Keyed.DecideKeyed(ctx, id, authorize, plan, render)
	if err != nil {
		return keyedResult{}, e.Gates.AfterRefusal(ctx, plan, err)
	}
	return keyedResult{Replayed: replayed, Body: resp.Body, Gate: decided}, nil
}
