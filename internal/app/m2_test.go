package app

// Acceptance of the M2 composition (T-039): m2_composition_smoke and
// m2_contract_compatibility. Everything runs in the `development` environment
// over the modules' memory fixtures with deterministic provider and tool
// responses, no container and no service (`make test-m2`). It verifies
// AC-10/AC-12, PX-01/PX-13 and GM-12 at the composition, not at the real SQL,
// Git, deployment or Podman joins (T-073, T-067, T-038) and not against a live
// provider or wrapper image (T-085).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/agents/agentstest"
	"github.com/optimiware/kodama/internal/app/apispec"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	gatesmemory "github.com/optimiware/kodama/internal/gates/gatestest/memory"
	"github.com/optimiware/kodama/internal/gateway/gatewaytest"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/gitwebhooktest"
	"github.com/optimiware/kodama/internal/mcpproxy"
	mcpmem "github.com/optimiware/kodama/internal/mcpproxy/mcptest/memory"
)

// call sends a request to the composed process and returns the status, headers
// and body.
func (d *devSlice) send(method, path, token string, header map[string]string, body string) (int, http.Header, []byte) {
	d.t.Helper()
	req, err := http.NewRequest(method, d.Server.URL+path, strings.NewReader(body))
	if err != nil {
		d.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, resp.Header, out
}

const (
	m2Endpoint = gatewaytest.DefaultEndpoint // https://models.t083.example.invalid/v1
	m2ModelKey = "secret://project/model-key"
)

func m2Settings(maxRequests int64) client.ReplaceModelSettingsRequest {
	return client.ReplaceModelSettingsRequest{
		Enabled: true, Endpoint: m2Endpoint, Credential: client.CredentialReference{Ref: m2ModelKey, Version: 1},
		Models: []string{devModel},
		Limits: client.ModelLimits{MaxRequests: maxRequests, TokenAllowance: 10000, MaxRequestBytes: 1 << 16, MaxInputTokens: 2000, MaxOutputTokens: 500},
	}
}

func ifMatch(version int64) *client.ReplaceModelSettingsParams {
	return &client.ReplaceModelSettingsParams{IfMatch: `"` + strconv.FormatInt(version, 10) + `"`}
}

// m2Profile is a profile draft for the fixture's qualified wrapper tuple naming
// the given model and tools.
func m2Profile(t *testing.T, name string, model agents.Model, tools ...string) agents.Draft {
	t.Helper()
	dr := agentstest.Draft(t, name)
	dr.Model, dr.Tools = model, tools
	return dr
}

// refusedWith reports whether err is a profile validation failure carrying the
// diagnostic code.
func refusedWith(err error, code string) bool {
	var v *agents.ValidationError
	if !errors.As(err, &v) {
		return false
	}
	for _, dg := range v.Diagnostics {
		if dg.Code == code {
			return true
		}
	}
	return false
}

func (d *devSlice) registerProfile(dr agents.Draft) error {
	_, err := d.registerProfileVersion(dr)
	return err
}

func (d *devSlice) registerProfileVersion(dr agents.Draft) (agents.ProfileVersion, error) {
	res, err := d.M2.Profiles.Register(context.Background(), agents.RegisterCommand{
		Scope: devScope, Caller: agents.Caller{PrincipalID: devEngineer, RequestID: "req_m2"}, Draft: dr,
	})
	return res.Profile, err
}

// seedAgentWorld is a work item with a provisioned workspace and a registered
// one-step agent pipeline naming the profile reference, registered through the
// public API (the definition validator resolves the profile through the
// composition's profile service).
func (d *devSlice) seedAgentWorld(name, profileRef string) (devWorld, *client.RegisterPipelineDefinitionResponse) {
	d.t.Helper()
	w := d.seedWorld(name, "echo ok") // the work item, workspace and a container definition
	doc := map[string]any{
		"apiVersion": "kodama/v1alpha1", "kind": "Pipeline", "metadata": map[string]any{"name": name + "-agent"},
		"spec": map[string]any{"inputs": map[string]any{}, "steps": []any{map[string]any{
			"id": "implement", "kind": "agent", "agent": map[string]any{"profile": profileRef}, "timeoutSeconds": 600,
			"workspace": map[string]any{"access": "none"},
			"outputs":   map[string]any{"summary": map[string]any{"type": "string"}},
		}}},
	}
	r, err := d.api(devEngineerToken).RegisterPipelineDefinitionWithBodyWithResponse(context.Background(), string(devTenant), string(devProject), "application/json", jsonReader(d.t, doc))
	if err != nil {
		d.t.Fatal(err)
	}
	if r.JSON201 != nil {
		w.def = *r.JSON201
	}
	return w, r
}

// TestM2CompositionSmoke (m2_composition_smoke): the manual agent/delivery slice
// of M1 joined with the model gateway, the MCP proxy and the native Git trigger,
// composed in one process over deterministic fixtures: route and capability
// registration, authority at every seam, approvals that follow the stored
// settings and registrations, a streamed deterministic reply under a run
// budget, tool visibility, an exact-commit push and the construction guard of
// the production environment. GM-12, PX-01, PX-13 and AC-12 at the composition.
func TestM2CompositionSmoke(t *testing.T) {
	d := newDevSlice(t)
	ctx := context.Background()
	tenant, project := string(devTenant), string(devProject)
	admin, viewer, stranger := d.api(devAdminToken), d.api(devViewerToken), d.api(devOtherToken)

	t.Run("the M2 groups are served and recorded, and ChatOps is served by the business-loop composition", func(t *testing.T) {
		for _, group := range []string{"model-settings", "model-gateway", "mcp-servers", "mcp-proxy", "agent-profiles", "gates", "deployments"} {
			if !slices.Contains(d.Routes.Registered, group) {
				t.Errorf("route group %q is not registered: %v", group, d.Routes.Registered)
			}
		}
		for _, served := range []string{"git-webhooks", "model-gateway", "mcp-proxy", "agent-steps", "agents", "gates"} {
			if slices.Contains(d.Routes.Disabled, served) {
				t.Errorf("%q is wired and must not be recorded as disabled: %v", served, d.Routes.Disabled)
			}
		}
		if slices.Contains(d.Routes.Disabled, "chatops") || !slices.Contains(d.Routes.Registered, "chatops") {
			t.Errorf("chatops is served since T-075 and must be registered, not disabled: %v %v", d.Routes.Registered, d.Routes.Disabled)
		}
		for _, w := range d.ModelTools.Workers {
			if w.Name == "" || w.Run == nil {
				t.Errorf("worker %+v is incomplete", w)
			}
		}
		names := map[string]bool{}
		for _, w := range d.ModelTools.Workers {
			names[w.Name] = true
		}
		if !names["model-gateway-reconciler"] || !names["mcp-proxy-sweeper"] {
			t.Errorf("recovery workers are missing: %v", names)
		}
	})

	t.Run("a profile naming an unapproved model or tool is refused before any settings exist", func(t *testing.T) {
		before := d.registerProfile(m2Profile(t, "early", agents.Model{Name: devModel, Endpoint: m2Endpoint}))
		if !refusedWith(before, agents.CodeUnapprovedModel) {
			t.Fatalf("a model no project settings enable was approved: %v", before)
		}
	})

	t.Run("model settings are managed through the public API under the identity authority", func(t *testing.T) {
		got, err := viewer.GetModelSettingsWithResponse(ctx, tenant, project)
		if err != nil || got.JSON200 == nil || got.JSON200.Stored || got.JSON200.Enabled || got.JSON200.Version != 1 {
			t.Fatalf("the virtual disabled revision: %v %d %s", err, got.StatusCode(), got.Body)
		}
		if r, _ := viewer.ReplaceModelSettingsWithResponse(ctx, tenant, project, ifMatch(1), m2Settings(1)); r.StatusCode() != http.StatusForbidden {
			t.Fatalf("a viewer changed model settings: %d %s", r.StatusCode(), r.Body)
		}
		if r, _ := stranger.ReplaceModelSettingsWithResponse(ctx, tenant, project, ifMatch(1), m2Settings(1)); r.StatusCode() != http.StatusNotFound {
			t.Fatalf("a stranger learned about the project: %d %s", r.StatusCode(), r.Body)
		}
		if r, _ := d.api("").GetModelSettingsWithResponse(ctx, tenant, project); r.StatusCode() != http.StatusUnauthorized {
			t.Fatalf("anonymous read: %d", r.StatusCode())
		}
		bad := m2Settings(1)
		bad.Endpoint = "https://evil.example.invalid/v1"
		if r, _ := admin.ReplaceModelSettingsWithResponse(ctx, tenant, project, ifMatch(1), bad); r.StatusCode() < 400 || r.StatusCode() >= 500 {
			t.Fatalf("an endpoint the installation never approved was stored: %d %s", r.StatusCode(), r.Body)
		}
		if st, err := d.M2.GWStore.Settings(ctx, devScope); err != nil || st.Stored() {
			t.Fatalf("a refused edit stored something: %+v %v", st, err)
		}
		missing, _ := admin.ReplaceModelSettingsWithResponse(ctx, tenant, project, &client.ReplaceModelSettingsParams{}, m2Settings(1))
		if missing.StatusCode() != http.StatusBadRequest && missing.StatusCode() != http.StatusPreconditionRequired {
			t.Fatalf("a replacement without If-Match: %d %s", missing.StatusCode(), missing.Body)
		}
		put, err := admin.ReplaceModelSettingsWithResponse(ctx, tenant, project, ifMatch(1), m2Settings(1))
		if err != nil || put.JSON200 == nil || !put.JSON200.Stored || !put.JSON200.Enabled || put.JSON200.Version != 2 {
			t.Fatalf("store settings: %v %d %s", err, put.StatusCode(), put.Body)
		}
		if stale, _ := admin.ReplaceModelSettingsWithResponse(ctx, tenant, project, ifMatch(1), m2Settings(1)); stale.StatusCode() != http.StatusPreconditionFailed {
			t.Fatalf("a stale version: %d %s", stale.StatusCode(), stale.Body)
		}
	})

	var registration client.McpServer
	t.Run("MCP registrations are approved by the installation and checked against the project's secrets", func(t *testing.T) {
		draft := client.McpServerDraft{Endpoint: mcpmem.DefaultEndpoint, Credential: client.CredentialRef{Name: "remote-token", Version: 1}, Tools: []string{"echo"}}
		if r, _ := viewer.CreateMcpServerWithResponse(ctx, tenant, project, draft); r.StatusCode() != http.StatusForbidden {
			t.Fatalf("a viewer registered a server: %d %s", r.StatusCode(), r.Body)
		}
		if r, _ := admin.CreateMcpServerWithResponse(ctx, tenant, project, draft); r.StatusCode() < 400 || r.StatusCode() >= 500 {
			t.Fatalf("a registration naming no existing secret was stored: %d %s", r.StatusCode(), r.Body)
		}
		ref, err := d.M2.MCPWorld.Secrets.Create(ctx, devScope, "remote-token", mcpmem.DefaultOrigin, "remote-bearer-token")
		if err != nil {
			t.Fatal(err)
		}
		draft.Credential = client.CredentialRef{Name: ref.Name, Version: ref.Version}
		other := draft
		other.Endpoint = "https://not-approved.example.invalid/mcp"
		if r, _ := admin.CreateMcpServerWithResponse(ctx, tenant, project, other); r.StatusCode() != http.StatusUnprocessableEntity {
			t.Fatalf("an unapproved endpoint: %d %s", r.StatusCode(), r.Body)
		}
		r, err := admin.CreateMcpServerWithResponse(ctx, tenant, project, draft)
		if err != nil || r.JSON201 == nil || r.JSON201.Version != 1 {
			t.Fatalf("register: %v %d %s", err, r.StatusCode(), r.Body)
		}
		registration = *r.JSON201
		list, err := viewer.ListMcpServersWithResponse(ctx, tenant, project, &client.ListMcpServersParams{})
		if err != nil || list.JSON200 == nil || len(list.JSON200.Items) != 1 || list.JSON200.Items[0].Id != registration.Id {
			t.Fatalf("list: %v %d %s", err, list.StatusCode(), list.Body)
		}
		if strings.Contains(string(list.Body), "remote-bearer-token") {
			t.Fatal("a credential value reached a registration response")
		}
	})

	var coding, withTool agents.ProfileVersion
	var codingDraft agents.Draft
	t.Run("profile registration follows the stored settings and registrations (PX-01)", func(t *testing.T) {
		ok := agents.Model{Name: devModel, Endpoint: m2Endpoint}
		codingDraft = m2Profile(t, "coding", ok)
		var err error
		if coding, err = d.registerProfileVersion(codingDraft); err != nil {
			t.Fatalf("the approved model was refused: %v", err)
		}
		for name, tc := range map[string]struct {
			draft agents.Draft
			code  string
		}{
			"another model":       {m2Profile(t, "other-model", agents.Model{Name: "gpt-z", Endpoint: m2Endpoint}), agents.CodeUnapprovedModel},
			"another endpoint":    {m2Profile(t, "other-endpoint", agents.Model{Name: devModel, Endpoint: "https://elsewhere.example.invalid/v1"}), agents.CodeUnapprovedModel},
			"a bare tool name":    {m2Profile(t, "bare-tool", ok, "echo"), agents.CodeUnapprovedTool},
			"a wildcard tool":     {m2Profile(t, "wild-tool", ok, registration.Id+"/*"), agents.CodeInvalidField},
			"an unenabled tool":   {m2Profile(t, "hidden-tool", ok, registration.Id+"/hidden"), agents.CodeUnapprovedTool},
			"a foreign server":    {m2Profile(t, "foreign", ok, "mcp_0000000000000000/echo"), agents.CodeUnapprovedTool},
			"an unqualified tool": {m2Profile(t, "unqualified", ok, "repo.read"), agents.CodeUnapprovedTool},
		} {
			if err := d.registerProfile(tc.draft); !refusedWith(err, tc.code) {
				t.Errorf("%s: %v (want %s)", name, err, tc.code)
			}
		}
		if withTool, err = d.registerProfileVersion(m2Profile(t, "with-tool", ok, registration.Id+"/echo")); err != nil {
			t.Fatalf("the registered tool was refused: %v", err)
		}
		// A tuple the installation never qualified registers nothing (AR-02).
		unqualified := m2Profile(t, "unqualified-wrapper", ok)
		unqualified.Wrapper.Version = "9.9.9"
		if err := d.registerProfile(unqualified); !errors.Is(err, agents.ErrUnsupported) {
			t.Fatalf("a wrapper tuple without compatibility evidence registered: %v", err)
		}
		// Disabling the project's settings withdraws the model approval.
		off := m2Settings(1)
		off.Enabled = false
		if r, err := admin.ReplaceModelSettingsWithResponse(ctx, tenant, project, ifMatch(2), off); err != nil || r.JSON200 == nil {
			t.Fatalf("disable: %v %d %s", err, r.StatusCode(), r.Body)
		}
		if err := d.registerProfile(m2Profile(t, "after-disable", ok)); !refusedWith(err, agents.CodeUnapprovedModel) {
			t.Fatalf("a disabled project approved a model: %v", err)
		}
		if r, err := admin.ReplaceModelSettingsWithResponse(ctx, tenant, project, ifMatch(3), m2Settings(1)); err != nil || r.JSON200 == nil {
			t.Fatalf("re-enable: %v %d %s", err, r.StatusCode(), r.Body)
		}
	})

	t.Run("an agent definition is validated and admitted through the M2-approved profile (PX-01)", func(t *testing.T) {
		// A definition can name only what the composition's profile service
		// resolves: an unknown profile and a mutable alias never register.
		for name, ref := range map[string]string{"an unknown profile": "missing@1", "a mutable alias": "coding"} {
			if _, r := d.seedAgentWorld("m2-bad-"+strings.ReplaceAll(name, " ", "-"), ref); r.JSON201 != nil || r.StatusCode() < 400 || r.StatusCode() >= 500 {
				t.Errorf("%s: a definition was registered: %d %s", name, r.StatusCode(), r.Body)
			}
		}
		w, r := d.seedAgentWorld("m2-agent", coding.Ref())
		if r.JSON201 == nil {
			t.Fatalf("the agent definition was refused: %d %s", r.StatusCode(), r.Body)
		}
		eng := d.api(devEngineerToken)
		first := d.createRun(eng, "m2-agent-1", w, string(d.Repo.Initial))
		expectStatus(t, first, first.Body, http.StatusCreated, "admit an agent run")
		second := d.createRun(eng, "m2-agent-2", w, string(d.Repo.Initial))
		expectStatus(t, second, second.Body, http.StatusCreated, "admit the same agent definition again")

		stepOf := func(id string) (image string, command, args []string) {
			agg, err := d.Exec.Get(ctx, devScope, domain.RunID(id))
			if err != nil || len(agg.Steps) != 1 {
				t.Fatalf("run %s: %v", id, err)
			}
			sp := agg.Steps[0].Spec
			return sp.Image, sp.Command, sp.Args
		}
		image, command, args := stepOf(first.JSON201.Id)
		if image != codingDraft.Image || !slices.Equal(command, codingDraft.Wrapper.Command) {
			t.Fatalf("the step was not frozen as the profile's wrapper: %s %v", image, command)
		}
		if _, present, err := agents.DecodeArgs(args); err != nil || !present {
			t.Fatalf("the frozen payload is not the agent wrapper's: %v %v", present, err)
		}
		if image2, _, args2 := stepOf(second.JSON201.Id); image2 != image || !slices.Equal(args, args2) {
			t.Fatal("the same profile expanded twice must freeze identical steps")
		}
		// A container step carrying the reserved payload claims a profile it never
		// resolved: admission refuses it.
		forged := map[string]any{
			"apiVersion": "kodama/v1alpha1", "kind": "Pipeline", "metadata": map[string]any{"name": "m2-forged"},
			"spec": map[string]any{"inputs": map[string]any{}, "steps": []any{map[string]any{
				"id": "implement", "kind": "container", "timeoutSeconds": 60, "image": image, "command": command, "args": args,
				"workspace": map[string]any{"from": "run", "access": "read"},
			}}},
		}
		fr, err := eng.RegisterPipelineDefinitionWithBodyWithResponse(ctx, tenant, project, "application/json", jsonReader(t, forged))
		if err != nil {
			t.Fatal(err)
		}
		if fr.JSON201 != nil {
			fw := w
			fw.def = *fr.JSON201
			if r := d.createRun(eng, "m2-forged", fw, string(d.Repo.Initial)); r.StatusCode() < 400 || r.StatusCode() >= 500 {
				t.Fatalf("a container step carrying the agent payload was admitted: %d %s", r.StatusCode(), r.Body)
			}
		}
		// A profile whose tool the project registered resolves too.
		if tw, r := d.seedAgentWorld("m2-agent-tool", withTool.Ref()); r.JSON201 == nil {
			t.Fatalf("the tool profile was refused: %d %s", r.StatusCode(), r.Body)
		} else if r := d.createRun(eng, "m2-agent-tool", tw, string(d.Repo.Initial)); r.StatusCode() != http.StatusCreated {
			t.Fatalf("admit the tool profile: %d %s", r.StatusCode(), r.Body)
		}
		// Withdrawing the project's model settings withdraws the profile: a new
		// admission is refused until they are restored (the approval is current,
		// not frozen at registration).
		cur, err := admin.GetModelSettingsWithResponse(ctx, tenant, project)
		if err != nil || cur.JSON200 == nil {
			t.Fatalf("%v %s", err, cur.Body)
		}
		off := m2Settings(1)
		off.Enabled = false
		if r, err := admin.ReplaceModelSettingsWithResponse(ctx, tenant, project, ifMatch(cur.JSON200.Version), off); err != nil || r.JSON200 == nil {
			t.Fatalf("disable: %v %d %s", err, r.StatusCode(), r.Body)
		}
		refused := d.createRun(eng, "m2-agent-withdrawn", w, string(d.Repo.Initial))
		restore, err := admin.ReplaceModelSettingsWithResponse(ctx, tenant, project, ifMatch(cur.JSON200.Version+1), m2Settings(1))
		if err != nil || restore.JSON200 == nil {
			t.Fatalf("restore: %v %d %s", err, restore.StatusCode(), restore.Body)
		}
		if refused.StatusCode() == http.StatusCreated {
			t.Fatalf("a profile whose model approval was withdrawn was admitted: %s", refused.Body)
		}
		if refused.StatusCode() < 400 || refused.StatusCode() >= 500 {
			t.Fatalf("the refusal is not a client error: %d %s", refused.StatusCode(), refused.Body)
		}
		if r := d.createRun(eng, "m2-agent-restored", w, string(d.Repo.Initial)); r.StatusCode() != http.StatusCreated {
			t.Fatalf("admission after the settings were restored: %d %s", r.StatusCode(), r.Body)
		}
	})

	t.Run("a gate waits without compute, progresses once after a valid decision and refuses stale and revoked ones (PX-13)", func(t *testing.T) {
		// The T-072 gate fixture (the memory gate module over its execution peer)
		// consumed by this composition's delivery slice: its workers, routes and
		// the reconciler are the ones NewCore starts; only the gate service and its
		// store are the module's memory twin.
		h := gatesmemory.NewHarness(t)
		h.E.SetTenants(gatesmemory.ScopeA.TenantID)
		in := d.DeliveryIn
		in.Gates = h.E.Svc
		in.Tuning.ReconcileInterval = 5 * time.Millisecond
		slice, err := AssembleDelivery(in)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(slice.Routes, "gates") {
			t.Fatalf("the composition serves no gate routes: %v", slice.Routes)
		}
		start := func() *workerGroup {
			g := newWorkerGroup(context.Background(), nil)
			for _, wk := range slice.Workers {
				g.Go(wk.Name, wk.Run)
			}
			return g
		}
		stop := func(g *workerGroup) {
			t.Helper()
			sc, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			if err := g.Stop(sc); err != nil {
				t.Fatal(err)
			}
		}
		eventually := func(what string, ok func() bool) {
			t.Helper()
			deadline := time.Now().Add(5 * time.Second)
			for !ok() {
				if time.Now().After(deadline) {
					t.Fatalf("timed out waiting for %s", what)
				}
				time.Sleep(2 * time.Millisecond)
			}
		}
		requested := func(c gatesmemoryCase) bool {
			_, err := h.E.Store.ByStep(ctx, c.Scope, c.Run, c.Step)
			return err == nil
		}
		decide := func(c gatesmemoryCase, who string, dec gates.Decision) error {
			t.Helper()
			g, err := h.E.Store.ByStep(ctx, c.Scope, c.Run, c.Step)
			if err != nil {
				t.Fatalf("the reconciler did not request the gate: %v", err)
			}
			_, err = h.E.Svc.Decide(ctx, gates.Caller{PrincipalID: who, RequestID: "req_m2_gate"}, g.Scope, g.ID, gates.DecideCommand{
				Decision: dec, Reason: "Reviewed the identified source and test evidence.", ExpectedVersion: g.Version, SubjectDigest: g.SubjectDigest(),
			})
			return err
		}
		noCompute := func(c gatesmemoryCase, what string) {
			t.Helper()
			if h.Attempts(t, c) != 0 || h.E.Reservations() != 0 {
				t.Fatalf("%s allocated compute: %d attempts, %d reservations", what, h.Attempts(t, c), h.E.Reservations())
			}
		}

		// Waiting, then one valid decision: the step succeeds once, with no attempt.
		c := h.NewCase(t, false)
		g := start()
		eventually("the gate request", func() bool { return requested(c) })
		time.Sleep(30 * time.Millisecond)
		if st := h.StepStatus(t, c); st != execution.StepPending {
			t.Fatalf("a waiting gate moved its step: %s", st)
		}
		noCompute(c, "a waiting gate")
		if err := decide(c, gatesmemory.Engineer, gates.DecisionApprove); !errors.Is(err, gates.ErrDenied) {
			t.Fatalf("an engineer decided a gate: %v", err)
		}
		if err := decide(c, gatesmemory.Reviewer, gates.DecisionApprove); err != nil {
			t.Fatalf("the reviewer's decision: %v", err)
		}
		eventually("the step transition", func() bool { return h.StepStatus(t, c) == execution.StepSucceeded })
		noCompute(c, "progression")
		ev := h.Events(t, c, mustGate(t, h, c).ID)
		time.Sleep(30 * time.Millisecond)
		if ev.Requested != 1 || ev.Decided != 1 || h.Events(t, c, mustGate(t, h, c).ID) != ev {
			t.Fatalf("one request and one decision, nothing repeated: %+v", ev)
		}

		// A stale decision: the subject changed after the request. With the workers
		// stopped (so no pass records the change first) the decision is refused and
		// the staleness recorded once as the gate's invalidation; the workers then
		// find it settled, and the step keeps waiting without compute.
		stale := h.NewCase(t, false)
		eventually("the second gate request", func() bool { return requested(stale) })
		stop(g)
		h.ChangeContent(t, stale)
		if err := decide(stale, gatesmemory.Reviewer, gates.DecisionApprove); !errors.Is(err, gates.ErrStaleSubject) {
			t.Fatalf("a decision about a changed subject: %v", err)
		}
		cur := mustGate(t, h, stale)
		if cur.Status != gates.StatusPending || cur.Decision != nil || cur.Invalidation == nil || cur.Invalidation.Reason != gates.ReasonContentChanged {
			t.Fatalf("the stale decision was recorded or the staleness was not: %+v", cur)
		}
		g = start()
		time.Sleep(40 * time.Millisecond)
		if st := h.StepStatus(t, stale); st != execution.StepPending {
			t.Fatalf("a stale gate moved its step: %s", st)
		}
		noCompute(stale, "a stale gate")
		if ev := h.Events(t, stale, cur.ID); ev.Requested != 1 || ev.Invalidated != 1 || ev.Decided != 0 {
			t.Fatalf("the staleness must be recorded once and nothing decided: %+v", ev)
		}
		stop(g)

		// A revoked reviewer: the decision was durable while the reviewer was
		// current; the grant is revoked before any worker consumes it.
		revoked := h.NewCase(t, false)
		g = start()
		eventually("the third gate request", func() bool { return requested(revoked) })
		stop(g)
		if err := decide(revoked, gatesmemory.Reviewer, gates.DecisionApprove); err != nil {
			t.Fatal(err)
		}
		h.Revoke(t, gatesmemory.Reviewer)
		g = start()
		defer stop(g)
		time.Sleep(40 * time.Millisecond)
		if st := h.StepStatus(t, revoked); st != execution.StepPending {
			t.Fatalf("a revoked reviewer's approval progressed the step: %s", st)
		}
		noCompute(revoked, "a revoked approval")
		if err := g.Healthy(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("the gateway streams a deterministic reply and refuses past the run budget (GM-12)", func(t *testing.T) {
		wl, bearer := d.M2.Workloads.Add(devScope)
		path := "/gateway/v1/attempts/" + string(wl.AttemptID) + "/chat/completions"
		body := `{"model":"` + devModel + `","stream":true,"messages":[{"role":"user","content":"say hello"}]}`
		auth := map[string]string{"Content-Type": "application/json"}

		if status, _, _ := d.send(http.MethodPost, path, "", auth, body); status != http.StatusUnauthorized {
			t.Fatalf("no workload token: %d", status)
		}
		if status, _, _ := d.send(http.MethodPost, path, devAdminToken, auth, body); status != http.StatusUnauthorized {
			t.Fatalf("a user token is no workload credential: %d", status)
		}
		other, _ := d.M2.Workloads.Add(devScope)
		if status, _, _ := d.send(http.MethodPost, "/gateway/v1/attempts/"+string(other.AttemptID)+"/chat/completions", bearer, auth, body); status != http.StatusUnauthorized && status != http.StatusForbidden {
			t.Fatalf("another attempt's route with this token: %d", status)
		}
		if got := len(d.M2.Provider.Calls()); got != 0 {
			t.Fatalf("a refused request reached the provider %d times", got)
		}

		d.M2.Provider.Script(m2Reply(devModel, 12, 3, "hello ", "world"))
		status, header, out := d.send(http.MethodPost, path, bearer, auth, body)
		if status != http.StatusOK || !strings.HasPrefix(header.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("stream: %d %s\n%s", status, header.Get("Content-Type"), out)
		}
		text := string(out)
		if !strings.Contains(text, "hello ") || !strings.Contains(text, "world") || !strings.Contains(text, "[DONE]") {
			t.Fatalf("the deterministic reply is incomplete:\n%s", text)
		}
		for _, secret := range []string{devProviderKey, "t083.example.invalid"} {
			if strings.Contains(text, secret) {
				t.Fatalf("the response leaks %q", secret)
			}
		}
		calls := d.M2.Provider.Calls()
		if len(calls) != 1 {
			t.Fatalf("the provider was called %d times for one request", len(calls))
		}
		// The second request exceeds the run's request cap of one: refused at
		// admission, never sent.
		d.M2.Provider.Script(m2Reply(devModel, 1, 1, "unreachable"))
		status, _, out = d.send(http.MethodPost, path, bearer, auth, body)
		if status < 400 || strings.Contains(string(out), "unreachable") {
			t.Fatalf("the request cap was not enforced: %d %s", status, out)
		}
		if len(d.M2.Provider.Calls()) != 1 {
			t.Fatal("a request past the cap reached the provider")
		}
		// A model outside the allow-list is refused too, even for a fresh run.
		wl2, bearer2 := d.M2.Workloads.Add(devScope)
		if status, _, _ := d.send(http.MethodPost, "/gateway/v1/attempts/"+string(wl2.AttemptID)+"/chat/completions", bearer2, auth,
			`{"model":"gpt-z","stream":true,"messages":[{"role":"user","content":"x"}]}`); status < 400 {
			t.Fatalf("a model outside the allow-list was served: %d", status)
		}
		if len(d.M2.Provider.Calls()) != 1 {
			t.Fatal("a refused model reached the provider")
		}
		// An unknown workload path is the module's not_found envelope.
		if status, _, _ := d.send(http.MethodPost, "/gateway/v1/unknown", bearer, auth, body); status != http.StatusNotFound {
			t.Fatalf("unknown gateway path: %d", status)
		}
	})

	t.Run("the MCP proxy offers an attempt only its pinned, registered tools", func(t *testing.T) {
		world := d.M2.MCPWorld
		for _, name := range []string{"echo", "hidden"} {
			world.Remote.Script(name, mcpmem.Tool{})
		}
		endpoint := d.Server.URL + "/internal/mcp/v1/attempts/att_m2_1/servers/" + registration.Id
		if status, header, _ := d.send(http.MethodPost, "/internal/mcp/v1/attempts/att_m2_1/servers/"+registration.Id, "", nil, `{}`); status != http.StatusUnauthorized || header.Get("WWW-Authenticate") == "" {
			t.Fatalf("no bearer: %d %q", status, header.Get("WWW-Authenticate"))
		}
		wl, err := world.Authority.NewWorkload(ctx, devScope, "run_m2_1", "att_m2_1")
		if err != nil {
			t.Fatal(err)
		}
		world.Policies.Set("att_m2_1", []string{mcpproxy.Qualify(mcpproxy.RegistrationID(registration.Id), "echo")}, nil, true)
		if status, _, _ := d.send(http.MethodPost, "/internal/mcp/v1/attempts/att_other/servers/"+registration.Id, wl.Token, nil, `{}`); status != http.StatusUnauthorized && status != http.StatusForbidden && status != http.StatusNotFound {
			t.Fatalf("another attempt's route with this token: %d", status)
		}

		sdk := mcp.NewClient(&mcp.Implementation{Name: "m2-smoke", Version: "1"}, nil)
		session, err := sdk.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: endpoint, MaxRetries: -1, DisableStandaloneSSE: true,
			HTTPClient: &http.Client{Transport: workloadBearer{token: wl.Token}},
		}, nil)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer session.Close()
		tools, err := session.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tool := range tools.Tools {
			names = append(names, tool.Name)
		}
		if !slices.Equal(names, []string{"echo"}) {
			t.Fatalf("the attempt sees %v, want only its pinned and registered tool", names)
		}
		if res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"v": 1}}); err != nil || res.IsError {
			t.Fatalf("the approved tool: %v %+v", err, res)
		}
		if res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "hidden", Arguments: map[string]any{}}); err == nil && !res.IsError {
			t.Fatal("a tool outside the pinned set was callable")
		}
		if n := world.Remote.CallsOf("hidden"); n != 0 {
			t.Fatalf("the hidden tool reached the remote server %d times", n)
		}
		if n := world.Remote.CallsOf("echo"); n != 1 {
			t.Fatalf("the approved tool reached the remote server %d times", n)
		}
	})

	branch := strings.TrimPrefix(string(d.Repo.BaseRef), "refs/heads/")
	t.Run("a push notification starts a run for the exact commit through the process handler", func(t *testing.T) {
		m := d.M2
		secret := []byte(strings.Repeat("hook-secret-", 4))
		m.HookSecrets.Put(devScope, "hook-secret", secret)
		m.HookDefs.Register(devScope, "def_m2")
		trig, err := m.Hook.Install(ctx, gitwebhook.InstallCommand{
			Scope: devScope, Principal: devLead, DefinitionID: "def_m2", Branches: []string{branch}, SecretRef: "hook-secret",
		})
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		if _, err := m.Hook.Install(ctx, gitwebhook.InstallCommand{
			Scope: devScope, Principal: devViewer, DefinitionID: "def_m2", Branches: []string{"main"}, SecretRef: "hook-secret",
		}); err == nil {
			t.Fatal("a viewer installed a trigger")
		}
		payload := func(repo, ref, after string) []byte {
			return gitwebhooktest.Marshal(t, gitwebhook.Payload{SchemaVersion: gitwebhook.SchemaVersion, Repository: repo, Ref: ref, Before: gitwebhooktest.Zero, After: after})
		}
		send := func(id gitwebhook.TriggerID, key string, secret []byte, ts time.Time, body []byte) (int, []byte) {
			n := gitwebhook.Sign(secret, id, key, ts.Unix(), body)
			status, _, out := d.send(http.MethodPost, "/api/v1/git-hooks/"+string(id), "", map[string]string{
				gitwebhook.HeaderDeliveryID: key, gitwebhook.HeaderTimestamp: strconv.FormatInt(ts.Unix(), 10), gitwebhook.HeaderSignature: n,
				"Content-Type": "application/json",
			}, string(body))
			return status, out
		}
		now := time.Now()
		good := payload("repo_dev", "refs/heads/"+branch, string(d.Repo.Initial))

		// A forged signature, an unknown trigger and a stale timestamp are the
		// same 401 and consume no delivery identifier.
		for name, tc := range map[string]struct {
			id     gitwebhook.TriggerID
			secret []byte
			ts     time.Time
		}{
			"forged":  {trig.ID, []byte(strings.Repeat("x", 48)), now},
			"unknown": {"tr_unknown_trigger", secret, now},
			"stale":   {trig.ID, secret, now.Add(-time.Hour)},
		} {
			if status, out := send(tc.id, "dlv-forged-"+name, tc.secret, tc.ts, good); status != http.StatusUnauthorized || errorCode(out) != "invalid_credential" {
				t.Errorf("%s: %d %s", name, status, out)
			}
		}
		if n := m.HookStore.DeliveryCount(); n != 0 {
			t.Fatalf("an unauthenticated request left %d delivery records", n)
		}
		if status, out := send(trig.ID, "dlv-1", secret, now, good); status != http.StatusAccepted || !strings.Contains(string(out), `"outcome":"started"`) || !strings.Contains(string(out), string(d.Repo.Initial)) {
			t.Fatalf("a signed push of the base branch: %d %s", status, out)
		}
		// The same delivery again is a duplicate of the same run; nothing more starts.
		if status, out := send(trig.ID, "dlv-1", secret, now, good); status != http.StatusAccepted || !strings.Contains(string(out), `"duplicate":true`) {
			t.Fatalf("duplicate delivery: %d %s", status, out)
		}
		if n := m.HookRuns.Created(); n != 1 {
			t.Fatalf("%d runs were admitted for one delivery", n)
		}
		// The same identifier with another body is a conflict.
		if status, out := send(trig.ID, "dlv-1", secret, now, payload("repo_dev", "refs/heads/"+branch, strings.Repeat("b", 40))); status != http.StatusConflict || errorCode(out) != "delivery_conflict" {
			t.Fatalf("conflicting body: %d %s", status, out)
		}
		// A repository other than the trigger's is refused and recorded.
		if status, out := send(trig.ID, "dlv-2", secret, now, payload("repo_other", "refs/heads/"+branch, string(d.Repo.Initial))); status != http.StatusUnprocessableEntity || errorCode(out) != "foreign_repository" {
			t.Fatalf("foreign repository: %d %s", status, out)
		}
		// A commit the branch does not contain is refused against the real
		// (local) Git remote: the announced commit is proved, never trusted.
		if status, out := send(trig.ID, "dlv-3", secret, now, payload("repo_dev", "refs/heads/"+branch, strings.Repeat("c", 40))); status != http.StatusUnprocessableEntity || !strings.Contains(string(out), "commit_not_on_branch") {
			t.Fatalf("unreachable commit: %d %s", status, out)
		}
		// A branch outside the filter is acknowledged and starts nothing.
		if status, out := send(trig.ID, "dlv-4", secret, now, payload("repo_dev", "refs/heads/feature-x", string(d.Repo.Initial))); status != http.StatusOK || !strings.Contains(string(out), `"outcome":"ignored"`) {
			t.Fatalf("filtered branch: %d %s", status, out)
		}
		if n := m.HookRuns.Created(); n != 1 {
			t.Fatalf("a refused or ignored delivery started a run: %d", n)
		}
		// The hook is not part of the bearer-authenticated API: a user token is no
		// credential for it, and other methods are not served.
		if status, _, _ := d.send(http.MethodPost, "/api/v1/git-hooks/"+string(trig.ID), devAdminToken, nil, string(good)); status != http.StatusUnauthorized {
			t.Fatalf("a user token as hook credential: %d", status)
		}
		if status, _, _ := d.send(http.MethodGet, "/api/v1/git-hooks/"+string(trig.ID), devAdminToken, nil, ""); status == http.StatusOK || status == http.StatusAccepted {
			t.Fatalf("GET on the hook: %d", status)
		}
	})

	t.Run("the hook is throttled per remote host and the throttle is not authentication", func(t *testing.T) {
		slice, err := AssembleTriggers(TriggerInputs{Env: Development, Service: d.M2.Hook, RateLimitPerMinute: 2})
		if err != nil {
			t.Fatal(err)
		}
		srv := httptestServer(t, slice.Handler)
		var limited atomic.Int32
		for i := 0; i < 5; i++ {
			req, _ := http.NewRequest(http.MethodPost, srv+"/api/v1/git-hooks/tr_any", strings.NewReader("{}"))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusTooManyRequests:
				limited.Add(1)
				if resp.Header.Get("Retry-After") == "" || errorCode(b) != "rate_limited" {
					t.Fatalf("throttle response: %v %s", resp.Header, b)
				}
			case http.StatusUnauthorized:
			default:
				t.Fatalf("status %d %s", resp.StatusCode, b)
			}
		}
		if limited.Load() == 0 {
			t.Fatal("the flood was not throttled")
		}
	})

	t.Run("the recovery workers start and stop deliberately", func(t *testing.T) {
		g := newWorkerGroup(context.Background(), nil)
		for _, w := range d.ModelTools.Workers {
			g.Go(w.Name, w.Run)
		}
		time.Sleep(50 * time.Millisecond)
		if got := g.Names(); len(got) != len(d.ModelTools.Workers) {
			t.Fatalf("started %v", got)
		}
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := g.Stop(stop); err != nil {
			t.Fatalf("stop: %v", err)
		}
	})

	t.Run("production refuses the development adapters of the M2 slices", func(t *testing.T) {
		if _, err := newDevM2(t, Production, devM2Inputs{
			Log: nil, Tuning: d.Tuning, Cursors: fxCursors(t), Tenants: d.Tenants, Authority: d.M2.In.Authority, Access: d.IDs, Repos: d.M2.In.Repos, Git: d.M2.In.Git,
		}); !errors.Is(err, ErrDevelopmentAdapter) {
			t.Fatalf("production composed the M2 fixtures: %v", err)
		}
		for name, adapter := range map[string]any{
			"gateway store": devGWStore{d.M2.GWStore}, "gateway provider": devGWProvider{d.M2.Provider}, "mcp store": devMCPStore{d.M2.MCPWorld.Store},
			"hook store": devHookStore{d.M2.HookStore}, "hook runs": devHookRuns{d.M2.HookRuns},
		} {
			if _, err := Admit(Production, PortStore, adapter); !errors.Is(err, ErrDevelopmentAdapter) {
				t.Errorf("production accepted the %s: %v", name, err)
			}
		}
	})

	t.Run("a missing collaborator is a construction error, never a default", func(t *testing.T) {
		base := ModelToolInputs{Env: Development, Tuning: d.Tuning, Cursors: fxCursors(t), Tenants: d.Tenants, Gateway: d.M2.Gateway, MCP: d.M2.MCP}
		for name, mutate := range map[string]func(*ModelToolInputs){
			"no gateway":  func(in *ModelToolInputs) { in.Gateway = nil },
			"no MCP":      func(in *ModelToolInputs) { in.MCP = nil },
			"no cursors":  func(in *ModelToolInputs) { in.Cursors = nil },
			"no tenants":  func(in *ModelToolInputs) { in.Tenants = nil },
			"no interval": func(in *ModelToolInputs) { in.Tuning.ReconcileInterval = 0 },
			"no env":      func(in *ModelToolInputs) { in.Env = "" },
		} {
			in := base
			mutate(&in)
			if _, err := AssembleModelTools(in); err == nil {
				t.Errorf("%s: assembled", name)
			}
		}
		if _, err := AssembleTriggers(TriggerInputs{Env: Development, Service: d.M2.Hook}); err == nil {
			t.Error("a trigger slice without a rate limit assembled")
		}
		if _, err := AssembleTriggers(TriggerInputs{Env: Development, RateLimitPerMinute: 10}); err == nil {
			t.Error("a trigger slice without a service assembled")
		}
		if _, _, err := NewPublicRouter(PublicInputs{}); err == nil {
			t.Error("a router without services was built")
		}
	})
}

