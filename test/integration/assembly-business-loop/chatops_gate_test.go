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

// TestChatopsGateIntegration (chatops_gate_integration, transferred from T-046)
// verifies the notifications of the REAL gate producer through the assembled
// ChatOps path (outbox, JetStream or repair, inbox, planner, delivery worker,
// outbound client): the events a gate commits, the payload projected from them,
// the destinations that select them, and the authority around configuration.
//
//   - Configuration is project:settings only, bounded by the installation's
//     destination policy (https, no user-info, no private address that is not
//     allowed, no unknown event types, unique names).
//   - A gate's requested, decided and invalidated events reach exactly the
//     destinations that select each, once; a disabled destination and another
//     tenant's destination receive nothing.
//   - The payload announces the committed gate: its version at that event, the
//     subject digest and content revision, the current-record link, a bounded
//     summary, and no credential, no evidence content and no decision text.
//   - The decision announced is the immutable one the gate recorded, for both
//     verdicts.
//
// Covers AC-08, AC-09, DQ-04, DQ-A03 and IT-03 (tenant scope).
func TestChatopsGateIntegration(t *testing.T) {
	e := kit.New(t, kit.Options{NoQA: true, ForeignReceiver: true})
	def := e.GatePipeline("chatops-gate")
	dests := e.ProjectPath() + "/chatops-destinations"

	// ---- configuration authority and policy ----
	good := map[string]any{"name": "policy-probe", "endpoint": e.Receiver.URL(), "events": []string{"gate.requested"}, "enabled": true}
	for _, who := range []string{"engineer", "reviewer", "viewer", "outsider", "foreign"} {
		if code, b := e.Do(who, "POST", dests, good, nil); code != http.StatusForbidden && code != http.StatusNotFound {
			t.Fatalf("%s configured a destination: %d %s", who, code, b)
		}
	}
	refused := map[string]map[string]any{
		"a plain http endpoint":             {"name": "p1", "endpoint": strings.Replace(e.Receiver.URL(), "https://", "http://", 1), "events": []string{"gate.requested"}},
		"user-info in the endpoint":         {"name": "p2", "endpoint": strings.Replace(e.Receiver.URL(), "https://", "https://user:pw@", 1), "events": []string{"gate.requested"}},
		"a query in the endpoint":           {"name": "p3", "endpoint": e.Receiver.URL() + "?token=1", "events": []string{"gate.requested"}},
		"a private address nobody allowed":  {"name": "p4", "endpoint": "https://10.1.2.3:" + portOf(e) + "/hook", "events": []string{"gate.requested"}},
		"the metadata address":              {"name": "p5", "endpoint": "https://169.254.169.254:" + portOf(e) + "/hook", "events": []string{"gate.requested"}},
		"a port the policy does not permit": {"name": "p6", "endpoint": "https://127.0.0.1:1/hook", "events": []string{"gate.requested"}},
		"an event outside the allow-list":   {"name": "p7", "endpoint": e.Receiver.URL(), "events": []string{"secret.revealed"}},
		"no events":                         {"name": "p8", "endpoint": e.Receiver.URL(), "events": []string{}},
	}
	for name, body := range refused {
		if code, b := e.Do("admin", "POST", dests, body, nil); code < 400 || code >= 500 {
			t.Fatalf("%s: %d %s", name, code, b)
		}
	}
	if n := len(e.DestinationList()); n != 0 {
		t.Fatalf("%d destinations after only refusals", n)
	}

	// ---- destinations: who selects what ----
	onlyRequested := e.Destination("requested-only", "gate.requested")
	onlyDecided := e.Destination("decided-only", "gate.decided")
	allGate := e.Destination("all-gate", "gate.requested", "gate.decided", "gate.invalidated")
	disabled := e.Destination("disabled", "gate.requested", "gate.decided", "gate.invalidated")
	e.Disable(disabled)
	if code, b := e.Do("admin", "POST", dests, map[string]any{"name": "all-gate", "endpoint": e.Receiver.URL(), "events": []string{"gate.requested"}}, nil); code != http.StatusConflict {
		t.Fatalf("a duplicate destination name: %d %s", code, b)
	}
	// Tenant B's own destination cannot be configured by tenant A and receives nothing of tenant A.
	foreignPath := "/api/v1/tenants/" + m2.TenantB + "/projects/" + e.World.ProjectB + "/chatops-destinations"
	if code, b := e.Do("admin", "GET", foreignPath, nil, nil); code != http.StatusForbidden && code != http.StatusNotFound {
		t.Fatalf("tenant A's administrator reached tenant B's destinations: %d %s", code, b)
	}
	var foreignDest client.Destination
	if err := json.Unmarshal(e.MustDo("foreign", "POST", foreignPath, map[string]any{"name": "globex", "endpoint": e.Receiver.URL(), "events": kit.AllEvents, "enabled": true}, nil, http.StatusCreated), &foreignDest); err != nil {
		t.Fatal(err)
	}

	// ---- an approved review: requested, then decided ----
	item := e.NewItem("ChatOps gate", "# chatops gate")
	run := e.StartRun("cg-run", item, def.Id, nil)
	g := e.WaitGate(run.Id, 3*time.Minute)
	req := e.WaitReceived("gate.requested", g.Id, time.Minute)
	if code, b := e.Decide("reviewer", g, "approve", "the stage satisfies the criteria; see "+strings.Repeat("evidence ", 5), "cg-approve"); code != http.StatusOK {
		t.Fatalf("approve: %d %s", code, b)
	}
	if f := e.World.WaitRun(run.Id, 2*time.Minute); f.Status != client.RunStatusSucceeded {
		t.Fatalf("run %s (%s)", f.Status, f.Reason)
	}
	dec := e.WaitReceived("gate.decided", g.Id, time.Minute)
	decided := e.GetGate("reviewer", g.Id)

	link := e.Links.Origin + "/api/v1/tenants/" + m2.TenantA + "/projects/" + e.World.Project + "/gates/" + g.Id
	if req.URL != link || dec.URL != link || req.Resource.Type != "gate" || req.Resource.ID != g.Id || req.TenantID != m2.TenantA || req.ProjectID != e.World.Project {
		t.Fatalf("requested %+v decided %+v, want link %s", req, dec, link)
	}
	if req.Resource.Version != int64(g.Version) || dec.Resource.Version != int64(decided.Version) || dec.Resource.Version <= req.Resource.Version {
		t.Fatalf("announced versions %d, %d vs committed %d, %d", req.Resource.Version, dec.Resource.Version, g.Version, decided.Version)
	}
	if req.Attributes["subjectDigest"] != string(g.SubjectDigest) || req.Attributes["contentRevision"] != "1" || req.Attributes["reviewerScope"] == "" {
		t.Fatalf("requested attributes %+v", req.Attributes)
	}
	if dec.Attributes["subjectDigest"] != string(g.SubjectDigest) {
		t.Fatalf("decided attributes %+v", dec.Attributes)
	}
	for _, p := range []kit.Payload{req, dec} {
		raw, _ := json.Marshal(p)
		if len(p.Summary) > 256 || len(raw) > 8<<10 || len(p.Attributes) > 12 || p.Event.CorrelationID == "" || p.NotificationID == "" || p.SchemaVersion != 1 {
			t.Fatalf("payload bounds: %s", raw)
		}
		for _, banned := range []string{"bearer", "secret", "password", "authorization"} {
			if strings.Contains(strings.ToLower(string(raw)), banned) {
				t.Fatalf("a notification carries %q: %s", banned, raw)
			}
		}
		if strings.Contains(string(raw), "the stage satisfies") {
			t.Fatalf("the reviewer's free text is in the notification: %s", raw)
		}
	}
	// The link opens the gate the notification announced, at its current subject (DQ-04).
	if code, body := e.Follow("reviewer", req.URL); code != http.StatusOK || !strings.Contains(string(body), string(g.SubjectDigest)) {
		t.Fatalf("the link: %d %s", code, body)
	}

	// ---- a rejected review and an invalidated one ----
	item2 := e.NewItem("ChatOps gate two", "# chatops gate two")
	run2 := e.StartRun("cg-run-2", item2, def.Id, nil)
	g2 := e.WaitGate(run2.Id, 3*time.Minute)
	e.WaitReceived("gate.requested", g2.Id, time.Minute)
	if code, b := e.Decide("reviewer", g2, "reject", "not acceptable", "cg-reject"); code != http.StatusOK {
		t.Fatalf("reject: %d %s", code, b)
	}
	rej := e.WaitReceived("gate.decided", g2.Id, time.Minute)
	if rej.Resource.Version != int64(e.GetGate("reviewer", g2.Id).Version) || rej.Attributes["subjectDigest"] != string(g2.SubjectDigest) {
		t.Fatalf("the rejection announced %+v", rej)
	}
	if dec.Attributes["decision"] != "approved" || rej.Attributes["decision"] != "rejected" || dec.Attributes["decidedByType"] != "user" {
		t.Fatalf("an approval and a rejection are announced as %v / %v", dec.Attributes, rej.Attributes)
	}

	item3 := e.NewItem("ChatOps gate three", "# chatops gate three")
	run3 := e.StartRun("cg-run-3", item3, def.Id, nil)
	g3 := e.WaitGate(run3.Id, 3*time.Minute)
	e.WaitReceived("gate.requested", g3.Id, time.Minute)
	e.EditSpec(item3, "# chatops gate three\nchanged after the request")
	if code, b := e.Decide("reviewer", g3, "approve", "stale", "cg-stale"); code != http.StatusConflict || m2.ErrorCode(b) != "stale_gate_subject" {
		t.Fatalf("stale approval: %d %s", code, b)
	}
	inv := e.WaitReceived("gate.invalidated", g3.Id, time.Minute)
	if inv.Resource.Version <= int64(g3.Version) || inv.Attributes["subjectDigest"] != string(g3.SubjectDigest) {
		t.Fatalf("invalidation announced %+v (gate version %d)", inv, g3.Version)
	}

	// ---- who got what: exactly the destinations that select the event, once ----
	e.WaitSettled(time.Minute)
	count := func(dest client.Destination, eventType string) int {
		n := 0
		for _, d := range e.Deliveries("admin", "destinationId="+dest.Id) {
			if string(d.EventType) == eventType {
				n++
			}
			if d.State != "delivered" {
				t.Fatalf("delivery %+v", d)
			}
		}
		return n
	}
	type want struct {
		dest                   client.Destination
		req, dec, inv, workTrn int
	}
	for name, w := range map[string]want{
		"requested-only": {onlyRequested, 3, 0, 0, 0},
		"decided-only":   {onlyDecided, 0, 2, 0, 0},
		"all-gate":       {allGate, 3, 2, 1, 0},
		"disabled":       {disabled, 0, 0, 0, 0},
	} {
		if got := [3]int{count(w.dest, "gate.requested"), count(w.dest, "gate.decided"), count(w.dest, "gate.invalidated")}; got != [3]int{w.req, w.dec, w.inv} {
			t.Fatalf("%s destination received requested/decided/invalidated %v, want %v", name, got, [3]int{w.req, w.dec, w.inv})
		}
		if n := count(w.dest, "work_item.transitioned"); n != 0 {
			t.Fatalf("%s destination received %d work transitions it never selected", name, n)
		}
	}
	// Tenant B's destination saw nothing of tenant A, and tenant A cannot read its history.
	var bPage struct{ Items []client.Delivery }
	if err := json.Unmarshal(e.MustDo("foreign", "GET", "/api/v1/tenants/"+m2.TenantB+"/projects/"+e.World.ProjectB+"/chatops-deliveries?destinationId="+foreignDest.Id, nil, nil, http.StatusOK), &bPage); err != nil || len(bPage.Items) != 0 {
		t.Fatalf("tenant B's destination has %d deliveries from tenant A's gates (%v)", len(bPage.Items), err)
	}
	if code, _ := e.Do("admin", "GET", "/api/v1/tenants/"+m2.TenantB+"/projects/"+e.World.ProjectB+"/chatops-deliveries", nil, nil); code != http.StatusForbidden && code != http.StatusNotFound {
		t.Fatalf("tenant A's administrator read tenant B's deliveries: %d", code)
	}
	if a, b := e.TenantCount(m2.TenantA, `SELECT count(*) FROM chatops_deliveries`), e.TenantCount(m2.TenantB, `SELECT count(*) FROM chatops_deliveries`); a == 0 || b != 0 {
		t.Fatalf("row level security: tenant A %d, tenant B %d deliveries", a, b)
	}
}

func portOf(e *kit.Env) string {
	return e.Receiver.AddrPort().String()[strings.LastIndex(e.Receiver.AddrPort().String(), ":")+1:]
}
