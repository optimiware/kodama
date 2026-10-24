package pipeline_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
	clipipeline "github.com/optimiware/kodama/internal/cli/pipeline"
	"github.com/optimiware/kodama/internal/cli/root"
	"github.com/optimiware/kodama/internal/pipeline"
)

func Test_pipeline_register_sends_the_document_itself(t *testing.T) {
	v := m1Validator(t)
	for _, name := range []string{"minimal-test", "build-and-publish"} {
		src := readFixture(t, filepath.Join("valid", name+".yaml"))
		want := strings.TrimSpace(string(readFixture(t, filepath.Join("canonical", name+".digest"))))
		srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteJSON(w, 201, definitionJSON("pd_1", name, 1, want), map[string]string{"ETag": `"1"`})
		}))
		r := runWith(t, v, srv.URL, "", "pipeline", "register", "--file", writeFile(t, name+".yaml", string(src)), "--output", "json")
		if r.code != 0 {
			t.Fatalf("%s: exit %d %s", name, r.code, r.err)
		}
		reqs := srv.Requests()
		if len(reqs) != 1 || reqs[0].Method != "POST" || reqs[0].Path != base+"/pipeline-definitions" {
			t.Fatalf("%s: requests %+v", name, reqs)
		}
		if h := reqs[0].Header; h.Get("Content-Type") != "application/json" || h.Get("If-Match") != "" || h.Get("Idempotency-Key") != "" {
			t.Fatalf("%s: headers %v", name, h)
		}
		// The body is the Pipeline document, not a file name or wrapper, and
		// parses back to exactly what the local parser read from the YAML.
		sent, err := pipeline.ParseJSON(reqs[0].Body, pipeline.Options{MaxBytes: 1 << 20})
		if err != nil {
			t.Fatalf("%s: sent body is not a pipeline document: %v: %s", name, err, reqs[0].Body)
		}
		parsed, err := v.ParseYAML(src)
		if err != nil || !reflect.DeepEqual(sent, parsed) {
			t.Fatalf("%s: sent document differs from the parsed YAML: %v", name, err)
		}
		validated, err := v.ValidateJSON(context.Background(), scopeOf(), diagnosticActor, reqs[0].Body)
		if err != nil || string(validated.Digest()) != want {
			t.Fatalf("%s: the sent document has digest %s (%v), fixture says %s", name, validated.Digest(), err, want)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(r.out), &out); err != nil || out["id"] != "pd_1" || out["digest"] != want {
			t.Fatalf("%s: output %q", name, r.out)
		}
	}
}

func Test_pipeline_register_json_file_and_replay_report(t *testing.T) {
	src := readFixture(t, filepath.Join("valid", "minimal-test.json"))
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteJSON(w, 200, definitionJSON("pd_1", "test", 4, "sha256:"+strings.Repeat("2", 64)), nil)
	}))
	r := runWith(t, m1Validator(t), srv.URL, "", "pipeline", "register", "--file", writeFile(t, "p.json", string(src)))
	if r.code != 0 || !strings.Contains(r.out, "already registered") || !strings.Contains(r.out, "version 4") {
		t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
	}
	var sent, orig map[string]any
	if err := json.Unmarshal(srv.Requests()[0].Body, &sent); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(src, &orig); err != nil || !reflect.DeepEqual(sent, orig) {
		t.Fatalf("a JSON source must be sent as written")
	}
}

func Test_pipeline_register_refuses_unparseable_documents_before_sending(t *testing.T) {
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("request sent") }))
	for name, doc := range map[string]string{
		"duplicate key": "apiVersion: kodama/v1alpha1\napiVersion: kodama/v1alpha1\n",
		"unknown field": "apiVersion: kodama/v1alpha1\nkind: Pipeline\nmetadata: {name: p}\nspec: {steps: [], extra: 1}\n",
		"not yaml":      "{{{",
	} {
		r := runWith(t, m1Validator(t), srv.URL, "", "pipeline", "register", "--file", writeFile(t, "p.yaml", doc), "--output", "json")
		if r.code != 2 || r.out != "" || !strings.Contains(r.err, `"code":"invalid_document"`) {
			t.Errorf("%s: exit %d err %q", name, r.code, r.err)
		}
	}
}

func Test_pipeline_register_leaves_semantic_validation_to_the_server(t *testing.T) {
	doc := cycleDoc() // a graph cycle: a semantic, not structural, violation
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteJSON(w, 422, map[string]any{"error": map[string]any{
			"code": "invalid_definition", "message": "the pipeline definition violates one or more rules", "requestId": "req_422",
			"details": map[string]any{"truncated": false, "diagnostics": []any{map[string]any{"code": "cycle", "path": "spec.steps", "message": "the graph has a cycle"}}},
		}}, nil)
	}))
	r := runWith(t, m1Validator(t), srv.URL, "", "pipeline", "register", "--file", writeFile(t, "p.yaml", doc), "--output", "json")
	if r.code != 1 || !strings.Contains(r.err, "cycle") || !strings.Contains(r.err, "req_422") {
		t.Fatalf("exit %d err %q", r.code, r.err)
	}
	if len(srv.Requests()) != 1 {
		t.Fatalf("requests %d", len(srv.Requests()))
	}
}

