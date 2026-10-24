package worktest_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/worktest"
)

// TestUnitMemoryWorkContract runs the shared work contract on the memory
// adapter: no database, no broker.
func TestUnitMemoryWorkContract(t *testing.T) {
	worktest.Contract(t, worktest.OpenMem)
}

func memScopeA() domain.Scope { return worktest.ScopeA }

func TestUnitMemoryWorkIsDevelopmentOnly(t *testing.T) {
	e := worktest.NewMemEnv(t)
	d, ok := any(e.Store).(interface{ DevelopmentOnly() bool })
	if !ok || !d.DevelopmentOnly() {
		t.Fatal("the memory work store is not flagged development-only")
	}
	if _, err := worktest.NewMemStore(nil, e.Events, e.Auth.Guards); err == nil {
		t.Fatal("a store without a kernel was accepted")
	}
	if _, err := worktest.NewMemStore(e.DB, nil, e.Auth.Guards); err == nil {
		t.Fatal("a store without an event appender was accepted")
	}
	if _, err := worktest.NewMemStore(e.DB, e.Events, nil); err == nil {
		t.Fatal("a store without guards was accepted")
	}
}

// A failing commit leaves nothing behind and is an unknown outcome, never a
// success: the item, its history and its event are all absent.
func TestUnitMemoryWorkCommitFaultIsAnUnknownOutcome(t *testing.T) {
	var fail atomic.Bool
	e := worktest.NewMemEnv(t, memdb.WithCommitFault(func() error {
		if fail.Load() {
			return errors.New("commit refused")
		}
		return nil
	}))
	ctx := context.Background()
	scope := memScopeA()
	if err := e.Store.SeedProject(ctx, scope, 3); err != nil {
		t.Fatal(err)
	}
	if err := e.Auth.Principal(ctx, scope.TenantID, "usr_lead", work.ActorUser, work.PermRead, work.PermCreate); err != nil {
		t.Fatal(err)
	}
	cmd := work.CreateCommand{Scope: scope, Caller: work.Caller{PrincipalID: "usr_lead"}, Kind: work.KindTask, Title: "t", Spec: "s"}
	fail.Store(true)
	if _, err := e.Svc.Create(ctx, cmd); !errors.Is(err, work.ErrUnavailable) {
		t.Fatalf("a failed commit was not reported as an unknown outcome: %v", err)
	}
	fail.Store(false)
	if items, _ := e.Store.List(ctx, scope, work.ListQuery{Limit: 10}); len(items) != 0 {
		t.Fatalf("the failed commit left an item: %+v", items)
	}
	if ids, _ := e.Events.EventIDs(ctx, scope.TenantID); len(ids) != 0 {
		t.Fatalf("the failed commit left an event: %v", ids)
	}
	if _, err := e.Svc.Create(ctx, cmd); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

// A peer given the caller's Tx rolls back with it: the admission check and the
// peer's own reservation are one unit (SD-A12).
func TestUnitMemoryWorkAdmissionJoinsTheCallersTransaction(t *testing.T) {
	e := worktest.NewMemEnv(t)
	ctx := context.Background()
	scope := memScopeA()
	if err := e.Store.SeedProject(ctx, scope, 3); err != nil {
		t.Fatal(err)
	}
	if err := e.Auth.Principal(ctx, scope.TenantID, "usr_lead", work.ActorUser, work.PermRead, work.PermCreate); err != nil {
		t.Fatal(err)
	}
	it, err := e.Svc.Create(ctx, work.CreateCommand{Scope: scope, Caller: work.Caller{PrincipalID: "usr_lead"}, Kind: work.KindTask, Title: "t", Spec: "s"})
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("reservation failed")
	err = e.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		if _, err := worktest.LockActionable(tx, scope, it.ID); err != nil {
			return err
		}
		if err := eventstest.Note(tx, "reservation:"+string(it.ID)); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("update: %v", err)
	}
	if notes, _ := e.Events.Notes(ctx, scope.TenantID); len(notes) != 0 {
		t.Fatalf("the peer's reservation survived the caller's rollback: %v", notes)
	}
	// The cancellation either precedes or follows the whole reservation unit.
	if err := e.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		_, err := worktest.LockActionable(tx, scope, it.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUnitMemoryWorkAssigneeMustBeAMember(t *testing.T) {
	e := worktest.NewMemEnv(t)
	ctx := context.Background()
	scope := memScopeA()
	if err := e.Store.SeedProject(ctx, scope, 3); err != nil {
		t.Fatal(err)
	}
	if err := e.Auth.Principal(ctx, scope.TenantID, "usr_lead", work.ActorUser, work.PermRead, work.PermCreate, work.PermEditTechnical); err != nil {
		t.Fatal(err)
	}
	cmd := work.CreateCommand{Scope: scope, Caller: work.Caller{PrincipalID: "usr_lead"}, Kind: work.KindTask, Title: "t", Spec: "s", AssigneeID: "usr_dev"}
	if _, err := e.Svc.Create(ctx, cmd); !errors.Is(err, work.ErrNotFound) {
		t.Fatalf("an assignee without a membership: %v", err)
	}
	if err := e.Store.AddMember(ctx, scope.TenantID, "usr_dev", scope.ProjectID); err != nil {
		t.Fatal(err)
	}
	it, err := e.Svc.Create(ctx, cmd)
	if err != nil || it.AssigneeID != "usr_dev" {
		t.Fatalf("assignee with a membership: %+v %v", it, err)
	}
	// A membership is tenant scoped: another tenant's member is no assignee here.
	if err := e.Store.SeedProject(ctx, worktest.ScopeB, 3); err != nil {
		t.Fatal(err)
	}
	if err := e.Auth.Principal(ctx, worktest.ScopeB.TenantID, "usr_lead", work.ActorUser, work.PermRead, work.PermCreate, work.PermEditTechnical); err != nil {
		t.Fatal(err)
	}
	cmd.Scope = worktest.ScopeB
	if _, err := e.Svc.Create(ctx, cmd); !errors.Is(err, work.ErrNotFound) {
		t.Fatalf("tenant A's member assigned in tenant B: %v", err)
	}
}
