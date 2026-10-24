package postgres_test

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/agents"
	agentspg "github.com/optimiware/kodama/internal/agents/postgres"
)

func TestConcurrentProfileRegistration(t *testing.T) {
	t.Run("identical concurrent requests yield one version and one audit row", func(t *testing.T) {
		e := newEnv(t)
		const n = 16
		results := make([]agents.RegisterResult, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i], errs[i] = e.register(scopeA, engineer, e.draft)
			}()
		}
		close(start)
		wg.Wait()
		created := 0
		var id agents.ProfileID
		for i := range results {
			if errs[i] != nil {
				t.Fatalf("request %d: %v", i, errs[i])
			}
			if results[i].Created {
				created++
			}
			if id == "" {
				id = results[i].Profile.ID
			}
			if results[i].Profile.ID != id || results[i].Profile.Version != 1 {
				t.Fatalf("every request must converge on one version: %+v", results[i].Profile)
			}
		}
		if created != 1 || e.profiles(scopeA) != 1 || e.audits(scopeA) != 1 {
			t.Fatalf("created=%d profiles=%d audits=%d", created, e.profiles(scopeA), e.audits(scopeA))
		}
	})

	t.Run("distinct content gets distinct monotonic versions", func(t *testing.T) {
		e := newEnv(t)
		const n = 12
		versions := make([]int64, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				r, err := e.register(scopeA, engineer, e.variant(i))
				versions[i], errs[i] = int64(r.Profile.Version), err
			}()
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("request %d: %v", i, err)
			}
		}
		sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
		for i, v := range versions {
			if v != int64(i+1) {
				t.Fatalf("versions must be gapless 1..%d, got %v", n, versions)
			}
		}
		if e.profiles(scopeA) != n || e.audits(scopeA) != n {
			t.Fatalf("profiles=%d audits=%d", e.profiles(scopeA), e.audits(scopeA))
		}
	})

	t.Run("projects allocate versions independently and do not block each other", func(t *testing.T) {
		e := newEnv(t)
		var wg sync.WaitGroup
		for _, s := range []struct {
			scope string
			n     int
		}{{projectA, 5}, {projectA2, 5}} {
			for i := 0; i < s.n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					scope := scopeA
					if s.scope == projectA2 {
						scope = scopeA2
					}
					if _, err := e.register(scope, engineer, e.variant(i)); err != nil {
						t.Error(err)
					}
				}()
			}
		}
		wg.Wait()
		for _, p := range []string{projectA, projectA2} {
			if got := e.count(`SELECT max(version) FROM agent_profiles WHERE project_id = $1 AND name = 'coding'`, p); got != 5 {
				t.Errorf("project %s max version = %d", p, got)
			}
		}
	})

	t.Run("a lock timeout is classified as unavailable and duplicates need no lock", func(t *testing.T) {
		e := newEnvWith(t, envOptions{store: agentspg.Options{LockTimeout: 250 * time.Millisecond}})
		existing := e.mustRegister(scopeA, engineer, e.draft)

		ctx := context.Background()
		holder, err := e.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		released := false
		release := func() {
			if !released {
				released = true
				_ = holder.Rollback(ctx)
			}
		}
		defer release()
		if _, err := holder.Exec(ctx, `SELECT 1 FROM projects WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantA, projectA); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		_, err = e.register(scopeA, engineer, e.variant(2))
		if !errors.Is(err, agents.ErrUnavailable) || errors.Is(err, agents.ErrOutcomeUnknown) {
			t.Fatalf("expected unavailable after the lock timeout, got %v", err)
		}
		if d := time.Since(started); d > 3*time.Second {
			t.Fatalf("lock wait was not bounded: %v", d)
		}
		dup, err := e.register(scopeA, engineer, e.draft)
		if err != nil || dup.Created || dup.Profile.ID != existing.Profile.ID {
			t.Fatalf("an existing profile is returned without the project lock: %+v %v", dup, err)
		}
		release()
		if res, err := e.register(scopeA, engineer, e.variant(2)); err != nil || !res.Created || res.Profile.Version != 2 {
			t.Fatalf("after the lock is released: %+v %v", res, err)
		}
	})
}
