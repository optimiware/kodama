package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/workspace"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// Test_tenant_isolation_and_atomic_events: row level security, pooled tenant
// reset, composite ownership keys, runtime grants, database-enforced
// publication invariants, and rollback on audit or event failure.
func Test_tenant_isolation_and_atomic_events(t *testing.T) {
	ctx := context.Background()
	e := wt.New(t, wt.Config{})
	wa := e.Ready(wt.ScopeA, "wi_a")
	wb := e.Ready(wt.ScopeB, "wi_b")
	e.Writer(wa, "att_1")
	in := e.Proposal(wa, "att_1", e.Repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "a"})
	if _, err := e.Svc.ProposeSource(ctx, in); err != nil {
		t.Fatal(err)
	}
	pin, err := e.Svc.PinCommit(ctx, wt.Engineer, workspace.PinInput{Scope: wa.Scope, WorkspaceID: wa.ID, Commit: workspace.Commit(e.Repo.Initial), RunID: "run_1"})
	if err != nil {
		t.Fatal(err)
	}
	_ = pin

	t.Run("RLS hides every table from other tenants and from no tenant", func(t *testing.T) {
		for _, table := range []string{"workspaces", "workspace_publications", "workspace_retained_refs"} {
			var n int
			if err := e.Runtime.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 0 {
				t.Fatalf("%s without tenant context: %d rows, %v", table, n, err)
			}
			if got := e.CountTenant(wt.ScopeB.TenantID, `SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, string(wt.ScopeA.TenantID)); got != 0 {
				t.Fatalf("%s: tenant B sees %d of tenant A's rows", table, got)
			}
			if got := e.CountTenant(wt.ScopeA.TenantID, `SELECT count(*) FROM `+table); got == 0 {
				t.Fatalf("%s: tenant A sees nothing", table)
			}
		}
		// Writes under the wrong tenant are refused by the policy, not silently dropped.
		err := storage.WithTenant(ctx, e.Runtime, wt.ScopeB.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO workspaces (tenant_id, project_id, id, work_item_id, repository_id, repository_url, checkout_secret_ref, publisher_secret_ref, work_branch)
				VALUES ($1, $2, 'ws_evil', 'wi_x', 'repo', 'https://x/y.git', 'a', 'b', 'refs/heads/x')`, string(wt.ScopeA.TenantID), string(wt.ScopeA.ProjectID))
			return err
		})
		if sqlState(err) != "42501" {
			t.Fatalf("cross-tenant insert: %v", err)
		}
		// The store itself never returns another tenant's workspace.
		if _, err := e.PG.Get(ctx, wb.Scope, wa.ID); !errors.Is(err, workspace.ErrNotFound) {
			t.Fatalf("store crossed tenants: %v", err)
		}
		if _, err := e.PG.Get(ctx, domain.Scope{TenantID: wa.Scope.TenantID, ProjectID: wt.ScopeA2.ProjectID}, wa.ID); !errors.Is(err, workspace.ErrNotFound) {
			t.Fatalf("store crossed projects: %v", err)
		}
	})

	t.Run("a pooled connection never carries the previous tenant's context", func(t *testing.T) {
		pool := e.DB.RuntimePool(ctx, 1) // one connection: every call reuses it
		for range 3 {
			var n int
			err := storage.WithTenant(ctx, pool, wt.ScopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM workspaces`).Scan(&n)
			})
			if err != nil || n == 0 {
				t.Fatalf("tenant A: %d %v", n, err)
			}
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspaces`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("leaked context: %d rows, %v", n, err)
			}
		}
	})

	t.Run("composite foreign keys reject cross-project and cross-tenant ownership", func(t *testing.T) {
		_, err := e.Migration.Exec(ctx, `INSERT INTO workspaces (tenant_id, project_id, id, work_item_id, repository_id, repository_url, checkout_secret_ref, publisher_secret_ref, work_branch)
			VALUES ($1, $2, 'ws_fk', 'wi_fk', 'repo', 'https://x/y.git', 'a', 'b', 'refs/heads/fk')`, string(wt.ScopeA.TenantID), string(wt.ScopeB.ProjectID))
		if sqlState(err) != "23503" {
			t.Fatalf("a project of another tenant: %v", err)
		}
		_, err = e.Migration.Exec(ctx, `INSERT INTO workspace_publications (tenant_id, project_id, workspace_id, attempt_id, commit_oid, expected_commit, artifact_id, bundle_digest)
			VALUES ($1, $2, $3, 'att_fk', $4, $5, 'art', $6)`,
			string(wt.ScopeA.TenantID), string(wt.ScopeA2.ProjectID), string(wa.ID), strings.Repeat("a", 40), strings.Repeat("b", 40), "sha256:"+strings.Repeat("0", 64))
		if sqlState(err) != "23503" {
			t.Fatalf("a receipt pointing into another project's workspace: %v", err)
		}
		_, err = e.Migration.Exec(ctx, `INSERT INTO workspace_retained_refs (tenant_id, project_id, workspace_id, id, commit_oid, ref_name, purpose, run_id)
			VALUES ($1, $2, $3, 'pin_fk', $4, 'refs/heads/kodama/retained/x', 'reader', 'run_1')`,
			string(wt.ScopeA.TenantID), string(wt.ScopeA2.ProjectID), string(wa.ID), strings.Repeat("a", 40))
		if sqlState(err) != "23503" {
			t.Fatalf("a pin pointing into another project's workspace: %v", err)
		}
	})

	t.Run("the runtime role cannot delete or rewrite identity", func(t *testing.T) {
		for _, stmt := range []string{
			`DELETE FROM workspaces`,
			`DELETE FROM workspace_publications`,
			`DELETE FROM workspace_retained_refs`,
			`UPDATE workspaces SET work_item_id = 'wi_other'`,
			`UPDATE workspaces SET repository_url = 'https://evil.example/x.git'`,
			`UPDATE workspaces SET publisher_secret_ref = checkout_secret_ref`,
			`UPDATE workspace_publications SET commit_oid = expected_commit`,
			`UPDATE workspace_retained_refs SET commit_oid = '` + strings.Repeat("c", 40) + `'`,
			`UPDATE audit SET action = 'x'`,
			`DELETE FROM audit`,
		} {
			err := storage.WithTenant(ctx, e.Runtime, wt.ScopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, stmt)
				return err
			})
			if sqlState(err) != "42501" {
				t.Errorf("%s: %v", stmt, err)
			}
		}
	})

	t.Run("the database enforces publication invariants", func(t *testing.T) {
		d := wb.ID
		bad := map[string]string{
			"one workspace per work item": `INSERT INTO workspaces (tenant_id, project_id, id, work_item_id, repository_id, repository_url, checkout_secret_ref, publisher_secret_ref, work_branch)
				VALUES ('` + string(wt.ScopeB.TenantID) + `', '` + string(wt.ScopeB.ProjectID) + `', 'ws_dup', 'wi_b', 'repo_other', 'https://x/y.git', 'a', 'b', 'refs/heads/other')`,
			"one workspace per branch": `INSERT INTO workspaces (tenant_id, project_id, id, work_item_id, repository_id, repository_url, checkout_secret_ref, publisher_secret_ref, work_branch)
				VALUES ('` + string(wt.ScopeB.TenantID) + `', '` + string(wt.ScopeB.ProjectID) + `', 'ws_dup2', 'wi_new', 'repo_t013', 'https://x/y.git', 'a', 'b', 'refs/heads/kodama/work/wi_b')`,
			"distinct credential references": `UPDATE workspaces SET publisher_secret_ref = checkout_secret_ref WHERE id = '` + string(d) + `'`,
			"pending publication without a writer": `UPDATE workspaces SET publish_attempt_id = 'att_x', publish_commit = '` + strings.Repeat("a", 40) + `', publish_expected_commit = '` + strings.Repeat("b", 40) + `',
				publish_artifact_id = 'art', publish_bundle_digest = 'sha256:` + strings.Repeat("0", 64) + `', publish_started_at = now() WHERE id = '` + string(d) + `'`,
			"half-set writer":      `UPDATE workspaces SET writer_attempt_id = 'att_x' WHERE id = '` + string(d) + `'`,
			"half-set publication": `UPDATE workspaces SET publish_commit = '` + strings.Repeat("a", 40) + `' WHERE id = '` + string(d) + `'`,
			"malformed commit":     `UPDATE workspaces SET accepted_commit = 'main' WHERE id = '` + string(d) + `'`,
			"unknown state":        `UPDATE workspaces SET state = 'ready' WHERE id = '` + string(d) + `'`,
			"version range":        `UPDATE workspaces SET version = 0 WHERE id = '` + string(d) + `'`,
			"a usable workspace has an accepted commit": `UPDATE workspaces SET accepted_commit = NULL WHERE id = '` + string(d) + `'`,
		}
		for name, stmt := range bad {
			if _, err := e.Migration.Exec(ctx, stmt); sqlState(err) != "23514" && sqlState(err) != "23505" {
				t.Errorf("%s: %v", name, err)
			}
		}
		// A writer holding an unsettled publication cannot be released, even by SQL.
		wp := e.Ready(wt.ScopeA, "wi_pending")
		e.Writer(wp, "att_p")
		inp := e.Proposal(wp, "att_p", e.Repo.Initial, "pending", gitfx.File{Path: "p.txt", Content: "p"})
		if _, err := e.PG.Update(ctx, wp.Scope, wp.ID, workspace.Load{Receipt: "att_p"}, func(a *workspace.Aggregate) (workspace.Effects, error) {
			r, _, err := a.Workspace.BeginPublication(workspace.Proposal{AttemptID: "att_p", Commit: inp.Commit, ExpectedTip: inp.ExpectedTip, ArtifactID: inp.BundleArtifactID,
				BundleDigest: wt.Digest([]byte("x"))}, nil, e.Clock.Now())
			return workspace.Effects{Receipt: r}, err
		}); err != nil {
			t.Fatal(err)
		}
		_, err := e.Migration.Exec(ctx, `UPDATE workspaces SET writer_attempt_id = NULL, writer_input_commit = NULL, writer_expected_tip = NULL, writer_reserved_at = NULL WHERE id = $1`, string(wp.ID))
		if sqlState(err) != "23514" {
			t.Errorf("releasing a writer with a pending publication: %v", err)
		}
		_, err = e.Migration.Exec(ctx, `UPDATE workspaces SET writer_attempt_id = 'att_other' WHERE id = $1`, string(wp.ID))
		if sqlState(err) != "23514" {
			t.Errorf("reassigning a writer with a pending publication: %v", err)
		}
	})

	t.Run("audit or event failure rolls the whole change back", func(t *testing.T) {
		before, _ := e.PG.Get(ctx, wa.Scope, wa.ID)
		beforeAudit := e.CountSQL(`SELECT count(*) FROM audit WHERE resource_id = $1`, string(wa.ID))
		cases := map[string]workspace.Effects{
			"audit": {Audit: []workspace.Audit{{Action: "workspace.test", Actor: workspace.Actor{Type: workspace.ActorService, ID: strings.Repeat("x", 300)}}}},
			"event": {Events: []workspace.Event{{Kind: workspace.EventCheckpointCreated, Actor: workspace.Actor{Type: workspace.ActorService, ID: "svc"}, NewCommit: "not-a-commit", HumanSynchronization: true}}},
		}
		for name, fx := range cases {
			_, err := e.PG.Update(ctx, wa.Scope, wa.ID, workspace.Load{}, func(a *workspace.Aggregate) (workspace.Effects, error) {
				a.Workspace.LastError = "rolled_back"
				a.Workspace.Version++
				return fx, nil
			})
			if err == nil {
				t.Fatalf("%s: the failing effect was accepted", name)
			}
		}
		after, _ := e.PG.Get(ctx, wa.Scope, wa.ID)
		if after.Version != before.Version || after.LastError != before.LastError {
			t.Fatalf("a failed audit/event left a state change: %+v", after)
		}
		if got := e.CountSQL(`SELECT count(*) FROM audit WHERE resource_id = $1`, string(wa.ID)); got != beforeAudit {
			t.Fatalf("%d audit rows leaked", got-beforeAudit)
		}
	})
}
