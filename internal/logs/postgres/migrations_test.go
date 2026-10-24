package postgres_test

import (
	"context"
	"testing"
	"time"

	artifactspg "github.com/optimiware/kodama/internal/artifacts/postgres"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/logs/logstest"
	logspg "github.com/optimiware/kodama/internal/logs/postgres"
	pipelinepg "github.com/optimiware/kodama/internal/pipeline/postgres"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	workspacepg "github.com/optimiware/kodama/internal/workspace/postgres"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

func fullManifest(t *testing.T, withExecution bool) storage.Manifest {
	t.Helper()
	cs := []storage.Contribution{
		storage.BaseContribution(), eventspg.Migrations(), httppg.Migrations(), identitypg.Migrations(), workpg.Migrations(),
		secretspg.Migrations(), pipelinepg.Migrations(), workspacepg.Migrations(), artifactspg.Migrations(),
	}
	if withExecution {
		cs = append(cs, execpg.Migrations()...)
	}
	cs = append(cs, logspg.Migrations())
	m, err := storage.NewManifest(cs...)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestMigrationContribution: the contribution is one named migration, ordered
// after the artifact and execution tables it references, and re-applying it is
// a no-op.
func TestMigrationContribution(t *testing.T) {
	c := logspg.Migrations()
	if c.Module != "logs" || len(c.Migrations) != 1 || c.Migrations[0].ID != "T023_001_log_records.sql" {
		t.Fatalf("contribution = %+v", c)
	}

	e := logstest.New(t, logstest.Options{NoS3: true})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := storage.Migrate(ctx, e.Migration, fullManifest(t, true), storage.MigrateOptions{RuntimeRole: e.Base.DB.RuntimeRole})
	if err != nil || len(res.Applied) != 0 || res.Skipped == 0 {
		t.Fatalf("re-applying must be a no-op: %+v %v", res, err)
	}

	// Without the execution tables the contribution cannot be applied: its
	// ownership foreign key needs them, so the manifest order is load-bearing.
	db := pgtest.New(t)
	owner := db.MigrationPool(ctx, 2)
	if _, err := storage.Migrate(ctx, owner, fullManifest(t, false), storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err == nil {
		t.Fatal("the log tables were created without the attempts they reference")
	}
}
