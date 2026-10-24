package postgres_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/metrics"
	metricsauth "github.com/optimiware/kodama/internal/metrics/auth"
	"github.com/optimiware/kodama/internal/work"
)

// scopedAuthority grants work_item:read to principals only in the scopes listed;
// it stands in for the identity authority behind work.Authority.
type scopedAuthority struct {
	read   map[string]map[domain.Scope]bool
	outage bool
}

func (a *scopedAuthority) ResolveActor(_ context.Context, scope domain.Scope, principal string) (work.Actor, error) {
	if a.outage {
		return work.Actor{}, errors.New("authority down")
	}
	if len(a.read[principal]) == 0 {
		return work.Actor{}, work.ErrHidden
	}
	return work.Actor{ID: principal, Type: work.ActorUser}, nil
}

func (a *scopedAuthority) Authorize(_ context.Context, actor work.Actor, scope domain.Scope, p work.Permission) error {
	if p != work.PermRead || !a.read[actor.ID][scope] {
		return work.ErrDenied
	}
	return nil
}

// countingReader counts the calls that reach the real store.
type countingReader struct {
	inner metrics.SummaryReader
	calls atomic.Int32
}

func (r *countingReader) ReadSummaries(ctx context.Context, s domain.Scope, ids []domain.WorkItemID, w metrics.Window) ([]metrics.WorkSummary, error) {
	r.calls.Add(1)
	return r.inner.ReadSummaries(ctx, s, ids, w)
}

