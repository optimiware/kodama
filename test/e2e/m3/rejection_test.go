package m3_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	m2 "github.com/optimiware/kodama/test/e2e/m2/kit"
	kit "github.com/optimiware/kodama/test/e2e/m3/kit"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

// TestM3Rejection (m3_rejection) is the rejected business path of the staging
// example and DQ-A07: the exploratory-QA attempt inspects the staged deployment
// and reports a finding; the control plane maps it to an ordinary defect work
// item that links the QA evidence and names the deployment, configuration,
// candidate and image inspected; the human reviewer rejects the deployment-bound
// gate; the rejection preserves the evidence and the decision exactly and is
// immutable; rework is explicit (a new commit on the work branch, a new image
// build, a new stage deployment) and reaches a FRESH gate with its own subject,
// which the rejected subject's digest cannot decide. The first gate, the first
// run's history and the defect stay as they were.
// Covers DQ-A07, DQ-A04, DQ-06, DQ-10, DQ-11, DQ-05, DQ-03, WM-01, AC-A06 (the
// decision notification) and the human half of WM-A03/WM-A05. The WIP guard of
// the rework transition (review -> active) is unreachable in the assembly (see
// TestM3ReviewGuardNotWired).
func TestM3Rejection(t *testing.T) {
	e := kit.New(t, kit.Options{Delivery: true})
	s := e.NewStage(t)
	e.Destination("review-channel", "gate.requested", "gate.decided")
	item := e.Admit(e.NewItem("Inventory gap resolution", "# Inventory gap\nTotals follow the selected warehouse."))

	// ---- the first candidate has a defect the QA pass finds ----
	c1 := s.Candidate("rej-cand-1", item)
	_, d1 := s.Stage("rej-stage-1", item, c1)
	run1 := s.Accept("rej-accept-1", item, c1, d1, "defect")
	g1 := e.WaitGate(run1.Id, 4*time.Minute)
	qa1 := e.StepOf(run1.Id, "qa")

	res, err := e.RecordQA(qa1.Attempts[0].Id)
	if err != nil || len(res.Defects) != 1 || !res.Defects[0].Created {
		t.Fatalf("record the QA report: %+v %v", res, err)
	}
	defectID := string(res.Defects[0].ID)
	defect := e.GetItem(defectID)
	if defect.Kind != "defect" || defect.State != "backlog" || !strings.Contains(defect.Title, "warehouse filter") {
		t.Fatalf("defect %+v", defect)
	}
	// Its provenance and evidence: the deployment inspected, the report and the trace.
	for _, want := range []string{d1.Id, string(d1.ConfigurationDigest), c1.Commit, string(d1.Image.Digest), qa1.Attempts[0].Id, run1.Id} {
		if !strings.Contains(defect.Spec, want) {
			t.Fatalf("the defect does not name %s:\n%s", want, defect.Spec)
		}
	}
	report, trace := m2.Output(qa1, "report"), m2.Output(qa1, "trace")
	links := map[string]bool{}
	for _, l := range defect.EvidenceLinks {
		links[l.Id] = true
	}
	if !links[report] || !links[trace] || len(defect.EvidenceLinks) != 2 {
		t.Fatalf("the defect links %+v, want the QA report %s and the trace %s", defect.EvidenceLinks, report, trace)
	}
	hist := e.History(defectID)
	if len(hist) != 1 || hist[0].Actor.Id != e.MapperID || hist[0].Actor.Type != "user" {
		t.Fatalf("the defect's history %+v: it is filed as the mapping principal %s", hist, e.MapperID)
	}
	var pins int
	e.MustScan(&pins, `SELECT count(*) FROM artifact_pins WHERE owner_id = $1`, defectID)
	if pins != 2 {
		t.Fatalf("%d retention pins for the defect's two evidence artifacts", pins)
	}
	// Mapping again, however often, files nothing new (the key is the finding on the candidate).
	again, err := e.RecordQA(qa1.Attempts[0].Id)
	if err != nil || len(again.Defects) != 1 || again.Defects[0].Created || again.Defects[0].ID != res.Defects[0].ID || again.Defects[0].EvidenceAdded != 0 {
		t.Fatalf("record again: %+v %v", again, err)
	}
	if n := e.OwnerCount(`SELECT count(*) FROM work_items WHERE kind = 'defect' AND project_id = $1`, e.World.Project); n != 1 {
		t.Fatalf("%d defects", n)
	}
	// The QA pass never decided anything: the gate still waits for the human.
	if now := e.GetGate("reviewer", g1.Id); now.Status != "pending" {
		t.Fatalf("the gate is %s after the QA pass", now.Status)
	}

	// ---- the human rejects, naming the defect ----
	g1 = e.GetGate("reviewer", g1.Id)
	note := e.WaitReceived("gate.requested", g1.Id, time.Minute)
	if code, _ := e.Follow("reviewer", note.URL); code != http.StatusOK {
		t.Fatalf("follow: %d", code)
	}
	reason := "defect " + defectID + ": the adjustment total ignores the warehouse filter"
	if code, b := e.Decide("reviewer", g1, "reject", reason, "rej-reject"); code != http.StatusOK {
		t.Fatalf("reject: %d %s", code, b)
	}
	f1 := e.World.WaitRun(run1.Id, time.Minute)
	if f1.Status != client.RunStatusFailed || f1.Reason != "gate_rejected" {
		t.Fatalf("run %s (%s), want failed (gate_rejected)", f1.Status, f1.Reason)
	}
	kept := e.GetGate("reviewer", g1.Id)
	if kept.Status != "rejected" || kept.Decision == nil || kept.Decision.Reason != reason || kept.SubjectDigest != g1.SubjectDigest || kept.Applicability.Applicable ||
		kept.Subject.CandidateCommit != c1.Commit || kept.Subject.DeploymentId == nil || *kept.Subject.DeploymentId != d1.Id {
		t.Fatalf("the rejected gate %+v", kept)
	}
	for i, d := range []string{"approve", "reject"} {
		if code, b := e.Decide("reviewer", kept, d, "reconsidered", "rej-again-"+d); code != http.StatusUnprocessableEntity || kit2Code(b) != "invalid_transition" {
			t.Fatalf("decision %d on a rejected gate: %d %s", i, code, b)
		}
	}
	// The evidence of the rejected review is preserved: usable, bound to the candidate, and linked by the defect.
	for _, id := range []string{report, trace, m2.Output(e.StepOf(run1.Id, "e2e"), "report")} {
		if a := artifactByID(t, e, id); a.State != "usable" {
			t.Fatalf("evidence %s is %s after the rejection", id, a.State)
		}
	}
	// The decision notification reaches the receiver with the same record link.
	dn := e.WaitReceived("gate.decided", g1.Id, time.Minute)
	if dn.URL != note.URL || dn.Resource.Version != int64(kept.Version) {
		t.Fatalf("decision notification %+v vs gate version %d", dn, kept.Version)
	}

	// ---- rework: a new commit, a new image, a new stage deployment, a FRESH review ----
	c2 := s.Candidate("rej-cand-2", item, gitfx.File{Path: "docs/rework.md", Content: "Totals now follow the selected warehouse (defect " + defectID + ").\n"})
	if c2.Commit == c1.Commit {
		t.Fatal("the rework has the rejected candidate's commit")
	}
	if parent := e.Repo.Git(t, "rev-parse", c2.Commit+"^"); parent != c1.Commit {
		t.Fatalf("the rework builds on %s, not on the rejected candidate %s", parent, c1.Commit)
	}
	_, d2 := s.Stage("rej-stage-2", item, c2)
	if d2.Id == d1.Id || d2.Source.Commit != c2.Commit {
		t.Fatalf("rework deployment %+v vs first %s", d2, d1.Id)
	}
	run2 := s.Accept("rej-accept-2", item, c2, d2, "clean")
	g2 := e.WaitGate(run2.Id, 4*time.Minute)
	if g2.Id == g1.Id || g2.RunId == g1.RunId || g2.SubjectDigest == g1.SubjectDigest || g2.Subject.CandidateCommit != c2.Commit || *g2.Subject.DeploymentId != d2.Id {
		t.Fatalf("the rework gate is not a fresh review of the new candidate: %+v", g2.Subject)
	}
	if g2.Status != "pending" || g2.Decision != nil {
		t.Fatalf("fresh gate %+v", g2)
	}
	qa2 := e.StepOf(run2.Id, "qa")
	if r2, err := e.RecordQA(qa2.Attempts[0].Id); err != nil || len(r2.Defects) != 0 {
		t.Fatalf("record the rework's QA report: %+v %v", r2, err)
	}
	// The rejected subject's digest decides nothing of the new review (DQ-05).
	if code, b := e.DecideAt("reviewer", g2, int64(g2.Version), string(g1.SubjectDigest), "approve", "old subject", "rej-old-digest"); code != http.StatusConflict || kit2Code(b) != "stale_gate_subject" {
		t.Fatalf("the rejected subject's digest decided the new gate: %d %s", code, b)
	}
	if code, b := e.Decide("reviewer", g2, "approve", "the defect "+defectID+" is fixed on the new stage deployment", "rej-approve"); code != http.StatusOK {
		t.Fatalf("approve the rework: %d %s", code, b)
	}
	if f2 := e.World.WaitRun(run2.Id, 2*time.Minute); f2.Status != client.RunStatusSucceeded {
		t.Fatalf("the reworked acceptance run is %s (%s)", f2.Status, f2.Reason)
	}

	// ---- history: nothing of the first review changed; the defect is still the one ordinary work item ----
	if now := e.GetGate("reviewer", g1.Id); now.Status != "rejected" || now.Decision == nil || now.Decision.Reason != reason || now.SubjectDigest != g1.SubjectDigest {
		t.Fatalf("the first gate changed: %+v", now)
	}
	if old := e.World.GetRun(run1.Id); old.Status != client.RunStatusFailed || *old.ContentRevision != item.ContentRevision {
		t.Fatalf("the first run changed: %s", old.Status)
	}
	if got := e.GetItem(defectID); got.Version != defect.Version || got.State != "backlog" {
		t.Fatalf("the defect changed: %+v", got)
	}
	if n := e.OwnerCount(`SELECT count(*) FROM gates WHERE work_item_id = $1`, item.Id); n != 2 {
		t.Fatalf("%d gates for two reviews", n)
	}
	if now := e.GetItem(item.Id); now.State != "active" || now.Completion != nil {
		t.Fatalf("an approved rework moved the work item to %s", now.State)
	}
	// The decision of each review was announced once.
	for _, g := range []client.Gate{g1, g2} {
		e.WaitReceived("gate.decided", g.Id, time.Minute)
	}
}

func kit2Code(b []byte) string { return m2.ErrorCode(b) }
