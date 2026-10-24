package secrets_test

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets"
)

type fakeAuthority struct {
	grants map[domain.AttemptID]secrets.AttemptGrant
	errs   map[domain.AttemptID]error
	calls  int
	// onCall runs on every Grant, so a test can change authority between the
	// first and second check.
	onCall func(n int)
}

func (a *fakeAuthority) Grant(_ context.Context, id domain.AttemptID) (secrets.AttemptGrant, error) {
	a.calls++
	if a.onCall != nil {
		a.onCall(a.calls)
	}
	if err := a.errs[id]; err != nil {
		return secrets.AttemptGrant{}, err
	}
	g, ok := a.grants[id]
	if !ok {
		return secrets.AttemptGrant{}, secrets.ErrAuthorityMissing
	}
	return g, nil
}

type fakeSource struct {
	opens     []secrets.VersionRequest
	err       error
	finalize  error
	finalized int
	discarded int
	value     string
}

type fakePending struct {
	src   *fakeSource
	value []byte
}

func (s *fakeSource) Open(_ context.Context, req secrets.VersionRequest) (secrets.PendingValue, error) {
	s.opens = append(s.opens, req)
	if s.err != nil {
		return nil, s.err
	}
	return &fakePending{src: s, value: []byte(s.value)}, nil
}

func (p *fakePending) Value() []byte { return p.value }
func (p *fakePending) Finalize(context.Context, secrets.FinalizeInfo) error {
	p.src.finalized++
	return p.src.finalize
}
func (p *fakePending) Discard() { clear(p.value); p.src.discarded++ }

const refERP = "secret://project/erp-token"

func grantFor(attempt domain.AttemptID, scope domain.Scope) secrets.AttemptGrant {
	return secrets.AttemptGrant{
		Scope: scope, RunID: "run_1", AttemptID: attempt,
		Bindings: []runtimev1.SecretBinding{
			{Ref: refERP, Version: 2, Name: "erp-token", Delivery: runtimev1.DeliveryFile},
		},
	}
}

