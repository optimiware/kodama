package kit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/agents/agentstest"
	"github.com/optimiware/kodama/internal/mcpproxy/adapters"
)

// Secret names of the installation's provider and remote credentials.
const (
	ModelSecret = "provider-key"
	MCPSecret   = "remote-token"
	// ProviderKey is the provider credential's value: a sentinel no response,
	// event, log or audit may ever contain.
	ProviderKey = "sk-t038-PROVIDER-KEY-SENTINEL-77aa"
)

// AddMember binds the persona to the project with the role.
func (e *Env) AddMember(persona, role string) {
	t := e.T
	t.Helper()
	me, err := e.API(persona).GetMeWithResponse(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	Expect(t, me, me.Body, http.StatusOK, "me "+persona)
	pid := e.World.Project
	r, err := e.API("admin").CreateMembershipWithResponse(context.Background(), TenantA, client.CreateMembership{UserId: me.JSON200.Id, Role: client.Role(role), ProjectId: &pid})
	if err != nil {
		t.Fatal(err)
	}
	Expect(t, r, r.Body, http.StatusCreated, "membership "+role)
}

// ProjectPath is the project's API path.
func (e *Env) ProjectPath() string {
	return fmt.Sprintf("/api/v1/tenants/%s/projects/%s", TenantA, e.World.Project)
}

// Do issues a JSON request as the persona against the project and returns the
// status and body.
func (e *Env) Do(persona, method, path string, body any, headers map[string]string) (int, []byte) {
	t := e.T
	t.Helper()
	var rd = (*strings.Reader)(nil)
	var resp *http.Response
	var b []byte
	h := map[string]string{"Content-Type": "application/json"}
	for k, v := range headers {
		h[k] = v
	}
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = strings.NewReader(string(raw))
		resp, b = e.Raw(method, path, e.Token(persona), rd, h)
	} else {
		resp, b = e.Raw(method, path, e.Token(persona), nil, h)
	}
	return resp.StatusCode, b
}

// MustDo is Do requiring a status.
func (e *Env) MustDo(persona, method, path string, body any, headers map[string]string, want ...int) []byte {
	e.T.Helper()
	code, b := e.Do(persona, method, path, body, headers)
	if !slices.Contains(want, code) {
		e.T.Fatalf("%s %s as %s: HTTP %d, want %v: %s", method, path, persona, code, want, b)
	}
	return b
}

// CreateSecret creates a project secret as the administrator and returns its
// current version (1).
func (e *Env) CreateSecret(name, kindName string, value []byte) {
	e.T.Helper()
	e.MustDo("admin", "POST", e.ProjectPath()+"/secrets", map[string]any{"name": name, "kind": kindName, "value": base64.StdEncoding.EncodeToString(value)},
		map[string]string{"Idempotency-Key": "t038-secret-" + name}, http.StatusCreated)
}

// configureModelTools stores the project's model settings, the provider and
// remote credentials and the MCP registration, all through the public API.
func (e *Env) configureModelTools(o Options) {
	t := e.T
	t.Helper()
	pk, _ := json.Marshal(map[string]any{"version": 1, "bearer": ProviderKey, "origin": e.Provider.Origin()})
	e.CreateSecret(ModelSecret, "model", pk)
	e.CreateSecret(MCPSecret, "mcp", adapters.EncodeCredential(e.MCP.Origin(), e.MCP.Token()))
	e.SetModelLimits(o.Limits)

	b := e.MustDo("admin", "POST", e.ProjectPath()+"/mcp-servers", map[string]any{
		"endpoint": e.MCP.URL(), "credential": map[string]any{"name": MCPSecret, "version": 1}, "tools": []string{"echo", "mutate"},
	}, nil, http.StatusCreated)
	var reg struct{ Id string }
	if err := json.Unmarshal(b, &reg); err != nil || reg.Id == "" {
		t.Fatalf("mcp registration %s: %v", b, err)
	}
	e.Registration = reg.Id
}

