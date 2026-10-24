package rtest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// The scopes of the contract.
var (
	ContractScopeA  = domain.Scope{TenantID: "t088_acme", ProjectID: "p088_web"}
	ContractScopeA2 = domain.Scope{TenantID: "t088_acme", ProjectID: "p088_api"}
	ContractScopeB  = domain.Scope{TenantID: "t088_globex", ProjectID: "p088_ops"}
)

// Env is the runtime-authority store under contract, with the execution
// owner's side of the seam: the attempt rows the grant transactions lock and the
// owner's cancellation transaction.
type Env interface {
	Store() runtimeauth.Store
	// SeedProject creates the tenant (if new) and the project.
	SeedProject(t testing.TB, scope domain.Scope)
	// SeedAttempt creates an active attempt at revision 1.
	SeedAttempt(t testing.TB, scope domain.Scope, run domain.RunID, attempt domain.AttemptID, job runtimeauth.ExpectedJob, digest runtimeauth.Digest, actions runtimeauth.ActionSet)
	// Narrow is the owner's change of the attempt's authority: a new revision
	// and a narrower action set.
	Narrow(t testing.TB, scope domain.Scope, attempt domain.AttemptID, revision int64, actions runtimeauth.ActionSet)
	// Cancel is the owner's cancellation transaction: lock the attempt, mark it
	// cancelled and, when revoke is true, revoke its grants in the same
	// transaction through the module's seam.
	Cancel(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, revoke bool, now time.Time) error
}

// Open builds an empty Env.
type Open func(testing.TB) Env

const (
	podJobUID = "11111111-2222-4333-8444-555555555555"
	podUID    = "66666666-7777-4888-9999-aaaaaaaaaaaa"
	podName   = "kodama-att-1-x7k2p"
)

var contractNow = time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)

