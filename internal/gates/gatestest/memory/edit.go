package memory

import (
	"context"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/memdb"
)

// The helpers below change the facts a gate subject binds, the way their owning
// modules would (a run re-pinned, an artifact replaced, a work item cancelled).
// They are for tests only.

// EditRun applies fn to a copy of the stored run and stores the result.
func (e *Env) EditRun(ctx context.Context, scope domain.Scope, id domain.RunID, fn func(*execution.Aggregate)) error {
	return e.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		row, ok := runTable.Get(tx, projectKey(scope, string(id)))
		if !ok {
			return gates.ErrNotFound
		}
		agg := cloneAggregate(row.agg)
		fn(agg)
		return runTable.Put(tx, runRow{agg: agg})
	})
}

// ReplaceArtifact replaces an artifact's bytes and digest; it stays usable.
func (e *Env) ReplaceArtifact(ctx context.Context, scope domain.Scope, id domain.ArtifactID, data []byte) error {
	return e.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		a, ok := artTable.Get(tx, projectKey(scope, string(id)))
		if !ok {
			return gates.ErrNotFound
		}
		a.Data, a.Meta.Digest, a.Meta.Size = data, digestOf(data), int64(len(data))
		return artTable.Put(tx, a)
	})
}

// EditArtifact applies fn to a copy of the artifact's metadata.
func (e *Env) EditArtifact(ctx context.Context, scope domain.Scope, id domain.ArtifactID, fn func(*gates.EvidenceMeta)) error {
	return e.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		a, ok := artTable.Get(tx, projectKey(scope, string(id)))
		if !ok {
			return gates.ErrNotFound
		}
		fn(&a.Meta)
		return artTable.Put(tx, a)
	})
}

// CancelWork cancels the work item.
func (e *Env) CancelWork(ctx context.Context, scope domain.Scope, item domain.WorkItemID) error {
	return e.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		w, ok := workTable.Get(tx, projectKey(scope, string(item)))
		if !ok {
			return gates.ErrNotFound
		}
		w.Cancelled = true
		return workTable.Put(tx, w)
	})
}

// Restart rebuilds the service over the same durable records, as a new process
// would. Nothing is carried over in memory.
func (e *Env) Restart() { e.build() }
