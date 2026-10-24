package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest"
)

// TestTenantIsolation (IT-02, IT-04, IT-05, IT-11, DQ-01, SD-04, SD-A03):
// gates are readable and writable only inside their own tenant and project:
// another tenant or project finds nothing and learns nothing, evidence
// references and runs cannot cross ownership, forced row level security
// confines the runtime role, pooled connections never keep a tenant context,
// and the immutable columns resist every role.
func TestTenantIsolation(t *testing.T) {
	e := gatestest.New(t)
	e.GrantAll()
	ctx := context.Background()
	scope := gatestest.ScopeA
	_, run, g := pendingGate(t, e, scope)

	t.Run("reads of another tenant or project find nothing", func(t *testing.T) {
		for _, other := range []domain.Scope{gatestest.ScopeB, gatestest.ScopeA2, {TenantID: scope.TenantID, ProjectID: "p_unknown"}, {TenantID: "t_unknown", ProjectID: scope.ProjectID}} {
			if _, err := e.Store.Get(ctx, other, g.ID); !errors.Is(err, gates.ErrNotFound) {
				t.Errorf("get in %s: %v", other, err)
			}
			if _, err := e.Store.ByStep(ctx, other, run.ID, "review"); !errors.Is(err, gates.ErrNotFound) {
				t.Errorf("by step in %s: %v", other, err)
			}
			if got, err := e.Store.List(ctx, other, gates.ListQuery{Limit: 10}); err == nil && len(got) != 0 {
				t.Errorf("list in %s: %d gates", other, len(got))
			}
		}
		// The service hides it the same way, for a reviewer of the other tenant.
		if _, err := e.Gates.Get(ctx, gatestest.Caller("u034_reviewer_b"), scope, g.ID); !errors.Is(err, gates.ErrHidden) {
			t.Errorf("a reviewer of another tenant: %v", err)
		}
		if _, err := e.Gates.Get(ctx, gatestest.Caller(gatestest.Reviewer), gatestest.ScopeA2, g.ID); !errors.Is(err, gates.ErrNotFound) {
			t.Errorf("the right principal in the wrong project: %v", err)
		}
	})

	t.Run("a decision cannot cross ownership", func(t *testing.T) {
		if _, err := e.Gates.Decide(ctx, gatestest.Caller("u034_reviewer_b"), scope, g.ID, approveCmd(g)); !errors.Is(err, gates.ErrHidden) {
			t.Errorf("a decision by a reviewer of another tenant: %v", err)
		}
		if _, err := e.Gates.Decide(ctx, gatestest.Caller(gatestest.Reviewer), gatestest.ScopeA2, g.ID, approveCmd(g)); !errors.Is(err, gates.ErrNotFound) {
			t.Errorf("a decision through another project: %v", err)
		}
		if _, _, err := e.Store.Invalidate(ctx, gatestest.ScopeB, g.ID, &gates.StaleError{Definitive: true, Reason: gates.ReasonContentChanged}, ""); !errors.Is(err, gates.ErrNotFound) {
			t.Errorf("invalidation through another tenant: %v", err)
		}
		if cur, _ := e.Store.Get(ctx, scope, g.ID); cur.Status != gates.StatusPending || cur.Invalidation != nil || cur.Version != g.Version {
			t.Fatalf("a foreign request changed the gate: %+v", cur)
		}
	})

	t.Run("row level security confines the runtime role", func(t *testing.T) {
		var visible int
		err := e.X.RuntimeTx(gatestest.ScopeB.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM gates`).Scan(&visible)
		})
		if err != nil || visible != 0 {
			t.Fatalf("tenant B sees %d gates: %v", visible, err)
		}
		err = e.X.RuntimeTx(scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM gates`).Scan(&visible)
		})
		if err != nil || visible == 0 {
			t.Fatalf("the owning tenant sees %d gates: %v", visible, err)
		}
		// A write for another tenant is refused by the policy, not by the caller.
		err = e.X.RuntimeTx(gatestest.ScopeB.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO gate_evidence (tenant_id, project_id, gate_id, artifact_id, role, digest) VALUES ($1, $2, $3, 'art_x', 'evidence', $4)`,
				string(scope.TenantID), string(scope.ProjectID), string(g.ID), "sha256:"+string(make([]byte, 0))+"0000000000000000000000000000000000000000000000000000000000000000")
			return err
		})
		if err == nil {
			t.Fatalf("tenant B wrote evidence for tenant A")
		}
		err = e.X.RuntimeTx(gatestest.ScopeB.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE gates SET status = 'approved' WHERE id = $1`, string(g.ID))
			if err == nil && tag.RowsAffected() != 0 {
				t.Errorf("tenant B updated %d gates of tenant A", tag.RowsAffected())
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("pooled connections keep no tenant context", func(t *testing.T) {
		if err := e.X.RuntimeTx(scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var n int
			return tx.QueryRow(ctx, `SELECT count(*) FROM gates`).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 30; i++ {
			var n int
			if err := e.X.Runtime.QueryRow(ctx, `SELECT count(*) FROM gates`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("a pooled connection kept a tenant context: %d gates visible", n)
			}
		}
	})

	t.Run("composite keys keep evidence, steps and work items in their project", func(t *testing.T) {
		// An artifact of another project cannot be bound to this gate.
		pB, _, _ := pendingGate(t, e, gatestest.ScopeA2)
		var foreign string
		e.X.Scalar(&foreign, `SELECT id FROM artifacts WHERE project_id = $1 LIMIT 1`, string(gatestest.ScopeA2.ProjectID))
		_ = pB
		err := e.X.OwnerExecErr(`INSERT INTO gate_evidence (tenant_id, project_id, gate_id, artifact_id, role, digest) VALUES ($1, $2, $3, $4, 'evidence', $5)`,
			string(scope.TenantID), string(scope.ProjectID), string(g.ID), foreign, "sha256:0000000000000000000000000000000000000000000000000000000000000000")
		if err == nil {
			t.Errorf("an artifact of another project was bound as evidence")
		}
		// A gate cannot pause a step of another project's run.
		err = e.X.OwnerExecErr(`INSERT INTO gates (tenant_id, project_id, id, run_id, step_id, work_item_id, content_revision, candidate_commit, definition_id, subject, subject_digest, required, requested_at, deadline, created_at, updated_at)
			SELECT tenant_id, $1, 'gate_cross', run_id, step_id, work_item_id, content_revision, candidate_commit, definition_id, subject, subject_digest, required, requested_at, deadline, created_at, updated_at FROM gates WHERE id = $2`,
			string(gatestest.ScopeA2.ProjectID), string(g.ID))
		if err == nil {
			t.Errorf("a gate in another project paused this project's step")
		}
		// The searchable columns must equal the canonical subject.
		err = e.X.OwnerExecErr(`INSERT INTO gates (tenant_id, project_id, id, run_id, step_id, work_item_id, content_revision, candidate_commit, definition_id, subject, subject_digest, required, requested_at, deadline, created_at, updated_at)
			SELECT tenant_id, project_id, 'gate_forged', run_id, 'deploy', work_item_id, content_revision + 1, candidate_commit, definition_id, subject, subject_digest, required, requested_at, deadline, created_at, updated_at FROM gates WHERE id = $1`, string(g.ID))
		if err == nil {
			t.Errorf("columns disagreeing with the canonical subject were accepted")
		}
		// One gate per run step.
		err = e.X.OwnerExecErr(`INSERT INTO gates (tenant_id, project_id, id, run_id, step_id, work_item_id, content_revision, candidate_commit, definition_id, subject, subject_digest, required, requested_at, deadline, created_at, updated_at)
			SELECT tenant_id, project_id, 'gate_twice', run_id, step_id, work_item_id, content_revision, candidate_commit, definition_id, subject, subject_digest, required, requested_at, deadline, created_at, updated_at FROM gates WHERE id = $1`, string(g.ID))
		if err == nil {
			t.Errorf("a second gate was recorded for one step")
		}
	})

	t.Run("evidence of another tenant is not found", func(t *testing.T) {
		var artifact string
		e.X.Scalar(&artifact, `SELECT id FROM artifacts WHERE project_id = $1 LIMIT 1`, string(scope.ProjectID))
		if _, err := e.Evidence.Describe(ctx, gatestest.ScopeB, domain.ArtifactID(artifact)); !errors.Is(err, gates.ErrNotFound) {
			t.Fatalf("describe through another tenant: %v", err)
		}
		if _, err := e.Evidence.Describe(ctx, gatestest.ScopeA2, domain.ArtifactID(artifact)); !errors.Is(err, gates.ErrNotFound) {
			t.Fatalf("describe through another project: %v", err)
		}
	})

	t.Run("immutable columns resist every role", func(t *testing.T) {
		for name, q := range map[string]string{
			"subject":           `UPDATE gates SET subject = subject || ' ' WHERE id = $1`,
			"subject digest":    `UPDATE gates SET subject_digest = 'sha256:0000000000000000000000000000000000000000000000000000000000000000' WHERE id = $1`,
			"required snapshot": `UPDATE gates SET required = '{"evidence":[],"conformance":null}' WHERE id = $1`,
			"deadline":          `UPDATE gates SET deadline = deadline + interval '1 day' WHERE id = $1`,
			"run":               `UPDATE gates SET step_id = 'deploy' WHERE id = $1`,
			"delete":            `DELETE FROM gates WHERE id = $1`,
			"evidence digest":   `UPDATE gate_evidence SET digest = 'sha256:0000000000000000000000000000000000000000000000000000000000000000' WHERE gate_id = $1`,
			"evidence delete":   `DELETE FROM gate_evidence WHERE gate_id = $1`,
		} {
			// Either a guard refuses the statement or row level security leaves no
			// row for it (the evidence table has no UPDATE or DELETE policy).
			if n, err := e.X.OwnerExecRows(q, string(g.ID)); err == nil && n != 0 {
				t.Errorf("%s: the owner rewrote %d immutable records", name, n)
			}
		}
		// The runtime role has no UPDATE on the immutable columns at all.
		err := e.X.RuntimeTx(scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE gates SET subject = subject WHERE id = $1`, string(g.ID))
			return err
		})
		if err == nil {
			t.Errorf("the runtime role may update the subject")
		}
		// A decision, once recorded, is never rewritten and the status never reverts.
		if _, err := e.Gates.Decide(ctx, gatestest.Caller(gatestest.Reviewer), scope, g.ID, approveCmd(g)); err != nil {
			t.Fatal(err)
		}
		for name, q := range map[string]string{
			"revert status":  `UPDATE gates SET status = 'pending', decision = NULL, decided_by_type = NULL, decided_by_id = NULL, decision_reason = NULL, decided_at = NULL WHERE id = $1`,
			"flip to reject": `UPDATE gates SET status = 'rejected', decision = 'reject' WHERE id = $1`,
			"rewrite reason": `UPDATE gates SET decision_reason = 'something else' WHERE id = $1`,
			"rewrite actor":  `UPDATE gates SET decided_by_id = 'someone_else' WHERE id = $1`,
		} {
			if err := e.X.OwnerExecErr(q, string(g.ID)); err == nil {
				t.Errorf("%s: the decision was rewritten", name)
			}
		}
		if _, _, err := e.Store.Invalidate(ctx, scope, g.ID, &gates.StaleError{Definitive: true, Reason: gates.ReasonCandidateChanged}, ""); err != nil {
			t.Fatal(err)
		}
		if err := e.X.OwnerExecErr(`UPDATE gates SET invalidation_reason = 'content_changed' WHERE id = $1`, string(g.ID)); err == nil {
			t.Errorf("an invalidation was rewritten")
		}
	})
}
