package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/storage"
)

var eventTables = []string{
	"event_tenant_subjects", "event_backlog", "outbox", "outbox_attempts", "event_inbox", "event_inbox_attempts",
	"event_scopes", "event_observations", "event_quarantine", "event_replay_requests", "event_recovery_fences", "event_dedup_boundary",
}

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// TestMigrationReapply: base plus event migrations apply once, re-apply as a
// no-op, and enforce the expected RLS, grants, constraints and indexes.
func TestMigrationReapply(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	manifest, err := storage.NewManifest(storage.BaseContribution())
	if err != nil {
		t.Fatal(err)
	}
	res, err := storage.Migrate(ctx, f.env.Migration, manifest, storage.MigrateOptions{RuntimeRole: f.env.DB.RuntimeRole})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 0 || res.Skipped != len(manifest.Migrations()) {
		t.Fatalf("re-apply: applied=%v skipped=%d of %d", res.Applied, res.Skipped, len(manifest.Migrations()))
	}
	var ids []string
	for _, m := range manifest.Migrations() {
		ids = append(ids, m.ID)
	}
	if fmt.Sprint(ids) != "[001_schema.sql 002_grants.sql 003_git_hosting.sql]" {
		t.Fatalf("manifest = %v", ids)
	}

	t.Run("row level security is enabled and forced", func(t *testing.T) {
		for _, table := range eventTables {
			var enabled, forced bool
			if err := f.env.Migration.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = $1::regclass`, table).Scan(&enabled, &forced); err != nil {
				t.Fatal(err)
			}
			if !enabled || !forced {
				t.Errorf("%s: rls enabled=%v forced=%v", table, enabled, forced)
			}
		}
	})

	t.Run("runtime role is unprivileged and grants are minimal", func(t *testing.T) {
		var super, bypass bool
		if err := f.env.Runtime.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil || super || bypass {
			t.Fatalf("runtime role privileged: %v %v %v", super, bypass, err)
		}
		type priv struct {
			table, privilege string
			want             bool
		}
		for _, p := range []priv{
			{"outbox", "SELECT", true}, {"outbox", "INSERT", true}, {"outbox", "UPDATE", false}, {"outbox", "DELETE", true}, {"outbox", "TRUNCATE", false},
			{"event_observations", "UPDATE", false}, {"event_observations", "DELETE", true}, {"event_observations", "TRUNCATE", false},
			{"event_scopes", "DELETE", false}, {"event_backlog", "DELETE", false},
			{"event_tenant_subjects", "UPDATE", false}, {"event_tenant_subjects", "DELETE", false},
			{"event_inbox_attempts", "UPDATE", false}, {"event_inbox_attempts", "DELETE", false},
			{"event_recovery_fences", "DELETE", false}, {"outbox_attempts", "DELETE", false},
		} {
			var got bool
			if err := f.env.Runtime.QueryRow(ctx, `SELECT has_table_privilege(current_user, $1, $2)`, p.table, p.privilege).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != p.want {
				t.Errorf("runtime %s on %s = %v, want %v", p.privilege, p.table, got, p.want)
			}
		}
		// Event content is immutable by column privileges.
		for _, col := range []string{"payload", "payload_digest", "event_id", "event_type", "tenant_id", "project_id", "created_at"} {
			var got bool
			if err := f.env.Runtime.QueryRow(ctx, `SELECT has_column_privilege(current_user, 'outbox', $1, 'UPDATE')`, col).Scan(&got); err != nil || got {
				t.Errorf("runtime may update outbox.%s (err=%v)", col, err)
			}
		}
		for _, col := range []string{"state", "attempts", "claim_token", "lease_expires_at", "next_attempt_at", "published_at"} {
			var got bool
			if err := f.env.Runtime.QueryRow(ctx, `SELECT has_column_privilege(current_user, 'outbox', $1, 'UPDATE')`, col).Scan(&got); err != nil || !got {
				t.Errorf("runtime cannot update outbox.%s (err=%v)", col, err)
			}
		}
		var def bool
		if err := f.env.Runtime.QueryRow(ctx, `SELECT has_function_privilege(current_user, 'kodama_event_resolve_subject(text)', 'EXECUTE')`).Scan(&def); err != nil || !def {
			t.Errorf("runtime cannot resolve subjects: %v", err)
		}
	})

	t.Run("expected indexes exist", func(t *testing.T) {
		for _, idx := range []string{"outbox_due_idx", "outbox_lease_idx", "outbox_published_idx", "outbox_unpublished_age_idx", "event_inbox_retry_idx", "event_inbox_completed_idx",
			"event_inbox_quarantine_idx", "event_observations_recorded_idx", "event_observations_resource_idx", "event_quarantine_open_idx", "event_replay_active_idx"} {
			var n int
			if err := f.env.Migration.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname = $1`, idx).Scan(&n); err != nil || n != 1 {
				t.Errorf("index %s missing (%v)", idx, err)
			}
		}
	})

	t.Run("constraints reject inconsistent rows", func(t *testing.T) {
		insert := func(sql string, args ...any) error {
			return f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, sql, args...)
				return err
			})
		}
		digest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
		// A claimed row needs a lease.
		if err := insert(`INSERT INTO outbox (tenant_id, event_id, event_type, payload, payload_digest, state) VALUES ($1, 'e1', 'run.created', '\x01', $2, 'claimed')`, tenantA, digest); pgCode(err) != "23514" {
			t.Errorf("claimed without lease: %v", err)
		}
		// Payload size is bounded.
		big := make([]byte, 65537)
		if err := insert(`INSERT INTO outbox (tenant_id, event_id, event_type, payload, payload_digest) VALUES ($1, 'e2', 'run.created', $3, $2)`, tenantA, digest, big); pgCode(err) != "23514" {
			t.Errorf("oversized payload: %v", err)
		}
		// Composite ownership: another tenant's project cannot be referenced.
		if err := insert(`INSERT INTO outbox (tenant_id, event_id, project_id, event_type, payload, payload_digest) VALUES ($1, 'e3', $3, 'run.created', '\x01', $2)`, tenantA, digest, projectB); pgCode(err) != "23503" {
			t.Errorf("cross-owner project reference: %v", err)
		}
		// Scope key must match the project.
		if err := insert(`INSERT INTO event_scopes (tenant_id, scope_key, project_id) VALUES ($1, 'wrong', $2)`, tenantA, projectA); pgCode(err) != "23514" {
			t.Errorf("scope key mismatch: %v", err)
		}
		// A completed inbox entry needs its completion time.
		if err := insert(`INSERT INTO event_inbox (tenant_id, consumer, event_id, event_type, payload_digest, state) VALUES ($1, 'audit', 'e4', 'run.created', $2, 'completed')`, tenantA, digest); pgCode(err) != "23514" {
			t.Errorf("completed without completed_at: %v", err)
		}
		// Unknown quarantine reasons are rejected.
		if err := insert(`INSERT INTO event_quarantine (tenant_id, id, consumer, delivery_key, reason_code, payload) VALUES ($1, 'q1', 'audit', 'k', 'because', '\x01')`, tenantA); pgCode(err) != "23514" {
			t.Errorf("unknown reason: %v", err)
		}
	})

	t.Run("pending and unresolved rows cannot be deleted or rewritten", func(t *testing.T) {
		digest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
		if err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO outbox (tenant_id, event_id, event_type, payload, payload_digest) VALUES ($1, 'pending1', 'run.created', '\x01', $2)`, tenantA, digest)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var deleted int64
		if err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `DELETE FROM outbox WHERE event_id = 'pending1'`)
			deleted = tag.RowsAffected()
			return err
		}); err != nil || deleted != 0 {
			t.Fatalf("a pending outbox row was deleted: %d %v", deleted, err)
		}
		err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE outbox SET payload = '\x02' WHERE event_id = 'pending1'`)
			return err
		})
		if pgCode(err) != "42501" {
			t.Fatalf("event content was rewritten: %v", err)
		}
	})
}
