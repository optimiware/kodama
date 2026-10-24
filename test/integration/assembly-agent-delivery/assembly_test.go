package assemblyagentdelivery_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	kit "github.com/optimiware/kodama/test/e2e/m2/kit"
)

// TestAgentDeliveryAssembly (agent_delivery_assembly) is the join of the agent,
// gate and delivery modules on the assembled core, over real PostgreSQL (the
// runtime role under forced row level security), a real Git host and registry,
// Podman steps and a kind namespace:
//
//   - an agent-produced candidate is approvable only with design conformance
//     evidence the CORE derives from immutable run data: a missing report fails
//     the agent step, and a blocked, wrong-commit, wrong-baseline or uncited
//     report leaves a gate that refuses approval with the specific code, starts
//     no build and still permits a human to reject;
//   - a gate bound to a deployment snapshots that deployment's record (id and
//     configuration digest), and tearing the deployment down makes the pending
//     decision definitively stale;
//   - the candidate commit is one fact across the gate, the registered image,
//     the deployment and its attempt, and a second tenant's runtime role sees
//     none of those rows.
//
// Covers AR-A07, GM-A04, GM-A05, DQ-A02, DQ-A03, DQ-A06, DQ-02, DQ-06, DQ-11,
// SD-11 and PX-13.
func TestAgentDeliveryAssembly(t *testing.T) {
	e := kit.New(t, kit.Options{Delivery: true, NoProfile: true})
	p := e.NewERP(t, kit.ERPSetup{Label: "assembly"})

	t.Run("a conformance report the core cannot accept blocks approval, not rejection", func(t *testing.T) {
		cases := []struct{ mode, code string }{
			{"blocked", "conformance_blocked"},
			{"wrongcommit", "conformance_candidate_mismatch"},
			{"wrongbaseline", "conformance_baseline_mismatch"},
			{"uncited", "conformance_malformed"},
		}
		// The four candidates are produced concurrently by the one installation; each is then
		// examined on its own.
		runs := map[string]client.Run{}
		for _, c := range cases {
			runs[c.mode] = p.StartWith("conf-"+c.mode, e.Item("Conformance "+c.mode, "# conformance "+c.mode), c.mode)
		}
		for _, c := range cases {
			run := runs[c.mode]
			g := e.WaitGate(run.Id, 3*time.Minute)
			if st := e.StepOf(run.Id, "implement"); st.Status != client.StepStatusSucceeded || st.SourceCommit == nil {
				t.Fatalf("%s: implement %+v", c.mode, st)
			}
			code, body := e.Decide("reviewer", g, "approve", "approve despite the report", "conf-a-"+c.mode)
			if code != http.StatusUnprocessableEntity || kit.ErrorCode(body) != c.code {
				t.Fatalf("%s: approve = %d %s, want 422 %s", c.mode, code, body, c.code)
			}
			if n := e.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1 AND step_id = 'build'`, run.Id); n != 0 {
				t.Fatalf("%s: %d build attempts after a refused approval", c.mode, n)
			}
			if got := e.GetGate("reviewer", g.Id); got.Status != "pending" {
				t.Fatalf("%s: a refused approval changed the gate to %s", c.mode, got.Status)
			}
			if got := strings.Join(e.GateEvents(g.Id), ","); got != "gate.requested" {
				t.Fatalf("%s: gate events %q after a refusal", c.mode, got)
			}
			// Rejection needs no conformance: the human can always say no.
			if code, body := e.Decide("reviewer", g, "reject", "does not conform", "conf-r-"+c.mode); code != http.StatusOK {
				t.Fatalf("%s: reject = %d %s", c.mode, code, body)
			}
			if f := e.World.WaitRun(run.Id, 3*time.Minute); f.Status != client.RunStatusFailed {
				t.Fatalf("%s: rejected run is %s (%s)", c.mode, f.Status, f.Reason)
			}
			if n := e.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1 AND step_id = 'build'`, run.Id); n != 0 {
				t.Fatalf("%s: the rejected candidate was built", c.mode)
			}
		}
	})

	t.Run("a candidate with no conformance report never reaches a gate", func(t *testing.T) {
		item := e.Item("Conformance missing", "# conformance missing")
		run := p.StartWith("conf-missing", item, "missing")
		f := e.World.WaitRun(run.Id, 4*time.Minute)
		if f.Status != client.RunStatusFailed {
			t.Fatalf("run %s (%s)", f.Status, f.Reason)
		}
		if st := e.StepOf(run.Id, "implement"); st.Status != client.StepStatusFailed {
			t.Fatalf("implement %+v: a declared artifact that was never written must fail the step", st)
		}
		if logs := strings.Join(e.AttemptLogs("engineer", run.Id, e.StepOf(run.Id, "implement").Attempts[0].Id), "\n"); !strings.Contains(logs, "agent: tool call ok") {
			t.Fatalf("the agent did not do its work before the report went missing:\n%s", logs)
		}
		if gs := e.Gates("reviewer", run.Id); len(gs) != 0 {
			t.Fatalf("%d gates for an agent step that produced no report", len(gs))
		}
	})

	t.Run("a gate bound to a deployment goes stale when the deployment is torn down", func(t *testing.T) {
		steps := e.PreviewSteps(p.Opts)
		gate := kit.GateStep("accept", nil, "preview")
		gate["gate"] = map[string]any{"deploymentId": "${{ steps.preview.outputs.deployment_id }}", "evidence": []any{}}
		def := e.Register("inventory-accept", kit.PreviewInputs(), append(steps, gate)...)

		item := e.Item("Deployment bound", "# deployment bound")
		cand := p.HumanCandidate("cand-bound", item)
		run := e.StartRunAt("bound-run", item, def.Id, cand.Commit, map[string]any{"image": cand.ImageID, "image_ref": cand.ImageRef, "commit": cand.Commit})
		g := e.WaitGate(run.Id, 6*time.Minute)

		var dep client.Deployment
		for _, d := range e.Deployments() {
			if d.RunId == run.Id {
				dep = d
			}
		}
		if dep.Id == "" || dep.Status != "succeeded" {
			t.Fatalf("deployment %+v", dep)
		}
		if g.Subject.DeploymentId == nil || *g.Subject.DeploymentId != dep.Id || g.Subject.DeploymentConfigDigest == nil || string(*g.Subject.DeploymentConfigDigest) != string(dep.ConfigurationDigest) ||
			g.Subject.CandidateCommit != cand.Commit {
			t.Fatalf("the gate subject %+v does not name deployment %s / %s at %s", g.Subject, dep.Id, dep.ConfigurationDigest, cand.Commit)
		}

		// Tear the deployment down while the decision is pending.
		code, body := e.Do("engineer", "POST", e.ProjectPath()+"/deployments/"+dep.Id+"/cleanup", nil, map[string]string{"If-Match": fmt.Sprintf(`"%d"`, dep.Version)})
		if code >= 300 {
			t.Fatalf("request cleanup: %d %s", code, body)
		}
		kit.WaitFor(t, "the deployment to be cleaned", 3*time.Minute, func() bool {
			for _, d := range e.Deployments() {
				if d.Id == dep.Id {
					return d.DeletedAt != nil
				}
			}
			return false
		})
		code, body = e.Decide("reviewer", g, "approve", "accept the torn down preview", "bound-approve")
		if code < 400 {
			t.Fatalf("a decision on a gate bound to a deleted deployment was accepted: %d %s", code, body)
		}
		t.Logf("approve after teardown: %d %s", code, kit.ErrorCode(body))
		got := e.GetGate("reviewer", g.Id)
		if got.Status == "approved" || got.Applicability.Applicable || got.Invalidation == nil || got.Applicability.Reason == nil {
			t.Fatalf("the gate after the teardown is %s, applicable=%v, invalidation=%v: it must be definitively stale", got.Status, got.Applicability.Applicable, got.Invalidation)
		}
		t.Logf("applicability reason after teardown: %s", *got.Applicability.Reason)
		if f := e.World.GetRun(run.Id); f.Status == client.RunStatusSucceeded {
			t.Fatal("the run succeeded on an approval of a deployment that no longer exists")
		}
	})

	t.Run("the candidate is one fact across the gate, image, deployment and tenant boundary", func(t *testing.T) {
		item := e.Item("Joined", "# joined")
		run := p.Start("joined-candidate", item)
		g := e.WaitGate(run.Id, 3*time.Minute)
		if code, b := e.Decide("reviewer", g, "approve", "reviewed", "joined-approve"); code != http.StatusOK {
			t.Fatalf("approve: %d %s", code, b)
		}
		final := e.World.WaitRun(run.Id, 5*time.Minute)
		if final.Status != client.RunStatusSucceeded {
			t.Fatalf("candidate run %s (%s)", final.Status, final.Reason)
		}
		pv := p.Preview("joined-preview", item, final)
		if pf := e.World.WaitRun(pv.Id, 6*time.Minute); pf.Status != client.RunStatusSucceeded {
			t.Fatalf("preview %s (%s)", pf.Status, pf.Reason)
		}
		commit := *e.StepOf(run.Id, "implement").SourceCommit
		if g.Subject.CandidateCommit != commit {
			t.Fatalf("the gate approved %s, the agent published %s", g.Subject.CandidateCommit, commit)
		}
		var facts int
		e.MustScan(&facts, `SELECT count(*) FROM deployments d
			JOIN gates g ON g.id = $1 AND g.candidate_commit = d.source_commit
			JOIN artifacts a ON a.id = d.image_artifact_id AND a.source_commit = d.source_commit
			JOIN step_attempts at ON at.id = d.attempt_id
			WHERE d.run_id = $2`, g.Id, pv.Id)
		if facts != 1 {
			t.Fatalf("the gate, the registered image and the deployment do not share commit %s (%d joined rows)", commit, facts)
		}
		// Tenant B's runtime role, under forced row level security, sees none of tenant A's rows.
		for _, q := range []string{
			`SELECT count(*) FROM gates`, `SELECT count(*) FROM deployments`, `SELECT count(*) FROM artifacts`,
			`SELECT count(*) FROM step_attempts`, `SELECT count(*) FROM model_requests`, `SELECT count(*) FROM mcp_invocations`,
		} {
			if a := e.TenantCount(kit.TenantA, q); a == 0 {
				t.Fatalf("tenant A has no rows for %q: the isolation check proves nothing", q)
			}
			if b := e.TenantCount(kit.TenantB, q); b != 0 {
				t.Fatalf("tenant B's role sees %d rows for %q", b, q)
			}
		}
	})
}
