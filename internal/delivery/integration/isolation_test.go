package integration_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	"github.com/optimiware/kodama/internal/domain"
)

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// seeded is one scope's delivery data: an environment, its retained snapshot, a
// registered deployment and its history.
type seeded struct {
	scope domain.Scope
	d     *deliverytest.Deploy
	id    delivery.DeploymentID
	req   delivery.RegistrationRequest
}

// seedScope creates a kubeconfig secret, an environment, an admitted deploy run
// and a registered, applying deployment in the scope.
func seedScope(t *testing.T, s *stack, scope domain.Scope) seeded {
	t.Helper()
	s.Kubeconfig(scope, "kubeconfig", "KUBE-SENTINEL-"+string(scope.ProjectID))
	env := s.Environment(scope, "preview", "kubeconfig")
	d := s.NewDeploy(scope, env, deliverytest.FlowOptions{})
	req := delivery.RegistrationRequest{Source: d.Source, Image: delivery.Image{ArtifactID: d.Image, Digest: d.ImageDigest}}
	rc, err := s.Svc.RegisterRuntime(context.Background(), d.Deploy.ID(), d.Deploy.Token, req)
	if err != nil {
		t.Fatalf("seed %s: %v", scope.ProjectID, err)
	}
	if _, err := s.Svc.ReportOutcome(context.Background(), d.Deploy.ID(), d.Deploy.Token, rc.Deployment.ID(),
		delivery.OutcomeReport{State: delivery.OutcomeApplying, Objects: objects(deliverytest.TargetNamespace, "api")}); err != nil {
		t.Fatalf("seed %s: %v", scope.ProjectID, err)
	}
	return seeded{scope: scope, d: d, id: rc.Deployment.ID(), req: req}
}

var deliveryTables = []string{"environments", "environment_snapshots", "deployments", "deployment_history"}

