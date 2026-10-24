package work_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
	"github.com/optimiware/kodama/internal/cli/configuration"
	"github.com/optimiware/kodama/internal/cli/pipeline"
	"github.com/optimiware/kodama/internal/cli/root"
	"github.com/optimiware/kodama/internal/cli/run"
	"github.com/optimiware/kodama/internal/cli/work"
	"github.com/optimiware/kodama/internal/cli/workspace"
	pipelinelib "github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

const (
	runIdentifier = "run_0123456789abcdef01234567"
	secondRun     = "run_fedcba9876543210fedcba98"
	head          = "1111111111111111111111111111111111111111"
)

// journeyFactory is a stateful HTTP fixture shaped by the api/ fragments'
// examples. It enforces the contract rules the CLI must honour (If-Match,
// Idempotency-Key, scope prefix, bearer token) and records every request.
type journeyFactory struct {
	t  *testing.T
	mu sync.Mutex

	item     map[string]any
	history  []any
	runs     map[string]string // idempotency key -> run id
	runState map[string]string
	posts    []string // method path of every mutation, in order
	wsState  string
	wsVer    int64
}

func (f *journeyFactory) record(kind string, changed []any, before, after map[string]any, extra map[string]any) {
	v, _ := f.item["version"].(int64)
	rev, _ := f.item["contentRevision"].(int64)
	e := map[string]any{
		"id": "hist_" + itoa(len(f.history)+1), "version": v, "contentRevision": rev, "kind": kind,
		"actor": map[string]any{"type": "user", "id": "usr_analyst"}, "occurredAt": "2026-10-19T10:10:00Z",
		"changedFields": changed, "before": before, "after": after,
	}
	for k, val := range extra {
		e[k] = val
	}
	f.history = append(f.history, e)
}

func (f *journeyFactory) workspace(version int64, state string) map[string]any {
	return map[string]any{
		"id": "ws_1042", "workItemId": "wi_1042", "repositoryId": "repo_1", "branch": "refs/heads/kodama/work/wi_1042",
		"acceptedCommit": head, "state": state, "writerAttemptId": nil, "publication": nil, "archiveRequestedAt": nil,
		"lastError": nil, "version": version, "createdAt": "2026-10-19T10:00:00Z", "updatedAt": "2026-10-19T10:00:00Z",
	}
}

func (f *journeyFactory) bump(extra map[string]any) {
	v, _ := f.item["version"].(int64)
	f.item["version"] = v + 1
	f.item["updatedAt"] = "2026-10-19T10:10:00Z"
	for k, val := range extra {
		f.item[k] = val
	}
}

func (f *journeyFactory) fail(w http.ResponseWriter, status int, code string) {
	clienttest.WriteError(w, status, code, code, "req_"+code, nil)
}

func (f *journeyFactory) body(r *http.Request) map[string]any {
	var m map[string]any
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	_ = dec.Decode(&m)
	return m
}

func (f *journeyFactory) expect(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer tok-1" {
		f.fail(w, 401, "unauthenticated")
		return false
	}
	if !strings.HasPrefix(r.URL.Path, base) {
		f.fail(w, 404, "not_found")
		return false
	}
	return true
}

