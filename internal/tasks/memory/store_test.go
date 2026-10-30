package memory_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/tasks/memory"
	"github.com/optimiware/kodama/internal/tasks/taskstest"
)

func TestTaskStoreContract(t *testing.T) {
	taskstest.Contract(t, func(t *testing.T) taskstest.Fixture {
		s := memory.Store{DB: memdb.New()}
		for _, scope := range []domain.Scope{taskstest.Scope, taskstest.OtherProject, taskstest.OtherTenant} {
			if err := s.SeedProject(t.Context(), scope, taskstest.Workflow()); err != nil {
				t.Fatal(err)
			}
		}
		return taskstest.Fixture{Store: s, Events: s.Events}
	})
}