// Contract is the behavior every runtimeauth.Store adapter shares: credential
// lookup without a tenant selector, tenant-scoped grant reads and writes,
// registration that is idempotent for the same attempt and unique per
// credential, a write-once consumption, an irreversible revocation, the
// authoritative attempt read in the same transaction, all-or-nothing
// transactions and an owner cancellation that revokes in its own transaction.
// It asserts outcomes, not lock order or isolation levels.
func Contract(t *testing.T, open Open) {
	t.Helper()
	ctx := context.Background()
	actions, err := runtimeauth.NewActionSet(runtimeauth.ActionContextRead, runtimeauth.ActionTokenRenew, runtimeauth.ActionResultSubmit)
	if err != nil {
		t.Fatal(err)
	}
	narrower, err := runtimeauth.NewActionSet(runtimeauth.ActionContextRead)
	if err != nil {
		t.Fatal(err)
	}

	seeded := map[domain.AttemptID]bool{}
	fresh := func(t *testing.T) Env {
		t.Helper()
		env := open(t)
		for _, s := range []domain.Scope{ContractScopeA, ContractScopeA2, ContractScopeB} {
			env.SeedProject(t, s)
		}
		return env
	}
	// newGrant builds an unconsumed grant for the attempt with a fresh credential
	// and seeds the attempt in the execution owner's rows.
	newGrant := func(t *testing.T, env Env, scope domain.Scope, attempt domain.AttemptID) (runtimeauth.AdmissionGrant, runtimeauth.Credential) {
		t.Helper()
		cred, err := runtimeauth.GenerateCredential(nil)
		if err != nil {
			t.Fatal(err)
		}
		doc := []byte(`{"attempt":"` + string(attempt) + `"}`)
		job := runtimeauth.ExpectedJob{Namespace: "t088-runs", Name: "kodama-" + strings.ReplaceAll(string(attempt), "_", "-")}
		digest := runtimeauth.NonceDigest(string(attempt)) // any stable 32-byte digest of the pinned context
		g, err := runtimeauth.NewAdmissionGrant(runtimeauth.Provision{
			Scope: scope, RunID: "run_t088", AttemptID: attempt, Job: job, CredentialDigest: cred.Digest(), Generation: 1,
			Actions: actions, AuthorityRevision: 1, ContextDigest: digest, Context: doc,
		}, contractNow, TestPolicy)
		if err != nil {
			t.Fatal(err)
		}
		if !seeded[attempt] {
			seeded[attempt] = true
			env.SeedAttempt(t, scope, "run_t088", attempt, job, digest, actions)
		}
		return g, cred
	}
	do := func(t *testing.T, env Env, tenant domain.TenantID, fn func(tx runtimeauth.GrantTx) error) error {
		t.Helper()
		return env.Store().Do(ctx, tenant, func(ctx context.Context, tx runtimeauth.GrantTx) error { return fn(tx) })
	}
	register := func(t *testing.T, env Env, g runtimeauth.AdmissionGrant) (stored runtimeauth.AdmissionGrant, inserted bool, err error) {
		t.Helper()
		err = do(t, env, g.Scope.TenantID, func(tx runtimeauth.GrantTx) (err error) {
			stored, inserted, err = tx.InsertGrant(ctx, g)
			return err
		})
		return
	}
	mustRegister := func(t *testing.T, env Env, g runtimeauth.AdmissionGrant) {
		t.Helper()
		if _, inserted, err := register(t, env, g); err != nil || !inserted {
			t.Fatalf("register %s: inserted %v, %v", g.AttemptID, inserted, err)
		}
	}
	read := func(t *testing.T, env Env, scope domain.Scope, d runtimeauth.Digest) runtimeauth.AdmissionGrant {
		t.Helper()
		var g runtimeauth.AdmissionGrant
		if err := do(t, env, scope.TenantID, func(tx runtimeauth.GrantTx) (err error) {
			g, err = tx.GrantByCredential(ctx, d, runtimeauth.LockShare)
			return err
		}); err != nil {
			t.Fatalf("read grant: %v", err)
		}
		return g
	}
	session := func(t *testing.T) runtimeauth.SessionID {
		t.Helper()
		s, err := runtimeauth.NewSessionID(nil)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	pod := runtimeauth.PodBinding{JobUID: podJobUID, PodName: podName, PodUID: podUID}

	t.Run("a registered credential is located without a tenant selector", func(t *testing.T) {
		env := fresh(t)
		g, cred := newGrant(t, env, ContractScopeA, "att_t088_1")
		if _, err := env.Store().Locate(ctx, cred.Digest()); !errors.Is(err, runtimeauth.ErrInvalidCredential) {
			t.Fatalf("locate before registration: %v", err)
		}
		mustRegister(t, env, g)
		loc, err := env.Store().Locate(ctx, cred.Digest())
		if err != nil || loc.Scope != ContractScopeA || loc.AttemptID != "att_t088_1" {
			t.Fatalf("locate: %+v %v", loc, err)
		}
		other, _ := runtimeauth.GenerateCredential(nil)
		if _, err := env.Store().Locate(ctx, other.Digest()); !errors.Is(err, runtimeauth.ErrInvalidCredential) {
			t.Fatalf("an unknown digest: %v", err)
		}
		stored := read(t, env, ContractScopeA, cred.Digest())
		if !stored.SameRegistration(g) || stored.Consumed() || stored.Revoked() || !stored.ExpiresAt.Equal(g.ExpiresAt) {
			t.Fatalf("stored grant differs from the registration: %+v vs %+v", stored, g)
		}
	})

	t.Run("registration is idempotent per attempt and unique per credential", func(t *testing.T) {
		env := fresh(t)
		g, cred := newGrant(t, env, ContractScopeA, "att_t088_2")
		mustRegister(t, env, g)
		again, inserted, err := register(t, env, g)
		if err != nil || inserted || !again.SameRegistration(g) {
			t.Fatalf("replay of the same registration: %+v inserted %v %v", again, inserted, err)
		}
		// Another credential for the same attempt returns the stored grant and
		// leaves it untouched: the caller compares and refuses.
		other, _ := newGrant(t, env, ContractScopeA, "att_t088_2")
		got, inserted, err := register(t, env, other)
		if err != nil || inserted || got.SameRegistration(other) || !got.CredentialDigest.Equal(cred.Digest()) {
			t.Fatalf("a different registration of the same attempt: %+v inserted %v %v", got, inserted, err)
		}
		// The same credential cannot open a second attempt, in this tenant or another.
		dup, _ := newGrant(t, env, ContractScopeA, "att_t088_3")
		dup.CredentialDigest = cred.Digest()
		if _, _, err := register(t, env, dup); !errors.Is(err, runtimeauth.ErrConflict) {
			t.Fatalf("a credential reused for another attempt: %v", err)
		}
		foreign, _ := newGrant(t, env, ContractScopeB, "att_t088_4")
		foreign.CredentialDigest = cred.Digest()
		if _, _, err := register(t, env, foreign); !errors.Is(err, runtimeauth.ErrConflict) {
			t.Fatalf("a credential reused in another tenant: %v", err)
		}
		loc, err := env.Store().Locate(ctx, cred.Digest())
		if err != nil || loc.Scope != ContractScopeA || loc.AttemptID != "att_t088_2" {
			t.Fatalf("the credential moved: %+v %v", loc, err)
		}
	})

	t.Run("grants are visible to their own tenant only", func(t *testing.T) {
		env := fresh(t)
		g, cred := newGrant(t, env, ContractScopeA, "att_t088_5")
		mustRegister(t, env, g)
		err := do(t, env, ContractScopeB.TenantID, func(tx runtimeauth.GrantTx) error {
			_, err := tx.GrantByCredential(ctx, cred.Digest(), runtimeauth.LockNone)
			return err
		})
		if !errors.Is(err, runtimeauth.ErrNotFound) {
			t.Fatalf("tenant B read tenant A's grant: %v", err)
		}
		err = do(t, env, ContractScopeB.TenantID, func(tx runtimeauth.GrantTx) error {
			_, _, err := tx.InsertGrant(ctx, g)
			return err
		})
		if !errors.Is(err, runtimeauth.ErrInvalidRequest) {
			t.Fatalf("tenant B registered a grant of tenant A: %v", err)
		}
		err = do(t, env, ContractScopeB.TenantID, func(tx runtimeauth.GrantTx) error {
			_, err := tx.LockAttempt(ctx, ContractScopeA, "att_t088_5")
			return err
		})
		if !errors.Is(err, runtimeauth.ErrNotFound) {
			t.Fatalf("tenant B locked tenant A's attempt: %v", err)
		}
		err = do(t, env, ContractScopeB.TenantID, func(tx runtimeauth.GrantTx) error {
			_, err := tx.RevokeAttempt(ctx, ContractScopeA, "att_t088_5", contractNow)
			return err
		})
		if !errors.Is(err, runtimeauth.ErrInvalidRequest) {
			t.Fatalf("tenant B revoked tenant A's attempt: %v", err)
		}
		if read(t, env, ContractScopeA, cred.Digest()).Revoked() {
			t.Fatal("a foreign revocation reached the grant")
		}
		// A sibling project of the same tenant does not own the attempt either.
		var n int
		if err := do(t, env, ContractScopeA.TenantID, func(tx runtimeauth.GrantTx) (err error) {
			n, err = tx.RevokeAttempt(ctx, ContractScopeA2, "att_t088_5", contractNow)
			return err
		}); err != nil || n != 0 {
			t.Fatalf("revocation through another project: %d %v", n, err)
		}
	})

	t.Run("the first consumption is written once and unlocks a session", func(t *testing.T) {
		env := fresh(t)
		g, cred := newGrant(t, env, ContractScopeA, "att_t088_6")
		mustRegister(t, env, g)
		sid := session(t)
		nonce := runtimeauth.NonceDigest("nonce-1")
		if err := do(t, env, ContractScopeA.TenantID, func(tx runtimeauth.GrantTx) error {
			cur, err := tx.GrantByCredential(ctx, cred.Digest(), runtimeauth.LockUpdate)
			if err != nil {
				return err
			}
			if _, err := cur.Consume(nonce, pod, sid, contractNow, TestPolicy); err != nil {
				return err
			}
			return tx.SaveConsumption(ctx, cur)
		}); err != nil {
			t.Fatalf("consume: %v", err)
		}
		stored := read(t, env, ContractScopeA, cred.Digest())
		if !stored.Consumed() || stored.Consumption.SessionID != sid || stored.Consumption.Pod != pod || !stored.Consumption.NonceDigest.Equal(nonce) {
			t.Fatalf("stored consumption: %+v", stored.Consumption)
		}
		var bySession runtimeauth.AdmissionGrant
		if err := do(t, env, ContractScopeA.TenantID, func(tx runtimeauth.GrantTx) (err error) {
			bySession, err = tx.GrantBySession(ctx, sid, runtimeauth.LockShare)
			return err
		}); err != nil || bySession.AttemptID != "att_t088_6" {
			t.Fatalf("by session: %+v %v", bySession, err)
		}
		// A second writer holding the earlier, unconsumed view cannot consume again.
		again := g
		if _, err := again.Consume(runtimeauth.NonceDigest("nonce-2"), pod, session(t), contractNow, TestPolicy); err != nil {
			t.Fatal(err)
		}
		err := do(t, env, ContractScopeA.TenantID, func(tx runtimeauth.GrantTx) error { return tx.SaveConsumption(ctx, again) })
		if !errors.Is(err, runtimeauth.ErrConflict) {
			t.Fatalf("a second consumption: %v", err)
		}
		if got := read(t, env, ContractScopeA, cred.Digest()); got.Consumption.SessionID != sid {
			t.Fatal("a refused consumption replaced the session")
		}
		for _, bad := range []runtimeauth.SessionID{"", "ses_short", "not-a-session"} {
			err := do(t, env, ContractScopeA.TenantID, func(tx runtimeauth.GrantTx) error {
				_, err := tx.GrantBySession(ctx, bad, runtimeauth.LockNone)
				return err
			})
			if !errors.Is(err, runtimeauth.ErrNotFound) {
				t.Fatalf("session %q: %v", bad, err)
			}
		}
		err = do(t, env, ContractScopeB.TenantID, func(tx runtimeauth.GrantTx) error {
			_, err := tx.GrantBySession(ctx, sid, runtimeauth.LockNone)
			return err
		})
		if !errors.Is(err, runtimeauth.ErrNotFound) {
			t.Fatalf("another tenant's session: %v", err)
		}
	})

	t.Run("a revocation is irreversible and blocks consumption", func(t *testing.T) {
		env := fresh(t)
		g, cred := newGrant(t, env, ContractScopeA, "att_t088_7")
		mustRegister(t, env, g)
		first := contractNow.Add(time.Minute)
		var n int
		if err := do(t, env, ContractScopeA.TenantID, func(tx runtimeauth.GrantTx) (err error) {
			n, err = tx.RevokeAttempt(ctx, ContractScopeA, "att_t088_7", first)
			return err
		}); err != nil || n != 1 {
			t.Fatalf("revoke: %d %v", n, err)
		}
		if err := do(t, env, ContractScopeA.TenantID, func(tx runtimeauth.GrantTx) (err error) {
			n, err = tx.RevokeAttempt(ctx, ContractScopeA, "att_t088_7", first.Add(time.Hour))
			return err
		}); err != nil || n != 0 {
			t.Fatalf("a repeated revocation: %d %v", n, err)
		}
		stored := read(t, env, ContractScopeA, cred.Digest())
		if !stored.Revoked() || !stored.RevokedAt.Equal(first) {
			t.Fatalf("the revocation time moved: %+v", stored.RevokedAt)
		}
		// SaveRevocation on a revoked grant keeps the first revocation.
		later := stored
		at := first.Add(2 * time.Hour)
		later.RevokedAt = &at
		if err := do(t, env, ContractScopeA.TenantID, func(tx runtimeauth.GrantTx) error { return tx.SaveRevocation(ctx, later) }); err != nil {
			t.Fatal(err)
		}
		if got := read(t, env, ContractScopeA, cred.Digest()); !got.RevokedAt.Equal(first) {
			t.Fatalf("a later revocation replaced the first: %v", got.RevokedAt)
		}
		retry := g
		if _, err := retry.Consume(runtimeauth.NonceDigest("n"), pod, session(t), contractNow, TestPolicy); err != nil {
			t.Fatal(err)
		}
		if err := do(t, env, ContractScopeA.TenantID, func(tx runtimeauth.GrantTx) error { return tx.SaveConsumption(ctx, retry) }); !errors.Is(err, runtimeauth.ErrConflict) {
			t.Fatalf("consuming a revoked grant: %v", err)
		}
		// A revocation of an attempt with no grant revokes nothing.
		if err := do(t, env, ContractScopeA.TenantID, func(tx runtimeauth.GrantTx) (err error) {
			n, err = tx.RevokeAttempt(ctx, ContractScopeA, "att_t088_none", first)
			return err
		}); err != nil || n != 0 {
			t.Fatalf("revoking a missing attempt: %d %v", n, err)
		}
	})

	t.Run("a transaction commits all of its writes or none", func(t *testing.T) {
		env := fresh(t)
		g, cred := newGrant(t, env, ContractScopeA, "att_t088_8")
		boom := errors.New("registration failed after the insert")
		err := do(t, env, ContractScopeA.TenantID, func(tx runtimeauth.GrantTx) error {
			if _, inserted, err := tx.InsertGrant(ctx, g); err != nil || !inserted {
				return errors.Join(errors.New("insert"), err)
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("do: %v", err)
		}
		if _, err := env.Store().Locate(ctx, cred.Digest()); !errors.Is(err, runtimeauth.ErrInvalidCredential) {
			t.Fatalf("a rolled-back registration is locatable: %v", err)
		}
		err = do(t, env, ContractScopeA.TenantID, func(tx runtimeauth.GrantTx) error {
			_, err := tx.GrantByCredential(ctx, cred.Digest(), runtimeauth.LockNone)
			return err
		})
		if !errors.Is(err, runtimeauth.ErrNotFound) {
			t.Fatalf("a rolled-back registration is readable: %v", err)
		}
		// The credential and the attempt are free again.
		mustRegister(t, env, g)
		// A failure after consumption and revocation writes undoes both.
		sid := session(t)
		err = do(t, env, ContractScopeA.TenantID, func(tx runtimeauth.GrantTx) error {
			cur, err := tx.GrantByCredential(ctx, cred.Digest(), runtimeauth.LockUpdate)
			if err != nil {
				return err
			}
			if _, err := cur.Consume(runtimeauth.NonceDigest("n"), pod, sid, contractNow, TestPolicy); err != nil {
				return err
			}
			if err := tx.SaveConsumption(ctx, cur); err != nil {
				return err
			}
			if _, err := tx.RevokeAttempt(ctx, ContractScopeA, "att_t088_8", contractNow); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("do: %v", err)
		}
		if got := read(t, env, ContractScopeA, cred.Digest()); got.Consumed() || got.Revoked() {
			t.Fatalf("a failed transaction left writes behind: %+v", got)
		}
	})

	t.Run("the attempt is re-read authoritatively in the transaction", func(t *testing.T) {
		env := fresh(t)
		g, _ := newGrant(t, env, ContractScopeA, "att_t088_9")
		mustRegister(t, env, g)
		lock := func() (runtimeauth.AttemptState, error) {
			var st runtimeauth.AttemptState
			err := do(t, env, ContractScopeA.TenantID, func(tx runtimeauth.GrantTx) (err error) {
				st, err = tx.LockAttempt(ctx, ContractScopeA, "att_t088_9")
				return err
			})
			return st, err
		}
		st, err := lock()
		if err != nil || st.Phase != runtimeauth.AttemptActive || st.Revision != 1 || !st.Actions.Equal(actions) || st.RunID != "run_t088" || st.AttemptID != "att_t088_9" {
			t.Fatalf("attempt state: %+v %v", st, err)
		}
		env.Narrow(t, ContractScopeA, "att_t088_9", 2, narrower)
		if st, err = lock(); err != nil || st.Revision != 2 || !st.Actions.Equal(narrower) {
			t.Fatalf("after the owner narrowed the authority: %+v %v", st, err)
		}
		err = do(t, env, ContractScopeA.TenantID, func(tx runtimeauth.GrantTx) error {
			_, err := tx.LockAttempt(ctx, ContractScopeA, "att_t088_none")
			return err
		})
		if !errors.Is(err, runtimeauth.ErrNotFound) {
			t.Fatalf("an unknown attempt: %v", err)
		}
		err = do(t, env, ContractScopeA.TenantID, func(tx runtimeauth.GrantTx) error {
			_, err := tx.LockAttempt(ctx, ContractScopeA2, "att_t088_9")
			return err
		})
		if !errors.Is(err, runtimeauth.ErrNotFound) {
			t.Fatalf("an attempt of another project: %v", err)
		}
	})

	t.Run("an owner cancellation revokes in its own transaction", func(t *testing.T) {
		env := fresh(t)
		g, cred := newGrant(t, env, ContractScopeA, "att_t088_10")
		mustRegister(t, env, g)
		if err := env.Cancel(ctx, ContractScopeA, "att_t088_10", true, contractNow.Add(time.Minute)); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		stored := read(t, env, ContractScopeA, cred.Digest())
		if !stored.Revoked() {
			t.Fatal("the cancellation committed without revoking the grant")
		}
		var st runtimeauth.AttemptState
		if err := do(t, env, ContractScopeA.TenantID, func(tx runtimeauth.GrantTx) (err error) {
			st, err = tx.LockAttempt(ctx, ContractScopeA, "att_t088_10")
			return err
		}); err != nil || st.Phase != runtimeauth.AttemptCancelled {
			t.Fatalf("attempt after cancellation: %+v %v", st, err)
		}

		// An owner that only changed state leaves the grant for the currentness
		// checks, which read the phase.
		g2, cred2 := newGrant(t, env, ContractScopeA, "att_t088_11")
		mustRegister(t, env, g2)
		if err := env.Cancel(ctx, ContractScopeA, "att_t088_11", false, contractNow); err != nil {
			t.Fatal(err)
		}
		if read(t, env, ContractScopeA, cred2.Digest()).Revoked() {
			t.Fatal("a cancellation without revocation revoked the grant")
		}
		if err := env.Cancel(ctx, ContractScopeA, "att_t088_unknown", true, contractNow); !errors.Is(err, runtimeauth.ErrNotFound) {
			t.Fatalf("cancelling an unknown attempt: %v", err)
		}
		if err := env.Cancel(ctx, ContractScopeB, "att_t088_11", true, contractNow); !errors.Is(err, runtimeauth.ErrNotFound) {
			t.Fatalf("tenant B cancelling tenant A's attempt: %v", err)
		}
	})
}
