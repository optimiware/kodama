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

// TestM2AgentPreview (m2_agent_preview) is the ERP gap-to-preview example
// (docs/examples/workflows/erp-gap-to-preview.md) end to end on the real stack:
// the assembled core, PostgreSQL, JetStream, S3, a real Git host and registry,
// the Podman runtime and a kind namespace as the preview's deployment target,
// with a deterministic agent, provider and tool server.
//
// An agent step of a pinned profile calls the model gateway and the MCP proxy
// with its own runtime token, writes the candidate into its checked-out
// workspace and binds design conformance evidence to it; the entrypoint
// publishes the commit through the workspace protocol. The test step reads
// exactly that commit and produces the report; the review gate snapshots the
// canonical subject (this commit, this content revision, the report and the
// conformance evidence) and waits for one human without compute; the approved
// decision admits the build, which registers an image verified against the
// registry; the preview deploys that digest into the namespace through the
// delivery records, and the objects the cluster holds are the recorded ones.
// Covers AR-A01, AR-A07, GM-A01 (the agent's own calls), DQ-A01, DQ-A03,
// DQ-A04, DQ-A05's record half, AR-11, AR-12, AR-14, DQ-01, DQ-02, DQ-07, DQ-08,
// DQ-10, DQ-14 and PX-13. The single-pipeline form of the example is not
// accepted by the pipeline validator (see TestM2ExampleEnvArtifactReference).
func TestM2AgentPreview(t *testing.T) {
	e := kit.New(t, kit.Options{Delivery: true, NoProfile: true})
	p := e.NewERP(t, kit.ERPSetup{Label: "preview"})
	item := e.Item("Inventory gap", "# Inventory gap\nResolve inventory gaps with audited, idempotent ERP writes.")
	events := e.OpenEvents("engineer", e.World.ProjectEvents(), "")
	events.Until(t, "the snapshot", 10*time.Second, func(f kit.Frame) bool { return f.Event == "snapshot" })

	run := p.Start("preview-candidate", item)
	g := e.WaitGate(run.Id, 3*time.Minute)

	// ---- the agent: pinned profile, own authority, bounded calls, a published commit ----
	impl := e.StepOf(run.Id, "implement")
	if impl.Status != client.StepStatusSucceeded || len(impl.Attempts) != 1 || impl.Attempts[0].Access != "write" || impl.SourceCommit == nil {
		t.Fatalf("implement %+v", impl)
	}
	candidate := *impl.SourceCommit
	if rows := e.Dump(`SELECT state FROM model_requests WHERE run_id = $1`, run.Id); len(rows) != 1 || rows[0] != "succeeded" {
		t.Fatalf("model requests %v", rows)
	}
	if rows := e.Dump(`SELECT basis || ':' || charged_tokens FROM model_usage WHERE run_id = $1`, run.Id); len(rows) != 1 || !strings.HasPrefix(rows[0], "observed:") {
		t.Fatalf("model usage %v", rows)
	}
	if rows := e.Dump(`SELECT state FROM mcp_invocations WHERE run_id = $1`, run.Id); len(rows) != 1 || rows[0] != "succeeded" {
		t.Fatalf("tool invocations %v", rows)
	}
	// The commit is on the real remote, on the work item's branch, on top of the pinned one, with the agent's files.
	branch := "refs/heads/kodama/work/" + item.Id
	if e.Repo.Tip(t, branch) != candidate {
		t.Fatalf("the work branch is at %s, not the agent's published %s", e.Repo.Tip(t, branch), candidate)
	}
	if parent := e.Repo.Git(t, "rev-parse", candidate+"^"); parent != e.Repo.Initial {
		t.Fatalf("the candidate's parent is %s, want the pinned %s", parent, e.Repo.Initial)
	}
	if files := e.Repo.Git(t, "ls-tree", "-r", "--name-only", candidate); !strings.Contains(files, "ci/build.sh") || !strings.Contains(files, "deploy/deployment.yaml") {
		t.Fatalf("the candidate's files: %s", files)
	}

	// ---- the gate: the canonical subject, evidence bound to this commit, no compute ----
	if g.Status != "pending" || g.Subject.CandidateCommit != candidate || g.Subject.ContentRevision != 1 || g.Subject.DefinitionId != p.Def.Id || g.Subject.WorkItemId != item.Id {
		t.Fatalf("gate %+v", g)
	}
	if g.Required.Conformance == nil || g.Required.Conformance.ProducerStep != "implement" || g.Required.Conformance.ReportId == nil || g.Required.Conformance.Code != nil {
		t.Fatalf("the agent-produced candidate's conformance evidence: %+v", g.Required.Conformance)
	}
	evidence := map[string]bool{}
	for _, ev := range g.Subject.Evidence {
		evidence[ev.Id] = true
	}
	testStep := e.StepOf(run.Id, "test")
	report := kit.Output(testStep, "report")
	if !evidence[report] || !evidence[*g.Required.Conformance.ReportId] || len(g.Subject.Evidence) < 3 {
		t.Fatalf("the subject binds %v, want the test report %s and the conformance report %s and its cited evidence", g.Subject.Evidence, report, *g.Required.Conformance.ReportId)
	}
	for _, id := range []string{report, *g.Required.Conformance.ReportId} {
		var art client.Artifact
		code, body := e.Do("reviewer", "GET", e.ProjectPath()+"/artifacts/"+id, nil, nil)
		if code != http.StatusOK || json.Unmarshal(body, &art) != nil || art.State != "usable" || art.Source == nil || art.Source.Commit == "" || art.Source.RepositoryUrl != e.Repo.URL {
			t.Fatalf("evidence %s: %d %s", id, code, body)
		}
	}
	if art := artifactOf(t, e, report); art.Source.Commit != candidate {
		t.Fatalf("the test report was produced from %s, not the candidate %s", art.Source.Commit, candidate)
	}
	if n := e.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1 AND step_id IN ('review', 'build')`, run.Id); n != 0 {
		t.Fatalf("%d attempts exist for the waiting gate and the build it protects", n)
	}
	if st := e.StepOf(run.Id, "build"); st.Status != client.StepStatusPending {
		t.Fatalf("build %s before the decision", st.Status)
	}

	// ---- a human approves; the approved decision admits the build once ----
	if code, b := e.Decide("reviewer", g, "approve", "report and conformance evidence reviewed", "preview-d"); code != http.StatusOK {
		t.Fatalf("approve: %d %s", code, b)
	}
	final := e.World.WaitRun(run.Id, 5*time.Minute)
	if final.Status != client.RunStatusSucceeded {
		t.Fatalf("candidate run %s (%s)", final.Status, final.Reason)
	}
	build := e.StepOf(run.Id, "build")
	imageID := kit.Output(build, "image")
	if build.Status != client.StepStatusSucceeded || len(build.Attempts) != 1 || imageID == "" {
		t.Fatalf("build %+v", build)
	}
	image := artifactOf(t, e, imageID)
	if image.Kind != "oci" || image.State != "registered" || image.Registry == nil || *image.Registry != kit.RegistryHost()+"/"+kit.RegistryRepositoryPrefix()+"/inventory" ||
		image.Source == nil || image.Source.Commit != candidate || image.Source.RepositoryUrl != e.Repo.URL || string(image.Digest) != string(e.Tools.Digest) {
		t.Fatalf("the registered image %+v (candidate %s, tools digest %s)", image, candidate, e.Tools.Digest)
	}

	// ---- the preview: the verified digest, deployed to the namespace, recorded first ----
	pv := p.Preview("preview-deploy", item, final)
	pf := e.World.WaitRun(pv.Id, 6*time.Minute)
	if pf.Status != client.RunStatusSucceeded || pf.SourceCommit == nil || *pf.SourceCommit != candidate {
		t.Fatalf("preview run %s (%s) at %v", pf.Status, pf.Reason, pf.SourceCommit)
	}
	step := e.StepOf(pv.Id, "preview")
	deps := e.Deployments()
	if len(deps) != 1 {
		t.Fatalf("%d deployments", len(deps))
	}
	d := deps[0]
	if d.Status != "succeeded" || d.OutcomeUnknown || d.RunId != pv.Id || d.EnvironmentId != p.EnvID || d.Namespace != p.NS.Name || d.Source.Commit != candidate || d.Source.RepositoryUrl != e.Repo.URL ||
		d.Image.ArtifactId != imageID || string(d.Image.Digest) != string(image.Digest) || d.DeletedAt != nil || d.ExpiresAt == nil || !d.ExpiresAt.After(d.CreatedAt) {
		t.Fatalf("deployment %+v", d)
	}
	if d.Producer.AttemptId != build.Attempts[0].Id || len(d.Objects) != 2 {
		t.Fatalf("producer %+v objects %+v", d.Producer, d.Objects)
	}
	for out, want := range map[string]string{"deployment_id": d.Id, "configuration_digest": string(d.ConfigurationDigest), "image_digest": string(image.Digest), "source_commit": candidate, "deployment_status": "succeeded"} {
		if got := kit.Output(step, out); got != want {
			t.Fatalf("preview output %s = %q, want %q", out, got, want)
		}
	}
	// The cluster holds exactly the recorded objects, annotated with the deployment, running the verified digest,
	// created after the record existed (DQ-08: intent before the external change).
	dep := p.NS.Object(t, "deployment", "inventory")
	cm := p.NS.Object(t, "configmap", "inventory-config")
	if dep == nil || cm == nil {
		t.Fatalf("the namespace lacks the deployed objects: deployment %v configmap %v", dep != nil, cm != nil)
	}
	for _, obj := range []map[string]any{dep, cm} {
		found := false
		for k, v := range kit.Annotations(obj) {
			found = found || (strings.Contains(k, "deployment") && v == d.Id)
		}
		if !found {
			t.Fatalf("an object of the namespace does not name deployment %s: %v", d.Id, kit.Annotations(obj))
		}
	}
	live := p.NS.Admin(t, "get", "deployment", "inventory", "-o", "jsonpath={.spec.template.spec.containers[0].image}")
	if !strings.HasSuffix(live, "@"+string(image.Digest)) {
		t.Fatalf("the live workload runs %s, not the verified digest %s", live, image.Digest)
	}
	created := p.NS.Admin(t, "get", "deployment", "inventory", "-o", "jsonpath={.metadata.creationTimestamp}")
	if ct, err := time.Parse(time.RFC3339, created); err != nil || ct.Before(d.CreatedAt.Truncate(time.Second)) {
		t.Fatalf("the deployment object (%s) predates its record (%s)", created, d.CreatedAt)
	}

	// ---- the history is consistent, tenant-scoped and attributable ----
	if got := strings.Join(e.GateEvents(g.Id), ","); got != "gate.requested,gate.decided" {
		t.Fatalf("gate events %q", got)
	}
	if code, _ := e.Do("analyst", "GET", "/api/v1/tenants/"+kit.TenantB+"/projects/"+e.World.ProjectB+"/deployments/"+d.Id, nil, nil); code != http.StatusNotFound {
		t.Fatalf("another tenant read the deployment: %d", code)
	}
	var shared int
	e.MustScan(&shared, `SELECT count(*) FROM deployments d JOIN gates g ON g.candidate_commit = d.source_commit JOIN step_attempts a ON a.id = d.attempt_id
		WHERE d.id = $1 AND g.id = $2 AND a.run_id = $3`, d.Id, g.Id, pv.Id)
	if shared != 1 {
		t.Fatalf("the deployment, the gate and the preview attempt do not share the candidate: %d", shared)
	}
	if now := e.GetItem(item.Id); now.State == "done" {
		t.Fatalf("a successful preview completed the work item: %s", now.State)
	}
}

func artifactOf(t *testing.T, e *kit.Env, id string) client.Artifact {
	t.Helper()
	code, body := e.Do("reviewer", "GET", e.ProjectPath()+"/artifacts/"+id, nil, nil)
	var art client.Artifact
	if code != http.StatusOK || json.Unmarshal(body, &art) != nil {
		t.Fatalf("artifact %s: %d %s", id, code, body)
	}
	return art
}
