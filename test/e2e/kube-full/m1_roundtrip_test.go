package kube_full_test

import (
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/test/support/stage"
)

// TestKubeM1Roundtrip is kube_m1_roundtrip: a reader, a writer that publishes a commit, and a
// downstream reader of that commit, on the installed server. A foreign tenant is denied. A human
// push that moves the tip must not be reported as a successful publication.
func TestKubeM1Roundtrip(t *testing.T) {
	i := stage.New(t)
	w := i.Seed()
	ctx := i.Context(8 * time.Minute)
	before := w.Repo.Commit(ctx, "main")
	if before == "" {
		t.Fatal("the repository has no base commit")
	}

	item := w.WorkItem("Reader", "# read")
	w.Provision(item.Id)
	read := w.Definition("reader", stage.Step{
		ID: "read", Access: "read",
		Script:  `test -n "$(git rev-parse HEAD)" && printf '{"outcome":"completed","outputs":{"v":"read"}}' > "$KODAMA_RESULTS_FILE"`,
		Outputs: []string{"v"},
	})
	run := w.Run("reader-run", item, read.Id)
	final := w.WaitRun(run.Id, 5*time.Minute)
	if final.Status != client.RunStatusSucceeded {
		t.Fatalf("reader %s (%s)", final.Status, final.Reason)
	}
	if got := w.Repo.Commit(ctx, "main"); got != before {
		t.Fatalf("a reader moved the remote tip")
	}
	if final.InputSnapshot.Digest == "" {
		t.Fatal("the run has no immutable input snapshot")
	}
	digest := final.InputSnapshot.Digest
	// The reader prints nothing, so its retained log is complete and has no command lines.
	if lines := w.WaitLogComplete("engineer", run.Id); len(lines) != 0 {
		t.Fatalf("the quiet reader's retained log has %d command lines", len(lines))
	}
	// The reader prints nothing: its zero-byte command.log must still be uploaded (a chunked zero-length
	// presigned PUT was refused by the object store with HTTP 405).
	readerAttempts := w.Attempts(run.Id)
	if len(readerAttempts) != 1 {
		t.Fatalf("reader attempts=%d, want 1", len(readerAttempts))
	}
	diagnostics := w.DiagnosticNames(readerAttempts[0])
	hasLog := false
	for _, name := range diagnostics {
		hasLog = hasLog || name == "command.log"
	}
	if !hasLog {
		t.Fatalf("the quiet reader retained no command.log diagnostic (saw %v)", diagnostics)
	}

	writeItem := w.WorkItem("Writer", "# write")
	ws := w.Provision(writeItem.Id)
	writer := w.RegisterDefinition("engineer", "writer", []any{
		w.StepDocument(stage.Step{
			ID: "write", Access: "write",
			Script:  `echo published-by-kodama > published.txt; printf '{"outcome":"completed","outputs":{"v":"wrote"}}' > "$KODAMA_RESULTS_FILE"`,
			Outputs: []string{"v"},
		}),
		w.StepDocument(stage.Step{
			ID: "follow", Access: "read", From: "write", Needs: []string{"write"},
			Script:  `grep -q published-by-kodama published.txt && printf '{"outcome":"completed","outputs":{"v":"followed"}}' > "$KODAMA_RESULTS_FILE"`,
			Outputs: []string{"v"},
		}),
	})
	wrun := w.Run("writer-run", writeItem, writer.Id)
	wf := w.WaitRun(wrun.Id, 8*time.Minute)
	if wf.Status != client.RunStatusSucceeded {
		t.Fatalf("writer %s (%s)", wf.Status, wf.Reason)
	}
	// Publication lands on the work branch, not the repository's base ref.
	tip := refSHA(w.Repo.Refs(ctx), ws.Branch)
	if tip == "" || tip == before {
		t.Fatalf("the writer did not publish %s (refs %v)", ws.Branch, w.Repo.Refs(ctx))
	}
	if body, ok := w.Repo.File(ctx, tip, "published.txt"); !ok || !strings.Contains(body, "published-by-kodama") {
		t.Fatal("the published file is not on the remote work branch")
	}
	if got := w.Repo.Commit(ctx, "main"); got != before {
		t.Fatal("publication moved the base ref")
	}
	if got := w.GetRun(run.Id); got.InputSnapshot.Digest != digest {
		t.Fatal("the reader's immutable inputs changed after a later publication")
	}
	if st := w.Steps(wrun.Id); len(st) < 2 || st[1].Status != client.StepStatusSucceeded {
		t.Fatalf("downstream step did not succeed: %+v", st)
	}

	foreign, err := i.API("analyst").GetRunWithResponse(ctx, w.TenantA, w.Project, run.Id)
	if err != nil {
		t.Fatal(err)
	}
	if foreign.StatusCode() != 403 && foreign.StatusCode() != 404 {
		t.Fatalf("a foreign tenant read the run: HTTP %d", foreign.StatusCode())
	}

	// Publication conflict: a writer that has started, then a human push of its work branch,
	// must not report success.
	raceItem := w.WorkItem("Conflict", "# conflict")
	raceWS := w.Provision(raceItem.Id)
	raceDef := w.Definition("racer", stage.Step{
		ID: "race", Access: "write", Timeout: 600,
		Script:  stage.CommandBarrier + `echo raced > raced.txt; printf '{"outcome":"completed","outputs":{"v":"raced"}}' > "$KODAMA_RESULTS_FILE"`,
		Outputs: []string{"v"},
	})
	rr := w.Run("race-run", raceItem, raceDef.Id)
	att := w.WaitAttempt(rr.Id, 2*time.Minute)
	ns := w.Namespace(w.TenantA)
	job := i.WaitJob(ns, att, true, 2*time.Minute)
	pod := i.WaitPod(ns, job.Name, 2*time.Minute)
	i.WaitCommandBarrier(ns, pod.Name)
	moved := w.Repo.PutFileOn(ctx, raceWS.Branch, "human.txt", "human push\n", "human push")
	if moved == "" {
		t.Fatal("the human push did not move the work branch")
	}
	i.ReleaseCommand(ns, pod.Name)
	rf := w.WaitRun(rr.Id, 5*time.Minute)
	if rf.Status != client.RunStatusFailed {
		t.Fatalf("publication conflict: run %s (%s), want failed", rf.Status, rf.Reason)
	}
	// The attempt reason is the generic command_failed (execution keeps a specific reason only for
	// output failures); the conflict itself is the recorded publication outcome and an untouched branch.
	atts := w.Attempts(rr.Id)
	if len(atts) != 1 {
		t.Fatalf("attempts=%d, want 1", len(atts))
	}
	if n := i.OwnerCount(`SELECT count(*) FROM workspace_publications WHERE attempt_id=$1 AND status='conflict' AND conflict_reason='tip_moved' AND outcome='failed'`, atts[0].Id); n != 1 {
		t.Fatalf("recorded publication conflicts=%d, want 1", n)
	}
	if n := i.OwnerCount(`SELECT count(*) FROM workspace_publications WHERE attempt_id=$1 AND status='published'`, atts[0].Id); n != 0 {
		t.Fatal("a conflicted publication was also recorded as published")
	}
	if tip := w.Repo.Commit(ctx, strings.TrimPrefix(raceWS.Branch, "refs/heads/")); tip != moved {
		t.Fatal("the human's commit on the work branch was overwritten")
	}
}

func refSHA(refs []string, name string) string {
	for _, ref := range refs {
		got, sha, ok := strings.Cut(ref, " ")
		if ok && (got == name || got == "refs/heads/"+strings.TrimPrefix(name, "refs/heads/")) {
			return sha
		}
	}
	return ""
}