// Test_metrics_tenant_scope: authorization precedes SQL on every path; copied
// identifiers from another tenant or project disclose nothing and look exactly
// like missing ones; runtime queries and direct view access are bounded by row
// level security; a pooled connection carries no tenant context between
// transactions. IT-03, IT-04, IT-05, WM-12.
func Test_metrics_tenant_scope(t *testing.T) {
	e := newEnv(t)
	itemA := e.toActive(e.create(scopeA, at(0), "tenant A"), at(mins(1)), at(mins(2)))
	itemA2 := e.toActive(e.create(scopeA2, at(0), "tenant A, other project"), at(mins(1)), at(mins(2)))
	itemB := e.toActive(e.create(scopeB, at(0), "tenant B"), at(mins(1)), at(mins(2)))
	w := window(at(0), at(mins(60)))

	authority := &scopedAuthority{read: map[string]map[domain.Scope]bool{
		"u_a":      {scopeA: true},
		"u_b":      {scopeB: true},
		"u_a_only": {scopeA2: true},
	}}
	authz, err := metricsauth.NewWorkReadAuthorizer(authority)
	if err != nil {
		t.Fatal(err)
	}
	reader := &countingReader{inner: e.store}
	svc, err := metrics.NewService(metrics.Config{Reader: reader, Authorizer: authz, Clock: func() time.Time { return at(mins(60)) }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	t.Run("authorization_precedes_sql", func(t *testing.T) {
		before := reader.calls.Load()
		for name, c := range map[string]struct {
			scope     domain.Scope
			principal string
			want      error
		}{
			"principal of another tenant":  {scopeA, "u_b", metrics.ErrDenied},
			"principal of another project": {scopeA, "u_a_only", metrics.ErrDenied},
			"unknown principal":            {scopeA, "u_nobody", metrics.ErrHidden},
			"read in another tenant":       {scopeB, "u_a", metrics.ErrDenied},
		} {
			got, err := svc.Summarize(ctx, c.scope, c.principal, []domain.WorkItemID{itemA.ID, itemB.ID}, w)
			if !errors.Is(err, c.want) || got != nil {
				t.Errorf("%s: %v %v", name, got, err)
			}
		}
		authority.outage = true
		if got, err := svc.Summarize(ctx, scopeA, "u_a", []domain.WorkItemID{itemA.ID}, w); !errors.Is(err, metrics.ErrUnavailable) || got != nil {
			t.Errorf("authority outage: %v %v", got, err)
		}
		authority.outage = false
		if n := reader.calls.Load(); n != before {
			t.Fatalf("%d reads reached SQL without authorization", n-before)
		}
		got, err := svc.Summarize(ctx, scopeA, "u_a", []domain.WorkItemID{itemA.ID}, w)
		if err != nil || len(got) != 1 || got[0].ItemID != itemA.ID || got[0].Scope != scopeA {
			t.Fatalf("authorized read: %+v %v", got, err)
		}
	})

	t.Run("copied_identifiers_disclose_nothing", func(t *testing.T) {
		missing, err := svc.Summarize(ctx, scopeA, "u_a", []domain.WorkItemID{"wi_does_not_exist"}, w)
		if err != nil || len(missing) != 0 {
			t.Fatalf("missing item: %v %v", missing, err)
		}
		for name, id := range map[string]domain.WorkItemID{
			"another tenant's item":                itemB.ID,
			"another project's item (same tenant)": itemA2.ID,
		} {
			got, err := svc.Summarize(ctx, scopeA, "u_a", []domain.WorkItemID{id}, w)
			if err != nil || len(got) != len(missing) || got == nil {
				t.Errorf("%s: %+v %v (a missing item answers %v)", name, got, err, missing)
			}
		}
		// Mixed pages keep the own item and drop the copies.
		got, err := svc.Summarize(ctx, scopeA, "u_a", []domain.WorkItemID{itemB.ID, itemA.ID, itemA2.ID, "wi_none"}, w)
		if err != nil || len(got) != 1 || got[0].ItemID != itemA.ID {
			t.Fatalf("mixed page: %+v %v", got, err)
		}
		// The store itself applies the explicit predicates, so it holds without the service.
		direct, err := e.store.ReadSummaries(ctx, scopeB, []domain.WorkItemID{itemA.ID, itemA2.ID}, w)
		if err != nil || len(direct) != 0 {
			t.Fatalf("direct store read: %+v %v", direct, err)
		}
		wrongProject, err := e.store.ReadSummaries(ctx, scopeA, []domain.WorkItemID{itemA2.ID}, w)
		if err != nil || len(wrongProject) != 0 {
			t.Fatalf("wrong project: %+v %v", wrongProject, err)
		}
	})

	t.Run("views_are_bounded_by_row_level_security", func(t *testing.T) {
		const q = `SELECT count(*) FROM work_flow_segments`
		if n := e.count(tenantA, q); n != 6 { // two items of tenant A, 3 history rows each
			t.Errorf("tenant A sees %d segments", n)
		}
		if n := e.count(tenantB, q); n != 3 {
			t.Errorf("tenant B sees %d segments", n)
		}
		for _, view := range []string{"work_flow_steps", "work_flow_segments", "work_flow_episodes", "work_flow_blocked_runs"} {
			if n := e.count(tenantB, `SELECT count(*) FROM `+view+` WHERE work_item_id = $1`, string(itemA.ID)); n != 0 {
				t.Errorf("tenant B reads %s of tenant A's item: %d rows", view, n)
			}
			// No tenant context: nothing, although the owner sees rows.
			var n int
			if err := e.rt.QueryRow(ctx, `SELECT count(*) FROM `+view).Scan(&n); err != nil || n != 0 {
				t.Errorf("%s without tenant context: %d rows, %v", view, n, err)
			}
		}
		var ownerRows int
		if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM work_flow_segments`).Scan(&ownerRows); err != nil || ownerRows != 9 {
			t.Fatalf("owner baseline: %d %v", ownerRows, err)
		}
	})

	t.Run("a_pooled_connection_keeps_no_tenant_context", func(t *testing.T) {
		one := e.stack.DB.RuntimePool(ctx, 1)
		single, err := newStoreOn(one)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := single.ReadSummaries(ctx, scopeA, []domain.WorkItemID{itemA.ID}, w); err != nil || len(got) != 1 {
			t.Fatalf("tenant A read: %v %v", got, err)
		}
		// The very same connection, outside a transaction, sees no tenant.
		var setting *string
		if err := one.QueryRow(ctx, `SELECT nullif(current_setting('kodama.tenant_id', true), '')`).Scan(&setting); err != nil || setting != nil {
			t.Fatalf("tenant context survived the read: %v %v", setting, err)
		}
		var n int
		if err := one.QueryRow(ctx, `SELECT count(*) FROM work_flow_segments`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("pooled connection reads %d segments without context: %v", n, err)
		}
		// Tenant B right after tenant A on the same connection gets only its own.
		got, err := single.ReadSummaries(ctx, scopeB, []domain.WorkItemID{itemA.ID, itemB.ID}, w)
		if err != nil || len(got) != 1 || got[0].ItemID != itemB.ID {
			t.Fatalf("tenant B after A: %+v %v", got, err)
		}
	})

	t.Run("an_empty_id_set_reads_nothing", func(t *testing.T) {
		got, err := e.store.ReadSummaries(ctx, scopeA, nil, w)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("empty set: %v %v", got, err)
		}
	})

}
