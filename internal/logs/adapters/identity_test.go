package adapters_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/adapters"
)

type recordingAuthority struct {
	mu    sync.Mutex
	perms []identity.Permission
	deny  map[identity.Permission]error
}

func (r *recordingAuthority) Authorize(_ context.Context, a identity.Actor, _ identity.Target, p identity.Permission) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.perms = append(r.perms, p)
	return r.deny[p]
}

type bindings struct {
	list []identity.Binding
	err  error
}

func (b bindings) CurrentBindings(context.Context, domain.TenantID, identity.UserID) ([]identity.Binding, error) {
	return b.list, b.err
}

var scopeA = domain.Scope{TenantID: "t_a", ProjectID: "p_a"}

func projectBinding() identity.Binding {
	return identity.Binding{ID: "mem_1", Tenant: "t_a", Project: "p_a", User: "u_1", Role: identity.RoleViewer}
}

func TestAuthorityMapsDiagnosticAccess(t *testing.T) {
	t.Run("both existing permissions are required on every call", func(t *testing.T) {
		auth := &recordingAuthority{}
		a, err := adapters.NewAuthority(auth, bindings{list: []identity.Binding{projectBinding()}})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if err := a.AuthorizeRead(context.Background(), "u_1", scopeA); err != nil {
				t.Fatal(err)
			}
		}
		if len(auth.perms) != 4 || auth.perms[0] != identity.PermRunRead || auth.perms[1] != identity.PermEvidenceRead {
			t.Fatalf("permissions asked = %v (nothing may be cached)", auth.perms)
		}
	})
	t.Run("a missing evidence permission denies", func(t *testing.T) {
		auth := &recordingAuthority{deny: map[identity.Permission]error{identity.PermEvidenceRead: identity.ErrDenied}}
		a, _ := adapters.NewAuthority(auth, bindings{list: []identity.Binding{projectBinding()}})
		if err := a.AuthorizeRead(context.Background(), "u_1", scopeA); !errors.Is(err, logs.ErrDenied) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("no binding hides the project without asking the authority", func(t *testing.T) {
		auth := &recordingAuthority{}
		a, _ := adapters.NewAuthority(auth, bindings{})
		if err := a.AuthorizeRead(context.Background(), "u_1", scopeA); !errors.Is(err, logs.ErrNotFound) || len(auth.perms) != 0 {
			t.Fatalf("%v %v", err, auth.perms)
		}
		other := identity.Binding{ID: "mem_2", Tenant: "t_a", Project: "p_other", User: "u_1", Role: identity.RoleAdmin}
		a, _ = adapters.NewAuthority(auth, bindings{list: []identity.Binding{other}})
		if err := a.AuthorizeRead(context.Background(), "u_1", scopeA); !errors.Is(err, logs.ErrNotFound) {
			t.Fatalf("a binding to another project is no binding here: %v", err)
		}
	})
	t.Run("a hidden target is not found and an outage is unavailable", func(t *testing.T) {
		auth := &recordingAuthority{deny: map[identity.Permission]error{identity.PermRunRead: identity.ErrHidden}}
		a, _ := adapters.NewAuthority(auth, bindings{list: []identity.Binding{projectBinding()}})
		if err := a.AuthorizeRead(context.Background(), "u_1", scopeA); !errors.Is(err, logs.ErrNotFound) {
			t.Fatalf("%v", err)
		}
		auth = &recordingAuthority{deny: map[identity.Permission]error{identity.PermRunRead: errors.New("pq: dial 10.0.0.1 refused")}}
		a, _ = adapters.NewAuthority(auth, bindings{list: []identity.Binding{projectBinding()}})
		err := a.AuthorizeRead(context.Background(), "u_1", scopeA)
		if !errors.Is(err, logs.ErrUnavailable) || errors.Is(err, logs.ErrDenied) {
			t.Fatalf("an authority outage is never access nor a refusal: %v", err)
		}
		a, _ = adapters.NewAuthority(&recordingAuthority{}, bindings{err: errors.New("db down")})
		if err := a.AuthorizeRead(context.Background(), "u_1", scopeA); !errors.Is(err, logs.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("malformed principals and scopes are not found", func(t *testing.T) {
		a, _ := adapters.NewAuthority(&recordingAuthority{}, bindings{list: []identity.Binding{projectBinding()}})
		for _, p := range []string{"", "../x", "u 1"} {
			if err := a.AuthorizeRead(context.Background(), p, scopeA); !errors.Is(err, logs.ErrNotFound) {
				t.Errorf("principal %q: %v", p, err)
			}
		}
		if err := a.AuthorizeRead(context.Background(), "u_1", domain.Scope{}); !errors.Is(err, logs.ErrNotFound) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("constructors fail closed", func(t *testing.T) {
		if _, err := adapters.NewAuthority(nil, bindings{}); err == nil {
			t.Fatal("authority")
		}
		if _, err := adapters.NewAuthority(&recordingAuthority{}, nil); err == nil {
			t.Fatal("reader")
		}
	})
}

func TestWorkloadsNeverGuess(t *testing.T) {
	w, err := adapters.NewWorkloads(adapters.StaticPlacement{Namespace: "runs", Container: "step"})
	if err != nil {
		t.Fatal(err)
	}
	info := logs.AttemptInfo{Scope: scopeA, ID: "att_1", JobName: "kodama-att-1"}
	if _, err := w.Target(context.Background(), info); !errors.Is(err, logs.ErrWorkloadNotObserved) {
		t.Fatalf("an unobserved Job has no workload: %v", err)
	}
	info.JobUID = "11111111-2222-3333-4444-555555555555"
	tgt, err := w.Target(context.Background(), info)
	if err != nil || tgt != (logs.Target{Namespace: "runs", JobName: "kodama-att-1", JobUID: info.JobUID, Container: "step"}) {
		t.Fatalf("%+v %v", tgt, err)
	}
	for _, bad := range []adapters.StaticPlacement{{}, {Namespace: "Runs", Container: "step"}, {Namespace: "runs", Container: "a/b"}, {Namespace: "runs"}} {
		w, _ := adapters.NewWorkloads(bad)
		if _, err := w.Target(context.Background(), info); !errors.Is(err, logs.ErrNotConfigured) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	if _, err := adapters.NewWorkloads(nil); err == nil {
		t.Fatal("a placement resolver is required")
	}
	if _, err := adapters.NewAttempts(nil); err == nil {
		t.Fatal("a run reader is required")
	}
}
