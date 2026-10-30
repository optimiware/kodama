package postgres

import (
	"testing"

	"github.com/optimiware/kodama/internal/storage"
)

func TestStateMigrationContributionIsOrdered(t *testing.T) {
	c := Migrations()
	m, err := storage.NewManifest(storage.BaseContribution(), c)
	if err != nil {
		t.Fatal(err)
	}
	migrations := m.Migrations()
	if len(migrations) != 6 {
		t.Fatalf("migrations: %+v", migrations)
	}
	if migrations[2].ID != "300_state.sql" || migrations[3].ID != "301_state_grants.sql" || migrations[4].ID != "302_state_quota.sql" || migrations[5].ID != "303_state_audit.sql" {
		t.Fatalf("unexpected state migrations: %+v", migrations[2:])
	}
}
