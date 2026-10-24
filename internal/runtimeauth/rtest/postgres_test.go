package rtest_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
	"github.com/optimiware/kodama/test/support/layer"
)

// TestPostgresRuntimeauthContract runs the same contract on the PostgreSQL
// adapter. It is recorded in the `test` environment (T-026) and skips in the
// unit layer and without PostgreSQL.
func TestPostgresRuntimeauthContract(t *testing.T) {
	layer.SkipService(t, "PostgreSQL")
	rtest.Contract(t, rtest.OpenPostgres)
}