// Test_delivery_rls proves the persistence boundary directly in SQL, as the
// runtime role the application uses (not an owner, no BYPASSRLS): forced row
// level security on every delivery table, denial without a tenant context,
// cross-tenant read and write isolation, same-tenant project scoping through
// composite ownership and the module's scoped queries, and the immutability of
// snapshots, history and a deployment's identity and settled outcome. Real
// PostgreSQL.
func Test_delivery_rls(t *testing.T) {
	ctx := context.Background()
	s := newStack(t, deliverytest.Options{}, deliverytest.FlowOptions{})
	a := seeded{scope: scopeA, d: s.D, req: s.Req, id: s.mustRegister(t).Deployment.ID()}
	s.apply(t, a.id, objects(deliverytest.TargetNamespace, "api"))
	a2 := seedScope(t, s, deliverytest.ScopeA2)
	b := seedScope(t, s, deliverytest.ScopeB)

	inTenant := func(tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
		return s.X.RuntimeTx(tenant, fn)
	}
	count := func(tenant domain.TenantID, table string) int {
		t.Helper()
		var n int
		if err := inTenant(tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	ownerCount := func(table, tenant string) int {
		return s.count(`SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, tenant)
	}

	t.Run("the runtime role is a plain application role and every table forces RLS", func(t *testing.T) {
		var super, bypass bool
		var owner bool
		if err := s.X.Runtime.QueryRow(ctx, `SELECT r.rolsuper, r.rolbypassrls, (r.rolname = (SELECT tableowner FROM pg_tables WHERE tablename = 'deployments'))
FROM pg_roles r WHERE r.rolname = current_user`).Scan(&super, &bypass, &owner); err != nil {
			t.Fatal(err)
		}
		if super || bypass || owner {
			t.Fatalf("the runtime role is privileged: super=%v bypassrls=%v owner=%v", super, bypass, owner)
		}
		for _, table := range deliveryTables {
			var enabled, forced bool
			s.X.Scalar(&enabled, `SELECT relrowsecurity FROM pg_class WHERE relname = $1 AND relkind = 'r'`, table)
			s.X.Scalar(&forced, `SELECT relforcerowsecurity FROM pg_class WHERE relname = $1 AND relkind = 'r'`, table)
			if !enabled || !forced {
				t.Errorf("%s: row level security enabled=%v forced=%v", table, enabled, forced)
			}
		}
	})

	t.Run("an absent tenant context sees and writes nothing", func(t *testing.T) {
		for _, table := range deliveryTables {
			var n int
			if err := s.X.Runtime.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 0 {
				t.Errorf("%s without context: %d rows, %v", table, n, err)
			}
		}
		_, err := s.X.Runtime.Exec(ctx, `INSERT INTO environments (tenant_id, project_id, id, name, kind, target, namespace, kubeconfig, preview_ttl_seconds, version, created_at, updated_at)
VALUES ($1, $2, 'env_t035_ctxless', 'ctxless', 'stage', 'kind', 'ns', '{}', 0, 1, now(), now())`, string(scopeA.TenantID), string(scopeA.ProjectID))
		if pgCode(err) != "42501" {
			t.Errorf("an insert without tenant context: %v", err)
		}
		tag, err := s.X.Runtime.Exec(ctx, `UPDATE deployments SET reason = 'applying' WHERE true`)
		if err != nil || tag.RowsAffected() != 0 {
			t.Errorf("an update without tenant context touched %d rows: %v", tag.RowsAffected(), err)
		}
	})

	t.Run("each tenant reads exactly its own rows", func(t *testing.T) {
		for _, table := range deliveryTables {
			for _, tenant := range []domain.TenantID{scopeA.TenantID, deliverytest.ScopeB.TenantID} {
				if got, want := count(tenant, table), ownerCount(table, string(tenant)); got != want || want == 0 {
					t.Errorf("%s as %s: sees %d rows, the tenant owns %d", table, tenant, got, want)
				}
			}
			if got := count("t035_nobody", table); got != 0 {
				t.Errorf("%s: an unknown tenant sees %d rows", table, got)
			}
		}
		// And by identity: a foreign deployment, snapshot and history are invisible.
		err := inTenant(scopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var n int
			for _, q := range []string{
				`SELECT count(*) FROM deployments WHERE id = '` + string(b.id) + `'`,
				`SELECT count(*) FROM deployment_history WHERE deployment_id = '` + string(b.id) + `'`,
				`SELECT count(*) FROM environment_snapshots WHERE environment_id = '` + string(b.d.Env.ID()) + `'`,
				`SELECT count(*) FROM environments WHERE id = '` + string(b.d.Env.ID()) + `'`,
			} {
				if err := tx.QueryRow(ctx, q).Scan(&n); err != nil || n != 0 {
					t.Errorf("%s: %d %v", q, n, err)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a tenant cannot write or change another tenant's rows", func(t *testing.T) {
		cols := columns(t, s, "deployments")
		// An insert for another tenant under this tenant's context: the policy's
		// WITH CHECK refuses it.
		err := inTenant(scopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, cloneDeployment(cols, map[string]string{"tenant_id": "$2", "project_id": "$3", "id": "'dep_t035_x'", "action_key": "'t035:x'"}),
				string(a.id), string(deliverytest.ScopeB.TenantID), string(deliverytest.ScopeB.ProjectID))
			return err
		})
		if pgCode(err) != "42501" {
			t.Errorf("a cross-tenant insert: %v", err)
		}
		// Updating and deleting a foreign row touches nothing.
		_ = inTenant(scopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE deployments SET reason = 'applying', version = version + 1 WHERE id = $1`, string(b.id))
			if err != nil || tag.RowsAffected() != 0 {
				t.Errorf("a cross-tenant update touched %d rows: %v", tag.RowsAffected(), err)
			}
			return nil
		})
		if s.count(`SELECT count(*) FROM deployments WHERE id = $1 AND version = 2`, string(b.id)) != 1 {
			t.Fatal("the foreign deployment changed")
		}
		// An environment of this tenant cannot be rehomed into another tenant.
		err = inTenant(scopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE environments SET tenant_id = $2 WHERE id = $1`, string(s.D.Env.ID()), string(deliverytest.ScopeB.TenantID))
			return err
		})
		if err == nil {
			t.Error("an environment was moved to another tenant")
		}
	})

	t.Run("same-tenant projects are separated by composite ownership and scoped queries", func(t *testing.T) {
		cols := columns(t, s, "deployments")
		// A deployment row of project A2 cannot reference project A's environment
		// snapshot, originating attempt or image: the composite keys name the project.
		err := inTenant(scopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, cloneDeployment(cols, map[string]string{"project_id": "$2", "id": "'dep_t035_x'", "action_key": "'t035:x'"}),
				string(a.id), string(deliverytest.ScopeA2.ProjectID))
			return err
		})
		if pgCode(err) != "23503" {
			t.Errorf("a cross-project deployment: %v", err)
		}
		// Likewise an environment of one project under a project that does not exist
		// in the tenant, and history for a deployment of another project.
		err = inTenant(scopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO deployment_history (tenant_id, project_id, deployment_id, kind, version, from_status, to_status, reason, actor_type, actor_id, occurred_at)
VALUES ($1, $2, $3, 'x', 1, 'a', 'b', 'c', 'user', 'u', now())`, string(scopeA.TenantID), string(deliverytest.ScopeA2.ProjectID), string(a.id))
			return err
		})
		if pgCode(err) != "23503" {
			t.Errorf("history under another project: %v", err)
		}
		err = inTenant(scopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO environments (tenant_id, project_id, id, name, kind, target, namespace, kubeconfig, preview_ttl_seconds, version, created_at, updated_at)
VALUES ($1, 'p035_missing', 'env_t035_orphan', 'orphan', 'stage', 'kind', 'ns', '{}', 0, 1, now(), now())`, string(scopeA.TenantID))
			return err
		})
		if pgCode(err) != "23503" {
			t.Errorf("an environment of an unknown project: %v", err)
		}

		// The module's queries are project-scoped inside one tenant.
		if _, err := s.Store.GetDeployment(ctx, a2.scope, a.id); !errors.Is(err, delivery.ErrNotFound) {
			t.Errorf("GetDeployment across projects: %v", err)
		}
		if _, err := s.Store.FindByAction(ctx, a2.scope, a.d.Deploy.Context.ActionKey); !errors.Is(err, delivery.ErrNotFound) {
			t.Errorf("FindByAction across projects: %v", err)
		}
		if _, err := s.Store.GetEnvironment(ctx, a2.scope, a.d.Env.ID()); !errors.Is(err, delivery.ErrNotFound) {
			t.Errorf("GetEnvironment across projects: %v", err)
		}
		pin := a.d.Deploy.Context.Environment
		if _, err := s.Store.GetSnapshot(ctx, a2.scope, pin.ID, pin.ConfigDigest); !errors.Is(err, delivery.ErrNotFound) {
			t.Errorf("GetSnapshot across projects: %v", err)
		}
		if h, err := s.Store.History(ctx, a2.scope, a.id); err != nil || len(h) != 0 {
			t.Errorf("History across projects: %v %d", err, len(h))
		}
		if l, err := s.Store.ListDeployments(ctx, a2.scope, delivery.DeploymentFilter{}, "", 50); err != nil || len(l) != 1 || l[0].ID() != a2.id {
			t.Errorf("ListDeployments in the sibling project: %v %v", err, l)
		}
		if l, err := s.Store.ListEnvironments(ctx, a2.scope, "", 50); err != nil || len(l) != 1 || l[0].ID() != a2.d.Env.ID() {
			t.Errorf("ListEnvironments in the sibling project: %v %v", err, l)
		}
		// A mutation through the other project's scope finds nothing to change.
		_, _, err = s.Store.Update(ctx, a2.scope, a.id, delivery.Mutation{Actor: delivery.Actor{Type: delivery.ActorSystem, ID: "t035"}, Now: s.X.Clock.Now()},
			func(d *delivery.Deployment) (delivery.Transition, error) { return delivery.Transition{}, nil })
		if !errors.Is(err, delivery.ErrNotFound) {
			t.Errorf("Update across projects: %v", err)
		}
		// The service: a sibling project's registration cannot be observed by
		// project A's attempt and vice versa (the logical action is per project).
		if a.id == a2.id {
			t.Error("two projects share a deployment identity")
		}
	})

	t.Run("snapshots and history are immutable and nothing is ever deleted", func(t *testing.T) {
		before := ownerCount("deployments", string(scopeA.TenantID))
		for name, q := range map[string]string{
			"snapshot update":    `UPDATE environment_snapshots SET namespace = 'changed' WHERE tenant_id = $1`,
			"snapshot delete":    `DELETE FROM environment_snapshots WHERE tenant_id = $1`,
			"history update":     `UPDATE deployment_history SET reason = 'edited' WHERE tenant_id = $1`,
			"history delete":     `DELETE FROM deployment_history WHERE tenant_id = $1`,
			"deployment delete":  `DELETE FROM deployments WHERE tenant_id = $1`,
			"environment delete": `DELETE FROM environments WHERE tenant_id = $1`,
		} {
			// The owner is subject to the immutability triggers; the runtime role
			// additionally has no privilege.
			// (Deleting a deployment or an environment also fails for the owner: history
			// and deployments reference them.)
			if err := s.X.OwnerExecErr(q, string(scopeA.TenantID)); err == nil {
				t.Errorf("owner: %s succeeded", name)
			}
			err := inTenant(scopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, q, string(scopeA.TenantID))
				return err
			})
			if pgCode(err) != "42501" {
				t.Errorf("runtime role: %s: %v", name, err)
			}
		}
		if n := ownerCount("deployments", string(scopeA.TenantID)); n != before || n != 2 {
			t.Fatalf("a delete removed rows: %d (was %d)", n, before)
		}
	})

	t.Run("a deployment's identity, provenance and settled outcome cannot be rewritten", func(t *testing.T) {
		// Runtime role: only the mutable columns are granted.
		for _, col := range []string{"image_digest", "config_digest", "action_key", "attempt_id", "request_fingerprint", "expires_at", "source_commit", "tool_image"} {
			err := inTenant(scopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE deployments SET %s = %s WHERE id = $1`, col, col), string(a.id))
				return err
			})
			if pgCode(err) != "42501" {
				t.Errorf("runtime role updated %s: %v", col, err)
			}
		}
		// Owner: the triggers refuse an identity or provenance rewrite, an invalid
		// transition and a version that does not increase.
		for name, q := range map[string]string{
			"image digest":    `UPDATE deployments SET image_digest = 'sha256:` + strings.Repeat("d", 64) + `', version = version + 1 WHERE id = $1`,
			"source":          `UPDATE deployments SET source_commit = '` + strings.Repeat("9", 40) + `', version = version + 1 WHERE id = $1`,
			"expiry":          `UPDATE deployments SET expires_at = now(), version = version + 1 WHERE id = $1`,
			"version":         `UPDATE deployments SET reason = 'applying' WHERE id = $1`,
			"back to pending": `UPDATE deployments SET status = 'pending', reason = 'registered', version = version + 1 WHERE id = $1`,
		} {
			if err := s.X.OwnerExecErr(q, string(a.id)); pgCode(err) != "23514" {
				t.Errorf("%s: %v", name, err)
			}
		}
		// Settle it, then a settled outcome cannot change even for the owner.
		if _, err := s.report(s.D.Deploy, a.id, delivery.OutcomeReport{State: delivery.OutcomeFailed, Reason: "apply_failed"}); err != nil {
			t.Fatal(err)
		}
		for name, q := range map[string]string{
			"outcome":      `UPDATE deployments SET status = 'succeeded', reason = 'ready', version = version + 1 WHERE id = $1`,
			"objects":      `UPDATE deployments SET objects = '[]', version = version + 1 WHERE id = $1`,
			"failure code": `UPDATE deployments SET reason = 'other_reason', version = version + 1 WHERE id = $1`,
		} {
			if err := s.X.OwnerExecErr(q, string(a.id)); pgCode(err) != "23514" {
				t.Errorf("settled %s: %v", name, err)
			}
		}
		// One deployment per logical action, whatever the identifier.
		cols := columns(t, s, "deployments")
		err := inTenant(scopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, cloneDeployment(cols, map[string]string{"id": "'dep_t035_dup'"}), string(a.id))
			return err
		})
		if pgCode(err) != "23505" {
			t.Errorf("a duplicate action key: %v", err)
		}
	})

	t.Run("environment identity is immutable and its secrets are references only", func(t *testing.T) {
		for name, q := range map[string]string{
			"name":    `UPDATE environments SET name = 'renamed', version = version + 1 WHERE id = $1`,
			"kind":    `UPDATE environments SET kind = 'stage', preview_ttl_seconds = 0, version = version + 1 WHERE id = $1`,
			"version": `UPDATE environments SET target = 'other' WHERE id = $1`,
		} {
			if err := s.X.OwnerExecErr(q, string(a.d.Env.ID())); pgCode(err) != "23514" {
				t.Errorf("%s: %v", name, err)
			}
		}
		// No secret value is stored anywhere in the delivery tables.
		for _, table := range deliveryTables {
			if n := s.count(`SELECT count(*) FROM ` + table + ` t WHERE t::text LIKE '%KUBE-SENTINEL%'`); n != 0 {
				t.Errorf("%s holds a credential value", table)
			}
		}
	})
}

// columns lists the columns of a table in order.
func columns(t *testing.T, s *stack, table string) []string {
	t.Helper()
	rows, err := s.X.Migration.Query(context.Background(),
		`SELECT column_name FROM information_schema.columns WHERE table_name = $1 AND table_schema = current_schema() AND is_generated = 'NEVER' ORDER BY ordinal_position`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		t.Fatalf("no columns for %s", table)
	}
	return out
}

// cloneDeployment builds INSERT ... SELECT copying the deployment $1 with the
// listed columns replaced by SQL expressions.
func cloneDeployment(cols []string, replace map[string]string) string {
	sel := make([]string, len(cols))
	for i, c := range cols {
		if r, ok := replace[c]; ok {
			sel[i] = r
		} else {
			sel[i] = c
		}
	}
	return `INSERT INTO deployments (` + strings.Join(cols, ", ") + `) SELECT ` + strings.Join(sel, ", ") + ` FROM deployments WHERE id = $1`
}
