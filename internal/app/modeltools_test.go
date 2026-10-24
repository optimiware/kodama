package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gateway"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook"
)

func writeModelTools(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "model-tools.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const modelToolsDoc = `{
  "gateway": {"origins": [{"origin": "https://models.example", "pathPrefix": "/v1"}], "models": {"gpt-a": {}, "gpt-b": {"bytesPerToken": 4, "requestOverhead": 8, "messageOverhead": 4, "toolOverhead": 8}}},
  "mcp": {"allowedHosts": ["mcp.example", "mcp.internal:8443"]},
  "catalogue": [{"image": "registry.example/agent@sha256:1111111111111111111111111111111111111111111111111111111111111111",
    "wrapperId": "kodama-opencode", "wrapperVersion": "1.0.0", "command": ["/usr/local/bin/wrapper"], "harnessId": "opencode", "harnessVersion": "2.1.2",
    "controls": ["model", "requests"], "evidence": "t085-smoke"}]
}`

func TestModelToolsFileIsStrictAndSelectsNothingByDefault(t *testing.T) {
	var zero ModelToolsConfig
	if zero.Configured() {
		t.Fatal("the zero configuration selects model access")
	}
	if err := zero.Validate(Production); err != nil {
		t.Fatalf("the empty configuration is valid (and approves nothing): %v", err)
	}
	if cat, err := zero.catalogue(); err != nil || cat == nil {
		t.Fatalf("an empty catalogue: %v", err)
	}
	c, err := LoadModelTools(writeModelTools(t, modelToolsDoc))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Configured() || c.Validate(Production) != nil || c.Validate(Test) != nil {
		t.Fatalf("a complete statement was refused: %v", c.Validate(Production))
	}
	est, err := c.estimator()
	if err != nil || est == nil {
		t.Fatalf("estimator: %v", err)
	}
	for name, body := range map[string]string{
		"unknown field":          `{"gateway":{"origins":[],"trust":true}}`,
		"trailing content":       modelToolsDoc + `{}`,
		"not an object":          `[]`,
		"unknown catalogue key":  `{"catalogue":[{"image":"x","approved":true}]}`,
		"unknown accounting key": `{"gateway":{"origins":[{"origin":"https://m.example"}],"models":{"a":{"discount":1}}}}`,
	} {
		if _, err := LoadModelTools(writeModelTools(t, body)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := LoadModelTools(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("a missing file was accepted")
	}
}

func TestModelToolsValidation(t *testing.T) {
	base := func() ModelToolsConfig {
		c, err := LoadModelTools(writeModelTools(t, modelToolsDoc))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	for name, tc := range map[string]struct {
		env    Env
		mutate func(*ModelToolsConfig)
		want   string
	}{
		"models without an origin":    {Test, func(c *ModelToolsConfig) { c.Gateway.Origins = nil }, "no approved origin"},
		"an origin without models":    {Test, func(c *ModelToolsConfig) { c.Gateway.Models = nil }, "accounting entry"},
		"an insecure origin":          {Test, func(c *ModelToolsConfig) { c.Gateway.Origins[0].Origin = "http://models.example" }, "origin"},
		"a catalogue without gateway": {Test, func(c *ModelToolsConfig) { c.Gateway = GatewayConfig{} }, "needs the model gateway"},
		"MCP settings without a host": {Test, func(c *ModelToolsConfig) { c.MCP = MCPConfig{AllowPrivate: true} }, "no approved host"},
		"a wildcard MCP host":         {Test, func(c *ModelToolsConfig) { c.MCP.AllowedHosts = []string{"*.example"} }, "invalid approved destination"},
		"an unknown control":          {Test, func(c *ModelToolsConfig) { c.Catalogue[0].Controls = []string{"root"} }, "at controls"},
		"catalogue without evidence":  {Test, func(c *ModelToolsConfig) { c.Catalogue[0].Evidence = "" }, "evidence"},
		"an unpinned image":           {Test, func(c *ModelToolsConfig) { c.Catalogue[0].Image = "registry.example/agent:latest" }, "tuple"},
		"a duplicated tuple":          {Test, func(c *ModelToolsConfig) { c.Catalogue = append(c.Catalogue, c.Catalogue[0]) }, "duplicate tuple"},
		"a malformed network":         {Test, func(c *ModelToolsConfig) { c.Gateway.AllowNetworks = []string{"10.0.0.1"} }, "allowed network"},
		"a loopback network":          {Production, func(c *ModelToolsConfig) { c.Gateway.AllowNetworks = []string{"127.0.0.0/8"} }, "test fixture"},
		"the whole address space":     {Production, func(c *ModelToolsConfig) { c.Gateway.AllowNetworks = []string{"0.0.0.0/0"} }, "test fixture"},
	} {
		c := base()
		tc.mutate(&c)
		err := c.Validate(tc.env)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v (want %q)", name, err, tc.want)
		}
	}
	// A private provider network is a legitimate operator choice in production;
	// loopback is the fixture the test environment may name.
	c := base()
	c.Gateway.AllowNetworks = []string{"10.20.0.0/16"}
	if err := c.Validate(Production); err != nil {
		t.Errorf("a private provider network was refused: %v", err)
	}
	c.Gateway.AllowNetworks = []string{"127.0.0.0/8"}
	if err := c.Validate(Test); err != nil {
		t.Errorf("a loopback fixture network was refused outside production: %v", err)
	}
	c.Gateway.AllowNetworks = []string{"127.0.0.0/8"}
	if err := c.Validate(Production); !errors.Is(err, ErrDevelopmentAdapter) {
		t.Errorf("production did not name its refusal of a loopback provider network: %v", err)
	}
}

func TestCoreConfigLoadsAndValidatesTheModelToolsStatement(t *testing.T) {
	cfg := validConfig()
	c, err := LoadModelTools(writeModelTools(t, modelToolsDoc))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ModelTools = c
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a complete statement: %v", err)
	}
	half := cfg
	half.ModelTools.MCP = MCPConfig{}
	if err := half.Validate(); err == nil || !strings.Contains(err.Error(), "selected together") {
		t.Fatalf("a gateway without an MCP host: %v", err)
	}
	half = cfg
	half.ModelTools.Gateway = GatewayConfig{}
	half.ModelTools.Catalogue = nil
	if err := half.Validate(); err == nil || !strings.Contains(err.Error(), "selected together") {
		t.Fatalf("an MCP host without a gateway: %v", err)
	}
	prod := cfg
	prod.Env = Production
	prod.ModelTools.Gateway.AllowNetworks = []string{"127.0.0.1/32"}
	if err := prod.Validate(); !errors.Is(err, ErrDevelopmentAdapter) {
		t.Fatalf("production accepted a loopback provider network: %v", err)
	}
}

func TestApprovalsFailClosed(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inv"}
	model := agents.Model{Name: "gpt-a", Endpoint: "https://models.example/v1"}

	// Before the gateway and the proxy are built, nothing is approved.
	late := &LateApprovals{}
	if err := late.ApproveModel(ctx, scope, model); !errors.Is(err, agents.ErrDenied) {
		t.Fatalf("an unbound approval approved a model: %v", err)
	}
	if err := late.ApproveTools(ctx, scope, []string{"a/b"}); !errors.Is(err, agents.ErrDenied) {
		t.Fatalf("an unbound approval approved a tool: %v", err)
	}
	if err := (ComposedApprovals{}).ApproveModel(ctx, scope, model); !errors.Is(err, agents.ErrDenied) {
		t.Fatalf("no model half: %v", err)
	}
	if err := (ComposedApprovals{}).ApproveTools(ctx, scope, []string{"a/b"}); !errors.Is(err, agents.ErrDenied) {
		t.Fatalf("no tool half: %v", err)
	}
	if err := (ComposedApprovals{}).ApproveTools(ctx, scope, nil); err != nil {
		t.Fatalf("a profile that names no tool needs none approved: %v", err)
	}
	if _, err := NewModelApprovals(nil); err == nil {
		t.Fatal("approvals without a settings reader were built")
	}

	ep, err := gateway.ParseEndpoint("https://models.example/v1")
	if err != nil {
		t.Fatal(err)
	}
	stored := gateway.ModelSettings{Scope: scope, Revision: 2, Enabled: true, Endpoint: ep, Models: []string{"gpt-a"}}
	settings := &fakeSettings{st: stored}
	approvals, err := NewModelApprovals(settings)
	if err != nil {
		t.Fatal(err)
	}
	late.Bind(ComposedApprovals{Models: approvals})
	if err := late.ApproveModel(ctx, scope, model); err != nil {
		t.Fatalf("the enabled model: %v", err)
	}
	// A trailing slash on the profile's endpoint is the same endpoint; nothing
	// looser is.
	if err := approvals.ApproveModel(ctx, scope, agents.Model{Name: "gpt-a", Endpoint: "https://models.example/v1/"}); err != nil {
		t.Fatalf("a trailing slash: %v", err)
	}
	for name, m := range map[string]agents.Model{
		"another model":    {Name: "gpt-b", Endpoint: model.Endpoint},
		"another endpoint": {Name: "gpt-a", Endpoint: "https://models.example/v2"},
		"another origin":   {Name: "gpt-a", Endpoint: "https://models.example.evil/v1"},
		"a prefix":         {Name: "gpt-a", Endpoint: "https://models.example/v1/x"},
	} {
		if err := approvals.ApproveModel(ctx, scope, m); !errors.Is(err, agents.ErrDenied) {
			t.Errorf("%s: %v", name, err)
		}
	}
	settings.st.Enabled = false
	if err := approvals.ApproveModel(ctx, scope, model); !errors.Is(err, agents.ErrDenied) {
		t.Fatalf("disabled settings approved a model: %v", err)
	}
	settings.st = gateway.Disabled(scope)
	if err := approvals.ApproveModel(ctx, scope, model); !errors.Is(err, agents.ErrDenied) {
		t.Fatalf("the virtual settings approved a model: %v", err)
	}
	for _, err := range []error{gateway.ErrNotFound, gateway.ErrHidden} {
		settings.err = err
		if got := approvals.ApproveModel(ctx, scope, model); !errors.Is(got, agents.ErrDenied) {
			t.Errorf("%v: %v", err, got)
		}
	}
	settings.err = errors.New("database down")
	if err := approvals.ApproveModel(ctx, scope, model); !errors.Is(err, agents.ErrUnavailable) || errors.Is(err, agents.ErrDenied) {
		t.Fatalf("an outage must be unavailable, never an approval or a clean denial: %v", err)
	}
}

type fakeSettings struct {
	st  gateway.ModelSettings
	err error
}

func (f *fakeSettings) Settings(context.Context, domain.Scope) (gateway.ModelSettings, error) {
	return f.st, f.err
}

func TestTriggerAuthorizerMapsCurrentAuthority(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inv"}
	var asked []identity.Permission
	var target identity.Target
	var actor identity.Actor
	answer := error(nil)
	auth := &fakeAuthority{fn: func(a identity.Actor, tg identity.Target, p identity.Permission) error {
		asked, target, actor = append(asked, p), tg, a
		return answer
	}}
	a, err := NewTriggerAuthorizer(auth)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Authorize(ctx, "u_lead", scope, gitwebhook.ActionInstall); err != nil {
		t.Fatal(err)
	}
	if err := a.Authorize(ctx, "u_lead", scope, gitwebhook.ActionRun); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(asked, []identity.Permission{identity.PermProjectSettings, identity.PermRunManage}) {
		t.Fatalf("asked %v", asked)
	}
	if target.TenantID != scope.TenantID || target.ProjectID != scope.ProjectID || actor.ID != "u_lead" || actor.Type != identity.ActorUser {
		t.Fatalf("the question: %+v %+v", actor, target)
	}
	if err := a.Authorize(ctx, "u_lead", scope, gitwebhook.Action("git_trigger.sudo")); !errors.Is(err, gitwebhook.ErrDenied) {
		t.Fatalf("an unknown action must deny: %v", err)
	}
	for in, want := range map[error]error{identity.ErrHidden: gitwebhook.ErrNotFound, identity.ErrDenied: gitwebhook.ErrDenied} {
		answer = in
		if err := a.Authorize(ctx, "u_x", scope, gitwebhook.ActionRun); !errors.Is(err, want) {
			t.Errorf("%v -> %v", in, err)
		}
	}
	outage := errors.New("identity outage")
	answer = outage
	if err := a.Authorize(ctx, "u_x", scope, gitwebhook.ActionRun); !errors.Is(err, outage) || errors.Is(err, gitwebhook.ErrDenied) {
		t.Fatalf("an outage is returned as such, never as access or a denial: %v", err)
	}
	if _, err := NewTriggerAuthorizer(nil); err == nil {
		t.Fatal("an authorizer without authority was built")
	}
}

func TestDisabledCapabilitiesFollowTheM2Slices(t *testing.T) {
	slice := &ExecutionSlice{Disabled: []string{"runtime-secret-resolution", "gates", "agents", "delivery", "cleanup-runs", "git-webhooks", "chatops"}}
	delivery := &DeliverySlice{Disabled: deliveryUnavailableM2(false, true, true)}
	none := DisabledCapabilities(slice, delivery)
	if !slices.Contains(none, "git-webhooks") || !slices.Contains(none, "chatops") {
		t.Fatalf("without the trigger slice the hook is disabled: %v", none)
	}
	with := DisabledCapabilitiesM2(slice, delivery, M2Slices{Triggers: &TriggerSlice{}, ModelTools: &ModelToolSlice{}})
	if slices.Contains(with, "git-webhooks") || !slices.Contains(with, "chatops") {
		t.Fatalf("the trigger slice activates git-webhooks only: %v", with)
	}
	// The delivery slice's own refusals follow what is wired and qualified.
	for _, tc := range []struct {
		modelTools, qualified bool
		want, absent          []string
	}{
		{false, false, []string{"agent-steps", "model-gateway", "mcp-proxy"}, nil},
		{false, true, []string{"agent-steps", "model-gateway", "mcp-proxy"}, nil},
		{true, false, []string{"agent-steps"}, []string{"model-gateway", "mcp-proxy"}},
		{true, true, nil, []string{"agent-steps", "model-gateway", "mcp-proxy"}},
	} {
		got := deliveryUnavailableM2(true, tc.modelTools, tc.qualified)
		for _, w := range tc.want {
			if !slices.Contains(got, w) {
				t.Errorf("wired=%v qualified=%v: %q is missing from %v", tc.modelTools, tc.qualified, w, got)
			}
		}
		for _, a := range tc.absent {
			if slices.Contains(got, a) {
				t.Errorf("wired=%v qualified=%v: %q must not be listed: %v", tc.modelTools, tc.qualified, a, got)
			}
		}
		for _, always := range []string{"deployment-gates", "agent-candidate-gates"} {
			if !slices.Contains(got, always) {
				t.Errorf("%q is a standing refusal: %v", always, got)
			}
		}
	}
	if !slices.Equal(deliveryUnavailable(false), deliveryUnavailableM2(false, false, false)) {
		t.Fatal("the M1 refusal list changed")
	}
}
