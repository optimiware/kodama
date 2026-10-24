package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/secrets"
)

// state is a fingerprint of every encrypted and audit row the module owns.
type state struct {
	secrets, versions, scopeKeys, wraps, audits, events, pins int
}

func (e *env) state() state {
	return state{
		secrets:   e.count(`SELECT count(*) FROM secrets`),
		versions:  e.count(`SELECT count(*) FROM secret_versions`),
		scopeKeys: e.count(`SELECT count(*) FROM secret_scope_keys`),
		wraps:     e.count(`SELECT count(*) FROM secret_scope_key_wraps`),
		audits:    e.count(`SELECT count(*) FROM audit`),
		events:    e.outboxTypes(),
		pins:      e.count(`SELECT count(*) FROM secret_pins`),
	}
}

var errOutage = &secrets.KeyError{Class: secrets.KeyUnavailable, Op: "test", Err: errors.New("kms timeout")}

func isRetryableKey(err error) bool {
	var ke *secrets.KeyError
	return errors.As(err, &ke) && ke.Retryable()
}

// Test_crypto_failure_atomicity covers VS-09, VS-A07: wrap/unwrap failure,
// cancellation and wrapping CAS loss preserve encrypted state without partial
// mutation, classify the failure and persist no plaintext.
func Test_crypto_failure_atomicity(t *testing.T) {
	const sentinel = "crypto-SENTINEL"

	t.Run("wrap failure while creating the first scope key", func(t *testing.T) {
		e := newEnv(t)
		e.wrapper.set(func(f *faultyWrapper) { f.wrapErr = errOutage })
		before := e.state()
		_, err := e.svc.PrepareSecret(e.ctx(), e.mutation("alice"), scopeA1, "erp-token", secrets.KindGeneric, []byte(sentinel))
		if !isRetryableKey(err) {
			t.Fatalf("outage not reported as retryable: %v", err)
		}
		if e.state() != before {
			t.Fatalf("state changed: %+v -> %+v", before, e.state())
		}
		// No fallback: with the provider denying, nothing is created either.
		e.wrapper.set(func(f *faultyWrapper) { f.wrapErr = &secrets.KeyError{Class: secrets.KeyDenied, Op: "wrap"} })
		_, err = e.svc.PrepareSecret(e.ctx(), e.mutation("alice"), scopeA1, "erp-token", secrets.KindGeneric, []byte(sentinel))
		var ke *secrets.KeyError
		if !errors.As(err, &ke) || ke.Class != secrets.KeyDenied || ke.Retryable() {
			t.Fatalf("denial: %v", err)
		}
		if e.state() != before {
			t.Fatal("state changed after denial")
		}
		// Recovery: the same operation succeeds once the provider is back.
		e.wrapper.set(func(f *faultyWrapper) { f.wrapErr = nil })
		e.createSecret(scopeA1, "erp-token", sentinel)
		notContains(t, "persisted metadata", e.dumpAll(), sentinel)
	})

	t.Run("unwrap failure while rotating", func(t *testing.T) {
		e := newEnv(t)
		sec := e.createSecret(scopeA1, "erp-token", "one")
		before := e.state()
		e.wrapper.set(func(f *faultyWrapper) { f.unwrapEr = errOutage })
		if _, err := e.rotateErr(scopeA1, sec.ID(), sentinel); !isRetryableKey(err) {
			t.Fatalf("rotation during an outage: %v", err)
		}
		if e.state() != before {
			t.Fatalf("state changed: %+v -> %+v", before, e.state())
		}
		cur, _ := e.store.GetSecret(e.ctx(), scopeA1, sec.ID())
		if cur.CurrentVersion() != 1 || cur.Version() != 1 {
			t.Fatalf("secret moved to %d/%d", cur.CurrentVersion(), cur.Version())
		}
		e.wrapper.set(func(f *faultyWrapper) { f.unwrapEr = nil })
		if s, err := e.rotateErr(scopeA1, sec.ID(), sentinel); err != nil || s.CurrentVersion() != 2 {
			t.Fatalf("rotation after recovery: %v", err)
		}
		notContains(t, "persisted metadata", e.dumpAll(), sentinel)
	})

	t.Run("outage during resolution preserves encrypted state and is retryable", func(t *testing.T) {
		e := newEnv(t)
		e.createSecret(scopeA1, "erp-token", sentinel)
		if err := e.pin(scopeA1, "run_1", "erp-token", 1); err != nil {
			t.Fatal(err)
		}
		auth := newAuthority()
		auth.set(secrets.AttemptGrant{Scope: scopeA1, RunID: "run_1", AttemptID: "att_1", Bindings: []runtimeBinding{binding("erp-token", 1)}})
		r := e.resolver(auth)
		before := e.state()
		e.wrapper.set(func(f *faultyWrapper) { f.unwrapEr = errOutage })
		resp, err := r.Resolve(e.ctx(), "att_1", ref("erp-token", 1), "req_1")
		if !isRetryableKey(err) || resp.ValueBase64 != "" {
			t.Fatalf("outage during resolution: %v", err)
		}
		if e.state() != before {
			t.Fatalf("state changed: %+v -> %+v", before, e.state())
		}
		e.wrapper.set(func(f *faultyWrapper) { f.unwrapEr = nil })
		if resp, err := r.Resolve(e.ctx(), "att_1", ref("erp-token", 1), "req_2"); err != nil || decode(t, resp) != sentinel {
			t.Fatalf("resolution after recovery: %v", err)
		}
	})

	t.Run("rewrap failures change nothing", func(t *testing.T) {
		e := newEnv(t)
		e.createSecret(scopeA1, "erp-token", "one")
		before := e.state()
		for name, set := range map[string]func(*faultyWrapper){
			"wrap":   func(f *faultyWrapper) { f.wrapErr = errOutage },
			"unwrap": func(f *faultyWrapper) { f.unwrapEr = errOutage },
		} {
			e.wrapper.set(set)
			if _, err := e.svc.RewrapScopeKey(e.ctx(), e.mutation("ops"), tenantA, ""); !isRetryableKey(err) {
				t.Fatalf("%s outage during rewrap: %v", name, err)
			}
			e.wrapper.set(func(f *faultyWrapper) { f.wrapErr, f.unwrapEr = nil, nil })
			if e.state() != before {
				t.Fatalf("%s: state changed", name)
			}
		}
		sk, err := e.store.ActiveScopeKey(e.ctx(), tenantA)
		if err != nil || sk.Revision() != 1 {
			t.Fatalf("scope key moved: %v", err)
		}
	})

	t.Run("cancellation leaves no partial mutation", func(t *testing.T) {
		e := newEnv(t)
		sec := e.createSecret(scopeA1, "erp-token", "one")
		cur, _ := e.store.GetSecret(e.ctx(), scopeA1, sec.ID())
		m := e.mutation("alice")
		p, err := e.svc.PrepareVersion(e.ctx(), m, scopeA1, sec.ID(), cur.Version(), []byte(sentinel))
		if err != nil {
			t.Fatal(err)
		}
		before := e.state()
		cctx, cancel := context.WithCancel(e.ctx())
		cancel()
		err = e.svc.Atomically(cctx, tenantA, func(ctx context.Context, w secrets.Writer) error {
			_, err := e.svc.AddVersion(ctx, w, m, p)
			return err
		})
		if err == nil {
			t.Fatal("cancelled write succeeded")
		}
		// Cancellation after the prepared write but before commit: the callback
		// fails after its writes, so everything rolls back together.
		err = e.svc.Atomically(e.ctx(), tenantA, func(ctx context.Context, w secrets.Writer) error {
			if _, err := e.svc.AddVersion(ctx, w, m, p); err != nil {
				return err
			}
			return context.Canceled
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("callback failure: %v", err)
		}
		if e.state() != before {
			t.Fatalf("partial mutation after cancellation: %+v -> %+v", before, e.state())
		}
		// A cancelled key-provider call is classified retryable.
		cctx2, cancel2 := context.WithCancel(e.ctx())
		cancel2()
		e.wrapper.set(func(f *faultyWrapper) {
			f.unwrapEr = &secrets.KeyError{Class: secrets.KeyUnavailable, Op: "unwrap", Err: context.Canceled}
		})
		if _, err := e.svc.PrepareVersion(cctx2, m, scopeA1, sec.ID(), cur.Version(), []byte(sentinel)); err == nil {
			t.Fatal("cancelled preparation succeeded")
		}
		notContains(t, "persisted metadata", e.dumpAll(), sentinel)
	})

	t.Run("wrapping CAS loss keeps the winner's wrap", func(t *testing.T) {
		e := newEnv(t)
		e.createSecret(scopeA1, "erp-token", "one")
		sk, _ := e.store.ActiveScopeKey(e.ctx(), tenantA)
		material, err := e.svc.UnwrapScopeKey(e.ctx(), sk)
		if err != nil {
			t.Fatal(err)
		}
		loser, err := e.wrapper.Wrap(e.ctx(), sk.WrapContext(installation), material)
		if err != nil {
			t.Fatal(err)
		}
		// The winner rewraps first.
		if _, err := e.svc.RewrapScopeKey(e.ctx(), e.mutation("ops"), tenantA, ""); err != nil {
			t.Fatal(err)
		}
		before := e.state()
		err = e.svc.Atomically(e.ctx(), tenantA, func(ctx context.Context, w secrets.Writer) error {
			_, err := w.RewrapScopeKey(ctx, e.mutation("ops"), tenantA, sk.ID(), sk.Revision(), loser)
			return err
		})
		var c *secrets.ConflictError
		if !errors.As(err, &c) || c.Resource != "wrap revision" {
			t.Fatalf("stale rewrap: %v", err)
		}
		if e.state() != before {
			t.Fatal("the losing rewrap changed state")
		}
		now, _ := e.store.ActiveScopeKey(e.ctx(), tenantA)
		if now.Revision() != 2 {
			t.Fatalf("revision %d", now.Revision())
		}
	})
}

// Test_stored_ciphertext_tampering: tampering with or swapping stored envelopes
// fails authentication at resolution (VS-06 at the storage boundary).
func Test_stored_ciphertext_tampering(t *testing.T) {
	e := newEnv(t)
	a := e.createSecret(scopeA1, "alpha", "alpha-SENTINEL")
	b := e.createSecret(scopeA1, "bravo", "bravo-SENTINEL")
	for _, n := range []string{"alpha", "bravo"} {
		if err := e.pin(scopeA1, "run_1", n, 1); err != nil {
			t.Fatal(err)
		}
	}
	auth := newAuthority()
	auth.set(secrets.AttemptGrant{Scope: scopeA1, RunID: "run_1", AttemptID: "att_1", Bindings: []runtimeBinding{binding("alpha", 1), binding("bravo", 1)}})
	r := e.resolver(auth)
	for _, n := range []string{"alpha", "bravo"} {
		if _, err := r.Resolve(e.ctx(), "att_1", ref(n, 1), "r"); err != nil {
			t.Fatalf("baseline %s: %v", n, err)
		}
	}
	// The owner disables the immutability guard to simulate a tampering database administrator.
	e.ownerExec(`ALTER TABLE secret_versions DISABLE TRIGGER secret_versions_guard`)
	e.ownerExec(`UPDATE secret_versions v SET ciphertext = o.ciphertext, nonce = o.nonce
FROM secret_versions o WHERE v.secret_id = $1 AND o.secret_id = $2`, a.ID(), b.ID())
	resp, err := r.Resolve(e.ctx(), "att_1", ref("alpha", 1), "r")
	if !errors.Is(err, secrets.ErrInconsistent) || resp.ValueBase64 != "" {
		t.Fatalf("swapped ciphertext decrypted: %v", err)
	}
	if _, err := r.Resolve(e.ctx(), "att_1", ref("bravo", 1), "r"); err != nil {
		t.Fatalf("the untouched secret failed: %v", err)
	}
	e.ownerExec(`UPDATE secret_versions SET ciphertext = set_byte(ciphertext, 0, get_byte(ciphertext, 0) # 1) WHERE secret_id = $1`, b.ID())
	if _, err := r.Resolve(e.ctx(), "att_1", ref("bravo", 1), "r"); !errors.Is(err, secrets.ErrInconsistent) {
		t.Fatalf("bit-flipped ciphertext decrypted: %v", err)
	}
	// Moving a version to another project/secret identity (the context) also fails.
	e.ownerExec(`UPDATE secret_versions SET ciphertext = set_byte(ciphertext, 0, get_byte(ciphertext, 0) # 1) WHERE secret_id = $1`, b.ID())
	if _, err := r.Resolve(e.ctx(), "att_1", ref("bravo", 1), "r"); err != nil {
		t.Fatalf("restored ciphertext failed: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM audit WHERE action = 'secret.resolve'`); n != 4 {
		t.Fatalf("audited resolutions %d, want only the 4 successful ones", n)
	}
}