// workloadBearer sets the workload's token on every request.
type workloadBearer struct{ token string }

func (b workloadBearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// ---- the contract ---------------------------------------------------------------

// TestM2ContractCompatibility (m2_contract_compatibility): the aggregate stays
// compatible with the clients written against the manual agent/delivery
// aggregate (AC-10), the public client is generated from it reproducibly, the
// operations M2 adds are served exactly as published and every served payload
// validates against its contract (AC-12).
func TestM2ContractCompatibility(t *testing.T) {
	root := repoRoot(t)
	apiDir := filepath.Join(root, "api")
	current, err := os.ReadFile(filepath.Join(apiDir, "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("every operation, parameter, response and member the baseline declared is still there", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join("testdata", "api-baseline-t072.json"))
		if err != nil {
			t.Fatal(err)
		}
		baseline, err := apispec.ParseSummary(raw)
		if err != nil {
			t.Fatal(err)
		}
		now, err := apispec.Summarize(current)
		if err != nil {
			t.Fatal(err)
		}
		if p := now.CompatibleWith(baseline, current); len(p) != 0 {
			t.Fatalf("the aggregate breaks supported clients:\n%s", strings.Join(p, "\n"))
		}
		// The M2 additions are real: new operations exist beyond the baseline.
		var added []string
		for k := range now.Operations {
			if _, known := baseline.Operations[k]; !known {
				added = append(added, k)
			}
		}
		// The business-loop composition (T-075) adds its own operations after M2; they
		// are checked by TestBusinessLoopContract.
		added = slices.DeleteFunc(added, isBusinessLoopOperation)
		// The kernel assembly's Git routes (T-170) are served by the kernel, not by this composition.
		added = slices.DeleteFunc(added, isKernelGitOperation)
		slices.Sort(added)
		want := []string{
			"DELETE /tenants/{tenantId}/projects/{projectId}/mcp-servers/{id}", "GET /tenants/{tenantId}/projects/{projectId}/mcp-servers",
			"GET /tenants/{tenantId}/projects/{projectId}/model-settings", "PATCH /tenants/{tenantId}/projects/{projectId}/mcp-servers/{id}",
			"POST /tenants/{tenantId}/projects/{projectId}/mcp-servers", "PUT /tenants/{tenantId}/projects/{projectId}/model-settings",
		}
		if !slices.Equal(added, want) {
			t.Fatalf("operations added by M2: %v, want %v", added, want)
		}
	})

	t.Run("a baseline break is detected", func(t *testing.T) {
		// Negative controls on the real document: each edit removes or tightens
		// something a baseline client relies on.
		raw, _ := os.ReadFile(filepath.Join("testdata", "api-baseline-t072.json"))
		baseline, _ := apispec.ParseSummary(raw)
		for name, edit := range map[string]func(string) string{
			"an operation removed": func(s string) string { return strings.Replace(s, "operationId: cancelRun", "operationId: abortRun", 1) },
			"a response member removed": func(s string) string {
				return strings.Replace(s, "        sourceCommit:\n", "        sourceCommitX:\n", 1)
			},
		} {
			mutated := edit(string(current))
			if mutated == string(current) {
				t.Fatalf("%s: the edit did not apply", name)
			}
			s, err := apispec.Summarize([]byte(mutated))
			if err != nil {
				t.Fatal(err)
			}
			if p := s.CompatibleWith(baseline, []byte(mutated)); len(p) == 0 {
				t.Errorf("%s: not detected", name)
			}
		}
	})

	t.Run("generation from the assembled document is reproducible", func(t *testing.T) {
		assertClientReproducible(t)
	})

	t.Run("a client tolerates additive response members", func(t *testing.T) {
		srv := httptestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"enabled":false,"stored":false,"version":1,"models":[],"recording":false,"endpoint":null,"credential":null,"limits":null,"updatedAt":null,"updatedBy":null,"addedByALaterServer":{"k":1}}`))
		}))
		c, err := client.NewClientWithResponses(srv)
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.GetModelSettingsWithResponse(context.Background(), "t_acme", "p_inv")
		if err != nil || got.JSON200 == nil || got.JSON200.Version != 1 {
			t.Fatalf("an unknown member broke the generated client: %v %s", err, got.Body)
		}
	})

	d := newDevSlice(t)
	ctx := context.Background()
	tenant, project := string(devTenant), string(devProject)
	admin := d.api(devAdminToken)
	public := compileFile(t, filepath.Join(apiDir, "openapi.yaml"), true)

	t.Run("the M1 client flow runs unchanged on the M2 composition", func(t *testing.T) {
		// The submission/status/idempotency flow of TestM1GeneratedClient, on the
		// composition that now also serves the M2 groups.
		w := d.seedWorld("m2-compat", "echo ok")
		eng := d.api(devEngineerToken)
		r := d.createRun(eng, "m2-key-1", w, string(d.Repo.Initial))
		expectStatus(t, r, r.Body, http.StatusCreated, "create run")
		again := d.createRun(eng, "m2-key-1", w, string(d.Repo.Initial))
		if again.HTTPResponse.Header.Get("Idempotent-Replayed") != "true" || again.JSON201 == nil || again.JSON201.Id != r.JSON201.Id {
			t.Fatalf("replay: %s %s", again.HTTPResponse.Header, again.Body)
		}
		other := d.seedWorld("m2-compat-2", "echo other")
		if c := d.createRun(eng, "m2-key-1", other, string(d.Repo.Initial)); c.StatusCode() != http.StatusConflict || errorCode(c.Body) != "idempotency_conflict" {
			t.Fatalf("conflicting key: %d %s", c.StatusCode(), c.Body)
		}
	})

	t.Run("served M2 payloads validate against the published schemas", func(t *testing.T) {
		got, err := admin.GetModelSettingsWithResponse(ctx, tenant, project)
		if err != nil || got.StatusCode() != http.StatusOK {
			t.Fatalf("%v %d %s", err, got.StatusCode(), got.Body)
		}
		if err := public("#/components/schemas/ModelSettings", got.Body); err != nil {
			t.Errorf("the virtual settings: %v", err)
		}
		put, err := admin.ReplaceModelSettingsWithResponse(ctx, tenant, project, ifMatch(1), m2Settings(5))
		if err != nil || put.JSON200 == nil {
			t.Fatalf("%v %d %s", err, put.StatusCode(), put.Body)
		}
		if err := public("#/components/schemas/ModelSettings", put.Body); err != nil {
			t.Errorf("the stored settings: %v", err)
		}
		if err := public("#/components/schemas/ModelSettings", []byte(`{"enabled":"yes"}`)); err == nil {
			t.Error("the schema accepted a mistyped settings document: the check proves nothing")
		}
		ref, err := d.M2.MCPWorld.Secrets.Create(ctx, devScope, "remote-token", mcpmem.DefaultOrigin, "tok")
		if err != nil {
			t.Fatal(err)
		}
		reg, err := admin.CreateMcpServerWithResponse(ctx, tenant, project, client.McpServerDraft{
			Endpoint: mcpmem.DefaultEndpoint, Credential: client.CredentialRef{Name: ref.Name, Version: ref.Version}, Tools: []string{"echo"},
		})
		if err != nil || reg.JSON201 == nil {
			t.Fatalf("%v %d %s", err, reg.StatusCode(), reg.Body)
		}
		if err := public("#/components/schemas/McpServer", reg.Body); err != nil {
			t.Errorf("the registration: %v", err)
		}
		list, _ := admin.ListMcpServersWithResponse(ctx, tenant, project, &client.ListMcpServersParams{})
		if err := public("#/components/schemas/McpServerPage", list.Body); err != nil {
			t.Errorf("the registration page: %v", err)
		}
		if err := public("#/components/schemas/McpServer", []byte(`{"id":1}`)); err == nil {
			t.Error("the schema accepted a mistyped registration: the check proves nothing")
		}
		// The error envelope of a refused write is the public one.
		bad, _ := admin.CreateMcpServerWithResponse(ctx, tenant, project, client.McpServerDraft{Endpoint: "https://nope.example.invalid/mcp", Credential: client.CredentialRef{Name: ref.Name, Version: ref.Version}, Tools: []string{"echo"}})
		if bad.StatusCode() != http.StatusUnprocessableEntity {
			t.Fatalf("%d %s", bad.StatusCode(), bad.Body)
		}
		if err := public("#/components/schemas/ErrorEnvelope", bad.Body); err != nil {
			t.Errorf("the refusal envelope: %v", err)
		}
	})

	t.Run("served workload and hook payloads validate against their fragments", func(t *testing.T) {
		// api/gateway.yaml declares `finish_reason` as a nullable enum without null
		// in the enum, which OpenAPI 3.0.3 reads as "never null", yet the gateway
		// (like OpenAI) sends `"finish_reason": null` on content chunks. The
		// fragment is T-032's; until it is corrected the check validates against
		// the intended contract (null admitted) and the discrepancy is reported
		// in the T-039 handoff.
		src, err := os.ReadFile(filepath.Join(apiDir, "gateway.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		const nullableEnum = "enum: [stop, length, tool_calls, content_filter]"
		if !bytes.Contains(src, []byte(nullableEnum)) {
			t.Fatal("api/gateway.yaml changed: drop the finish_reason adjustment if the enum now admits null")
		}
		intended := filepath.Join(t.TempDir(), "gateway.yaml")
		if err := os.WriteFile(intended, bytes.Replace(src, []byte(nullableEnum), []byte("enum: [stop, length, tool_calls, content_filter, null]"), 1), 0o600); err != nil {
			t.Fatal(err)
		}
		gw := compileFile(t, intended, true)
		wl, bearer := d.M2.Workloads.Add(devScope)
		if r, _ := admin.ReplaceModelSettingsWithResponse(ctx, tenant, project, ifMatch(2), m2Settings(5)); r.StatusCode() != http.StatusOK {
			t.Fatalf("settings: %d %s", r.StatusCode(), r.Body)
		}
		d.M2.Provider.Script(m2Reply(devModel, 5, 2, "alpha", "beta"))
		status, _, out := d.send(http.MethodPost, "/gateway/v1/attempts/"+string(wl.AttemptID)+"/chat/completions", bearer, map[string]string{"Content-Type": "application/json"},
			`{"model":"`+devModel+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		if status != http.StatusOK {
			t.Fatalf("%d %s", status, out)
		}
		chunks := 0
		for _, line := range strings.Split(string(out), "\n") {
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok || data == "[DONE]" {
				continue
			}
			chunks++
			if err := gw("#/components/schemas/ChatCompletionChunk", []byte(data)); err != nil {
				t.Errorf("chunk %s: %v", data, err)
			}
		}
		if chunks == 0 {
			t.Fatal("no stream chunk was validated")
		}
		if err := gw("#/components/schemas/ChatCompletionChunk", []byte(`{"object":7}`)); err == nil {
			t.Error("the chunk schema accepted a mistyped chunk: the check proves nothing")
		}

		hooks := compileFile(t, filepath.Join(apiDir, "git-webhooks.yaml"), true)
		secret := []byte(strings.Repeat("hook-secret-", 4))
		d.M2.HookSecrets.Put(devScope, "hook-secret", secret)
		d.M2.HookDefs.Register(devScope, "def_m2c")
		trig, err := d.M2.Hook.Install(ctx, gitwebhook.InstallCommand{Scope: devScope, Principal: devLead, DefinitionID: "def_m2c", Branches: []string{strings.TrimPrefix(d.Repo.BaseRef, "refs/heads/")}, SecretRef: "hook-secret"})
		if err != nil {
			t.Fatal(err)
		}
		body := gitwebhooktest.Marshal(t, gitwebhook.Payload{SchemaVersion: gitwebhook.SchemaVersion, Repository: "repo_dev", Ref: d.Repo.BaseRef, Before: gitwebhooktest.Zero, After: string(d.Repo.Initial)})
		if err := hooks("#/components/schemas/PushNotification", body); err != nil {
			t.Errorf("the notification the trigger is sent: %v", err)
		}
		ts := time.Now().Unix()
		hs, _, hb := d.send(http.MethodPost, "/api/v1/git-hooks/"+string(trig.ID), "", map[string]string{
			gitwebhook.HeaderDeliveryID: "dlv-c1", gitwebhook.HeaderTimestamp: strconv.FormatInt(ts, 10),
			gitwebhook.HeaderSignature: gitwebhook.Sign(secret, trig.ID, "dlv-c1", ts, body),
		}, string(body))
		if hs != http.StatusAccepted {
			t.Fatalf("%d %s", hs, hb)
		}
		if err := hooks("#/components/schemas/PushReceipt", hb); err != nil {
			t.Errorf("the receipt: %v", err)
		}
		var env struct {
			Error struct{ Code string }
		}
		_, _, bad := d.send(http.MethodPost, "/api/v1/git-hooks/"+string(trig.ID), "", nil, string(body))
		if json.Unmarshal(bad, &env) != nil || env.Error.Code != "invalid_credential" {
			t.Fatalf("an unsigned notification: %s", bad)
		}
		if err := public("#/components/schemas/ErrorEnvelope", bad); err != nil {
			t.Errorf("the hook error envelope: %v", err)
		}
	})

	t.Run("the aggregate document stays free of workload and hook interfaces", func(t *testing.T) {
		for _, banned := range []string{"/gateway/v1", "/internal/mcp", "/git-hooks", "ChatCompletion", "attemptToken", "McpSessionId"} {
			if bytes.Contains(current, []byte(banned)) {
				t.Errorf("the public document mentions %q", banned)
			}
		}
	})
}

// httptestServer serves h on a loopback listener and returns its URL.
func httptestServer(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}
