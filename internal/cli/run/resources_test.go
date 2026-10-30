package run_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
)

func sharedRunDoc() map[string]any {
	d := runDoc("running", 4)
	d["runModel"] = "run_shared"
	d["deadlineAt"] = "2026-10-19T16:05:00Z"
	d["executionOrder"] = []any{"generate", "build"}
	d["effectiveAccess"] = map[string]any{"generate": "write", "build": "read"}
	d["cleanupReason"] = nil
	d["resources"] = []any{
		map[string]any{"kind": "workspace", "key": "workspace", "phase": "ready", "reason": nil, "readyAt": "2026-10-19T10:05:10Z", "stoppedAt": nil, "removedAt": nil},
		map[string]any{"kind": "preparation", "key": "preparation", "phase": "stopped", "reason": nil, "readyAt": "2026-10-19T10:05:20Z", "stoppedAt": "2026-10-19T10:05:40Z", "removedAt": nil},
		map[string]any{"kind": "service", "key": "service.db", "phase": "failed", "reason": "service_lost", "readyAt": "2026-10-19T10:05:30Z", "stoppedAt": nil, "removedAt": nil, "future": "kept"},
	}
	return d
}

func resourceMeta(resource string) string {
	return rec(map[string]any{"type": "meta", "runId": runID, "resource": resource, "origin": "live", "completeness": "unknown", "reasons": []any{},
		"tail": 100, "follow": false, "terminal": false, "limits": map[string]any{"maxBytes": 1, "maxLines": 1, "maxLineBytes": 1, "maxDurationSeconds": 1}})
}

// Test_unit_cli_run_resources_view: `run get` shows the run model, the deadline,
// the execution order with each step's effective access and the resources of a
// run_shared run (and renders an attempt_private run exactly as before), keeping
// members the CLI does not know in the JSON; `run logs --resource` reads the log
// of the preparation or a service with only the supported query parameters,
// refuses the combinations it must, and renders nothing for a response that is not
// the requested resource's or not complete.
func Test_unit_cli_run_resources_view(t *testing.T) {
	var served map[string]any
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteJSON(w, 200, served, map[string]string{"ETag": `"4"`})
	}))

	served = sharedRunDoc()
	g := runCLI(t, srv, "run", "get", runID)
	if g.code != 0 {
		t.Fatalf("get: exit %d err %q", g.code, g.err)
	}
	for _, want := range []string{
		"run model: run_shared  deadline: 2026-10-19T16:05:00Z",
		"execution order: generate (write) -> build (read)",
		"resources:", "workspace", "ready", "preparation", "stopped", "service.db", "failed", "(service_lost)",
	} {
		if !strings.Contains(g.out, want) {
			t.Errorf("get lacks %q:\n%s", want, g.out)
		}
	}
	j := runCLI(t, srv, "run", "get", runID, "--output", "json")
	doc := decodeJSON(t, j.out)
	rs, _ := doc["resources"].([]any)
	if doc["runModel"] != "run_shared" || len(rs) != 3 || rs[2].(map[string]any)["future"] != "kept" || doc["executionOrder"] == nil {
		t.Fatalf("json view: %s", j.out)
	}

	// An attempt_private run renders as before: no model, no resources.
	served = runDoc("running", 3)
	plain := runCLI(t, srv, "run", "get", runID)
	if plain.code != 0 || strings.Contains(plain.out, "run model") || strings.Contains(plain.out, "resources") {
		t.Fatalf("an attempt_private run grew a resource view: %q", plain.out)
	}
	// A run_shared run with no resource yet says so.
	served = sharedRunDoc()
	served["resources"] = []any{}
	if none := runCLI(t, srv, "run", "get", runID); !strings.Contains(none.out, "resources: none") {
		t.Fatalf("no resources: %q", none.out)
	}
	served = sharedRunDoc()
	served["cleanupReason"] = "cleanup_pending"
	if held := runCLI(t, srv, "run", "get", runID); !strings.Contains(held.out, "held by resources: cleanup_pending") {
		t.Fatalf("cleanup reason: %q", held.out)
	}

	// Resource logs.
	body := func(resource string) string {
		return resourceMeta(resource) + chunkRec("listening on 5432", false) + chunkRec("ready", false) + summaryRec(2, false, "end")
	}
	logs := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		resource := r.URL.Query().Get("resource")
		switch r.URL.Query().Get("tail") {
		case "9":
			_, _ = w.Write([]byte(body("service.other"))) // another resource's log
		case "8":
			_, _ = w.Write([]byte(resourceMeta(resource) + chunkRec("partial", false))) // no summary
		default:
			_, _ = w.Write([]byte(body(resource)))
		}
	}))
	ok := runCLI(t, logs, "run", "logs", runID, "--resource", "service.db", "--tail", "50")
	if ok.code != 0 {
		t.Fatalf("resource logs: exit %d err %q", ok.code, ok.err)
	}
	for _, want := range []string{"resource service.db", "listening on 5432", "ready", "not the run's outcome"} {
		if !strings.Contains(ok.out, want) {
			t.Errorf("resource log lacks %q: %q", want, ok.out)
		}
	}
	reqs := logs.Requests()
	last := reqs[len(reqs)-1]
	q, _ := url.ParseQuery(last.RawQuery)
	if last.Path != base+"/runs/"+runID+"/logs" || q.Get("resource") != "service.db" || q.Get("tail") != "50" || len(q) != 2 {
		t.Fatalf("log request %s ? %s", last.Path, last.RawQuery)
	}
	if p := runCLI(t, logs, "run", "logs", runID, "--resource", "preparation", "--output", "json"); p.code != 0 ||
		decodeJSON(t, p.out)["meta"].(map[string]any)["resource"] != "preparation" {
		t.Fatalf("preparation log: %d %s %s", p.code, p.out, p.err)
	}
	n := len(logs.Requests())
	for name, args := range map[string][]string{
		"a malformed resource":   {"--resource", "service.Bad_Name"},
		"another kind":           {"--resource", "workspace"},
		"with an attempt":        {"--resource", "preparation", "--attempt", "att_1"},
		"with a step":            {"--resource", "preparation", "--step", "build"},
		"with follow":            {"--resource", "preparation", "--follow"},
		"with a source":          {"--resource", "preparation", "--source", "live"},
		"an empty resource name": {"--resource", ""},
	} {
		if r := runCLI(t, logs, append([]string{"run", "logs", runID}, args...)...); r.code != 2 || r.out != "" {
			t.Errorf("%s: exit %d out %q", name, r.code, r.out)
		}
	}
	if len(logs.Requests()) != n {
		t.Error("a refused combination reached the server")
	}
	if mism := runCLI(t, logs, "run", "logs", runID, "--resource", "service.db", "--tail", "9"); mism.code == 0 || mism.out != "" || !strings.Contains(mism.err, "logs_mismatch") {
		t.Errorf("another resource's log was rendered: %d %q %q", mism.code, mism.out, mism.err)
	}
	if cut := runCLI(t, logs, "run", "logs", runID, "--resource", "service.db", "--tail", "8"); cut.code == 0 || cut.out != "" || !strings.Contains(cut.err, "logs_incomplete") {
		t.Errorf("an interrupted resource log was rendered: %d %q %q", cut.code, cut.out, cut.err)
	}
}
