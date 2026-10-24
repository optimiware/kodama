package assembly_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
)

func errorCode(body []byte) string {
	var env struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	return env.Error.Code
}

// Test_core_assembly_authorization (IT-03, SD-10, SD-11, PX-05, AC-10, AC-12):
// public requests with real OIDC access tokens against the running composition.
// Membership, never a token claim, decides; a foreign tenant's caller cannot
// tell a protected resource from a missing one; each fixed role gets exactly its
// permissions; definitions admit container steps only and reference only
// existing secrets by file; run admission rechecks current authority on replay;
// a rejected admission rolls back completely; and row level security holds for
// the runtime role beneath all of it.
func Test_core_assembly_authorization(t *testing.T) {
	s := newStack(t, options{})
	s.start()
	w := s.seed(true)
	ctx := context.Background()

	item := w.workItem("Authorization subject", "# Plan\nDo the thing.")

	t.Run("an absent, malformed or wrong-audience credential is unauthenticated", func(t *testing.T) {
		path := "/api/v1/tenants/" + TenantA + "/projects/" + w.project + "/work-items"
		for name, tok := range map[string]string{
			"none":              "",
			"garbage":           "not-a-token",
			"id token":          s.OIDC.Login("engineer").ID, // audience is the client, not the API
			"bearer with space": "a b",
		} {
			resp, body := s.raw("GET", path, tok, nil, nil)
			if resp.StatusCode != http.StatusUnauthorized || errorCode(body) != "invalid_credential" {
				t.Errorf("%s: %d %s", name, resp.StatusCode, body)
			}
		}
		// Credentials only travel in the Authorization header.
		resp, _ := s.raw("GET", path+"?access_token="+s.token("engineer"), "", nil, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("a query-string token authenticated: %d", resp.StatusCode)
		}
	})

	t.Run("claims grant nothing: a caller without a binding sees the same as a missing resource", func(t *testing.T) {
		// "outsider" carries admin and reviewer role claims and holds no binding.
		out := s.api("outsider")
		me, err := out.GetMeWithResponse(ctx)
		if err != nil || me.StatusCode() != 200 {
			t.Fatalf("an authenticated caller can always read itself: %v %v", me, err)
		}
		tenants, err := out.ListTenantsWithResponse(ctx, &client.ListTenantsParams{})
		if err != nil || tenants.StatusCode() != 200 || len(tenants.JSON200.Items) != 0 {
			t.Fatalf("an unbound caller lists tenants: %v %v", tenants, err)
		}
		type probe struct {
			name   string
			status func() (int, []byte)
		}
		missingTenant := "t070_missing"
		for _, p := range []probe{
			{"work items", func() (int, []byte) {
				r, _ := out.ListWorkItemsWithResponse(ctx, TenantA, w.project, &client.ListWorkItemsParams{})
				return r.StatusCode(), r.Body
			}},
			{"work item", func() (int, []byte) {
				r, _ := out.GetWorkItemWithResponse(ctx, TenantA, w.project, item.Id)
				return r.StatusCode(), r.Body
			}},
			{"create work item", func() (int, []byte) {
				r, _ := out.CreateWorkItemWithResponse(ctx, TenantA, w.project, client.CreateWorkItem{Kind: "story", Title: "x"})
				return r.StatusCode(), r.Body
			}},
			{"runs", func() (int, []byte) {
				r, _ := out.ListRunsWithResponse(ctx, TenantA, w.project, &client.ListRunsParams{})
				return r.StatusCode(), r.Body
			}},
			{"secrets", func() (int, []byte) {
				r, _ := out.ListSecretsWithResponse(ctx, TenantA, w.project, &client.ListSecretsParams{})
				return r.StatusCode(), r.Body
			}},
			{"members", func() (int, []byte) {
				r, _ := out.ListMembershipsWithResponse(ctx, TenantA, &client.ListMembershipsParams{})
				return r.StatusCode(), r.Body
			}},
			{"project", func() (int, []byte) {
				r, _ := out.GetProjectWithResponse(ctx, TenantA, w.project)
				return r.StatusCode(), r.Body
			}},
		} {
			code, body := p.status()
			if code != http.StatusNotFound || errorCode(body) != "not_found" {
				t.Errorf("%s as an unbound caller: %d %s", p.name, code, body)
			}
		}
		// Byte-identical to a tenant that does not exist.
		_, existing := s.raw("GET", "/api/v1/tenants/"+TenantA+"/projects/"+w.project+"/work-items", s.token("outsider"), nil, nil)
		_, missing := s.raw("GET", "/api/v1/tenants/"+missingTenant+"/projects/"+w.project+"/work-items", s.token("outsider"), nil, nil)
		if normalizeRequestID(string(existing)) != normalizeRequestID(string(missing)) {
			t.Errorf("a protected tenant is distinguishable from a missing one:\n%s\n%s", existing, missing)
		}
	})

	t.Run("a foreign tenant's administrator cannot reach, mutate or observe tenant A", func(t *testing.T) {
		f := s.api("foreign")
		own, err := f.GetProjectWithResponse(ctx, TenantB, w.projectB)
		if err != nil || own.StatusCode() != 200 {
			t.Fatalf("a caller reads its own project: %v %v", own, err)
		}
		for name, r := range map[string]func() (int, []byte){
			"project A": func() (int, []byte) {
				r, _ := f.GetProjectWithResponse(ctx, TenantA, w.project)
				return r.StatusCode(), r.Body
			},
			"work items of A": func() (int, []byte) {
				r, _ := f.ListWorkItemsWithResponse(ctx, TenantA, w.project, &client.ListWorkItemsParams{})
				return r.StatusCode(), r.Body
			},
			"work item of A under tenant B": func() (int, []byte) {
				// The administrator holds no work permission in its own project, so the
				// answer is the permission refusal it would give for any identifier.
				r, _ := f.GetWorkItemWithResponse(ctx, TenantB, w.projectB, item.Id)
				m, _ := f.GetWorkItemWithResponse(ctx, TenantB, w.projectB, "wi_t070_missing")
				if r.StatusCode() != m.StatusCode() || normalizeRequestID(string(r.Body)) != normalizeRequestID(string(m.Body)) {
					t.Errorf("a work item of tenant A is distinguishable from a missing one: %d %s vs %d %s", r.StatusCode(), r.Body, m.StatusCode(), m.Body)
				}
				return http.StatusNotFound, nil
			},
			"project A under tenant B": func() (int, []byte) {
				r, _ := f.GetProjectWithResponse(ctx, TenantB, w.project)
				return r.StatusCode(), r.Body
			},
			"membership grant in A": func() (int, []byte) {
				pid := w.project
				r, _ := f.CreateMembershipWithResponse(ctx, TenantA, client.CreateMembership{UserId: w.foreignID, Role: "admin", ProjectId: &pid})
				return r.StatusCode(), r.Body
			},
			"events of A": func() (int, []byte) {
				resp, body := s.raw("GET", "/api/v1/tenants/"+TenantA+"/projects/"+w.project+"/events", s.token("foreign"), nil, map[string]string{"Accept": "application/json"})
				return resp.StatusCode, body
			},
			"run of A": func() (int, []byte) {
				r, _ := f.CreateRunWithResponse(ctx, TenantA, w.project, &client.CreateRunParams{IdempotencyKey: "t070-foreign-run"},
					client.CreateRunRequest{WorkItemId: item.Id, DefinitionId: "pd_x", ContentRevision: 1, SourceCommit: s.Repo.Initial})
				return r.StatusCode(), r.Body
			},
			"workspace of A": func() (int, []byte) {
				r, _ := f.ProvisionWorkspaceWithResponse(ctx, TenantA, w.project, item.Id, client.EmptyRequest{})
				return r.StatusCode(), r.Body
			},
			"artifact of A": func() (int, []byte) {
				r, _ := f.GetArtifactWithResponse(ctx, TenantA, w.project, "art_x", &client.GetArtifactParams{})
				return r.StatusCode(), r.Body
			},
		} {
			code, body := r()
			if code != http.StatusNotFound {
				t.Errorf("%s: %d %s", name, code, body)
			}
		}
		if n := s.ownerCount(`SELECT count(*) FROM memberships WHERE tenant_id = $1 AND user_id = $2`, TenantA, w.foreignID); n != 0 {
			t.Fatalf("the foreign administrator gained %d memberships in tenant A", n)
		}
	})

	t.Run("each fixed role holds exactly its permissions", func(t *testing.T) {
		viewer, admin, eng := s.api("viewer"), s.api("admin"), s.api("engineer")
		if r, _ := viewer.ListWorkItemsWithResponse(ctx, TenantA, w.project, &client.ListWorkItemsParams{}); r.StatusCode() != 200 || len(r.JSON200.Items) != 1 {
			t.Errorf("viewer reads work: %s", r.Body)
		}
		if r, _ := viewer.CreateWorkItemWithResponse(ctx, TenantA, w.project, client.CreateWorkItem{Kind: "story", Title: "no"}); r.StatusCode() != 403 || errorCode(r.Body) != "forbidden" {
			t.Errorf("viewer creates work: %d %s", r.StatusCode(), r.Body)
		}
		if r, _ := viewer.ListSecretsWithResponse(ctx, TenantA, w.project, &client.ListSecretsParams{}); r.StatusCode() != 403 {
			t.Errorf("viewer lists secrets: %d %s", r.StatusCode(), r.Body)
		}
		if r, _ := viewer.CreateRunWithResponse(ctx, TenantA, w.project, &client.CreateRunParams{IdempotencyKey: "t070-viewer-run"},
			client.CreateRunRequest{WorkItemId: item.Id, DefinitionId: "pd_x", ContentRevision: 1, SourceCommit: s.Repo.Initial}); r.StatusCode() != 403 {
			t.Errorf("viewer starts a run: %d %s", r.StatusCode(), r.Body)
		}
		if r, _ := viewer.RegisterPipelineDefinitionWithBodyWithResponse(ctx, TenantA, w.project, "application/json", strings.NewReader("{}")); r.StatusCode() != 403 {
			t.Errorf("viewer registers a definition: %d %s", r.StatusCode(), r.Body)
		}
		// The administrator manages membership, settings and credentials; it is not an engineer.
		if r, _ := admin.CreateWorkItemWithResponse(ctx, TenantA, w.project, client.CreateWorkItem{Kind: "story", Title: "no"}); r.StatusCode() != 403 {
			t.Errorf("admin creates work: %d %s", r.StatusCode(), r.Body)
		}
		if r, _ := admin.CreateRunWithResponse(ctx, TenantA, w.project, &client.CreateRunParams{IdempotencyKey: "t070-admin-run"},
			client.CreateRunRequest{WorkItemId: item.Id, DefinitionId: "pd_x", ContentRevision: 1, SourceCommit: s.Repo.Initial}); r.StatusCode() != 403 {
			t.Errorf("admin starts a run: %d %s", r.StatusCode(), r.Body)
		}
		// Engineers read credential metadata but cannot manage credentials; no role reads plaintext.
		const secretValue = "s3cr3t-value-t070"
		if r, _ := eng.CreateSecretWithResponse(ctx, TenantA, w.project, &client.CreateSecretParams{IdempotencyKey: "t070-secret-eng"},
			client.CreateSecretRequest{Name: "token", Kind: "generic", Value: b64(secretValue)}); r.StatusCode() != 403 {
			t.Errorf("engineer manages credentials: %d %s", r.StatusCode(), r.Body)
		}
		created, err := admin.CreateSecretWithResponse(ctx, TenantA, w.project, &client.CreateSecretParams{IdempotencyKey: "t070-secret-admin"},
			client.CreateSecretRequest{Name: "token", Kind: "generic", Value: b64(secretValue)})
		if err != nil || created.StatusCode() != 201 {
			t.Fatalf("admin creates a secret: %v %s", err, created.Body)
		}
		if strings.Contains(string(created.Body), secretValue) {
			t.Fatal("the secret value was returned")
		}
		list, _ := eng.ListSecretsWithResponse(ctx, TenantA, w.project, &client.ListSecretsParams{})
		if list.StatusCode() != 200 || len(list.JSON200.Items) != 1 || strings.Contains(string(list.Body), secretValue) {
			t.Fatalf("engineer reads secret metadata: %d %s", list.StatusCode(), list.Body)
		}
		if r, _ := eng.CreateVariableWithResponse(ctx, TenantA, w.project, &client.CreateVariableParams{IdempotencyKey: "t070-var"},
			client.CreateVariableRequest{Name: "channel", Value: client.Scalar{}}); r.StatusCode() == 500 {
			t.Errorf("variable create failed internally: %s", r.Body)
		}
	})

	t.Run("definitions admit container steps and file references to existing secrets only", func(t *testing.T) {
		def := w.definition("build", nil)
		again := w.definition("build", nil)
		if again.Id != def.Id {
			t.Fatalf("identical registration produced %s and %s", def.Id, again.Id)
		}
		register := func(step map[string]any) (int, []byte) {
			doc := map[string]any{"apiVersion": "kodama/v1alpha1", "kind": "Pipeline", "metadata": map[string]any{"name": "other"},
				"spec": map[string]any{"inputs": map[string]any{}, "steps": []any{step}}}
			r, err := s.api("engineer").RegisterPipelineDefinitionWithBodyWithResponse(ctx, TenantA, w.project, "application/json", jsonBody(t, doc))
			if err != nil {
				t.Fatal(err)
			}
			return r.StatusCode(), r.Body
		}
		container := func(extra map[string]any) map[string]any {
			step := map[string]any{"id": "s", "kind": "container", "timeoutSeconds": 60, "command": []string{"/bin/true"},
				"image": "registry.example.invalid/go-tools@sha256:1111111111111111111111111111111111111111111111111111111111111111"}
			for k, v := range extra {
				step[k] = v
			}
			return step
		}
		if code, body := register(map[string]any{"id": "ask", "kind": "agent", "agent": map[string]any{"profile": "coding-v1"}, "workspace": map[string]any{"access": "write", "from": "run"}, "timeoutSeconds": 60}); code != 422 || !strings.Contains(string(body), "mutable_profile") {
			// Agent and gate steps are enabled in the test composition (T-072); an
			// agent step must name an immutable profile version, never a profile.
			t.Errorf("an agent step naming a mutable profile: %d %s", code, body)
		}
		if code, body := register(map[string]any{"id": "approve", "kind": "gate", "gate": map[string]any{"evidence": []string{}}}); code != 201 {
			t.Errorf("a gate step: %d %s", code, body)
		}
		if code, body := register(container(map[string]any{"secretFiles": map[string]any{"nope": "secret://project/absent"}})); code != 422 || !strings.Contains(string(body), "secret") {
			t.Errorf("a file reference to an absent secret: %d %s", code, body)
		}
		if code, body := register(container(map[string]any{"env": map[string]any{"T": map[string]any{"secretRef": "secret://project/token"}}})); code != 422 {
			t.Errorf("environment delivery of a secret is not permitted in the core installation: %d %s", code, body)
		}
		if code, body := register(container(map[string]any{"secretFiles": map[string]any{"token": "secret://project/token"}})); code != 201 {
			t.Errorf("a file reference to an existing secret: %d %s", code, body)
		}
		if code, body := register(container(map[string]any{"environment": "env_prod"})); code != 422 {
			t.Errorf("an environment reference without deployments: %d %s", code, body)
		}
	})

	t.Run("run admission: authority, idempotency, source retention, rollback", func(t *testing.T) {
		eng := s.api("engineer")
		// A gate step: the test composition dispatches, so a container step would
		// be claimed by the real dispatcher; a run waiting at a gate holds no
		// attempt and lets admission, replay and rollback be observed as they are.
		def := w.holdDefinition("admit")
		wsRepoTip := func(workspaceBranch string) string { return s.Repo.Tip(t, workspaceBranch) }

		create := func(key string, item client.WorkItem, rev int64, def string, commit string) *client.CreateRunResponse {
			r, err := eng.CreateRunWithResponse(ctx, TenantA, w.project, &client.CreateRunParams{IdempotencyKey: key},
				client.CreateRunRequest{WorkItemId: item.Id, DefinitionId: def, ContentRevision: rev, SourceCommit: commit})
			if err != nil {
				t.Fatal(err)
			}
			return r
		}
		// Without a workspace there is no retained source to pin: refused, nothing created.
		if r := create("t070-no-workspace", item, item.ContentRevision, def.Id, s.Repo.Initial); r.StatusCode() < 400 || r.StatusCode() >= 500 {
			t.Fatalf("a run without a workspace: %d %s", r.StatusCode(), r.Body)
		}
		if n := s.ownerCount(`SELECT count(*) FROM runs`); n != 0 {
			t.Fatalf("a refused admission left %d runs", n)
		}

		ws := w.provision(item.Id)
		if got := wsRepoTip(ws.Branch); got != s.Repo.Initial {
			t.Fatalf("the work branch %s is at %s, not the initial commit", ws.Branch, got)
		}

		before := s.tenantCount(TenantA, `SELECT count(*) FROM outbox`)
		// A stale content revision conflicts and rolls back every effect.
		stale := create("t070-stale", item, item.ContentRevision+7, def.Id, s.Repo.Initial)
		if stale.StatusCode() != http.StatusConflict && stale.StatusCode() != http.StatusUnprocessableEntity && stale.StatusCode() != http.StatusBadRequest {
			t.Fatalf("a stale content revision: %d %s", stale.StatusCode(), stale.Body)
		}
		if n := s.ownerCount(`SELECT count(*) FROM runs`) + s.ownerCount(`SELECT count(*) FROM secret_pins`) + s.ownerCount(`SELECT count(*) FROM workspace_retained_refs`); n != 0 {
			t.Fatalf("a rejected admission left %d rows behind", n)
		}
		if after := s.tenantCount(TenantA, `SELECT count(*) FROM outbox`); after != before {
			t.Fatalf("a rejected admission appended %d events", after-before)
		}
		// A commit outside the workspace history is refused as well.
		if r := create("t070-foreign-commit", item, item.ContentRevision, def.Id, strings.Repeat("a", 40)); r.StatusCode() < 400 || r.StatusCode() >= 500 {
			t.Fatalf("a commit outside the workspace history: %d %s", r.StatusCode(), r.Body)
		}

		run := create("t070-admit", item, item.ContentRevision, def.Id, s.Repo.Initial)
		if run.StatusCode() != http.StatusCreated {
			t.Fatalf("admission: %d %s", run.StatusCode(), run.Body)
		}
		if run.JSON201.Status != "queued" || run.JSON201.RequestedBy.Id != w.engineerID {
			t.Fatalf("run %+v", run.JSON201)
		}
		// Retention is confirmed after the response, against the real Git server.
		waitFor(t, "the run's retained source reference", 20*time.Second, func() bool {
			r, _ := eng.GetRunWithResponse(ctx, TenantA, w.project, run.JSON201.Id)
			return r.StatusCode() == 200 && r.JSON200.SourceRetention == "confirmed"
		})
		refs := s.Repo.Git(t, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/kodama/retained/")
		if !strings.Contains(refs, s.Repo.Initial) {
			t.Fatalf("no retained reference at the pinned commit on the remote: %q", refs)
		}

		// The same key and body replays the original answer; a different body conflicts.
		replay := create("t070-admit", item, item.ContentRevision, def.Id, s.Repo.Initial)
		if replay.StatusCode() != http.StatusCreated || replay.JSON201.Id != run.JSON201.Id || replay.HTTPResponse.Header.Get("Idempotent-Replayed") != "true" {
			t.Fatalf("replay: %d %s", replay.StatusCode(), replay.Body)
		}
		other := w.holdDefinition("admit-other")
		if conflict := create("t070-admit", item, item.ContentRevision, other.Id, s.Repo.Initial); conflict.StatusCode() != http.StatusConflict || errorCode(conflict.Body) != "idempotency_conflict" {
			t.Fatalf("a changed request under the same key: %d %s", conflict.StatusCode(), conflict.Body)
		}
		if n := s.ownerCount(`SELECT count(*) FROM runs`); n != 1 {
			t.Fatalf("%d runs after a replay and a conflict", n)
		}
		// A run waiting at a gate holds no attempt, control or runtime grant.
		time.Sleep(time.Second)
		s.noAttempts(t)

		// Replays recheck current authority: a revoked engineer cannot read the stored answer.
		list, _ := s.api("admin").ListMembershipsWithResponse(ctx, TenantA, &client.ListMembershipsParams{})
		var binding string
		for _, m := range list.JSON200.Items {
			if m.UserId == w.engineerID {
				binding = m.Id
			}
		}
		if binding == "" {
			t.Fatal("the engineer's membership was not listed")
		}
		resp, body := s.raw("DELETE", "/api/v1/tenants/"+TenantA+"/memberships/"+binding, s.token("admin"), nil, map[string]string{"If-Match": `"1"`})
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
			t.Fatalf("revoke: %d %s", resp.StatusCode, body)
		}
		revoked := create("t070-admit", item, item.ContentRevision, def.Id, s.Repo.Initial)
		if revoked.StatusCode() != http.StatusNotFound {
			t.Fatalf("a revoked caller replayed an admission: %d %s", revoked.StatusCode(), revoked.Body)
		}
		if r, _ := eng.GetRunWithResponse(ctx, TenantA, w.project, run.JSON201.Id); r.StatusCode() != http.StatusNotFound {
			t.Fatalf("a revoked caller reads a run: %d", r.StatusCode())
		}
	})

	t.Run("row level security binds the runtime role under the composition", func(t *testing.T) {
		count := func(tenant domain.TenantID, sql string) int {
			var n int
			err := storage.WithTenant(ctx, s.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, sql).Scan(&n)
			})
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
		if a, b := count(TenantA, `SELECT count(*) FROM runs`), count(TenantB, `SELECT count(*) FROM runs`); a != 1 || b != 0 {
			t.Fatalf("tenant A sees %d runs, tenant B %d", a, b)
		}
		if a, b := count(TenantA, `SELECT count(*) FROM work_items`), count(TenantB, `SELECT count(*) FROM work_items`); a != 1 || b != 0 {
			t.Fatalf("work items: A %d, B %d", a, b)
		}
		var n int
		if err := s.Runtime.QueryRow(ctx, `SELECT count(*) FROM runs`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("a connection without tenant context sees %d runs (%v)", n, err)
		}
		// A write under tenant B's context cannot carry tenant A's ownership.
		err := storage.WithTenant(ctx, s.Runtime, TenantB, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO variables (tenant_id, project_id, id, name, value_type, description, current_version, version, created_at, updated_at)
				VALUES ($1, $2, 'var_t070_x', 'x', 'string', '', 1, 1, now(), now())`, TenantA, w.project)
			return err
		})
		if err == nil {
			t.Fatal("a tenant B transaction wrote a tenant A row")
		}
	})
}

var requestID = func(s string) string {
	i := strings.Index(s, `"requestId":"`)
	if i < 0 {
		return s
	}
	j := strings.Index(s[i+13:], `"`)
	return s[:i+13] + s[i+13+j:]
}

func normalizeRequestID(s string) string { return requestID(s) }
