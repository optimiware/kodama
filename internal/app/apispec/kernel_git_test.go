package apispec

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/kernel/api"
	"github.com/optimiware/kodama/internal/kernel/serve"
	"github.com/optimiware/kodama/internal/kernel/store"
)

type bearerAs map[string]api.Principal

func (m bearerAs) Authenticate(_ context.Context, r *http.Request) (api.Principal, error) {
	if p, ok := m[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]; ok {
		return p, nil
	}
	return api.Principal{}, api.ErrUnauthenticated
}

// kernelGitOperations reads the operations of the kernel-git fragment: method,
// path template and operation ID.
func kernelGitOperations(t *testing.T) (ops []struct{ Method, Path, ID string }) {
	t.Helper()
	data, err := os.ReadFile(apiDir + "/kernel-git.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	for path, item := range doc.Paths {
		for method, op := range item {
			if !methods[method] {
				continue
			}
			id, _ := op.(map[string]any)["operationId"].(string)
			ops = append(ops, struct{ Method, Path, ID string }{strings.ToUpper(method), path, id})
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].Path+ops[i].Method < ops[j].Path+ops[j].Method })
	return ops
}

// Test_unit_kernel_git_api_document_current: the published document is the
// aggregate of the fragments and lists every route the kernel serves for the
// repository resource and the credential exchange, each of which the real
// handler answers (none is "unknown route"); the Git smart-HTTP transport is
// described in prose and is not an operation; the richer unpublished drafts
// are not part of it; and the committed baseline is still honoured, with the
// `source` member of the runtime deployment registration required again.
func Test_unit_kernel_git_api_document_current(t *testing.T) {
	// The fragment is registered, and the committed aggregate is exactly its assembly.
	registered := false
	for _, f := range CoreFragments {
		if f == "kernel-git" {
			registered = true
		}
		if f == "repositories" || f == "git-credentials" || f == "git-pushes" || f == "git-triggers" || f == "git-webhooks" {
			t.Errorf("the unpublished draft %s is listed in the core fragments", f)
		}
	}
	if !registered {
		t.Fatal("kernel-git is not registered in CoreFragments")
	}
	want, err := Bundle(apiDir, CoreFragments, CoreInfo)
	if err != nil {
		t.Fatal(err)
	}
	have, err := os.ReadFile(apiDir + "/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, have) {
		t.Fatal("api/openapi.yaml is stale: run `make api-generate` and commit the result")
	}

	var doc struct {
		Info struct {
			Description string `yaml:"description"`
		} `yaml:"info"`
		Paths      map[string]map[string]any `yaml:"paths"`
		Components map[string]map[string]any `yaml:"components"`
	}
	if err := yaml.Unmarshal(have, &doc); err != nil {
		t.Fatal(err)
	}

	// The fragment describes the kernel's own wire format: its errors and its refs
	// cursor are not the common ones.
	fragment, err := os.ReadFile(apiDir + "/kernel-git.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, common := range []string{"common.yaml#/components/schemas/ErrorEnvelope", "common.yaml#/components/responses/", "common.yaml#/components/parameters/Cursor", "common.yaml#/components/schemas/Cursor"} {
		if strings.Contains(string(fragment), common) {
			t.Errorf("kernel-git.yaml references %s, which does not describe what the kernel writes", common)
		}
	}
	if !strings.Contains(string(fragment), "GitRefsCursorValue") {
		t.Error("the refs listing has no Git cursor")
	}

	ops := kernelGitOperations(t)
	if len(ops) != 15 {
		t.Fatalf("the kernel-git fragment has %d operations, want 15 (repository create/list/get/patch/archive/restore/refs at tenant and project level, and the credential exchange)", len(ops))
	}
	for _, op := range ops {
		item, ok := doc.Paths[op.Path]
		if !ok {
			t.Errorf("%s %s (%s) is not in the published document", op.Method, op.Path, op.ID)
			continue
		}
		got, _ := item[strings.ToLower(op.Method)].(map[string]any)
		if got["operationId"] != op.ID {
			t.Errorf("%s %s is %v in the document, want %s", op.Method, op.Path, got["operationId"], op.ID)
		}
	}

	// The Git transport is prose, never an operation, and the worker's control
	// routes and the hook endpoints are not public.
	for path := range doc.Paths {
		if strings.HasPrefix(path, "/git/") || strings.Contains(path, "/internal/git") || strings.HasPrefix(path, "/git-hooks") {
			t.Errorf("%s is published as an operation", path)
		}
	}
	for _, phrase := range []string{"/git/<tenant>/<project>/<repository>.git", "non-JSON", "HTTP Basic"} {
		if !strings.Contains(doc.Info.Description, phrase) {
			t.Errorf("the document does not describe the Git transport (%q)", phrase)
		}
	}

	// Every documented route is served: the real handler over a memory store
	// answers each with something other than the router's "unknown route" or 405.
	mem := store.NewMemoryGit()
	mem.AddTenant("t_acme", "acme")
	mem.AddProject("t_acme", "p_erp", "erp")
	mem.Grant("t_acme", "u_eng", "p_erp", "engineer")
	mem.Grant("t_acme", "u_eng", "", "engineer")
	signer, err := serve.NewGitSigner("development", "", "")
	if err != nil {
		t.Fatal(err)
	}
	workers, err := serve.NewGitWorkers(serve.GitWorkersConfig{Env: "development", StateDir: t.TempDir(), Verifier: signer.Verifier()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workers.Close() })
	git, err := api.NewGit(api.GitConfig{Store: mem, Auth: bearerAs{"tok": {UserID: "u_eng"}}, Workers: workers, Signer: signer, Identities: signer.Verifier()})
	if err != nil {
		t.Fatal(err)
	}
	workers.BindControl(git.Control())
	handler := git.Handler()
	for _, op := range ops {
		path := strings.NewReplacer("{tenantId}", "acme", "{projectId}", "p_erp", "{id}", "repo_missing").Replace(op.Path)
		var body *bytes.Reader
		if op.Method == http.MethodPost || op.Method == http.MethodPatch {
			body = bytes.NewReader([]byte(`{}`))
		} else {
			body = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(op.Method, "/api/v1"+path, body)
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var answer struct {
			Error struct{ Code, Message string }
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &answer)
		// An error is exactly {"error":{"code","message"}}: the document's own envelope,
		// not the common one that also requires a request identifier and details.
		if rec.Code >= 400 {
			var shape map[string]map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &shape); err != nil || len(shape) != 1 || len(shape["error"]) != 2 ||
				shape["error"]["code"] == nil || shape["error"]["message"] == nil {
				t.Errorf("%s %s answered %d with a body that is not the documented error envelope: %s", op.Method, path, rec.Code, rec.Body.String())
			}
		}
		if rec.Code == http.StatusMethodNotAllowed || answer.Error.Message == "unknown route" || rec.Code >= 500 {
			t.Errorf("%s %s (%s) is documented but not served: %d %s", op.Method, path, op.ID, rec.Code, rec.Body.String())
		}
	}
	// An undocumented route stays unserved.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tenants/acme/repositories/repo_missing/pushes", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Errorf("an undocumented route is served: %d", rec.Code)
	}

	// Baseline: the guard stays unweakened, and the registration keeps `source`.
	raw, err := os.ReadFile("../testdata/api-baseline-t072.json")
	if err != nil {
		t.Fatal(err)
	}
	base, err := ParseSummary(raw)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := Summarize(have)
	if err != nil {
		t.Fatal(err)
	}
	if problems := cur.CompatibleWith(base, have); len(problems) != 0 {
		t.Fatalf("the document breaks the committed baseline:\n%s", strings.Join(problems, "\n"))
	}
	reg := cur.Schemas["RuntimeDeploymentRegister"]
	if !contains(reg.Required, "source") || !contains(reg.Required, "image") {
		t.Errorf("RuntimeDeploymentRegister requires %v, want source and image", reg.Required)
	}
	if _, ok := reg.Properties["sourceSet"]; !ok {
		t.Error("RuntimeDeploymentRegister lost the additive sourceSet member")
	}
}
