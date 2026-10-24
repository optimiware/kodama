package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/api/client"
	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/app/apispec"
	cliauth "github.com/optimiware/kodama/internal/cli/auth"
	clientroot "github.com/optimiware/kodama/internal/cli/root"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/observation"
	"github.com/optimiware/kodama/internal/runtime/process"
)

// devAPI is the generated public client pointed at the development slice.
func (d *devSlice) api(token string) *client.ClientWithResponses {
	d.t.Helper()
	c, err := client.NewClientWithResponses(d.Server.URL+APIPrefix, client.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		return nil
	}))
	if err != nil {
		d.t.Fatal(err)
	}
	return c
}

func jsonReader(t testing.TB, v any) io.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}

type statused interface{ StatusCode() int }

func expectStatus(t testing.TB, r statused, body []byte, want int, what string) {
	t.Helper()
	if r.StatusCode() != want {
		t.Fatalf("%s: HTTP %d, want %d: %s", what, r.StatusCode(), want, body)
	}
}

// devWorld is a work item with a provisioned workspace and a registered
// one-step container pipeline: everything a run admission pins.
type devWorld struct {
	item client.WorkItem
	def  client.PipelineDefinition
	ws   client.Workspace
}

func (d *devSlice) seedWorld(name, command string) devWorld {
	d.t.Helper()
	return d.seedWorldAs(name, command, "read", nil)
}

// seedWorldAs is seedWorld with the workspace access of the step and optional
// acceptance criteria (a work item needs both to be ready).
func (d *devSlice) seedWorldAs(name, command, access string, criteria *[]client.AcceptanceCriterion) devWorld {
	d.t.Helper()
	ctx := context.Background()
	admin := d.api(devEngineerToken)
	spec := "Run the checks."
	wi, err := admin.CreateWorkItemWithResponse(ctx, string(devTenant), string(devProject), client.CreateWorkItem{Kind: "story", Title: name, Spec: &spec, AcceptanceCriteria: criteria})
	if err != nil {
		d.t.Fatal(err)
	}
	expectStatus(d.t, wi, wi.Body, http.StatusCreated, "create work item")
	doc := map[string]any{
		"apiVersion": "kodama/v1alpha1", "kind": "Pipeline", "metadata": map[string]any{"name": name},
		"spec": map[string]any{"inputs": map[string]any{}, "steps": []any{map[string]any{
			"id": "check", "kind": "container", "timeoutSeconds": 60,
			"image":     "registry.example.invalid/go-tools@sha256:1111111111111111111111111111111111111111111111111111111111111111",
			"command":   []string{"/bin/sh", "-ec"},
			"args":      []string{command},
			"workspace": map[string]any{"from": "run", "access": access},
		}}},
	}
	def, err := admin.RegisterPipelineDefinitionWithBodyWithResponse(ctx, string(devTenant), string(devProject), "application/json", jsonReader(d.t, doc))
	if err != nil {
		d.t.Fatal(err)
	}
	if def.JSON201 == nil {
		d.t.Fatalf("register definition: HTTP %d: %s", def.StatusCode(), def.Body)
	}
	ws, err := admin.ProvisionWorkspaceWithResponse(ctx, string(devTenant), string(devProject), wi.JSON201.Id, client.EmptyRequest{})
	if err != nil {
		d.t.Fatal(err)
	}
	if ws.JSON201 == nil && ws.JSON200 == nil {
		d.t.Fatalf("provision workspace: HTTP %d: %s", ws.StatusCode(), ws.Body)
	}
	w := ws.JSON201
	if w == nil {
		w = ws.JSON200
	}
	return devWorld{item: *wi.JSON201, def: *def.JSON201, ws: *w}
}

func (d *devSlice) createRun(api *client.ClientWithResponses, key string, w devWorld, commit string) *client.CreateRunResponse {
	d.t.Helper()
	r, err := api.CreateRunWithResponse(context.Background(), string(devTenant), string(devProject), &client.CreateRunParams{IdempotencyKey: key},
		client.CreateRunRequest{WorkItemId: w.item.Id, DefinitionId: w.def.Id, ContentRevision: int64(w.item.ContentRevision), SourceCommit: commit})
	if err != nil {
		d.t.Fatal(err)
	}
	return r
}

// errorCode extracts the stable code of an error envelope.
func errorCode(body []byte) string {
	var e client.ErrorEnvelope
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	return e.Error.Code
}

