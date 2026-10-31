package pipelines_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/shell/pipelines"
)

func memoryFixture(t *testing.T) fixture {
	t.Helper()
	m := pipelines.NewMemory(nil)
	return fixture{
		svc: pipelines.New(m),
		project: func(t *testing.T, tenant domain.TenantID) domain.Scope {
			scope := domain.Scope{TenantID: tenant, ProjectID: domain.ProjectID(fresh("p"))}
			m.AddProject(scope)
			return scope
		},
		repo: func(t *testing.T, scope domain.Scope) string {
			id := fresh("repo")
			m.AddRepository(scope, id)
			return id
		},
	}
}

// Test_unit_pipelines_project_unique holds the memory adapter to the project
// unique-name contract the PostgreSQL adapter also passes.
func Test_unit_pipelines_project_unique(t *testing.T) {
	runProjectUnique(t, memoryFixture(t))
}

// Test_unit_pipelines_source_conflict holds the memory adapter to the source
// ownership, idempotency and validation contract.
func Test_unit_pipelines_source_conflict(t *testing.T) {
	runSourceConflict(t, memoryFixture(t))
}
