package postgres

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/app"
	"github.com/optimiware/kodama/internal/storage"
)

// BenchmarkProvision compares what each test pays for a migrated database: the
// squashed baseline applied to a new database, against a clone of a template
// that was migrated once. Creation and migration are timed; teardown is not.
// The baseline is the whole product schema, so it is also what an assembly test
// pays for.
//
//	go test ./test/support/postgres -run '^$' -bench Provision -benchtime 30x
func BenchmarkProvision(b *testing.B) {
	core, err := app.Manifest()
	if err != nil {
		b.Fatal(err)
	}
	if _, err := storage.NewManifest(storage.BaseContribution()); err != nil {
		b.Fatal(err)
	}
	var entries []string
	for _, m := range core.Migrations() {
		entries = append(entries, m.ID+"|"+m.Checksum())
	}
	migrate := func(ctx context.Context, d *Database) error {
		_, err := storage.Migrate(ctx, d.MigrationPool(ctx, 2), core, storage.MigrateOptions{RuntimeRole: d.RuntimeRole})
		return err
	}
	b.Run("baseline/per_test_migration", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			d := New(b)
			if err := migrate(context.Background(), d); err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
			d.drop()
			b.StartTimer()
		}
	})
	b.Run("baseline/template_clone", func(b *testing.B) {
		digest := ManifestDigest(entries...)
		b.Cleanup(func() { DropTemplate(b, digest) })
		NewFromTemplate(b, digest, migrate).drop() // build the template outside the timed loop
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			d := NewFromTemplate(b, digest, migrate)
			b.StopTimer()
			d.drop()
			b.StartTimer()
		}
	})
}