// Test_declared_attempt_authority covers VS-03, VS-A02: undeclared,
// cross-project/tenant, cancelled, expired and missing-authority requests fail
// closed, and nothing is read or released without current authority.
func Test_declared_attempt_authority(t *testing.T) {
	ctx := context.Background()
	req := runtimev1.SecretResolveRequest{Ref: refERP, Version: 2}

	setup := func() (*secrets.Resolver, *fakeAuthority, *fakeSource) {
		auth := &fakeAuthority{
			grants: map[domain.AttemptID]secrets.AttemptGrant{"att_1": grantFor("att_1", scopeA)},
			errs:   map[domain.AttemptID]error{},
		}
		src := &fakeSource{value: "sentinel-value"}
		r, err := secrets.NewResolver(auth, src)
		if err != nil {
			t.Fatal(err)
		}
		return r, auth, src
	}

	t.Run("declared reference resolves under the trusted scope", func(t *testing.T) {
		r, _, src := setup()
		resp, err := r.Resolve(ctx, "att_1", req, "req_1")
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Validate(); err != nil {
			t.Fatalf("response violates the runtime contract: %v", err)
		}
		raw, _ := base64.StdEncoding.DecodeString(resp.ValueBase64)
		if string(raw) != "sentinel-value" || resp.Name != "erp-token" || resp.Delivery != runtimev1.DeliveryFile {
			t.Fatalf("response %+v", resp)
		}
		// Ownership comes from the attempt record, not from the request.
		if len(src.opens) != 1 || src.opens[0].Scope != scopeA || src.opens[0].RunID != "run_1" || src.opens[0].SecretName != "erp-token" || src.opens[0].Version != 2 {
			t.Fatalf("version request %+v", src.opens)
		}
		if src.finalized != 1 || src.discarded != 1 {
			t.Fatalf("finalized %d discarded %d", src.finalized, src.discarded)
		}
	})

	t.Run("undeclared references and versions", func(t *testing.T) {
		r, _, src := setup()
		for name, bad := range map[string]runtimev1.SecretResolveRequest{
			"other secret":  {Ref: "secret://project/other", Version: 2},
			"other version": {Ref: refERP, Version: 3},
			"older version": {Ref: refERP, Version: 1},
		} {
			if _, err := r.Resolve(ctx, "att_1", bad, ""); !errors.Is(err, secrets.ErrUndeclared) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if len(src.opens) != 0 {
			t.Fatal("storage was read for an undeclared reference")
		}
	})

	t.Run("malformed requests", func(t *testing.T) {
		r, auth, _ := setup()
		if _, err := r.Resolve(ctx, "att_1", runtimev1.SecretResolveRequest{Ref: "erp-token", Version: 1}, ""); !errors.Is(err, secrets.ErrInvalid) {
			t.Errorf("malformed ref: %v", err)
		}
		if _, err := r.Resolve(ctx, "att_1", runtimev1.SecretResolveRequest{Ref: "secret://project/erp-token", Version: 0}, ""); !errors.Is(err, secrets.ErrInvalid) {
			t.Errorf("zero version: %v", err)
		}
		if _, err := r.Resolve(ctx, "bad id!", req, ""); !errors.Is(err, secrets.ErrInvalid) {
			t.Errorf("malformed attempt: %v", err)
		}
		if auth.calls != 0 {
			t.Error("authority consulted for a malformed request")
		}
	})

	t.Run("tenant-scoped references are never declared locally", func(t *testing.T) {
		r, _, src := setup()
		if _, err := r.Resolve(ctx, "att_1", runtimev1.SecretResolveRequest{Ref: "secret://tenant/erp-token", Version: 2}, ""); !errors.Is(err, secrets.ErrUndeclared) {
			t.Errorf("tenant reference: %v", err)
		}
		if len(src.opens) != 0 {
			t.Fatal("storage was read for a non-project reference")
		}
	})

	t.Run("missing, cancelled, expired and failing authority fail closed", func(t *testing.T) {
		r, auth, src := setup()
		auth.errs["att_cancelled"] = secrets.ErrAttemptInactive
		auth.errs["att_broken"] = errors.New("database down")
		for id, want := range map[domain.AttemptID]error{
			"att_unknown":   secrets.ErrAuthorityMissing,
			"att_cancelled": secrets.ErrAttemptInactive,
			"att_broken":    secrets.ErrAuthorityMissing,
		} {
			if _, err := r.Resolve(ctx, id, req, ""); !errors.Is(err, want) {
				t.Errorf("%s: %v, want %v", id, err, want)
			}
		}
		if len(src.opens) != 0 {
			t.Fatal("storage was read without authority")
		}
	})

	t.Run("authority answering for another attempt or an invalid grant", func(t *testing.T) {
		r, auth, src := setup()
		auth.grants["att_2"] = grantFor("att_1", scopeA) // wrong attempt in the record
		if _, err := r.Resolve(ctx, "att_2", req, ""); !errors.Is(err, secrets.ErrAuthorityMissing) {
			t.Errorf("mismatched grant: %v", err)
		}
		g := grantFor("att_3", domain.Scope{TenantID: "t_acme"}) // no project
		auth.grants["att_3"] = g
		if _, err := r.Resolve(ctx, "att_3", req, ""); !errors.Is(err, secrets.ErrAuthorityMissing) {
			t.Errorf("grant without a project: %v", err)
		}
		if len(src.opens) != 0 {
			t.Fatal("storage was read for an invalid grant")
		}
	})

	t.Run("cross-project and cross-tenant attempts only see their own scope", func(t *testing.T) {
		r, auth, src := setup()
		auth.grants["att_other_project"] = grantFor("att_other_project", domain.Scope{TenantID: "t_acme", ProjectID: "p_billing"})
		auth.grants["att_other_tenant"] = grantFor("att_other_tenant", domain.Scope{TenantID: "t_globex", ProjectID: "p_inventory"})
		for _, id := range []domain.AttemptID{"att_other_project", "att_other_tenant"} {
			if _, err := r.Resolve(ctx, id, req, ""); err != nil {
				t.Fatal(err)
			}
		}
		// The source was asked for each attempt's own scope, never scopeA.
		if len(src.opens) != 2 || src.opens[0].Scope == scopeA || src.opens[1].Scope == scopeA {
			t.Fatalf("version requests %+v", src.opens)
		}
		if src.opens[0].Scope == src.opens[1].Scope {
			t.Fatal("two scopes collapsed into one request")
		}
	})

	t.Run("cancellation during decryption releases nothing", func(t *testing.T) {
		r, auth, src := setup()
		auth.onCall = func(n int) {
			if n == 2 { // after Open, before Finalize
				auth.errs["att_1"] = secrets.ErrAttemptInactive
			}
		}
		if _, err := r.Resolve(ctx, "att_1", req, ""); !errors.Is(err, secrets.ErrAttemptInactive) {
			t.Fatalf("cancelled mid-flight: %v", err)
		}
		if src.finalized != 0 || src.discarded != 1 {
			t.Fatalf("finalized %d discarded %d", src.finalized, src.discarded)
		}
	})

	t.Run("a changed declaration during decryption releases nothing", func(t *testing.T) {
		r, auth, src := setup()
		auth.onCall = func(n int) {
			if n == 2 {
				g := grantFor("att_1", scopeA)
				g.Bindings = nil
				auth.grants["att_1"] = g
			}
		}
		if _, err := r.Resolve(ctx, "att_1", req, ""); !errors.Is(err, secrets.ErrUndeclared) {
			t.Fatalf("declaration removed mid-flight: %v", err)
		}
		if src.finalized != 0 {
			t.Fatal("finalized after the declaration disappeared")
		}
	})

	t.Run("source and finalization failures release nothing", func(t *testing.T) {
		for name, tc := range map[string]struct {
			open, fin, want error
		}{
			"revoked":            {open: secrets.ErrRevoked, want: secrets.ErrRevoked},
			"not pinned":         {open: secrets.ErrNotPinned, want: secrets.ErrNotPinned},
			"missing":            {open: secrets.ErrNotFound, want: secrets.ErrNotFound},
			"key unavailable":    {open: &secrets.KeyError{Class: secrets.KeyUnavailable, Op: "unwrap"}, want: ErrKeyUnavailableForTest},
			"audit failure":      {fin: errors.New("audit insert failed"), want: nil},
			"uncertain finalize": {fin: secrets.ErrFinalizationUncertain, want: secrets.ErrFinalizationUncertain},
		} {
			r, _, src := setup()
			src.err, src.finalize = tc.open, tc.fin
			resp, err := r.Resolve(ctx, "att_1", req, "")
			if err == nil {
				t.Errorf("%s: resolved", name)
				continue
			}
			if tc.want == ErrKeyUnavailableForTest {
				var ke *secrets.KeyError
				if !errors.As(err, &ke) || !ke.Retryable() {
					t.Errorf("%s: %v", name, err)
				}
			} else if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("%s: %v", name, err)
			}
			if resp.ValueBase64 != "" || resp.Ref != "" {
				t.Errorf("%s: a response accompanied the error", name)
			}
		}
	})

	t.Run("constructing a resolver requires both ports", func(t *testing.T) {
		if _, err := secrets.NewResolver(nil, &fakeSource{}); err == nil {
			t.Error("resolver without authority")
		}
		if _, err := secrets.NewResolver(&fakeAuthority{}, nil); err == nil {
			t.Error("resolver without a version source")
		}
	})
}

// ErrKeyUnavailableForTest is a marker for the table above.
var ErrKeyUnavailableForTest = errors.New("marker")
