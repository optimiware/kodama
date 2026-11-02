package productmcp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
)

func Test_unit_productmcp_caller_authority_only(t *testing.T) {
	ctx := context.Background()

	t.Run("roles decide, the tool adds nothing", func(t *testing.T) {
		f := newFixture(t)
		viewer, analyst, eng, stranger := personCaller(tenantA, "vic"), personCaller(tenantA, "ana"), personCaller(tenantA, "erin"), personCaller(tenantA, "sam")
		f.w.grant(viewer, projP, "viewer")
		f.w.grant(analyst, projP, "analyst")
		f.w.grant(eng, projP, "engineer")
		repo := func(c Caller, key string) error {
			_, err := f.svc.CreateRepository(ctx, c, RepositoryCreateIn{ProjectID: string(projP), IdempotencyKey: key, Name: "svc"})
			return err
		}
		item := func(c Caller, key string) error {
			_, err := f.svc.CreateWorkItem(ctx, c, WorkItemCreateIn{ProjectID: string(projP), IdempotencyKey: key, Title: "t"})
			return err
		}
		mustCode(t, repo(viewer, "k1"), "forbidden")
		mustCode(t, item(viewer, "k1"), "forbidden")
		mustCode(t, repo(analyst, "k2"), "forbidden") // an analyst writes intent, not repositories
		if err := item(analyst, "k2"); err != nil {
			t.Fatalf("an analyst creates work items: %v", err)
		}
		mustCode(t, repo(stranger, "k3"), "not_found") // no binding: the project is not visible
		mustCode(t, item(stranger, "k3"), "not_found")
		if len(f.w.repos) != 0 {
			t.Fatal("a refused call created a repository")
		}
		// A viewer reads, a seed needs engineer.
		if err := repo(eng, "k4"); err != nil {
			t.Fatal(err)
		}
		var id string
		for k := range f.w.repos {
			id = k
		}
		if _, err := f.svc.GetRepository(ctx, viewer, RepositoryGetIn{ProjectID: string(projP), RepositoryID: id}); err != nil {
			t.Fatalf("a viewer reads: %v", err)
		}
		_, err := f.svc.RequestSeed(ctx, viewer, SeedIn{ProjectID: string(projP), RepositoryID: id})
		mustCode(t, err, "forbidden")
		if f.w.seeds != 0 {
			t.Fatal("a viewer obtained a seed grant")
		}
		// Only a tenant administrator creates projects; an engineer cannot.
		_, err = f.svc.CreateProject(ctx, eng, ProjectCreateIn{IdempotencyKey: "pk", Key: "x"})
		mustCode(t, err, "forbidden")
	})

	t.Run("tenant and project come from the caller, never from arguments", func(t *testing.T) {
		f := newFixture(t)
		eng := personCaller(tenantA, "erin")
		f.w.grant(eng, "*", "engineer")
		// A project of another tenant is not found, whatever the spelling.
		_, err := f.svc.CreateRepository(ctx, eng, RepositoryCreateIn{ProjectID: "p_foreign", IdempotencyKey: "k", Name: "x"})
		mustCode(t, err, "not_found")
		_, err = f.svc.GetWorkItem(ctx, eng, WorkItemGetIn{ProjectID: "p_foreign", WorkItemID: "wi_1"})
		mustCode(t, err, "not_found")
		for _, sc := range f.w.sawScope {
			if sc.TenantID != tenantA {
				t.Fatalf("a port saw tenant %q for a tenant %q caller", sc.TenantID, tenantA)
			}
		}
		// The caller handed to the port is exactly the authenticated one.
		if f.w.lastCall != eng {
			t.Fatalf("port saw %+v, want %+v", f.w.lastCall, eng)
		}
		// An unauthenticated or malformed principal never reaches a port.
		before := len(f.w.sawCalls)
		for _, bad := range []Caller{
			{},
			{Actor: eng.Actor},
			{Tenant: tenantA},
			{Actor: eng.Actor, Tenant: "bad tenant"},
			{Actor: attemptCaller(tenantA, projP, "a").Actor, Tenant: tenantA}, // a workload without its project
		} {
			_, err := f.svc.ListRepositories(ctx, bad, RepositoryListIn{ProjectID: string(projP)})
			mustCode(t, err, "invalid_credential")
		}
		if len(f.w.sawCalls) != before {
			t.Fatal("an unauthenticated caller reached a port")
		}
	})

	t.Run("a run's attempt stays inside its project and gains no tenant authority", func(t *testing.T) {
		f := newFixture(t)
		a := attemptCaller(tenantA, projP, "att1")
		// Omitting the project selects its own; naming another is not found.
		r, err := f.svc.CreateRepository(ctx, a, RepositoryCreateIn{IdempotencyKey: "k1", Name: "svc"})
		if err != nil {
			t.Fatal(err)
		}
		if got := f.w.repos[r.ID].scope; got != (domain.Scope{TenantID: tenantA, ProjectID: projP}) {
			t.Fatalf("created in %v", got)
		}
		_, err = f.svc.CreateRepository(ctx, a, RepositoryCreateIn{ProjectID: string(projQ), IdempotencyKey: "k2", Name: "svc"})
		mustCode(t, err, "not_found")
		_, err = f.svc.ListWorkItems(ctx, a, WorkItemListIn{ProjectID: string(projQ)})
		mustCode(t, err, "not_found")
		_, err = f.svc.GetRepository(ctx, a, RepositoryGetIn{ProjectID: "p_foreign", RepositoryID: r.ID})
		mustCode(t, err, "not_found")

		// It may not administer projects, request seeds or start runs: its seed
		// authority comes from the runtime route, for repositories its run created.
		before := len(f.w.sawCalls)
		_, err = f.svc.CreateProject(ctx, a, ProjectCreateIn{IdempotencyKey: "k", Key: "x"})
		mustCode(t, err, "forbidden")
		_, err = f.svc.GetProject(ctx, a, ProjectGetIn{ProjectID: string(projP)})
		mustCode(t, err, "forbidden")
		_, err = f.svc.ListProjects(ctx, a, ProjectListIn{})
		mustCode(t, err, "forbidden")
		_, err = f.svc.RequestSeed(ctx, a, SeedIn{ProjectID: string(projP), RepositoryID: r.ID})
		mustCode(t, err, "forbidden")
		_, err = f.svc.StartProvisioningRun(ctx, a, ProvisioningStartIn{ProjectID: string(projP), IdempotencyKey: "k", DefinitionID: "def"})
		mustCode(t, err, "forbidden")
		if len(f.w.sawCalls) != before || f.w.seeds != 0 || len(f.w.runs) != 0 {
			t.Fatalf("a refused attempt call reached a port: %v", f.w.sawCalls[before:])
		}
	})

	t.Run("an authority outage fails closed", func(t *testing.T) {
		f := newFixture(t)
		eng := personCaller(tenantA, "erin")
		f.w.grant(eng, projP, "engineer")
		f.w.failAuthority = true
		// The port reports an outage as a plain error; the tool answers
		// unavailable and creates nothing.
		_, err := f.svc.CreateRepository(ctx, eng, RepositoryCreateIn{ProjectID: string(projP), IdempotencyKey: "k", Name: "svc"})
		mustCode(t, err, "unavailable")
		_, err = f.svc.CreateWorkItem(ctx, eng, WorkItemCreateIn{ProjectID: string(projP), IdempotencyKey: "k", Title: "t"})
		mustCode(t, err, "unavailable")
		if len(f.w.repos)+len(f.w.items) != 0 {
			t.Fatal("created under an authority outage")
		}
	})

	t.Run("a replay rechecks current authority and never answers from storage", func(t *testing.T) {
		f := newFixture(t)
		eng := personCaller(tenantA, "erin")
		f.w.grant(eng, projP, "engineer")
		in := WorkItemCreateIn{ProjectID: string(projP), IdempotencyKey: "k", Title: "secret plan"}
		first, err := f.svc.CreateWorkItem(ctx, eng, in)
		if err != nil {
			t.Fatal(err)
		}
		f.w.grant(eng, projP, "") // access revoked
		_, err = f.svc.CreateWorkItem(ctx, eng, in)
		mustCode(t, err, "not_found")
		f.w.grant(eng, projP, "viewer") // downgraded: reads, still cannot create
		again, err := f.svc.CreateWorkItem(ctx, eng, in)
		if err != nil || again.ID != first.ID || !again.Replayed {
			t.Fatalf("a viewer may reread: %v %+v", err, again)
		}
	})

	t.Run("failures carry stable codes and no dependency detail", func(t *testing.T) {
		f := newFixture(t)
		eng := personCaller(tenantA, "erin")
		f.w.grant(eng, projP, "engineer")
		secret := errors.New("dial tcp 10.1.2.3:5432: password=hunter2")
		for _, err := range []error{secret, errors.Join(ErrDenied, secret), errors.Join(ErrNotFound, secret), errors.Join(ErrInvalid, secret), errors.Join(ErrConflict, secret), errors.Join(ErrUnavailable, secret)} {
			code, msg := classify(err)
			if code == "" || strings.Contains(msg, "hunter2") || strings.Contains(msg, "10.1.2.3") {
				t.Errorf("%v leaked: %s %s", err, code, msg)
			}
		}
	})

	t.Run("the seed credential is returned once and never stored or logged", func(t *testing.T) {
		f := newFixture(t)
		var logs bytes.Buffer
		svc, err := NewService(Config{Repositories: fakeRepos{f.w}, Projects: fakeProjects{f.w}, WorkItems: fakeItems{f.w}, Provisioning: fakeProv{f.w}, Results: f.res,
			Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
		if err != nil {
			t.Fatal(err)
		}
		eng := personCaller(tenantA, "erin")
		f.w.grant(eng, projP, "engineer")
		r, err := svc.CreateRepository(ctx, eng, RepositoryCreateIn{ProjectID: string(projP), IdempotencyKey: "k", Name: "svc"})
		if err != nil {
			t.Fatal(err)
		}
		seed, err := svc.RequestSeed(ctx, eng, SeedIn{ProjectID: string(projP), RepositoryID: r.ID})
		if err != nil || seed.Password == "" {
			t.Fatalf("%v %+v", err, seed)
		}
		_, _ = svc.RequestSeed(ctx, personCaller(tenantA, "sam"), SeedIn{ProjectID: string(projP), RepositoryID: r.ID})
		if strings.Contains(logs.String(), seed.Password) {
			t.Fatalf("the credential reached a log:\n%s", logs.String())
		}
		f.res.mu.Lock()
		defer f.res.mu.Unlock()
		for k, rec := range f.res.m {
			if strings.Contains(rec.ResourceID+rec.RequestHash+k.Key, seed.Password) {
				t.Fatal("the credential reached the idempotency store")
			}
		}
	})
}
