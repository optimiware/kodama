package m3_test

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/qa"
	m2 "github.com/optimiware/kodama/test/e2e/m2/kit"
	kit "github.com/optimiware/kodama/test/e2e/m3/kit"
)

var (
	stageRegisterPath = regexp.MustCompile(`^/runtime/v1/attempts/[^/]+/deployments$`)
	stageOutcomePath  = regexp.MustCompile(`^/runtime/v1/attempts/[^/]+/deployments/[^/]+/outcome$`)
)

// TestM3BusinessLoop (m3_business_loop) is the staging example
// (docs/examples/workflows/staging-and-acceptance.md) as one accepted user
// journey on the real stack: the assembled core, PostgreSQL, JetStream, S3, a
// real Git host and registry, the Podman runtime, a kind namespace as the
// stage, and the business-loop slice (ChatOps worker, QA mapping, dependency
// graph) selected by the installation statement.
//
// A candidate a human wrote is built; its verified image digest is deployed to
// the stage by the shipped apply script (the deployment record exists before
// the objects); the acceptance run executes the candidate's end-to-end check and
// a deterministic exploratory-QA attempt that holds the stage environment and
// returns the structured report; a deployment-bound gate snapshots the
// candidate, the content revision, the deployment and its configuration digest
// and both reports; the notification of the pending review reaches the receiver
// with an authenticated link; the reviewer (a persona distinct from every
// workload) follows the link, decides, and the passing run completes nothing.
// The stage's registration and outcome acknowledgments are lost after commit and the
// deployment still converges on one record and one set of objects (DQ-A05); the
// expiry cleanup of a deployment the QA inspected is TestM3ExpiryCleanup (DQ-A06).
// Covers DQ-A04, DQ-A05, DQ-A06's retained evidence, DQ-A09's negative half (a passing
// job and the QA principal cannot complete or approve), WM-A05's negative half,
// AC-A06 and AC-A07's positive path, DQ-06, DQ-07, DQ-10, WM-07.
// The human completion itself (review -> done) is unreachable in the current
// assembly: see TestM3ReviewGuardNotWired and docs/acceptance/m3.md.
func TestM3BusinessLoop(t *testing.T) {
	e := kit.New(t, kit.Options{Delivery: true})
	s := e.NewStage(t)
	dest := e.Destination("review-channel")
	item := e.NewItem("Inventory gap resolution", "# Inventory gap\nResolve inventory gaps with audited, idempotent ERP writes.")
	item = e.Admit(item)

	// ---- the candidate and its verified image ----
	cand := s.Candidate("m3-cand", item)
	image := artifactByID(t, e, cand.ImageID)
	if image.Kind != "oci" || image.State != "registered" || image.Source == nil || image.Source.Commit != cand.Commit || image.Source.RepositoryUrl != e.Repo.URL {
		t.Fatalf("the verified image %+v is not bound to candidate %s", image, cand.Commit)
	}

	// ---- stage: the verified digest, explicit configuration, recorded first ----
	// The first registration response and the first outcome response are lost AFTER the server
	// committed them (DQ-A05): the shipped script retries, and the stage converges on the one record
	// and the one set of objects.
	var registers, outcomes atomic.Int64
	e.Stack.Front.Faults(func(method, path string) m2.Fault {
		if method != http.MethodPost {
			return m2.FaultNone
		}
		switch {
		case stageRegisterPath.MatchString(path):
			if registers.Add(1) == 1 {
				return m2.FaultDropResponse
			}
		case stageOutcomePath.MatchString(path):
			if outcomes.Add(1) == 1 {
				return m2.FaultDropResponse
			}
		}
		return m2.FaultNone
	})
	sr, dep := s.Stage("m3-stage", item, cand)
	e.Stack.Front.Faults(nil)
	if registers.Load() < 2 || outcomes.Load() < 2 {
		t.Fatalf("the registration was posted %d times and the outcome %d: the lost acknowledgments were not retried", registers.Load(), outcomes.Load())
	}
	if all := e.Deployments(); len(all) != 1 || all[0].Id != dep.Id || len(all[0].Objects) != 2 {
		t.Fatalf("%d deployment records after lost acknowledgments, want the one %s", len(all), dep.Id)
	}
	if n := len(e.StepOf(sr.Id, "preview").Attempts); n != 1 {
		t.Fatalf("%d attempts: the retries were the script's own", n)
	}
	if gen := s.NS.Admin(t, "get", "deployment", "inventory", "-o", "jsonpath={.metadata.generation}"); gen != "1" {
		t.Fatalf("the stage deployment was applied again (generation %s)", gen)
	}
	if n := e.OwnerCount(`SELECT count(*) FROM run_history WHERE run_id = $1 AND subject_kind = 'step' AND step_id = 'preview' AND to_status = 'succeeded'`, sr.Id); n != 1 {
		t.Fatalf("the stage step succeeded %d times", n)
	}
	if dep.Status != "succeeded" || dep.OutcomeUnknown || dep.EnvironmentId != s.EnvID || dep.Namespace != s.NS.Name || dep.Source.Commit != cand.Commit ||
		dep.Image.ArtifactId != cand.ImageID || string(dep.Image.Digest) != string(image.Digest) || dep.DeletedAt != nil || dep.ExpiresAt != nil {
		t.Fatalf("stage deployment %+v (candidate %s, image %s)", dep, cand.Commit, image.Digest)
	}
	if len(dep.Objects) != 2 || dep.ConfigurationDigest == "" {
		t.Fatalf("the deployment records %d objects and configuration %q", len(dep.Objects), dep.ConfigurationDigest)
	}
	step := e.StepOf(sr.Id, "preview")
	for out, want := range map[string]string{"deployment_id": dep.Id, "configuration_digest": string(dep.ConfigurationDigest), "image_digest": string(image.Digest), "source_commit": cand.Commit} {
		if got := m2.Output(step, out); got != want {
			t.Fatalf("stage output %s = %q, want %q", out, got, want)
		}
	}
	live := s.NS.Admin(t, "get", "deployment", "inventory", "-o", "jsonpath={.spec.template.spec.containers[0].image}")
	if !strings.HasSuffix(live, "@"+string(image.Digest)) {
		t.Fatalf("the stage runs %s, not the verified digest %s", live, image.Digest)
	}
	created := s.NS.Admin(t, "get", "deployment", "inventory", "-o", "jsonpath={.metadata.creationTimestamp}")
	if ct, err := time.Parse(time.RFC3339, created); err != nil || ct.Before(dep.CreatedAt.Truncate(time.Second)) {
		t.Fatalf("the deployed object (%s) predates its record (%s)", created, dep.CreatedAt)
	}

	// ---- acceptance run: e2e, exploratory QA, deployment-bound review ----
	run := s.Accept("m3-accept", item, cand, dep, "clean")
	g := e.WaitGate(run.Id, 4*time.Minute)
	e2e, qaStep := e.StepOf(run.Id, "e2e"), e.StepOf(run.Id, "qa")
	if e2e.Status != client.StepStatusSucceeded || qaStep.Status != client.StepStatusSucceeded || len(qaStep.Attempts) != 1 {
		t.Fatalf("e2e %s, qa %s (%d attempts)", e2e.Status, qaStep.Status, len(qaStep.Attempts))
	}
	if g.Status != "pending" || g.Decision != nil || g.StepId != "business-review" {
		t.Fatalf("gate %+v", g)
	}
	if g.Subject.CandidateCommit != cand.Commit || g.Subject.ContentRevision != item.ContentRevision || g.Subject.WorkItemId != item.Id ||
		g.Subject.DeploymentId == nil || *g.Subject.DeploymentId != dep.Id || g.Subject.DeploymentConfigDigest == nil || *g.Subject.DeploymentConfigDigest != string(dep.ConfigurationDigest) {
		t.Fatalf("the gate subject %+v does not name candidate %s, deployment %s and configuration %s", g.Subject, cand.Commit, dep.Id, dep.ConfigurationDigest)
	}
	e2eReport, qaReport := m2.Output(e2e, "report"), m2.Output(qaStep, "report")
	bound := map[string]bool{}
	for _, ev := range g.Subject.Evidence {
		bound[ev.Id] = true
	}
	if !bound[e2eReport] || !bound[qaReport] || len(g.Subject.Evidence) != 2 {
		t.Fatalf("the subject binds %v, want the end-to-end report %s and the QA report %s", g.Subject.Evidence, e2eReport, qaReport)
	}
	for _, id := range []string{e2eReport, qaReport} {
		a := artifactByID(t, e, id)
		if a.State != "usable" || a.Source == nil || a.Source.Commit != cand.Commit {
			t.Fatalf("evidence %s %+v is not usable evidence of candidate %s", id, a, cand.Commit)
		}
	}
	if n := e.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1 AND step_id = 'business-review'`, run.Id); n != 0 {
		t.Fatalf("the waiting gate holds %d attempts", n)
	}

	// ---- QA is evidence, not authority (DQ-06): the mapping files nothing for a clean pass ----
	res, err := e.RecordQA(qaStep.Attempts[0].Id)
	if err != nil || len(res.Defects) != 0 || res.Deployment.ID != dep.Id || string(res.Deployment.ImageDigest) != string(image.Digest) || string(res.Deployment.CandidateCommit) != cand.Commit ||
		string(res.Deployment.ConfigDigest) != string(dep.ConfigurationDigest) {
		t.Fatalf("record the clean QA report: %+v %v", res, err)
	}
	for _, who := range []qa.Actor{{Type: qa.ActorUser, ID: e.UserID("reviewer")}, {Type: qa.ActorWorkload, ID: qaStep.Attempts[0].Id}, {Type: qa.ActorOperator, ID: "operator"}} {
		if _, err := e.RecordQAAs(qaStep.Attempts[0].Id, who); err == nil {
			t.Fatalf("a %s caller mapped the QA report", who.Type)
		}
	}
	// Neither the mapping principal nor any non-reviewer can decide the gate.
	for _, who := range []string{kit.MapperPersona, "engineer", "viewer", "admin"} {
		if code, b := e.Decide(who, g, "approve", "not mine to decide", "m3-nope-"+who); code != http.StatusForbidden && code != http.StatusNotFound {
			t.Fatalf("%s decided the gate: %d %s", who, code, b)
		}
	}
	if now := e.GetGate("reviewer", g.Id); now.Status != "pending" || now.Version != g.Version {
		t.Fatalf("a refused decision changed the gate: %+v", now)
	}

	// ---- the notification of the pending review: durable, with a link that carries no authority ----
	note := e.WaitReceived("gate.requested", g.Id, time.Minute)
	wantURL := e.Links.Origin + "/api/v1/tenants/" + m2.TenantA + "/projects/" + e.World.Project + "/gates/" + g.Id
	if note.URL != wantURL || note.TenantID != m2.TenantA || note.ProjectID != e.World.Project || note.Resource.Type != "gate" || note.SchemaVersion != 1 ||
		note.Attributes["subjectDigest"] != string(g.SubjectDigest) {
		t.Fatalf("notification %+v, want link %s", note, wantURL)
	}
	if strings.Contains(string(e.Receiver.Requests()[0].Body), "token") || e.Receiver.Requests()[0].Header.Get("Authorization") != "" {
		t.Fatal("a notification carries credential material")
	}
	if code, body := e.Follow("", note.URL); code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated follower: %d %s", code, body)
	}
	if code, _ := e.Follow("outsider", note.URL); code != http.StatusNotFound && code != http.StatusForbidden {
		t.Fatalf("a follower with no binding: %d", code)
	}
	code, body := e.Follow("reviewer", note.URL)
	var seen client.Gate
	if code != http.StatusOK || json.Unmarshal(body, &seen) != nil || seen.Id != g.Id || seen.Status != "pending" {
		t.Fatalf("the reviewer following the link: %d %s", code, body)
	}
	if code, _ := e.Follow("reviewer", note.URL); code != http.StatusOK || e.GetGate("reviewer", g.Id).Status != "pending" {
		t.Fatal("following the link changed the gate")
	}

	// ---- the reviewer decides; the passing run completes nothing ----
	if code, b := e.Decide("reviewer", seen, "approve", "stage inspected: the inventory gap resolves with an audit record", "m3-approve"); code != http.StatusOK {
		t.Fatalf("approve: %d %s", code, b)
	}
	final := e.World.WaitRun(run.Id, 2*time.Minute)
	if final.Status != client.RunStatusSucceeded {
		t.Fatalf("acceptance run %s (%s)", final.Status, final.Reason)
	}
	decided := e.GetGate("reviewer", g.Id)
	if decided.Status != "approved" || decided.Decision == nil || decided.Decision.DecidedBy.Id != e.UserID("reviewer") || decided.Decision.DecidedBy.Id == e.MapperID {
		t.Fatalf("decided gate %+v", decided)
	}
	if got := strings.Join(e.GateEvents(g.Id), ","); got != "gate.requested,gate.decided" {
		t.Fatalf("gate events %q", got)
	}
	d2 := e.WaitReceived("gate.decided", g.Id, time.Minute)
	if d2.URL != wantURL || d2.Resource.Version <= note.Resource.Version {
		t.Fatalf("decision notification %+v", d2)
	}
	if now := e.GetItem(item.Id); now.State != "active" || now.Completion != nil {
		t.Fatalf("a passing run, an approved gate and the QA pass moved the work item to %s (completion %+v): only an explicit human transition completes work", now.State, now.Completion)
	}
	// Only a human reviewer's explicit review -> done completes (WM-07); nobody completes from active.
	for _, who := range []string{"engineer", "reviewer", "admin"} {
		if code, b := e.Transition(who, e.GetItem(item.Id), "done", map[string]any{"completion": map[string]any{"contentRevision": item.ContentRevision, "acceptedCriteria": []string{"AC-1", "AC-2"}}}); code < 400 {
			t.Fatalf("%s completed an active item: %d %s", who, code, b)
		}
	}

	// ---- provenance: one candidate, one digest, one configuration across every record ----
	var joined int
	e.MustScan(&joined, `SELECT count(*) FROM gates g
		JOIN deployments d ON d.id = $2 AND d.source_commit = g.candidate_commit AND d.run_id = $3
		JOIN artifacts img ON img.id = d.image_artifact_id AND img.source_commit = g.candidate_commit
		WHERE g.id = $1`, g.Id, dep.Id, sr.Id)
	if joined != 1 {
		t.Fatalf("the gate, the stage deployment and the registered image do not share candidate %s", cand.Commit)
	}
	// Tenant B sees none of it.
	for _, q := range []string{`SELECT count(*) FROM gates`, `SELECT count(*) FROM deployments`, `SELECT count(*) FROM chatops_deliveries`} {
		if a := e.TenantCount(m2.TenantA, q); a == 0 {
			t.Fatalf("tenant A has no rows for %q", q)
		}
		if b := e.TenantCount(m2.TenantB, q); b != 0 {
			t.Fatalf("tenant B's role sees %d rows for %q", b, q)
		}
	}
	// Every notification of the loop reached the destination exactly once per event.
	ds := e.WaitSettled(time.Minute)
	for _, d := range ds {
		if d.State != "delivered" || d.DestinationId != dest.Id || d.Receipt == nil {
			t.Fatalf("delivery %+v", d)
		}
	}
	if len(ds) < 4 {
		t.Fatalf("%d deliveries for the transitions, the run, the deployment and the gate", len(ds))
	}
}

func artifactByID(t *testing.T, e *kit.Env, id string) client.Artifact {
	t.Helper()
	code, body := e.Do("reviewer", "GET", e.ProjectPath()+"/artifacts/"+id, nil, nil)
	var art client.Artifact
	if code != http.StatusOK || json.Unmarshal(body, &art) != nil {
		t.Fatalf("artifact %s: %d %s", id, code, body)
	}
	return art
}
