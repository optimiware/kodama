package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/entrypoint"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

// call sends one request to the composed process and returns what it answered.
func (d *devSlice) call(method, path, token, body string, headers ...string) (int, http.Header, []byte) {
	d.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, d.Server.URL+path, rd)
	if err != nil {
		d.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		d.t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, b
}

// publicSchemas validates a served body against the response schema the
// aggregate document declares for the operation and status. A status the
// operation does not declare is itself a divergence.
type publicSchemas struct {
	doc      map[string]any
	validate func(ref string, body []byte) error
}

func loadPublicSchemas(t testing.TB) *publicSchemas {
	t.Helper()
	path := filepath.Join(repoRoot(t), "api", "openapi.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	translateNullable(doc)
	return &publicSchemas{doc: doc, validate: compileFile(t, path, true)}
}

// responseSchema finds "#/…/content/application~1json/schema" of an operation's
// response, following $ref responses.
func (p *publicSchemas) responseSchema(template, method string, status int) (string, bool) {
	ptr := "#/paths/" + pointerToken(template) + "/" + strings.ToLower(method) + "/responses/" + strconv.Itoa(status)
	for i := 0; i < 8; i++ {
		node, ok := pointer(p.doc, ptr)
		if !ok {
			return "", false
		}
		m, _ := node.(map[string]any)
		if ref, ok := m["$ref"].(string); ok {
			ptr = ref
			continue
		}
		if _, ok := pointer(p.doc, ptr+"/content/application~1json/schema"); !ok {
			return "", false
		}
		return ptr + "/content/application~1json/schema", true
	}
	return "", false
}

func (p *publicSchemas) check(template, method string, status int, body []byte) error {
	ref, ok := p.responseSchema(template, method, status)
	if !ok {
		return fmt.Errorf("%s %s declares no JSON response for status %d", method, template, status)
	}
	return p.validate(strings.NewReplacer("{", "%7B", "}", "%7D").Replace(ref), body)
}

func mustJSON(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func field(t testing.TB, body []byte, name string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	s, _ := m[name].(string)
	if s == "" {
		t.Fatalf("no %q in %s", name, body)
	}
	return s
}

// checkPublicPayloads (a subtest of m1_contract_consistency): the successful payloads the composed
// process serves validate against the response schemas of the aggregate
// document, and failures are the documented statuses. The generated client
// tolerates missing members; this check does not.
func checkPublicPayloads(t *testing.T) {
	d := newDevSlice(t)
	schemas := loadPublicSchemas(t)
	base := APIPrefix + "/tenants/" + string(devTenant)
	proj := base + "/projects/" + string(devProject)
	const (
		tProjects = "/tenants/{tenantId}/projects"
		tProject  = "/tenants/{tenantId}/projects/{projectId}"
		tp        = "/tenants/{tenantId}/projects/{projectId}"
	)
	var checked int
	do := func(method, template, path, token, body string, want int, headers ...string) []byte {
		t.Helper()
		status, _, b := d.call(method, path, token, body, headers...)
		if status != want {
			t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, status, want, b)
		}
		if err := schemas.check(template, method, status, b); err != nil {
			t.Errorf("%s %s (%d) does not match the published schema: %v\n%s", method, template, status, err, b)
		}
		checked++
		return b
	}
	eng, admin, viewer := devEngineerToken, devAdminToken, devViewerToken

	do("GET", "/me", APIPrefix+"/me", eng, "", 200)
	do("GET", "/tenants", APIPrefix+"/tenants", eng, "", 200)
	do("GET", tProjects, base+"/projects", eng, "", 200)
	do("GET", tProject, proj, eng, "", 200)
	do("GET", "/tenants/{tenantId}/memberships", base+"/memberships", admin, "", 200)

	spec := "Run the checks."
	item := do("POST", tp+"/work-items", proj+"/work-items", eng,
		mustJSON(t, map[string]any{"kind": "story", "title": "payloads", "spec": spec, "acceptanceCriteria": []any{map[string]any{"id": "ac_1", "text": "passes"}}}), 201)
	itemID := field(t, item, "id")
	do("GET", tp+"/work-items/{id}", proj+"/work-items/"+itemID, eng, "", 200)
	do("GET", tp+"/work-items", proj+"/work-items", eng, "", 200)
	do("GET", tp+"/work-items/{id}/history", proj+"/work-items/"+itemID+"/history", viewer, "", 200)

	do("POST", tp+"/variables", proj+"/variables", eng, `{"name":"region","value":"eu"}`, 201, "Idempotency-Key", "payload-var")
	vars := do("GET", tp+"/variables", proj+"/variables", viewer, "", 200)
	var page struct {
		Items []struct{ ID string } `json:"items"`
	}
	if err := json.Unmarshal(vars, &page); err != nil || len(page.Items) != 1 {
		t.Fatalf("variables: %v %s", err, vars)
	}
	do("GET", tp+"/variables/{id}", proj+"/variables/"+page.Items[0].ID, viewer, "", 200)

	sec := do("POST", tp+"/secrets", proj+"/secrets", admin, `{"kind":"generic","name":"token","value":"c2VjcmV0LXZhbHVl"}`, 201, "Idempotency-Key", "payload-secret")
	if bytes.Contains(sec, []byte("c2VjcmV0")) || bytes.Contains(sec, []byte("secret-value")) {
		t.Fatalf("a secret value was served: %s", sec)
	}
	secID := field(t, sec, "id")
	do("GET", tp+"/secrets", proj+"/secrets", admin, "", 200)
	do("GET", tp+"/secrets/{id}", proj+"/secrets/"+secID, admin, "", 200)
	do("GET", tp+"/secrets/{id}/versions", proj+"/secrets/"+secID+"/versions", admin, "", 200)

	doc := map[string]any{
		"apiVersion": "kodama/v1alpha1", "kind": "Pipeline", "metadata": map[string]any{"name": "payloads"},
		"spec": map[string]any{"inputs": map[string]any{}, "steps": []any{map[string]any{
			"id": "check", "kind": "container", "timeoutSeconds": 60,
			"image":   "registry.example.invalid/go-tools@sha256:1111111111111111111111111111111111111111111111111111111111111111",
			"command": []string{"/bin/sh", "-ec"}, "args": []string{"echo ok"}, "workspace": map[string]any{"from": "run", "access": "read"},
		}}},
	}
	def := do("POST", tp+"/pipeline-definitions", proj+"/pipeline-definitions", eng, mustJSON(t, doc), 201)
	defID := field(t, def, "id")
	do("GET", tp+"/pipeline-definitions", proj+"/pipeline-definitions", eng, "", 200)
	do("GET", tp+"/pipeline-definitions/{id}", proj+"/pipeline-definitions/"+defID, eng, "", 200)

	ws := do("POST", tp+"/work-items/{id}/workspace", proj+"/work-items/"+itemID+"/workspace", eng, `{}`, 201)
	do("GET", tp+"/workspaces/{id}", proj+"/workspaces/"+field(t, ws, "id"), eng, "", 200)

	var wi struct {
		ContentRevision int `json:"contentRevision"`
	}
	if err := json.Unmarshal(item, &wi); err != nil {
		t.Fatal(err)
	}
	runBody := mustJSON(t, map[string]any{"workItemId": itemID, "definitionId": defID, "contentRevision": wi.ContentRevision, "sourceCommit": string(d.Repo.Initial)})
	run := do("POST", tp+"/runs", proj+"/runs", eng, runBody, 201, "Idempotency-Key", "payload-run")
	runID := field(t, run, "id")
	do("POST", tp+"/runs", proj+"/runs", eng, runBody, 201, "Idempotency-Key", "payload-run") // the replay
	do("GET", tp+"/runs/{id}", proj+"/runs/"+runID, viewer, "", 200)
	do("GET", tp+"/runs", proj+"/runs", viewer, "", 200)
	do("GET", tp+"/runs/{id}/steps", proj+"/runs/"+runID+"/steps", viewer, "", 200)
	do("POST", tp+"/runs/{id}/cancel", proj+"/runs/"+runID+"/cancel", eng, "", 202)

	// The documented failures.
	do("GET", tp+"/runs/{id}", proj+"/runs/run_unknown", eng, "", 404)
	do("GET", tp+"/runs/{id}", proj+"/runs/"+runID, "", "", 401)
	do("POST", tp+"/work-items", proj+"/work-items", viewer, `{"kind":"story","title":"no"}`, 403)
	do("POST", tp+"/runs", proj+"/runs", eng, `{}`, 400, "Idempotency-Key", "payload-bad")

	if checked < 30 {
		t.Fatalf("only %d responses were validated", checked)
	}

	t.Run("negative controls: a divergent shape fails the gate", func(t *testing.T) {
		_, _, good := d.call("GET", proj+"/runs/"+runID, eng, "")
		if err := schemas.check(tp+"/runs/{id}", "GET", 200, good); err != nil {
			t.Fatalf("the control body is not valid to begin with: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(good, &m); err != nil {
			t.Fatal(err)
		}
		mutate := map[string]func(map[string]any){
			"a missing required member": func(m map[string]any) { delete(m, "status") },
			"an undeclared member":      func(m map[string]any) { m["surprise"] = true },
			"a wrong type":              func(m map[string]any) { m["version"] = "one" },
			"an out-of-enum value":      func(m map[string]any) { m["status"] = "exploded" },
			"a null required member":    func(m map[string]any) { m["id"] = nil },
		}
		for name, f := range mutate {
			c := map[string]any{}
			for k, v := range m {
				c[k] = v
			}
			f(c)
			b, _ := json.Marshal(c)
			if err := schemas.check(tp+"/runs/{id}", "GET", 200, b); err == nil {
				t.Errorf("%s was accepted: %s", name, b)
			}
		}
		if err := schemas.check(tp+"/runs/{id}", "GET", 418, good); err == nil {
			t.Error("an undeclared status was accepted")
		}
		if err := schemas.check("/nowhere", "GET", 200, good); err == nil {
			t.Error("an undeclared operation was accepted")
		}
		if err := schemas.check(tp+"/runs/{id}", "GET", 404, []byte(`{"error":{"code":"not_found"}}`)); err == nil {
			t.Error("an incomplete error envelope was accepted")
		}
	})
}

// checkRuntimePayloads (a subtest of m1_contract_consistency) plays the entrypoint's calls against the composed runtime API for
// a real committed, dispatched attempt: admission, context, token renewal,
// artifact reservation and the final result. Each success payload is validated
// against the frozen runtime contract's schemas.
func checkRuntimePayloads(t *testing.T) {
	d := newDevSlice(t)
	ctx := context.Background()
	root := repoRoot(t)
	validate := compileFile(t, filepath.Join(root, "contracts", "runtime", "v1", "schema.json"), false)
	def := func(name string) string { return "schema.json#/$defs/" + name }
	var checked int
	expect := func(what, schema string, status int, body []byte, wantStatus int) {
		t.Helper()
		if status != wantStatus {
			t.Fatalf("%s: HTTP %d, want %d: %s", what, status, wantStatus, body)
		}
		if err := validate(def(schema), body); err != nil {
			t.Errorf("%s does not match runtime schema %s: %v\n%s", what, schema, err, body)
		}
		checked++
	}

	// A real run, dispatched to the Process runtime.
	w := d.seedWorld("m1-runtime", "echo ok")
	eng := d.api(devEngineerToken)
	r := d.createRun(eng, "m1-runtime-run", w, string(d.Repo.Initial))
	expectStatus(t, r, r.Body, http.StatusCreated, "create run")
	d.dispatchAll()
	agg, err := d.Exec.Get(ctx, devScope, domain.RunID(r.JSON201.Id))
	if err != nil || len(agg.Attempts) != 1 {
		t.Fatalf("attempts: %v %d", err, len(agg.Attempts))
	}
	at := agg.Attempts[0]
	var pinned runtimev1.Context
	if err := json.Unmarshal(at.Context, &pinned); err != nil {
		t.Fatalf("the pinned context: %v", err)
	}

	// The runtime authority registers the attempt (the dispatcher's step), and
	// the cluster shows the Job and Pod the entrypoint will claim.
	digest, _, err := runtimeauth.PinContext(pinned)
	if err != nil {
		t.Fatal(err)
	}
	job := runtimeauth.ExpectedJob{Namespace: rtest.Namespace, Name: at.WorkloadName}
	if err := d.Attempts.Seed(ctx, devScope, agg.Run.ID, at.ID, job, digest, rtest.AllActions()); err != nil {
		t.Fatal(err)
	}
	cred, err := runtimeauth.GenerateCredential(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Late.Register(ctx, runtimeauth.RegisterCommand{Scope: devScope, AttemptID: at.ID, Credential: cred, Actions: rtest.AllActions(), Context: pinned}); err != nil {
		t.Fatalf("register: %v", err)
	}
	id := runtimev1.JobIdentity{Name: at.WorkloadName, UID: "11111111-1111-4111-8111-111111111111", PodName: at.WorkloadName + "-x7k2p", PodUID: "22222222-2222-4222-8222-222222222222"}
	d.Jobs.SetCluster(runtimeauth.JobEvidence{Namespace: rtest.Namespace, JobName: id.Name, JobUID: id.UID, PodNamespace: rtest.Namespace, PodName: id.PodName, PodUID: id.PodUID, PodOwnerUID: id.UID})

	rt := "/runtime/v1/attempts/" + string(at.ID)
	admitReq := mustJSON(t, runtimev1.AdmitRequest{AdmissionCredential: cred.Reveal(), Nonce: "nonce-nonce-nonce-nonce", ClientVersion: entrypoint.Version, Job: id})
	status, _, body := d.call("POST", rt+"/admit", "", admitReq)
	expect("admit", "AdmitResponse", status, body, 200)
	var admitted runtimev1.AdmitResponse
	if err := json.Unmarshal(body, &admitted); err != nil || admitted.Token.Token == "" {
		t.Fatalf("admit body: %v %s", err, body)
	}
	token := admitted.Token.Token

	status, _, body = d.call("GET", rt+"/context", token, "")
	expect("context", "Context", status, body, 200)
	var served runtimev1.Context
	if err := json.Unmarshal(body, &served); err != nil || served.AttemptID != at.ID || served.RunID != agg.Run.ID || served.TenantID != devTenant {
		t.Fatalf("the served context is not the attempt's pinned context: %v %s", err, body)
	}
	status, _, body = d.call("POST", rt+"/token", token, `{}`)
	expect("renew", "TokenResponse", status, body, 200)

	// Artifact reservation: a diagnostic upload.
	sum := "sha256:" + strings.Repeat("a", 64)
	reserve := mustJSON(t, runtimev1.ArtifactReserveRequest{Name: "diag.txt", Kind: runtimev1.KindFile, Role: runtimev1.RoleDiagnostic, Digest: runtimev1.Digest(sum), MediaType: ptr("text/plain"), SizeBytes: ptr(int64(5))})
	status, _, body = d.call("POST", rt+"/artifacts", token, reserve, "Idempotency-Key", "diag-1")
	expect("reserve artifact", "ArtifactReserveResponse", status, body, 201)
	var reserved runtimev1.ArtifactReserveResponse
	if err := json.Unmarshal(body, &reserved); err != nil || reserved.ArtifactID == "" {
		t.Fatalf("reserve body: %v %s", err, body)
	}

	// The final result, under its canonical hash.
	res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
	hash, err := runtimev1.ResultHash(res)
	if err != nil {
		t.Fatal(err)
	}
	submit := mustJSON(t, runtimev1.ResultRequest{ResultHash: hash, Result: res})
	status, _, body = d.call("POST", rt+"/result", token, submit)
	expect("result", "ResultResponse", status, body, 200)
	var ack runtimev1.ResultResponse
	if err := json.Unmarshal(body, &ack); err != nil || ack.AttemptID != at.ID || ack.ResultHash != hash {
		t.Fatalf("acknowledgment: %v %s", err, body)
	}
	// The same result again is the original acknowledgment.
	status, _, again := d.call("POST", rt+"/result", token, submit)
	expect("result replay", "ResultResponse", status, again, 200)

	if checked < 6 {
		t.Fatalf("only %d runtime payloads were validated", checked)
	}

	t.Run("negative controls: a divergent shape fails the gate", func(t *testing.T) {
		for name, tc := range map[string]struct {
			schema string
			body   string
		}{
			"context without required members": {"Context", `{"tenantId":"t_acme"}`},
			"token response without a token":   {"TokenResponse", `{}`},
			"result ack of a wrong type":       {"ResultResponse", `{"attemptId":1,"resultHash":2,"attemptStatus":3,"acceptedAt":4}`},
			"an error without a code":          {"ErrorResponse", `{"error":{"message":"x"}}`},
		} {
			if err := validate(def(tc.schema), []byte(tc.body)); err == nil {
				t.Errorf("%s was accepted", name)
			}
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(strings.TrimSpace(string(mustBytes(t, admitted)))), &m)
		delete(m, "context")
		b, _ := json.Marshal(m)
		if err := validate(def("AdmitResponse"), b); err == nil {
			t.Error("an admit response without its context was accepted")
		}
	})
}

func mustBytes(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ptr[T any](v T) *T { return &v }
