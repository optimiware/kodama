package gitops_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitops"
	"github.com/optimiware/kodama/internal/gitops/gitopstest"
)

type lifecycle struct {
	states map[domain.TenantID]gitops.OffboardingState
}

func (l *lifecycle) Offboarding(_ context.Context, t domain.TenantID) (gitops.OffboardingState, error) {
	return l.states[t], nil
}

// calls is the shared ordered log of the stand-ins.
type calls struct{ log []string }

func (c *calls) add(s string) { c.log = append(c.log, s) }

type grants struct {
	c      *calls
	active map[domain.TenantID]int
}

func (g *grants) RevokeAll(_ context.Context, t domain.TenantID) error {
	g.c.add("revoke " + string(t))
	g.active[t] = 0
	return nil
}
func (g *grants) Active(_ context.Context, t domain.TenantID) (int, error) { return g.active[t], nil }

type storage struct {
	c    *calls
	fail error
}

func (s *storage) OffboardTenant(_ context.Context, t domain.TenantID) error {
	s.c.add("delete " + string(t))
	return s.fail
}

type auditor struct {
	c    *calls
	fail error
	ids  map[string]bool
}

func (a *auditor) Record(_ context.Context, e gitops.AuditEvent) error {
	if a.fail != nil {
		return a.fail
	}
	a.c.add("audit " + e.Action)
	a.ids[e.ID] = true
	return nil
}

