package assembly_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/optimiware/kodama/api/client"
)

// world is the seeded installation every acceptance test starts from: tenant A
// (administrator, an engineer and a viewer of one project) and tenant B (its own
// administrator and project), all created through the public API after the
// operator bootstrap.
type world struct {
	s *stack

	adminID, engineerID, viewerID, outsiderID, foreignID string

	project  string // tenant A's project
	projectB string // tenant B's project
}

type statusResponse interface {
	StatusCode() int
	Status() string
}

func expect(t testing.TB, r statusResponse, body []byte, want int, what string) {
	t.Helper()
	if r.StatusCode() != want {
		t.Fatalf("%s: HTTP %s, want %d: %s", what, r.Status(), want, body)
	}
}

// seed bootstraps both tenants and provisions identities, a project each, and
// the memberships.
func (s *stack) seed(repo bool) *world {
	t := s.t
	t.Helper()
	ctx := context.Background()
	w := &world{s: s}
	s.bootstrap(TenantA, "acme", "admin")

	me := func(persona string) string {
		r, err := s.api(persona).GetMeWithResponse(ctx)
		if err != nil {
			t.Fatal(err)
		}
		expect(t, r, r.Body, http.StatusOK, "me "+persona)
		return r.JSON200.Id
	}
	w.adminID, w.engineerID, w.viewerID, w.outsiderID, w.foreignID = me("admin"), me("engineer"), me("viewer"), me("outsider"), me("foreign")

	// The bootstrap is once per installation (its completion record is durable),
	// so the second tenant is provisioned the way an operator-controlled import
	// would: as the owner identity, which the serving role cannot be. Its
	// administrator then works only through the public API.
	for _, sql := range []string{
		`INSERT INTO tenants (id, key) VALUES ('` + TenantB + `', 'globex')`,
		`INSERT INTO memberships (tenant_id, id, user_id, project_id, role) VALUES ('` + TenantB + `', 'mb_t070_globex_admin', '` + w.foreignID + `', NULL, 'admin')`,
		`INSERT INTO identity_authz_revisions (tenant_id, user_id, revision) VALUES ('` + TenantB + `', '` + w.foreignID + `', 1)`,
	} {
		if _, err := s.Owner.Exec(ctx, sql); err != nil {
			t.Fatalf("seed tenant B: %v", err)
		}
	}

	settings := map[string]any{}
	if repo {
		settings["repository"] = map[string]any{
			"id": "repo_t070", "url": s.Repo.URL, "baseRef": s.Repo.BaseRef,
			"checkoutSecretRef": "secret://installation/checkout", "publisherSecretRef": "secret://installation/publisher",
		}
	}
	proj := func(persona, tenant, key string, settings map[string]any) string {
		r, err := s.api(persona).CreateProjectWithResponse(ctx, tenant, client.CreateProject{Key: key, Settings: &settings})
		if err != nil {
			t.Fatal(err)
		}
		expect(t, r, r.Body, http.StatusCreated, "create project "+key)
		return r.JSON201.Id
	}
	w.project = proj("admin", TenantA, "inventory", settings)
	w.projectB = proj("foreign", TenantB, "billing", map[string]any{})

	member := func(user, role string) {
		pid := w.project
		r, err := s.api("admin").CreateMembershipWithResponse(ctx, TenantA, client.CreateMembership{UserId: user, Role: client.Role(role), ProjectId: &pid})
		if err != nil {
			t.Fatal(err)
		}
		expect(t, r, r.Body, http.StatusCreated, "membership "+role)
	}
	member(w.engineerID, "engineer")
	member(w.viewerID, "viewer")
	return w
}

// workItem creates a story as the engineer.
func (w *world) workItem(title, spec string) client.WorkItem {
	t := w.s.t
	t.Helper()
	r, err := w.s.api("engineer").CreateWorkItemWithResponse(context.Background(), TenantA, w.project, client.CreateWorkItem{Kind: "story", Title: title, Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	expect(t, r, r.Body, http.StatusCreated, "create work item")
	return *r.JSON201
}

// definition registers a one-step container pipeline as the engineer.
func (w *world) definition(name string, extra map[string]any) client.PipelineDefinition {
	t := w.s.t
	t.Helper()
	step := map[string]any{
		"id": "test", "kind": "container", "timeoutSeconds": 600,
		"image":     "registry.example.invalid/go-tools@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		"command":   []string{"/bin/sh", "-ec"},
		"args":      []string{"echo ok"},
		"workspace": map[string]any{"from": "run", "access": "read"},
	}
	for k, v := range extra {
		step[k] = v
	}
	doc := map[string]any{
		"apiVersion": "kodama/v1alpha1", "kind": "Pipeline", "metadata": map[string]any{"name": name},
		"spec": map[string]any{"inputs": map[string]any{}, "steps": []any{step}},
	}
	r, err := w.s.api("engineer").RegisterPipelineDefinitionWithBodyWithResponse(context.Background(), TenantA, w.project, "application/json", jsonBody(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode() != http.StatusCreated && r.StatusCode() != http.StatusOK {
		t.Fatalf("register definition: HTTP %s: %s", r.Status(), r.Body)
	}
	if r.JSON201 != nil {
		return *r.JSON201
	}
	return *r.JSON200
}

// holdDefinition registers a one-step pipeline that waits at a human gate. The
// test composition dispatches (the dispatcher passes once at every start), so a
// run whose step is a container would be claimed and started by Podman; a gate
// step allocates no attempt, which lets the replay, cancellation and restart
// scenarios of this suite observe the run without a container racing them.
// Real container steps are proven by core_assembly_current_runtime.
func (w *world) holdDefinition(name string) client.PipelineDefinition {
	t := w.s.t
	t.Helper()
	doc := map[string]any{
		"apiVersion": "kodama/v1alpha1", "kind": "Pipeline", "metadata": map[string]any{"name": name},
		"spec": map[string]any{"inputs": map[string]any{}, "steps": []any{
			map[string]any{"id": "approve", "kind": "gate", "gate": map[string]any{"evidence": []string{}}},
		}},
	}
	r, err := w.s.api("engineer").RegisterPipelineDefinitionWithBodyWithResponse(context.Background(), TenantA, w.project, "application/json", jsonBody(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode() != http.StatusCreated && r.StatusCode() != http.StatusOK {
		t.Fatalf("register definition: HTTP %s: %s", r.Status(), r.Body)
	}
	if r.JSON201 != nil {
		return *r.JSON201
	}
	return *r.JSON200
}

// provision provisions the work item's workspace through the real Git server.
func (w *world) provision(item string) client.Workspace {
	t := w.s.t
	t.Helper()
	r, err := w.s.api("engineer").ProvisionWorkspaceWithResponse(context.Background(), TenantA, w.project, item, client.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode() != http.StatusCreated && r.StatusCode() != http.StatusOK {
		t.Fatalf("provision workspace: HTTP %s: %s", r.Status(), r.Body)
	}
	ws := r.JSON201
	if ws == nil {
		ws = r.JSON200
	}
	if ws.State != "available" {
		t.Fatalf("workspace is %s (%v)", ws.State, ws.LastError)
	}
	return *ws
}
