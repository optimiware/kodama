package postgres_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/tasks"
	taskpg "github.com/optimiware/kodama/internal/tasks/postgres"
	"github.com/optimiware/kodama/internal/tasks/taskstest"
	"github.com/optimiware/kodama/test/support/postgres"
)

func TestTaskStoreContract(t *testing.T) {
	taskstest.Contract(t, func(t *testing.T) taskstest.Fixture {
		db := postgres.New(t)
		owner := db.MigrationPool(t.Context(), 2)
		manifest, err := storage.NewManifest(storage.BaseContribution())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.Migrate(t.Context(), owner, manifest, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(t.Context(), `INSERT INTO tenants (id,key) VALUES ('tenant-a','a'),('tenant-b','b')`); err != nil {
			t.Fatal(err)
		}
		workflow, err := json.Marshal(taskstest.Workflow())
		if err != nil {
			t.Fatal(err)
		}
		for _, scope := range []domain.Scope{taskstest.Scope, taskstest.OtherProject, taskstest.OtherTenant} {
			if _, err := owner.Exec(t.Context(), `INSERT INTO projects (tenant_id,id,key,task_workflow) VALUES ($1,$2::text,$2::text,$3::jsonb)`, scope.TenantID, scope.ProjectID, workflow); err != nil {
				t.Fatal(err)
			}
		}
		runtime := db.RuntimePool(t.Context(), 4)
		return taskstest.Fixture{Store: taskpg.Store{DB: runtime}, Events: func(ctx context.Context, scope domain.Scope) ([]tasks.Event, error) {
			var events []tasks.Event
			err := storage.WithTenant(ctx, runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				rows, err := tx.Query(ctx, `SELECT id,resource_id,event_type,actor_type,actor_id,payload,created_at FROM audit_events WHERE tenant_id=$1 AND project_id=$2 ORDER BY id`, scope.TenantID, scope.ProjectID)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var e tasks.Event
					if err := rows.Scan(&e.ID, &e.TaskID, &e.Type, &e.ActorType, &e.ActorID, &e.Payload, &e.CreatedAt); err != nil {
						return err
					}
					events = append(events, e)
				}
				return rows.Err()
			})
			return events, err
		}}
	})
}
