package postgres_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/dependencies"
	"github.com/optimiware/kodama/internal/work/dependencies/depstest"
)

// Test_concurrent_cycle_insert: simultaneous reciprocal and longer-cycle-closing
// insertions cannot commit a cyclic graph, and a rejected write leaves no
// version, history, audit or outbox effect. Real PostgreSQL; no NATS, S3 or Git.
// WM-05, WM-A04, IT-11.
func Test_concurrent_cycle_insert(t *testing.T) {
	st := depstest.NewStack(t)

	t.Run("reciprocal_insertions", func(t *testing.T) {
		for round := 0; round < 8; round++ {
			pair := items(st, scopeA, 2, fmt.Sprintf("pair %d", round))
			a, b := pair[0], pair[1]
			before := [2]snapshot{snap(st, a), snap(st, b)}
			errs := parallel(
				func() error { _, err := add(st, a, b, a.Version); return err },
				func() error { _, err := add(st, b, a, b.Version); return err },
			)
			var ok, graph int
			rejected := -1
			for i, err := range errs {
				switch {
				case err == nil:
					ok++
				case is(err, dependencies.ErrGraph):
					graph++
					rejected = i
				default:
					t.Fatalf("round %d: unexpected error %v", round, err)
				}
			}
			if ok != 1 || graph != 1 {
				t.Fatalf("round %d: %d accepted, %d rejected as a cycle (%v)", round, ok, graph, errs)
			}
			assertAcyclic(t, st, tenantA)
			// The rejected dependent changed nothing; the accepted one changed exactly once.
			after := [2]snapshot{snap(st, a), snap(st, b)}
			if after[rejected] != before[rejected] {
				t.Fatalf("round %d: the rejected write left effects: %+v -> %+v", round, before[rejected], after[rejected])
			}
			won := 1 - rejected
			if want := (snapshot{before[won].version + 1, before[won].history + 1, before[won].audit + 1, before[won].outbox + 1}); after[won] != want {
				t.Fatalf("round %d: accepted write footprint %+v, want %+v", round, after[won], want)
			}
		}
	})

	t.Run("a_four_cycle_closed_by_four_simultaneous_insertions", func(t *testing.T) {
		for round := 0; round < 5; round++ {
			n := items(st, scopeA, 4, fmt.Sprintf("ring %d", round))
			before := make([]snapshot, 4)
			for i := range n {
				before[i] = snap(st, n[i])
			}
			fns := make([]func() error, 4)
			for i := range fns {
				i := i
				fns[i] = func() error { _, err := add(st, n[i], n[(i+1)%4], n[i].Version); return err }
			}
			errs := parallel(fns...)
			var ok, graph int
			for i, err := range errs {
				switch {
				case err == nil:
					ok++
				case is(err, dependencies.ErrGraph):
					graph++
					if after := snap(st, n[i]); after != before[i] {
						t.Fatalf("round %d: the rejected write of item %d left effects: %+v -> %+v", round, i, before[i], after)
					}
				default:
					t.Fatalf("round %d: %v", round, err)
				}
			}
			// Any three of the four edges form a path; the fourth always closes the ring.
			if ok != 3 || graph != 1 {
				t.Fatalf("round %d: %d accepted, %d rejected (%v)", round, ok, graph, errs)
			}
			assertAcyclic(t, st, tenantA)
		}
	})

	t.Run("a_long_path_cannot_be_closed_while_it_is_being_built", func(t *testing.T) {
		const length = 12
		n := items(st, scopeA, length, "path")
		// Build i -> i+1 concurrently with the edge that would close the path.
		fns := []func() error{func() error { _, err := add(st, n[length-1], n[0], n[length-1].Version); return err }}
		for i := 0; i < length-1; i++ {
			i := i
			fns = append(fns, func() error { _, err := add(st, n[i], n[i+1], n[i].Version); return err })
		}
		errs := parallel(fns...)
		ok := 0
		for _, err := range errs {
			if err == nil {
				ok++
			} else if !is(err, dependencies.ErrGraph) {
				t.Fatalf("%v", err)
			}
		}
		// All `length` edges can never commit together; at least the closing one or a path edge fails.
		if ok != length-1 {
			t.Fatalf("%d of %d edges committed, want %d (the ring is closed by exactly one failure)", ok, length, length-1)
		}
		assertAcyclic(t, st, tenantA)
	})
}