// TestM1GeneratedClient (m1_generated_client): the public client, built from
// the assembled OpenAPI document, drives the development composition: run
// submission and status, the stable error envelope, an idempotent replay, a
// conflicting key and body, and reproducible generation.
func TestM1GeneratedClient(t *testing.T) {
	d := newDevSlice(t)
	ctx := context.Background()
	tenant, project := string(devTenant), string(devProject)
	eng, viewer := d.api(devEngineerToken), d.api(devViewerToken)
	w := d.seedWorld("m1-client", "echo ok")
	commit := string(d.Repo.Initial)

	var run client.Run
	t.Run("run submission and status", func(t *testing.T) {
		r := d.createRun(eng, "m1-key-1", w, commit)
		expectStatus(t, r, r.Body, http.StatusCreated, "create run")
		run = *r.JSON201
		if r.HTTPResponse.Header.Get("Idempotent-Replayed") != "" {
			t.Fatal("a first admission is not a replay")
		}
		if run.Status != "queued" || run.WorkItemId == nil || *run.WorkItemId != w.item.Id || run.SourceCommit == nil || *run.SourceCommit != commit || run.RequestedBy.Id != devEngineer {
			t.Fatalf("created run %+v", run)
		}
		if loc := r.HTTPResponse.Header.Get("Location"); !strings.HasSuffix(loc, "/runs/"+run.Id) {
			t.Fatalf("Location %q", loc)
		}
		got, err := eng.GetRunWithResponse(ctx, tenant, project, run.Id)
		if err != nil || got.JSON200 == nil || got.JSON200.Id != run.Id || got.JSON200.Status != "queued" {
			t.Fatalf("status %v %+v", err, got)
		}
		steps, err := eng.ListRunStepsWithResponse(ctx, tenant, project, run.Id, &client.ListRunStepsParams{})
		if err != nil || steps.JSON200 == nil || len(steps.JSON200.Items) != 1 || steps.JSON200.Items[0].Id != "check" {
			t.Fatalf("steps %v %s", err, steps.Body)
		}
		list, err := eng.ListRunsWithResponse(ctx, tenant, project, &client.ListRunsParams{})
		if err != nil || list.JSON200 == nil || len(list.JSON200.Items) != 1 {
			t.Fatalf("runs %v %s", err, list.Body)
		}
	})

	t.Run("an idempotent replay returns the stored run and admits nothing", func(t *testing.T) {
		before, _ := d.Exec.List(ctx, devScope, listAll)
		r := d.createRun(eng, "m1-key-1", w, commit)
		expectStatus(t, r, r.Body, http.StatusCreated, "replay")
		if r.HTTPResponse.Header.Get("Idempotent-Replayed") != "true" || r.JSON201 == nil || r.JSON201.Id != run.Id {
			t.Fatalf("replay %s %s", r.HTTPResponse.Header, r.Body)
		}
		after, _ := d.Exec.List(ctx, devScope, listAll)
		if len(before) != 1 || len(after) != 1 {
			t.Fatalf("the replay admitted another run: %d then %d", len(before), len(after))
		}
	})

	t.Run("a conflicting key and body is the stable idempotency conflict", func(t *testing.T) {
		other := d.seedWorld("m1-client-2", "echo other")
		r := d.createRun(eng, "m1-key-1", other, commit)
		if r.StatusCode() != http.StatusConflict || r.JSON409 == nil || r.JSON409.Error.Code != "idempotency_conflict" {
			t.Fatalf("HTTP %d: %s", r.StatusCode(), r.Body)
		}
		var env client.ErrorEnvelope
		if err := json.Unmarshal(r.Body, &env); err != nil || env.Error.RequestId == "" || env.Error.Message == "" || env.Error.Details == nil {
			t.Fatalf("the envelope is incomplete: %v %s", err, r.Body)
		}
		if runs, _ := d.Exec.List(ctx, devScope, listAll); len(runs) != 1 {
			t.Fatalf("a conflicting key admitted a run: %d", len(runs))
		}
	})

	t.Run("errors decode to stable codes", func(t *testing.T) {
		anon := d.api("")
		if r, _ := anon.GetRunWithResponse(ctx, tenant, project, run.Id); r.StatusCode() != http.StatusUnauthorized || errorCode(r.Body) != "invalid_credential" {
			t.Fatalf("anonymous: %d %s", r.StatusCode(), r.Body)
		}
		if r, _ := d.api("not-a-token").GetRunWithResponse(ctx, tenant, project, run.Id); r.StatusCode() != http.StatusUnauthorized {
			t.Fatalf("unknown token: %d %s", r.StatusCode(), r.Body)
		}
		if r, _ := eng.GetRunWithResponse(ctx, tenant, project, "run_unknown"); r.StatusCode() != http.StatusNotFound || errorCode(r.Body) != "not_found" {
			t.Fatalf("unknown run: %d %s", r.StatusCode(), r.Body)
		}
		if r := d.createRun(viewer, "m1-viewer", w, commit); r.StatusCode() != http.StatusForbidden || errorCode(r.Body) != "forbidden" {
			t.Fatalf("viewer: %d %s", r.StatusCode(), r.Body)
		}
		// A caller with no membership learns nothing about the project.
		if r, _ := d.api(devOtherToken).GetRunWithResponse(ctx, tenant, project, run.Id); r.StatusCode() != http.StatusNotFound || errorCode(r.Body) != "not_found" {
			t.Fatalf("stranger: %d %s", r.StatusCode(), r.Body)
		}
		if r := d.createRun(eng, "m1-bad", w, "not-a-commit"); r.StatusCode() >= 500 || r.StatusCode() < 400 || errorCode(r.Body) == "" {
			t.Fatalf("malformed source: %d %s", r.StatusCode(), r.Body)
		}
		bad, err := eng.CreateRunWithBodyWithResponse(ctx, tenant, project, &client.CreateRunParams{IdempotencyKey: "m1-empty"}, "application/json", strings.NewReader("{}"))
		if err != nil || bad.StatusCode() != http.StatusBadRequest || errorCode(bad.Body) != "invalid_request" {
			t.Fatalf("empty body: %v %d %s", err, bad.StatusCode(), bad.Body)
		}
		missing, err := eng.CreateRunWithBodyWithResponse(ctx, tenant, project, &client.CreateRunParams{}, "application/json", strings.NewReader("{}"))
		if err != nil || missing.StatusCode() != http.StatusBadRequest {
			t.Fatalf("missing key: %v %d %s", err, missing.StatusCode(), missing.Body)
		}
	})

	t.Run("a dispatched run is running, then cancelled through the client", func(t *testing.T) {
		d.dispatchAll()
		got, err := eng.GetRunWithResponse(ctx, tenant, project, run.Id)
		if err != nil || got.JSON200 == nil || got.JSON200.Status != "running" {
			t.Fatalf("after dispatch: %v %s", err, got.Body)
		}
		if got.JSON200.StartedAt == nil {
			t.Fatal("a running run has a start time")
		}
		c, err := eng.CancelRunWithResponse(ctx, tenant, project, run.Id)
		if err != nil || (c.StatusCode() != http.StatusAccepted && c.StatusCode() != http.StatusOK) {
			t.Fatalf("cancel: %v %d %s", err, c.StatusCode(), c.Body)
		}
		d.settle(run.Id, "cancelled")
	})

	t.Run("generation from the assembled document is reproducible", func(t *testing.T) {
		assertClientReproducible(t)
	})
}

