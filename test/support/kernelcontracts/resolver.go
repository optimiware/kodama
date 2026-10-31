package kernelcontracts

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// ResolverSubject is the runtime.SecretResolver under test, with the two hooks
// the suite needs and the port does not offer.
type ResolverSubject interface {
	runtime.SecretResolver
	// Material returns one entry per piece of private material currently
	// published (readable by a workload). It is what must be empty after a
	// revocation.
	Material() []string
	// HoldNextResolve arms the subject so that the next Resolve blocks after it
	// prepared its material and before it publishes it, until release is called.
	// entered is closed when that Resolve reached the blocking point.
	HoldNextResolve() (entered <-chan struct{}, release func())
}

// ResolverFactory builds a fresh, empty ResolverSubject.
type ResolverFactory func(t TB) ResolverSubject

// ResolverCase is one named behaviour of the SecretResolver port.
type ResolverCase struct {
	Name string
	Run  func(t TB, r ResolverSubject)
}

var resolverOwner = domain.Owner{TenantID: "t1", ProjectID: "p1"}

func resolverRef(t TB, scope domain.Scope, step domain.StepID, attempt int) runtime.MaterializationRef {
	t.Helper()
	ref, err := runtime.NewMaterializationRef(scope, run1, step, attempt)
	if err != nil {
		t.Fatalf("name the material: %v", err)
	}
	return ref
}

func resolveReq(ref runtime.MaterializationRef) runtime.ResolveRequest {
	return runtime.ResolveRequest{Ref: ref, Refs: []runtime.SecretRef{{Owner: resolverOwner, Name: "TOKEN"}}}
}

func rexpect(t TB, cond bool, format string, args ...any) {
	t.Helper()
	if !cond {
		t.Fatalf(format, args...)
	}
}

