package postgres_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/identity/idtest"
)

func TestFirstLoginConcurrent(t *testing.T) {
	s := idtest.New(t, idtest.Options{Conns: 32})
	s.SeedProject(tenantA, projectA, 5)

	const n = 24
	ids := make([]identity.UserID, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ids[i], errs[i] = s.Store.MapPrincipal(ctx, identity.Principal{Issuer: "https://issuer.test", Subject: "first|login"})
		}()
	}
	close(start)
	wg.Wait()
	for i := range ids {
		if errs[i] != nil || ids[i] != ids[0] || ids[0] == "" {
			t.Fatalf("login %d: %q %v (first %q)", i, ids[i], errs[i], ids[0])
		}
	}
	if err := ids[0].Validate(); err != nil {
		t.Fatalf("user id: %v", err)
	}
	if got := s.Count(`SELECT count(*) FROM users WHERE issuer = 'https://issuer.test' AND subject = 'first|login'`); got != 1 {
		t.Fatalf("%d user rows", got)
	}

	t.Run("a login grants no membership", func(t *testing.T) {
		if got := s.Count(`SELECT count(*) FROM memberships WHERE user_id = $1`, string(ids[0])); got != 0 {
			t.Fatalf("%d memberships", got)
		}
		tenants, err := s.Store.Tenants(ctx, ids[0], "", 50)
		if err != nil || len(tenants) != 0 {
			t.Fatalf("%v %v", tenants, err)
		}
	})

	t.Run("the pair is exact: issuer and subject are not folded", func(t *testing.T) {
		seen := map[identity.UserID]bool{ids[0]: true}
		for _, p := range []identity.Principal{
			{Issuer: "https://issuer.test", Subject: "First|login"}, {Issuer: "https://issuer.test/", Subject: "first|login"},
			{Issuer: "https://other.test", Subject: "first|login"}, {Issuer: "https://issuer.test", Subject: "first|login "},
		} {
			id, err := s.Store.MapPrincipal(ctx, p)
			if err != nil || seen[id] {
				t.Fatalf("%+v collided: %q %v", p, id, err)
			}
			seen[id] = true
		}
	})

	t.Run("invalid principals are rejected before the database", func(t *testing.T) {
		for _, p := range []identity.Principal{{}, {Issuer: "i"}, {Subject: "s"}, {Issuer: "i", Subject: strings.Repeat("x", 513)}, {Issuer: "i\x00", Subject: "s"}} {
			if _, err := s.Store.MapPrincipal(ctx, p); !is(err, identity.ErrInvalid) {
				t.Errorf("%+v: %v", p, err)
			}
		}
	})

	t.Run("the runtime role cannot enumerate or write users directly", func(t *testing.T) {
		// Outside any tenant context the runtime role sees no user rows, and cannot insert.
		var n int
		if err := s.Runtime.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("global user enumeration: %d %v", n, err)
		}
		if _, err := s.Runtime.Exec(ctx, `INSERT INTO users (id, issuer, subject) VALUES ('u_direct', 'i', 's')`); err == nil {
			t.Fatal("runtime role inserted a user directly")
		}
		var id string
		if err := s.Runtime.QueryRow(ctx, `SELECT id FROM kodama_identity_user($1)`, string(ids[0])).Scan(&id); err != nil || id != string(ids[0]) {
			t.Fatalf("own record: %q %v", id, err)
		}
		if err := s.Runtime.QueryRow(ctx, `SELECT count(*) FROM kodama_identity_user($1)`, "u_nobody").Scan(&n); err != nil || n != 0 {
			t.Fatalf("unknown user: %d %v", n, err)
		}
	})

	t.Run("directory reads are principal-filtered", func(t *testing.T) {
		a := s.User("https://issuer.test", "a")
		b := s.User("https://issuer.test", "b")
		s.SeedTenant("t010_other")
		s.Bind(tenantA, a, "", identity.RoleViewer)
		s.Bind("t010_other", b, projectOrEmpty(), identity.RoleAdmin)
		ta := must(s.Store.Tenants(ctx, a, "", 50))
		tb := must(s.Store.Tenants(ctx, b, "", 50))
		if len(ta) != 1 || ta[0].ID != tenantA || len(ta[0].Roles) != 1 || ta[0].Roles[0] != identity.RoleViewer {
			t.Fatalf("a sees %+v", ta)
		}
		if len(tb) != 1 || tb[0].ID != "t010_other" {
			t.Fatalf("b sees %+v", tb)
		}
		u := must(s.Store.User(ctx, a))
		if u.ID != a || u.Issuer != "https://issuer.test" || u.Subject != "a" {
			t.Fatalf("%+v", u)
		}
		if _, err := s.Store.User(ctx, "u_missing"); !is(err, identity.ErrNotFound) {
			t.Fatalf("%v", err)
		}
	})

	t.Run("tenant discovery pages by ID and never repeats", func(t *testing.T) {
		u := s.User("https://issuer.test", "pager")
		for i := 0; i < 5; i++ {
			tid := fmt.Sprintf("t010_page%d", i)
			s.SeedTenant(domainTenant(tid))
			s.Bind(domainTenant(tid), u, "", identity.RoleViewer)
		}
		var seen []string
		after := ""
		for {
			page := must(s.Store.Tenants(ctx, u, after, 2))
			for _, tn := range page {
				seen = append(seen, string(tn.ID))
			}
			if len(page) < 2 {
				break
			}
			after = string(page[len(page)-1].ID)
		}
		if len(seen) != 5 || seen[0] != "t010_page0" || seen[4] != "t010_page4" {
			t.Fatalf("%v", seen)
		}
	})
}
