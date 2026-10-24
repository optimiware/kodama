package assemblybusinessloop_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	m2 "github.com/optimiware/kodama/test/e2e/m2/kit"
	kit "github.com/optimiware/kodama/test/e2e/m3/kit"
)

// TestBusinessLoopGateLinks (business_loop_gate_links) is AC-A07 and DQ-04 with
// the REAL gate producer, authorization and applicability behind the link, not
// the module-local target fixture of T-041: the review link a notification
// carries is the authenticated current-record URL of the gate, built from the
// installation's canonical origin, and it grants nothing.
//
//   - Anyone can hold the URL (it is in a webhook payload); each follower is
//     authenticated and authorized by the gate route against the record as it
//     is NOW: no credential gets nothing, a caller outside the project learns
//     nothing, a caller with read access reads but cannot decide, the mapping
//     principal and the engineer cannot decide, whatever the URL says.
//   - Opening the link (or adding decision-looking query parameters, or using
//     another method) decides nothing and changes no byte of the gate.
//   - A link forwarded to a second reviewer lets that reviewer decide as
//     themselves: the decision is attributed to the caller, never to the
//     recipient of the notification.
//   - A reviewer whose binding is revoked after the notification is refused by
//     the same URL.
//   - When the requirements change after the notification, the same URL shows
//     the gate as invalidated for its old subject, the old subject's digest
//     cannot approve, and a fresh review is a different record at a different
//     URL; the invalidation is announced once through the same link.
func TestBusinessLoopGateLinks(t *testing.T) {
	e := kit.New(t, kit.Options{}) // the QA mapping principal is bound in the project as analyst
	def := e.GatePipeline("links")
	e.Destination("review-channel")
	reviewer, analyst := e.UserID("reviewer"), e.UserID("analyst")
	if reviewer == analyst {
		t.Fatal("the fixture has one reviewer")
	}

	start := func(key, title string) (client.WorkItem, client.Run, client.Gate, kit.Payload) {
		item := e.NewItem(title, "# "+title+"\nA reviewer inspects the stage.")
		run := e.StartRun(key, item, def.Id, nil)
		g := e.WaitGate(run.Id, 3*time.Minute)
		note := e.WaitReceived("gate.requested", g.Id, time.Minute)
		return item, run, g, note
	}
	wantURL := func(g client.Gate) string {
		return e.Links.Origin + "/api/v1/tenants/" + m2.TenantA + "/projects/" + e.World.Project + "/gates/" + g.Id
	}
	gateBytes := func(id string) string {
		code, b := e.Do("reviewer", "GET", e.ProjectPath()+"/gates/"+id, nil, nil)
		if code != http.StatusOK {
			t.Fatalf("read gate: %d %s", code, b)
		}
		return string(b)
	}

	// ---- the link and who may follow it ----
	_, run1, g1, note := start("links-1", "Links one")
	if note.URL != wantURL(g1) || !strings.HasPrefix(note.URL, "https://") {
		t.Fatalf("link %q, want %q", note.URL, wantURL(g1))
	}
	for _, n := range e.Received() {
		if !strings.HasPrefix(n.URL, e.Links.Origin+"/api/v1/tenants/"+m2.TenantA+"/projects/") {
			t.Fatalf("a notification link is not built from the canonical origin: %s", n.URL)
		}
	}
	if strings.Contains(note.URL, "?") || strings.Contains(note.URL, "#") || strings.Contains(strings.ToLower(note.URL), "token") {
		t.Fatalf("the link carries more than the record's address: %s", note.URL)
	}
	before := gateBytes(g1.Id)
	type who struct {
		persona string
		read    []int
		decide  []int
	}
	deny := []int{http.StatusForbidden, http.StatusNotFound}
	for _, c := range []who{
		{"", []int{http.StatusUnauthorized}, []int{http.StatusUnauthorized}},
		{"foreign", deny, deny},  // tenant B's administrator
		{"outsider", deny, deny}, // claims galore, no binding
		{"viewer", []int{200}, deny},
		{"engineer", []int{200}, deny},
		{"admin", deny, deny},                 // administers membership and settings, reads no review records
		{kit.MapperPersona, []int{200}, deny}, // the QA mapping principal holds no decision authority
	} {
		code, body := e.Follow(c.persona, note.URL)
		if !has(c.read, code) {
			t.Fatalf("%q following the link: %d %s", c.persona, code, body)
		}
		if code == http.StatusNotFound || code == http.StatusForbidden {
			if strings.Contains(string(body), g1.Id) || strings.Contains(string(body), string(g1.SubjectDigest)) {
				t.Fatalf("%q learned something of the gate from a refusal: %s", c.persona, body)
			}
		}
		if c.persona == "" {
			continue
		}
		if code, b := e.Decide(c.persona, g1, "approve", "I hold the link", "links-nope-"+c.persona); !has(c.decide, code) {
			t.Fatalf("%q decided through possession of the link: %d %s", c.persona, code, b)
		}
	}
	if after := gateBytes(g1.Id); after != before {
		t.Fatalf("followers and refused decisions changed the gate:\n%s\n%s", before, after)
	}

	// Opening the link, with decision-looking parameters or another method, decides nothing.
	for _, u := range []string{note.URL + "?decision=approve", note.URL + "?approve=true&subjectDigest=" + string(g1.SubjectDigest)} {
		if code, _ := e.Follow("reviewer", u); code >= 500 {
			t.Fatalf("follow %s: %d", u, code)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		code, _ := e.FollowWith(method, "reviewer", note.URL, map[string]any{"decision": "approve", "subjectDigest": string(g1.SubjectDigest), "reason": "by link"})
		if code < 400 {
			t.Fatalf("%s on the link answered %d", method, code)
		}
	}
	if after := gateBytes(g1.Id); after != before {
		t.Fatalf("a link request changed the gate:\n%s\n%s", before, after)
	}
	if got := e.GetGate("reviewer", g1.Id); got.Status != "pending" {
		t.Fatalf("gate %s", got.Status)
	}

	// ---- forwarded to a second reviewer: the decision is theirs ----
	forwarded := note.URL // the notification forwarded to a colleague
	code, body := e.Follow("analyst", forwarded)
	var seen client.Gate
	if code != http.StatusOK || json.Unmarshal(body, &seen) != nil || seen.Id != g1.Id {
		t.Fatalf("the forwarded link for a second reviewer: %d %s", code, body)
	}
	if code, b := e.Decide("analyst", seen, "approve", "inspected by the second reviewer", "links-forwarded"); code != http.StatusOK {
		t.Fatalf("decision by the second reviewer: %d %s", code, b)
	}
	decided := e.GetGate("reviewer", g1.Id)
	if decided.Decision == nil || decided.Decision.DecidedBy.Id != analyst || decided.Decision.DecidedBy.Id == reviewer {
		t.Fatalf("the decision is attributed to %+v, want the caller %s and not the recipient %s", decided.Decision, analyst, reviewer)
	}
	if f := e.World.WaitRun(run1.Id, 2*time.Minute); f.Status != client.RunStatusSucceeded {
		t.Fatalf("run %s (%s)", f.Status, f.Reason)
	}
	// The same URL now shows the decision (the link is the current record, not a snapshot).
	if code, body := e.Follow("viewer", note.URL); code != http.StatusOK || !strings.Contains(string(body), `"approved"`) {
		t.Fatalf("the link after the decision: %d %s", code, body)
	}
	if dn := e.WaitReceived("gate.decided", g1.Id, time.Minute); dn.URL != note.URL {
		t.Fatalf("the decision was announced with %s, not the same record link %s", dn.URL, note.URL)
	}

	// ---- changed requirements: the same URL shows the old review as invalid ----
	item2, run2, g2, note2 := start("links-2", "Links two")
	revised := e.EditSpec(item2, "# Links two\nThe requirement changed after the notification.")
	if revised.ContentRevision != item2.ContentRevision+1 {
		t.Fatalf("content revision %d", revised.ContentRevision)
	}
	code, body = e.Decide("reviewer", g2, "approve", "approving the old subject", "links-stale")
	if code != http.StatusConflict || m2.ErrorCode(body) != "stale_gate_subject" {
		t.Fatalf("approving a gate whose requirements changed: %d %s", code, body)
	}
	if code, b := e.Follow("reviewer", note2.URL); code != http.StatusOK {
		t.Fatalf("follow: %d %s", code, b)
	} else {
		var now client.Gate
		if err := json.Unmarshal(b, &now); err != nil || now.Status == "approved" || now.Applicability.Applicable || now.Invalidation == nil || now.SubjectDigest != g2.SubjectDigest || now.Subject.ContentRevision != item2.ContentRevision {
			t.Fatalf("the link after the change shows %s: %v %v", b, err, now.Invalidation)
		}
	}
	if inv := e.WaitReceived("gate.invalidated", g2.Id, time.Minute); inv.URL != note2.URL {
		t.Fatalf("the invalidation was announced with %s, not %s", inv.URL, note2.URL)
	}
	if n := len(e.ReceivedFor("gate.invalidated", g2.Id)); n != 1 {
		t.Fatalf("%d invalidation notifications", n)
	}
	time.Sleep(2 * time.Second)
	if f := e.World.GetRun(run2.Id); f.Status == client.RunStatusSucceeded || e.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1 AND step_id = 'build'`, run2.Id) != 0 {
		t.Fatalf("run %s: a stale approval progressed the run", f.Status)
	}
	// The fresh review of the new requirements is another record at another URL.
	run2b := e.StartRun("links-2b", revised, def.Id, nil)
	g2b := e.WaitGate(run2b.Id, 3*time.Minute)
	note2b := e.WaitReceived("gate.requested", g2b.Id, time.Minute)
	if g2b.Id == g2.Id || note2b.URL == note2.URL || g2b.SubjectDigest == g2.SubjectDigest || g2b.Subject.ContentRevision != revised.ContentRevision {
		t.Fatalf("the fresh review: %+v vs %+v", g2b.Subject, g2.Subject)
	}
	if code, b := e.DecideAt("reviewer", g2b, int64(g2b.Version), string(g2.SubjectDigest), "approve", "old digest", "links-2b-old"); code != http.StatusConflict || m2.ErrorCode(b) != "stale_gate_subject" {
		t.Fatalf("the old subject's digest decided the fresh gate: %d %s", code, b)
	}
	if code, b := e.Decide("reviewer", g2b, "approve", "the revised requirement is satisfied", "links-2b"); code != http.StatusOK {
		t.Fatalf("approve the fresh review: %d %s", code, b)
	}

	// ---- a reviewer revoked after the notification is refused by the same URL ----
	_, _, g3, note3 := start("links-3", "Links three")
	if code, _ := e.Follow("reviewer", note3.URL); code != http.StatusOK {
		t.Fatalf("follow before the revocation: %d", code)
	}
	e.RemoveMember("reviewer")
	if code, body := e.Follow("reviewer", note3.URL); code != http.StatusForbidden && code != http.StatusNotFound {
		t.Fatalf("the revoked reviewer following the link: %d %s", code, body)
	}
	if code, b := e.Decide("reviewer", g3, "approve", "revoked", "links-revoked"); code != http.StatusForbidden && code != http.StatusNotFound {
		t.Fatalf("the revoked reviewer decided: %d %s", code, b)
	}
	if got := e.GetGate("analyst", g3.Id); got.Status != "pending" || got.Version != g3.Version {
		t.Fatalf("a revoked caller changed the gate: %+v", got)
	}
	if code, _ := e.Follow("analyst", note3.URL); code != http.StatusOK {
		t.Fatalf("the remaining reviewer following the link: %d", code)
	}
	if code, b := e.Decide("analyst", g3, "reject", "not acceptable", "links-3-reject"); code != http.StatusOK {
		t.Fatalf("the remaining reviewer: %d %s", code, b)
	}
}

func has(set []int, v int) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