func rok(t TB, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// ResolverCases returns every resolver case in a stable order.
func ResolverCases() []ResolverCase {
	return []ResolverCase{
		{"resolver/resolve_publishes_and_revoke_removes", func(t TB, r ResolverSubject) {
			ctx := context.Background()
			a, b := resolverRef(t, scopeA, "build", 1), resolverRef(t, scopeA, "deploy", 1)
			m, err := r.Resolve(ctx, resolveReq(a))
			rok(t, err, "resolve")
			rexpect(t, m.ID == a.ID && m.Attempt == 1 && m.Scope == scopeA && m.RunID == run1 && m.StepKey == "build" && m.Directory != "",
				"the materialization carries its identity: %+v", m)
			rexpect(t, len(r.Material()) == 1, "one material is published: %v", r.Material())
			_, err = r.Resolve(ctx, resolveReq(a))
			rok(t, err, "resolve the same identity again")
			rexpect(t, len(r.Material()) == 1, "a repeat replaces, it never adds a second copy: %v", r.Material())
			_, err = r.Resolve(ctx, resolveReq(b))
			rok(t, err, "resolve another identity")
			rexpect(t, len(r.Material()) == 2, "two identities, two materials: %v", r.Material())

			rok(t, r.Revoke(ctx, a), "revoke")
			rexpect(t, len(r.Material()) == 1, "only the revoked identity's material is removed: %v", r.Material())
			rok(t, r.Revoke(ctx, a), "revoke again")
			rok(t, r.Revoke(ctx, resolverRef(t, scopeA, "never", 7)), "revoke an identity that names nothing")
			rexpect(t, len(r.Material()) == 1, "a repeat and a revoke of nothing change nothing: %v", r.Material())
			rok(t, r.Revoke(ctx, b), "revoke the other")
			rexpect(t, len(r.Material()) == 0, "nothing is left: %v", r.Material())
		}},
		{"resolver/revoked_identity_cannot_be_resolved_again", func(t TB, r ResolverSubject) {
			ctx := context.Background()
			// Revoked before it was ever resolved: the identity is closed.
			early := resolverRef(t, scopeA, "build", 1)
			rok(t, r.Revoke(ctx, early), "revoke an identity that was never resolved")
			_, err := r.Resolve(ctx, resolveReq(early))
			rexpect(t, errors.Is(err, runtime.ErrMaterializationRevoked), "a later Resolve of an identity revoked while empty: %v", err)
			rexpect(t, len(r.Material()) == 0, "nothing was published: %v", r.Material())

			// Revoked after it was resolved: the same.
			used := resolverRef(t, scopeA, "deploy", 1)
			_, err = r.Resolve(ctx, resolveReq(used))
			rok(t, err, "resolve")
			rok(t, r.Revoke(ctx, used), "revoke")
			_, err = r.Resolve(ctx, resolveReq(used))
			rexpect(t, errors.Is(err, runtime.ErrMaterializationRevoked), "a later Resolve of a revoked identity: %v", err)
			rexpect(t, len(r.Material()) == 0, "nothing was published: %v", r.Material())
			rok(t, r.Revoke(ctx, used), "revoke the closed identity again")

			// The identity is closed, not the step: another attempt resolves.
			next := resolverRef(t, scopeA, "deploy", 2)
			_, err = r.Resolve(ctx, resolveReq(next))
			rok(t, err, "resolve the next attempt")
			rexpect(t, len(r.Material()) == 1, "the next attempt has its own material: %v", r.Material())
		}},
		{"resolver/in_flight_resolve_is_refused_at_publication", func(t TB, r ResolverSubject) {
			ctx := context.Background()
			ref := resolverRef(t, scopeA, "build", 1)
			entered, release := r.HoldNextResolve()
			done := make(chan error, 1)
			go func() {
				_, err := r.Resolve(ctx, resolveReq(ref))
				done <- err
			}()
			<-entered
			// The identity is revoked while Resolve is still preparing: it found nothing to remove.
			rok(t, r.Revoke(ctx, ref), "revoke while a Resolve is in flight")
			rexpect(t, len(r.Material()) == 0, "nothing is published yet: %v", r.Material())
			release()
			err := <-done
			rexpect(t, errors.Is(err, runtime.ErrMaterializationRevoked), "the in-flight Resolve is refused when it would publish: %v", err)
			rexpect(t, len(r.Material()) == 0, "it left no material behind: %v", r.Material())
			rok(t, r.Revoke(ctx, ref), "revoke again")
			rexpect(t, len(r.Material()) == 0, "and none appears later: %v", r.Material())
		}},
		{"resolver/refs_are_validated_and_scoped", func(t TB, r ResolverSubject) {
			ctx := context.Background()
			ref := resolverRef(t, scopeA, "build", 1)
			forged := ref
			forged.ID = resolverRef(t, scopeB, "build", 1).ID
			_, err := r.Resolve(ctx, resolveReq(forged))
			rexpect(t, errors.Is(err, domain.ErrInvalidValue), "Resolve of a forged identity: %v", err)
			rexpect(t, errors.Is(r.Revoke(ctx, forged), domain.ErrInvalidValue), "Revoke of a forged identity must be refused")
			rexpect(t, len(r.Material()) == 0, "nothing was published: %v", r.Material())

			// Another tenant's identity for the same run, step and attempt is another identity.
			other := resolverRef(t, scopeB, "build", 1)
			rok(t, r.Revoke(ctx, other), "revoke another tenant's identity")
			_, err = r.Resolve(ctx, resolveReq(ref))
			rok(t, err, "the revocation of another scope's identity does not close this one")
			rexpect(t, len(r.Material()) == 1, "the material is published: %v", r.Material())
			rok(t, r.Revoke(ctx, other), "revoke it again")
			rexpect(t, len(r.Material()) == 1, "and it is not removed: %v", r.Material())
		}},
		{"resolver/concurrent_resolves_and_revoke_leave_nothing", func(t TB, r ResolverSubject) {
			ctx := context.Background()
			for round := 1; round <= 20; round++ {
				ref := resolverRef(t, scopeA, "build", round)
				var wg sync.WaitGroup
				for range 6 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						_, _ = r.Resolve(ctx, resolveReq(ref))
					}()
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					_ = r.Revoke(ctx, ref)
				}()
				wg.Wait()
				// Every Resolve either published before the revocation (which removed it) or
				// was refused: whatever order they ran in, nothing is left.
				rok(t, r.Revoke(ctx, ref), fmt.Sprintf("revoke round %d", round))
				rexpect(t, len(r.Material()) == 0, "round %d left material behind: %v", round, r.Material())
			}
		}},
	}
}

// CheckResolver runs one resolver case against a fresh subject built by f.
func CheckResolver(t TB, f ResolverFactory, c ResolverCase) {
	t.Helper()
	c.Run(t, f(t))
}

// RunResolver runs every resolver case as a subtest. A SecretResolver adapter
// passes it to show that revocation is durable and closes the identity.
func RunResolver(t *testing.T, f ResolverFactory) {
	t.Helper()
	for _, c := range ResolverCases() {
		t.Run(c.Name, func(t *testing.T) { CheckResolver(t, f, c) })
	}
}
