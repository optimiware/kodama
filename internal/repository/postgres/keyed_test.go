package postgres_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
	"github.com/optimiware/kodama/internal/repository"
	repopg "github.com/optimiware/kodama/internal/repository/postgres"
	rt "github.com/optimiware/kodama/internal/repository/repositorytest"
)

func identity(key string, hash byte) httpapi.RequestIdentity {
	var h httpapi.RequestHash
	h[0] = hash
	return httpapi.RequestIdentity{
		Principal: httpapi.Principal{ID: rt.Engineer.ID}, TenantID: rt.ScopeA.TenantID, ProjectID: rt.ScopeA.ProjectID, Method: http.MethodPost,
		Route: "/api/v1/tenants/" + string(rt.ScopeA.TenantID) + "/projects/" + string(rt.ScopeA.ProjectID) + "/repositories",
		Key:   httpapi.ClientKey(key), Hash: h,
	}
}

func render(r *repository.Repository, created bool) (httpapi.Outcome, error) {
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	return httpapi.Outcome{
		Response: httpapi.StoredResponse{Status: status, Body: []byte(`{"id":"` + string(r.ID) + `"}`), Header: map[string]string{"Location": "/repositories/" + string(r.ID)}},
		Target:   httpapi.Reference{Kind: "repository", ID: string(r.ID)},
	}, nil
}

