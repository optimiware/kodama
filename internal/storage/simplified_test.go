package storage_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/test/support/postgres"
)

func TestRunSnapshotsAndOutbox(t *testing.T) {
	e := newEnv(t)
	e.exec(t, `INSERT INTO pipelines (tenant_id,project_id,id,name,source,definition) VALUES
 ('tenant-a','pa1','pipeline','test','api','{"steps":[{"name":"old"}]}');
INSERT INTO runs (tenant_id,project_id,id,pipeline_id,definition_snapshot,trigger_key,request_principal,request_key,request_hash)
 SELECT tenant_id,project_id,'run',id,definition,'event-1','user-1','key-1',repeat('a',64) FROM pipelines;
UPDATE pipelines SET definition='{"steps":[{"name":"new"}]}';`)
	pool := e.runtime(t, 1)
	err := storage.WithTenant(testCtx(t), pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		var name string
		if err := tx.QueryRow(ctx, `SELECT definition_snapshot->'steps'->0->>'name' FROM runs WHERE id='run'`).Scan(&name); err != nil {
			return err
		}
		if name != "old" {
			t.Fatalf("run changed with pipeline: %s", name)
		}
		_, err := tx.Exec(ctx, `UPDATE audit_events SET published_at=now(),publish_attempts=publish_attempts+1 WHERE id='au-a'`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`UPDATE runs SET definition_snapshot='{}' WHERE id='run'`,
		`UPDATE runs SET inputs='{"changed":true}' WHERE id='run'`,
		`UPDATE audit_events SET payload='{"tampered":true}' WHERE id='au-a'`,
	} {
		err := storage.WithTenant(testCtx(t), pool, tenantA, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, sql); return err })
		requireCode(t, err, "42501", sql)
	}
	// Broker redelivery and changed-body idempotency keys cannot create new runs.
	for _, sql := range []string{
		`INSERT INTO runs (tenant_id,project_id,id,pipeline_id,definition_snapshot,trigger_key) VALUES ('tenant-a','pa1','duplicate','pipeline','{}','event-1')`,
		`INSERT INTO runs (tenant_id,project_id,id,pipeline_id,definition_snapshot,request_principal,request_key,request_hash) VALUES ('tenant-a','pa1','duplicate','pipeline','{}','user-1','key-1',repeat('b',64))`,
	} {
		_, err := e.mig.Exec(testCtx(t), sql)
		requireCode(t, err, "23505", sql)
	}
}

func TestResetRefusesOldBaselineWithoutDataLoss(t *testing.T) {
	db := postgres.New(t)
	pool := db.MigrationPool(testCtx(t), 1)
	// The baseline at 74096e2623168db4502c3f2eff64721377be578e.
	_, err := pool.Exec(testCtx(t), `CREATE TABLE schema_migrations(seq bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,id text NOT NULL UNIQUE,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now());
INSERT INTO schema_migrations(id,checksum) VALUES ('001_schema.sql','c93805b483bb5b70cd6d5d7175b9675dd8a34eecad464ecc96bfd9e3ef8539b2');
CREATE TABLE work_items(id text); INSERT INTO work_items VALUES ('preserve-me');`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = storage.Migrate(testCtx(t), pool, baseManifest(t), storage.MigrateOptions{RuntimeRole: db.RuntimeRole})
	if !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Fatalf("old baseline: %v", err)
	}
	var id string
	if err := pool.QueryRow(testCtx(t), `SELECT id FROM work_items`).Scan(&id); err != nil || id != "preserve-me" {
		t.Fatalf("old data changed: %s %v", id, err)
	}
	if tableExists(t, pool, "tasks") {
		t.Fatal("reset partially applied over old schema")
	}
}
