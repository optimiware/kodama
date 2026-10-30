package secrets_test

import (
	"context"
	"errors"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/secrets"
)

// Test_resource_secret_resolution covers VS-14: a run service resolves only the
// bindings its committed spec pins, under current authority, audited to the
// resource, and a failure at any step releases nothing.
func Test_resource_secret_resolution(t *testing.T) {
	ctx := context.Background()
	req := runtimev1.SecretResolveRequest{Ref: refERP, Version: 2}
	grant := secrets.ResourceGrant{
		Scope: scopeA, RunID: "run_1", Key: "service.db",
		Bindings: []runtimev1.SecretBinding{{Ref: refERP, Version: 2, Name: "erp-token", Delivery: runtimev1.DeliveryFile}},
	}
	build := func(t *testing.T) (*secrets.Resolver, *fakeSource) {
		src := &fakeSource{value: "sentinel-value"}
		r, err := secrets.NewResolver(&fakeAuthority{}, src)
		if err != nil {
			t.Fatal(err)
		}
		return r, src
	}
	always := func(g secrets.ResourceGrant) secrets.ResourceAuthority {
		return func(context.Context) (secrets.ResourceGrant, error) { return g, nil }
	}

	t.Run("a pinned binding resolves with ownership from the grant", func(t *testing.T) {
		r, src := build(t)
		resp, err := r.ResolveResource(ctx, always(grant), req, "req_1")
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Validate(); err != nil || resp.Name != "erp-token" {
			t.Fatalf("response %+v %v", resp, err)
		}
		if len(src.opens) != 1 || src.opens[0].Scope != scopeA || src.opens[0].RunID != "run_1" || src.opens[0].SecretName != "erp-token" {
			t.Fatalf("version request %+v", src.opens)
		}
		if src.finalized != 1 || src.discarded != 1 {
			t.Fatalf("finalized %d discarded %d", src.finalized, src.discarded)
		}
	})

	t.Run("anything outside the service's own pins is undeclared", func(t *testing.T) {
		r, src := build(t)
		for name, bad := range map[string]runtimev1.SecretResolveRequest{
			"other secret":  {Ref: "secret://project/other", Version: 2},
			"other version": {Ref: refERP, Version: 3},
		} {
			if _, err := r.ResolveResource(ctx, always(grant), bad, ""); !errors.Is(err, secrets.ErrUndeclared) {
				t.Errorf("%s: %v", name, err)
			}
		}
		empty := grant
		empty.Bindings = nil
		if _, err := r.ResolveResource(ctx, always(empty), req, ""); !errors.Is(err, secrets.ErrUndeclared) {
			t.Errorf("a service without pins: %v", err)
		}
		if len(src.opens) != 0 {
			t.Fatal("a value was read for an undeclared reference")
		}
	})

	t.Run("a grant that is not a service's fails closed", func(t *testing.T) {
		r, src := build(t)
		for _, key := range []string{"preparation", "workspace", "service.", ""} {
			g := grant
			g.Key = key
			if _, err := r.ResolveResource(ctx, always(g), req, ""); !errors.Is(err, secrets.ErrAuthorityMissing) {
				t.Errorf("key %q: %v", key, err)
			}
		}
		if _, err := r.ResolveResource(ctx, nil, req, ""); !errors.Is(err, secrets.ErrAuthorityMissing) {
			t.Errorf("no authority: %v", err)
		}
		if _, err := r.ResolveResource(ctx, func(context.Context) (secrets.ResourceGrant, error) {
			return secrets.ResourceGrant{}, errors.New("outage")
		}, req, ""); !errors.Is(err, secrets.ErrAuthorityMissing) {
			t.Errorf("an outage must read as missing authority: %v", err)
		}
		if _, err := r.ResolveResource(ctx, func(context.Context) (secrets.ResourceGrant, error) {
			return secrets.ResourceGrant{}, secrets.ErrAttemptInactive
		}, req, ""); !errors.Is(err, secrets.ErrAttemptInactive) {
			t.Errorf("a stopped resource: %v", err)
		}
		if len(src.opens) != 0 {
			t.Fatal("a value was read without authority")
		}
	})

	t.Run("authority revoked while decrypting releases nothing", func(t *testing.T) {
		r, src := build(t)
		calls := 0
		_, err := r.ResolveResource(ctx, func(context.Context) (secrets.ResourceGrant, error) {
			calls++
			if calls == 2 {
				return secrets.ResourceGrant{}, secrets.ErrAttemptInactive
			}
			return grant, nil
		}, req, "")
		if !errors.Is(err, secrets.ErrAttemptInactive) || src.finalized != 0 || src.discarded != 1 {
			t.Fatalf("err %v finalized %d discarded %d", err, src.finalized, src.discarded)
		}
	})

	t.Run("finalization failure releases nothing", func(t *testing.T) {
		r, src := build(t)
		src.finalize = secrets.ErrFinalizationUncertain
		if _, err := r.ResolveResource(ctx, always(grant), req, ""); !errors.Is(err, secrets.ErrFinalizationUncertain) {
			t.Fatal(err)
		}
	})

	t.Run("the audit subject is the resource, not an attempt", func(t *testing.T) {
		info := secrets.FinalizeInfo{RunID: "run_1", ResourceKey: "service.db"}
		if got := info.Subject(); got != "resource:run_1/service.db" {
			t.Fatalf("subject %q", got)
		}
		if got := (secrets.FinalizeInfo{AttemptID: "att_1", RunID: "run_1"}).Subject(); got != "att_1" {
			t.Fatalf("attempt subject %q", got)
		}
	})
}
