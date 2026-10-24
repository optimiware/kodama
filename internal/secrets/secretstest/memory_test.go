package secretstest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/secretstest"
)

// TestUnitMemorySecretsContract runs the shared secrets contract on the memory
// adapter: no database, the ephemeral key wrapper.
func TestUnitMemorySecretsContract(t *testing.T) {
	secretstest.StoreContract(t, secretstest.OpenMem)
}

func TestUnitMemorySecretsAreDevelopmentOnly(t *testing.T) {
	e := secretstest.NewMemEnv(t)
	src := e.Source(t, e.Wrapper())
	for _, v := range []any{e.Store_, e.Wrap, src} {
		d, ok := v.(interface{ DevelopmentOnly() bool })
		if !ok || !d.DevelopmentOnly() {
			t.Fatalf("%T is not flagged development-only", v)
		}
	}
	if _, err := secretstest.NewMemStore(secretstest.MemConfig{}); err == nil {
		t.Fatal("a store without its collaborators was accepted")
	}
	if _, err := secretstest.NewMemVersionSource(e.Store_, nil, "inst"); err == nil {
		t.Fatal("a version source without a key wrapper was accepted")
	}
	// The ephemeral KEK dies with the wrapper: another wrapper cannot unwrap it.
	other, err := secretstest.NewEphemeralWrapper()
	if err != nil {
		t.Fatal(err)
	}
	w, err := e.Wrap.Wrap(context.Background(), secrets.WrapContext{InstallationID: "i", TenantID: "t", Generation: 1, KeyID: "k"}, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Unwrap(context.Background(), secrets.WrapContext{InstallationID: "i", TenantID: "t", Generation: 1, KeyID: "k"}, w); err == nil {
		t.Fatal("a fresh ephemeral wrapper unwrapped another's wrap")
	}
}

// A provider outage is a classified, retryable failure and never a fallback: no
// secret is sealed and no scope key is created while the wrapper is down.
func TestUnitMemoryEphemeralWrapperOutage(t *testing.T) {
	ctx := context.Background()
	e := secretstest.NewMemEnv(t)
	svc := e.Service(t, e.Wrap)
	e.Wrap.SetDown(errors.New("provider down"))
	m := secrets.Mutation{Actor: secrets.Actor{Type: secrets.ActorUser, ID: "alice"}, Now: time.Now()}
	_, err := svc.PrepareSecret(ctx, m, secretstest.ScopeA, "api-key", secrets.KindGeneric, []byte("v"))
	var ke *secrets.KeyError
	if !errors.As(err, &ke) || !ke.Retryable() {
		t.Fatalf("a provider outage: %v", err)
	}
	if _, err := e.Store_.ActiveScopeKey(ctx, secretstest.ScopeA.TenantID); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("a scope key was created during the outage: %v", err)
	}
	e.Wrap.SetDown(nil)
	if _, err := svc.PrepareSecret(ctx, m, secretstest.ScopeA, "api-key", secrets.KindGeneric, []byte("v")); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
	if wraps, _ := e.Wrap.Calls(); wraps != 2 {
		t.Fatalf("wrap attempts: %d", wraps)
	}
}

// Pin registration joins the caller's Tx: a run admission that fails after
// registering its pins leaves none behind, and a deletion committed first closes
// admission (SD-A12).
func TestUnitMemoryPinsJoinTheAdmissionTransaction(t *testing.T) {
	ctx := context.Background()
	e := secretstest.NewMemEnv(t)
	svc := e.Service(t, e.Wrap)
	m := secrets.Mutation{Actor: secrets.Actor{Type: secrets.ActorUser, ID: "alice"}, Now: time.Now()}
	p, err := svc.PrepareSecret(ctx, m, secretstest.ScopeA, "api-key", secrets.KindGeneric, []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Atomically(ctx, secretstest.ScopeA.TenantID, func(ctx context.Context, w secrets.Writer) error {
		_, err := svc.CreateSecret(ctx, w, m, p)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	b := []runtimev1.SecretBinding{{Ref: secrets.FormatSecretRef("api-key"), Version: 1, Name: "tok", Delivery: "env"}}
	consumer := secretstest.RunConsumer("run_join")
	boom := errors.New("the admission failed after registering its pins")
	err = e.DB.Update(ctx, secretstest.ScopeA.TenantID, func(tx memdb.Tx) error {
		if err := e.Store_.RegisterPins(tx, secretstest.PinRequest{Scope: secretstest.ScopeA, Consumer: consumer, Bindings: b}); err != nil {
			return err
		}
		if err := eventstest.Note(tx, "run:run_join"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("update: %v", err)
	}
	if found, _, _ := e.Store_.HasPin(ctx, secretstest.ScopeA, consumer, p.Secret.ID(), 1); found {
		t.Fatal("the pin survived the admission's rollback")
	}
	if notes, _ := e.Events.Notes(ctx, secretstest.ScopeA.TenantID); len(notes) != 0 {
		t.Fatalf("the admission's own write survived: %v", notes)
	}
	// Another tenant's Tx reaches nothing.
	err = e.DB.Update(ctx, secretstest.ScopeB.TenantID, func(tx memdb.Tx) error {
		return e.Store_.RegisterPins(tx, secretstest.PinRequest{Scope: secretstest.ScopeA, Consumer: consumer, Bindings: b})
	})
	if !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("a Tx of tenant B pinned tenant A's secret: %v", err)
	}
}
