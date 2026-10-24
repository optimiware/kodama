package m1_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/test/e2e/m1/kit"
	oidcfx "github.com/optimiware/kodama/test/support/oidc"
)

// TestM1TenantMatrix (m1_tenant_matrix): the tenant boundary over every public
// resource of the slice, with real OIDC tokens, real rows and a real container.
// Membership, never a claim, decides; a foreign or unbound caller cannot tell a
// protected resource from a missing one; each fixed role holds exactly its
// permissions; copied cursors, logs, artifacts, source commits and runtime
// attempt references disclose and change nothing; a pooled connection never
// carries a previous tenant; and cancelled or rotated attempt tokens do nothing.
// IT-A01..IT-A05, AC-A03.
func TestM1TenantMatrix(t *testing.T) {
	s := kit.New(t, kit.Options{})
	s.Start()
	s.WaitReady()
	w := s.Seed(true)
	ctx := t.Context()

	// One finished run with a retained log, in tenant A, and one sleeping attempt.
	var second string
	item, run := w.Scripted("matrix", kit.Step{Script: `echo "tenant a log"`})
	if final := w.WaitRun(run.Id, 3*time.Minute); final.Status != client.RunStatusSucceeded {
		t.Fatalf("run %s (%s)", final.Status, final.Reason)
	}
	w.WaitLogs("engineer", run.Id)
	base := "/api/v1/tenants/" + kit.TenantA + "/projects/" + w.Project
	itemB, err := s.API("analyst").CreateWorkItemWithResponse(ctx, kit.TenantB, w.ProjectB, client.CreateWorkItem{Kind: "story", Title: "B work"})
	if err != nil || itemB.StatusCode() != http.StatusCreated {
		t.Fatalf("tenant B's engineer creates work: %v %s", err, itemB.Body)
	}

	t.Run("credentials: absent, malformed, wrong audience, wrong issuer, no membership (IT-A01)", func(t *testing.T) {
		stub := oidcfx.NewStub(t)
		forged := stub.Token(t, stub.Claims(oidcfx.Subject("engineer")))
		for name, tok := range map[string]string{
			"none": "", "garbage": "not-a-token", "id token (audience is the client)": s.OIDC.Login("engineer").ID,
			"a token of another issuer": forged,
		} {
			resp, body := s.Raw("GET", base+"/work-items", tok, nil, nil)
			if resp.StatusCode != http.StatusUnauthorized || kit.ErrorCode(body) != "invalid_credential" {
				t.Errorf("%s: %d %s", name, resp.StatusCode, body)
			}
		}
		if resp, _ := s.Raw("GET", base+"/work-items?access_token="+s.Token("engineer"), "", nil, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("a query-string token authenticated: %d", resp.StatusCode)
		}
		// "outsider" and "foreign" carry admin claims and no binding in tenant A: absent
		// membership is the same answer as a missing tenant, byte for byte.
		_, existing := s.Raw("GET", base+"/work-items", s.Token("outsider"), nil, nil)
		_, missing := s.Raw("GET", "/api/v1/tenants/t026_missing/projects/"+w.Project+"/work-items", s.Token("outsider"), nil, nil)
		if kit.Anonymize(existing) != kit.Anonymize(missing) {
			t.Errorf("a protected tenant is distinguishable from a missing one:\n%s\n%s", existing, missing)
		}
		if list, err := s.API("outsider").ListTenantsWithResponse(ctx, &client.ListTenantsParams{}); err != nil || list.StatusCode() != 200 || len(list.JSON200.Items) != 0 {
			t.Errorf("an unbound caller lists tenants: %v %v", list, err)
		}
	})

	t.Run("each fixed role holds exactly its permissions", func(t *testing.T) {
		type op struct {
			name string
			do   func(persona string) int
			want map[string]int
		}
		create := func(persona string) int {
			r, err := s.API(persona).CreateWorkItemWithResponse(ctx, kit.TenantA, w.Project, client.CreateWorkItem{Kind: "story", Title: "matrix " + persona})
			if err != nil {
				t.Fatal(err)
			}
			return r.StatusCode()
		}
		ops := []op{
			{"read work", func(p string) int {
				r, _ := s.API(p).ListWorkItemsWithResponse(ctx, kit.TenantA, w.Project, &client.ListWorkItemsParams{})
				return r.StatusCode()
			}, map[string]int{"admin": 403, "engineer": 200, "viewer": 200, "outsider": 404, "foreign": 404}},
			{"create work", create, map[string]int{"admin": 403, "engineer": 201, "viewer": 403, "outsider": 404, "foreign": 404}},
			{"read runs", func(p string) int {
				r, _ := s.API(p).ListRunsWithResponse(ctx, kit.TenantA, w.Project, &client.ListRunsParams{})
				return r.StatusCode()
			}, map[string]int{"admin": 403, "engineer": 200, "viewer": 200, "outsider": 404, "foreign": 404}},
			{"cancel a run", func(p string) int {
				r, _ := s.API(p).CancelRunWithResponse(ctx, kit.TenantA, w.Project, run.Id)
				return r.StatusCode()
			}, map[string]int{"admin": 403, "engineer": 202, "viewer": 403, "outsider": 404, "foreign": 404}},
			{"read a log", func(p string) int { code, _ := w.Logs(p, run.Id); return code },
				map[string]int{"admin": 403, "engineer": 200, "viewer": 200, "outsider": 404, "foreign": 404}},
			{"list secrets", func(p string) int {
				r, _ := s.API(p).ListSecretsWithResponse(ctx, kit.TenantA, w.Project, &client.ListSecretsParams{})
				return r.StatusCode()
			}, map[string]int{"admin": 200, "engineer": 200, "viewer": 403, "outsider": 404, "foreign": 404}},
			{"register a definition", func(p string) int {
				r, _ := s.API(p).RegisterPipelineDefinitionWithBodyWithResponse(ctx, kit.TenantA, w.Project, "application/json", strings.NewReader("{}"))
				return r.StatusCode()
			}, map[string]int{"viewer": 403, "outsider": 404, "foreign": 404}},
			{"grant membership", func(p string) int {
				pid := w.Project
				r, _ := s.API(p).CreateMembershipWithResponse(ctx, kit.TenantA, client.CreateMembership{UserId: w.OutsiderID, Role: "viewer", ProjectId: &pid})
				return r.StatusCode()
			}, map[string]int{"engineer": 403, "viewer": 403, "outsider": 404, "foreign": 404}},
		}
		for _, o := range ops {
			for persona, want := range o.want {
				if got := o.do(persona); got != want {
					t.Errorf("%s as %s: HTTP %d, want %d", o.name, persona, got, want)
				}
			}
		}
		if n := s.OwnerCount(`SELECT count(*) FROM memberships WHERE tenant_id = $1 AND user_id = $2`, kit.TenantA, w.OutsiderID); n != 0 {
			t.Fatalf("the outsider gained %d memberships", n)
		}
		if n := s.OwnerCount(`SELECT count(*) FROM memberships WHERE tenant_id = $1 AND user_id = $2`, kit.TenantA, w.ForeignID); n != 0 {
			t.Fatalf("the foreign administrator gained %d memberships in tenant A", n)
		}
	})

	t.Run("composite ownership rejects cross-tenant and cross-project references (IT-A02)", func(t *testing.T) {
		// A second project of tenant A: the engineer holds no binding there, and the viewer,
		// who reads both projects, still cannot read project A's objects through the second.
		r, err := s.API("admin").CreateProjectWithResponse(ctx, kit.TenantA, client.CreateProject{Key: "second", Settings: &map[string]any{}})
		if err != nil || r.StatusCode() != http.StatusCreated {
			t.Fatalf("second project: %v %s", err, r.Body)
		}
		second = r.JSON201.Id
		if m, err := s.API("admin").CreateMembershipWithResponse(ctx, kit.TenantA, client.CreateMembership{UserId: w.ViewerID, Role: "viewer", ProjectId: &second}); err != nil || m.StatusCode() != http.StatusCreated {
			t.Fatalf("viewer of the second project: %v %s", err, m.Body)
		}
		if got, _ := s.API("engineer").GetProjectWithResponse(ctx, kit.TenantA, second); got.StatusCode() != http.StatusNotFound {
			t.Errorf("an unbound project of the same tenant: %d", got.StatusCode())
		}
		for what, path := range map[string]string{
			"work item": "/work-items/" + item.Id, "run": "/runs/" + run.Id, "run logs": "/runs/" + run.Id + "/logs", "workspace": "/workspaces/" + w.Provision(item.Id).Id,
		} {
			resp, body := s.Raw("GET", "/api/v1/tenants/"+kit.TenantA+"/projects/"+second+path, s.Token("viewer"), nil, nil)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s of project A read through another project: %d %s", what, resp.StatusCode, body)
			}
		}
		for what, req := range map[string]func() (int, []byte){
			"run of A's item by tenant B": func() (int, []byte) {
				r, _ := s.API("analyst").CreateRunWithResponse(ctx, kit.TenantB, w.ProjectB, &client.CreateRunParams{IdempotencyKey: "matrix-foreign-run"},
					client.CreateRunRequest{WorkItemId: item.Id, DefinitionId: "pd_x", ContentRevision: 1, SourceCommit: s.Repo.Initial})
				return r.StatusCode(), r.Body
			},
			"A's workspace by tenant B": func() (int, []byte) {
				r, _ := s.API("analyst").ProvisionWorkspaceWithResponse(ctx, kit.TenantB, w.ProjectB, item.Id, client.EmptyRequest{})
				return r.StatusCode(), r.Body
			},
			"A's work item through B's path": func() (int, []byte) {
				r, _ := s.API("analyst").GetWorkItemWithResponse(ctx, kit.TenantB, w.ProjectB, item.Id)
				return r.StatusCode(), r.Body
			},
			"A's project through B's tenant": func() (int, []byte) {
				r, _ := s.API("analyst").GetProjectWithResponse(ctx, kit.TenantB, w.Project)
				return r.StatusCode(), r.Body
			},
		} {
			if code, body := req(); code != http.StatusNotFound && code != http.StatusUnprocessableEntity {
				t.Errorf("%s: %d %s", what, code, body)
			}
		}
		// A tenant cannot write into another tenant's partition even as the table owner's peer:
		// the serving role under tenant B sees none of A's rows and cannot insert one of A's.
		if n := s.TenantCount(kit.TenantB, `SELECT count(*) FROM runs WHERE id = $1`, run.Id); n != 0 {
			t.Errorf("tenant B's context sees %d of A's runs", n)
		}
		err = storage.WithTenant(ctx, s.Runtime, domain.TenantID(kit.TenantB), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO tenants (id, key) VALUES ('t026_intruder', 'intruder')`)
			return err
		})
		if err == nil {
			t.Error("the serving role created a tenant")
		}
	})

	t.Run("a pooled connection never carries the previous tenant (IT-A03)", func(t *testing.T) {
		pool := s.DB.RuntimePool(ctx, 1) // one connection: every use follows another tenant's
		count := func(tenant string, _ bool, where string) int {
			var n int
			run := func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM work_items `+where, tenant).Scan(&n)
			}
			if err := storage.WithTenant(ctx, pool, domain.TenantID(tenant), run); err != nil {
				t.Fatalf("read as tenant %s: %v", tenant, err)
			}
			return n
		}
		a := count(kit.TenantA, true, `WHERE tenant_id = $1`)
		if a == 0 {
			t.Fatal("tenant A has no work items to leak")
		}
		for i := 0; i < 5; i++ {
			if got := count(kit.TenantA, true, `WHERE tenant_id = $1`); got != a {
				t.Fatalf("tenant A sees %d, want %d", got, a)
			}
			// Tenant B sees its own one item and none that belongs to another tenant.
			if got := count(kit.TenantB, true, `WHERE tenant_id <> $1`); got != 0 {
				t.Fatalf("tenant B sees %d rows of another tenant's work after A's transaction on the same connection", got)
			}
			if got := count(kit.TenantB, true, `WHERE tenant_id = $1`); got != 1 {
				t.Fatalf("tenant B sees %d of its own work items, want 1", got)
			}
		}
		var bare int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM work_items`).Scan(&bare); err == nil && bare != 0 {
			t.Fatalf("a bare query on the pooled connection sees %d rows", bare)
		}
		// Concurrently, through the server's own small pool, both tenants read only their own data.
		var wg sync.WaitGroup
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				persona, tenant, project := "engineer", kit.TenantA, w.Project
				if i%2 == 1 {
					persona, tenant, project = "foreign", kit.TenantB, w.ProjectB
				}
				resp, body := s.Raw("GET", "/api/v1/tenants/"+tenant+"/projects/"+project+"/work-items", s.Token(persona), nil, nil)
				if tenant == kit.TenantB && (strings.Contains(string(body), item.Id) || resp.StatusCode == 500) {
					t.Errorf("tenant B's request: %d %s", resp.StatusCode, body)
				}
				if tenant == kit.TenantA && (resp.StatusCode != 200 || !strings.Contains(string(body), item.Id)) {
					t.Errorf("tenant A's request: %d", resp.StatusCode)
				}
			}()
		}
		wg.Wait()
	})

	t.Run("copied cursors, logs, artifacts and source commits disclose nothing (IT-A04, AC-A03)", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			w.WorkItem(fmt.Sprintf("Paged %d", i), "# p")
		}
		page, err := s.API("engineer").ListWorkItemsWithResponse(ctx, kit.TenantA, w.Project, &client.ListWorkItemsParams{Limit: ptr(client.Limit(1))})
		if err != nil || page.StatusCode() != 200 || page.JSON200.NextCursor == nil {
			t.Fatalf("first page: %v %s", err, page.Body)
		}
		cur := *page.JSON200.NextCursor
		if again, _ := s.API("engineer").ListWorkItemsWithResponse(ctx, kit.TenantA, w.Project, &client.ListWorkItemsParams{Limit: ptr(client.Limit(1)), Cursor: &cur}); again.StatusCode() != 200 {
			t.Fatalf("the genuine cursor: %d %s", again.StatusCode(), again.Body)
		}
		state := client.WorkState("done")
		tampered := cur[:len(cur)-2] + "AA"
		for what, req := range map[string]func() int{
			"another project of the tenant": func() int {
				r, _ := s.API("viewer").ListWorkItemsWithResponse(ctx, kit.TenantA, second, &client.ListWorkItemsParams{Limit: ptr(client.Limit(1)), Cursor: &cur})
				return r.StatusCode()
			},
			"another tenant": func() int {
				r, _ := s.API("analyst").ListWorkItemsWithResponse(ctx, kit.TenantB, w.ProjectB, &client.ListWorkItemsParams{Limit: ptr(client.Limit(1)), Cursor: &cur})
				return r.StatusCode()
			},
			"another filter": func() int {
				r, _ := s.API("engineer").ListWorkItemsWithResponse(ctx, kit.TenantA, w.Project, &client.ListWorkItemsParams{Limit: ptr(client.Limit(1)), Cursor: &cur, State: &[]client.WorkState{state}})
				return r.StatusCode()
			},
			"a tampered cursor": func() int {
				r, _ := s.API("engineer").ListWorkItemsWithResponse(ctx, kit.TenantA, w.Project, &client.ListWorkItemsParams{Limit: ptr(client.Limit(1)), Cursor: &tampered})
				return r.StatusCode()
			},
		} {
			if got := req(); got != http.StatusBadRequest && got != http.StatusNotFound {
				t.Errorf("a cursor reused for %s: HTTP %d", what, got)
			}
		}
		// A copied run, log or artifact reference answers exactly as a reference that never existed.
		pathB := "/api/v1/tenants/" + kit.TenantB + "/projects/" + w.ProjectB
		for what, tail := range map[string]string{"run": "/runs/%s", "logs": "/runs/%s/logs", "steps": "/runs/%s/steps", "artifact": "/artifacts/%s"} {
			real, missing := run.Id, "run_t026_missing"
			if what == "artifact" {
				s.MustScan(&real, `SELECT id FROM artifacts WHERE tenant_id = $1 LIMIT 1`, kit.TenantA)
				missing = "art_t026_missing"
			}
			_, copied := s.Raw("GET", pathB+fmt.Sprintf(tail, real), s.Token("analyst"), nil, nil)
			_, never := s.Raw("GET", pathB+fmt.Sprintf(tail, missing), s.Token("analyst"), nil, nil)
			if kit.Anonymize(copied) != kit.Anonymize(never) || !strings.Contains(string(copied), "not_found") {
				t.Errorf("a copied %s reference is distinguishable:\n%s\n%s", what, copied, never)
			}
		}
		if code, lines := w.Logs("analyst", run.Id); code == 200 || len(lines) != 0 {
			t.Errorf("tenant B read tenant A's log: %d %q", code, lines)
		}
		// A source commit of A's repository is not in B's retained history.
		def := w.PodmanDefinition("matrix-b-ref", kit.Step{Script: `true`})
		r, err := s.API("analyst").CreateRunWithResponse(ctx, kit.TenantB, w.ProjectB, &client.CreateRunParams{IdempotencyKey: "matrix-b-ref"},
			client.CreateRunRequest{WorkItemId: itemB.JSON201.Id, DefinitionId: def.Id, ContentRevision: 1, SourceCommit: s.Repo.Initial})
		if err != nil || r.StatusCode() < 400 || r.StatusCode() == 500 {
			t.Errorf("tenant B admitted a run on tenant A's definition or source: %v %d %s", err, r.StatusCode(), r.Body)
		}
	})

	t.Run("a cancelled or rotated attempt token does nothing (IT-A05)", func(t *testing.T) {
		sleeper := w.PodmanDefinition("matrix-sleeper", kit.Step{Script: `sleep 300`})
		item2 := w.WorkItem("Token subject", "# t")
		w.Provision(item2.Id)
		a := w.Run("matrix-token-a", item2, sleeper.Id)
		item3 := w.WorkItem("Other attempt", "# t")
		w.Provision(item3.Id)
		b := w.Run("matrix-token-b", item3, sleeper.Id)
		attA, attB := w.WaitAttempt(a.Id, time.Minute), w.WaitAttempt(b.Id, time.Minute)
		ca := s.WaitContainer(kit.TenantA, attA, "running", time.Minute)
		cb := s.WaitContainer(kit.TenantA, attB, "running", time.Minute)
		s.WaitLaunched(ca.ID)
		s.WaitLaunched(cb.ID)
		tokA := s.RuntimeToken(ca.ID)
		tokB := s.RuntimeToken(cb.ID)

		runtimeStatus := func(attempt, token string) int {
			resp, _ := s.Raw("GET", "/runtime/v1/attempts/"+attempt+"/context", token, nil, nil)
			return resp.StatusCode
		}
		if got := runtimeStatus(attA, tokA); got != http.StatusOK {
			t.Fatalf("a live attempt reads its own context: %d", got)
		}
		if got := runtimeStatus(attB, tokA); got == http.StatusOK {
			t.Fatalf("attempt A's token read attempt B's context")
		}
		if got := runtimeStatus(attA, ""); got != http.StatusUnauthorized {
			t.Fatalf("no token: %d", got)
		}
		if got := runtimeStatus(attA, "garbage"); got != http.StatusUnauthorized {
			t.Fatalf("a garbage token: %d", got)
		}

		// Cancel A: its token can neither read, reserve, nor finalize.
		w.CancelRun(a.Id)
		kit.WaitFor(t, "attempt A to be revoked", 30*time.Second, func() bool {
			return s.OwnerCount(`SELECT count(*) FROM attempt_controls WHERE attempt_id = $1 AND revoked_at IS NOT NULL`, attA) == 1
		})
		if got := runtimeStatus(attA, tokA); got != http.StatusForbidden && got != http.StatusUnauthorized {
			t.Fatalf("a cancelled attempt read its context: %d", got)
		}
		if code, _ := lateResult(t, s, attA, tokA); code != http.StatusForbidden && code != http.StatusUnauthorized {
			t.Fatalf("a cancelled attempt finalized a result: %d", code)
		}
		resp, _ := s.Raw("POST", "/runtime/v1/attempts/"+attA+"/artifacts", tokA, strings.NewReader(`{}`), map[string]string{"Content-Type": "application/json"})
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("a cancelled attempt reserved an artifact: %d", resp.StatusCode)
		}
		if n := s.OwnerCount(`SELECT count(*) FROM artifacts WHERE attempt_id = $1`, attA); n != 0 {
			t.Fatalf("a cancelled attempt created %d artifacts", n)
		}

		// Rotation: a new signing generation refuses every token of the old one.
		s.RotateSigningGeneration(2)
		s.Restart()
		if got := runtimeStatus(attB, tokB); got != http.StatusUnauthorized && got != http.StatusForbidden {
			t.Fatalf("a token of the rotated generation read a context: %d", got)
		}
		if code, _ := lateResult(t, s, attB, tokB); code != http.StatusUnauthorized && code != http.StatusForbidden {
			t.Fatalf("a token of the rotated generation finalized a result: %d", code)
		}
		if got := w.GetRun(b.Id); got.Status == client.RunStatusSucceeded {
			t.Fatalf("run B succeeded through a rotated token")
		}
	})
}

func ptr[T any](v T) *T { return &v }
