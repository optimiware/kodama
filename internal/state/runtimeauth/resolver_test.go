package runtimeauth

import (
	"context"
	"net/http"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/state"
)

type fakeAuthority struct {
	req runtimeauth.AuthorizeRequest
}

func (f *fakeAuthority) Authorize(_ context.Context, req runtimeauth.AuthorizeRequest) (runtimeauth.Authorization, error) {
	f.req = req
	return runtimeauth.Authorization{
		Scope: domain.Scope{TenantID: "t_1", ProjectID: "p_1"},
		StateBindings: []runtimev1.StateBinding{{
			Name: "release", Namespace: "deploy", Prefix: "website/", Write: true,
		}},
	}, nil
}

func TestResolverUsesSharedWireAndDeclaredBinding(t *testing.T) {
	f := &fakeAuthority{}
	r := Resolver{Authority: f}
	req, _ := http.NewRequest(http.MethodPut, "http://runtime.example/runtime/v3/attempts/att_1/state/release/entries?key=resource", nil)
	req.Header.Set("Authorization", "Bearer token")
	scope, binding, err := r.ResolveBinding(context.Background(), req, "att_1", "release")
	if err != nil {
		t.Fatal(err)
	}
	if f.req.ContextVersion != runtimeauth.WireV2 {
		t.Fatalf("context version = %d, want WireV2", f.req.ContextVersion)
	}
	if f.req.Action != runtimeauth.ActionStateWrite || scope.TenantID != "t_1" || scope.ProjectID != "p_1" ||
		binding.Namespace != "deploy" || binding.Prefix != "website/" || !binding.Write {
		t.Fatalf("resolved %+v %+v request=%+v", scope, binding, f.req)
	}
}

func TestResolverFailsClosedForUndeclaredBinding(t *testing.T) {
	r := Resolver{Authority: &fakeAuthority{}}
	req, _ := http.NewRequest(http.MethodGet, "http://runtime.example/runtime/v3/attempts/att_1/state/other/entries?key=resource", nil)
	req.Header.Set("Authorization", "Bearer token")
	if _, _, err := r.ResolveBinding(context.Background(), req, "att_1", "other"); err != state.ErrDenied {
		t.Fatalf("err = %v, want denied", err)
	}
}
