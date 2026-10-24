package rtest_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

// TestUnitMemoryRuntimeauthContract runs the shared runtime-authority contract
// on the memory adapter: no database.
func TestUnitMemoryRuntimeauthContract(t *testing.T) {
	rtest.Contract(t, rtest.OpenMem)
}

func TestUnitMemoryRuntimeauthIsDevelopmentOnly(t *testing.T) {
	e := rtest.NewMemEnv(t)
	for _, v := range []any{e.Grants, e.Attempts} {
		d, ok := v.(interface{ DevelopmentOnly() bool })
		if !ok || !d.DevelopmentOnly() {
			t.Fatalf("%T is not flagged development-only", v)
		}
	}
	if _, err := rtest.NewMemStore(nil, e.Attempts); err == nil {
		t.Fatal("a store without a kernel was accepted")
	}
	if _, err := rtest.NewMemStore(e.DB, nil); err == nil {
		t.Fatal("a store without an attempt authority was accepted")
	}
}

// The whole service runs on the memory store: registration, the one-time
// admission, authorized use, renewal, and the owner's cancellation, which ends
// authority whether or not it revoked in its own transaction.
func TestUnitMemoryRuntimeauthServiceLifecycle(t *testing.T) {
	ctx := context.Background()
	for _, revoke := range []bool{true, false} {
		t.Run(fmt.Sprintf("cancel with revocation %v", revoke), func(t *testing.T) {
			s := rtest.NewMemStack(t)
			c, frozen := s.HappyContext()
			a := s.SeedAttempt(c, rtest.AllActions())
			cred, reg, err := s.Register(a, rtest.AllActions())
			if err != nil || !reg.Created {
				t.Fatalf("register: %+v %v", reg, err)
			}
			if again, err := s.RegisterWith(a, cred, rtest.AllActions()); err != nil || again.Created || !again.ExpiresAt.Equal(reg.ExpiresAt) {
				t.Fatalf("a retried registration: %+v %v", again, err)
			}
			req := frozen
			req.AdmissionCredential = cred.Reveal()
			resp, err := s.Service.Admit(ctx, a.Context.AttemptID, req)
			if err != nil || resp.Token.Token == "" {
				t.Fatalf("admit: %v", err)
			}
			if _, err := s.Service.Admit(ctx, a.Context.AttemptID, s.AdmitRequest(a, cred, "another-nonce-another-nonce")); !errors.Is(err, runtimeauth.ErrAdmissionConsumed) {
				t.Fatalf("a second nonce: %v", err)
			}
			authz, err := s.Service.Authorize(ctx, runtimeauth.AuthorizeRequest{AttemptID: a.Context.AttemptID, Bearer: resp.Token.Token, Action: runtimeauth.ActionResultSubmit})
			if err != nil || authz.Scope != a.Scope || authz.AttemptID != a.Context.AttemptID {
				t.Fatalf("authorize: %+v %v", authz, err)
			}
			if _, err := s.Service.Renew(ctx, a.Context.AttemptID, resp.Token.Token); err != nil {
				t.Fatalf("renew: %v", err)
			}
			if err := s.Env.Attempts.Cancel(ctx, a.Scope, a.Context.AttemptID, revoke, s.Clock.Now()); err != nil {
				t.Fatal(err)
			}
			// Every action stops, except that an unrevoked grant may still replay a
			// result receipt (T-022's terminal receipt exception).
			for _, action := range []runtimeauth.Action{runtimeauth.ActionResultSubmit, runtimeauth.ActionContextRead} {
				_, err := s.Service.Authorize(ctx, runtimeauth.AuthorizeRequest{AttemptID: a.Context.AttemptID, Bearer: resp.Token.Token, Action: action})
				receipt := action == runtimeauth.ActionResultSubmit && !revoke
				if receipt && err != nil || !receipt && !errors.Is(err, runtimeauth.ErrAttemptRevoked) {
					t.Fatalf("%s after cancellation (revoked %v): %v", action, revoke, err)
				}
			}
			if _, err := s.Service.Renew(ctx, a.Context.AttemptID, resp.Token.Token); !errors.Is(err, runtimeauth.ErrAttemptRevoked) {
				t.Fatalf("renew after cancellation: %v", err)
			}
		})
	}
}

// A failed commit leaves no registration behind and is an unknown outcome.
func TestUnitMemoryRuntimeauthCommitFault(t *testing.T) {
	var fail atomic.Bool
	s := rtest.NewMemStack(t, memdb.WithCommitFault(func() error {
		if fail.Load() {
			return errors.New("commit refused")
		}
		return nil
	}))
	c, _ := s.HappyContext()
	a := s.SeedAttempt(c, rtest.AllActions())
	fail.Store(true)
	cred, _, err := s.Register(a, rtest.AllActions())
	if !errors.Is(err, runtimeauth.ErrUnavailable) {
		t.Fatalf("a failed commit: %v", err)
	}
	fail.Store(false)
	if _, err := s.Env.Grants.Locate(context.Background(), cred.Digest()); !errors.Is(err, runtimeauth.ErrInvalidCredential) {
		t.Fatalf("the failed registration is locatable: %v", err)
	}
	if reg, err := s.RegisterWith(a, cred, rtest.AllActions()); err != nil || !reg.Created {
		t.Fatalf("retry: %+v %v", reg, err)
	}
}

// The execution owner's cancellation and the revocation share one Tx: when the
// owner's own transaction fails afterwards, both are undone (SD-A12).
func TestUnitMemoryRuntimeauthRevocationJoinsTheOwnersTransaction(t *testing.T) {
	ctx := context.Background()
	e := rtest.NewMemEnv(t)
	scope := rtest.ContractScopeA
	actions, _ := runtimeauth.NewActionSet(runtimeauth.ActionContextRead)
	cred, _ := runtimeauth.GenerateCredential(nil)
	job := runtimeauth.ExpectedJob{Namespace: "t088-runs", Name: "kodama-att-join"}
	digest := runtimeauth.NonceDigest("pinned")
	e.SeedAttempt(t, scope, "run_t088", "att_join", job, digest, actions)
	g, err := runtimeauth.NewAdmissionGrant(runtimeauth.Provision{Scope: scope, RunID: "run_t088", AttemptID: "att_join", Job: job,
		CredentialDigest: cred.Digest(), Generation: 1, Actions: actions, AuthorityRevision: 1, ContextDigest: digest, Context: []byte("{}")},
		time.Now(), rtest.TestPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Grants.Do(ctx, scope.TenantID, func(ctx context.Context, tx runtimeauth.GrantTx) error {
		_, _, err := tx.InsertGrant(ctx, g)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("the owner's cancellation failed after revoking")
	err = e.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		if n, err := rtest.RevokeAttemptMem(tx, scope, "att_join", time.Now()); err != nil || n != 1 {
			return errors.Join(errors.New("revoke"), err)
		}
		// The seam sees its own revocation inside the Tx...
		got, err := rtest.BindMem(tx, e.Attempts).GrantByCredential(ctx, cred.Digest(), runtimeauth.LockShare)
		if err != nil || !got.Revoked() {
			return errors.Join(errors.New("the revocation is not visible in its own Tx"), err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("update: %v", err)
	}
	// ...and the rollback undid it.
	if err := e.Grants.Do(ctx, scope.TenantID, func(ctx context.Context, tx runtimeauth.GrantTx) error {
		got, err := tx.GrantByCredential(ctx, cred.Digest(), runtimeauth.LockShare)
		if err != nil {
			return err
		}
		if got.Revoked() {
			return errors.New("a rolled-back revocation survived")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
