package rtest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

// ResourceAuthority is a transaction-aware stand-in for execution's committed
// resource state (T-113), backed by a real table so locks, stops and admissions
// serialize in PostgreSQL exactly as the documented lock order requires. It is a
// fixture: the module's behavior is proved against it, execution's is not.
type ResourceAuthority struct {
	s *Stack
	Pods
	pause func()
}

var (
	_ rtpg.ResourceAuthority        = (*ResourceAuthority)(nil)
	_ runtimeauth.ResourceWorkloads = (*ResourceAuthority)(nil)
)

// SetStopPause runs fn inside every Stop transaction after its writes and before
// it commits, holding the resource lock; tests use it to order a stop against a
// concurrent admission deterministically.
func (a *ResourceAuthority) SetStopPause(fn func()) { a.pause = fn }

func newResourceAuthority(t testing.TB, s *Stack) *ResourceAuthority {
	t.Helper()
	s.OwnerExec(`CREATE TABLE t113_resources (
		tenant_id text NOT NULL, project_id text NOT NULL, run_id text NOT NULL, resource_key text NOT NULL,
		phase text NOT NULL, fingerprint text NOT NULL, workload_name text NOT NULL, workload_uid text NOT NULL,
		deadline_at timestamptz NOT NULL, context jsonb NOT NULL, PRIMARY KEY (tenant_id, run_id, resource_key))`)
	s.OwnerExec(`ALTER TABLE t113_resources ENABLE ROW LEVEL SECURITY`)
	s.OwnerExec(`CREATE POLICY tenant_isolation ON t113_resources USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id())`)
	s.OwnerExec(`GRANT SELECT, INSERT, UPDATE ON t113_resources TO ` + pgx.Identifier{s.DB.RuntimeRole}.Sanitize())
	return &ResourceAuthority{s: s}
}

// Put inserts or replaces the committed state of a resource.
func (a *ResourceAuthority) Put(st runtimeauth.ResourceState) {
	a.s.T.Helper()
	doc, err := json.Marshal(st.Context)
	if err != nil {
		a.s.T.Fatal(err)
	}
	a.s.OwnerExec(`INSERT INTO t113_resources (tenant_id, project_id, run_id, resource_key, phase, fingerprint, workload_name, workload_uid, deadline_at, context)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (tenant_id, run_id, resource_key) DO UPDATE SET phase = $5, fingerprint = $6, workload_name = $7, workload_uid = $8, deadline_at = $9, context = $10`,
		string(st.Scope.TenantID), string(st.Scope.ProjectID), string(st.RunID), st.Key, string(st.Phase), st.Fingerprint, st.WorkloadName, st.WorkloadUID, st.DeadlineAt.UTC(), doc)
}

const resourceStateColumns = `tenant_id, project_id, run_id, resource_key, phase, fingerprint, workload_name, workload_uid, deadline_at, context`

func scanResourceState(row pgx.Row) (runtimeauth.ResourceState, error) {
	var (
		st                               runtimeauth.ResourceState
		tenant, project, run, key, phase string
		deadline                         time.Time
		doc                              []byte
	)
	if err := row.Scan(&tenant, &project, &run, &key, &phase, &st.Fingerprint, &st.WorkloadName, &st.WorkloadUID, &deadline, &doc); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return st, runtimeauth.ErrNotFound
		}
		return st, err
	}
	st.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	st.RunID, st.Key, st.Phase, st.DeadlineAt = domain.RunID(run), key, runtimeauth.ResourcePhase(phase), deadline.UTC()
	var err error
	if st.Kind, err = runtimeauth.ResourceKindOf(key); err != nil {
		return st, err
	}
	st.Context, err = runtimev2.DecodeRequest[runtimev2.ResourceContext](doc)
	return st, err
}

// ObserveResource implements runtimeauth.ResourceObserver.
func (a *ResourceAuthority) ObserveResource(ctx context.Context, ref runtimeauth.ResourceRef) (runtimeauth.ResourceState, error) {
	var st runtimeauth.ResourceState
	err := storage.WithTenant(ctx, a.s.Runtime, ref.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		st, err = scanResourceState(tx.QueryRow(ctx, `SELECT `+resourceStateColumns+` FROM t113_resources WHERE tenant_id = $1 AND run_id = $2 AND resource_key = $3`,
			string(ref.Scope.TenantID), string(ref.RunID), ref.Key))
		return err
	})
	return st, err
}

// LockResource implements postgres.ResourceAuthority: FOR SHARE conflicts with
// Stop's FOR UPDATE.
func (a *ResourceAuthority) LockResource(ctx context.Context, tx pgx.Tx, ref runtimeauth.ResourceRef) (runtimeauth.ResourceState, error) {
	return scanResourceState(tx.QueryRow(ctx, `SELECT `+resourceStateColumns+` FROM t113_resources WHERE tenant_id = $1 AND run_id = $2 AND resource_key = $3 FOR SHARE`,
		string(ref.Scope.TenantID), string(ref.RunID), ref.Key))
}

// Stop is the execution owner's stop transaction: lock the resource (FOR UPDATE),
// mark it ended and, when revoke is true, revoke its grant through the module's
// transaction seam. With revoke false it models an owner that only changed state,
// proving the currentness checks do not depend on revocation having run.
func (a *ResourceAuthority) Stop(ctx context.Context, ref runtimeauth.ResourceRef, revoke bool) error {
	return storage.WithTenant(ctx, a.s.Runtime, ref.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := scanResourceState(tx.QueryRow(ctx, `SELECT `+resourceStateColumns+` FROM t113_resources WHERE tenant_id = $1 AND run_id = $2 AND resource_key = $3 FOR UPDATE`,
			string(ref.Scope.TenantID), string(ref.RunID), ref.Key)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE t113_resources SET phase = 'ended' WHERE tenant_id = $1 AND run_id = $2 AND resource_key = $3`,
			string(ref.Scope.TenantID), string(ref.RunID), ref.Key); err != nil {
			return err
		}
		if revoke {
			if _, err := rtpg.RevokeResource(ctx, tx, ref, a.s.Clock.Now()); err != nil {
				return err
			}
		}
		if a.pause != nil {
			a.pause()
		}
		return nil
	})
}

// Modify runs an arbitrary owner-side UPDATE of the resource row.
func (a *ResourceAuthority) Modify(set string, args ...any) {
	a.s.OwnerExec(`UPDATE t113_resources SET `+set, args...)
}
