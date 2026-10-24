package m2_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/agents"
	kit "github.com/optimiware/kodama/test/e2e/m2/kit"
)

// TestM2ProfileAuthority covers the registration-time half of the agent and
// gateway contracts over the real database and the public API: only a pinned,
// compatibility-qualified wrapper/profile registers (AR-A01), a profile can name
// only the model and tools its own project enabled (AR-A02, GM-A01: another
// tenant's references, credentials and profiles are not reachable), the
// reference an agent step names is the immutable name@version, and withdrawing
// the project's model approval stops admission of a run of the profile without
// touching a run that already pinned it. Covers AR-A01, AR-A02, AR-01, AR-02,
// AR-05, GM-A01, GM-01, GM-02, GM-12 (the frozen protocol revision only) and
// AR-12's registration side (no workload, service or operator actor registers).
func TestM2ProfileAuthority(t *testing.T) {
	e := kit.New(t, kit.Options{})
	post := func(persona string, d agents.Draft) (int, string) {
		code, b := e.Do(persona, "POST", e.ProjectPath()+"/agent-profiles", d, nil)
		return code, kit.ErrorCode(b)
	}
	base := e.ProfileDraft("stub-coder", nil)

	// ---- AR-A01: the pinned tuple registers, anything else does not ----
	if code, _ := post("engineer", base); code != http.StatusOK { // identical content: the stored version
		t.Fatalf("re-registering the pinned profile: %d", code)
	}
	for name, mutate := range map[string]func(*agents.Draft){
		"an unknown wrapper version": func(d *agents.Draft) { d.Wrapper.Version = "9.9.9" },
		"an unknown wrapper":         func(d *agents.Draft) { d.Wrapper.ID = "other-wrapper" },
		"another harness version":    func(d *agents.Draft) { d.Wrapper.Harness.Version = "2.0.0" },
		"a changed wrapper command":  func(d *agents.Draft) { d.Wrapper.Command = []string{"/bin/sh", "-c", "exec /bin/sh"} },
		"an image outside the tuple": func(d *agents.Draft) { d.Image = "docker.io/library/busybox@sha256:" + strings.Repeat("a", 64) },
		"a name not yet qualified":   func(d *agents.Draft) { d.Name = "other"; d.Wrapper.Version = "0.0.1" },
	} {
		d := e.ProfileDraft("stub-coder", nil)
		mutate(&d)
		if code, c := post("engineer", d); code != http.StatusUnprocessableEntity || c != "profile_unsupported" {
			t.Fatalf("%s registered or failed wrongly: %d %s", name, code, c)
		}
	}
	// A claimed limit the installation does not enforce for the tuple is unsupported too
	// is covered by the control list; an unapproved model or tool is invalid:
	for name, mutate := range map[string]func(*agents.Draft){
		"a model the project did not enable": func(d *agents.Draft) { d.Model.Name = "other-model" },
		"another endpoint":                   func(d *agents.Draft) { d.Model.Endpoint = "https://models.example.invalid/v1" },
		"a tool the registration hides":      func(d *agents.Draft) { d.Tools = []string{e.Registration + "/admin_secret"} },
		"a bare tool name":                   func(d *agents.Draft) { d.Tools = []string{"echo"} },
		"a wildcard tool":                    func(d *agents.Draft) { d.Tools = []string{e.Registration + "/*"} },
	} {
		d := e.ProfileDraft("stub-coder", nil)
		mutate(&d)
		if code, c := post("engineer", d); code != http.StatusUnprocessableEntity || (c != "invalid_profile") {
			t.Fatalf("%s: %d %s", name, code, c)
		}
	}
	// Who registers: run:manage only (no viewer, reviewer or administrator).
	for _, p := range []string{"viewer", "reviewer", "admin", "outsider"} {
		want := http.StatusForbidden
		if p == "outsider" {
			want = http.StatusNotFound
		}
		if code, _ := post(p, base); code != want {
			t.Fatalf("%s registered or learned something: %d", p, code)
		}
	}

	// ---- GM-A01 / AR-A02: another tenant reaches none of it ----
	bPath := "/api/v1/tenants/" + kit.TenantB + "/projects/" + e.World.ProjectB
	if code, b := e.Do("analyst", "GET", bPath+"/agent-profiles", nil, nil); code != http.StatusOK || strings.Contains(string(b), "stub-coder") {
		t.Fatalf("tenant B lists %d %s", code, b)
	}
	if code, _ := e.Do("analyst", "POST", bPath+"/agent-profiles", base, nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("tenant B registered a profile naming tenant A's registration and model: %d", code)
	}
	if code, _ := e.Do("foreign", "POST", bPath+"/mcp-servers", map[string]any{"endpoint": e.MCP.URL(), "credential": map[string]any{"name": kit.MCPSecret, "version": 1}, "tools": []string{"echo"}}, nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("tenant B registered an MCP server with tenant A's credential name: %d", code)
	}
	if code, b := e.Do("foreign", "PUT", bPath+"/model-settings", map[string]any{
		"enabled": true, "endpoint": e.Provider.Endpoint(), "credential": map[string]any{"ref": "secret://project/" + kit.ModelSecret, "version": 1},
		"models": []string{kit.ModelName}, "limits": kit.DefaultLimits,
	}, map[string]string{"If-Match": `"1"`}); code != http.StatusConflict || kit.ErrorCode(b) != "credential_unusable" {
		t.Fatalf("tenant B enabled models with tenant A's credential name: %d %s", code, b)
	}
	// A pipeline of tenant B naming tenant A's immutable reference is refused at registration.
	doc := map[string]any{"apiVersion": "kodama/v1alpha1", "kind": "Pipeline", "metadata": map[string]any{"name": "foreign-ref"},
		"spec": map[string]any{"inputs": map[string]any{}, "steps": []any{map[string]any{"id": "implement", "kind": "agent", "agent": map[string]any{"profile": e.Profile}, "timeoutSeconds": 60, "workspace": map[string]any{"access": "none"}}}}}
	if code, b := e.Do("analyst", "POST", bPath+"/pipeline-definitions", doc, nil); code != http.StatusUnprocessableEntity || !strings.Contains(string(b), `"unknown_profile"`) {
		t.Fatalf("tenant B referenced tenant A's profile: %d %s", code, b)
	}

	// ---- the reference an agent step names is immutable name@version ----
	alias := func(ref string) (int, []byte) {
		d := map[string]any{"apiVersion": "kodama/v1alpha1", "kind": "Pipeline", "metadata": map[string]any{"name": "ref-" + strings.NewReplacer("@", "-", ".", "-").Replace(ref)},
			"spec": map[string]any{"inputs": map[string]any{}, "steps": []any{map[string]any{"id": "implement", "kind": "agent", "agent": map[string]any{"profile": ref}, "timeoutSeconds": 60, "workspace": map[string]any{"access": "none"}}}}}
		return e.Do("engineer", "POST", e.ProjectPath()+"/pipeline-definitions", d, nil)
	}
	name := strings.SplitN(e.Profile, "@", 2)[0]
	for _, ref := range []string{name, name + "@99", "no-such@1"} {
		if code, b := alias(ref); code != http.StatusUnprocessableEntity || !strings.Contains(string(b), "_profile") {
			t.Fatalf("the reference %q was accepted: %d %s", ref, code, b)
		}
	}
	if code, b := alias(e.Profile); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("the immutable reference %q was refused: %d %s", e.Profile, code, b)
	}

	// ---- withdrawing the model approval stops admission, not a pinned run ----
	def := e.Register("withdraw", kit.AgentInputs(), e.AgentStep("implement"))
	item := e.Item("Approval withdrawn", "# spec")
	pinned := e.StartRun("withdraw-1", item, def.Id, e.AgentRunInputs("completed"))
	if f := e.World.WaitRun(pinned.Id, 3*time.Minute); f.Status != client.RunStatusSucceeded {
		t.Fatalf("run %s (%s)", f.Status, f.Reason)
	}
	cur := e.SettingsVersion()
	e.ReplaceSettings(map[string]any{"enabled": false, "endpoint": e.Provider.Endpoint(), "credential": map[string]any{"ref": "secret://project/" + kit.ModelSecret, "version": 1},
		"models": []string{kit.ModelName}, "limits": kit.DefaultLimits}, cur)
	code, body := e.TryRun("withdraw-2", item, def.Id, e.AgentRunInputs("completed"))
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("a run of a profile whose model approval was withdrawn was admitted: %d %s", code, body)
	}
	if got := e.World.GetRun(pinned.Id); got.Status != client.RunStatusSucceeded {
		t.Fatalf("the finished run changed: %s", got.Status)
	}
	e.ReplaceSettings(map[string]any{"enabled": true, "endpoint": e.Provider.Endpoint(), "credential": map[string]any{"ref": "secret://project/" + kit.ModelSecret, "version": 1},
		"models": []string{kit.ModelName}, "limits": kit.DefaultLimits}, e.SettingsVersion())
	again := e.StartRun("withdraw-3", item, def.Id, e.AgentRunInputs("completed"))
	if f := e.World.WaitRun(again.Id, 3*time.Minute); f.Status != client.RunStatusSucceeded {
		t.Fatalf("after restoring the approval: %s (%s)", f.Status, f.Reason)
	}
	// The provider credential never appears in a stored profile or a settings response.
	var raw []byte
	code, raw = e.Do("admin", "GET", e.ProjectPath()+"/model-settings", nil, nil)
	if code != http.StatusOK || strings.Contains(string(raw), kit.ProviderKey) {
		t.Fatalf("model settings %d %s", code, raw)
	}
	_ = json.Marshal
}
