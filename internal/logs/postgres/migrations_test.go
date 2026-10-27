package postgres_test

import (
	"context"
	"testing"
	"time"

	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/logs/logstest"
	"github.com/optimiware/kodama/internal/storage"
)

// TestMigrationReapply: the squashed baseline, which owns the log tables and
// the execution tables they reference, re-applies as a no-op.
func TestMigrationReapply(t *testing.T) {
	e := logstest.New(t, logstest.Options{NoS3: true})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	manifest, err := storage.NewManifest(storage.BaseContribution(), execpg.RunResourceMigrations())
	if err != nil {
		t.Fatal(err)
	}
	res, err := storage.Migrate(ctx, e.Migration, manifest, storage.MigrateOptions{RuntimeRole: e.Base.DB.RuntimeRole})
	if err != nil || len(res.Applied) != 0 || res.Skipped != len(manifest.Migrations()) {
		t.Fatalf("re-applying must be a no-op: %+v %v", res, err)
	}
}