func cycleDoc() string {
	img := "registry.example.invalid/x@sha256:" + strings.Repeat("1", 64)
	return "apiVersion: kodama/v1alpha1\nkind: Pipeline\nmetadata: {name: p}\nspec:\n  steps:\n" +
		"    - {id: a, kind: container, needs: [b], image: " + img + ", command: [x], timeoutSeconds: 5}\n" +
		"    - {id: b, kind: container, needs: [a], image: " + img + ", command: [x], timeoutSeconds: 5}\n"
}

func Test_pipeline_register_lost_response_is_not_retried(t *testing.T) {
	src := readFixture(t, filepath.Join("valid", "minimal-test.yaml"))
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		c, _, _ := hj.Hijack()
		_ = c.Close()
	}))
	r := runWith(t, m1Validator(t), srv.URL, "", "pipeline", "register", "--file", writeFile(t, "p.yaml", string(src)), "--output", "json")
	if r.code != 1 || r.out != "" {
		t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
	}
	if !strings.Contains(r.err, `"outcomeUncertain":true`) || !strings.Contains(r.err, "content-addressed") || strings.Contains(r.err, "idempotency-key") {
		t.Fatalf("recovery guidance: %s", r.err)
	}
	if n := len(srv.Requests()); n != 1 {
		t.Fatalf("%d requests: registration is never retried automatically", n)
	}
}

func Test_pipeline_get_and_list(t *testing.T) {
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pipeline-definitions") {
			clienttest.WriteJSON(w, 200, map[string]any{"items": []any{definitionJSON("pd_1", "unit-tests", 2, "sha256:"+strings.Repeat("a", 64))}, "nextCursor": "x.y", "later": 1}, nil)
			return
		}
		clienttest.WriteJSON(w, 200, definitionJSON("pd_1", "unit-tests", 2, "sha256:"+strings.Repeat("a", 64)), nil)
	}))
	r := runWith(t, m1Validator(t), srv.URL, "", "pipeline", "list", "--name", "unit-tests", "--limit", "5", "--cursor", "c.d", "--output", "json")
	if r.code != 0 || !strings.Contains(r.out, `"nextCursor":"x.y"`) || !strings.Contains(r.out, `"later":1`) {
		t.Fatalf("list: exit %d out %q err %q", r.code, r.out, r.err)
	}
	q := srv.Requests()[0].RawQuery
	for _, want := range []string{"name=unit-tests", "limit=5", "cursor=c.d"} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q lacks %q", q, want)
		}
	}
	g := runWith(t, m1Validator(t), srv.URL, "", "pipeline", "get", "pd_1")
	if g.code != 0 || !strings.Contains(g.out, "unit-tests") || !strings.Contains(g.out, "version 2") {
		t.Fatalf("get: exit %d out %q", g.code, g.out)
	}
	if got := srv.Requests()[1].Path; got != base+"/pipeline-definitions/pd_1" {
		t.Fatalf("path %s", got)
	}
}

func Test_scope_cursor_output_contract(t *testing.T) {
	for _, tc := range []struct{ status, exit int }{{401, 3}, {403, 3}, {404, 1}, {429, 1}, {503, 1}} {
		srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteError(w, tc.status, "denied", "m", "req_p", map[string]string{"Retry-After": "4"})
		}))
		r := runWith(t, m1Validator(t), srv.URL, "", "pipeline", "get", "pd_foreign", "--output", "json")
		if int(r.code) != tc.exit || r.out != "" {
			t.Errorf("%d: exit %d out %q", tc.status, r.code, r.out)
		}
	}
	// Without a project selection and a diagnostic scope, local validation has
	// nothing to check references against and says so.
	contrib, err := clipipeline.NewCommands(clipipeline.Config{Validator: m1Validator(t), Actor: diagnosticActor})
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	rn, _ := root.NewRunner(root.RunnerConfig{Streams: root.Streams{Out: &out, Err: &errb}}, contrib)
	src := readFixture(t, filepath.Join("valid", "minimal-test.yaml"))
	if code := rn.Run(context.Background(), []string{"pipeline", "validate", "--file", writeFile(t, "p.yaml", string(src))}); code != 2 || !strings.Contains(errb.String(), "no project selected") {
		t.Fatalf("exit %d err %q", code, errb.String())
	}
}
