package m3_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	m2 "github.com/optimiware/kodama/test/e2e/m2/kit"
	kit "github.com/optimiware/kodama/test/e2e/m3/kit"
)

// TestM3ExpiryCleanup (part of m3_business_loop: DQ-A06, with DQ-09 and DQ-10) is
// the retention half of the journey. The deployment the exploratory QA inspected
// is a PREVIEW that expires: the QA finding is mapped to a defect and the human
// rejects the deployment-bound gate while it is still up; then the preview
// expires and two replicas sweep it. Exactly one cleanup run deletes exactly the
// objects the deployment record names (a bystander object of the same namespace
// survives), further sweeps and an explicit request admit nothing new, and what
// the review inspected stays: the deployment record (marked deleted, its objects,
// image, source and configuration digest), the QA attempt and its accepted
// outputs, the report and trace artifacts (usable, not removed, still pinned by
// the defect), the defect and its evidence links, and the rejected gate with its
// subject and decision.
func TestM3ExpiryCleanup(t *testing.T) {
	e := kit.New(t, kit.Options{Delivery: true})
	s := e.NewStageWith(t, 75*time.Second)
	e.StartReplica() // a second process sweeps the same installation
	item := e.Admit(e.NewItem("Inventory gap resolution", "# Inventory gap\nTotals follow the selected warehouse."))
	cand := s.Candidate("exp-cand", item)
	_, dep := s.Stage("exp-stage", item, cand)
	if dep.ExpiresAt == nil || !dep.ExpiresAt.After(dep.CreatedAt) || dep.DeletedAt != nil {
		t.Fatalf("the preview deployment %+v has no future expiry", dep)
	}
	// An object of the namespace the deployment never owned.
	s.NS.Admin(t, "create", "configmap", "bystander", "--from-literal=owner=somebody-else")

	// ---- the review inspects the deployment while it is up ----
	run := s.Accept("exp-accept", item, cand, dep, "defect")
	g := e.WaitGate(run.Id, 4*time.Minute)
	qa := e.StepOf(run.Id, "qa")
	res, err := e.RecordQA(qa.Attempts[0].Id)
	if err != nil || len(res.Defects) != 1 || !res.Defects[0].Created {
		t.Fatalf("record the QA report (before the expiry %s): %+v %v", dep.ExpiresAt, res, err)
	}
	defectID := string(res.Defects[0].ID)
	if code, b := e.Decide("reviewer", g, "reject", "defect "+defectID+": totals ignore the warehouse filter", "exp-reject"); code != http.StatusOK {
		t.Fatalf("reject: %d %s", code, b)
	}
	if f := e.World.WaitRun(run.Id, time.Minute); f.Status != client.RunStatusFailed || f.Reason != "gate_rejected" {
		t.Fatalf("run %s (%s)", f.Status, f.Reason)
	}
	if now := e.Deployments(); len(now) != 1 || now[0].DeletedAt != nil {
		t.Fatalf("the preview expired before the review ended: %+v", now)
	}
	rejected := e.GetGate("reviewer", g.Id)
	defect := e.GetItem(defectID)
	histBefore := len(e.History(defectID))
	report, trace := m2.Output(qa, "report"), m2.Output(qa, "trace")
	e2eReport := m2.Output(e.StepOf(run.Id, "e2e"), "report")
	evidence := []string{report, trace, e2eReport}
	for _, id := range evidence {
		if a := artifactByID(t, e, id); a.State != "usable" || a.Availability == "removed" {
			t.Fatalf("evidence %s %+v before the cleanup", id, a)
		}
	}
	artifactsOf := func() int {
		return e.OwnerCount(`SELECT count(*) FROM artifacts WHERE run_id = $1 AND state = 'usable'`, run.Id)
	}
	usableBefore := artifactsOf()
	var pinsBefore int
	e.MustScan(&pinsBefore, `SELECT count(*) FROM artifact_pins WHERE owner_id = $1`, defectID)
	if pinsBefore != 2 || len(defect.EvidenceLinks) != 2 || usableBefore < 3 {
		t.Fatalf("before the cleanup: %d pins, %d links, %d usable artifacts", pinsBefore, len(defect.EvidenceLinks), usableBefore)
	}

	// ---- the preview expires; one cleanup run, only the owned objects ----
	m2.Eventually(t, 5*time.Minute, "the expired preview to be cleaned", func() (bool, string) {
		for _, x := range e.Deployments() {
			if x.Id == dep.Id {
				return x.DeletedAt != nil, fmt.Sprintf("cleanup %s/%v", x.Cleanup.State, x.Cleanup.Reason)
			}
		}
		return false, "the record vanished"
	})
	var n int
	e.MustScan(&n, `SELECT count(*) FROM runs WHERE purpose = 'cleanup' AND deployment_id = $1`, dep.Id)
	if n != 1 {
		t.Fatalf("%d cleanup runs for one deployment swept by two replicas", n)
	}
	var cleanup string
	e.MustScan(&cleanup, `SELECT id FROM runs WHERE purpose = 'cleanup' AND deployment_id = $1`, dep.Id)
	if cf := e.World.WaitRun(cleanup, 3*time.Minute); cf.Status != client.RunStatusSucceeded {
		t.Fatalf("cleanup run %s (%s)", cf.Status, cf.Reason)
	}
	cs := e.StepOf(cleanup, "cleanup")
	if m2.Output(cs, "deployment_id") != dep.Id || m2.Output(cs, "cleanup_state") != "done" || len(cs.Attempts) != 1 {
		t.Fatalf("cleanup step %+v", cs)
	}
	if s.NS.Object(t, "deployment", "inventory") != nil || s.NS.Object(t, "configmap", "inventory-config") != nil {
		t.Fatal("a recorded object survived the cleanup")
	}
	if s.NS.Object(t, "configmap", "bystander") == nil {
		t.Fatal("the cleanup deleted an object the deployment never owned")
	}

	// ---- repeatable: more sweeps and an explicit request admit nothing new ----
	time.Sleep(6 * time.Second)
	after := e.Deployments()
	if len(after) != 1 {
		t.Fatalf("%d deployments after the cleanup", len(after))
	}
	e.Do("engineer", "POST", e.ProjectPath()+"/deployments/"+dep.Id+"/cleanup", nil, map[string]string{"If-Match": fmt.Sprintf(`"%d"`, after[0].Version)})
	time.Sleep(3 * time.Second)
	e.MustScan(&n, `SELECT count(*) FROM runs WHERE purpose = 'cleanup' AND deployment_id = $1`, dep.Id)
	if n != 1 {
		t.Fatalf("%d cleanup runs after further sweeps and an explicit request", n)
	}

	// ---- what the review inspected is retained ----
	d := after[0]
	if d.Id != dep.Id || d.Status != "succeeded" || d.DeletedAt == nil || len(d.Objects) != 2 || d.Image.ArtifactId != dep.Image.ArtifactId || d.Image.Digest != dep.Image.Digest ||
		d.Source.Commit != cand.Commit || d.ConfigurationDigest != dep.ConfigurationDigest || d.EnvironmentId != s.EnvID || d.Namespace != dep.Namespace {
		t.Fatalf("the retained deployment record %+v (was %+v)", d, dep)
	}
	if e.StepOf(run.Id, "qa").Attempts[0].Id != qa.Attempts[0].Id || m2.Output(e.StepOf(run.Id, "qa"), "report") != report || m2.Output(e.StepOf(run.Id, "qa"), "trace") != trace {
		t.Fatal("the QA attempt's accepted outputs changed")
	}
	for _, id := range evidence {
		if a := artifactByID(t, e, id); a.State != "usable" || a.Availability == "removed" || a.Source == nil || a.Source.Commit != cand.Commit {
			t.Fatalf("evidence %s %+v after the cleanup", id, a)
		}
	}
	var pinsAfter int
	e.MustScan(&pinsAfter, `SELECT count(*) FROM artifact_pins WHERE owner_id = $1`, defectID)
	if pinsAfter != pinsBefore || artifactsOf() != usableBefore {
		t.Fatalf("pins %d -> %d, usable artifacts %d -> %d", pinsBefore, pinsAfter, usableBefore, artifactsOf())
	}
	got := e.GetItem(defectID)
	links := map[string]bool{}
	for _, l := range got.EvidenceLinks {
		links[l.Id] = true
	}
	if got.Version != defect.Version || got.State != "backlog" || got.Spec != defect.Spec || !links[report] || !links[trace] || len(e.History(defectID)) != histBefore {
		t.Fatalf("the defect changed through the cleanup: %+v", got)
	}
	for _, want := range []string{dep.Id, string(dep.ConfigurationDigest), cand.Commit} {
		if !strings.Contains(got.Spec, want) {
			t.Fatalf("the defect no longer names %s", want)
		}
	}
	gnow := e.GetGate("reviewer", g.Id)
	if gnow.Status != "rejected" || gnow.Decision == nil || gnow.Decision.Reason != rejected.Decision.Reason || gnow.SubjectDigest != rejected.SubjectDigest ||
		gnow.Subject.DeploymentId == nil || *gnow.Subject.DeploymentId != dep.Id || len(gnow.Subject.Evidence) != len(rejected.Subject.Evidence) {
		t.Fatalf("the rejected review changed through the cleanup: %+v", gnow)
	}
	if f := e.World.GetRun(run.Id); f.Status != client.RunStatusFailed || f.Reason != "gate_rejected" {
		t.Fatalf("the reviewed run changed: %s (%s)", f.Status, f.Reason)
	}
}
