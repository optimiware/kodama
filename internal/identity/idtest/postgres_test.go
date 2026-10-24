package idtest_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/identity/idtest"
	"github.com/optimiware/kodama/test/support/layer"
)

// TestPostgresIdentityContract runs the same contract on the PostgreSQL adapter.
// It is recorded in the `test` environment (T-026) and skips in the unit layer
// and without PostgreSQL.
func TestPostgresIdentityContract(t *testing.T) {
	layer.SkipService(t, "PostgreSQL")
	idtest.Contract(t, idtest.OpenPostgres)
}