// Test_keyed_create: the reservation, its audit record and the stored response
// are one transaction; a retry replays; a changed request conflicts; a lost
// commit acknowledgment recovers with the same key; a replay is authorized
// against current permissions; provisioning is not part of the transaction.
func Test_keyed_create(t *testing.T) {
	ctx := context.Background()
	e := rt.NewPGEnv(t)
	k := rt.NewKit(t, e)
	idem, err := httppg.NewIdempotency(httppg.Config{DB: e.Runtime})
	if err != nil {
		t.Fatal(err)
	}
	keyed, err := repopg.NewKeyed(idem, e.PG)
	if err != nil {
		t.Fatal(err)
	}
	allow := func(context.Context, httpapi.Reference) error { return nil }
	prepare := func(req gitv1.CreateRepositoryRequest) *repository.CreatePlan {
		t.Helper()
		plan, err := k.Svc.Prepare(ctx, rt.Engineer, rt.ScopeA, req)
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}
	count := func() int {
		return countTenant(t, e, rt.ScopeA.TenantID, `SELECT count(*) FROM repositories WHERE project_id = $1`, string(rt.ScopeA.ProjectID))
	}

	plan := prepare(gitv1.CreateRepositoryRequest{Name: "backend"})
	resp, replayed, r, err := keyed.CreateKeyed(ctx, identity("k1", 1), allow, plan, render)
	if err != nil || replayed || r == nil || resp.Status != http.StatusCreated {
		t.Fatalf("first: %+v %v %v %v", resp, replayed, r, err)
	}
	if r.Status != gitv1.StatusProvisioning || r.Version != 1 {
		t.Fatalf("the reservation is committed before provisioning: %+v", r)
	}
	if k.Hosts.Initialized(rt.ScopeA.TenantID, string(r.ID)) != 0 {
		t.Fatal("the worker was called inside the transaction")
	}
	if got := e.AuditActions(t, rt.ScopeA, r.ID); len(got) != 1 || got[0] != "repository.provisioning_requested" {
		t.Fatalf("audit: %v", got)
	}

	t.Run("a retry with the same key replays the stored response", func(t *testing.T) {
		again, replayed, r2, err := keyed.CreateKeyed(ctx, identity("k1", 1), allow, prepare(gitv1.CreateRepositoryRequest{Name: "backend"}), render)
		if err != nil || !replayed || r2 != nil || string(again.Body) != string(resp.Body) || again.Status != http.StatusCreated {
			t.Fatalf("replay: %+v %v %v %v", again, replayed, r2, err)
		}
		if count() != 1 {
			t.Fatalf("%d repositories", count())
		}
	})

	t.Run("a changed request under the same key conflicts", func(t *testing.T) {
		_, _, _, err := keyed.CreateKeyed(ctx, identity("k1", 2), allow, prepare(gitv1.CreateRepositoryRequest{Name: "other"}), render)
		var api *httpapi.APIError
		if !errors.As(err, &api) || api.Code != domain.CodeIdempotencyConflict {
			t.Fatalf("changed body: %v", err)
		}
		if count() != 1 {
			t.Fatalf("%d repositories", count())
		}
	})

	t.Run("a replay is authorized again and never released on a refusal", func(t *testing.T) {
		denied := func(context.Context, httpapi.Reference) error { return httpapi.ForbiddenError() }
		_, _, _, err := keyed.CreateKeyed(ctx, identity("k1", 1), denied, prepare(gitv1.CreateRepositoryRequest{Name: "backend"}), render)
		var api *httpapi.APIError
		if !errors.As(err, &api) || api.Status != http.StatusForbidden {
			t.Fatalf("a refused replay: %v", err)
		}
	})

	t.Run("another key for the same name returns the existing repository", func(t *testing.T) {
		got, replayed, r2, err := keyed.CreateKeyed(ctx, identity("k2", 3), allow, prepare(gitv1.CreateRepositoryRequest{Name: "backend"}), render)
		if err != nil || replayed || r2 == nil || r2.ID != r.ID || got.Status != http.StatusOK {
			t.Fatalf("existing: %+v %v %v %v", got, replayed, r2, err)
		}
		if count() != 1 {
			t.Fatalf("%d repositories", count())
		}
	})

	t.Run("another key for another configuration of the name is a conflict and stores nothing", func(t *testing.T) {
		_, _, _, err := keyed.CreateKeyed(ctx, identity("k3", 4), allow, prepare(gitv1.CreateRepositoryRequest{Name: "backend", DefaultBranch: ptr("trunk")}), render)
		if !errors.Is(err, repository.ErrNameTaken) {
			t.Fatalf("name taken: %v", err)
		}
		// The key is free again: nothing of the failed attempt was stored.
		got, replayed, _, err := keyed.CreateKeyed(ctx, identity("k3", 4), allow, prepare(gitv1.CreateRepositoryRequest{Name: "second"}), render)
		if err != nil || replayed || got.Status != http.StatusCreated {
			t.Fatalf("the key after a refusal: %+v %v %v", got, replayed, err)
		}
	})

	t.Run("a failing render rolls the reservation back", func(t *testing.T) {
		boom := errors.New("render failed")
		_, _, _, err := keyed.CreateKeyed(ctx, identity("k4", 5), allow, prepare(gitv1.CreateRepositoryRequest{Name: "rolled-back"}),
			func(*repository.Repository, bool) (httpapi.Outcome, error) { return httpapi.Outcome{}, boom })
		if !errors.Is(err, boom) {
			t.Fatalf("render: %v", err)
		}
		if _, err := e.PG.GetByName(ctx, rt.ScopeA, "rolled-back"); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("the rolled-back reservation exists: %v", err)
		}
	})

	t.Run("provisioning after the commit finishes the reserved repository once", func(t *testing.T) {
		got, err := k.Svc.Provision(ctx, rt.ScopeA, r.ID)
		if err != nil || got.Status != gitv1.StatusReady {
			t.Fatalf("provision: %+v %v", got, err)
		}
		again, err := k.Svc.Provision(ctx, rt.ScopeA, r.ID)
		if err != nil || again.Version != got.Version {
			t.Fatalf("provision again: %+v %v", again, err)
		}
		if n := k.Hosts.Initialized(rt.ScopeA.TenantID, string(r.ID)); n != 1 {
			t.Fatalf("%d initializations", n)
		}
	})

	t.Run("concurrent creators with different keys converge", func(t *testing.T) {
		var wg sync.WaitGroup
		ids := make(chan repository.ID, 8)
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, got, err := keyed.CreateKeyed(ctx, identity("race-"+string(rune('a'+i)), byte(10+i)), allow,
					prepare(gitv1.CreateRepositoryRequest{Name: "raced"}), render)
				if err != nil {
					t.Error(err)
					return
				}
				ids <- got.ID
			}()
		}
		wg.Wait()
		close(ids)
		seen := map[repository.ID]bool{}
		for id := range ids {
			seen[id] = true
		}
		if len(seen) != 1 {
			t.Fatalf("%d repositories for one name", len(seen))
		}
	})
}

func ptr[T any](v T) *T { return &v }
