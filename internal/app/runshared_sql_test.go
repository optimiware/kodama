package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/storage"
)

// Test_sql_run_shared_resource_authority: the resource authority over the real
// execution tables. LockResource reads execution's committed state inside the
// caller's tenant transaction, writes nothing, and holds the run's lock, so a
// cancellation requested meanwhile waits for the caller and the next read sees
// the resource ended; an absent resource is absence, never an outage. It skips in
// the unit layer (it needs the PostgreSQL services of the `test` environment).
func Test_sql_run_shared_resource_authority(t *testing.T) {
	env, s := executiontest.NewSharedPG(t)
	ctx := context.Background()
	scope := executiontest.SharedScope

	def := s.DefineShared(executiontest.SharedSpecDoc(false, executiontest.SharedStep("build")))
	run, err := s.Svc.Admit(ctx, execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t116_sql"}, execution.AdmitCommand{
		Scope: scope, WorkItemID: s.Project.Item, DefinitionID: def.ID, ContentRevision: s.Seed.ContentRevision(s.Project), SourceCommit: executiontest.CommitA,
	})
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	s.Resources.AutoReady = true
	if _, err := s.Svc.ClaimRunnable(ctx, scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
		t.Fatalf("the first claim only starts resource holding: %v", err)
	}
	ref := execution.RunRef{Scope: scope, ID: run.ID}
	var uid string
	for i := 0; i < 6 && uid == ""; i++ {
		if _, err := s.Svc.ReconcileRun(ctx, ref); err != nil && !errors.Is(err, execution.ErrBusy) {
			t.Fatalf("reconcile: %v", err)
		}
		if agg, err := s.Store.Get(ctx, scope, run.ID); err == nil && agg.Resource("preparation") != nil {
			uid = agg.Resource("preparation").UID
		}
	}
	if uid == "" {
		t.Fatal("the preparation was never provisioned")
	}

	auth, err := newResourceAuthority(env.Store, nil)
	if err != nil {
		t.Fatal(err)
	}
	rref := runtimeauth.ResourceRef{Scope: scope, RunID: run.ID, Key: "preparation"}

	// A locked read returns the committed state and writes nothing.
	before, _ := env.Store.Get(ctx, scope, run.ID)
	var state runtimeauth.ResourceState
	if err := storage.WithTenant(ctx, env.Runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		state, err = auth.LockResource(ctx, tx, rref)
		return err
	}); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if state.Phase != runtimeauth.ResourceActive || state.WorkloadUID != uid || state.Key != "preparation" || state.Scope != scope || state.Context.Preparation == nil {
		t.Fatalf("state: %+v", state)
	}
	if observed, err := auth.ObserveResource(ctx, rref); err != nil || observed.WorkloadUID != uid || observed.Fingerprint != state.Fingerprint {
		t.Fatalf("observe: %+v %v", observed, err)
	}
	after, _ := env.Store.Get(ctx, scope, run.ID)
	if after.Run.Version != before.Run.Version || len(after.Resources) != len(before.Resources) {
		t.Fatalf("a read changed the run: version %d -> %d", before.Run.Version, after.Run.Version)
	}

	// Absence is absence; another tenant's run is not visible at all.
	if err := storage.WithTenant(ctx, env.Runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := auth.LockResource(ctx, tx, runtimeauth.ResourceRef{Scope: scope, RunID: run.ID, Key: "service.nope"})
		return err
	}); !errors.Is(err, runtimeauth.ErrNotFound) {
		t.Fatalf("an absent resource: %v", err)
	}
	if _, err := auth.ObserveResource(ctx, runtimeauth.ResourceRef{Scope: scope, RunID: "run_missing", Key: "preparation"}); !errors.Is(err, runtimeauth.ErrNotFound) {
		t.Fatalf("an absent run: %v", err)
	}

	// The lock conflicts with a cancellation: it waits for the open transaction and
	// the next read sees the resource ended.
	cancelled := make(chan error, 1)
	err = storage.WithTenant(ctx, env.Runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := auth.LockResource(ctx, tx, rref); err != nil {
			return err
		}
		go func() {
			_, err := s.Svc.Cancel(context.Background(), execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t116_cancel"}, scope, run.ID)
			cancelled <- err
		}()
		select {
		case err := <-cancelled:
			t.Errorf("the cancellation did not wait for the lock holder (%v)", err)
		case <-time.After(400 * time.Millisecond):
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-cancelled:
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancellation never completed after the lock holder committed")
	}
	if ended, err := auth.ObserveResource(ctx, rref); err != nil || ended.Phase != runtimeauth.ResourceEnded {
		t.Fatalf("after the cancellation: %+v %v", ended, err)
	}
}