// listAll lists every run of a project.
var listAll = execution.ListQuery{Limit: 100}

// dispatchAll runs the dispatch the worker would: every committed claim is
// started on the Process runtime, outside any transaction.
func (d *devSlice) dispatchAll() {
	d.t.Helper()
	ctx := context.Background()
	if _, err := d.Slice.Trusted.ReconcileTenant(ctx, devTenant); err != nil {
		d.t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		more, err := d.Slice.Trusted.DispatchNext(ctx, devTenant)
		if err != nil {
			d.t.Fatal(err)
		}
		if !more {
			return
		}
	}
}

// settle runs the repair pass until the run reaches the status.
func (d *devSlice) settle(id, want string) {
	d.t.Helper()
	ctx := context.Background()
	api := d.api(devEngineerToken)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := d.Slice.Trusted.ReconcileTenant(ctx, devTenant); err != nil {
			d.t.Fatal(err)
		}
		g, err := api.GetRunWithResponse(ctx, string(devTenant), string(devProject), id)
		if err == nil && g.JSON200 != nil && string(g.JSON200.Status) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	d.t.Fatalf("run %s did not reach %s", id, want)
}

// settleWriters runs the repair pass until the item's writer attempts are
// settled (terminated, removed and accounted for).
func (d *devSlice) settleWriters(item string) {
	d.t.Helper()
	ctx := context.Background()
	guard := NewMemoryWriterGuard(func() WriterRuns { return d.Exec })
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := d.Slice.Trusted.ReconcileTenant(ctx, devTenant); err != nil {
			d.t.Fatal(err)
		}
		if ok, err := guard.WriterSettled(ctx, devScope, domain.WorkItemID(item)); err != nil {
			d.t.Fatal(err)
		} else if ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	d.t.Fatalf("the writer attempts of %s did not settle", item)
}

