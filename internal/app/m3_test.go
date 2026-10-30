package app

// Acceptance of the M3 composition (T-046): m3_composition_smoke and
// m3_contract_compatibility. Everything runs in the `development` environment
// over the modules' memory fixtures and controlled adapters, with no container
// and no service (`make test-m3`). It verifies AC-10/AC-12 at the composition:
// that the business-loop contributions are registered through the central
// owner, that every operation they expose is decided by the identity module's
// authority and not by a fixture, that a forwarded review link follows current
// authority, and that the public contract still serves the M1 and M2 clients.
//
// What it does not claim. The real joins (actual transactions, outbox →
// JetStream → inbox → delivery, replay after an outage, the real gate as a link
// target in PostgreSQL) are T-044's, including chatops_gate_integration. Browser-
// agent QA and its image/profile (T-069), the Supabase blueprint (T-047) and live
// wrapper qualification (T-085) are deferred, and a mapping fixture is not
// exploratory QA. The flow-summary list contribution needs a hook in the work
// list handler (internal/work/http), which this task does not own; it stays
// recorded as not wired and the test below proves a request for it is refused,
// not silently ignored.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/app/apispec"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gates"
	gatesadapters "github.com/optimiware/kodama/internal/gates/adapters"
	gatesmemory "github.com/optimiware/kodama/internal/gates/gatestest/memory"
	gateshttp "github.com/optimiware/kodama/internal/gates/http"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	identityhttp "github.com/optimiware/kodama/internal/identity/http"
	"github.com/optimiware/kodama/internal/identity/idtest"
	"github.com/optimiware/kodama/internal/integrations/chatops"
	chatopshttp "github.com/optimiware/kodama/internal/integrations/chatops/http"
	"github.com/optimiware/kodama/internal/pipeline"
	dependencieshttp "github.com/optimiware/kodama/internal/work/dependencies/http"
)

const m3Prefix = "/tenants/{tenantId}/projects/{projectId}"

// m3Operations maps each business-loop operation of the aggregate ("METHOD
// /path") to the authorization action its module registers, from the modules'
// own route inventories. An operation the aggregate publishes without an entry
// here has no action the composition can check.
func m3Operations() map[string]httpapi.Action {
	ops := map[string]httpapi.Action{}
	for _, r := range chatopshttp.Routes() {
		ops[r.Method+" "+r.Path] = r.Action
	}
	dep := m3Prefix + "/work-items/{id}/dependencies"
	ops["GET "+dep] = dependencieshttp.ActionList
	ops["POST "+dep] = dependencieshttp.ActionAdd
	ops["DELETE "+dep+"/{dependencyId}"] = dependencieshttp.ActionRemove
	return ops
}

// publishedBusinessLoop returns the business-loop operations of the committed
// aggregate.
func publishedBusinessLoop(t testing.TB) []operation {
	t.Helper()
	var out []operation
	for _, op := range loadOperations(t, filepath.Join(repoRoot(t), "api", "openapi.yaml")) {
		if isBusinessLoopOperation(op.Method + " " + op.Path) {
			out = append(out, op)
		}
	}
	return out
}

// m3Roles are the verified callers of the fixture with the role each holds in
// the project (the administrator through a tenant-wide binding). The stranger is
// a verified principal with no binding at all.
var m3Roles = []struct {
	name  string
	token string
	role  identity.Role
}{
	{"administrator", devAdminToken, identity.RoleAdmin},
	{"engineer", devEngineerToken, identity.RoleEngineer},
	{"viewer", devViewerToken, identity.RoleViewer},
}

