package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets"
)

func createSecretErr(e *env, scope domain.Scope, name, value string) (secrets.Secret, error) {
	ctx := e.ctx()
	m := e.mutation("alice")
	p, err := e.svc.PrepareSecret(ctx, m, scope, name, secrets.KindGeneric, []byte(value))
	if err != nil {
		return secrets.Secret{}, err
	}
	var out secrets.Secret
	err = e.svc.Atomically(ctx, scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
		var err error
		out, err = e.svc.CreateSecret(ctx, w, m, p)
		return err
	})
	return out, err
}

// parallel runs n copies of fn released together.
func parallel(n int, fn func(i int)) {
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	ready.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			fn(i)
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()
}

// Test_concurrent_mutations: competing scope-key creation, value rotation,
// edits, rewrap and pin-versus-delete preserve uniqueness and expected-version
// semantics.
func Test_concurrent_mutations(t *testing.T) {
	t.Run("first scope key creation is serialized", func(t *testing.T) {
		e := newEnv(t)
		const n = 8
		errs := make([]error, n)
		created := make([]secrets.Secret, n)
		parallel(n, func(i int) {
			created[i], errs[i] = createSecretErr(e, scopeA1, fmt.Sprintf("secret-%d", i), fmt.Sprintf("value-%d", i))
		})
		for i, err := range errs {
			if err != nil {
				t.Fatalf("creator %d: %v", i, err)
			}
		}
		if k := e.count(`SELECT count(*) FROM secret_scope_keys WHERE tenant_id = 't_acme'`); k != 1 {
			t.Fatalf("%d scope keys for one tenant generation", k)
		}
		if w := e.count(`SELECT count(*) FROM secret_scope_key_wraps`); w != 1 {
			t.Fatalf("%d wraps", w)
		}
		if k := e.count(`SELECT count(DISTINCT scope_key_id) FROM secret_versions`); k != 1 {
			t.Fatalf("values sealed under %d different scope keys", k)
		}
		if a := e.count(`SELECT count(*) FROM audit WHERE action = 'secret_scope_key.create'`); a != 1 {
			t.Fatalf("%d scope key creations audited", a)
		}
		// Every loser's value decrypts under the committed key.
		auth := newAuthority()
		var bs []runtimeBinding
		for i := range created {
			name := fmt.Sprintf("secret-%d", i)
			if err := e.pin(scopeA1, "run_1", name, 1); err != nil {
				t.Fatal(err)
			}
			bs = append(bs, binding(name, 1))
		}
		auth.set(secrets.AttemptGrant{Scope: scopeA1, RunID: "run_1", AttemptID: "att_1", Bindings: bs})
		r := e.resolver(auth)
		for i := range created {
			resp, err := r.Resolve(e.ctx(), "att_1", ref(fmt.Sprintf("secret-%d", i), 1), "r")
			if err != nil || decode(t, resp) != fmt.Sprintf("value-%d", i) {
				t.Fatalf("secret-%d: %v", i, err)
			}
		}
	})

	t.Run("duplicate names are refused", func(t *testing.T) {
		e := newEnv(t)
		const n = 6
		errs := make([]error, n)
		parallel(n, func(i int) { _, errs[i] = createSecretErr(e, scopeA1, "same-name", fmt.Sprintf("v%d", i)) })
		wins := 0
		for _, err := range errs {
			switch {
			case err == nil:
				wins++
			case !errors.Is(err, secrets.ErrNameTaken):
				t.Fatalf("unexpected: %v", err)
			}
		}
		if wins != 1 || e.count(`SELECT count(*) FROM secrets`) != 1 || e.count(`SELECT count(*) FROM secret_versions`) != 1 {
			t.Fatalf("wins %d, secrets %d versions %d", wins, e.count(`SELECT count(*) FROM secrets`), e.count(`SELECT count(*) FROM secret_versions`))
		}
		// The same name is free in another project and another tenant.
		if _, err := createSecretErr(e, scopeA2, "same-name", "x"); err != nil {
			t.Fatal(err)
		}
		if _, err := createSecretErr(e, scopeB1, "same-name", "x"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("rotation under the same expected version has one winner", func(t *testing.T) {
		e := newEnv(t)
		sec := e.createSecret(scopeA1, "erp-token", "one")
		const n = 8
		errs := make([]error, n)
		parallel(n, func(i int) {
			ctx := e.ctx()
			m := e.mutation("alice")
			p, err := e.svc.PrepareVersion(ctx, m, scopeA1, sec.ID(), 1, []byte(fmt.Sprintf("rot-%d", i)))
			if err != nil {
				errs[i] = err
				return
			}
			errs[i] = e.svc.Atomically(ctx, tenantA, func(ctx context.Context, w secrets.Writer) error {
				_, err := e.svc.AddVersion(ctx, w, m, p)
				return err
			})
		})
		wins := 0
		for _, err := range errs {
			var c *secrets.ConflictError
			switch {
			case err == nil:
				wins++
			case errors.As(err, &c):
			default:
				t.Fatalf("unexpected: %v", err)
			}
		}
		cur, _ := e.store.GetSecret(e.ctx(), scopeA1, sec.ID())
		if wins != 1 || cur.CurrentVersion() != 2 || cur.Version() != 2 || e.count(`SELECT count(*) FROM secret_versions`) != 2 {
			t.Fatalf("wins %d current %d/%d versions %d", wins, cur.CurrentVersion(), cur.Version(), e.count(`SELECT count(*) FROM secret_versions`))
		}
	})

	t.Run("retrying rotations yield contiguous versions", func(t *testing.T) {
		e := newEnv(t)
		sec := e.createSecret(scopeA1, "erp-token", "v1")
		const workers, each = 5, 3
		var failed atomic.Int64
		parallel(workers, func(w int) {
			for k := 0; k < each; k++ {
				for attempt := 0; ; attempt++ {
					_, err := e.rotateErrQuiet(scopeA1, sec.ID(), fmt.Sprintf("w%d-%d", w, k))
					var c *secrets.ConflictError
					if err == nil {
						break
					}
					if !errors.As(err, &c) || attempt > 200 {
						failed.Add(1)
						return
					}
				}
			}
		})
		if failed.Load() != 0 {
			t.Fatalf("%d workers failed", failed.Load())
		}
		want := 1 + workers*each
		cur, _ := e.store.GetSecret(e.ctx(), scopeA1, sec.ID())
		if cur.CurrentVersion() != int64(want) || cur.Version() != domain.Version(want) {
			t.Fatalf("current %d resource %d, want %d", cur.CurrentVersion(), cur.Version(), want)
		}
		if n := e.count(`SELECT count(DISTINCT version) FROM secret_versions`); n != want {
			t.Fatalf("%d distinct versions, want %d", n, want)
		}
		if n := e.count(`SELECT count(*) FROM secret_versions WHERE version > $1 OR version < 1`, want); n != 0 {
			t.Fatal("version gap or overflow")
		}
	})

	t.Run("variable edits under one ETag have one winner", func(t *testing.T) {
		e := newEnv(t)
		v := e.createVariable(scopeA1, "region", "eu")
		const n = 8
		errs := make([]error, n)
		parallel(n, func(i int) {
			errs[i] = e.svc.Atomically(e.ctx(), tenantA, func(ctx context.Context, w secrets.Writer) error {
				val := strValue(fmt.Sprintf("zone-%d", i))
				_, err := e.svc.EditVariable(ctx, w, e.mutation("alice"), scopeA1, v.ID(), 1, secrets.VariableEdit{Value: &val})
				return err
			})
		})
		wins := 0
		for _, err := range errs {
			var c *secrets.ConflictError
			switch {
			case err == nil:
				wins++
			case errors.As(err, &c):
			default:
				t.Fatalf("unexpected: %v", err)
			}
		}
		cur, _ := e.store.GetVariable(e.ctx(), scopeA1, v.ID())
		if wins != 1 || cur.ValueVersion() != 2 || cur.Version() != 2 || e.count(`SELECT count(*) FROM variable_versions`) != 2 {
			t.Fatalf("wins %d value version %d version %d history %d", wins, cur.ValueVersion(), cur.Version(), e.count(`SELECT count(*) FROM variable_versions`))
		}
	})

	t.Run("rewrap races advance the wrapping revision one at a time", func(t *testing.T) {
		e := newEnv(t)
		e.createSecret(scopeA1, "erp-token", "one")
		e.keys.add("v2")
		e.wrapper.set(func(f *faultyWrapper) { f.inner = e.keys.provider("v2", "v1", "v2") })
		const n = 6
		errs := make([]error, n)
		parallel(n, func(i int) {
			_, errs[i] = e.svc.RewrapScopeKey(e.ctx(), e.mutation("ops"), tenantA, "")
		})
		wins := 0
		for _, err := range errs {
			var c *secrets.ConflictError
			switch {
			case err == nil:
				wins++
			case errors.As(err, &c):
			default:
				t.Fatalf("unexpected: %v", err)
			}
		}
		sk, _ := e.store.ActiveScopeKey(e.ctx(), tenantA)
		if wins < 1 || sk.Revision() != int64(1+wins) || e.count(`SELECT count(*) FROM secret_scope_key_wraps`) != 1+wins {
			t.Fatalf("wins %d revision %d wraps %d", wins, sk.Revision(), e.count(`SELECT count(*) FROM secret_scope_key_wraps`))
		}
		if n := e.count(`SELECT count(DISTINCT revision) FROM secret_scope_key_wraps`); n != 1+wins {
			t.Fatalf("revisions are not unique: %d", n)
		}
	})

	t.Run("pin registration and deletion are totally ordered", func(t *testing.T) {
		e := newEnv(t)
		pinFirst, deleteFirst := 0, 0
		for i := 0; i < 24; i++ {
			name := fmt.Sprintf("pd-%d", i)
			sec := e.createSecret(scopeA1, name, "x")
			run := fmt.Sprintf("run_%d", i)
			var pinErr, delErr error
			var report secrets.RetentionReport
			parallel(2, func(k int) {
				if k == 0 {
					pinErr = e.pin(scopeA1, run, name, 1)
					return
				}
				time.Sleep(time.Duration(i%4) * time.Millisecond) // vary which side starts first
				delErr = e.svc.Atomically(e.ctx(), tenantA, func(ctx context.Context, w secrets.Writer) error {
					var err error
					_, report, err = e.svc.DeleteSecret(ctx, w, e.mutation("alice"), scopeA1, sec.ID(), 1)
					return err
				})
			})
			if delErr != nil {
				t.Fatalf("delete: %v", delErr)
			}
			pins := e.count(`SELECT count(*) FROM secret_pins WHERE consumer_id = $1`, run)
			switch {
			case pinErr == nil:
				// The pin committed first: the deletion report must account for it.
				pinFirst++
				if report.PinnedVersions != 1 || pins != 1 {
					t.Fatalf("pin committed but the report shows %d (rows %d)", report.PinnedVersions, pins)
				}
			case errors.Is(pinErr, secrets.ErrDeleted):
				deleteFirst++
				if report.PinnedVersions != 0 || pins != 0 {
					t.Fatalf("pin refused but %d pins exist (report %d)", pins, report.PinnedVersions)
				}
			default:
				t.Fatalf("unexpected pin error: %v", pinErr)
			}
		}
		t.Logf("pin first %d, delete first %d", pinFirst, deleteFirst)
	})

	t.Run("pin registration and revocation are totally ordered", func(t *testing.T) {
		e := newEnv(t)
		for i := 0; i < 20; i++ {
			name := fmt.Sprintf("pr-%d", i)
			sec := e.createSecret(scopeA1, name, "x")
			run := fmt.Sprintf("run_%d", i)
			var pinErr error
			var res secrets.RevocationResult
			var revErr error
			parallel(2, func(k int) {
				if k == 0 {
					pinErr = e.pin(scopeA1, run, name, 1)
					return
				}
				res, revErr = e.revokeErr(scopeA1, sec.ID(), 1)
			})
			if revErr != nil {
				t.Fatalf("revoke: %v", revErr)
			}
			switch {
			case pinErr == nil:
				// Registered before the revocation committed: handling was recorded.
				if len(res.Handling) != 2 {
					t.Fatalf("pin committed before revocation but %d effects were recorded", len(res.Handling))
				}
			case errors.Is(pinErr, secrets.ErrRevoked):
				if len(res.Handling) != 0 || e.count(`SELECT count(*) FROM secret_pins WHERE consumer_id = $1`, run) != 0 {
					t.Fatal("a refused pin left state behind")
				}
			default:
				t.Fatalf("unexpected pin error: %v", pinErr)
			}
		}
	})

	t.Run("every released value was audited and none after revocation", func(t *testing.T) {
		e := newEnv(t)
		sec := e.createSecret(scopeA1, "erp-token", "race-SENTINEL")
		auth := newAuthority()
		const consumers = 8
		for i := 0; i < consumers; i++ {
			run := fmt.Sprintf("run_%d", i)
			if err := e.pin(scopeA1, run, "erp-token", 1); err != nil {
				t.Fatal(err)
			}
			auth.set(secrets.AttemptGrant{Scope: scopeA1, RunID: domain.RunID(run), AttemptID: domain.AttemptID(fmt.Sprintf("att_%d", i)), Bindings: []runtimeBinding{binding("erp-token", 1)}})
		}
		r := e.resolver(auth)
		var released atomic.Int64
		var revokedAt atomic.Bool
		var lateRelease atomic.Int64
		parallel(consumers+1, func(i int) {
			if i == consumers {
				e.revoke(scopeA1, sec.ID(), 1)
				revokedAt.Store(true)
				return
			}
			for k := 0; k < 5; k++ {
				after := revokedAt.Load() // revocation had already returned when this call started
				resp, err := r.Resolve(e.ctx(), domain.AttemptID(fmt.Sprintf("att_%d", i)), ref("erp-token", 1), "r")
				if err == nil {
					released.Add(1)
					if after {
						lateRelease.Add(1)
					}
					_ = resp
				} else if !errors.Is(err, secrets.ErrRevoked) {
					t.Errorf("unexpected resolution error: %v", err)
					return
				}
			}
		})
		if lateRelease.Load() != 0 {
			t.Fatalf("%d values were released after revocation had returned", lateRelease.Load())
		}
		if audited := e.count(`SELECT count(*) FROM audit WHERE action = 'secret.resolve'`); int64(audited) != released.Load() {
			t.Fatalf("released %d values but audited %d", released.Load(), audited)
		}
		if counted := e.count(`SELECT coalesce(sum(resolution_count), 0)::int FROM secret_pins`); int64(counted) != released.Load() {
			t.Fatalf("released %d values but pins count %d", released.Load(), counted)
		}
		// After revocation nothing resolves.
		for i := 0; i < consumers; i++ {
			if _, err := r.Resolve(e.ctx(), domain.AttemptID(fmt.Sprintf("att_%d", i)), ref("erp-token", 1), "r"); !errors.Is(err, secrets.ErrRevoked) {
				t.Fatalf("resolved after revocation: %v", err)
			}
		}
	})
}

// rotateErrQuiet is rotateErr without any t.Fatal, safe for goroutines.
func (e *env) rotateErrQuiet(scope domain.Scope, id string, value string) (secrets.Secret, error) {
	return e.rotateErr(scope, id, value)
}
