package workspacetest_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/workspace/workspacetest"
	"github.com/optimiware/kodama/test/support/layer"
)

// TestPostgresWorkspaceContract runs the same persistence contract on the
// PostgreSQL adapter. It is recorded in the `test` environment (T-026) and
// skips in the unit layer and without PostgreSQL.
func TestPostgresWorkspaceContract(t *testing.T) {
	layer.SkipService(t, "PostgreSQL")
	workspacetest.StoreContract(t, workspacetest.OpenPostgresStore)
}

// TestHTTPSGitContract runs the same Git contract on the HTTPS adapter against
// the fixture Git server in the `test` environment.
func TestHTTPSGitContract(t *testing.T) {
	layer.SkipService(t, "Git")
	workspacetest.GitContract(t, workspacetest.OpenHTTPSGit)
}
