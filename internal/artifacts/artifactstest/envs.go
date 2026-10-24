package artifactstest

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/storage"
)

// MemRepoEnv is the artifact persistence in memory over one memdb kernel.
type MemRepoEnv struct {
	DB    *memdb.Store
	Store *MemStore
}

var _ RepoEnv = (*MemRepoEnv)(nil)

// NewMemRepoEnv returns the memory persistence over a fresh kernel.
func NewMemRepoEnv(t testing.TB, opts ...memdb.Option) *MemRepoEnv {
	t.Helper()
	db := memdb.New(opts...)
	store, err := NewMemStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return &MemRepoEnv{DB: db, Store: store}
}

// OpenMemRepo is the OpenRepo of the memory adapter.
func OpenMemRepo(t testing.TB) RepoEnv { return NewMemRepoEnv(t) }

func (e *MemRepoEnv) Repository() artifacts.Repository { return e.Store }
func (e *MemRepoEnv) Cleanup() artifacts.CleanupStore  { return e.Store }

func (e *MemRepoEnv) ObjectStates(t testing.TB, scope domain.Scope, id domain.ArtifactID) map[string]string {
	t.Helper()
	out, err := e.Store.ObjectStates(context.Background(), scope, id)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *MemRepoEnv) AuditActions(t testing.TB, scope domain.Scope, id domain.ArtifactID) []string {
	t.Helper()
	out, err := e.Store.AuditActions(context.Background(), scope, id)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// PGRepoEnv adapts the PostgreSQL environment to the contract's RepoEnv.
type PGRepoEnv struct{ E *Env }

var _ RepoEnv = PGRepoEnv{}

// OpenPostgresRepo is the OpenRepo of the PostgreSQL adapter: an isolated
// migrated database with the two seeded tenants. It skips without PostgreSQL,
// and always in the unit layer.
func OpenPostgresRepo(t testing.TB) RepoEnv { return PGRepoEnv{E: NewDB(t, Options{})} }

func (e PGRepoEnv) Repository() artifacts.Repository { return e.E.Store }
func (e PGRepoEnv) Cleanup() artifacts.CleanupStore  { return e.E.Store }

func (e PGRepoEnv) ObjectStates(t testing.TB, scope domain.Scope, id domain.ArtifactID) map[string]string {
	t.Helper()
	return e.E.ObjectRecordsIn(scope, id)
}

func (e PGRepoEnv) AuditActions(t testing.TB, scope domain.Scope, id domain.ArtifactID) []string {
	t.Helper()
	var out []string
	// The audit policy is tenant-only: read in the tenant's context.
	err := storage.WithTenant(context.Background(), e.E.Owner, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT action FROM audit WHERE tenant_id = $1 AND resource_type = 'artifact' AND resource_id = $2 AND project_id = $3`, string(scope.TenantID), string(id), string(scope.ProjectID))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}