// TestM3CompositionSmoke (m3_composition_smoke): the business-loop contributions
// of T-040, T-041, T-042, T-043's installation inputs and T-045 composed with the
// M1 and M2 slices in one process over controlled adapters.
func TestM3CompositionSmoke(t *testing.T) {
	d := newDevSlice(t)
	ctx := context.Background()
	tenant, project := string(devTenant), string(devProject)

	t.Run("the contributions are registered once, through the central owner", func(t *testing.T) {
		for _, group := range []string{
			"identity", "work", "pipeline-definitions", "runs", "workspaces", // M1
			"agent-profiles", "gates", "deployments", "model-settings", "mcp-servers", // M2
			"work-dependencies", "chatops", // M3
		} {
			if !slices.Contains(d.Routes.Registered, group) {
				t.Errorf("route group %q is not registered: %v", group, d.Routes.Registered)
			}
		}
		seen := map[string]bool{}
		for _, g := range d.Routes.Registered {
			if seen[g] {
				t.Errorf("route group %q is registered twice", g)
			}
			seen[g] = true
		}

		// Every worker and event consumer of the composition has one name, and the
		// broker durables are distinct: a collision would silently merge two
		// consumers' cursors.
		workers := map[string]string{}
		for slice, list := range map[string][]NamedWorker{
			"execution": d.Slice.Workers, "delivery": d.Delivery.Workers, "model-tools": d.ModelTools.Workers, "business-loop": d.BusinessLoop.Workers,
		} {
			for _, w := range list {
				if w.Name == "" || w.Run == nil {
					t.Errorf("%s worker %+v is incomplete", slice, w)
				}
				if prev, dup := workers[w.Name]; dup {
					t.Errorf("worker %q is started by %s and %s", w.Name, prev, slice)
				}
				workers[w.Name] = slice
			}
		}
		if workers["chatops-delivery"] != "business-loop" {
			t.Errorf("the notification worker is not contributed by the business loop: %v", workers)
		}
		durables := map[string]string{
			cancellationDurable: "work cancellation", gateDecisionDurable: "gate decision",
		}
		for _, c := range d.BusinessLoop.Consumers {
			if prev, dup := durables[c.Durable]; dup {
				t.Errorf("durable %q of %s collides with %s", c.Durable, c.Spec.Name, prev)
			}
			durables[c.Durable] = c.Spec.Name
		}
		if len(durables) != 3 {
			t.Errorf("durables: %v", durables)
		}

		// The schema is the squashed two-file baseline: one contribution that
		// already holds every business-loop table and grant.
		m, err := Manifest()
		if err != nil {
			t.Fatal(err)
		}
		var modules []string
		for _, c := range Contributions() {
			modules = append(modules, c.Module)
		}
		// The squashed baseline is the whole existing schema; the additive
		// run_shared contributions (T-112 resources, T-113 resource grants, T-116
		// artifact workspace provenance) are the only later ones.
		if !slices.Equal(modules, []string{"storage", "execution-run-resources", "runtimeauth-resource-grants", "artifacts-workspace-provenance", "state"}) {
			t.Errorf("contributions = %v", modules)
		}
		if len(m.Migrations()) == 0 {
			t.Error("the manifest is empty")
		}

		// What is deliberately not wired is the explicit list, no more and no less.
		want := []string{"chatops-authenticated-destinations", "chatops-restore-fence", "exploratory-qa", "flow-summary-list"}
		var got []string
		for _, name := range d.Routes.Disabled {
			if slices.Contains(want, name) {
				got = append(got, name)
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("recorded refusals of the business loop %v, want %v (all: %v)", got, want, d.Routes.Disabled)
		}
		for _, served := range []string{"chatops", "gates", "agents", "delivery", "model-gateway", "mcp-proxy", "git-webhooks"} {
			if slices.Contains(d.Routes.Disabled, served) {
				t.Errorf("%q is wired and must not be recorded as disabled: %v", served, d.Routes.Disabled)
			}
		}
		// No command group is contributed for them: the CLI never offers a command
		// whose route the server does not serve.
		for _, g := range cliGroups {
			if strings.Contains(g, "chatops") || strings.Contains(g, "depend") || g == "qa" || g == "flow" {
				t.Errorf("the CLI offers the group %q, which the assembly has no command contract for", g)
			}
		}
	})

	t.Run("the installation statement example loads, validates for production and selects what it says", func(t *testing.T) {
		cfg, err := LoadBusinessLoop(filepath.Join("testdata", "business-loop.example.json"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.QA == nil || cfg.ChatOps == nil {
			t.Fatalf("the example selects neither half: %+v", cfg)
		}
		if err := cfg.Validate(Production, DefaultTuning()); err != nil {
			t.Fatalf("the example does not validate under production: %v", err)
		}
		// The example names what the mapping reads: the same output and input names
		// the QA pipeline fixture below declares, and a mapping principal that is a
		// user principal like every fixture one.
		if cfg.QA.ReportOutput != m3QAOutput || cfg.QA.DeploymentInput != m3QAInput {
			t.Fatalf("the example names %q/%q, the QA pipeline fixture declares %q/%q", cfg.QA.ReportOutput, cfg.QA.DeploymentInput, m3QAOutput, m3QAInput)
		}
		if _, err := chatops.NewLinkBuilder(cfg.ChatOps.Origin); err != nil {
			t.Fatalf("the example origin: %v", err)
		}
	})

	t.Run("every operation the business loop exposes is decided by the identity authority", func(t *testing.T) {
		ops := m3Operations()
		published := publishedBusinessLoop(t)
		if len(published) != len(ops) {
			t.Fatalf("the aggregate publishes %d business-loop operations, the modules register %d", len(published), len(ops))
		}
		rules := coreRules()
		for _, op := range published {
			action, ok := ops[op.Method+" "+op.Path]
			if !ok {
				t.Errorf("%s %s has no registered authorization action", op.Method, op.Path)
				continue
			}
			rule, ok := rules[action]
			if !ok || len(rule.Any) == 0 {
				t.Errorf("%s %s (%s): the rule table has no rule, so the authorizer would deny it for everyone", op.Method, op.Path, action)
				continue
			}
			path := "/api/v1" + concrete(op.Path)
			if status, code := d.probe(op.Method, path, ""); status != http.StatusUnauthorized || code != "invalid_credential" {
				t.Errorf("%s %s anonymous: %d %q", op.Method, op.Path, status, code)
			}
			if status, _ := d.probe(op.Method, path, devOtherToken); status != http.StatusNotFound && status != http.StatusForbidden {
				t.Errorf("%s %s for a caller without a binding: %d", op.Method, op.Path, status)
			}
			// The fixed role matrix is the oracle: a bound role without any of the
			// rule's permissions is refused; one with a permission passes the gate
			// (whatever the request then finds is the use case's own answer).
			for _, r := range m3Roles {
				allowed := slices.ContainsFunc(rule.Any, r.role.Grants)
				status, _ := d.probe(op.Method, path, r.token)
				switch {
				case !allowed && status != http.StatusForbidden:
					t.Errorf("%s %s as %s (%s lacks %v): %d, want 403", op.Method, op.Path, r.name, r.role, rule.Any, status)
				case allowed && (status == http.StatusUnauthorized || status == http.StatusForbidden):
					t.Errorf("%s %s as %s (%s holds one of %v): %d", op.Method, op.Path, r.name, r.role, rule.Any, status)
				}
			}
		}
		// The rule of each action is the module's own contribution, not a local one.
		for action, want := range chatopshttp.Rules() {
			if got := rules[action]; !slices.Equal(got.Any, want.Any) || got.Level != want.Level {
				t.Errorf("action %s: %+v, want the module's %+v", action, got, want)
			}
		}
	})

	t.Run("the flow-summary list option is refused, never silently ignored", func(t *testing.T) {
		base := "/api/v1/tenants/" + tenant + "/projects/" + project + "/work-items"
		for _, q := range []string{"?includeFlowSummary=true", "?includeFlowSummary=true&flowFrom=2026-01-01T00:00:00Z&flowTo=2026-02-01T00:00:00Z", "?flowFrom=2026-01-01T00:00:00Z"} {
			status, code := d.probe(http.MethodGet, base+q, devViewerToken)
			if status != http.StatusBadRequest {
				t.Errorf("GET work-items%s: %d %q; a client asking for a summary the server does not produce must be told", q, status, code)
			}
		}
		if r, err := d.api(devViewerToken).ListWorkItemsWithResponse(ctx, tenant, project, &client.ListWorkItemsParams{}); err != nil || r.JSON200 == nil {
			t.Fatalf("the plain list: %v %d %s", err, r.StatusCode(), r.Body)
		} else if strings.Contains(string(r.Body), "flowSummary") {
			t.Fatalf("an unrequested summary member appeared: %s", r.Body)
		}
	})

	t.Run("the M1 and M2 flows run unchanged beside the business loop", func(t *testing.T) {
		eng := d.api(devEngineerToken)
		w := d.seedWorld("m3-flow", "echo ok")
		r := d.createRun(eng, "m3-key-1", w, string(d.Repo.Initial))
		expectStatus(t, r, r.Body, http.StatusCreated, "create run")
		again := d.createRun(eng, "m3-key-1", w, string(d.Repo.Initial))
		if again.HTTPResponse.Header.Get("Idempotent-Replayed") != "true" || again.JSON201 == nil || again.JSON201.Id != r.JSON201.Id {
			t.Fatalf("replay: %s %s", again.HTTPResponse.Header, again.Body)
		}
		// A dependency is checked at the same admission as ever: a prerequisite that is
		// not done blocks ready → active, and a run is still admitted for an item whose
		// links are satisfied.
		pre, dep := d.createItem("m3-pre"), d.createItem("m3-dep")
		for _, id := range []string{pre.Id, dep.Id} {
			if r := d.transition(eng, id, "ready"); r.StatusCode() != http.StatusOK {
				t.Fatalf("ready: %d %s", r.StatusCode(), r.Body)
			}
		}
		if r := d.addDependency(devEngineerToken, dep.Id, pre.Id); r.StatusCode() != http.StatusCreated && r.StatusCode() != http.StatusOK {
			t.Fatalf("add dependency: %d %s", r.StatusCode(), r.Body)
		}
		if blocked := d.transition(eng, dep.Id, "active"); blocked.StatusCode() != http.StatusUnprocessableEntity {
			t.Fatalf("a dependent was admitted before its prerequisite: %d %s", blocked.StatusCode(), blocked.Body)
		}
		// The M2 management surface answers beside it.
		if got, err := d.api(devViewerToken).GetModelSettingsWithResponse(ctx, tenant, project); err != nil || got.JSON200 == nil {
			t.Fatalf("model settings: %v %d %s", err, got.StatusCode(), got.Body)
		}
	})

	t.Run("a QA pipeline fixture is admitted with the names the mapping reads (profile registration only: browser QA is T-069)", func(t *testing.T) {
		admin := d.api(devAdminToken)
		if r, err := admin.ReplaceModelSettingsWithResponse(ctx, tenant, project, ifMatch(1), m2Settings(5)); err != nil || r.JSON200 == nil {
			t.Fatalf("model settings: %v %d %s", err, r.StatusCode(), r.Body)
		}
		prof, err := d.registerProfileVersion(m2Profile(t, "qa-explorer", agents.Model{Name: devModel, Endpoint: m2Endpoint}))
		if err != nil {
			t.Fatalf("the QA profile was refused: %v", err)
		}
		w := d.seedWorld("m3-qa", "echo ok")
		doc := map[string]any{
			"apiVersion": "kodama/v1alpha1", "kind": "Pipeline", "metadata": map[string]any{"name": "m3-qa-explore"},
			"spec": map[string]any{
				"inputs": map[string]any{m3QAInput: map[string]any{"type": "string", "required": true}},
				"steps": []any{map[string]any{
					"id": "explore", "kind": "agent", "agent": map[string]any{"profile": prof.Ref()}, "timeoutSeconds": 600,
					"workspace": map[string]any{"access": "none"},
					"outputs":   map[string]any{m3QAOutput: map[string]any{"type": "artifact", "path": pipeline.OutputsRoot + "/" + m3QAOutput + ".json"}},
				}},
			},
		}
		eng := d.api(devEngineerToken)
		reg, err := eng.RegisterPipelineDefinitionWithBodyWithResponse(ctx, tenant, project, "application/json", jsonReader(t, doc))
		if err != nil || reg.JSON201 == nil {
			t.Fatalf("the QA definition was refused: %v %d %s", err, reg.StatusCode(), reg.Body)
		}
		w.def = *reg.JSON201
		runReq := client.CreateRunRequest{
			WorkItemId: w.item.Id, DefinitionId: w.def.Id, ContentRevision: int64(w.item.ContentRevision), SourceCommit: string(d.Repo.Initial),
		}
		// The deployment under test is a pinned run input: a run that names none is
		// refused at admission, so a QA attempt cannot exist without it.
		missing, err := eng.CreateRunWithResponse(ctx, tenant, project, &client.CreateRunParams{IdempotencyKey: "m3-qa-none"}, runReq)
		if err != nil {
			t.Fatal(err)
		}
		if missing.StatusCode() == http.StatusCreated {
			t.Fatalf("a QA run without the deployment input was admitted: %s", missing.Body)
		}
		var dep client.RunsScalar
		if err := dep.FromRunsScalar0("dep_m3_probe"); err != nil {
			t.Fatal(err)
		}
		runReq.Inputs = &map[string]client.RunsScalar{m3QAInput: dep}
		ok, err := eng.CreateRunWithResponse(ctx, tenant, project, &client.CreateRunParams{IdempotencyKey: "m3-qa-run"}, runReq)
		if err != nil || ok.JSON201 == nil {
			t.Fatalf("the QA run was refused: %v %d %s", err, ok.StatusCode(), ok.Body)
		}
		agg, err := d.Exec.Get(ctx, devScope, domain.RunID(ok.JSON201.Id))
		if err != nil || len(agg.Steps) != 1 {
			t.Fatalf("the admitted QA run: %v", err)
		}
		if v, found := agg.Run.Snapshot.Values()[m3QAInput]; !found || !v.IsString() || v.String() != "dep_m3_probe" {
			t.Fatalf("the deployment is not pinned in the run's immutable snapshot: %+v", agg.Run.Snapshot.Inputs)
		}
		if out, found := agg.Steps[0].Spec.Outputs[m3QAOutput]; !found || out.Type != pipeline.TypeArtifact {
			t.Fatalf("the step does not declare %q as an artifact output: %+v", m3QAOutput, agg.Steps[0].Spec.Outputs)
		}
	})

	t.Run("the gate, run, work-item and deployment links of a notification name served, authenticated routes", func(t *testing.T) {
		links, err := chatops.NewLinkBuilder(devOrigin)
		if err != nil {
			t.Fatal(err)
		}
		item := d.createItem("m3-link")
		served := map[string]bool{}
		for _, op := range loadOperations(t, filepath.Join(repoRoot(t), "api", "openapi.yaml")) {
			if op.Method == http.MethodGet {
				served[op.Path] = true
			}
		}
		for _, kind := range []struct{ kind, template string }{
			{"work_item", m3Prefix + "/work-items/{id}"}, {"gate", m3Prefix + "/gates/{id}"},
			{"run", m3Prefix + "/runs/{id}"}, {"deployment", m3Prefix + "/deployments/{id}"},
		} {
			id := "res_probe1"
			if kind.kind == "work_item" {
				id = item.Id
			}
			link, err := links.URL(devScope, kind.kind, id)
			if err != nil {
				t.Fatalf("%s: %v", kind.kind, err)
			}
			if strings.ContainsAny(link, "?#@") {
				t.Errorf("the %s link carries more than the record's address: %s", kind.kind, link)
			}
			if !served[kind.template] {
				t.Errorf("the %s link targets %s, which the aggregate does not publish as a GET", kind.kind, kind.template)
			}
			path := strings.TrimPrefix(link, devOrigin)
			if status, code := d.probe(http.MethodGet, path, ""); status != http.StatusUnauthorized || code != "invalid_credential" {
				t.Errorf("the %s link anonymous: %d %q", kind.kind, status, code)
			}
			if status, _ := d.probe(http.MethodGet, path, devOtherToken); status == http.StatusOK {
				t.Errorf("the %s link opened for a caller without a binding", kind.kind)
			}
			// Following the link never changes anything, whatever the follower is.
			for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
				if status, _ := d.probe(method, path, devAdminToken); status < 400 {
					t.Errorf("%s on the %s link answered %d", method, kind.kind, status)
				}
			}
		}
		// The follower is judged now: a viewer reads the current work item through the
		// same URL that an anonymous follower is refused.
		path := strings.TrimPrefix(mustLink(t, links, devScope, "work_item", item.Id), devOrigin)
		if status, _ := d.probe(http.MethodGet, path, devViewerToken); status != http.StatusOK {
			t.Fatalf("the viewer following the work-item link: %d", status)
		}
	})

	t.Run("a forwarded gate link is judged by the identity authority against the record as it is now", func(t *testing.T) {
		w := newM3GateWorld(t)
		follow := func(method, token string) (int, []byte) {
			t.Helper()
			req, err := http.NewRequest(method, w.server.URL+w.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			return resp.StatusCode, b
		}
		status, body := follow(http.MethodGet, m3ReviewerToken)
		if status != http.StatusOK {
			t.Fatalf("the reviewer following the link: %d %s", status, body)
		}
		var rec struct{ ID, Status string }
		if err := json.Unmarshal(body, &rec); err != nil || rec.ID != string(w.gate.ID) || rec.Status != string(gates.StatusPending) {
			t.Fatalf("the link shows %s (%v), want the pending gate %s", body, err, w.gate.ID)
		}
		// Forwarding the URL grants nothing: the follower is authenticated and
		// authorized by the identity module, not by possession of the link.
		if status, _ := follow(http.MethodGet, ""); status != http.StatusUnauthorized {
			t.Errorf("an anonymous follower: %d", status)
		}
		if status, _ := follow(http.MethodGet, "not-a-token"); status != http.StatusUnauthorized {
			t.Errorf("a forged token: %d", status)
		}
		if status, _ := follow(http.MethodGet, m3StrangerToken); status != http.StatusNotFound && status != http.StatusForbidden {
			t.Errorf("a verified principal with no binding in the tenant: %d", status)
		}
		if status, _ := follow(http.MethodGet, m3ForeignToken); status != http.StatusNotFound && status != http.StatusForbidden {
			t.Errorf("a principal bound only in another tenant: %d", status)
		}
		// Opening the link decides nothing, for anyone, with any method.
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			if status, _ := follow(method, m3ReviewerToken); status < 400 {
				t.Errorf("%s on the link answered %d", method, status)
			}
		}
		if g, err := w.env.Store.ByStep(ctx, w.scope, w.run, w.step); err != nil || g.Version != w.gate.Version || g.Decision != nil {
			t.Fatalf("following the link changed the gate: %v %+v", err, g)
		}
		// The same URL follows current authority: revoking the reviewer closes it.
		w.revokeReviewer(t)
		if status, _ := follow(http.MethodGet, m3ReviewerToken); status == http.StatusOK {
			t.Fatal("the link still opened for a reviewer whose binding was revoked")
		}
	})

	t.Run("documentation names only targets and files that exist", func(t *testing.T) {
		root := repoRoot(t)
		readme := mustRead(t, filepath.Join(root, "docs", "implementation", "assembly-status.md"))
		makeDocs := readme + mustRead(t, filepath.Join(root, "README.md")) + mustRead(t, filepath.Join(root, "AGENTS.md"))
		makefile := mustRead(t, filepath.Join(root, "Makefile"))
		ci := mustRead(t, filepath.Join(root, ".github", "workflows", "ci.yml"))
		for _, m := range regexp.MustCompile("`make ([a-z][a-z0-9-]*)").FindAllStringSubmatch(makeDocs, -1) {
			if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(m[1]) + `:`).MatchString(makefile) {
				t.Errorf("the documentation names `make %s`, which the Makefile does not define", m[1])
			}
		}
		for _, target := range []string{"test-m1", "test-m2", "test-m3", "api-check"} {
			if !strings.Contains(ci, "make "+target) {
				t.Errorf("CI does not run make %s", target)
			}
		}
		line := regexp.MustCompile(`(?m)^test-m3:\n((?:\t.*\n)+)`).FindStringSubmatch(makefile)
		if line == nil || !strings.Contains(line[1], "TestM3") || !strings.Contains(line[1], "TestBusinessLoop") {
			t.Errorf("make test-m3 does not run the M3 acceptance and the business-loop wiring tests: %v", line)
		}
		for _, claim := range []string{
			"m3_composition_smoke", "m3_contract_compatibility", "flow-summary-list", "exploratory-qa", "chatops-authenticated-destinations",
			"chatops-restore-fence", "business-loop.example.json", "T-044",
		} {
			if !strings.Contains(readme, claim) {
				t.Errorf("the assembly status document does not say %q", claim)
			}
		}
		for _, name := range d.Routes.Disabled {
			if strings.HasPrefix(name, "chatops") || name == "exploratory-qa" || name == "flow-summary-list" {
				if !strings.Contains(readme, name) {
					t.Errorf("the not-wired capability %q is not documented", name)
				}
			}
		}
		if !strings.Contains(readme, "internal/app/testdata/business-loop.example.json") {
			t.Error("the assembly status document does not point at the installation statement example")
		}
	})
}

func mustRead(t testing.TB, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustLink(t testing.TB, b chatops.LinkBuilder, scope domain.Scope, kind, id string) string {
	t.Helper()
	l, err := b.URL(scope, kind, id)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// The names the QA fixture and the installation example agree on.
const (
	m3QAOutput = "qa-report"
	m3QAInput  = "deployment"
)

// ---- the gate link world ----------------------------------------------------------

const (
	m3ReviewerToken = "m3-token-reviewer"
	m3StrangerToken = "m3-token-stranger"
	m3ForeignToken  = "m3-token-foreign"
	m3AdminToken    = "m3-token-admin"
	m3Issuer        = "https://m3.issuer.example"
)

// m3GateWorld is a real gate use case over the gate module's memory store (a
// controlled adapter), served by the gate module's own HTTP handler behind the
// router and the authorizer the production composition builds: the identity
// module's standalone authority and the core rule table, over a seeded identity
// store. Only the gate record's persistence is a double.
type m3GateWorld struct {
	env    *gatesmemory.Env
	scope  domain.Scope
	run    domain.RunID
	step   string
	gate   *gates.Gate
	server *httptest.Server
	path   string

	ids      *idtest.Mem
	identity *identity.Service
	binding  identity.BindingID
}

func newM3GateWorld(t *testing.T) *m3GateWorld {
	t.Helper()
	ctx := context.Background()
	h := gatesmemory.NewHarness(t)
	h.E.SetTenants(gatesmemory.ScopeA.TenantID)
	c := h.NewCase(t, false)
	gate, _, err := h.E.Svc.RequestForStep(ctx, c.Scope, c.Run, c.Step)
	if err != nil {
		t.Fatalf("request the gate: %v", err)
	}
	w := &m3GateWorld{env: h.E, scope: c.Scope, run: c.Run, step: c.Step, gate: gate}

	ids := idtest.NewMem()
	w.ids = ids
	other := domain.Scope{TenantID: "t_m3_foreign", ProjectID: "p_m3_foreign"}
	for _, s := range []domain.Scope{c.Scope, other} {
		ids.AddTenant(s.TenantID, strings.TrimPrefix(string(s.TenantID), "t_"))
		ids.AddProject(s.TenantID, s.ProjectID, strings.TrimPrefix(string(s.ProjectID), "p_"))
	}
	users := map[identity.UserID]string{"u_m3_reviewer": "sub-reviewer", "u_m3_stranger": "sub-stranger", "u_m3_foreign": "sub-foreign", "u_m3_admin": "sub-admin"}
	for id, sub := range users {
		ids.AddUser(id, m3Issuer, sub)
	}
	w.binding = "mb_m3_reviewer"
	ids.AddBinding(c.Scope.TenantID, w.binding, "u_m3_reviewer", c.Scope.ProjectID, identity.RoleReviewer)
	ids.AddBinding(c.Scope.TenantID, "mb_m3_admin", "u_m3_admin", "", identity.RoleAdmin)
	ids.AddBinding(other.TenantID, "mb_m3_foreign", "u_m3_foreign", other.ProjectID, identity.RoleReviewer)

	svc, err := identity.NewService(identity.Config{Repository: ids, Directory: ids})
	if err != nil {
		t.Fatal(err)
	}
	w.identity = svc
	standalone, err := identity.NewStandaloneAuthority(ids)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := identity.SelectAuthority(identity.AuthorityConfig{Profile: identity.ProfileStandalone, Standalone: standalone})
	if err != nil {
		t.Fatal(err)
	}
	verifier := staticVerifier{
		m3ReviewerToken: {Issuer: m3Issuer, Subject: "sub-reviewer"}, m3StrangerToken: {Issuer: m3Issuer, Subject: "sub-stranger"},
		m3ForeignToken: {Issuer: m3Issuer, Subject: "sub-foreign"}, m3AdminToken: {Issuer: m3Issuer, Subject: "sub-admin"},
	}
	authn, err := identityhttp.NewAuthenticator(verifier, ids)
	if err != nil {
		t.Fatal(err)
	}
	authz, err := identityhttp.NewAuthorizer(authority, coreRules())
	if err != nil {
		t.Fatal(err)
	}
	// The gate use case reads through the identity-backed authority too.
	gateAuthority, err := gatesadapters.NewAuthority(authority, ids)
	if err != nil {
		t.Fatal(err)
	}
	deps := h.E.Dependencies()
	deps.Authority = gateAuthority
	gateSvc, err := gates.NewService(deps, h.E.ServiceOptions())
	if err != nil {
		t.Fatal(err)
	}
	gh, err := gateshttp.NewHandler(gateshttp.Config{Service: gateSvc, Decider: dvDecider{}, Cursors: fxCursors(t)})
	if err != nil {
		t.Fatal(err)
	}
	router, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: authn, Authorizer: authz, RateLimiter: httpapi.Unlimited(), Prefix: APIPrefix})
	if err != nil {
		t.Fatal(err)
	}
	if err := gh.RegisterRoutes(router); err != nil {
		t.Fatal(err)
	}
	w.server = httptest.NewServer(router)
	t.Cleanup(w.server.Close)

	links, err := chatops.NewLinkBuilder(devOrigin)
	if err != nil {
		t.Fatal(err)
	}
	link, err := links.URL(c.Scope, "gate", string(gate.ID))
	if err != nil {
		t.Fatal(err)
	}
	w.path = strings.TrimPrefix(link, devOrigin)
	return w
}

// revokeReviewer ends the reviewer's binding through the identity use case, as an
// administrator does.
func (w *m3GateWorld) revokeReviewer(t *testing.T) {
	t.Helper()
	_, err := w.identity.Revoke(context.Background(), identity.RevokeCommand{
		Caller: identity.Caller{UserID: "u_m3_admin", RequestID: "req_m3_revoke"}, Tenant: w.scope.TenantID, ID: w.binding, Expected: 1,
	})
	if err != nil {
		t.Fatalf("revoke the reviewer: %v", err)
	}
}

// ---- the contract -----------------------------------------------------------------

// summaryOf reads a committed client-visible baseline.
func summaryOf(t testing.TB, name string) apispec.Summary {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	s, err := apispec.ParseSummary(raw)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestM3ContractCompatibility (m3_contract_compatibility): the aggregate stays
// compatible with every client written against an earlier composition (the manual
// agent/delivery aggregate, the M2 aggregate and the business-loop aggregate:
// AC-10), the public client is generated from it reproducibly, the operations the
// business loop adds are exactly the ones the modules route and every payload the
// composition serves or sends validates against its published contract (AC-12).
func TestM3ContractCompatibility(t *testing.T) {
	root := repoRoot(t)
	apiDir := filepath.Join(root, "api")
	current, err := os.ReadFile(filepath.Join(apiDir, "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	now, err := apispec.Summarize(current)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("every operation, parameter, response and member of each earlier aggregate is still there", func(t *testing.T) {
		for _, name := range []string{"api-baseline-t072.json", "api-baseline-t039.json", "api-baseline-t075.json"} {
			baseline := summaryOf(t, name)
			if len(baseline.Operations) < 50 || len(baseline.Schemas) < 100 {
				t.Fatalf("%s is too small to mean anything: %d operations, %d schemas", name, len(baseline.Operations), len(baseline.Schemas))
			}
			if p := now.CompatibleWith(baseline, current); len(p) != 0 {
				t.Errorf("the aggregate breaks the clients of %s:\n%s", name, strings.Join(p, "\n"))
			}
		}
	})

	t.Run("the operations added since M2 are exactly the business loop's, each with a registered authorization action", func(t *testing.T) {
		m2 := summaryOf(t, "api-baseline-t039.json")
		var added []string
		for k := range now.Operations {
			if _, known := m2.Operations[k]; !known {
				added = append(added, k)
			}
		}
		slices.Sort(added)
		var want []string
		for k := range m3Operations() {
			want = append(want, k)
		}
		slices.Sort(want)
		if !slices.Equal(added, want) {
			t.Fatalf("operations added since the M2 aggregate\n got %v\nwant the modules' routes %v", added, want)
		}
		// This composition exposes nothing beyond the business-loop baseline: a later
		// operation needs its own action and its own entry in the matrix above.
		bl := summaryOf(t, "api-baseline-t075.json")
		for k := range now.Operations {
			if _, known := bl.Operations[k]; !known {
				t.Errorf("operation %s is new since the business-loop composition and has no authorization evidence here", k)
			}
		}
	})

	t.Run("a break of an earlier aggregate is detected", func(t *testing.T) {
		for _, name := range []string{"api-baseline-t039.json", "api-baseline-t075.json"} {
			baseline := summaryOf(t, name)
			for what, edit := range map[string]func(string) string{
				"an operation removed": func(s string) string { return strings.Replace(s, "operationId: cancelRun", "operationId: abortRun", 1) },
				"a business-loop operation removed": func(s string) string {
					return strings.Replace(s, "operationId: addWorkItemDependency", "operationId: linkWorkItem", 1)
				},
				"a response member removed": func(s string) string {
					return strings.Replace(s, "        sourceCommit:\n", "        sourceCommitX:\n", 1)
				},
			} {
				mutated := edit(string(current))
				if mutated == string(current) {
					t.Fatalf("%s: the edit %q did not apply", name, what)
				}
				s, err := apispec.Summarize([]byte(mutated))
				if err != nil {
					t.Fatal(err)
				}
				if name == "api-baseline-t039.json" && strings.HasPrefix(what, "a business-loop") {
					continue // the M2 aggregate never had it
				}
				if p := s.CompatibleWith(baseline, []byte(mutated)); len(p) == 0 {
					t.Errorf("%s: %s was not detected", name, what)
				}
			}
		}
	})

	t.Run("generation from the assembled document is reproducible and the client only grows", func(t *testing.T) {
		assertClientReproducible(t)
		// The exported surface of the client is pinned by api/client's own tests
		// (TestExportedSurfaceOnlyGrows and TestEarlierConstantsCompile).
		if _, err := os.Stat(filepath.Join(root, "api", "client", "testdata", "exported-baseline.txt")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("the document publishes neither the flow-summary options nor the outbound payload", func(t *testing.T) {
		for _, banned := range []string{"includeFlowSummary", "flowFrom", "flowTo", "WebhookPayload", "FlowSummary", "/gateway/v1", "/git-hooks"} {
			if strings.Contains(string(current), banned) {
				t.Errorf("the public document mentions %q", banned)
			}
		}
	})

	t.Run("a client tolerates additive members of the business-loop responses", func(t *testing.T) {
		srv := httptestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.Contains(r.URL.Path, "chatops-destinations"):
				_, _ = w.Write([]byte(`{"items":[{"id":"cdt_1","name":"n","endpoint":"https://h.example.test/x","events":["gate.requested"],"enabled":true,"bindingRevision":1,"version":1,"createdAt":"2026-10-19T10:00:00Z","updatedAt":"2026-10-19T10:00:00Z","addedLater":{"k":1}}],"nextCursor":null,"addedLater":true}`))
			default:
				_, _ = w.Write([]byte(`{"items":[{"dependentId":"wi_1","dependencyId":"wi_2","createdAt":"2026-10-19T10:00:00Z","createdBy":{"id":"u_1","type":"user","addedLater":1},"addedLater":[1]}],"nextCursor":null}`))
			}
		}))
		c, err := client.NewClientWithResponses(srv)
		if err != nil {
			t.Fatal(err)
		}
		dest, err := c.ListChatopsDestinationsWithResponse(context.Background(), "t_acme", "p_inv", &client.ListChatopsDestinationsParams{})
		if err != nil || dest.JSON200 == nil || len(dest.JSON200.Items) != 1 || dest.JSON200.Items[0].Id != "cdt_1" {
			t.Fatalf("an unknown member broke the destination list: %v %s", err, dest.Body)
		}
		deps, err := c.ListWorkItemDependenciesWithResponse(context.Background(), "t_acme", "p_inv", "wi_1", &client.ListWorkItemDependenciesParams{})
		if err != nil || deps.JSON200 == nil || len(deps.JSON200.Items) != 1 || deps.JSON200.Items[0].DependencyId != "wi_2" {
			t.Fatalf("an unknown member broke the dependency list: %v %s", err, deps.Body)
		}
	})

	d := newDevSlice(t)
	ctx := context.Background()
	tenant, project := string(devTenant), string(devProject)
	admin, eng, viewer := d.api(devAdminToken), d.api(devEngineerToken), d.api(devViewerToken)
	public := compileFile(t, filepath.Join(apiDir, "openapi.yaml"), true)

	t.Run("served business-loop payloads validate against the published schemas", func(t *testing.T) {
		a, b := d.createItem("m3c-a"), d.createItem("m3c-b")
		for _, id := range []string{a.Id, b.Id} {
			if r := d.transition(eng, id, "ready"); r.StatusCode() != http.StatusOK {
				t.Fatalf("ready: %d %s", r.StatusCode(), r.Body)
			}
		}
		added := d.addDependency(devEngineerToken, a.Id, b.Id)
		if added.StatusCode() != http.StatusCreated && added.StatusCode() != http.StatusOK {
			t.Fatalf("add: %d %s", added.StatusCode(), added.Body)
		}
		if err := public("#/components/schemas/DependencyChange", added.Body); err != nil {
			t.Errorf("the added link: %v", err)
		}
		list, err := viewer.ListWorkItemDependenciesWithResponse(ctx, tenant, project, a.Id, &client.ListWorkItemDependenciesParams{})
		if err != nil || list.JSON200 == nil || len(list.JSON200.Items) != 1 {
			t.Fatalf("list: %v %d %s", err, list.StatusCode(), list.Body)
		}
		if err := public("#/components/schemas/DependencyPage", list.Body); err != nil {
			t.Errorf("the link page: %v", err)
		}
		if err := public("#/components/schemas/DependencyPage", []byte(`{"items":[{"dependentId":7}],"nextCursor":null}`)); err == nil {
			t.Error("the schema accepted a mistyped page: the check proves nothing")
		}

		hook := "https://" + devHookHost + "/m3"
		created, err := admin.CreateChatopsDestinationWithResponse(ctx, tenant, project, client.DestinationCreate{
			Name: "m3", Endpoint: hook, Events: []client.ChatopsEventType{client.ChatopsEventTypeWorkItemTransitioned},
		})
		if err != nil || created.JSON201 == nil {
			t.Fatalf("create destination: %v %d %s", err, created.StatusCode(), created.Body)
		}
		if err := public("#/components/schemas/Destination", created.Body); err != nil {
			t.Errorf("the destination: %v", err)
		}
		got, _ := admin.GetChatopsDestinationWithResponse(ctx, tenant, project, created.JSON201.Id)
		if err := public("#/components/schemas/Destination", got.Body); err != nil {
			t.Errorf("the read destination: %v", err)
		}
		off := false
		patched, err := admin.UpdateChatopsDestinationWithResponse(ctx, tenant, project, created.JSON201.Id,
			&client.UpdateChatopsDestinationParams{IfMatch: ifMatchVersion(int64(created.JSON201.Version))}, client.DestinationEdit{Enabled: &off})
		if err != nil || patched.JSON200 == nil {
			t.Fatalf("patch destination: %v %d %s", err, patched.StatusCode(), patched.Body)
		}
		if err := public("#/components/schemas/Destination", patched.Body); err != nil {
			t.Errorf("the edited destination: %v", err)
		}
		on := true
		if r, _ := admin.UpdateChatopsDestinationWithResponse(ctx, tenant, project, created.JSON201.Id,
			&client.UpdateChatopsDestinationParams{IfMatch: ifMatchVersion(int64(patched.JSON200.Version))}, client.DestinationEdit{Enabled: &on}); r.JSON200 == nil {
			t.Fatalf("re-enable: %d %s", r.StatusCode(), r.Body)
		}
		dests, _ := admin.ListChatopsDestinationsWithResponse(ctx, tenant, project, &client.ListChatopsDestinationsParams{})
		if err := public("#/components/schemas/DestinationPage", dests.Body); err != nil {
			t.Errorf("the destination page: %v", err)
		}
		if err := public("#/components/schemas/Destination", []byte(`{"id":1}`)); err == nil {
			t.Error("the schema accepted a mistyped destination: the check proves nothing")
		}

		// A notification becomes a delivery record the API shows, and the payload the
		// receiver is sent is the published receiver contract.
		d.publishAndPump()
		item := d.createItem("m3c-notify")
		if r := d.transition(eng, item.Id, "ready"); r.StatusCode() != http.StatusOK {
			t.Fatalf("ready: %d %s", r.StatusCode(), r.Body)
		}
		d.publishAndPump()
		if res, err := d.BL.Worker.RunOnce(ctx); err != nil || res.Delivered == 0 {
			t.Fatalf("worker pass: %+v %v", res, err)
		}
		dl, err := viewer.ListChatopsDeliveriesWithResponse(ctx, tenant, project, &client.ListChatopsDeliveriesParams{})
		if err != nil || dl.JSON200 == nil || len(dl.JSON200.Items) == 0 {
			t.Fatalf("deliveries: %v %d %s", err, dl.StatusCode(), dl.Body)
		}
		if err := public("#/components/schemas/DeliveryPage", dl.Body); err != nil {
			t.Errorf("the delivery page: %v", err)
		}
		one, _ := viewer.GetChatopsDeliveryWithResponse(ctx, tenant, project, dl.JSON200.Items[0].Id)
		if err := public("#/components/schemas/DeliveryDetail", one.Body); err != nil {
			t.Errorf("the delivery detail: %v", err)
		}
		hooks := compileWebhookSchemas(t, apiDir)
		payloads := 0
		for _, del := range d.BL.Chat.Deliveries() {
			if !strings.Contains(string(del.Payload), item.Id) {
				continue
			}
			payloads++
			if err := hooks("#/components/schemas/WebhookPayload", del.Payload); err != nil {
				t.Errorf("the payload sent to the receiver: %v\n%s", err, del.Payload)
			}
		}
		if payloads == 0 {
			t.Fatal("no payload of the transition was validated")
		}
		if err := hooks("#/components/schemas/WebhookPayload", []byte(`{"schemaVersion":"one"}`)); err == nil {
			t.Error("the payload schema accepted a mistyped document: the check proves nothing")
		}

		// The error envelope of a refused write is the public one.
		bad, _ := admin.CreateChatopsDestinationWithResponse(ctx, tenant, project, client.DestinationCreate{
			Name: "evil", Endpoint: "https://evil.example.invalid/x", Events: []client.ChatopsEventType{client.ChatopsEventTypeWorkItemTransitioned},
		})
		if bad.StatusCode() != http.StatusUnprocessableEntity {
			t.Fatalf("%d %s", bad.StatusCode(), bad.Body)
		}
		if err := public("#/components/schemas/ErrorEnvelope", bad.Body); err != nil {
			t.Errorf("the refusal envelope: %v", err)
		}
		cyc := d.addDependency(devEngineerToken, b.Id, a.Id)
		if cyc.StatusCode() != http.StatusUnprocessableEntity {
			t.Fatalf("a cycle: %d %s", cyc.StatusCode(), cyc.Body)
		}
		if err := public("#/components/schemas/ErrorEnvelope", cyc.Body); err != nil {
			t.Errorf("the cycle envelope: %v", err)
		}
	})
}

// compileWebhookSchemas returns a validator over the receiver contract of
// api/chatops.yaml. The outbound payload is not part of the public document (a
// client never calls it), so the fragment itself is compiled: the references to
// api/common.yaml are made local and the shared identifier schemas are taken from
// the aggregate, which carries the same definitions.
func compileWebhookSchemas(t testing.TB, apiDir string) func(ref string, doc []byte) error {
	t.Helper()
	var frag, agg map[string]any
	if err := yaml.Unmarshal([]byte(strings.ReplaceAll(mustRead(t, filepath.Join(apiDir, "chatops.yaml")), "common.yaml#/", "#/")), &frag); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(mustRead(t, filepath.Join(apiDir, "openapi.yaml"))), &agg); err != nil {
		t.Fatal(err)
	}
	schemas := frag["components"].(map[string]any)["schemas"].(map[string]any)
	for _, name := range []string{"ID", "Version", "Page", "ErrorEnvelope"} {
		if _, own := schemas[name]; !own {
			schemas[name] = agg["components"].(map[string]any)["schemas"].(map[string]any)[name]
		}
	}
	out, err := yaml.Marshal(frag)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "chatops-receiver.yaml")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return compileFile(t, path, true)
}
