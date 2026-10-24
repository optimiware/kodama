package kube_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// Test_credential_recovery (PX-03, IT-06): crashes before and after registration,
// lost Secret and create responses and racing controllers never regenerate a
// registered credential and never extend its admission expiry.
func Test_credential_recovery(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	tenant := executiontest.ScopeA.TenantID
	fresh := func(script string) (*dispatchHandle, func()) {
		run, _ := s.submit(executiontest.ScopeA, stepSpec{Script: script, Outputs: []string{"v"}, Image: unpullable})
		in := s.claim(tenant)
		return &dispatchHandle{in: in, run: run}, func() {}
	}
	matches := func(t *testing.T, h *dispatchHandle) {
		t.Helper()
		want, ok := s.storedDigest(h.in)
		if !ok {
			t.Fatal("no stored credential")
		}
		got, _, _ := s.grantRow(h.in)
		if !bytes.Equal(got, want[:]) {
			t.Fatal("the registered credential is not the stored one")
		}
	}

	t.Run("secret create applied, response lost", func(t *testing.T) {
		h, _ := fresh(quick)
		s.cl.Lose = func(op string, n int) bool { return op == "create secret" && n == 1 }
		defer s.cl.reset()
		if _, err := s.runtime.Start(ctx, *h.in); err != nil {
			t.Fatal(err)
		}
		s.cl.reset()
		matches(t, h)
		if s.grants(h.in) != 1 || s.jobsNamed(h.in) != 1 {
			t.Fatalf("grants=%d jobs=%d", s.grants(h.in), s.jobsNamed(h.in))
		}
	})

	t.Run("crash before registration", func(t *testing.T) {
		h, _ := fresh(quick)
		reg := &faultRegistrar{inner: s.registry, Before: func(n int) error {
			if n == 1 {
				return runtimeauth.ErrUnavailable
			}
			return nil
		}}
		if _, err := s.newRuntimeWith(s.c.client, reg).Start(ctx, *h.in); !errors.Is(err, kube.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
		first, ok := s.storedDigest(h.in)
		if !ok || s.grants(h.in) != 0 {
			t.Fatalf("stored=%v grants=%d", ok, s.grants(h.in))
		}
		if _, err := s.newRuntime(s.c.client).Start(ctx, *h.in); err != nil {
			t.Fatal(err)
		}
		again, _ := s.storedDigest(h.in)
		if again != first {
			t.Fatal("the credential was regenerated after a crash before registration")
		}
		matches(t, h)
	})

	t.Run("crash after registration keeps the expiry", func(t *testing.T) {
		h, _ := fresh(quick)
		reg := &faultRegistrar{inner: s.registry, After: func(n int) error {
			if n == 1 {
				return runtimeauth.ErrUnavailable
			}
			return nil
		}}
		if _, err := s.newRuntimeWith(s.c.client, reg).Start(ctx, *h.in); !errors.Is(err, kube.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
		digest1, created1, expires1 := s.grantRow(h.in)
		first, _ := s.storedDigest(h.in)
		time.Sleep(1200 * time.Millisecond) // a new registration would carry a later expiry
		if _, err := s.newRuntime(s.c.client).Start(ctx, *h.in); err != nil {
			t.Fatal(err)
		}
		digest2, created2, expires2 := s.grantRow(h.in)
		if !bytes.Equal(digest1, digest2) || !created1.Equal(created2) || !expires1.Equal(expires2) {
			t.Fatalf("the registration changed: %v -> %v", expires1, expires2)
		}
		if again, _ := s.storedDigest(h.in); again != first {
			t.Fatal("the credential was regenerated")
		}
		if s.grants(h.in) != 1 {
			t.Fatalf("%d grants", s.grants(h.in))
		}
	})

	t.Run("racing controllers", func(t *testing.T) {
		h, _ := fresh(quick)
		var wg sync.WaitGroup
		for range 6 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = s.newRuntime(s.c.client).Start(ctx, *h.in)
			}()
		}
		wg.Wait()
		if _, err := s.newRuntime(s.c.client).Start(ctx, *h.in); err != nil {
			t.Fatal(err)
		}
		if s.grants(h.in) != 1 || s.jobsNamed(h.in) != 1 {
			t.Fatalf("grants=%d jobs=%d", s.grants(h.in), s.jobsNamed(h.in))
		}
		matches(t, h)
		_, created, expires := s.grantRow(h.in)
		if expires.Sub(created) != 10*time.Minute {
			t.Fatalf("admission window %v: a racing registration moved it", expires.Sub(created))
		}
	})

	t.Run("a lost secret is not regenerated for a registered attempt", func(t *testing.T) {
		h, _ := fresh(quick)
		reg := &faultRegistrar{inner: s.registry, After: func(n int) error {
			if n == 1 {
				return runtimeauth.ErrUnavailable
			}
			return nil
		}}
		_, _ = s.newRuntimeWith(s.c.client, reg).Start(ctx, *h.in)
		digest1, _, expires1 := s.grantRow(h.in)
		s.mustKubectl("-n", s.ns(h.in.Scope.TenantID), "delete", "secret", kube.SecretName(h.in.WorkloadName))
		_, err := s.newRuntime(s.c.client).Start(ctx, *h.in)
		if !errors.Is(err, kube.ErrCredentialConflict) {
			t.Fatalf("got %v", err)
		}
		digest2, _, expires2 := s.grantRow(h.in)
		if !bytes.Equal(digest1, digest2) || !expires1.Equal(expires2) {
			t.Fatal("the registration was replaced")
		}
		if s.jobsNamed(h.in) != 0 {
			t.Fatal("a Job was created with a credential that cannot be redeemed")
		}
	})
}

type dispatchHandle struct {
	in  *execution.AttemptSpec
	run *execution.Run
}
