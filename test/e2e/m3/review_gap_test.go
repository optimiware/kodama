package m3_test

import (
	"net/http"
	"testing"

	m2 "github.com/optimiware/kodama/test/e2e/m2/kit"
	kit "github.com/optimiware/kodama/test/e2e/m3/kit"
)

// TestM3ReviewGuardNotWired pins what the assembled core does today at the
// edge that human completion depends on, and is NOT an acceptance check.
//
// internal/app/core.go selects the dependency guard for the work store and
// leaves the review-subject guard (work.TxGuards.Subjects: candidate history and
// evidence read from Git and the object store) unset, because no
// transaction-safe adapter exists. A work item therefore cannot enter review:
// every active -> review transition, with a candidate commit or with artifact
// evidence, fails closed as 503 `unavailable` and writes nothing. Without a
// recorded review subject nothing can be completed (review -> done), reworked
// (review -> active) or reopened, so the human-completion half of WM-A05 and
// DQ-A09, the WIP guard of rework/reopen (WM-A03) and the review/reopen
// intervals of WM-A08 are not provable on the composition (see
// docs/acceptance/m3.md, finding 1).
//
// This test fails by design the day the guard is wired; at that point the
// journey tests gain their completion steps (docs/acceptance/m3.md lists them)
// and this test is deleted.
func TestM3ReviewGuardNotWired(t *testing.T) {
	e := kit.New(t, kit.Options{NoQA: true, NoChatOps: true})
	item := e.Admit(e.NewBareItem("Review edge", "# review edge"))
	events := e.WorkEvents("work_item.transitioned", item.Id)

	code, body := e.Transition("engineer", item, "review", map[string]any{"review": map[string]any{
		"candidateCommit": e.Repo.Initial, "evidence": []map[string]any{{"id": e.Repo.Initial, "type": "commit"}},
	}})
	if code != http.StatusServiceUnavailable || m2.ErrorCode(body) != "unavailable" {
		t.Fatalf("active -> review with a candidate commit: %d %s, the review subject guard is expected to fail closed", code, body)
	}
	// Without a claim the entry is refused for what it is.
	if code, body = e.Transition("engineer", item, "review", nil); code != http.StatusUnprocessableEntity || m2.ErrorCode(body) != "prerequisite_unmet" {
		t.Fatalf("active -> review without a claim: %d %s", code, body)
	}
	// Nobody completes from active: done is reachable only from review.
	for _, who := range []string{"engineer", "reviewer", "admin"} {
		if code, _ := e.Transition(who, item, "done", map[string]any{"completion": map[string]any{"contentRevision": item.ContentRevision, "acceptedCriteria": []string{"AC-1", "AC-2"}}}); code < 400 {
			t.Fatalf("%s completed an active item", who)
		}
	}
	got := e.GetItem(item.Id)
	if got.State != "active" || got.Version != item.Version || got.ReviewSubject != nil || got.Completion != nil || e.WorkEvents("work_item.transitioned", item.Id) != events || len(e.History(item.Id)) != 3 {
		t.Fatalf("a refused review transition wrote something: %+v", got)
	}
}
