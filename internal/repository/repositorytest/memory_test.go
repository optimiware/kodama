package repositorytest_test

import (
	"errors"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/repository"
	"github.com/optimiware/kodama/internal/repository/repositorytest"
)

// The acceptance scenarios of T-127 (GR-A01, GR-A02, persistence side) on the
// memory adapter, which needs no service; the PostgreSQL adapter runs the same
// suite in repository/postgres.

func Test_unit_repository_create_idempotent(t *testing.T) {
	repositorytest.CreateIdempotent(t, repositorytest.OpenMem)
}

func Test_unit_repository_name_unique_per_project(t *testing.T) {
	repositorytest.NameUniquePerProject(t, repositorytest.OpenMem)
}

func Test_unit_repository_policy_defaults_deny(t *testing.T) {
	repositorytest.PolicyDefaultsDeny(t, repositorytest.OpenMem)
}

func Test_unit_repository_provisioning_reconciles(t *testing.T) {
	repositorytest.ProvisioningReconciles(t, repositorytest.OpenMem)
}

func Test_unit_repository_archive_restore(t *testing.T) {
	repositorytest.ArchiveRestore(t, repositorytest.OpenMem)
}

func Test_unit_repository_cross_tenant_hidden(t *testing.T) {
	repositorytest.CrossTenantHidden(t, repositorytest.OpenMem)
}

func Test_unit_repository_external_mapping(t *testing.T) {
	repositorytest.ExternalMapping(t, repositorytest.OpenMem)
}

// The mapping must not change or even link the workspace module: the module,
// its adapters and its test support never import it.
func Test_unit_repository_external_mapping_leaves_workspace_alone(t *testing.T) {
	for _, pattern := range []string{"*.go", "../*.go", "../postgres/*.go", "../http/*.go"} {
		files, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range f.Imports {
				if strings.Contains(imp.Path.Value, "kodama/internal/workspace") {
					t.Errorf("%s imports %s", name, imp.Path.Value)
				}
			}
		}
	}
}

func Test_unit_repository_memory_contract(t *testing.T) {
	repositorytest.Contract(t, repositorytest.OpenMem)
}

func Test_unit_repository_memory_is_development_only(t *testing.T) {
	e := repositorytest.NewMemEnv(t)
	if d, ok := any(e.Mem).(interface{ DevelopmentOnly() bool }); !ok || !d.DevelopmentOnly() {
		t.Fatal("the memory store is not flagged development-only")
	}
	if _, err := repositorytest.NewMemStore(nil, e.Events, repositorytest.MemOptions{}); !errors.Is(err, repository.ErrNotConfigured) {
		t.Fatalf("a store without a kernel: %v", err)
	}
	if _, err := repositorytest.NewMemStore(e.DB, nil, repositorytest.MemOptions{}); !errors.Is(err, repository.ErrNotConfigured) {
		t.Fatalf("a store without an event appender: %v", err)
	}
}