// Test_dependency_graph_cases: long paths, diamonds, duplicate insertions,
// removals, the size bound and unrelated projects behave correctly. Real
// PostgreSQL. WM-05.
func Test_dependency_graph_cases(t *testing.T) {
	st := depstest.NewStack(t)

	t.Run("long_path", func(t *testing.T) {
		const length = 30
		n := items(st, scopeA, length, "chain")
		for i := 0; i < length-1; i++ {
			st.MustLink(n[i], n[i+1])
		}
		if _, err := st.Link(n[length-1], n[0]); !is(err, dependencies.ErrGraph) {
			t.Fatalf("closing the path: %v", err)
		}
		if _, err := st.Link(n[length-1], n[length/2]); !is(err, dependencies.ErrGraph) {
			t.Fatalf("closing the path in the middle: %v", err)
		}
		// A shortcut that follows the direction is not a cycle.
		if _, err := st.Link(n[0], n[length-1]); err != nil {
			t.Fatalf("shortcut: %v", err)
		}
		assertAcyclic(t, st, tenantA)
	})

	t.Run("diamond", func(t *testing.T) {
		n := items(st, scopeA, 4, "diamond") // a depends on b and c, which both depend on d
		a, b, c, d := n[0], n[1], n[2], n[3]
		st.MustLink(a, b)
		st.MustLink(a, c)
		st.MustLink(b, d)
		st.MustLink(c, d) // the second path to d is not a cycle
		for name, edge := range map[string][2]work.Item{"d->a": {d, a}, "d->b": {d, b}, "d->c": {d, c}, "b->a": {b, a}, "c->a": {c, a}} {
			if _, err := st.Link(edge[0], edge[1]); !is(err, dependencies.ErrGraph) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if got := prerequisites(t, st, a); len(got) != 2 {
			t.Fatalf("a prerequisites = %v", got)
		}
		assertAcyclic(t, st, tenantA)
	})

	t.Run("self_link", func(t *testing.T) {
		a := st.Create(scopeA, work.KindStory, "self")
		before := snap(st, a)
		if _, err := st.Link(a, a); !is(err, dependencies.ErrGraph) {
			t.Fatalf("self link: %v", err)
		}
		if after := snap(st, a); after != before {
			t.Fatalf("self link left effects: %+v -> %+v", before, after)
		}
	})

	t.Run("duplicate_insertion_is_a_no_op", func(t *testing.T) {
		n := items(st, scopeA, 2, "dup")
		first := st.MustLink(n[0], n[1])
		if !first.Changed || first.Owner.Version != n[0].Version+1 {
			t.Fatalf("first: %+v", first)
		}
		before := snap(st, n[0])
		second, err := add(st, n[0], n[1], first.Owner.Version)
		if err != nil {
			t.Fatal(err)
		}
		if second.Changed || second.Owner.Version != first.Owner.Version {
			t.Fatalf("duplicate: %+v", second)
		}
		if after := snap(st, n[0]); after != before {
			t.Fatalf("duplicate wrote: %+v -> %+v", before, after)
		}
		if got := prerequisites(t, st, n[0]); len(got) != 1 {
			t.Fatalf("prerequisites = %v", got)
		}
	})

	t.Run("removal", func(t *testing.T) {
		n := items(st, scopeA, 3, "removal")
		a, b, c := n[0], n[1], n[2]
		st.MustLink(a, b)
		st.MustLink(b, c)
		if _, err := st.Link(c, a); !is(err, dependencies.ErrGraph) {
			t.Fatalf("cycle: %v", err)
		}
		out, err := st.Unlink(b, c) // break the chain: c -> a is now legal
		if err != nil || !out.Changed {
			t.Fatalf("remove: %+v %v", out, err)
		}
		if _, err := st.Link(c, a); err != nil {
			t.Fatalf("after the removal: %v", err)
		}
		// The new c -> a -> b chain makes b -> c a cycle again.
		if _, err := st.Link(b, c); !is(err, dependencies.ErrGraph) {
			t.Fatalf("reinsertion: %v", err)
		}
		// Removing an absent link is a no-op.
		before := snap(st, a)
		if out, err := st.Unlink(a, c); err != nil || out.Changed {
			t.Fatalf("absent removal: %+v %v", out, err)
		}
		if after := snap(st, a); after != before {
			t.Fatalf("absent removal wrote: %+v -> %+v", before, after)
		}
		assertAcyclic(t, st, tenantA)
	})

	t.Run("unrelated_projects_do_not_interact", func(t *testing.T) {
		a1 := items(st, scopeA, 2, "p1")
		a2 := items(st, scopeA2, 2, "p2")
		b := items(st, scopeB, 2, "tb")
		st.MustLink(a1[0], a1[1])
		// The mirrored edge in another project and tenant is independent and legal.
		st.MustLink(a2[1], a2[0])
		st.MustLink(b[1], b[0])
		if _, err := st.Link(a1[1], a1[0]); !is(err, dependencies.ErrGraph) {
			t.Fatalf("same project reciprocal: %v", err)
		}
		// Links never cross projects or tenants; the reference is simply not found.
		if _, err := st.Link(a1[0], a2[0]); !is(err, work.ErrNotFound) {
			t.Fatalf("cross project: %v", err)
		}
		if _, err := st.Link(a1[0], b[0]); !is(err, work.ErrNotFound) {
			t.Fatalf("cross tenant: %v", err)
		}
		if st.Links(tenantA) < 2 || st.Links("t_globex") != 1 {
			t.Fatalf("link counts: %d %d", st.Links(tenantA), st.Links("t_globex"))
		}
	})

	t.Run("the_size_bound", func(t *testing.T) {
		if testing.Short() {
			t.Skip("creates MaxPrerequisites+1 work items")
		}
		n := items(st, scopeA, dependencies.MaxPrerequisites+2, "wide")
		for i := 1; i <= dependencies.MaxPrerequisites; i++ {
			st.MustLink(n[0], n[i])
		}
		if _, err := st.Link(n[0], n[dependencies.MaxPrerequisites+1]); !is(err, dependencies.ErrGraph) {
			t.Fatalf("the %dth prerequisite: %v", dependencies.MaxPrerequisites+1, err)
		}
		// A duplicate at the bound is still an idempotent no-op, not a failure.
		if out, err := st.Link(n[0], n[1]); err != nil || out.Changed {
			t.Fatalf("duplicate at the bound: %+v %v", out, err)
		}
		_ = context.Background()
	})
}