// Test_unit_gitops_offboarding_after_retention: grants are revoked and pushes
// settled first; the storage is deleted only after the retention period with no
// hold, with the audit record written before the deletion; nothing is deleted for
// a tenant that is not offboarded, and another tenant is untouched (GR-14).
func Test_unit_gitops_offboarding_after_retention(t *testing.T) {
	ctx := context.Background()
	clock := gitopstest.NewClock()
	log := &calls{}
	lc := &lifecycle{states: map[domain.TenantID]gitops.OffboardingState{}}
	g := &grants{c: log, active: map[domain.TenantID]int{tenantA: 3, tenantB: 5}}
	st := &storage{c: log}
	au := &auditor{c: log, ids: map[string]bool{}}
	catalog := &gitopstest.Catalog{}
	catalog.Add(repo(tenantA, "r_o1"))
	catalog.Add(repo(tenantA, "r_o2"))
	catalog.Add(repo(tenantB, "r_ob"))
	fence := &gitopstest.Fence{}
	pushes := &gitopstest.Pushes{InFlight: map[string]int{"r_o2": 1}, Unknown: map[string]int{"r_o1": 2}, Known: map[string]int64{}}
	settled := 0
	pushes.ReconcileFn = func(string) { settled++ }
	o, err := gitops.NewOffboarding(gitops.OffboardingConfig{Lifecycle: lc, Catalog: catalog, Fence: fence, Grants: g, Pushes: pushes, Storage: st, Audit: au, Clock: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	deleted := func() bool { return slices.Contains(log.log, "delete "+string(tenantA)) }

	// Not offboarded: nothing happens at all.
	if _, err := o.Offboard(ctx, tenantA); !errors.Is(err, gitops.ErrNotOffboarding) {
		t.Fatalf("a tenant that is not offboarded: %v", err)
	}
	if len(log.log) != 0 || fence.IsClosed(tenantA) || g.active[tenantA] != 3 {
		t.Fatalf("an offboarding that was never requested changed something: %v", log.log)
	}
	if ok, err := o.MayDeleteVolume(ctx, tenantA); ok || !errors.Is(err, gitops.ErrNotOffboarding) {
		t.Fatalf("MayDeleteVolume: %v %v", ok, err)
	}

	// Requested, retention in the future, a push still in flight: access closes,
	// grants are revoked, pushes are reconciled, nothing is deleted.
	retain := clock.Now().Add(30 * 24 * time.Hour)
	lc.states[tenantA] = gitops.OffboardingState{Requested: true, RetainUntil: retain}
	res, err := o.Offboard(ctx, tenantA)
	if err != nil || res.Stage != gitops.StageSettling || res.InFlight != 1 || res.Unknown != 2 {
		t.Fatalf("settling: %+v %v", res, err)
	}
	if !fence.IsClosed(tenantA) || g.active[tenantA] != 0 || settled != 2 || deleted() {
		t.Fatalf("closed=%v active=%d reconciled=%d deleted=%v", fence.IsClosed(tenantA), g.active[tenantA], settled, deleted())
	}
	if g.active[tenantB] != 5 || fence.IsClosed(tenantB) {
		t.Fatal("another tenant was touched")
	}
	// Pushes settled, retention not elapsed: waiting.
	pushes.InFlight["r_o2"] = 0
	if res, err = o.Offboard(ctx, tenantA); err != nil || res.Stage != gitops.StageRetaining || !res.RetainUntil.Equal(retain) || deleted() {
		t.Fatalf("retaining: %+v %v", res, err)
	}
	if ok, _ := o.MayDeleteVolume(ctx, tenantA); ok {
		t.Fatal("the storage provider may delete before retention elapsed")
	}
	// Elapsed but on hold.
	clock.Advance(31 * 24 * time.Hour)
	lc.states[tenantA] = gitops.OffboardingState{Requested: true, RetainUntil: retain, Hold: true}
	if res, err = o.Offboard(ctx, tenantA); err != nil || res.Stage != gitops.StageRetaining || deleted() {
		t.Fatalf("hold: %+v %v", res, err)
	}
	// Elapsed, no hold, but the audit cannot be recorded: nothing is deleted.
	lc.states[tenantA] = gitops.OffboardingState{Requested: true, RetainUntil: retain}
	au.fail = errors.New("audit store down")
	if _, err = o.Offboard(ctx, tenantA); !errors.Is(err, gitops.ErrUnavailable) || deleted() {
		t.Fatalf("an unrecorded deletion: %v", err)
	}
	au.fail = nil
	// The storage provider's own gate agrees (grants revoked, nothing in flight, retention elapsed).
	if ok, err := o.MayDeleteVolume(ctx, tenantA); err != nil || !ok {
		t.Fatalf("MayDeleteVolume: %v %v", ok, err)
	}
	// Everything holds: the audit record comes first, then the deletion; a repeat is idempotent.
	log.log = nil
	res, err = o.Offboard(ctx, tenantA)
	if err != nil || res.Stage != gitops.StageDeleted {
		t.Fatalf("deletion: %+v %v", res, err)
	}
	if want := []string{"revoke " + string(tenantA), "audit git.offboarding.delete_storage", "delete " + string(tenantA)}; !slices.Equal(log.log, want) {
		t.Fatalf("order %v, want %v", log.log, want)
	}
	if _, err = o.Offboard(ctx, tenantA); err != nil || len(au.ids) != 1 {
		t.Fatalf("repeat: %v (%d distinct audit events)", err, len(au.ids))
	}
	// The storage provider's refusal is reported, not hidden.
	st.fail = errors.New("retention gate refused")
	if _, err = o.Offboard(ctx, tenantA); err == nil {
		t.Fatal("a refusal of the storage provider was swallowed")
	}
	// Another tenant that is not offboarded stays untouched throughout.
	if g.active[tenantB] != 5 || slices.Contains(log.log, "delete "+string(tenantB)) {
		t.Fatal("another tenant was offboarded")
	}
}

func Test_unit_gitops_offboarding_in_flight_blocks_deletion(t *testing.T) {
	ctx := context.Background()
	clock := gitopstest.NewClock()
	log := &calls{}
	lc := &lifecycle{states: map[domain.TenantID]gitops.OffboardingState{tenantA: {Requested: true, RetainUntil: clock.Now().Add(-time.Hour)}}}
	g := &grants{c: log, active: map[domain.TenantID]int{}}
	catalog := &gitopstest.Catalog{}
	catalog.Add(repo(tenantA, "r_f1"))
	pushes := &gitopstest.Pushes{InFlight: map[string]int{"r_f1": 1}, Unknown: map[string]int{}, Known: map[string]int64{}}
	st := &storage{c: log}
	fence := &gitopstest.Fence{}
	o, err := gitops.NewOffboarding(gitops.OffboardingConfig{Lifecycle: lc, Catalog: catalog, Fence: fence, Grants: g, Pushes: pushes, Storage: st,
		Audit: &auditor{c: log, ids: map[string]bool{}}, Clock: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	res, err := o.Offboard(ctx, tenantA)
	if err != nil || res.Stage != gitops.StageSettling || slices.Contains(log.log, "delete "+string(tenantA)) {
		t.Fatalf("%+v %v %v", res, err, log.log)
	}
	if ok, _ := o.MayDeleteVolume(ctx, tenantA); ok {
		t.Fatal("MayDeleteVolume with a push in flight")
	}
	if _, err := gitops.NewOffboarding(gitops.OffboardingConfig{}); !errors.Is(err, gitops.ErrNotConfigured) {
		t.Fatal(err)
	}
}
