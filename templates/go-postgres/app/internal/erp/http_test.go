package erp_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"example.invalid/inventory/internal/erp"
	"example.invalid/inventory/internal/exceptions"
	"example.invalid/inventory/internal/tenancy"
	"example.invalid/inventory/test/support/erpfake"
)

const (
	tA = tenancy.TenantID("11111111-1111-4111-8111-111111111111")
	tB = tenancy.TenantID("22222222-2222-4222-8222-222222222222")
)

type creds map[tenancy.TenantID]string

func (c creds) Token(t tenancy.TenantID) (string, error) {
	if v, ok := c[t]; ok {
		return v, nil
	}
	return "", errors.New("none")
}

func setup(t *testing.T) (*erp.HTTPAdapter, *erpfake.Server) {
	t.Helper()
	srv := erpfake.New(t, map[string]string{string(tA): "tok-a", string(tB): "tok-b"})
	a, err := erp.NewHTTPAdapter(erp.Config{BaseURL: srv.URL(), Credentials: creds{tA: "tok-a", tB: "tok-b"}, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return a, srv
}

func op(key string, qty int64) exceptions.Operation {
	return exceptions.Operation{Key: key, ERPRef: "WH1/SKU-9", AdjustedQuantity: qty, Reason: "recount", PayloadHash: "h"}
}

func TestAdapterProtocol(t *testing.T) {
	ctx := context.Background()
	a, srv := setup(t)

	out, err := a.Apply(ctx, tA, op("k1", 7))
	if err != nil || out.Status != exceptions.OutcomeAcknowledged || out.Reference == "" {
		t.Fatalf("apply: %v %+v", err, out)
	}
	again, err := a.Apply(ctx, tA, op("k1", 7))
	if err != nil || again.Reference != out.Reference {
		t.Fatalf("a replay returns the original outcome: %v %+v", err, again)
	}
	if e, _ := srv.Effect(string(tA), "k1"); e.Applied != 1 {
		t.Fatalf("one effect: %+v", e)
	}
	// Changed payload under the same key is refused, not applied.
	if c, err := a.Apply(ctx, tA, op("k1", 8)); err != nil || c.Status != exceptions.OutcomeRejected || e(srv, tA, "k1").Payload.Quantity != 7 {
		t.Fatalf("conflict: %v %+v", err, c)
	}
	// Lookup.
	if l, err := a.Lookup(ctx, tA, "k1"); err != nil || l.Status != exceptions.OutcomeAcknowledged {
		t.Fatalf("lookup: %v %+v", err, l)
	}
	if l, err := a.Lookup(ctx, tA, "never"); err != nil || l.Status != exceptions.OutcomeNotFound {
		t.Fatalf("lookup missing: %v %+v", err, l)
	}
	// Tenant separation: the same key in another tenant is a separate operation.
	if _, err := a.Apply(ctx, tB, op("k1", 3)); err != nil {
		t.Fatal(err)
	}
	if srv.TotalEffects() != 2 {
		t.Fatalf("effects %d", srv.TotalEffects())
	}
	// A tenant without a configured credential never borrows another's.
	if _, err := a.Apply(ctx, "33333333-3333-4333-8333-333333333333", op("k1", 1)); err == nil {
		t.Fatal("missing credential must fail closed")
	}
}

func e(s *erpfake.Server, tenant tenancy.TenantID, key string) erpfake.Effect {
	v, _ := s.Effect(string(tenant), key)
	return v
}

func TestAdapterFailureClassification(t *testing.T) {
	ctx := context.Background()
	a, srv := setup(t)

	srv.RejectNext("unknown item")
	if out, err := a.Apply(ctx, tA, op("r1", 1)); err != nil || out.Status != exceptions.OutcomeRejected || out.Reason != "unknown item" {
		t.Fatalf("rejection: %v %+v", err, out)
	}
	srv.LoseAcknowledgments(1)
	if _, err := a.Apply(ctx, tA, op("lost", 1)); !errors.Is(err, erp.ErrUnreachable) {
		t.Fatalf("a dropped connection is an unknown outcome: %v", err)
	}
	if e(srv, tA, "lost").Applied != 1 {
		t.Fatal("the lost acknowledgment follows a real effect")
	}
	// Lookup establishes what the lost response hid.
	if out, err := a.Lookup(ctx, tA, "lost"); err != nil || out.Status != exceptions.OutcomeAcknowledged {
		t.Fatalf("reconcile: %v %+v", err, out)
	}
	srv.SetApplyDown(true)
	srv.SetLookupDown(true)
	if _, err := a.Apply(ctx, tA, op("down", 1)); !errors.Is(err, erp.ErrUnreachable) {
		t.Fatalf("5xx is unknown: %v", err)
	}
	if _, err := a.Lookup(ctx, tA, "lost"); !errors.Is(err, erp.ErrUnreachable) {
		t.Fatalf("unavailable lookup is unknown, not 'not found': %v", err)
	}
	// A wrong credential is a configuration error, never an outcome.
	bad, _ := erp.NewHTTPAdapter(erp.Config{BaseURL: srv.URL(), Credentials: creds{tA: "wrong"}})
	srv.SetApplyDown(false)
	if _, err := bad.Apply(ctx, tA, op("x", 1)); err == nil || errors.Is(err, erp.ErrUnreachable) {
		t.Fatalf("credential refusal: %v", err)
	}
	if _, ok := srv.Effect(string(tA), "x"); ok {
		t.Fatal("a refused credential must not cause an effect")
	}
}

func TestAdapterConfigValidation(t *testing.T) {
	for name, cfg := range map[string]erp.Config{
		"empty":       {},
		"no creds":    {BaseURL: "https://erp.example.invalid"},
		"insecure":    {BaseURL: "http://erp.example.invalid", Credentials: creds{}},
		"credentials": {BaseURL: "https://u:p@erp.example.invalid", Credentials: creds{}},
	} {
		if _, err := erp.NewHTTPAdapter(cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