func (f *journeyFactory) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.expect(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, base)
	if r.Method != "GET" {
		f.posts = append(f.posts, r.Method+" "+path)
	}
	ifMatch := r.Header.Get("If-Match")
	switch {
	case r.Method == "POST" && path == "/work-items":
		b := f.body(r)
		f.item = itemJSON("wi_1042", "backlog", 1, 1)
		f.item["spec"], f.item["title"] = b["spec"], b["title"]
		f.item["acceptanceCriteria"] = b["acceptanceCriteria"]
		f.record("created", []any{"title", "spec"}, map[string]any{}, map[string]any{"title": b["title"], "spec": b["spec"]}, nil)
		clienttest.WriteJSON(w, 201, f.item, nil)
	case path == "/work-items/wi_1042" && r.Method == "GET":
		clienttest.WriteJSON(w, 200, f.item, nil)
	case path == "/work-items/wi_1042" && r.Method == "PATCH":
		if ifMatch != `"`+itoa(f.item["version"])+`"` {
			f.fail(w, 412, "version_conflict")
			return
		}
		b := f.body(r)
		prev := f.item["spec"]
		if b["spec"] == prev {
			// A real server records no content change for identical content.
			f.fail(w, 422, "no_change")
			return
		}
		f.bump(map[string]any{"spec": b["spec"], "contentRevision": f.item["contentRevision"].(int64) + 1})
		f.record("updated", []any{"spec"}, map[string]any{"spec": prev}, map[string]any{"spec": b["spec"]}, nil)
		clienttest.WriteJSON(w, 200, f.item, nil)
	case path == "/work-items/wi_1042/transitions":
		if ifMatch != `"`+itoa(f.item["version"])+`"` {
			f.fail(w, 412, "version_conflict")
			return
		}
		b := f.body(r)
		to, _ := b["to"].(string)
		if to == "done" && b["completion"] == nil {
			f.fail(w, 422, "prerequisite_unmet")
			return
		}
		from := f.item["state"]
		f.bump(map[string]any{"state": to})
		f.record("transitioned", []any{"state"}, map[string]any{"state": from}, map[string]any{"state": to}, map[string]any{"from": from, "to": to})
		clienttest.WriteJSON(w, 200, f.item, nil)
	case path == "/work-items/wi_1042/history":
		clienttest.WriteJSON(w, 200, map[string]any{"items": f.history, "nextCursor": nil}, nil)
	case path == "/work-items/wi_1042/workspace":
		f.wsState, f.wsVer = "available", 3
		clienttest.WriteJSON(w, 201, f.workspace(f.wsVer, f.wsState), nil)
	case path == "/workspaces/ws_1042":
		clienttest.WriteJSON(w, 200, f.workspace(f.wsVer, f.wsState), nil)
	case path == "/workspaces/ws_1042/archive" || path == "/workspaces/ws_1042/restore":
		if ifMatch != `"`+itoa(f.wsVer)+`"` {
			f.fail(w, 412, "version_conflict")
			return
		}
		want, next := "available", "archived"
		if strings.HasSuffix(path, "/restore") {
			want, next = "archived", "available"
		}
		if f.wsState != want {
			f.fail(w, 409, "invalid_state")
			return
		}
		f.wsState, f.wsVer = next, f.wsVer+1
		clienttest.WriteJSON(w, 200, f.workspace(f.wsVer, f.wsState), nil)
	case path == "/pipeline-definitions" && r.Method == "POST":
		clienttest.WriteJSON(w, 201, map[string]any{
			"id": "pd_01", "name": "test", "version": 1, "digest": "sha256:2bd5fefbd645e5ebbc320019f611ddd14517b4a2458b465a1e3ab2c2ff1b1831",
			"definition": map[string]any{}, "createdBy": map[string]any{"type": "user", "id": "usr_1"}, "createdAt": "2026-10-19T10:05:00Z",
		}, nil)
	case path == "/runs" && r.Method == "POST":
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			f.fail(w, 428, "key_required")
			return
		}
		id, seen := f.runs[key]
		if !seen {
			id = []string{runIdentifier, secondRun}[len(f.runs)]
			f.runs[key] = id
			f.runState[id] = "queued"
		}
		clienttest.WriteJSON(w, 201, runBody(id, f.runState[id]), nil)
	case strings.HasPrefix(path, "/runs/") && strings.HasSuffix(path, "/cancel"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/runs/"), "/cancel")
		d := runBody(id, "running")
		d["cancelRequestedAt"] = "2026-10-19T10:07:00Z"
		clienttest.WriteJSON(w, 202, d, nil)
	case path == "/runs/"+runIdentifier+"/events":
		f.stream(w, r)
	case path == "/runs/"+runIdentifier && r.Method == "GET":
		clienttest.WriteJSON(w, 200, runBody(runIdentifier, f.runState[runIdentifier]), nil)
	case path == "/runs/"+runIdentifier+"/steps":
		clienttest.WriteJSON(w, 200, map[string]any{"items": []any{map[string]any{
			"id": "test", "kind": "container", "status": "succeeded", "attemptsStarted": 1, "maxAttempts": 1,
			"attempts": []any{map[string]any{"id": "att_1", "ordinal": 1, "status": "succeeded", "reason": "ok", "settled": true}},
		}}, "nextCursor": nil}, nil)
	case path == "/runs/"+runIdentifier+"/logs":
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, `{"type":"meta","runId":"`+runIdentifier+`","attemptId":"att_1","stepId":"test","origin":"wrapper","completeness":"complete","reasons":[],"tail":100,"follow":false,"terminal":true,"limits":{"maxBytes":1,"maxLines":1,"maxLineBytes":1,"maxDurationSeconds":1}}`+"\n"+
			`{"type":"chunk","text":"ok  inventory 0.4s","lineTruncated":false}`+"\n"+
			`{"type":"summary","lines":1,"bytes":18,"truncated":false,"stop":"end"}`+"\n")
	case path == "/secrets" && r.Method == "POST":
		if r.Header.Get("Idempotency-Key") == "" {
			f.fail(w, 428, "key_required")
			return
		}
		clienttest.WriteJSON(w, 201, map[string]any{"id": "sec_1", "name": "erp-read-token", "kind": "generic", "ref": "secret://project/erp-read-token",
			"currentVersion": 1, "version": 1, "createdAt": "2026-10-19T10:00:00Z", "updatedAt": "2026-10-19T10:00:00Z", "deletedAt": nil}, nil)
	case path == "/variables" && r.Method == "POST":
		clienttest.WriteJSON(w, 201, variableDoc(), nil)
	case path == "/variables" && r.Method == "GET":
		clienttest.WriteJSON(w, 200, map[string]any{"items": []any{variableDoc()}, "nextCursor": nil}, nil)
	default:
		f.t.Errorf("fixture has no route for %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func variableDoc() map[string]any {
	return map[string]any{"id": "var_1", "name": "region", "type": "string", "value": "eu", "valueVersion": 1, "description": "",
		"version": 1, "createdAt": "2026-10-19T10:00:00Z", "updatedAt": "2026-10-19T10:00:00Z", "deletedAt": nil}
}

