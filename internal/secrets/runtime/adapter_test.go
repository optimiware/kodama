package secretsruntime_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets"
	secretsruntime "github.com/optimiware/kodama/internal/secrets/runtime"
)

type authority struct {
	grant secrets.AttemptGrant
	err   error
}

func (a authority) Grant(_ context.Context, id domain.AttemptID) (secrets.AttemptGrant, error) {
	if a.err != nil {
		return secrets.AttemptGrant{}, a.err
	}
	g := a.grant
	g.AttemptID = id
	return g, nil
}

type source struct {
	value []byte
	err   error
	fin   error
}

type pending struct {
	s *source
	v []byte
}

func (s source) Open(context.Context, secrets.VersionRequest) (secrets.PendingValue, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &pending{s: &s, v: append([]byte(nil), s.value...)}, nil
}
func (p *pending) Value() []byte                                        { return p.v }
func (p *pending) Finalize(context.Context, secrets.FinalizeInfo) error { return p.s.fin }
func (p *pending) Discard()                                             { clear(p.v) }

var grant = secrets.AttemptGrant{
	Scope: domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}, RunID: "run_1",
	Bindings: []runtimev1.SecretBinding{{Ref: "secret://project/erp-token", Version: 1, Name: "erp-token", Delivery: runtimev1.DeliveryFile}},
}

func adapter(t *testing.T, a authority, s source) *secretsruntime.Adapter {
	t.Helper()
	r, err := secrets.NewResolver(a, s)
	if err != nil {
		t.Fatal(err)
	}
	ad, err := secretsruntime.NewAdapter(r)
	if err != nil {
		t.Fatal(err)
	}
	return ad
}

func code(err error) domain.ErrorCode {
	var ce *runtimev1.ContractError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

// Test_runtime_translation checks the adapter against the executable runtime
// contract: the response is a valid SecretResolveResponse, and every error code
// the adapter can emit is one the contract allows for resolveSecret.
func Test_runtime_translation(t *testing.T) {
	ctx := context.Background()
	req := runtimev1.SecretResolveRequest{Ref: "secret://project/erp-token", Version: 1}

	ok, err := adapter(t, authority{grant: grant}, source{value: []byte("translated-SENTINEL")}).Resolve(ctx, "att_1", req, "req_1")
	if err != nil || ok.Validate() != nil || ok.Name != "erp-token" || ok.Delivery != runtimev1.DeliveryFile || ok.Version != 1 {
		t.Fatalf("response %+v err %v", ok, err)
	}
	bound := secretsruntime.Bound{Adapter: adapter(t, authority{grant: grant}, source{value: []byte("x")}), Attempt: "att_1", RequestID: "r"}
	if r, err := bound.ResolveSecret(ctx, req); err != nil || r.ValueBase64 == "" {
		t.Fatalf("bound resolve: %v", err)
	}

	var op runtimev1.Operation
	for _, o := range runtimev1.Operations {
		if o.ID == "resolveSecret" {
			op = o
		}
	}
	if op.ID == "" {
		t.Fatal("the runtime contract has no resolveSecret operation")
	}
	allowed := map[domain.ErrorCode]bool{}
	for _, c := range op.Errors {
		allowed[c] = true
	}

	cases := map[string]struct {
		a    authority
		s    source
		req  runtimev1.SecretResolveRequest
		want domain.ErrorCode
	}{
		"malformed request": {authority{grant: grant}, source{}, runtimev1.SecretResolveRequest{Ref: "erp-token", Version: 1}, domain.CodeInvalidRequest},
		"undeclared":        {authority{grant: grant}, source{}, runtimev1.SecretResolveRequest{Ref: "secret://project/other", Version: 1}, domain.CodeForbidden},
		"revoked":           {authority{grant: grant}, source{err: secrets.ErrRevoked}, req, domain.CodeForbidden},
		"missing":           {authority{grant: grant}, source{err: secrets.ErrNotFound}, req, domain.CodeNotFound},
		"not pinned":        {authority{grant: grant}, source{err: secrets.ErrNotPinned}, req, domain.CodeNotFound},
		"attempt inactive":  {authority{err: secrets.ErrAttemptInactive}, source{}, req, runtimev1.CodeAttemptRevoked},
		"no authority":      {authority{err: secrets.ErrAuthorityMissing}, source{}, req, domain.CodeInvalidCredential},
		"key outage":        {authority{grant: grant}, source{err: &secrets.KeyError{Class: secrets.KeyUnavailable}}, req, domain.CodeUnavailable},
		"key denied":        {authority{grant: grant}, source{err: &secrets.KeyError{Class: secrets.KeyDenied}}, req, domain.CodeUnavailable},
		"integrity failure": {authority{grant: grant}, source{err: secrets.ErrInconsistent}, req, domain.CodeUnavailable},
		"uncertain audit":   {authority{grant: grant}, source{fin: secrets.ErrFinalizationUncertain}, req, domain.CodeUnavailable},
		"unknown failure":   {authority{grant: grant}, source{err: fmt.Errorf("driver: password=hunter2")}, req, domain.CodeUnavailable},
		"authority broken":  {authority{err: errors.New("db down")}, source{}, req, domain.CodeInvalidCredential},
		"cancelled":         {authority{grant: grant}, source{err: context.Canceled}, req, domain.CodeUnavailable},
	}
	for name, tc := range cases {
		resp, err := adapter(t, tc.a, tc.s).Resolve(ctx, "att_1", tc.req, "r")
		got := code(err)
		if got != tc.want {
			t.Errorf("%s: code %q, want %q (%v)", name, got, tc.want, err)
			continue
		}
		if !allowed[got] {
			t.Errorf("%s: code %q is not allowed for resolveSecret", name, got)
		}
		if _, known := runtimev1.Status(got); !known {
			t.Errorf("%s: code %q has no status", name, got)
		}
		if resp.ValueBase64 != "" || resp.Ref != "" {
			t.Errorf("%s: a response accompanied the error", name)
		}
		if msg := err.Error(); strings.Contains(msg, "hunter2") || strings.Contains(msg, "erp-token") {
			t.Errorf("%s: the message leaks detail: %s", name, msg)
		}
	}
	if _, err := secretsruntime.NewAdapter(nil); err == nil {
		t.Error("adapter without a resolver")
	}
}
