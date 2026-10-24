package kmsproduction

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/envelope"
	"github.com/optimiware/kodama/internal/secrets/kms/production"
)

const installation = "inst-t028"

var now = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func valueContext() envelope.Context {
	return envelope.Context{InstallationID: installation, TenantID: "t_acme", ProjectID: "p_one", SecretID: "sec_1", Name: "TOKEN",
		Version: 1, ScopeKeyID: "sk_1", KeyGeneration: 1}
}

func newScopeKey(t *testing.T, p *production.Provider) (secrets.ScopeKey, []byte, envelope.Envelope) {
	t.Helper()
	material, err := envelope.NewScopeKey()
	if err != nil {
		t.Fatal(err)
	}
	sk0, err := secrets.NewScopeKey("t_acme", "sk_1", 1, secrets.WrappedKey{Bytes: []byte("x"), Algorithm: "x", KEK: secrets.KEKRef{Provider: "x", ID: "x", Version: "x"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	w, err := p.Wrap(context.Background(), sk0.WrapContext(installation), material)
	if err != nil {
		t.Fatal(err)
	}
	sk, err := secrets.NewScopeKey("t_acme", "sk_1", 1, w, now)
	if err != nil {
		t.Fatal(err)
	}
	env, err := envelope.Seal(material, valueContext(), []byte("super-secret-value"))
	if err != nil {
		t.Fatal(err)
	}
	return sk, material, env
}

// Test_kms_rewrap_scope_key: rotating the KEK changes only the wrapping
// revision and wrapped bytes of the existing ScopeKey, never its identity,
// generation, or any value ciphertext.
func Test_kms_rewrap_scope_key(t *testing.T) {
	for _, c := range clouds() {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			s := &sim{}
			clients := c.clients(t, s)
			v1, err := production.New(ctx, c.config("v1", "v1"), clients)
			if err != nil {
				t.Fatal(err)
			}
			sk, material, env := newScopeKey(t, v1)
			sealed := envelope.Envelope{FormatVersion: env.FormatVersion, Algorithm: env.Algorithm,
				Nonce: bytes.Clone(env.Nonce), Ciphertext: bytes.Clone(env.Ciphertext)}

			// The rotated installation retains v1 and wraps with v2.
			v2, err := production.New(ctx, c.config("v2", "v1", "v2"), clients)
			if err != nil {
				t.Fatal(err)
			}
			prior := sk
			recovered, err := v2.Unwrap(ctx, sk.WrapContext(installation), sk.Wrapped())
			if err != nil || !bytes.Equal(recovered, material) {
				t.Fatal("retained unwrap", err)
			}
			w, err := v2.Wrap(ctx, sk.WrapContext(installation), recovered)
			if err != nil {
				t.Fatal(err)
			}
			if err := sk.Rewrap(1, w, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if sk.ID() != prior.ID() || sk.Generation() != prior.Generation() || sk.TenantID() != prior.TenantID() {
				t.Fatal("rewrap changed the scope key identity")
			}
			if sk.Revision() != 2 || sk.Wrapped().KEK.Version != "v2" || prior.Wrapped().KEK.Version != "v1" {
				t.Fatalf("revision %d, KEK %+v", sk.Revision(), sk.Wrapped().KEK)
			}
			if bytes.Equal(sk.Wrapped().Bytes, prior.Wrapped().Bytes) {
				t.Fatal("rewrap kept the old wrapped bytes")
			}
			// Value ciphertext and version are unchanged and still open.
			if !bytes.Equal(env.Ciphertext, sealed.Ciphertext) || !bytes.Equal(env.Nonce, sealed.Nonce) || valueContext().Version != 1 {
				t.Fatal("value envelope changed")
			}
			again, err := v2.Unwrap(ctx, sk.WrapContext(installation), sk.Wrapped())
			if err != nil || !bytes.Equal(again, material) {
				t.Fatal("new wrap does not recover the material", err)
			}
			plain, err := envelope.Open(again, valueContext(), sealed)
			if err != nil || string(plain) != "super-secret-value" {
				t.Fatal("value no longer opens after rewrap", err)
			}
			// The old wrap stays retained and usable for backups.
			if old, err := v2.Unwrap(ctx, prior.WrapContext(installation), prior.Wrapped()); err != nil || !bytes.Equal(old, material) {
				t.Fatal("retained wrap lost", err)
			}
			// A stale writer loses and the entity is unchanged.
			stale := sk
			err = stale.Rewrap(1, w, now.Add(2*time.Hour))
			var conflict *secrets.ConflictError
			if !errors.As(err, &conflict) || conflict.Expected != 1 || conflict.Current != 2 || stale.Revision() != 2 {
				t.Fatalf("stale rewrap: %v", err)
			}
		})
	}
}

// Test_kms_rewrap_failure: a failed preparation produces no usable wrap and
// leaves the stored wrap usable; retry succeeds after recovery.
func Test_kms_rewrap_failure(t *testing.T) {
	for _, c := range clouds() {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			s := &sim{}
			clients := c.clients(t, s)
			v1, err := production.New(ctx, c.config("v1", "v1"), clients)
			if err != nil {
				t.Fatal(err)
			}
			sk, material, _ := newScopeKey(t, v1)
			v2, err := production.New(ctx, c.config("v2", "v1", "v2"), clients)
			if err != nil {
				t.Fatal(err)
			}
			before := sk.Record()

			s.down.Store(true)
			key, err := v2.Unwrap(ctx, sk.WrapContext(installation), sk.Wrapped())
			var ke *secrets.KeyError
			if !errors.As(err, &ke) || ke.Class != secrets.KeyUnavailable || !ke.Retryable() || key != nil {
				t.Fatalf("outage during unwrap: %v", err)
			}
			w, err := v2.Wrap(ctx, sk.WrapContext(installation), material)
			if !errors.As(err, &ke) || !ke.Retryable() || w.Bytes != nil {
				t.Fatalf("outage during wrap: %v", err)
			}
			// New startup also fails closed while the provider is down.
			if p, err := production.New(ctx, c.config("v2", "v1", "v2"), clients); p != nil || !errors.Is(err, production.ErrStartupProbe) {
				t.Fatalf("startup during outage: %v", err)
			}
			s.down.Store(false)

			if got := sk.Record(); got.CurrentRevision != before.CurrentRevision || !bytes.Equal(got.Wrapped.Bytes, before.Wrapped.Bytes) || got.Wrapped.KEK != before.Wrapped.KEK {
				t.Fatal("failed preparation changed stored state")
			}
			// Retry after recovery.
			recovered, err := v2.Unwrap(ctx, sk.WrapContext(installation), sk.Wrapped())
			if err != nil || !bytes.Equal(recovered, material) {
				t.Fatal("retry unwrap", err)
			}
			nw, err := v2.Wrap(ctx, sk.WrapContext(installation), recovered)
			if err != nil {
				t.Fatal(err)
			}
			if err := sk.Rewrap(1, nw, now); err != nil || sk.Revision() != 2 {
				t.Fatal(err)
			}
		})
	}
}