func (f *journeyFactory) stream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	fl := w.(http.Flusher)
	write := func(s string) { _, _ = io.WriteString(w, s); fl.Flush() }
	ev := func(id string, version int, to string) string {
		b, _ := json.Marshal(map[string]any{"id": "evt_" + id, "schemaVersion": 1, "type": "run.state_changed", "tenantId": tenant, "projectId": project,
			"resource": map[string]any{"type": "run", "id": runIdentifier, "version": version}, "data": map[string]any{"from": "queued", "to": to}})
		return "event: run.state_changed\nid: c" + id + ".s\ndata: " + string(b) + "\n\n"
	}
	write("event: snapshot\nid: c0.s\ndata: {\"cursor\":\"c0.s\",\"resources\":[{\"type\":\"run\",\"id\":\"" + runIdentifier + "\",\"version\":1,\"state\":\"queued\"}]}\n\n")
	write(ev("1", 2, "running"))
	f.runState[runIdentifier] = "succeeded"
	write(ev("2", 3, "succeeded"))
	<-r.Context().Done()
}

func runBody(id, status string) map[string]any {
	return map[string]any{
		"id": id, "purpose": "work", "workItemId": "wi_1042", "definitionId": "pd_01", "definitionDigest": "sha256:" + strings.Repeat("2", 64),
		"contentRevision": 2, "sourceCommit": head, "status": status, "reason": "created", "cancelRequestedAt": nil, "failureReason": nil,
		"inputSnapshot": map[string]any{"digest": "sha256:" + strings.Repeat("3", 64), "inputs": []any{}, "secrets": []any{}},
		"requestedBy":   map[string]any{"type": "user", "id": "usr_1"}, "sourceRetention": "confirmed", "version": 1,
		"createdAt": "2026-10-19T10:05:00Z", "startedAt": nil, "finishedAt": nil,
	}
}

