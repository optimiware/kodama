package postgres_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/gitpush/gitpushtest"
)

// Test_postgres_gitpush_contract runs the shared contract (the store, the use
// cases over it and the gitauth seed transaction seam) on real PostgreSQL. It
// skips in the unit layer and without PostgreSQL (test/support/postgres).
func Test_postgres_gitpush_contract(t *testing.T) {
	gitpushtest.Contract(t, gitpushtest.OpenPostgres)
}
