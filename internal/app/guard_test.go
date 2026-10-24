package app

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/memdb"
	rpg "github.com/optimiware/kodama/internal/results/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

// txRecorder is the execution store's transaction-joined update, reduced to
// the one thing the guard must control: whether the mutation ran. It calls the
// mutation the guard wraps, as the real store does inside the run's update.
type txRecorder struct {
	execution.Store
	ran int
}

func (r *txRecorder) UpdateInTx(ctx context.Context, _ memdb.Tx, _ domain.Scope, _ domain.RunID, _ execution.UpdateOptions, fn execution.Mutation) (*execution.Aggregate, any, error) {
	if _, err := fn(ctx, nil, nil); err != nil {
		return nil, nil, err
	}
	r.ran++
	return nil, nil, nil
}

type guardFixture struct {
	stack   *rtest.MemStack
	store   execution.Store
	inner   *txRecorder
	session rpg.Session
	scope   domain.Scope
	run     domain.RunID
	attempt domain.AttemptID
}

func newGuardFixture(t *testing.T) *guardFixture {
	t.Helper()
	ctx := context.Background()
	s := rtest.NewMemStack(t)
	c, frozen := s.HappyContext()
	a := s.SeedAttempt(c, rtest.AllActions())
	cred, _, err := s.Register(a, rtest.AllActions())
	if err != nil {
		t.Fatal(err)
	}
	req := frozen
	req.AdmissionCredential = cred.Reveal()
	resp, err := s.Service.Admit(ctx, a.Context.AttemptID, req)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := s.Verifier.Verify(ctx, resp.Token.Token)
	if err != nil {
		t.Fatal(err)
	}
	inner := &txRecorder{}
	store, err := NewMemoryGuardedStore(inner, s.Env.DB, inner, func(tx memdb.Tx) runtimeauth.GrantTx { return rtest.BindMem(tx, s.Env.Attempts) }, s.Ring.Generation)
	if err != nil {
		t.Fatal(err)
	}
	store.(*memGuardedStore).now = s.Clock.Now
	return &guardFixture{
		stack: s, store: store, inner: inner, scope: a.Scope, run: a.Context.RunID, attempt: a.Context.AttemptID,
		session: rpg.Session{Tenant: claims.Tenant, ID: claims.Session, Generation: claims.Generation, Route: a.Context.AttemptID, Action: runtimeauth.ActionResultSubmit},
	}
}

func (f *guardFixture) update(ctx context.Context, scope domain.Scope, run domain.RunID) error {
	_, _, err := f.store.Update(ctx, scope, run, execution.UpdateOptions{}, func(context.Context, execution.Env, *execution.Aggregate) (execution.Effects, error) {
		return execution.Effects{}, nil
	})
	return err
}

func TestMemoryGuardedStoreBindsTheSessionToItsScopeAndRun(t *testing.T) {
	f := newGuardFixture(t)
	ctx := rpg.WithSession(context.Background(), f.session)

	// Without a session, or with a session of another tenant, nothing runs.
	if err := f.update(context.Background(), f.scope, f.run); !errors.Is(err, runtimeauth.ErrInvalidCredential) {
		t.Fatalf("no session: %v", err)
	}
	foreign := f.session
	foreign.Tenant = "t_other"
	if err := f.update(rpg.WithSession(context.Background(), foreign), f.scope, f.run); !errors.Is(err, runtimeauth.ErrInvalidCredential) {
		t.Fatalf("a session of another tenant: %v", err)
	}
	// A valid session authorizes nothing on another run of the same tenant and
	// project, nor on another project of the same tenant.
	if err := f.update(ctx, f.scope, "run_another_run"); !errors.Is(err, runtimeauth.ErrInvalidCredential) {
		t.Fatalf("a valid session mutated another run: %v", err)
	}
	other := domain.Scope{TenantID: f.scope.TenantID, ProjectID: "p_another_project"}
	if err := f.update(ctx, other, f.run); err == nil {
		t.Fatal("a valid session mutated another project")
	}
	if f.inner.ran != 0 {
		t.Fatalf("%d mutations ran without a matching session", f.inner.ran)
	}
	// Its own run is served.
	if err := f.update(ctx, f.scope, f.run); err != nil || f.inner.ran != 1 {
		t.Fatalf("the session's own run: %v ran=%d", err, f.inner.ran)
	}
}

func TestMemoryGuardedStoreRefusesAfterRevocation(t *testing.T) {
	f := newGuardFixture(t)
	ctx := rpg.WithSession(context.Background(), f.session)
	if err := f.update(ctx, f.scope, f.run); err != nil || f.inner.ran != 1 {
		t.Fatalf("before revocation: %v ran=%d", err, f.inner.ran)
	}
	// The owner's cancellation revokes the grants in its own update.
	if err := f.stack.Env.Attempts.Cancel(context.Background(), f.scope, f.attempt, true, f.stack.Clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.update(ctx, f.scope, f.run); !errors.Is(err, runtimeauth.ErrAttemptRevoked) {
		t.Fatalf("after revocation: %v", err)
	}
	if f.inner.ran != 1 {
		t.Fatalf("a revoked session mutated: ran=%d", f.inner.ran)
	}
}

// The check and the mutation are one memdb update: a mutation that fails after
// the session passed leaves the update discarded, and the guard reports the
// commit failure as an unknown outcome rather than an acknowledgment.
func TestMemoryGuardedStoreCheckAndMutationShareOneUpdate(t *testing.T) {
	f := newGuardFixture(t)
	ctx := rpg.WithSession(context.Background(), f.session)
	boom := errors.New("mutation refused")
	_, _, err := f.store.Update(ctx, f.scope, f.run, execution.UpdateOptions{}, func(context.Context, execution.Env, *execution.Aggregate) (execution.Effects, error) {
		return execution.Effects{}, boom
	})
	if !errors.Is(err, boom) || f.inner.ran != 0 {
		t.Fatalf("err=%v ran=%d", err, f.inner.ran)
	}
	if _, err := NewMemoryGuardedStore(nil, nil, nil, nil, nil); err == nil {
		t.Fatal("an incomplete guard was accepted")
	}
}