// SetModelLimits stores the project's model settings with the limits (nil: the
// defaults), as the administrator.
func (e *Env) SetModelLimits(l *ModelLimits) {
	t := e.T
	t.Helper()
	if l == nil {
		d := DefaultLimits
		l = &d
	}
	version := "1"
	if code, b := e.Do("admin", "GET", e.ProjectPath()+"/model-settings", nil, nil); code == http.StatusOK {
		var cur struct{ Version int64 }
		if err := json.Unmarshal(b, &cur); err == nil && cur.Version > 0 {
			version = fmt.Sprint(cur.Version)
		}
	}
	e.MustDo("admin", "PUT", e.ProjectPath()+"/model-settings", map[string]any{
		"enabled": true, "endpoint": e.Provider.Endpoint(), "credential": map[string]any{"ref": "secret://project/" + ModelSecret, "version": 1},
		"models": []string{ModelName}, "limits": l,
	}, map[string]string{"If-Match": `"` + version + `"`}, http.StatusOK, http.StatusCreated)
}

// ProfileDraft is the registered profile: the deterministic stub's pinned
// tuple, the project's model and one tool of the registration.
func (e *Env) ProfileDraft(name string, limits *agents.Limits) agents.Draft {
	d := agentstest.Draft(e.T, name)
	d.Image = PodmanImage
	d.Wrapper = agents.Wrapper{ID: WrapperID, Version: WrapperVersion, Command: WrapperCommand(), Harness: agents.Harness{ID: HarnessID, Version: HarnessVersion}}
	d.Instructions = Instructions(e.Front.Origin)
	d.Model = agents.Model{Name: ModelName, Endpoint: e.Provider.Endpoint()}
	d.Tools = []string{e.Registration + "/echo"}
	d.Transcript = agents.Transcript{Record: false}
	calls := int64(10)
	d.Limits = agents.Limits{WallSeconds: 300, MaxToolCalls: &calls}
	if limits != nil {
		d.Limits = *limits
	}
	return d
}

// RegisterProfile registers the agent profile as the engineer and keeps its
// immutable reference.
func (e *Env) RegisterProfile(limits *agents.Limits) string {
	t := e.T
	t.Helper()
	ref := e.RegisterDraft(e.ProfileDraft("stub-coder", limits))
	e.Profile = ref
	return ref
}

// RegisterDraft registers a draft as the engineer and returns name@version.
func (e *Env) RegisterDraft(d agents.Draft) string {
	t := e.T
	t.Helper()
	b := e.MustDo("engineer", "POST", e.ProjectPath()+"/agent-profiles", d, nil, http.StatusCreated, http.StatusOK)
	var p struct{ Reference string }
	if err := json.Unmarshal(b, &p); err != nil || p.Reference == "" {
		t.Fatalf("profile registration %s: %v", b, err)
	}
	return p.Reference
}

var _ testing.TB

