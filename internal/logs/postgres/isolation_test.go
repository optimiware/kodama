package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/contract"
	"github.com/optimiware/kodama/internal/logs/logstest"
	"github.com/optimiware/kodama/internal/storage"
)

// TestTenantPersistence: RLS, pooled context reuse, composite ownership,
// immutable evidence, nondisclosing lookups and rollback of state and audit,
// on a real PostgreSQL under the non-owner runtime role.
func TestTenantPersistence(t *testing.T) {
	e := logstest.New(t)
	a := e.NewAttempt(logstest.ScopeA, true)
	b := e.NewAttempt(logstest.ScopeB, true)
	recA := newRecord(t, e, a, logs.SourceWrapper)
	refA := logRef(t, e, a, contract.CommandLogName, "alpha\n")
	if _, err := observe(e, a.Scope, recA.ID, wrapperLink(refA)); err != nil {
		t.Fatal(err)
	}

	t.Run("lookups never cross tenant, project or attempt", func(t *testing.T) {
		if got, err := e.Logs.Get(ctx(), a.Scope, recA.ID); err != nil || got.State != logs.StateRetained || got.Link.Log != refA {
			t.Fatalf("own record: %+v %v", got, err)
		}
		for name, sc := range map[string]domain.Scope{
			"other tenant":  b.Scope,
			"other project": logstest.ScopeA2,
			"mixed":         {TenantID: a.Scope.TenantID, ProjectID: b.Scope.ProjectID},
		} {
			if _, err := e.Logs.Get(ctx(), sc, recA.ID); !errors.Is(err, logs.ErrNotFound) {
				t.Errorf("%s: Get = %v", name, err)
			}
			if rs, err := e.Logs.ForAttempt(ctx(), sc, a.ID); err != nil || len(rs) != 0 {
				t.Errorf("%s: ForAttempt = %v %v", name, rs, err)
			}
			if _, err := e.Logs.Update(ctx(), sc, recA.ID, func(r *logs.Record) (bool, logs.Audit, error) { return false, logs.Audit{}, nil }); !errors.Is(err, logs.ErrNotFound) {
				t.Errorf("%s: Update = %v", name, err)
			}
		}
		if rs, err := e.Logs.ForAttempt(ctx(), a.Scope, a.ID); err != nil || len(rs) != 1 {
			t.Fatalf("ForAttempt = %v %v", rs, err)
		}
		// Ensure for an attempt of another scope cannot create a record: the
		// composite foreign key to the attempt refuses it.
		info := e.Info(a)
		tmpl, _ := logs.NewRecord(b.Scope, "logrec_foreign", a.ID, a.Run, info.Step, logs.SourceWrapper, 1, t0)
		if _, err := e.Logs.Ensure(ctx(), *tmpl); !errors.Is(err, logs.ErrNotFound) {
			t.Fatalf("a record binding another scope's attempt must be refused: %v", err)
		}
	})

	t.Run("row level security confines the table", func(t *testing.T) {
		var n int
		err := storage.WithTenant(ctx(), e.Runtime, b.Scope.TenantID, func(c context.Context, tx pgx.Tx) error {
			return tx.QueryRow(c, `SELECT count(*) FROM log_records WHERE tenant_id = $1`, string(a.Scope.TenantID)).Scan(&n)
		})
		if err != nil || n != 0 {
			t.Fatalf("tenant B sees %d of tenant A's rows (%v)", n, err)
		}
		if err := e.Runtime.QueryRow(ctx(), `SELECT count(*) FROM log_records`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("without a tenant context %d rows are visible (%v)", n, err)
		}
		_, err = e.Runtime.Exec(ctx(), `INSERT INTO log_records (tenant_id, project_id, id, run_id, attempt_id, step_id, source, generation, state, version, created_at, updated_at)
VALUES ($1, $2, 'logrec_x', $3, $4, 'build', 'wrapper', 1, 'pending', 1, now(), now())`, string(a.Scope.TenantID), string(a.Scope.ProjectID), string(a.Run), string(a.ID))
		if err == nil {
			t.Fatal("an insert without tenant context succeeded")
		}
		err = storage.WithTenant(ctx(), e.Runtime, b.Scope.TenantID, func(c context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(c, `INSERT INTO log_records (tenant_id, project_id, id, run_id, attempt_id, step_id, source, generation, state, version, created_at, updated_at)
VALUES ($1, $2, 'logrec_y', $3, $4, 'build', 'wrapper', 1, 'pending', 1, now(), now())`, string(a.Scope.TenantID), string(a.Scope.ProjectID), string(a.Run), string(a.ID))
			return err
		})
		if err == nil {
			t.Fatal("tenant B wrote a row owned by tenant A")
		}
	})

	t.Run("pooled connections do not carry tenant context", func(t *testing.T) {
		for i := 0; i < 200; i++ {
			if i%2 == 0 {
				if _, err := e.Logs.Get(ctx(), a.Scope, recA.ID); err != nil {
					t.Fatal(err)
				}
				continue
			}
			var n int
			var setting string
			if err := e.Runtime.QueryRow(ctx(), `SELECT count(*), coalesce(current_setting('kodama.tenant_id', true), '') FROM log_records`).Scan(&n, &setting); err != nil || n != 0 || setting != "" {
				t.Fatalf("a pooled connection kept tenant context: %d rows %q %v", n, setting, err)
			}
		}
	})

	t.Run("the runtime role cannot delete or rewrite identity", func(t *testing.T) {
		if _, err := e.Runtime.Exec(ctx(), `DELETE FROM log_records`); err == nil {
			t.Fatal("DELETE must not be granted")
		}
		err := storage.WithTenant(ctx(), e.Runtime, a.Scope.TenantID, func(c context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(c, `UPDATE log_records SET attempt_id = 'att_other' WHERE id = $1`, string(recA.ID))
			return err
		})
		if err == nil {
			t.Fatal("identity columns must not be updatable by the runtime role")
		}
	})

	t.Run("evidence is immutable even for the owner", func(t *testing.T) {
		for name, stmt := range map[string]string{
			"completeness rewrite": `UPDATE log_records SET completeness = 'partial', reasons = '{truncated}', version = version + 1 WHERE id = $1`,
			"relink":               `UPDATE log_records SET log_size = log_size + 1, version = version + 1 WHERE id = $1`,
			"back to pending":      `UPDATE log_records SET state = 'pending', version = version + 1 WHERE id = $1`,
			"delete":               `DELETE FROM log_records WHERE id = $1`,
			"version skip":         `UPDATE log_records SET removed_at = now(), version = version + 5 WHERE id = $1`,
		} {
			if _, err := e.Migration.Exec(ctx(), stmt, string(recA.ID)); err == nil {
				t.Errorf("%s was accepted", name)
			}
		}
	})

	t.Run("a link must name usable diagnostic files of the same attempt", func(t *testing.T) {
		other := e.NewAttempt(logstest.ScopeA, true)
		foreign := logRef(t, e, other, contract.CommandLogName, "another attempt\n")
		target := e.NewAttempt(logstest.ScopeA, true)
		rec := newRecord(t, e, target, logs.SourceWrapper)

		if _, err := observe(e, target.Scope, rec.ID, wrapperLink(foreign)); err == nil {
			t.Fatal("a log artifact produced by another attempt was linked")
		}
		missing := logs.ArtifactRef{ID: "art_missing", Digest: foreign.Digest, Size: foreign.Size}
		if _, err := observe(e, target.Scope, rec.ID, wrapperLink(missing)); err == nil {
			t.Fatal("a missing artifact was linked")
		}
		own := logRef(t, e, target, contract.CommandLogName, "mine\n")
		bad := own
		bad.Digest = foreign.Digest
		if _, err := observe(e, target.Scope, rec.ID, wrapperLink(bad)); err == nil {
			t.Fatal("a digest that differs from the artifact's was linked")
		}
		bad = own
		bad.Size++
		if _, err := observe(e, target.Scope, rec.ID, wrapperLink(bad)); err == nil {
			t.Fatal("a size that differs from the artifact's was linked")
		}
		got, err := e.Logs.Get(ctx(), target.Scope, rec.ID)
		if err != nil || got.State != logs.StatePending || got.Version != 1 {
			t.Fatalf("failed linkage must leave the record untouched: %+v %v", got, err)
		}
		if _, err := observe(e, target.Scope, rec.ID, wrapperLink(own)); err != nil {
			t.Fatalf("valid linkage: %v", err)
		}
	})

	t.Run("a mutation error rolls back state and audit together", func(t *testing.T) {
		a4 := e.NewAttempt(logstest.ScopeA, true)
		rec := newRecord(t, e, a4, logs.SourceController)
		before := e.Count(`SELECT count(*) FROM audit WHERE resource_type = 'log_record' AND resource_id = $1`, string(rec.ID))
		boom := errors.New("boom")
		_, err := e.Logs.Update(ctx(), a4.Scope, rec.ID, func(r *logs.Record) (bool, logs.Audit, error) {
			if _, err := r.Begin("tok", time.Minute, t0); err != nil {
				return false, logs.Audit{}, err
			}
			return true, logs.Audit{Action: "log.collecting"}, boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		got, _ := e.Logs.Get(ctx(), a4.Scope, rec.ID)
		if got.State != logs.StatePending || e.Count(`SELECT count(*) FROM audit WHERE resource_type = 'log_record' AND resource_id = $1`, string(rec.ID)) != before {
			t.Fatalf("a failed mutation changed state or audit: %+v", got)
		}
		// A successful one writes exactly one safe audit row.
		if _, err := e.Logs.Update(ctx(), a4.Scope, rec.ID, func(r *logs.Record) (bool, logs.Audit, error) {
			_, err := r.Begin("tok", time.Minute, t0)
			return err == nil, logs.Audit{Action: "log.collecting", RequestID: "req_t023"}, err
		}); err != nil {
			t.Fatal(err)
		}
		var meta string
		var n int
		if err := e.Migration.QueryRow(ctx(), `SELECT count(*), max(metadata::text) FROM audit WHERE resource_type = 'log_record' AND resource_id = $1 AND action = 'log.collecting'`, string(rec.ID)).Scan(&n, &meta); err != nil || n != 1 {
			t.Fatalf("audit rows = %d (%v)", n, err)
		}
		for _, secret := range []string{"sha256:", "tok", "http", "artifacts/"} {
			if strings.Contains(meta, secret) {
				t.Fatalf("audit metadata carries %q: %s", secret, meta)
			}
		}
	})
}