func itoa(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// journey drives the assembled contributions through the real runner.
type journey struct {
	t      *testing.T
	runner *root.Runner
	out    *bytes.Buffer
	errb   *bytes.Buffer
	stdin  *bytes.Buffer
	dir    string
}

func newJourney(t *testing.T, factoryURL string) *journey {
	t.Helper()
	j := &journey{t: t, out: &bytes.Buffer{}, errb: &bytes.Buffer{}, stdin: &bytes.Buffer{}, dir: t.TempDir()}
	validator, err := pipelinelib.NewValidator(pipelinelib.Config{
		Capabilities: pipelinelib.EnabledKinds{pipelinelib.KindContainer}, References: pipelinetest.References{},
	})
	if err != nil {
		t.Fatal(err)
	}
	pc, err := pipeline.NewCommands(pipeline.Config{Validator: validator, Actor: pipelinelib.Actor{ID: "local-cli", Type: pipelinelib.ActorUser}})
	if err != nil {
		t.Fatal(err)
	}
	rc, err := run.NewCommands(run.Config{Tokens: tokenResolver{clienttest.Tokens("tok-1")}, AllowInsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	j.runner, err = root.NewRunner(root.RunnerConfig{
		Streams:               root.Streams{In: j.stdin, Out: j.out, Err: j.errb},
		Contexts:              root.NewFileContextStore(filepath.Join(j.dir, "kodama", "contexts.json")),
		Tokens:                tokenResolver{clienttest.Tokens("tok-1")},
		AllowInsecureLoopback: true,
	}, root.NewContextCommands(), work.NewCommands(), workspace.NewCommands(), pc, rc,
		configuration.NewVariableCommands(), configuration.NewSecretCommands())
	if err != nil {
		t.Fatal(err)
	}
	j.do(0, "context", "set", "--context", "dev", "--factory", factoryURL, "--select")
	j.do(0, "context", "use", "--tenant", tenant, "--project", project)
	return j
}

// do runs one command and requires its exit code; it returns stdout.
func (j *journey) do(want int, args ...string) string {
	j.t.Helper()
	j.out.Reset()
	j.errb.Reset()
	code := j.runner.Run(context.Background(), args)
	if int(code) != want {
		j.t.Fatalf("%v: exit %d, want %d; stderr %q", args, code, want, j.errb.String())
	}
	return j.out.String()
}

// Test_cli_core_journey: the assembled core contributions, the real runner and
// client and a contract-shaped HTTP fixture complete the M1 workflow through
// public routes with deterministic JSON, and a successful run never completes
// the work item: completion is a separate human transition.
func Test_cli_core_journey(t *testing.T) {
	f := &journeyFactory{t: t, runs: map[string]string{}, runState: map[string]string{}}
	srv := clienttest.NewServer(t, f)
	j := newJourney(t, srv.URL)
	files := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(files, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	spec := write("specification.md", "# Plan\nResolve inventory exceptions.\n", 0o600)
	revised := write("specification-r2.md", "# Plan\nResolve inventory exceptions.\n\nRevised: reviewers also see the supplier.\n", 0o600)
	acceptance := write("acceptance.json", `[{"id":"AC-1","text":"A reviewer resolves an exception."}]`, 0o600)
	pipelineFile := write("pipeline.yaml", string(readPipelineFixture(t)), 0o600)

	// Work: create, edit under a version, then explicit transitions.
	out := decode(t, j.do(0, "work", "create", "--kind", "story", "--title", "Resolve inventory exceptions", "--spec-file", spec, "--acceptance-file", acceptance, "--output", "json"))
	if out["id"] != "wi_1042" || itoa(out["version"]) != "1" {
		t.Fatalf("create: %v", out)
	}
	edited := decode(t, j.do(0, "work", "update", "wi_1042", "--spec-file", revised, "--if-version", "1", "--output", "json"))
	if itoa(edited["contentRevision"]) != "2" || !strings.Contains(edited["spec"].(string), "Revised: reviewers also see the supplier.") {
		t.Fatalf("update did not carry the revised specification: %v", edited)
	}
	j.do(0, "work", "transition", "wi_1042", "--to", "ready", "--if-version", "2", "--reason", "Input ready", "--output", "json")
	active := decode(t, j.do(0, "work", "transition", "wi_1042", "--to", "active", "--if-version", "3", "--reason", "Pull work", "--output", "json"))
	if active["state"] != "active" || itoa(active["version"]) != "4" || itoa(active["contentRevision"]) != "2" {
		t.Fatalf("active: %v", active)
	}
	// A stale version is a conflict (exit 4), never retried or refreshed.
	j.do(4, "work", "transition", "wi_1042", "--to", "review", "--if-version", "3")

	// History is the server's immutable record of the edit and the transitions.
	hist := decode(t, j.do(0, "work", "history", "wi_1042", "--output", "json"))
	entries := hist["items"].([]any)
	if len(entries) != 4 {
		t.Fatalf("history has %d entries: %v", len(entries), hist)
	}
	if upd := entries[1].(map[string]any); upd["kind"] != "updated" || itoa(upd["contentRevision"]) != "2" ||
		!strings.Contains(itoa(upd["after"]), "Revised: reviewers") || !strings.Contains(itoa(upd["before"]), "Resolve inventory exceptions.") {
		t.Fatalf("the edit is not recorded with its before and after content: %v", upd)
	}
	if h := j.do(0, "work", "history", "wi_1042"); !strings.Contains(h, "updated") || !strings.Contains(h, "ready -> active") {
		t.Fatalf("history text: %q", h)
	}

	// Workspace.
	ws := j.do(0, "workspace", "provision", "--work", "wi_1042", "--output", "json")
	if !strings.Contains(ws, `"branch":"refs/heads/kodama/work/wi_1042"`) || !strings.Contains(ws, `"acceptedCommit":"`+head+`"`) {
		t.Fatalf("provision: %s", ws)
	}
	j.do(0, "workspace", "get", "ws_1042", "--output", "json")
	// Archive and restore address the same workspace under its observed version;
	// a stale version conflicts (exit 4) and is never refreshed by the CLI.
	archived := decode(t, j.do(0, "workspace", "archive", "ws_1042", "--if-version", "3", "--output", "json"))
	if archived["state"] != "archived" || itoa(archived["version"]) != "4" {
		t.Fatalf("archive: %v", archived)
	}
	j.do(4, "workspace", "restore", "ws_1042", "--if-version", "3")
	restored := decode(t, j.do(0, "workspace", "restore", "ws_1042", "--if-version", "4", "--output", "json"))
	if restored["id"] != "ws_1042" || restored["state"] != "available" || itoa(restored["version"]) != "5" {
		t.Fatalf("restore: %v", restored)
	}

	// Pipeline: local validation (no network) then registration.
	before := len(srv.Requests())
	j.do(0, "pipeline", "validate", "--file", pipelineFile, "--output", "json")
	if len(srv.Requests()) != before {
		t.Fatal("pipeline validate contacted the factory")
	}
	j.do(0, "pipeline", "register", "--file", pipelineFile, "--output", "json")

	// Run: keyed start, watch to terminal, logs, a second run cancelled.
	started := decode(t, j.do(0, "run", "start", "--work", "wi_1042", "--definition", "pd_01", "--content-revision", "2",
		"--source-commit", head, "--idempotency-key", "run-create-01", "--output", "json"))
	if started["status"] != "queued" || started["id"] != runIdentifier {
		t.Fatalf("start: %v", started)
	}
	replay := decode(t, j.do(0, "run", "start", "--work", "wi_1042", "--definition", "pd_01", "--content-revision", "2",
		"--source-commit", head, "--idempotency-key", "run-create-01", "--output", "json"))
	if replay["id"] != runIdentifier {
		t.Fatalf("replay: %v", replay)
	}
	watched := decode(t, j.do(0, "run", "watch", runIdentifier, "--timeout", "20s", "--output", "json"))
	if watched["run"].(map[string]any)["status"] != "succeeded" {
		t.Fatalf("watch: %v", watched)
	}
	logs := j.do(0, "run", "logs", runIdentifier, "--step", "test", "--output", "json")
	if !strings.Contains(logs, `"text":"ok  inventory 0.4s"`) || !strings.Contains(logs, `"completeness":"complete"`) {
		t.Fatalf("logs: %s", logs)
	}
	second := decode(t, j.do(0, "run", "start", "--work", "wi_1042", "--definition", "pd_01", "--content-revision", "2",
		"--source-commit", head, "--idempotency-key", "run-create-02", "--output", "json"))
	cancelled := decode(t, j.do(0, "run", "cancel", second["id"].(string), "--output", "json"))
	if cancelled["status"] != "running" || cancelled["cancelRequestedAt"] == nil {
		t.Fatalf("cancel: %v", cancelled)
	}

	// Configuration metadata. The secret arrives on redirected standard input.
	j.stdin.WriteString("erp-token-value\n")
	j.do(0, "secret", "create", "erp-read-token", "--kind", "generic", "--value-file", "-", "--idempotency-key", "secret-01", "--output", "json")
	j.do(0, "variable", "create", "region", "--value", "eu", "--idempotency-key", "var-01", "--output", "json")
	j.do(0, "variable", "list", "--output", "json")

	// A passing run must not have completed or moved the work item.
	transitions := 0
	for _, p := range f.posts {
		if strings.HasSuffix(p, "/transitions") {
			transitions++
		}
	}
	if transitions != 3 { // ready, active, and the rejected stale attempt: all explicit
		t.Fatalf("%d transition requests after the run; only the user's explicit ones are allowed: %v", transitions, f.posts)
	}
	if got := decode(t, j.do(0, "work", "get", "wi_1042", "--output", "json")); got["state"] != "active" || got["completion"] != nil {
		t.Fatalf("a passing run changed the work item: %v", got)
	}

	// Human completion is a separate, explicit, versioned transition.
	review := write("review.json", `{"candidateCommit":"`+head+`","evidence":[{"type":"artifact","id":"art_report_1"}]}`, 0o600)
	j.do(0, "work", "transition", "wi_1042", "--to", "review", "--if-version", "4", "--review-file", review, "--output", "json")
	completion := write("completion.json", `{"contentRevision":2,"candidateCommit":"`+head+`","acceptedCriteria":["AC-1"]}`, 0o600)
	done := decode(t, j.do(0, "work", "transition", "wi_1042", "--to", "done", "--if-version", "5", "--reason", "Accepted", "--completion-file", completion, "--output", "json"))
	if done["state"] != "done" {
		t.Fatalf("done: %v", done)
	}

	// Every recorded request was a scoped, authenticated public route.
	for _, rq := range srv.Requests() {
		if !strings.HasPrefix(rq.Path, base+"/") || rq.Header.Get("Authorization") != "Bearer tok-1" {
			t.Errorf("request outside the scope or unauthenticated: %s %s", rq.Method, rq.Path)
		}
	}
	// Deterministic JSON: the same read twice is byte-identical.
	a := j.do(0, "work", "get", "wi_1042", "--output", "json")
	b := j.do(0, "work", "get", "wi_1042", "--output", "json")
	if a != b || !strings.HasSuffix(a, "}\n") || strings.Count(a, "\n") != 1 {
		t.Fatalf("output is not one deterministic document: %q vs %q", a, b)
	}
	if strings.Contains(srv.Requests()[0].Header.Get("Authorization"), "erp-token-value") {
		t.Fatal("secret leaked")
	}
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	return decodeJSON(t, s)
}

func readPipelineFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "pipeline", "v1", "fixtures", "valid", "minimal-test.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
