package idtest_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/identity/idtest"
	"github.com/optimiware/kodama/internal/memdb"
)

// TestUnitMemoryIdentityContract runs the shared identity contract on the memory
// adapter: no database.
func TestUnitMemoryIdentityContract(t *testing.T) {
	idtest.Contract(t, idtest.OpenMem)
}

func TestUnitMemoryIdentityIsDevelopmentOnly(t *testing.T) {
	d, ok := any(idtest.NewMem()).(interface{ DevelopmentOnly() bool })
	if !ok || !d.DevelopmentOnly() {
		t.Fatal("the memory identity store is not flagged development-only")
	}
}

// TestUnitMemoryIdentityJoinsASharedTransaction proves that an identity mutation
// run through TransactTx commits and rolls back with the caller's own writes:
// the binding, the revision, the audit rows and the events are one unit with a
// write of another module.
func TestUnitMemoryIdentityJoinsASharedTransaction(t *testing.T) {
	ctx := context.Background()
	const tenant, project = domain.TenantID("t088_join"), domain.ProjectID("p088_join")
	const user, admin = identity.UserID("usr_joined"), identity.UserID("usr_admin")

	var failCommit atomic.Bool
	kernel := memdb.New(memdb.WithCommitFault(func() error {
		if failCommit.Load() {
			return errors.New("commit refused")
		}
		return nil
	}))
	bus, err := eventstest.NewMem(kernel, eventstest.DefaultMemOptions())
	if err != nil {
		t.Fatal(err)
	}
	m, err := idtest.NewMemOn(kernel, bus)
	if err != nil {
		t.Fatal(err)
	}
	bus.AddTenant(tenant)
	m.AddTenant(tenant, string(tenant))
	m.AddProject(tenant, project, string(project))
	m.AddUser(user, "https://issuer.test", "joined")
	m.AddUser(admin, "https://issuer.test", "admin")
	m.AddBinding(tenant, "mb_admin", admin, "", identity.RoleAdmin)

	grant := func(ctx context.Context, tx identity.Tx) error {
		if err := tx.LockAuthorization(ctx, []identity.UserID{user}, []identity.UserID{admin}); err != nil {
			return err
		}
		b, err := identity.Grant("mb_joined", tenant, user, "", identity.RoleViewer, time.Now())
		if err != nil {
			return err
		}
		if err := tx.InsertBinding(ctx, b); err != nil {
			return err
		}
		rev, err := tx.BumpAuthorization(ctx, user)
		if err != nil {
			return err
		}
		return tx.Record(ctx, identity.Change{
			ID: "chg_joined", Kind: identity.ChangeBindingGranted, Actor: identity.UserActor(admin), OccurredAt: time.Now(), Tenant: tenant,
			Bindings: []identity.BindingChange{{User: user, Ops: []identity.BindingOp{{ID: b.ID, Kind: identity.ChangeBindingGranted, Role: b.Role, Version: b.Version}}, EffectiveVersion: rev}},
		})
	}
	state := func() (bindings int, revision int64, audit, evs []string) {
		t.Helper()
		bs, err := m.CurrentBindings(ctx, tenant, user)
		if err != nil {
			t.Fatal(err)
		}
		if audit, err = m.AuditActions(ctx, tenant); err != nil {
			t.Fatal(err)
		}
		if evs, err = bus.EventIDs(ctx, tenant); err != nil {
			t.Fatal(err)
		}
		return len(bs), m.Revision(tenant, user), audit, evs
	}

	// The caller's own write fails after the identity mutation: everything undone.
	boom := errors.New("the caller's later write failed")
	err = kernel.Update(ctx, tenant, func(tx memdb.Tx) error {
		if err := m.TransactTx(ctx, tx, grant); err != nil {
			return err
		}
		// The joined authority sees the grant made in the same unit at once.
		if err := m.AuthorityIn(tx).Authorize(ctx, identity.UserActor(user), identity.Target{TenantID: tenant, ProjectID: project}, identity.PermProjectRead); err != nil {
			t.Errorf("a grant in the same unit is not visible to its authority: %v", err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("the unit: %v", err)
	}
	if n, rev, audit, evs := state(); n != 0 || rev != 0 || len(audit) != 0 || len(evs) != 0 {
		t.Fatalf("a rolled-back unit left %d bindings, revision %d, audit %v, events %v", n, rev, audit, evs)
	}
	if got, _ := m.ChangesOf(ctx, tenant); len(got) != 0 {
		t.Fatalf("a rolled-back unit left changes: %+v", got)
	}

	// A failed commit undoes it as well.
	failCommit.Store(true)
	if err := kernel.Update(ctx, tenant, func(tx memdb.Tx) error { return m.TransactTx(ctx, tx, grant) }); err == nil {
		t.Fatal("a refused commit reported success")
	}
	failCommit.Store(false)
	if n, rev, audit, evs := state(); n != 0 || rev != 0 || len(audit) != 0 || len(evs) != 0 {
		t.Fatalf("a refused commit left %d bindings, revision %d, audit %v, events %v", n, rev, audit, evs)
	}

	// Committed with the caller: all of it is visible together.
	if err := kernel.Update(ctx, tenant, func(tx memdb.Tx) error { return m.TransactTx(ctx, tx, grant) }); err != nil {
		t.Fatal(err)
	}
	n, rev, audit, evs := state()
	if n != 1 || rev != 1 || !slices.Equal(audit, []string{string(identity.ChangeBindingGranted)}) || len(evs) != 1 {
		t.Fatalf("the committed unit: %d bindings, revision %d, audit %v, events %v", n, rev, audit, evs)
	}
	if got, _ := m.ChangesOf(ctx, tenant); len(got) != 1 || got[0].ID != "chg_joined" {
		t.Fatalf("changes: %+v", got)
	}
}
