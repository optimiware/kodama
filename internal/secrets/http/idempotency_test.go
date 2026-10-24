package secretshttp_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/httpapi"
)

// Test_idempotent_configuration_write: concurrent identical keys cause one
// effect, changed bodies conflict, revoked authorization denies replay, and
// lost or unknown commits recover using the same key.
func Test_idempotent_configuration_write(t *testing.T) {
	t.Run("concurrent identical keys produce one effect", func(t *testing.T) {
		e := newEnv(t)
		const n = 8
		results := make([]result, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				results[i] = e.createSecret(t, "k-same", "erp-token", "concurrent-SENTINEL")
			}(i)
		}
		close(start)
		wg.Wait()
		replays := 0
		var first string
		for _, r := range results {
			if r.Code != 201 {
				t.Fatalf("a concurrent request failed: %d %s", r.Code, r.Body)
			}
			if r.Header.Get("Idempotent-Replayed") == "true" {
				replays++
			}
			// Every response describes the same secret (same ID).
			id := secretID(t, r)
			if first == "" {
				first = id
			} else if id != first {
				t.Fatalf("two different secrets were created: %s %s", first, id)
			}
		}
		if replays != n-1 {
			t.Fatalf("%d replays of %d", replays, n)
		}
		if e.count(`SELECT count(*) FROM secrets`) != 1 || e.count(`SELECT count(*) FROM secret_versions`) != 1 ||
			e.count(`SELECT count(*) FROM audit WHERE action = 'secret.create'`) != 1 || e.events(tenantA) != 1 || e.count(`SELECT count(*) FROM idempotency`) != 1 {
			t.Fatalf("effects: secrets %d versions %d audit %d events %d records %d",
				e.count(`SELECT count(*) FROM secrets`), e.count(`SELECT count(*) FROM secret_versions`),
				e.count(`SELECT count(*) FROM audit WHERE action = 'secret.create'`), e.events(tenantA), e.count(`SELECT count(*) FROM idempotency`))
		}
	})

	t.Run("a changed body conflicts and discloses nothing", func(t *testing.T) {
		e := newEnv(t)
		first := e.createSecret(t, "k1", "erp-token", "original-SENTINEL")
		if first.Code != 201 {
			t.Fatal(first.Body)
		}
		changed := e.createSecret(t, "k1", "erp-token", "different-SENTINEL")
		if changed.Code != 409 || changed.errorCode(t) != "idempotency_conflict" {
			t.Fatalf("changed body: %d %s", changed.Code, changed.Body)
		}
		if strings.Contains(changed.Body, secretID(t, first)) {
			t.Fatal("the conflict response disclosed stored content")
		}
		// Even a different name is a different request.
		other := e.createSecret(t, "k1", "other-name", "original-SENTINEL")
		if other.Code != 409 {
			t.Fatalf("changed name: %d", other.Code)
		}
		if e.count(`SELECT count(*) FROM secrets`) != 1 {
			t.Fatal("a conflicting request had an effect")
		}
		// The same key under a different principal or tenant is a different request identity.
		viaCarol := e.call("POST", baseURL+"/secrets", "carol", mustJSON(t, map[string]any{"name": "erp-token", "kind": "generic", "value": b64("original-SENTINEL")}), "Idempotency-Key", "k1")
		if viaCarol.Code != 409 || viaCarol.errorCode(t) != "name_conflict" {
			t.Fatalf("a different principal reused the stored response: %d %s", viaCarol.Code, viaCarol.Body)
		}
	})

	t.Run("revoked authorization denies replay", func(t *testing.T) {
		e := newEnv(t)
		first := e.createSecret(t, "k1", "erp-token", "replay-SENTINEL")
		if first.Code != 201 {
			t.Fatal(first.Body)
		}
		// alice loses access after the first response.
		e.auth.revoke("alice", httpapi.ErrDenied)
		replay := e.createSecret(t, "k1", "erp-token", "replay-SENTINEL")
		if replay.Code != 403 || strings.Contains(replay.Body, secretID(t, first)) {
			t.Fatalf("replay after revocation: %d %s", replay.Code, replay.Body)
		}
		e.auth.revoke("alice", httpapi.ErrHidden)
		if res := e.createSecret(t, "k1", "erp-token", "replay-SENTINEL"); res.Code != 404 || strings.Contains(res.Body, secretID(t, first)) {
			t.Fatalf("hidden replay: %d %s", res.Code, res.Body)
		}
		// Access restored: the replay is served again without a second effect.
		e.auth.revoke("alice", nil)
		if res := e.createSecret(t, "k1", "erp-token", "replay-SENTINEL"); res.Code != 201 || res.Header.Get("Idempotent-Replayed") != "true" {
			t.Fatalf("replay after restoration: %d %v", res.Code, res.Header)
		}
		if e.count(`SELECT count(*) FROM secrets`) != 1 {
			t.Fatal("replays created secrets")
		}
	})

	t.Run("a commit that never happened is retried with the same key", func(t *testing.T) {
		e := newEnv(t)
		e.createSecret(t, "k-seed", "seed", "x") // creates the tenant's scope key
		e.db.set("rollback")
		res := e.createSecret(t, "k1", "erp-token", "lost-SENTINEL")
		if res.Code != 503 {
			t.Fatalf("failed commit: %d %s", res.Code, res.Body)
		}
		if e.count(`SELECT count(*) FROM secrets`) != 1 || e.count(`SELECT count(*) FROM idempotency`) != 1 {
			t.Fatal("a failed commit left state behind")
		}
		e.db.set("")
		retry := e.createSecret(t, "k1", "erp-token", "lost-SENTINEL")
		if retry.Code != 201 || retry.Header.Get("Idempotent-Replayed") == "true" {
			t.Fatalf("retry: %d %v", retry.Code, retry.Header)
		}
		if e.count(`SELECT count(*) FROM secrets`) != 2 {
			t.Fatal("retry did not create the secret")
		}
	})

	t.Run("an unknown commit replays on retry with no second effect", func(t *testing.T) {
		e := newEnv(t)
		e.createSecret(t, "k-seed", "seed", "x") // creates the tenant's scope key
		e.db.set("lost")
		res := e.createSecret(t, "k1", "erp-token", "unknown-SENTINEL")
		if res.Code != 503 || res.errorCode(t) != "unavailable" {
			t.Fatalf("unknown commit must never report success: %d %s", res.Code, res.Body)
		}
		if strings.Contains(res.Body, "currentVersion") {
			t.Fatal("an unknown outcome leaked the response")
		}
		// The commit did happen on the server.
		if e.count(`SELECT count(*) FROM secrets`) != 2 {
			t.Fatalf("the unknown commit did not commit: %d", e.count(`SELECT count(*) FROM secrets`))
		}
		e.db.set("")
		retry := e.createSecret(t, "k1", "erp-token", "unknown-SENTINEL")
		if retry.Code != 201 || retry.Header.Get("Idempotent-Replayed") != "true" {
			t.Fatalf("retry after an unknown commit: %d %v %s", retry.Code, retry.Header, retry.Body)
		}
		if e.count(`SELECT count(*) FROM secrets`) != 2 || e.count(`SELECT count(*) FROM audit WHERE action = 'secret.create'`) != 2 || e.events(tenantA) != 2 {
			t.Fatal("a retry after an unknown commit repeated the effect")
		}
	})

	t.Run("rotation is keyed and a lost response is recovered", func(t *testing.T) {
		e := newEnv(t)
		created := e.createSecret(t, "k-create", "erp-token", "one")
		id := secretID(t, created)
		path := baseURL + "/secrets/" + id + "/versions"
		body := mustJSON(t, map[string]any{"value": b64("two")})
		e.db.set("lost")
		if res := e.call("POST", path, "alice", body, "Idempotency-Key", "k-rot", "If-Match", `"1"`); res.Code != 503 {
			t.Fatalf("lost rotation response: %d %s", res.Code, res.Body)
		}
		e.db.set("")
		// The rotation committed. Retrying with the same key and the original If-Match replays.
		retry := e.call("POST", path, "alice", body, "Idempotency-Key", "k-rot", "If-Match", `"1"`)
		if retry.Code != 201 || retry.Header.Get("Idempotent-Replayed") != "true" {
			t.Fatalf("retry: %d %s", retry.Code, retry.Body)
		}
		if e.count(`SELECT count(*) FROM secret_versions`) != 2 {
			t.Fatalf("%d versions after a recovered rotation", e.count(`SELECT count(*) FROM secret_versions`))
		}
		// A new key with the old ETag is a stale write, not a replay.
		if res := e.call("POST", path, "alice", body, "Idempotency-Key", "k-rot-2", "If-Match", `"1"`); res.Code != 412 {
			t.Fatalf("stale rotation: %d", res.Code)
		}
		// Different If-Match under the old key is a different request.
		if res := e.call("POST", path, "alice", body, "Idempotency-Key", "k-rot", "If-Match", `"2"`); res.Code != 409 {
			t.Fatalf("changed If-Match: %d", res.Code)
		}
	})

	t.Run("keys are scoped to the route", func(t *testing.T) {
		e := newEnv(t)
		a := e.createSecret(t, "shared", "alpha", "x")
		b := e.call("POST", "/api/v1/tenants/t_acme/projects/p_billing/secrets", "alice", mustJSON(t, map[string]any{"name": "alpha", "kind": "generic", "value": b64("x")}), "Idempotency-Key", "shared")
		if a.Code != 201 || b.Code != 201 || secretID(t, a) == secretID(t, b) || b.Header.Get("Idempotent-Replayed") == "true" {
			t.Fatalf("keys leaked across projects: %d %d", a.Code, b.Code)
		}
		if e.count(`SELECT count(*) FROM secrets`) != 2 {
			t.Fatal("expected one secret per project")
		}
		// Variables and secrets with the same key are independent routes.
		v := e.createVariable(t, "shared", "alpha", "x")
		if v.Code != 201 {
			t.Fatalf("variable with a key used by a secret: %d %s", v.Code, v.Body)
		}
	})

}
