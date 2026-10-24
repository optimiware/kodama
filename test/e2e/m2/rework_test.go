package m2_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	kit "github.com/optimiware/kodama/test/e2e/m2/kit"
)

// TestM2Rework (m2_rework): a human rejection preserves the evidence, fails the
// run without compute for what the gate protects, and is immutable; rework is
// explicit: the engineer revises the item and a NEW run reaches a fresh gate
// with its own subject, which is the only one that can admit the build. The old
// decision is never reused, rewritten or revived. Covers DQ-A07's human half
// (AI QA defect filing is M3), DQ-11, DQ-03, DQ-05, AR-A07 and PX-13.
func TestM2Rework(t *testing.T) {
	e := kit.New(t, kit.Options{})
	def := e.ReviewPipeline("rework")
	item := e.Item("Idempotent ERP writes", "# Spec\nERP writes are idempotent.")

	// ---- the first run is rejected ----
	first := e.StartRun("rework-1", item, def.Id, e.AgentRunInputs("completed"))
	g1 := e.WaitGate(first.Id, 2*time.Minute)
	code, body := e.Decide("reviewer", g1, "reject", "the adjustment path is not idempotent", "rework-reject")
	if code != http.StatusOK {
		t.Fatalf("reject: %d %s", code, body)
	}
	f1 := e.World.WaitRun(first.Id, time.Minute)
	if f1.Status != client.RunStatusFailed || f1.Reason != "gate_rejected" {
		t.Fatalf("run %s (%s), want failed (gate_rejected)", f1.Status, f1.Reason)
	}
	review, build := e.StepOf(first.Id, "review"), e.StepOf(first.Id, "build")
	if review.Status != client.StepStatusFailed || review.Reason != "gate_rejected" || build.Status != client.StepStatusCancelled || len(build.Attempts) != 0 {
		t.Fatalf("review %s/%s build %s with %d attempts", review.Status, review.Reason, build.Status, len(build.Attempts))
	}
	if n := e.Attempts(first.Id); n != 2 {
		t.Fatalf("%d attempts: the rejected candidate must not reach the build", n)
	}
	// The evidence and decision are preserved exactly.
	kept := e.GetGate("reviewer", g1.Id)
	if kept.Status != "rejected" || kept.Decision == nil || kept.Decision.Reason != "the adjustment path is not idempotent" ||
		kept.SubjectDigest != g1.SubjectDigest || kept.Subject.CandidateCommit != g1.Subject.CandidateCommit || kept.Applicability.Applicable {
		t.Fatalf("the rejected gate %+v", kept)
	}
	// Immutable: neither an approval nor another rejection rewrites it.
	for i, d := range []string{"approve", "reject"} {
		if code, b := e.Decide("reviewer", kept, d, "reconsidered", "rework-again-"+d); code != http.StatusUnprocessableEntity || kit.ErrorCode(b) != "invalid_transition" {
			t.Fatalf("decision %d on a rejected gate: %d %s", i, code, b)
		}
	}
	var status, reason string
	e.MustScan(&status, `SELECT status FROM gates WHERE id = $1`, g1.Id)
	e.MustScan(&reason, `SELECT decision_reason FROM gates WHERE id = $1`, g1.Id)
	if status != "rejected" || reason != "the adjustment path is not idempotent" {
		t.Fatalf("stored gate %s %q", status, reason)
	}

	// ---- rework: a revised intent and a new run reach a fresh gate ----
	revised := e.EditSpec(item, "# Spec\nERP writes are idempotent: key every adjustment by (warehouse, document).")
	second := e.StartRun("rework-2", revised, def.Id, e.AgentRunInputs("completed"))
	g2 := e.WaitGate(second.Id, 2*time.Minute)
	if g2.Id == g1.Id || g2.RunId == g1.RunId || g2.SubjectDigest == g1.SubjectDigest || g2.Subject.ContentRevision != 2 || g1.Subject.ContentRevision != 1 {
		t.Fatalf("the rework gate is not a fresh review: %s/%s digests %s %s revisions %d %d", g1.Id, g2.Id, g1.SubjectDigest, g2.SubjectDigest, g1.Subject.ContentRevision, g2.Subject.ContentRevision)
	}
	if g2.Status != "pending" || g2.Decision != nil {
		t.Fatalf("the fresh gate %+v", g2)
	}
	// The old digest cannot approve the new subject (DQ-05: old approval is not reused).
	if code, b := e.DecideAt("reviewer", g2, int64(g2.Version), string(g1.SubjectDigest), "approve", "old subject", "rework-old-subject"); code != http.StatusConflict || kit.ErrorCode(b) != "stale_gate_subject" {
		t.Fatalf("the rejected subject's digest decided the new gate: %d %s", code, b)
	}
	if code, b := e.Decide("reviewer", g2, "approve", "idempotency key reviewed", "rework-approve"); code != http.StatusOK {
		t.Fatalf("approve the rework: %d %s", code, b)
	}
	f2 := e.World.WaitRun(second.Id, 3*time.Minute)
	if f2.Status != client.RunStatusSucceeded {
		t.Fatalf("the reworked run is %s (%s)", f2.Status, f2.Reason)
	}
	if b2 := e.StepOf(second.Id, "build"); b2.Status != client.StepStatusSucceeded || len(b2.Attempts) != 1 {
		t.Fatalf("build of the reworked run %+v", b2)
	}

	// History: both gates exist, the first still rejected and the first run failed.
	if now := e.GetGate("reviewer", g1.Id); now.Status != "rejected" || now.Decision == nil || now.Decision.Reason != kept.Decision.Reason {
		t.Fatalf("the first gate changed: %+v", now)
	}
	if old := e.World.GetRun(first.Id); old.Status != client.RunStatusFailed || *old.ContentRevision != 1 {
		t.Fatalf("the first run changed: %s rev %v", old.Status, old.ContentRevision)
	}
	if n := e.OwnerCount(`SELECT count(*) FROM gates WHERE work_item_id = $1`, item.Id); n != 2 {
		t.Fatalf("%d gates for two reviews", n)
	}
	if got := len(e.GateEvents(g1.Id)); got != 2 {
		t.Fatalf("the rejected gate recorded %d events, want requested and decided", got)
	}
}
