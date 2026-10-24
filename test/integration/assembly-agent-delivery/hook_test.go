package assemblyagentdelivery_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	kit "github.com/optimiware/kodama/test/e2e/m2/kit"
)

// TestNativeHookRoundtrip (native_hook_roundtrip) joins the authenticated
// native Git push trigger (T-036, O-03) to real Git and the assembled core's
// durable admission, and runs what it admits on Podman. The core mounts the
// endpoint with its PostgreSQL trigger store and admission ledger (T-100,
// T-101). A real `git push` to the project's Git host runs the shipped
// post-receive hook, which signs and posts the notification; the endpoint
// authenticates it, claims the delivery durably, proves the announced commit on
// the remote with the read-only credential, rechecks the installing principal's
// CURRENT authority and admits one exact-commit run. Duplicate and replayed
// deliveries converge on that run and a REAL restart of the core loses nothing
// (the claim, the recorded refusal and the run are PostgreSQL rows); a forged,
// stale or unknown-trigger request, another repository's name, a commit that is
// not on the branch, a foreign branch and an installer who lost run:manage
// start nothing; the branch moving later never changes the commit of a run.
// Covers AC-07, AC-A05, PX-02, IT-A05 and DQ-01's commit provenance for the
// trigger (the module's own matrix stays T-036's, the adapters' T-100's).
func TestNativeHookRoundtrip(t *testing.T) {
	e := kit.New(t, kit.Options{NoProfile: true})
	h := newHookWorld(t, e)
	if string(h.trigger.RepositoryID) != h.repoID {
		t.Fatalf("the trigger is bound to repository %q, the project's is %q", h.trigger.RepositoryID, h.repoID)
	}
	if len(h.runsOf()) != 0 {
		t.Fatal("runs exist before any push")
	}
	for _, d := range e.Server().Core().Routes.Disabled {
		if d == "git-webhooks" {
			t.Fatalf("the core reports the trigger disabled: %v", e.Server().Core().Routes.Disabled)
		}
	}

	// ---- a real push starts exactly one run at exactly the pushed commit ----
	first := h.push(h.branch, "first change")
	runs := h.runsOf()
	if len(runs) != 1 || runs[0].SourceCommit == nil || *runs[0].SourceCommit != first || runs[0].Purpose != "work" {
		t.Fatalf("after the push of %s the work item has runs %+v", first, runs)
	}
	run1 := runs[0]
	if run1.RequestedBy.Id != e.UserID("admin") || run1.RequestedBy.Type != "user" {
		t.Fatalf("the run is attributed to %+v, not the trigger's installing principal", run1.RequestedBy)
	}
	rec := h.rec.all()
	if len(rec) != 1 || rec[0].status != http.StatusAccepted || !strings.Contains(string(rec[0].reply), `"started"`) || !strings.Contains(string(rec[0].reply), run1.Id) {
		t.Fatalf("the endpoint recorded %+v", rec)
	}
	// The run executes in a real container and succeeds at that commit.
	if f := e.World.WaitRun(run1.Id, 3*time.Minute); f.Status != client.RunStatusSucceeded || f.SourceCommit == nil || *f.SourceCommit != first {
		t.Fatalf("run %s (%s) at %v", f.Status, f.Reason, f.SourceCommit)
	}
	if st := e.StepOf(run1.Id, "work"); st.Status != client.StepStatusSucceeded || kit.Output(st, "ran") != "yes" {
		t.Fatalf("step %+v", st)
	}

	// ---- the same delivery, replayed byte for byte, is the same run ----
	for i := 0; i < 3; i++ {
		code, reply := h.post(h.coreURL(), rec[0].header, rec[0].body)
		if code != http.StatusAccepted || reply["runId"] != run1.Id || reply["duplicate"] != true {
			t.Fatalf("replay %d: %d %v", i, code, reply)
		}
	}
	if n := len(h.runsOf()); n != 1 {
		t.Fatalf("%d runs after replays", n)
	}
	// The same identifier with another body is a conflict, never a second run.
	hd, body := h.signed(rec[0].header.Get(gw.HeaderDeliveryID), gw.Payload{SchemaVersion: gw.SchemaVersion, Repository: h.repoID, Ref: "refs/heads/" + h.branch, Before: strings.Repeat("0", 40), After: e.Repo.Initial})
	if code, _ := h.post(h.coreURL(), hd, body); code != http.StatusConflict {
		t.Fatalf("a different body under a used delivery id: %d", code)
	}

	// ---- the branch moves; the old delivery still means the old commit ----
	second := h.push(h.branch, "second change")
	if second == first || e.Repo.Tip(t, "refs/heads/"+h.branch) != second {
		t.Fatal("the branch did not move")
	}
	if code, reply := h.post(h.coreURL(), rec[0].header, rec[0].body); code != http.StatusAccepted || reply["runId"] != run1.Id || reply["commit"] != first {
		t.Fatalf("replay after the branch moved: %d %v", code, reply)
	}
	runs = h.runsOf()
	if len(runs) != 2 {
		t.Fatalf("%d runs after the second push", len(runs))
	}
	commits := map[string]bool{}
	for _, r := range runs {
		commits[*r.SourceCommit] = true
	}
	if !commits[first] || !commits[second] {
		t.Fatalf("the runs' commits %v, want %s and %s", commits, first, second)
	}
	before := len(runs)

	// ---- authentication: a forged, stale or unknown notification starts nothing ----
	ref := "refs/heads/" + h.branch
	payload := gw.Payload{SchemaVersion: gw.SchemaVersion, Repository: h.repoID, Ref: ref, Before: strings.Repeat("0", 40), After: second}
	hd, body = h.signed("forged-1", payload)
	hd.Set(gw.HeaderSignature, "v1="+strings.Repeat("ab", 32))
	if code, _ := h.post(h.coreURL(), hd, body); code != http.StatusUnauthorized {
		t.Fatalf("a forged signature: %d", code)
	}
	hd, body = h.signed("forged-2", payload)
	hd.Set(gw.HeaderTimestamp, "1000000000")
	if code, _ := h.post(h.coreURL(), hd, body); code != http.StatusUnauthorized {
		t.Fatalf("a stale timestamp: %d", code)
	}
	hd, body = h.signed("forged-3", payload)
	if code, _ := h.post(e.Base()+"/api/v1/git-hooks/ght_unknown", hd, body); code != http.StatusUnauthorized {
		t.Fatalf("an unknown trigger: %d", code)
	}
	if len(h.runsOf()) != before {
		t.Fatal("an unauthenticated notification started a run")
	}
	if n := e.TenantCount(kit.TenantA, `SELECT count(*) FROM git_trigger_deliveries WHERE delivery_id LIKE 'forged-%'`); n != 0 {
		t.Fatalf("%d forged deliveries burned an identifier", n)
	}

	// ---- what the signature cannot vouch for: the remote's own history decides ----
	hd, body = h.signed("foreign-repo", gw.Payload{SchemaVersion: gw.SchemaVersion, Repository: "repo_other", Ref: ref, Before: strings.Repeat("0", 40), After: second})
	if code, reply := h.post(h.coreURL(), hd, body); code != http.StatusUnprocessableEntity || reply["error"] == nil {
		t.Fatalf("another repository's name: %d %v", code, reply)
	}
	orphan := e.Repo.OrphanCommit(t, "never on the branch")
	refusedHd, refusedBody := h.signed("not-on-branch", gw.Payload{SchemaVersion: gw.SchemaVersion, Repository: h.repoID, Ref: ref, Before: strings.Repeat("0", 40), After: orphan})
	if code, reply := h.post(h.coreURL(), refusedHd, refusedBody); code != http.StatusUnprocessableEntity || !strings.Contains(string(mustJSON(reply)), gw.ReasonCommitNotOnBranch) {
		t.Fatalf("a commit that is not on the branch: %d %v", code, reply)
	}
	if code, _ := h.post(h.coreURL(), refusedHd, refusedBody); code != http.StatusUnprocessableEntity { // the recorded refusal replays
		t.Fatalf("replay of the refusal: %d", code)
	}
	// A branch outside the trigger's explicit filter is ignored, not run.
	unrelated := h.pushNew("feature/unrelated", e.Repo.Initial, "unrelated branch")
	last := h.recordOf(unrelated)
	if last.status != http.StatusOK || !strings.Contains(string(last.reply), `"ignored"`) {
		t.Fatalf("a push to a foreign branch was %d %s", last.status, last.reply)
	}
	if len(h.runsOf()) != before {
		t.Fatalf("a refused or ignored delivery started a run: %d runs", len(h.runsOf()))
	}

	// ---- a real restart of the core: claims, refusals and runs are PostgreSQL rows ----
	e.Restart()
	if code, reply := h.post(h.coreURL(), rec[0].header, rec[0].body); code != http.StatusAccepted || reply["runId"] != run1.Id || reply["duplicate"] != true {
		t.Fatalf("the first delivery after a restart: %d %v", code, reply)
	}
	if code, reply := h.post(h.coreURL(), refusedHd, refusedBody); code != http.StatusUnprocessableEntity || !strings.Contains(string(mustJSON(reply)), gw.ReasonCommitNotOnBranch) {
		t.Fatalf("the recorded refusal after a restart: %d %v", code, reply)
	}
	if n := len(h.runsOf()); n != before {
		t.Fatalf("%d runs after the restart, want %d", n, before)
	}
	// A new push after the restart still starts its own run: the trigger survived.
	third := h.push(h.branch, "third change after the restart")
	if n := len(h.runsOf()); n != before+1 {
		t.Fatalf("the push of %s after the restart left %d runs", third, n)
	}
	before++

	// ---- authority is the installer's CURRENT authority ----
	e.RemoveMember("admin") // the administrator's engineer binding: run:manage goes away
	fourth := h.push(h.branch, "fourth change after the installer lost run:manage")
	last = h.recordOf(fourth)
	if last.status != http.StatusUnprocessableEntity || !strings.Contains(string(last.reply), gw.ReasonInstallerDenied) {
		t.Fatalf("a delivery of a revoked installer was %d %s", last.status, last.reply)
	}
	if len(h.runsOf()) != before {
		t.Fatalf("a revoked installer's push started a run at %s", fourth)
	}
}
