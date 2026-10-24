package worktest_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/work/worktest"
	"github.com/optimiware/kodama/test/support/layer"
)

// TestPostgresWorkContract runs the same contract on the PostgreSQL adapter. It
// is recorded in the `test` environment (T-026) and skips in the unit layer and
// without PostgreSQL.
func TestPostgresWorkContract(t *testing.T) {
	layer.SkipService(t, "PostgreSQL")
	worktest.Contract(t, worktest.OpenPostgres)
}
