package kernelcontracts_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	rt "github.com/optimiware/kodama/internal/kernel/runtime"
	kc "github.com/optimiware/kodama/test/support/kernelcontracts"
)

// flaws are the ways a resolver can break the revocation guarantee. The zero
// value is the reference resolver, which passes the suite.
type flaws struct {
	// noTombstone: Revoke removes material but does not close the identity.
	noTombstone bool
	// checkOnlyAtStart: Resolve consults the tombstone when it starts, not when it
	// publishes, so one in flight at the revocation still publishes.
	checkOnlyAtStart bool
	// keepsMaterial: Revoke closes the identity but leaves published material.
	keepsMaterial bool
	// duplicates: Resolve adds a second copy instead of replacing.
	duplicates bool
	// revokeFailsWhenEmpty: Revoke of an identity that names nothing is an error.
	revokeFailsWhenEmpty bool
	// acceptsForged: the identity is not validated.
	acceptsForged bool
	// tombstoneIsGlobal: revoking one identity closes every identity of the run.
	tombstoneIsGlobal bool
}

// memResolver is an in-memory SecretResolver whose faults are selectable.
type memResolver struct {
	f flaws

	mu        sync.Mutex
	published map[string]int
	closed    map[string]bool
	hold      chan struct{}
	entered   chan struct{}
	runClosed map[domain.RunID]bool
}

func newMemResolver(f flaws) *memResolver {
	return &memResolver{f: f, published: map[string]int{}, closed: map[string]bool{}, runClosed: map[domain.RunID]bool{}}
}

func (m *memResolver) isClosed(ref rt.MaterializationRef) bool {
	return m.closed[ref.ID] || (m.f.tombstoneIsGlobal && m.runClosed[ref.RunID])
}

func (m *memResolver) Resolve(_ context.Context, req rt.ResolveRequest) (rt.Materialization, error) {
	ref := req.Ref
	if !m.f.acceptsForged {
		if err := ref.Validate(); err != nil {
			return rt.Materialization{}, err
		}
	}
	m.mu.Lock()
	closedAtStart := m.isClosed(ref)
	hold, entered := m.hold, m.entered
	m.hold, m.entered = nil, nil
	m.mu.Unlock()
	if closedAtStart {
		return rt.Materialization{}, rt.ErrMaterializationRevoked
	}
	if hold != nil { // the material is prepared; wait before publishing it
		close(entered)
		<-hold
	}
	publish := func() {
		if m.f.duplicates {
			m.published[ref.ID]++
		} else {
			m.published[ref.ID] = 1
		}
	}
	m.mu.Lock()
	if m.isClosed(ref) && !m.f.checkOnlyAtStart {
		m.mu.Unlock()
		return rt.Materialization{}, rt.ErrMaterializationRevoked
	}
	publish()
	m.mu.Unlock()
	return rt.Materialization{Scope: ref.Scope, RunID: ref.RunID, StepKey: ref.StepKey, Attempt: ref.Attempt, ID: ref.ID,
		Directory: "/run/secrets/" + ref.ID, Refs: req.Refs}, nil
}

func (m *memResolver) Revoke(_ context.Context, ref rt.MaterializationRef) error {
	if !m.f.acceptsForged {
		if err := ref.Validate(); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.published[ref.ID] == 0 && m.f.revokeFailsWhenEmpty {
		return errors.New("nothing to revoke")
	}
	if !m.f.noTombstone {
		m.closed[ref.ID] = true
		m.runClosed[ref.RunID] = true
	}
	if !m.f.keepsMaterial {
		delete(m.published, ref.ID)
	}
	return nil
}

func (m *memResolver) Material() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for id, n := range m.published {
		for range n {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func (m *memResolver) HoldNextResolve() (<-chan struct{}, func()) {
	hold, entered := make(chan struct{}), make(chan struct{})
	m.mu.Lock()
	m.hold, m.entered = hold, entered
	m.mu.Unlock()
	return entered, func() { close(hold) }
}

func resolverFactory(f flaws) kc.ResolverFactory {
	return func(kc.TB) kc.ResolverSubject { return newMemResolver(f) }
}

func resolverFailures(f kc.ResolverFactory, c kc.ResolverCase) []string {
	r := &recorder{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				r.Errorf("panic: %v", p)
			}
		}()
		kc.CheckResolver(r, f, c)
	}()
	<-done
	return r.failed
}

// Test_unit_kernel_resolver_contract runs the shared resolver suite against the
// reference resolver.
func Test_unit_kernel_resolver_contract(t *testing.T) {
	kc.RunResolver(t, resolverFactory(flaws{}))
}

// Test_unit_kernel_resolver_contract_rejects_broken_resolvers proves the resolver
// suite cannot pass vacuously: each flaw fails the case that guards the guarantee
// it undoes.
func Test_unit_kernel_resolver_contract_rejects_broken_resolvers(t *testing.T) {
	cases := kc.ResolverCases()
	names := map[string]bool{}
	for _, c := range cases {
		names[c.Name] = true
	}
	for _, m := range []struct {
		name   string
		flaw   flaws
		mustBe string
	}{
		{"revocation does not close the identity", flaws{noTombstone: true}, "resolver/revoked_identity_cannot_be_resolved_again"},
		{"the tombstone is not checked at publication", flaws{checkOnlyAtStart: true}, "resolver/in_flight_resolve_is_refused_at_publication"},
		{"revocation leaves the material published", flaws{keepsMaterial: true}, "resolver/resolve_publishes_and_revoke_removes"},
		{"a repeat resolve adds a copy", flaws{duplicates: true}, "resolver/resolve_publishes_and_revoke_removes"},
		{"revoking nothing is an error", flaws{revokeFailsWhenEmpty: true}, "resolver/resolve_publishes_and_revoke_removes"},
		{"a forged identity is accepted", flaws{acceptsForged: true}, "resolver/refs_are_validated_and_scoped"},
		{"one identity's revocation closes the whole run", flaws{tombstoneIsGlobal: true}, "resolver/revoked_identity_cannot_be_resolved_again"},
	} {
		t.Run(m.name, func(t *testing.T) {
			if !names[m.mustBe] {
				t.Fatalf("no case named %q", m.mustBe)
			}
			var failed []string
			for _, c := range cases {
				if len(resolverFailures(resolverFactory(m.flaw), c)) > 0 {
					failed = append(failed, c.Name)
				}
			}
			if !slices.Contains(failed, m.mustBe) {
				t.Fatalf("the suite did not fail %q for %q; it failed %v", m.mustBe, m.name, failed)
			}
		})
	}
	if len(cases) < 5 {
		t.Fatalf("the resolver suite shrank to %d cases", len(cases))
	}
}
