package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

func pinCount(e *gatestest.Env, g gates.ID) int {
	return count(e, `SELECT count(*) FROM artifact_pins WHERE owner_kind = 'gate' AND owner_id = $1`, string(g))
}

func gateCount(e *gatestest.Env, run *execution.Run) int {
	return count(e, `SELECT count(*) FROM gates WHERE run_id = $1`, string(run.ID))
}

// TestEvidenceFailureRecovery (DQ-01, DQ-02, DQ-A01, PX-05): incomplete or
// removed artifacts, a wrong producer or source commit, foreign evidence, byte
// and digest mismatches, an object-store outage and interrupted pin preparation
// fail closed; outages and interruptions recover safely and repeatably without
// duplicate gates, pins or events.
func TestEvidenceFailureRecovery(t *testing.T) {
	faults := &gatestest.Faults{}
	e := gatestest.New(t, gatestest.Options{S3: true, WrapObjects: faults.Objects, WrapEvidence: faults.Evidence, WrapPins: faults.Pins, WrapEvents: faults.Appender})
	e.GrantAll()
	ctx := context.Background()
	base := gatestest.NewBaseline(t, requiredChecks...)
	scope := gatestest.ScopeA
	tenant := scope.TenantID

	t.Run("removed evidence is refused and nothing is pinned", func(t *testing.T) {
		p, run := reviewed(t, e)
		ids := finishTest(t, e, p)
		e.X.OwnerExec(`UPDATE artifacts SET disposition = 'removed', version = version + 1 WHERE id = $1`, string(ids["report"]))
		rep := reconcile(t, e, tenant)
		if rep.Requested != 0 || rep.Blocked != 1 {
			t.Fatalf("reconcile: %+v", rep)
		}
		if gateCount(e, run) != 0 {
			t.Fatalf("a gate exists over removed evidence")
		}
		if n := count(e, `SELECT count(*) FROM artifact_pins WHERE artifact_id = $1`, string(ids["report"])); n != 0 {
			t.Fatalf("%d pins on removed evidence", n)
		}
		if got := stepStatus(t, e, run, "review"); got != execution.StepPending {
			t.Fatalf("gate step %s", got)
		}
	})

	t.Run("evidence from another source commit than the candidate is refused", func(t *testing.T) {
		e.X.Sources.History[gatestest.CommitB] = true
		// test reads the original commit; implement then publishes a new one. The
		// test report assessed a different commit than the candidate.
		p := e.X.NewProject(scope)
		def := e.Define(scope,
			pipelinetest.Container("test", readRun, reportOutput),
			pipelinetest.Container("implement", "needs: [test]", writeRun, "outputs: {notes: {type: artifact, path: /kodama/outputs/notes.txt}}"),
			gatestest.Gate("stale-review", []string{"implement"}, "${{ steps.test.outputs.report }}"),
			gatestest.Gate("fresh-review", []string{"implement"}, "${{ steps.implement.outputs.notes }}"))
		run := e.MustAdmit(p, def, nil)
		cancelAtEnd(t, e, run)
		e.Finish(e.MustClaim(tenant), "", gatestest.Output{Name: "report", Data: []byte("tested the original commit")})
		e.Finish(e.MustClaim(tenant), gatestest.CommitB, gatestest.Output{Name: "notes", Data: []byte("what the writer changed")})
		rep := reconcile(t, e, tenant)
		if rep.Requested != 1 || rep.Blocked != 1 {
			t.Fatalf("reconcile: %+v", rep)
		}
		if _, err := e.Store.ByStep(ctx, scope, run.ID, "stale-review"); !errors.Is(err, gates.ErrNotFound) {
			t.Fatalf("a gate exists over evidence of another commit: %v", err)
		}
		// The candidate producer's own artifacts are consistent with the candidate.
		fresh := gateOf(t, e, run, "fresh-review")
		if fresh.Subject.CandidateCommit() != gatestest.CommitB {
			t.Fatalf("candidate %s", fresh.Subject.CandidateCommit())
		}
		// The refused gate pinned nothing.
		if n := pinCount(e, gates.GateIDFor(scope, run.ID, "stale-review")); n != 0 {
			t.Fatalf("%d pins for a refused gate", n)
		}
	})

	t.Run("an object-store outage leaves no gate and recovers", func(t *testing.T) {
		e.SetBaseline(&base)
		run := startAgent(t, e, agentFlow{declare: true, supports: 2, report: goodReport})
		faults.FailObjects(errors.New("s3 unreachable"))
		rep := reconcile(t, e, tenant)
		if rep.Requested != 0 || rep.Failed != 1 || gateCount(e, run) != 0 {
			t.Fatalf("during the outage: %+v", rep)
		}
		if n := count(e, `SELECT count(*) FROM step_runs WHERE run_id = $1 AND gate_ref IS NOT NULL`, string(run.ID)); n != 0 {
			t.Fatalf("a step was bound without a gate")
		}
		faults.FailObjects(nil)
		rep = reconcile(t, e, tenant)
		if rep.Requested != 1 {
			t.Fatalf("after recovery: %+v", rep)
		}
		g := gateOf(t, e, run, "review")
		if g.Required.Conformance == nil || g.Required.Conformance.Status != gates.ConformanceVerified {
			t.Fatalf("snapshot after recovery %+v", g.Required.Conformance)
		}
	})

	t.Run("an artifact-metadata outage leaves no gate", func(t *testing.T) {
		p, run := reviewed(t, e)
		finishTest(t, e, p)
		faults.FailEvidence(errors.New("database down"))
		rep := reconcile(t, e, tenant)
		if rep.Requested != 0 || rep.Failed != 1 || gateCount(e, run) != 0 {
			t.Fatalf("during the outage: %+v", rep)
		}
		faults.FailEvidence(nil)
		if rep := reconcile(t, e, tenant); rep.Requested != 1 {
			t.Fatalf("after recovery: %+v", rep)
		}
	})

	t.Run("report bytes that do not match their digest, or are gone, block approval", func(t *testing.T) {
		e.SetBaseline(&base)
		for name, damage := range map[string]func(key string){
			"tampered": func(key string) {
				_ = e.Objects.Delete(ctx, key)
				e.PutObject(key, []byte(`{"disposition":"conforms"}`))
			},
			"deleted": func(key string) { _ = e.Objects.Delete(ctx, key) },
		} {
			run := startAgent(t, e, agentFlow{declare: true, supports: 2, report: goodReport})
			var key string
			e.X.Scalar(&key, `SELECT sealed_key FROM artifacts WHERE run_id = $1 AND name = 'conformance'`, string(run.ID))
			damage(key)
			if rep := reconcile(t, e, tenant); rep.Requested != 1 {
				t.Fatalf("%s: %+v", name, rep)
			}
			g := gateOf(t, e, run, "review")
			if c := g.Required.Conformance; c == nil || c.Status != gates.ConformanceBlocked || c.Code != gates.CodeConformanceEvidenceInvalid {
				t.Fatalf("%s: snapshot %+v", name, c)
			}
			if _, err := approve(t, e, g, gatestest.Reviewer); !errors.Is(err, gates.ErrConformance) {
				t.Fatalf("%s: approval %v", name, err)
			}
		}
	})

	t.Run("interrupted pin preparation recovers without duplicates", func(t *testing.T) {
		e.SetBaseline(&base)
		run := startAgent(t, e, agentFlow{declare: true, supports: 2, report: goodReport})
		faults.FailPinsAfter(1)
		rep := reconcile(t, e, tenant)
		if rep.Requested != 0 || rep.Failed != 1 || gateCount(e, run) != 0 {
			t.Fatalf("interrupted: %+v", rep)
		}
		id := gates.GateIDFor(scope, run.ID, "review")
		if n := pinCount(e, id); n != 1 {
			t.Fatalf("%d pins after the interruption, want 1 partial pin", n)
		}
		faults.FailPinsAfter(0)
		if rep := reconcile(t, e, tenant); rep.Requested != 1 {
			t.Fatalf("recovery: %+v", rep)
		}
		g := gateOf(t, e, run, "review")
		if g.ID != id {
			t.Fatalf("the gate identity is not stable: %s != %s", g.ID, id)
		}
		if n := pinCount(e, id); n != 3 {
			t.Fatalf("%d pins after recovery, want the report and two supporting artifacts", n)
		}
		if rep := reconcile(t, e, tenant); rep.Requested != 0 {
			t.Fatalf("repeat: %+v", rep)
		}
		if n := pinCount(e, id); n != 3 || gateCount(e, run) != 1 {
			t.Fatalf("repeat left %d pins and %d gates", n, gateCount(e, run))
		}
	})

	t.Run("a failure between pinning and the gate transaction rolls back and recovers", func(t *testing.T) {
		p, run := reviewed(t, e)
		finishTest(t, e, p)
		faults.FailEvents(errors.New("event backlog full"), "gate.requested")
		rep := reconcile(t, e, tenant)
		if rep.Requested != 0 || rep.Failed != 1 || gateCount(e, run) != 0 {
			t.Fatalf("failed append: %+v", rep)
		}
		if n := count(e, `SELECT count(*) FROM step_runs WHERE run_id = $1 AND gate_ref IS NOT NULL`, string(run.ID)); n != 0 {
			t.Fatalf("the step stayed bound after the rollback")
		}
		faults.FailEvents(nil)
		if rep := reconcile(t, e, tenant); rep.Requested != 1 {
			t.Fatalf("recovery: %+v", rep)
		}
		g := gateOf(t, e, run, "review")
		if n := outboxCount(e, "gate.requested", g.ID); n != 1 {
			t.Fatalf("%d gate.requested events", n)
		}
		if n := pinCount(e, g.ID); n != 1 {
			t.Fatalf("%d pins", n)
		}
	})
}
