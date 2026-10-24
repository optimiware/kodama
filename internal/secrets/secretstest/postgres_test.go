package secretstest_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/secrets/secretstest"
	"github.com/optimiware/kodama/test/support/layer"
)

// TestPostgresSecretsContract runs the same contract on the PostgreSQL adapter
// with the development keyfile provider. It is recorded in the `test`
// environment (T-026) and skips in the unit layer and without PostgreSQL.
func TestPostgresSecretsContract(t *testing.T) {
	layer.SkipService(t, "PostgreSQL")
	secretstest.StoreContract(t, secretstest.OpenPostgres)
}