// UserID is the persona's user identifier.
func (e *Env) UserID(persona string) string {
	t := e.T
	t.Helper()
	me, err := e.API(persona).GetMeWithResponse(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	Expect(t, me, me.Body, http.StatusOK, "me "+persona)
	return me.JSON200.Id
}

// RemoveMember deletes the persona's project binding as the administrator (the
// revocation of a reviewer) and returns the removed membership's role.
func (e *Env) RemoveMember(persona string) {
	t := e.T
	t.Helper()
	ctx := context.Background()
	uid := e.UserID(persona)
	pid := e.World.Project
	list, err := e.API("admin").ListMembershipsWithResponse(ctx, TenantA, &client.ListMembershipsParams{ProjectId: &pid, UserId: &uid})
	if err != nil {
		t.Fatal(err)
	}
	Expect(t, list, list.Body, http.StatusOK, "list memberships")
	if len(list.JSON200.Items) != 1 {
		t.Fatalf("%s has %d bindings", persona, len(list.JSON200.Items))
	}
	m := list.JSON200.Items[0]
	del, err := e.API("admin").DeleteMembershipWithResponse(ctx, TenantA, m.Id, &client.DeleteMembershipParams{IfMatch: fmt.Sprintf(`"%d"`, m.Version)})
	if err != nil {
		t.Fatal(err)
	}
	if del.StatusCode() != http.StatusNoContent && del.StatusCode() != http.StatusOK {
		t.Fatalf("delete membership: HTTP %s: %s", del.Status(), del.Body)
	}
}

// SettingsVersion is the project's current model settings version.
func (e *Env) SettingsVersion() int64 {
	e.T.Helper()
	b := e.MustDo("admin", "GET", e.ProjectPath()+"/model-settings", nil, nil, http.StatusOK)
	var cur struct{ Version int64 }
	if err := json.Unmarshal(b, &cur); err != nil {
		e.T.Fatal(err)
	}
	return cur.Version
}

// ReplaceSettings replaces the model settings at the version.
func (e *Env) ReplaceSettings(body map[string]any, version int64) {
	e.T.Helper()
	e.MustDo("admin", "PUT", e.ProjectPath()+"/model-settings", body, map[string]string{"If-Match": fmt.Sprintf(`"%d"`, version)}, http.StatusOK, http.StatusCreated)
}

// IDOf reads the identifier of a created resource's response.
func IDOf(t testing.TB, body []byte) string {
	t.Helper()
	var r struct{ Id string }
	if err := json.Unmarshal(body, &r); err != nil || r.Id == "" {
		t.Fatalf("no id in %s: %v", body, err)
	}
	return r.Id
}

// Dump returns the first column of every row of an owner-role query, for
// diagnostics and for assertions over rows the API does not expose.
func (e *Env) Dump(sql string, args ...any) []string {
	e.T.Helper()
	rows, err := e.Owner.Query(context.Background(), sql, args...)
	if err != nil {
		e.T.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			e.T.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

// RegisterMCPServer registers another MCP server of the project (as the
// administrator) at the fixture's endpoint (or another approved one) with the
// tools it enables, and returns its registration identifier.
func (e *Env) RegisterMCPServer(endpoint string, tools ...string) string {
	e.T.Helper()
	b := e.MustDo("admin", "POST", e.ProjectPath()+"/mcp-servers", map[string]any{
		"endpoint": endpoint, "credential": map[string]any{"name": MCPSecret, "version": 1}, "tools": tools,
	}, nil, http.StatusCreated)
	return IDOf(e.T, b)
}

// RegisterProfileFor registers a profile named name whose tools are the
// qualified names given, and returns its immutable reference.
func (e *Env) RegisterProfileFor(name string, limits *agents.Limits, tools ...string) string {
	e.T.Helper()
	d := e.ProfileDraft(name, limits)
	d.Tools = tools
	return e.RegisterDraft(d)
}

// ForeignMCPRegistration registers an MCP server in tenant B's project (its own
// secret, the same approved remote) and returns the registration identifier.
func (e *Env) ForeignMCPRegistration() string {
	t := e.T
	t.Helper()
	path := "/api/v1/tenants/" + TenantB + "/projects/" + e.World.ProjectB
	e.MustDo("foreign", "POST", path+"/secrets", map[string]any{"name": MCPSecret, "kind": "mcp", "value": base64.StdEncoding.EncodeToString(adapters.EncodeCredential(e.MCP.Origin(), e.MCP.Token()))},
		map[string]string{"Idempotency-Key": "t038-foreign-secret"}, http.StatusCreated)
	b := e.MustDo("foreign", "POST", path+"/mcp-servers", map[string]any{
		"endpoint": e.MCP.URL(), "credential": map[string]any{"name": MCPSecret, "version": 1}, "tools": []string{"echo"},
	}, nil, http.StatusCreated)
	return IDOf(t, b)
}

// SecretID is the identifier of the project secret with the name.
func (e *Env) SecretID(name string) string {
	t := e.T
	t.Helper()
	b := e.MustDo("admin", "GET", e.ProjectPath()+"/secrets?limit=200", nil, nil, http.StatusOK)
	var page struct{ Items []struct{ Id, Name string } }
	if err := json.Unmarshal(b, &page); err != nil {
		t.Fatal(err)
	}
	for _, s := range page.Items {
		if s.Name == name {
			return s.Id
		}
	}
	t.Fatalf("no secret %s", name)
	return ""
}

// RevokeSecretVersion revokes one version of the secret as the administrator.
func (e *Env) RevokeSecretVersion(name string, version int) {
	e.T.Helper()
	e.MustDo("admin", "POST", fmt.Sprintf("%s/secrets/%s/versions/%d/revoke", e.ProjectPath(), e.SecretID(name), version),
		map[string]any{"reason": "t038 revocation"}, nil, http.StatusOK)
}

// EditEnvironment edits the environment as the engineer at its current version.
func (e *Env) EditEnvironment(id string, edit map[string]any) {
	t := e.T
	t.Helper()
	b := e.MustDo("engineer", "GET", e.ProjectPath()+"/environments/"+id, nil, nil, http.StatusOK)
	var cur struct{ Version int64 }
	if err := json.Unmarshal(b, &cur); err != nil {
		t.Fatal(err)
	}
	e.MustDo("engineer", "PATCH", e.ProjectPath()+"/environments/"+id, edit, map[string]string{"If-Match": fmt.Sprintf(`"%d"`, cur.Version)}, http.StatusOK)
}
