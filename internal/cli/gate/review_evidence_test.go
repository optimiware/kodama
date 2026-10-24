package gate_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
)

// rawGate is a gate body written as the server would, with a large integer, a
// nullable deployment, an additive member, a conformance snapshot, an
// invalidation and a recorded decision.
const rawGate = `{"id":"` + gateID + `","runId":"run_0123456789abcdef01234567","stepId":"review","status":"approved",` +
	`"subjectDigest":"` + digest + `",` +
	`"subject":{"tenantId":"t_acme","projectId":"p_inventory","workItemId":"wi_1042","contentRevision":9007199254740991,` +
	`"candidateCommit":"1111111111111111111111111111111111111111","definitionId":"pd_01",` +
	`"evidence":[{"id":"art_report","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},` +
	`{"id":"art_tests","digest":"` + digest + `"}],"deploymentId":null,"deploymentConfigDigest":null},` +
	`"required":{"evidence":[{"role":"report","id":"art_report","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","expression":null,"producer":"design"}],` +
	`"conformance":{"status":"blocked","code":"conformance_blocked","producerStep":"design","baselineDigest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",` +
	`"requiredChecks":["module-boundaries","tenant-authority"],"reportId":"art_report","reportDigest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}},` +
	`"requestedAt":"2026-10-19T10:05:00Z","deadline":"2026-10-20T10:05:00Z",` +
	`"decision":{"decidedBy":{"type":"user","id":"usr_reviewer"},"decision":"reject","reason":"The report is blocked.","decidedAt":"2026-10-19T11:00:00Z"},` +
	`"invalidation":{"at":"2026-10-19T12:00:00Z","reason":"content_changed","supersedingCommit":null,"supersedingRevision":9007199254740992},` +
	`"applicability":{"expired":false,"applicable":false,"reason":"invalidated"},` +
	`"version":9007199254740991,"createdAt":"2026-10-19T10:05:00Z","updatedAt":"2026-10-19T11:00:00Z","futureField":{"b":1,"a":[1.50,null]}}`

func rawHandler(t *testing.T, status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"9007199254740991"`)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(rawGate))
	})
}

// TestJSONReviewEvidence: inspection is exactly one GET, never approves, and
// its deterministic JSON keeps the complete subject, evidence and provenance,
// conformance, nullable deployment fields, applicability, decision history,
// exact large integers and additive members.
func TestJSONReviewEvidence(t *testing.T) {
	c := newCLI(t, rawHandler(t, 200))
	r := c.run("gate", "get", gateID, "--output", "json")
	if r.code != 0 || r.err != "" {
		t.Fatalf("exit %d err %q", r.code, r.err)
	}
	reqs := c.srv.Requests()
	if len(reqs) != 1 || reqs[0].Method != "GET" || reqs[0].Path != gatesAt || len(reqs[0].Body) != 0 {
		t.Fatalf("inspection must be one body-less GET: %+v", reqs)
	}
	if reqs[0].Header.Get("Authorization") != "Bearer tok-1" || reqs[0].Header.Get("If-Match") != "" || reqs[0].Header.Get("Idempotency-Key") != "" {
		t.Fatalf("headers %v", reqs[0].Header)
	}
	for _, want := range []string{
		`"version":9007199254740991`, `"contentRevision":9007199254740991`, `"supersedingRevision":9007199254740992`,
		`"deploymentId":null`, `"deploymentConfigDigest":null`, `"supersedingCommit":null`,
		`"futureField":{"a":[1.50,null],"b":1}`,
		`"conformance":{"baselineDigest":"sha256:cccc`, `"requiredChecks":["module-boundaries","tenant-authority"]`,
		`"producer":"design"`, `"expression":null`,
		`"applicability":{"applicable":false,"expired":false,"reason":"invalidated"}`,
		`"decision":{"decidedAt":"2026-10-19T11:00:00Z","decidedBy":{"id":"usr_reviewer","type":"user"},"decision":"reject","reason":"The report is blocked."}`,
		`"evidence":[{"digest":"sha256:bbbb`, `"id":"art_tests"`,
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("JSON lacks %s\n%s", want, r.out)
		}
	}
	if !strings.HasSuffix(r.out, "}\n") || strings.Count(r.out, "\n") != 1 {
		t.Fatalf("output is not one newline-terminated document: %q", r.out)
	}
	again := newCLI(t, rawHandler(t, 200)).run("gate", "get", gateID, "--output", "json")
	if again.out != r.out {
		t.Fatal("JSON output is not deterministic")
	}
}

func TestJSONReviewEvidenceHumanText(t *testing.T) {
	c := newCLI(t, rawHandler(t, 200))
	r := c.run("gate", "get", gateID)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.err)
	}
	for _, want := range []string{
		"version: 9007199254740991 (use with --if-version)", "subject digest: " + digest + " (use with --subject-digest)",
		"work item wi_1042", "content revision 9007199254740991", "candidate 1111111111111111111111111111111111111111",
		"evidence: art_tests " + digest, "evidence: art_report sha256:bbbb",
		"required evidence: report art_report", "produced by design",
		"conformance: blocked code conformance_blocked", "deployment: - config digest -",
		"invalidated: content_changed", "not proof current facts authorize progression", "applicable false, reason invalidated",
		"decision: reject by user/usr_reviewer",
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("text lacks %q\n%s", want, r.out)
		}
	}
}

// TestJSONReviewEvidenceReadingNeverDecides: reading a pending, approved,
// rejected, expired or inapplicable gate succeeds without any mutation.
func TestJSONReviewEvidenceReadingNeverDecides(t *testing.T) {
	for _, status := range []string{"pending", "approved", "rejected"} {
		c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteJSON(w, 200, gateJSON(status, 1), map[string]string{"ETag": `"1"`})
		}))
		for _, args := range [][]string{{"gate", "get", gateID}, {"gate", "get", gateID, "--output", "json"}} {
			if r := c.run(args...); r.code != 0 {
				t.Fatalf("%s %v: exit %d %s", status, args, r.code, r.err)
			}
		}
		for _, req := range c.srv.Requests() {
			if req.Method != "GET" || strings.HasSuffix(req.Path, "/decisions") {
				t.Fatalf("%s: inspection sent %s %s", status, req.Method, req.Path)
			}
		}
	}
}
