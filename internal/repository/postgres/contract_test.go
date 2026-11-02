package postgres_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/repository/repositorytest"
)

// Test_postgres_repository_contract runs the shared contract (the store and the
// use cases over it) on real PostgreSQL. It skips in the unit layer and without
// PostgreSQL (test/support/postgres).
func Test_postgres_repository_contract(t *testing.T) {
	repositorytest.Contract(t, repositorytest.OpenPostgres)
}
