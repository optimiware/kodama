package m2_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	kit "github.com/optimiware/kodama/test/e2e/m2/kit"
)

// TestM2AgentReviewBuild is the successful agent/test/review/build fixture of
// the ERP pipeline in the nearest form the assembled core runs on the Podman
// runtime (docs/acceptance/m2.md, "What the assembled core cannot run yet": no
// model or tool call, no source-bound evidence, no preview step). It is not
// `m2_agent_preview`. An agent step of an immutable profile is admitted,
// dispatched and finalized; the review gate then WAITS WITHOUT COMPUTE for one
// human; only a current reviewer decides (role, expected version, subject
// digest), the decision is immutable and keyed replays converge; the approved
// decision, not the event, moves the step once; and the agent's own runtime
// credential is no human credential (AR-A07, AR-12). Covers AR-A07, DQ-A02,
// DQ-02, DQ-03, DQ-13's neighbour (a passing job completes nothing: the run's
// work item is untouched), PX-13 and GM-A01's authority half.
func TestM2AgentReviewBuild(t *testing.T) {
	e := kit.New(t, kit.Options{})
	def := e.ReviewPipeline("agent-review-build")
	item := e.Item("Warehouse adjustment audit", "# Spec\nAudited resolution of inventory gaps.")
	events := e.OpenEvents("engineer", e.World.ProjectEvents(), "")
	events.Until(t, "the snapshot", 10*time.Second, func(f kit.Frame) bool { return f.Event == "snapshot" })

	in := e.AgentRunInputs("completed")
	in["probe"] = e.ProjectPath() + "/gates/gate_none/decisions" // the agent tries to decide a gate with its own token
	run := e.StartRun("arb-1", item, def.Id, in)
	g := e.WaitGate(run.Id, 2*time.Minute)

	// ---- the agent step ran as an ordinary attempt and its credential decided nothing ----
	impl := e.StepOf(run.Id, "implement")
	if impl.Status != client.StepStatusSucceeded || len(impl.Attempts) != 1 || impl.Attempts[0].Access != "none" || !impl.Attempts[0].Settled {
		t.Fatalf("implement %+v", impl)
	}
	if got := kit.Output(impl, "trace"); got != "p401" {
		t.Fatalf("the agent's runtime token against the public API answered %q, want p401", got)
	}
	if g.Status != client.GateStatus("pending") {
		t.Fatalf("gate %s", g.Status)
	}

	// ---- the gate waits without compute ----
	e.World.WaitRemoved(impl.Attempts[0].Id)
	if n := e.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1 AND step_id IN ('review', 'build')`, run.Id); n != 0 {
		t.Fatalf("%d attempts exist for the waiting gate and its successor", n)
	}
	if st := e.StepOf(run.Id, "review"); st.Status != client.StepStatusPending || len(st.Attempts) != 0 {
		t.Fatalf("review step %+v", st)
	}
	time.Sleep(2 * time.Second) // several reconcile passes: still no compute, no progress
	if n := e.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, run.Id); n != 2 {
		t.Fatalf("%d attempts while the gate waits, want the agent's and the test's", n)
	}

	// ---- the subject the reviewer sees: this commit, this revision, this definition ----
	sub := g.Subject
	if sub.CandidateCommit != e.Repo.Initial || sub.ContentRevision != 1 || sub.DefinitionId != def.Id || sub.WorkItemId != item.Id ||
		sub.TenantId != kit.TenantA || sub.ProjectId != e.World.Project || len(sub.Evidence) != 0 || !strings.HasPrefix(string(g.SubjectDigest), "sha256:") {
		t.Fatalf("subject %+v digest %s", sub, g.SubjectDigest)
	}
	if g.Applicability.Applicable || g.Decision != nil || g.Invalidation != nil {
		t.Fatalf("a pending gate is applicable or decided: %+v", g)
	}

	// ---- who may decide, and on what (DQ-03) ----
	refused := []struct {
		what    string
		persona string
		version int64
		digest  string
		code    int
		err     string
	}{
		{"an engineer holds no gate:decide", "engineer", 1, string(g.SubjectDigest), http.StatusForbidden, "forbidden"},
		{"a viewer holds no gate:decide", "viewer", 1, string(g.SubjectDigest), http.StatusForbidden, "forbidden"},
		{"an administrator is not a reviewer", "admin", 1, string(g.SubjectDigest), http.StatusForbidden, "forbidden"},
		{"an unbound caller learns nothing", "outsider", 1, string(g.SubjectDigest), http.StatusNotFound, "not_found"},
		{"another tenant's administrator learns nothing", "foreign", 1, string(g.SubjectDigest), http.StatusNotFound, "not_found"},
		{"a stale version", "reviewer", 7, string(g.SubjectDigest), http.StatusPreconditionFailed, "version_conflict"},
		{"another subject", "reviewer", 1, "sha256:" + strings.Repeat("0", 64), http.StatusConflict, "stale_gate_subject"},
	}
	for i, r := range refused {
		code, body := e.DecideAt(r.persona, g, r.version, r.digest, "approve", "no", "refused-"+string(rune('a'+i)))
		if code != r.code || kit.ErrorCode(body) != r.err {
			t.Fatalf("%s: HTTP %d %s, want %d %s", r.what, code, body, r.code, r.err)
		}
	}
	if code, body := e.Do("reviewer", "POST", e.ProjectPath()+"/gates/"+g.Id+"/decisions",
		map[string]any{"decision": "approve", "subjectDigest": string(g.SubjectDigest), "reason": "x"}, map[string]string{"Idempotency-Key": "no-if-match"}); code != http.StatusPreconditionRequired || kit.ErrorCode(body) != "version_required" {
		t.Fatalf("a decision without If-Match: %d %s", code, body)
	}
	if now := e.GetGate("reviewer", g.Id); now.Status != "pending" || now.Version != g.Version {
		t.Fatalf("a refused decision changed the gate: %+v", now)
	}

	// ---- one current reviewer decides; the response is replayable ----
	code, body := e.Decide("reviewer", g, "approve", "tests and scope reviewed", "arb-decide")
	if code != http.StatusOK {
		t.Fatalf("approve: %d %s", code, body)
	}
	var approved client.Gate
	if err := json.Unmarshal(body, &approved); err != nil {
		t.Fatal(err)
	}
	if approved.Status != "approved" || approved.Decision == nil || approved.Decision.Reason != "tests and scope reviewed" || approved.Decision.DecidedBy.Type != "user" || !approved.Applicability.Applicable {
		t.Fatalf("approved gate %+v", approved)
	}
	if code, again := e.Decide("reviewer", g, "approve", "tests and scope reviewed", "arb-decide"); code != http.StatusOK || string(again) != string(body) {
		t.Fatalf("the keyed replay answered %d %s", code, again)
	}
	if code, b := e.Decide("reviewer", g, "reject", "changed my mind", "arb-decide"); code != http.StatusConflict || kit.ErrorCode(b) != "idempotency_conflict" {
		t.Fatalf("the same key with another body: %d %s", code, b)
	}
	if code, b := e.Decide("reviewer", approved, "reject", "changed my mind", "arb-decide-2"); code != http.StatusUnprocessableEntity || kit.ErrorCode(b) != "invalid_transition" {
		t.Fatalf("a second decision on an approved gate: %d %s", code, b)
	}

	// ---- the approved decision is consumed once; the build runs and the run succeeds ----
	final := e.World.WaitRun(run.Id, 3*time.Minute)
	if final.Status != client.RunStatusSucceeded {
		t.Fatalf("run %s (%s)", final.Status, final.Reason)
	}
	review := e.StepOf(run.Id, "review")
	build := e.StepOf(run.Id, "build")
	if review.Status != client.StepStatusSucceeded || review.Reason != "gate_approved" || len(review.Attempts) != 0 ||
		build.Status != client.StepStatusSucceeded || len(build.Attempts) != 1 || kit.Output(build, "built") != "yes" {
		t.Fatalf("review %+v build %+v", review, build)
	}
	if n := e.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, run.Id); n != 3 {
		t.Fatalf("%d attempts for four steps, one of them a gate", n)
	}
	var decided, status string
	e.MustScan(&decided, `SELECT decided_by_id FROM gates WHERE id = $1`, g.Id)
	e.MustScan(&status, `SELECT status FROM gates WHERE id = $1`, g.Id)
	if decided != e.UserID("reviewer") || status != "approved" {
		t.Fatalf("stored decision by %s status %s", decided, status)
	}

	// The gate's events are committed with its transitions (outbox), once each and
	// in order, and the run's success reaches the stream after them.
	if got := strings.Join(e.GateEvents(g.Id), ","); got != "gate.requested,gate.decided" {
		t.Fatalf("gate events %q", got)
	}
	events.Until(t, "the run's success on the stream", 30*time.Second, func(f kit.Frame) bool {
		return f.Type() == "run.state_changed" && strings.Contains(f.Data, run.Id) && strings.Contains(f.Data, `"succeeded"`)
	})

	// A passing run completes nothing: completion is a human transition (DQ-13).
	if now := e.GetItem(item.Id); now.State == "done" {
		t.Fatalf("a passing run completed the work item: %s", now.State)
	}
}