// assertClientReproducible regenerates the client from the fragments with the
// pinned generator and the committed configuration in a scratch directory and
// requires byte equality with the committed files.
func assertClientReproducible(t *testing.T) {
	t.Helper()
	root := repoRoot(t)
	bundled, err := apispec.Bundle(filepath.Join(root, "api"), apispec.CoreFragments, apispec.CoreInfo)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile(filepath.Join(root, "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bundled, committed) {
		t.Fatal("api/openapi.yaml is stale: run make api-generate and commit the result")
	}
	scratch := t.TempDir()
	if err := os.WriteFile(filepath.Join(scratch, "openapi.yaml"), bundled, 0o600); err != nil {
		t.Fatal(err)
	}
	copyTree(t, filepath.Join(root, "api", "client", "templates"), filepath.Join(scratch, "templates"))
	cfg, err := os.ReadFile(filepath.Join(root, "api", "client", "oapi-codegen.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "oapi-codegen.yaml"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	// The generator version is the one the //go:generate directive pins.
	gen, err := os.ReadFile(filepath.Join(root, "api", "client", "generate.go"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`oapi-codegen/v2/cmd/oapi-codegen@(v[0-9.]+) `).FindSubmatch(gen)
	if m == nil {
		t.Fatal("generate.go does not pin the generator version")
	}
	cmd := exec.Command("go", "run", "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@"+string(m[1]), "-config", "oapi-codegen.yaml", "openapi.yaml")
	cmd.Dir = scratch
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(scratch, "client.gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(root, "api", "client", "client.gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("api/client/client.gen.go differs from a fresh generation: run make api-generate and commit the result")
	}
}

func repoRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func copyTree(t testing.TB, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		if e.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ---- the OpenAPI document as the list of what must be served ----------------

type operation struct{ Method, Path string }

var methods = map[string]string{"get": "GET", "put": "PUT", "post": "POST", "delete": "DELETE", "options": "OPTIONS", "head": "HEAD", "patch": "PATCH"}

// loadOperations reads the operations of an OpenAPI document or fragment.
func loadOperations(t testing.TB, path string) []operation {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var ops []operation
	for p, item := range doc.Paths {
		for m := range item {
			if method, ok := methods[m]; ok {
				ops = append(ops, operation{Method: method, Path: p})
			}
		}
	}
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].Path != ops[j].Path {
			return ops[i].Path < ops[j].Path
		}
		return ops[i].Method < ops[j].Method
	})
	return ops
}

var pathParam = regexp.MustCompile(`\{([^{}]+)\}`)

// concrete fills a path template with valid identifiers.
func concrete(path string) string {
	return pathParam.ReplaceAllStringFunc(path, func(p string) string {
		switch p {
		case "{tenantId}":
			return string(devTenant)
		case "{projectId}":
			return string(devProject)
		case "{attemptId}":
			return "att_probe1"
		}
		return "res_probe1"
	})
}

// probe sends a body-less request and returns the status and the error code.
func (d *devSlice) probe(method, path, token string) (int, string) {
	d.t.Helper()
	req, err := http.NewRequest(method, d.Server.URL+path, nil)
	if err != nil {
		d.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, errorCode(body)
}

// outsidePrefix names the fragments' paths that are served (when they are)
// outside the public API prefix.
func servedPath(p string) string {
	for _, root := range []string{"/gateway/", "/internal/", "/git-hooks/", "/runtime/"} {
		if strings.HasPrefix(p, root) {
			return p
		}
	}
	return APIPrefix + p
}

// TestM1CompositionSmoke (m1_composition_smoke): the whole M1 slice composed
// under KODAMA_ENV=development with memory adapters, the Process runtime and no
// service. It verifies what the composition registers (routes, workers, event
// consumers, migrations, CLI groups) against what the contracts declare.
func TestM1CompositionSmoke(t *testing.T) {
	d := newDevSlice(t)
	root := repoRoot(t)
	ctx := context.Background()

	t.Run("development composes memory adapters and refuses production", func(t *testing.T) {
		rows, err := Selections(Development)
		if err != nil {
			t.Fatal(err)
		}
		for port, sel := range rows {
			if !sel.Restricted {
				t.Errorf("the development %s adapter %q is not flagged development-only", port, sel.Adapter)
			}
		}
		if d.Slice.Backend != process.Backend {
			t.Fatalf("runtime backend %q, want the Process runtime", d.Slice.Backend)
		}
		for name, adapter := range map[string]any{
			"events": d.Events, "identity": d.IDs, "work": d.Work, "secrets": d.Secrets, "workspaces": d.WS, "execution": d.Exec,
			"objects": d.Objects, "runtime": d.Runtime, "idempotency": d.Idem,
		} {
			if !IsDevelopmentAdapter(adapter) {
				t.Errorf("the %s adapter is not flagged development-only: production would accept it", name)
			}
			if _, err := Admit(Production, PortStore, adapter); !errors.Is(err, ErrDevelopmentAdapter) {
				t.Errorf("production accepted the %s adapter: %v", name, err)
			}
		}
		// The identical wiring under production is refused at construction.
		if _, err := buildDevSlice(t, Production); !errors.Is(err, ErrDevelopmentAdapter) {
			t.Fatalf("production composed the development adapters: %v", err)
		}
		// NewCore never substitutes memory for a missing service.
		if _, err := NewCore(ctx, CoreConfig{Env: Development}, nil); err == nil {
			t.Fatal("NewCore composed development")
		}
		status, _ := d.probe(http.MethodGet, "/healthz", "")
		ready, _ := d.probe(http.MethodGet, "/readyz", "")
		if status != http.StatusOK || ready != http.StatusOK {
			t.Fatalf("healthz %d readyz %d", status, ready)
		}
	})

	published := loadOperations(t, filepath.Join(root, "api", "openapi.yaml"))
	t.Run("every published operation is registered behind authentication", func(t *testing.T) {
		if len(published) < 30 {
			t.Fatalf("only %d operations in the aggregate", len(published))
		}
		for _, op := range published {
			path := servedPath(concrete(op.Path))
			if status, code := d.probe(op.Method, path, ""); status != http.StatusUnauthorized || code != "invalid_credential" {
				t.Errorf("%s %s without a credential: %d %q (a route that is unregistered or open)", op.Method, op.Path, status, code)
			}
			if status, _ := d.probe(op.Method, path, "not-a-token"); status != http.StatusUnauthorized {
				t.Errorf("%s %s with an unknown token: %d", op.Method, op.Path, status)
			}
			// A verified caller with no binding learns nothing about a tenant route.
			if strings.Contains(op.Path, "{tenantId}") {
				if status, _ := d.probe(op.Method, path, devOtherToken); status != http.StatusNotFound && status != http.StatusForbidden {
					t.Errorf("%s %s for a caller without a binding: %d", op.Method, op.Path, status)
				}
			}
		}
	})

	t.Run("operations outside the subset are not served", func(t *testing.T) {
		have := map[operation]bool{}
		for _, op := range published {
			have[op] = true
		}
		files, err := filepath.Glob(filepath.Join(root, "api", "*.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		checked := 0
		for _, f := range files {
			name := strings.TrimSuffix(filepath.Base(f), ".yaml")
			if name == "openapi" || name == "common" || name == "runtime" {
				continue
			}
			// T-039 serves the workload and hook routes of the model gateway, the MCP
			// proxy and the native Git trigger (beside the public API): their
			// composition is verified by TestM2CompositionSmoke.
			if name == "gateway" || name == "mcp" || name == "git-webhooks" {
				continue
			}
			for _, op := range loadOperations(t, f) {
				if have[op] {
					continue
				}
				checked++
				if status, _ := d.probe(op.Method, servedPath(concrete(op.Path)), devAdminToken); status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
					t.Errorf("%s %s (%s.yaml) is outside the subset but answered %d", op.Method, op.Path, name, status)
				}
			}
		}
		if checked == 0 {
			t.Fatal("no unsupported operation was probed")
		}
		// What is not served is also recorded, so the claim is checkable.
		for _, disabled := range []string{"flow-summary-list", "exploratory-qa"} {
			if !slices.Contains(d.Routes.Disabled, disabled) {
				t.Errorf("%q is not served and not recorded as disabled: %v", disabled, d.Routes.Disabled)
			}
		}
		// T-075 serves the ChatOps destination routes and the dependency routes.
		if slices.Contains(d.Routes.Disabled, "chatops") {
			t.Errorf("chatops is served and must not be recorded as disabled: %v", d.Routes.Disabled)
		}
		// T-072 serves the human routes of profiles, gates, environments and
		// deployment records: they are no longer reported as disabled.
		for _, served := range []string{"gates", "agents", "delivery", "cleanup-runs", "git-webhooks", "model-gateway", "mcp-proxy"} {
			if slices.Contains(d.Routes.Disabled, served) {
				t.Errorf("%q is served and must not be recorded as disabled: %v", served, d.Routes.Disabled)
			}
		}
	})

	t.Run("the runtime API serves the frozen contract and nothing else", func(t *testing.T) {
		for _, op := range loadOperations(t, filepath.Join(root, "api", "runtime.yaml")) {
			status, code := d.probe(op.Method, concrete(strings.ReplaceAll(op.Path, "{artifactId}", "art_probe1")), "")
			resolve := strings.HasSuffix(op.Path, "/secrets/resolve")
			admit := strings.HasSuffix(op.Path, "/admit")
			switch {
			case admit:
				// Admission is the one route that is reached before a token exists:
				// it authenticates the one-time admission credential in its body,
				// so an empty request is refused as invalid, never served.
				if status != http.StatusBadRequest || code != "invalid_request" {
					t.Errorf("admit without an admission credential: %d %q", status, code)
				}
			case resolve && (status != http.StatusNotFound || code != "not_found"):
				t.Errorf("%s %s is not wired and must answer not_found, got %d %q", op.Method, op.Path, status, code)
			case !resolve && status != http.StatusUnauthorized:
				t.Errorf("%s %s without a runtime token: %d %q", op.Method, op.Path, status, code)
			}
		}
		// A user's token is not a runtime credential.
		if status, _ := d.probe(http.MethodGet, "/runtime/v1/attempts/att_probe1/context", devAdminToken); status != http.StatusUnauthorized {
			t.Fatalf("a user token reached the runtime API: %d", status)
		}
		if !slices.Contains(d.Slice.Disabled, "runtime-secret-resolution") {
			t.Fatalf("the unwired runtime route is not recorded: %v", d.Slice.Disabled)
		}
	})

	t.Run("deny by default: a role never exceeds its permissions", func(t *testing.T) {
		viewer := d.api(devViewerToken)
		spec := "x"
		r, err := viewer.CreateWorkItemWithResponse(ctx, string(devTenant), string(devProject), client.CreateWorkItem{Kind: "story", Title: "no", Spec: &spec})
		if err != nil || r.StatusCode() != http.StatusForbidden {
			t.Fatalf("viewer create: %v %d %s", err, r.StatusCode(), r.Body)
		}
		l, err := viewer.ListWorkItemsWithResponse(ctx, string(devTenant), string(devProject), &client.ListWorkItemsParams{})
		if err != nil || l.StatusCode() != http.StatusOK {
			t.Fatalf("viewer list: %v %d %s", err, l.StatusCode(), l.Body)
		}
		// Variables and secrets are write-only for values and need their own permissions.
		if s, err := viewer.ListSecretsWithResponse(ctx, string(devTenant), string(devProject), &client.ListSecretsParams{}); err != nil || s.StatusCode() == http.StatusOK {
			t.Fatalf("viewer secrets: %v %d %s", err, s.StatusCode(), s.Body)
		}
	})

	t.Run("workers are started and stopped deliberately", func(t *testing.T) {
		names := []string{}
		for _, w := range d.Slice.Workers {
			names = append(names, w.Name)
		}
		if len(names) == 0 || names[0] != "dispatch" {
			t.Fatalf("workers %v", names)
		}
		wg := newWorkerGroup(ctx, slog.New(slog.DiscardHandler))
		for _, w := range d.Slice.Workers {
			wg.Go(w.Name, w.Run)
		}
		time.Sleep(30 * time.Millisecond)
		if err := wg.Healthy(); err != nil {
			t.Fatal(err)
		}
		stop, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := wg.Stop(stop); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a work cancellation event reaches execution through its consumer", func(t *testing.T) {
		w := d.seedWorld("m1-cancel", "echo ok")
		eng := d.api(devEngineerToken)
		r := d.createRun(eng, "m1-cancel-1", w, string(d.Repo.Initial))
		expectStatus(t, r, r.Body, http.StatusCreated, "create run")
		got, _ := eng.GetWorkItemWithResponse(ctx, string(devTenant), string(devProject), w.item.Id)
		if got.JSON200 == nil {
			t.Fatalf("work item: %s", got.Body)
		}
		reason := "no longer needed"
		tr, err := eng.TransitionWorkItemWithResponse(ctx, string(devTenant), string(devProject), w.item.Id,
			&client.TransitionWorkItemParams{IfMatch: client.IfMatch(`"` + strconv.FormatInt(int64(got.JSON200.Version), 10) + `"`)}, client.TransitionRequest{To: "cancelled", Reason: &reason})
		if err != nil || tr.StatusCode() != http.StatusOK {
			t.Fatalf("cancel work: %v %d %s", err, tr.StatusCode(), tr.Body)
		}
		pub, err := events.NewPublisher(d.Tenants, d.Events, d.Broker, events.PublisherConfig{
			Lease: time.Minute, PublishTimeout: time.Second, BatchSize: 50, Interval: time.Second,
			Retry: d.Tuning.Events.Retry,
		})
		if err != nil {
			t.Fatal(err)
		}
		res, err := pub.RunOnce(ctx)
		if err != nil || res.Published == 0 {
			t.Fatalf("publish: %+v %v", res, err)
		}
		if n, err := d.Broker.Pump(ctx); err != nil || n == 0 {
			t.Fatalf("deliver: %d %v", n, err)
		}
		run, err := eng.GetRunWithResponse(ctx, string(devTenant), string(devProject), r.JSON201.Id)
		if err != nil || run.JSON200 == nil || run.JSON200.CancelRequestedAt == nil {
			t.Fatalf("the consumer did not record the cancellation intent: %v %s", err, run.Body)
		}
		d.settle(r.JSON201.Id, "cancelled")
		// A repeat delivery is absorbed (inbox), and no new run can be admitted.
		again := d.createRun(eng, "m1-cancel-2", w, string(d.Repo.Initial))
		if again.StatusCode() < 400 {
			t.Fatalf("a cancelled work item admitted a run: %d %s", again.StatusCode(), again.Body)
		}
	})

	t.Run("an active work item waits for its writer attempt to settle", checkWriterGuard)
	t.Run("keyed writes are all or nothing", checkKeyedWritesAreAllOrNothing)

	t.Run("migrations register in dependency order and create every readiness table", func(t *testing.T) {
		m, err := Manifest()
		if err != nil {
			t.Fatal(err)
		}
		var all strings.Builder
		for _, mig := range m.Migrations() {
			all.WriteString(mig.SQL)
		}
		for _, table := range sentinelTables {
			if !regexp.MustCompile(`(?i)CREATE TABLE (IF NOT EXISTS )?(public\.)?` + table + `\b`).MatchString(all.String()) {
				t.Errorf("no migration creates %q, which readiness requires", table)
			}
		}
		seen := map[string]bool{}
		for _, c := range Contributions() {
			if c.Module == "" || seen[c.Module] || len(c.Migrations) == 0 {
				t.Errorf("contribution %q is unnamed, repeated or empty", c.Module)
			}
			seen[c.Module] = true
		}
	})

	t.Run("the command line is a client of the same API", func(t *testing.T) {
		dir := t.TempDir()
		credDir := filepath.Join(dir, "credentials")
		if err := os.Mkdir(credDir, 0o700); err != nil {
			t.Fatal(err)
		}
		key := clientroot.Context{Factory: d.Server.URL}.CredentialKey()
		if err := cliauth.NewFileStore(credDir).Save(ctx, key, cliauth.Credential{
			AccessToken: devEngineerToken, TokenType: "Bearer", ObtainedAt: time.Now(), Expiry: time.Now().Add(10 * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
		env := map[string]string{"KODAMA_CONTEXT_FILE": filepath.Join(dir, "contexts.json"), "KODAMA_CREDENTIAL_DIR": credDir, "KODAMA_ALLOW_INSECURE_LOOPBACK": "true"}
		run := func(args ...string) (int, string, string) {
			var out, errb bytes.Buffer
			code := Main(ctx, append(args, "--factory", d.Server.URL, "--tenant", string(devTenant), "--project", string(devProject),
				"--credential-store", "file", "--non-interactive", "--output", "json"), func(k string) string { return env[k] }, &out, &errb)
			return code, out.String(), errb.String()
		}
		code, out, errs := run("work", "create", "--kind", "story", "--title", "From the CLI")
		if code != 0 {
			t.Fatalf("work create: %d %s %s", code, out, errs)
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(out), &created); err != nil || created.ID == "" {
			t.Fatalf("output %q: %v", out, err)
		}
		if code, out, errs = run("work", "list"); code != 0 || !strings.Contains(out, created.ID) {
			t.Fatalf("work list: %d %s %s", code, out, errs)
		}
		if code, out, errs = run("run", "list"); code != 0 {
			t.Fatalf("run list: %d %s %s", code, out, errs)
		}
		// Groups the slice does not serve are not offered.
		for _, args := range [][]string{{"gate", "list"}, {"agent", "list"}} {
			var o, e bytes.Buffer
			if code := Main(ctx, args, func(k string) string { return env[k] }, &o, &e); code == 0 {
				t.Errorf("%v succeeded", args)
			}
		}
		// The server command never degrades to a partial composition.
		var o, e bytes.Buffer
		if code := Main(ctx, []string{"server", "--assembly", "core"}, func(string) string { return "" }, &o, &e); code == 0 {
			t.Fatal("the core assembly started without KODAMA_ENV")
		}
		o.Reset()
		e.Reset()
		if code := Main(ctx, []string{"server", "--assembly", "core"}, func(k string) string {
			if k == EnvVariable {
				return "development"
			}
			return ""
		}, &o, &e); code == 0 {
			t.Fatal("the core assembly started under development from the executable's constructors")
		}
	})
}

// ---- contract consistency ----------------------------------------------------

// translateNullable rewrites OpenAPI 3.0 nullability into JSON Schema so a
// schema of the document can validate a value.
func translateNullable(n any) {
	switch v := n.(type) {
	case map[string]any:
		if nb, _ := v["nullable"].(bool); nb {
			if ty, ok := v["type"].(string); ok {
				v["type"] = []any{ty, "null"}
			} else if all, ok := v["allOf"].([]any); ok {
				delete(v, "allOf")
				v["anyOf"] = []any{map[string]any{"allOf": all}, map[string]any{"type": "null"}}
			}
		}
		delete(v, "nullable")
		for _, c := range v {
			translateNullable(c)
		}
	case []any:
		for _, c := range v {
			translateNullable(c)
		}
	}
}

// pointer resolves a JSON pointer ("#/components/examples/X/value") in doc.
func pointer(doc any, ref string) (any, bool) {
	cur := doc
	for _, tok := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		tok = strings.NewReplacer("~1", "/", "~0", "~").Replace(tok)
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[tok]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func pointerToken(s string) string { return strings.NewReplacer("~", "~0", "/", "~1").Replace(s) }

// exampleSite is a media or parameter object that carries a schema and at least
// one example, with the JSON pointer of the object.
type exampleSite struct {
	ptr      string
	examples map[string]any
}

func collectExamples(doc any, ptr string, out *[]exampleSite) {
	switch v := doc.(type) {
	case map[string]any:
		if _, has := v["schema"]; has {
			site := exampleSite{ptr: ptr, examples: map[string]any{}}
			if ex, ok := v["example"]; ok {
				site.examples["example"] = ex
			}
			if exs, ok := v["examples"].(map[string]any); ok {
				for name, e := range exs {
					site.examples["examples/"+name] = e
				}
			}
			if len(site.examples) > 0 {
				*out = append(*out, site)
			}
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			collectExamples(v[k], ptr+"/"+pointerToken(k), out)
		}
	case []any:
		for i, c := range v {
			collectExamples(c, ptr+"/"+strconv.Itoa(i), out)
		}
	}
}

// TestM1ContractConsistency (m1_contract_consistency): the aggregate document,
// the runtime contract and the events the composed modules emit agree: every
// example validates against its schema, the runtime operations and error
// schemas are the frozen contract's and are the ones served, and the public and
// runtime interfaces do not leak into each other.
func TestM1ContractConsistency(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	t.Run("the aggregate declares its fragments and secures every operation", func(t *testing.T) {
		var listed []string
		for _, f := range doc["x-kodama-fragments"].([]any) {
			listed = append(listed, f.(string))
		}
		if !slices.Equal(listed, apispec.CoreFragments) {
			t.Fatalf("fragments %v, want %v", listed, apispec.CoreFragments)
		}
		ids := map[string]string{}
		for p, item := range doc["paths"].(map[string]any) {
			if strings.Contains(p, "runtime") {
				t.Errorf("the public document contains the runtime-only route %s", p)
			}
			for m, o := range item.(map[string]any) {
				op, ok := o.(map[string]any)
				if !ok || methods[m] == "" {
					continue
				}
				id, _ := op["operationId"].(string)
				if id == "" || ids[id] != "" {
					t.Errorf("%s %s: operation ID %q is empty or repeated (also %s)", m, p, id, ids[id])
				}
				ids[id] = m + " " + p
				if sec, set := op["security"]; set {
					if l, _ := sec.([]any); len(l) == 0 {
						t.Errorf("%s %s opts out of authentication", m, p)
					}
				}
			}
		}
		if sec, _ := doc["security"].([]any); len(sec) == 0 {
			t.Error("the document has no default security requirement")
		}
	})

	t.Run("every example validates against its schema", func(t *testing.T) {
		translated := map[string]any{}
		if err := yaml.Unmarshal(raw, &translated); err != nil {
			t.Fatal(err)
		}
		translateNullable(translated)
		js, err := json.Marshal(translated)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
		if err != nil {
			t.Fatal(err)
		}
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource("openapi.json", parsed); err != nil {
			t.Fatal(err)
		}
		var sites []exampleSite
		collectExamples(translated, "#", &sites)
		if len(sites) < 8 {
			t.Fatalf("only %d example sites found: the walk is broken", len(sites))
		}
		checked := 0
		for _, site := range sites {
			schema, err := compiler.Compile("openapi.json" + strings.NewReplacer("{", "%7B", "}", "%7D").Replace(site.ptr) + "/schema")
			if err != nil {
				t.Errorf("%s: schema: %v", site.ptr, err)
				continue
			}
			for name, ex := range site.examples {
				// An example is a value, or a reference (or an object holding a
				// value or a reference) into components.examples.
				value := ex
				if m, ok := ex.(map[string]any); ok {
					if ref, ok := m["$ref"].(string); ok {
						resolved, ok := pointer(translated, ref)
						if !ok {
							t.Errorf("%s %s: dangling example reference %s", site.ptr, name, ref)
							continue
						}
						value = resolved
						if inner, ok := resolved.(map[string]any); ok && !strings.HasSuffix(ref, "/value") {
							if v, ok := inner["value"]; ok {
								value = v
							}
						}
					} else if v, ok := m["value"]; ok && strings.HasPrefix(name, "examples/") {
						value = v
					}
				}
				b, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
				if err != nil {
					t.Fatal(err)
				}
				if err := schema.Validate(inst); err != nil {
					t.Errorf("%s %s does not validate: %v\n%s", site.ptr, name, err, b)
				}
				checked++
			}
		}
		t.Logf("validated %d examples at %d sites", checked, len(sites))
		if checked < 15 {
			t.Fatalf("only %d examples were validated", checked)
		}
	})

	t.Run("the runtime operations are the frozen contract's, under its base path", func(t *testing.T) {
		if got, want := runtimePrefix+"/attempts/{attemptId}", runtimev1.BasePath; got != want {
			t.Fatalf("the served runtime prefix is %q, the contract's is %q", got, want)
		}
		served := map[operation]bool{}
		for _, op := range loadOperations(t, filepath.Join(root, "api", "runtime.yaml")) {
			served[op] = true
		}
		if len(served) != len(runtimev1.Operations) {
			t.Fatalf("runtime.yaml has %d operations, the contract %d", len(served), len(runtimev1.Operations))
		}
		for _, op := range runtimev1.Operations {
			if !served[operation{Method: op.Method, Path: runtimev1.BasePath + op.Path}] {
				t.Errorf("%s %s is in the contract but not in api/runtime.yaml", op.Method, op.Path)
			}
		}
	})

	t.Run("served error envelopes validate against the contracts' error schemas", func(t *testing.T) {
		d := newDevSlice(t)
		public := compileFile(t, filepath.Join(root, "api", "openapi.yaml"), true)
		runtime := compileFile(t, filepath.Join(root, "contracts", "runtime", "v1", "schema.json"), false)

		body := func(method, path, token string) []byte {
			req, _ := http.NewRequest(method, d.Server.URL+path, nil)
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return b
		}
		publicErrors := map[string][]byte{
			"unauthenticated": body(http.MethodGet, APIPrefix+"/tenants/t_acme/projects/p_inv/runs", ""),
			"not found":       body(http.MethodGet, APIPrefix+"/tenants/t_acme/projects/p_inv/runs/run_unknown", devEngineerToken),
			"unrouted":        body(http.MethodGet, APIPrefix+"/nowhere", devEngineerToken),
			"wrong method":    body(http.MethodPut, APIPrefix+"/tenants/t_acme/projects/p_inv/runs", devEngineerToken),
			"no membership":   body(http.MethodGet, APIPrefix+"/tenants/t_acme/projects/p_inv/runs", devOtherToken),
		}
		for name, b := range publicErrors {
			if err := public("#/components/schemas/ErrorEnvelope", b); err != nil {
				t.Errorf("public error (%s) does not validate: %v\n%s", name, err, b)
			}
		}
		runtimeErrors := map[string][]byte{
			"unwired route": body(http.MethodPost, runtimev1.BasePath[:len("/runtime/v1/attempts/")]+"att_probe1/secrets/resolve", ""),
			"unrouted":      body(http.MethodGet, "/runtime/v1/nowhere", ""),
			"no token":      body(http.MethodGet, "/runtime/v1/attempts/att_probe1/context", ""),
		}
		for name, b := range runtimeErrors {
			if err := runtime("schema.json#/$defs/ErrorResponse", b); err != nil {
				t.Errorf("runtime error (%s) does not validate: %v\n%s", name, err, b)
			}
		}
	})

	t.Run("served public payloads validate against the published response schemas", checkPublicPayloads)
	t.Run("served runtime payloads validate against the frozen runtime contract", checkRuntimePayloads)

	t.Run("the events the composed modules emit are catalog events", func(t *testing.T) {
		d := newDevSlice(t)
		ctx := context.Background()
		eng := d.api(devEngineerToken)
		w := d.seedWorld("m1-events", "echo ok")
		r := d.createRun(eng, "m1-events-1", w, string(d.Repo.Initial))
		expectStatus(t, r, r.Body, http.StatusCreated, "create run")
		d.dispatchAll()
		if c, _ := eng.CancelRunWithResponse(ctx, string(devTenant), string(devProject), r.JSON201.Id); c.StatusCode() >= 300 {
			t.Fatalf("cancel: %d %s", c.StatusCode(), c.Body)
		}
		d.settle(r.JSON201.Id, "cancelled")
		recs, wm, err := devObservation{d: d}.Read(ctx, observation.Scope{Tenant: devTenant, Project: devProject}, 0, 1000)
		if err != nil || len(recs) == 0 || wm.HighWater != recs[len(recs)-1].Sequence {
			t.Fatalf("observations: %d %+v %v", len(recs), wm, err)
		}
		types := map[string]bool{}
		for _, rec := range recs {
			enc, err := eventsv1.Encode(rec.Envelope)
			if err != nil {
				t.Fatalf("sequence %d: %v", rec.Sequence, err)
			}
			dec, err := eventsv1.Decode(enc)
			if err != nil || dec.Validate() != nil {
				t.Fatalf("sequence %d (%s) is not a valid catalog event: %v %v", rec.Sequence, rec.Envelope.Type, err, dec.Validate())
			}
			if rec.Envelope.TenantID != devTenant || rec.Envelope.ProjectID != devProject {
				t.Fatalf("event %s carries another ownership: %s/%s", rec.Envelope.ID, rec.Envelope.TenantID, rec.Envelope.ProjectID)
			}
			types[rec.Envelope.Type] = true
		}
		for _, want := range []string{eventsv1.TypeWorkItemCreated, eventsv1.TypeRunCreated} {
			if !types[want] {
				t.Errorf("no %s event was emitted: %v", want, types)
			}
		}
	})
}

// compileFile returns a validator over the schemas of a JSON or YAML document.
// An OpenAPI document gets its 3.0 nullability rewritten first.
func compileFile(t testing.TB, path string, openapi bool) func(ref string, doc []byte) error {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if openapi {
		translateNullable(doc)
	}
	js, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
	if err != nil {
		t.Fatal(err)
	}
	name := "schema.json"
	if openapi {
		name = "openapi.json"
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(name, parsed); err != nil {
		t.Fatal(err)
	}
	return func(ref string, body []byte) error {
		if !openapi {
			ref = name + ref[strings.Index(ref, "#"):]
		} else {
			ref = name + ref
		}
		sch, err := c.Compile(ref)
		if err != nil {
			return err
		}
		inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
		if err != nil {
			return err
		}
		return sch.Validate(inst)
	}
}
