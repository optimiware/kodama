package agentstest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/domain"
)

// Scopes are the three projects a repository must be able to address: two
// projects of one tenant and a project of another. The memory repository
// addresses any scope; a database-backed one needs these projects to exist
// before the suite starts.
type Scopes struct {
	// A and A2 are two projects of one tenant; B belongs to another tenant.
	A, A2, B domain.Scope
}

// MemScopes are the scopes the memory repository's own contract run uses.
var MemScopes = Scopes{
	A:  domain.Scope{TenantID: "t030_acme", ProjectID: "p030_inventory"},
	A2: domain.Scope{TenantID: "t030_acme", ProjectID: "p030_billing"},
	B:  domain.Scope{TenantID: "t030_globex", ProjectID: "p030_ops"},
}

// Contract is the one exported suite of the agents.Repository port (SD-13,
// SD-A11). The memory repository and the PostgreSQL store both run it. It
// asserts content-addressed registration, per-name versioning, immutability of
// stored versions, scope isolation, ordered paging and convergence of racing
// identical registrations. It does not assert locking, lock order or isolation
// levels.
//
// open returns a fresh, empty repository for the test it is given.
func Contract(t *testing.T, open func(testing.TB) agents.Repository, s Scopes) {
	t.Helper()
	ctx := context.Background()
	actor := agents.Actor{ID: "u030_contract", Type: agents.ActorUser}
	base := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

	var seq int
	var mu sync.Mutex
	candidate := func(tb testing.TB, scope domain.Scope, d agents.Draft) agents.Candidate {
		tb.Helper()
		mu.Lock()
		seq++
		n := seq
		mu.Unlock()
		return agents.Candidate{
			Scope: scope, ID: agents.ProfileID(fmt.Sprintf("apc_contract_%04d", n)), Content: Content(tb, d), CreatedBy: actor,
			CreatedAt: base.Add(time.Duration(n) * time.Second), RequestID: fmt.Sprintf("req_contract_%04d", n),
		}
	}
	register := func(tb testing.TB, r agents.Repository, scope domain.Scope, d agents.Draft) (agents.ProfileVersion, bool) {
		tb.Helper()
		p, created, err := r.RegisterOrGet(ctx, candidate(tb, scope, d))
		if err != nil {
			tb.Fatalf("register: %v", err)
		}
		if err := p.Validate(); err != nil {
			tb.Fatalf("the stored version is inconsistent: %v", err)
		}
		return p, created
	}
	count := func(tb testing.TB, r agents.Repository, scope domain.Scope, name string) int {
		tb.Helper()
		rows, err := r.List(ctx, scope, agents.ListQuery{Name: name, Limit: 1000})
		if err != nil {
			tb.Fatalf("list: %v", err)
		}
		return len(rows)
	}

	t.Run("a new name starts at version one and identical content resolves to it", func(t *testing.T) {
		r := open(t)
		d := Draft(t, "coding")
		p, created := register(t, r, s.A, d)
		if !created || p.Version != 1 || p.Scope != s.A || p.Name != "coding" || p.Ref() != "coding@1" {
			t.Fatalf("first registration: created=%v %+v", created, p)
		}
		again, created := register(t, r, s.A, d)
		if created || again.ID != p.ID || again.Version != 1 || again.Digest != p.Digest || !again.CreatedAt.Equal(p.CreatedAt) {
			t.Fatalf("identical content must resolve to the existing version without writing: created=%v %+v", created, again)
		}
		if n := count(t, r, s.A, ""); n != 1 {
			t.Fatalf("%d versions after an identical registration", n)
		}
	})

	t.Run("changed content is the next version of its name and never alters the earlier one", func(t *testing.T) {
		r := open(t)
		d1 := Draft(t, "coding")
		v1, _ := register(t, r, s.A, d1)
		d2 := d1
		d2.Instructions += "\nAlso write tests first."
		v2, created := register(t, r, s.A, d2)
		if !created || v2.Version != 2 || v2.Digest == v1.Digest || v2.ID == v1.ID {
			t.Fatalf("second version: created=%v %+v", created, v2)
		}
		other := Draft(t, "review")
		o, created := register(t, r, s.A, other)
		if !created || o.Version != 1 {
			t.Fatalf("another name has its own version counter: created=%v %+v", created, o)
		}
		got, err := r.Get(ctx, s.A, "coding", 1)
		if err != nil {
			t.Fatal(err)
		}
		if got.Digest != v1.Digest || got.ID != v1.ID || string(got.Content.Canonical()) != string(v1.Content.Canonical()) ||
			got.Content.Draft().Instructions != d1.Instructions {
			t.Fatal("a stored version must read back exactly as registered, whatever was registered after it")
		}
		// Reverting to the first content is the existing version again, not a third.
		back, created := register(t, r, s.A, d1)
		if created || back.Version != 1 || back.ID != v1.ID {
			t.Fatalf("the digest identifies content: created=%v %+v", created, back)
		}
		if n := count(t, r, s.A, "coding"); n != 2 {
			t.Fatalf("%d versions of coding", n)
		}
	})

	t.Run("a version reads back with its own content and attribution", func(t *testing.T) {
		r := open(t)
		d := Draft(t, "coding")
		p, _ := register(t, r, s.A, d)
		got, err := r.Get(ctx, s.A, "coding", p.Version)
		if err != nil {
			t.Fatal(err)
		}
		if err := got.Validate(); err != nil {
			t.Fatal(err)
		}
		if got.CreatedBy != actor || got.Digest != p.Digest || string(got.Content.Canonical()) != string(Content(t, d).Canonical()) {
			t.Fatalf("round trip: %+v", got)
		}
		if _, err := r.Get(ctx, s.A, "coding", 2); !errors.Is(err, agents.ErrNotFound) {
			t.Fatalf("an unregistered version: %v", err)
		}
		if _, err := r.Get(ctx, s.A, "absent", 1); !errors.Is(err, agents.ErrNotFound) {
			t.Fatalf("an unregistered name: %v", err)
		}
	})

	t.Run("projects and tenants are isolated", func(t *testing.T) {
		r := open(t)
		d := Draft(t, "coding")
		a, _ := register(t, r, s.A, d)
		a2, created := register(t, r, s.A2, d)
		if !created || a2.Version != 1 || a2.ID == a.ID || a2.Scope != s.A2 {
			t.Fatalf("identical content in another project is its own version: created=%v %+v", created, a2)
		}
		b, created := register(t, r, s.B, d)
		if !created || b.Version != 1 || b.ID == a.ID || b.Scope != s.B {
			t.Fatalf("identical content in another tenant is its own version: created=%v %+v", created, b)
		}
		changed := d
		changed.Instructions += " v2"
		if p, _ := register(t, r, s.A, changed); p.Version != 2 {
			t.Fatalf("version %d", p.Version)
		}
		if p, _ := register(t, r, s.B, d); p.Version != 1 {
			t.Fatal("another scope's registration must not advance this scope's counter")
		}
		if n := count(t, r, s.A2, ""); n != 1 {
			t.Fatalf("project A2 sees %d versions", n)
		}
		if n := count(t, r, s.B, ""); n != 1 {
			t.Fatalf("tenant B sees %d versions", n)
		}
		if _, err := r.Get(ctx, s.A2, "coding", 2); !errors.Is(err, agents.ErrNotFound) {
			t.Fatalf("a version of another project must be indistinguishable from an absent one: %v", err)
		}
		only := Draft(t, "only-a")
		register(t, r, s.A, only)
		for _, foreign := range []domain.Scope{s.A2, s.B} {
			if _, err := r.Get(ctx, foreign, "only-a", 1); !errors.Is(err, agents.ErrNotFound) {
				t.Fatalf("scope %v reads a foreign profile: %v", foreign, err)
			}
		}
	})

	t.Run("lists are ordered by creation, filterable and resumable", func(t *testing.T) {
		r := open(t)
		var order []string
		for _, name := range []string{"alpha", "bravo", "charlie"} {
			p, _ := register(t, r, s.A, Draft(t, name))
			order = append(order, p.Ref())
		}
		revised := Draft(t, "alpha")
		revised.Instructions += " revised"
		p, _ := register(t, r, s.A, revised)
		order = append(order, p.Ref())

		var all []agents.ProfileVersion
		var after *agents.Position
		for pages := 0; ; pages++ {
			if pages > 10 {
				t.Fatal("paging does not terminate")
			}
			page, err := r.List(ctx, s.A, agents.ListQuery{After: after, Limit: 3})
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, page...)
			if len(page) < 3 {
				break
			}
			last := page[len(page)-1]
			after = &agents.Position{CreatedAt: last.CreatedAt, ID: last.ID}
		}
		if len(all) != len(order) {
			t.Fatalf("paging returned %d of %d versions", len(all), len(order))
		}
		for i, p := range all {
			if p.Ref() != order[i] {
				t.Fatalf("position %d is %s, want %s (creation order)", i, p.Ref(), order[i])
			}
		}
		named, err := r.List(ctx, s.A, agents.ListQuery{Name: "alpha", Limit: 10})
		if err != nil || len(named) != 2 || named[0].Ref() != "alpha@1" || named[1].Ref() != "alpha@2" {
			t.Fatalf("name filter: %v %v", named, err)
		}
		if rows, err := r.List(ctx, s.B, agents.ListQuery{Limit: 10}); err != nil || len(rows) != 0 {
			t.Fatalf("an empty scope lists nothing: %v %v", rows, err)
		}
		if rows, err := r.List(ctx, s.A, agents.ListQuery{Limit: 2}); err != nil || len(rows) != 2 {
			t.Fatalf("limit: %d %v", len(rows), err)
		}
	})

	t.Run("racing identical registrations converge on one version", func(t *testing.T) {
		r := open(t)
		d := Draft(t, "coding")
		const racers = 8
		type result struct {
			p       agents.ProfileVersion
			created bool
			err     error
		}
		out := make(chan result, racers)
		var wg sync.WaitGroup
		for i := 0; i < racers; i++ {
			c := candidate(t, s.A, d)
			wg.Add(1)
			go func() {
				defer wg.Done()
				// A busy store may ask for a retry; content addressing makes the
				// retry converge.
				for attempt := 0; attempt < 20; attempt++ {
					p, created, err := r.RegisterOrGet(ctx, c)
					if errors.Is(err, agents.ErrUnavailable) {
						time.Sleep(10 * time.Millisecond)
						continue
					}
					out <- result{p, created, err}
					return
				}
				out <- result{err: errors.New("registration stayed unavailable")}
			}()
		}
		wg.Wait()
		close(out)
		created, ids := 0, map[agents.ProfileID]bool{}
		for res := range out {
			if res.err != nil {
				t.Fatal(res.err)
			}
			if res.created {
				created++
			}
			if res.p.Version != 1 {
				t.Fatalf("version %d", res.p.Version)
			}
			ids[res.p.ID] = true
		}
		if created != 1 || len(ids) != 1 {
			t.Fatalf("created %d times with %d identities", created, len(ids))
		}
		if n := count(t, r, s.A, ""); n != 1 {
			t.Fatalf("%d versions", n)
		}
	})
}
