package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/domain"
)

func TestProfileRegistration(t *testing.T) {
	e := newEnv(t)

	first := e.mustRegister(scopeA, engineer, e.draft)
	if !first.Created || first.Profile.Version != 1 || first.Profile.Name != "coding" || first.Profile.Ref() != "coding@1" {
		t.Fatalf("first: %+v", first)
	}
	if first.Profile.Digest != first.Profile.Content.Digest() {
		t.Fatal("the digest identifies the content")
	}
	if e.profiles(scopeA) != 1 || e.audits(scopeA) != 1 {
		t.Fatalf("one profile and one audit row: %d %d", e.profiles(scopeA), e.audits(scopeA))
	}

	t.Run("identical content converges and writes nothing", func(t *testing.T) {
		again := e.mustRegister(scopeA, engineer, e.draft)
		if again.Created || again.Profile.ID != first.Profile.ID {
			t.Fatalf("again: %+v", again)
		}
		if e.profiles(scopeA) != 1 || e.audits(scopeA) != 1 {
			t.Fatal("a duplicate creates no version and no audit row")
		}
	})

	t.Run("a different registrant still converges on the same version", func(t *testing.T) {
		svc := e.mustRegister(scopeA, agentSvc, e.draft)
		if svc.Created || svc.Profile.ID != first.Profile.ID || svc.Profile.CreatedBy.ID != engineer {
			t.Fatalf("the original attribution is kept: %+v", svc)
		}
	})

	t.Run("changed content is the next version of its name", func(t *testing.T) {
		second := e.mustRegister(scopeA, engineer, e.variant(2))
		if !second.Created || second.Profile.Version != 2 || second.Profile.Ref() != "coding@2" {
			t.Fatalf("second: %+v", second)
		}
		other := agentstestDraftNamed(t, e, "review")
		third := e.mustRegister(scopeA, agentSvc, other)
		if !third.Created || third.Profile.Version != 1 || third.Profile.CreatedBy.Type != agents.ActorService {
			t.Fatalf("another name starts at version 1, attributed to its actor: %+v", third)
		}
	})

	t.Run("reads are exact and scoped", func(t *testing.T) {
		ctx := context.Background()
		got, err := e.store.Get(ctx, scopeA, "coding", 1)
		if err != nil || got.ID != first.Profile.ID || got.Digest != first.Profile.Digest || string(got.Content.Canonical()) != string(first.Profile.Content.Canonical()) {
			t.Fatalf("get: %+v %v", got, err)
		}
		if _, err := e.store.Get(ctx, scopeA, "coding", 99); !errors.Is(err, agents.ErrNotFound) {
			t.Fatalf("unknown version: %v", err)
		}
		if _, err := e.store.Get(ctx, scopeA2, "coding", 1); !errors.Is(err, agents.ErrNotFound) {
			t.Fatalf("another project of the tenant: %v", err)
		}
		if _, err := e.store.Get(ctx, scopeB, "coding", 1); !errors.Is(err, agents.ErrNotFound) {
			t.Fatalf("another tenant: %v", err)
		}
	})

	t.Run("listing pages in creation order and filters by name", func(t *testing.T) {
		ctx := context.Background()
		all, err := e.store.List(ctx, scopeA, agents.ListQuery{Limit: 10})
		if err != nil || len(all) != 3 {
			t.Fatalf("list: %d %v", len(all), err)
		}
		for i := 1; i < len(all); i++ {
			if all[i].CreatedAt.Before(all[i-1].CreatedAt) {
				t.Fatal("(created_at, id) order")
			}
		}
		page1, err := e.store.List(ctx, scopeA, agents.ListQuery{Limit: 2})
		if err != nil || len(page1) != 2 {
			t.Fatalf("page 1: %d %v", len(page1), err)
		}
		last := page1[1]
		page2, err := e.store.List(ctx, scopeA, agents.ListQuery{Limit: 2, After: &agents.Position{CreatedAt: last.CreatedAt, ID: last.ID}})
		if err != nil || len(page2) != 1 || page2[0].ID != all[2].ID {
			t.Fatalf("page 2: %v %v", page2, err)
		}
		named, err := e.store.List(ctx, scopeA, agents.ListQuery{Limit: 10, Name: "review"})
		if err != nil || len(named) != 1 || named[0].Name != "review" {
			t.Fatalf("name filter: %v %v", named, err)
		}
		if _, err := e.store.List(ctx, scopeA, agents.ListQuery{Limit: 0}); !errors.Is(err, agents.ErrInvalid) {
			t.Fatalf("an invalid page size: %v", err)
		}
	})

	t.Run("the audit row is attributable and content free", func(t *testing.T) {
		var actorType, actorID, action, resource, req string
		var meta []byte
		var version int64
		if err := e.owner.QueryRow(context.Background(), `SELECT actor_type, actor_id, action, resource_type, resource_version, request_id, metadata FROM audit
			WHERE tenant_id = $1 AND resource_id = $2`, tenantA, string(first.Profile.ID)).Scan(&actorType, &actorID, &action, &resource, &version, &req, &meta); err != nil {
			t.Fatal(err)
		}
		if actorType != "user" || actorID != engineer || action != "agent_profile.registered" || resource != "agent_profile" || version != 1 || req != "req_"+prefix {
			t.Fatalf("audit: %s %s %s %s %d %s", actorType, actorID, action, resource, version, req)
		}
		var m map[string]any
		if err := json.Unmarshal(meta, &m); err != nil || len(m) != 3 || m["name"] != "coding" || m["digest"] != string(first.Profile.Digest) {
			t.Fatalf("audit metadata must hold the name, version and digest only: %s", meta)
		}
		var svcType string
		if err := e.owner.QueryRow(context.Background(), `SELECT actor_type FROM audit WHERE actor_id = $1`, agentSvc).Scan(&svcType); err != nil || svcType != "system" {
			t.Fatalf("the service actor is audited as system: %q %v", svcType, err)
		}
	})

	t.Run("an unknown project is not found", func(t *testing.T) {
		ghost := domain.Scope{TenantID: tenantA, ProjectID: "p030_ghost"}
		e.auth.Grant(ghost, engineer, agents.ActorUser, agents.PermRegister)
		if _, err := e.register(ghost, engineer, e.draft); !errors.Is(err, agents.ErrNotFound) {
			t.Fatalf("expected not found, got %v", err)
		}
	})

	t.Run("the store rejects malformed candidates", func(t *testing.T) {
		long := make([]byte, 125)
		for i := range long {
			long[i] = 'a'
		}
		_, _, err := e.store.RegisterOrGet(context.Background(), agents.Candidate{
			Scope: scopeA, ID: agents.ProfileID("ap_" + string(long)), Content: first.Profile.Content,
			CreatedBy: agents.Actor{ID: engineer, Type: agents.ActorUser}, CreatedAt: first.Profile.CreatedAt})
		if !errors.Is(err, agents.ErrInvalid) {
			t.Fatalf("an over-long identifier must be rejected: %v", err)
		}
		if _, _, err = e.store.RegisterOrGet(context.Background(), agents.Candidate{}); !errors.Is(err, agents.ErrInvalid) {
			t.Fatalf("an empty candidate must be rejected: %v", err)
		}
	})
}

func agentstestDraftNamed(t testing.TB, e *env, name string) agents.Draft {
	t.Helper()
	d := e.draft
	d.Name = name
	return d
}
