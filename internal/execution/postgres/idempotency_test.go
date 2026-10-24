package postgres_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

func identityOf(p executiontest.Project, key string, hash byte) httpapi.RequestIdentity {
	var h httpapi.RequestHash
	h[0] = hash
	return httpapi.RequestIdentity{
		Principal: httpapi.Principal{ID: executiontest.Engineer}, TenantID: p.Scope.TenantID, ProjectID: p.Scope.ProjectID, Method: http.MethodPost,
		Route: "/api/v1/tenants/" + string(p.Scope.TenantID) + "/projects/" + string(p.Scope.ProjectID) + "/runs", Key: httpapi.ClientKey(key), Hash: h,
	}
}

func render(run *execution.Run) (httpapi.Outcome, error) {
	body := []byte(`{"id":"` + string(run.ID) + `"}`)
	return httpapi.Outcome{
		Response: httpapi.StoredResponse{Status: http.StatusCreated, Body: body, Header: map[string]string{"Location": "/runs/" + string(run.ID)}},
		Target:   httpapi.Reference{Kind: "run", ID: string(run.ID)},
	}, nil
}

// Test_creation_idempotency: PX-02, PX-A09. Concurrent keys create one run, a
// changed body conflicts, a lost acknowledgment recovers with the same key, and
// a replay is authorized against current permissions.
func Test_creation_idempotency(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*executiontest.Env, executiontest.Project, pipeline.Definition) {
		e := executiontest.New(t)
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		return e, p, e.Define(p.Scope, pipelinetest.Doc(job("a", readRun, "timeoutSeconds: 60")))
	}
	prepare := func(e *executiontest.Env, p executiontest.Project, def pipeline.Definition) *execution.AdmissionPlan {
		plan, err := e.Svc.Prepare(ctx, engineer, execution.AdmitCommand{Scope: p.Scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: e.ContentRevision(p), SourceCommit: executiontest.CommitA})
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}
	allowAll := func(context.Context, httpapi.Reference) error { return nil }

	t.Run("concurrent requests with one key create one run", func(t *testing.T) {
		e, p, def := setup(t)
		id := identityOf(p, "key-concurrent", 1)
		const n = 12
		var wg sync.WaitGroup
		bodies := make([]string, n)
		replays := make([]bool, n)
		errs := make([]error, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, replayed, _, err := e.Keyed.CreateKeyed(ctx, id, allowAll, prepare(e, p, def), render)
				bodies[i], replays[i], errs[i] = string(resp.Body), replayed, err
			}()
		}
		wg.Wait()
		fresh := 0
		for i := range n {
			if errs[i] != nil {
				t.Fatalf("request %d: %v", i, errs[i])
			}
			if bodies[i] != bodies[0] {
				t.Fatalf("response %d differs: %s vs %s", i, bodies[i], bodies[0])
			}
			if !replays[i] {
				fresh++
			}
		}
		if fresh != 1 || e.Count(`SELECT count(*) FROM runs`) != 1 || e.Count(`SELECT count(*) FROM workspace_retained_refs`) != 1 {
			t.Fatalf("%d executions, %d runs, %d retained refs", fresh, e.Count(`SELECT count(*) FROM runs`), e.Count(`SELECT count(*) FROM workspace_retained_refs`))
		}
	})

	t.Run("a changed body under the same key conflicts", func(t *testing.T) {
		e, p, def := setup(t)
		if _, _, _, err := e.Keyed.CreateKeyed(ctx, identityOf(p, "key-body", 1), allowAll, prepare(e, p, def), render); err != nil {
			t.Fatal(err)
		}
		_, _, _, err := e.Keyed.CreateKeyed(ctx, identityOf(p, "key-body", 2), allowAll, prepare(e, p, def), render)
		var api *httpapi.APIError
		if !errors.As(err, &api) || api.Code != domain.CodeIdempotencyConflict {
			t.Fatalf("got %v", err)
		}
		if e.Count(`SELECT count(*) FROM runs`) != 1 {
			t.Fatal("a conflicting request created a run")
		}
	})

	t.Run("a lost acknowledgment recovers with the same key and creates nothing new", func(t *testing.T) {
		e, p, def := setup(t)
		first, _, run, err := e.Keyed.CreateKeyed(ctx, identityOf(p, "key-lost", 1), allowAll, prepare(e, p, def), render)
		if err != nil || run == nil {
			t.Fatal(err)
		}
		// The client never saw the response. Meanwhile the specification changed;
		// the replay must not resolve new inputs.
		e.EditSpec(p.Scope, p.Item, "# changed after the commit")
		second, replayed, again, err := e.Keyed.CreateKeyed(ctx, identityOf(p, "key-lost", 1), allowAll, &execution.AdmissionPlan{Aggregate: &execution.Aggregate{Run: &execution.Run{Scope: p.Scope}}}, render)
		if err != nil || !replayed || again != nil || string(second.Body) != string(first.Body) {
			t.Fatalf("replay: %v replayed=%v body %s", err, replayed, second.Body)
		}
		if e.Count(`SELECT count(*) FROM runs`) != 1 {
			t.Fatal("recovery created a second run")
		}
		// A rerun is another creation with a new key and a new run ID.
		plan := prepare(e, p, def)
		_, replayed, rerun, err := e.Keyed.CreateKeyed(ctx, identityOf(p, "key-rerun", 3), allowAll, plan, render)
		if err != nil || replayed || rerun.ID == run.ID {
			t.Fatalf("rerun: %v %v", err, replayed)
		}
	})

	t.Run("an admission failure stores nothing under the key", func(t *testing.T) {
		e, p, def := setup(t)
		bad := prepare(e, p, def)
		e.EditSpec(p.Scope, p.Item, "# raced")
		if _, _, _, err := e.Keyed.CreateKeyed(ctx, identityOf(p, "key-fail", 1), allowAll, bad, render); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("got %v", err)
		}
		if e.Count(`SELECT count(*) FROM idempotency`) != 0 {
			t.Fatal("a failed creation left an idempotency record")
		}
		// The same key can now succeed.
		if _, _, _, err := e.Keyed.CreateKeyed(ctx, identityOf(p, "key-fail", 1), allowAll, prepare(e, p, def), render); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a replay is authorized against current permissions", func(t *testing.T) {
		e, p, def := setup(t)
		replayAuth := func(ctx context.Context, ref httpapi.Reference) error {
			if ref.Kind != "run" && ref != (httpapi.Reference{}) {
				return httpapi.ForbiddenError()
			}
			if err := e.Svc.Authorize(ctx, engineer, p.Scope, execution.PermRunRead); err != nil {
				return httpapi.ForbiddenError()
			}
			return nil
		}
		id := identityOf(p, "key-auth", 1)
		if _, _, _, err := e.Keyed.CreateKeyed(ctx, id, replayAuth, prepare(e, p, def), render); err != nil {
			t.Fatal(err)
		}
		e.Revoke(p.Scope, executiontest.Engineer)
		var api *httpapi.APIError
		resp, _, _, err := e.Keyed.CreateKeyed(ctx, id, replayAuth, prepare2(t, e, p, def), render)
		if !errors.As(err, &api) || api.Status != http.StatusForbidden || len(resp.Body) != 0 {
			t.Fatalf("replay after revocation: %v body %q", err, resp.Body)
		}
	})
}

// prepare2 builds a plan even though the caller's access is gone, by preparing
// it before the revocation took effect for the plan only.
func prepare2(t *testing.T, e *executiontest.Env, p executiontest.Project, def pipeline.Definition) *execution.AdmissionPlan {
	t.Helper()
	// The engineer is revoked: only the plan shell is needed, because the replay
	// must be refused before any admission could run.
	return &execution.AdmissionPlan{Aggregate: &execution.Aggregate{Run: &execution.Run{Scope: p.Scope}}}
}
