package secretshttp_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/secrets"
	secretshttp "github.com/optimiware/kodama/internal/secrets/http"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func (e *env) createVariable(t testing.TB, key, name string, value any) result {
	t.Helper()
	return e.call("POST", baseURL+"/variables", "alice", mustJSON(t, map[string]any{"name": name, "value": value}), "Idempotency-Key", key)
}

func (e *env) createSecret(t testing.TB, key, name, value string) result {
	t.Helper()
	return e.call("POST", baseURL+"/secrets", "alice", mustJSON(t, map[string]any{"name": name, "kind": "generic", "value": b64(value)}), "Idempotency-Key", key)
}

func Test_variable_routes(t *testing.T) {
	e := newEnv(t)

	created := e.createVariable(t, "k-var-1", "region", "eu")
	if created.Code != 201 || created.etag() != `"1"` || !strings.HasSuffix(created.Header.Get("Location"), "/variables/"+varID(t, created)) {
		t.Fatalf("create: %d %s %v", created.Code, created.Body, created.Header)
	}
	var v secretshttp.Variable
	created.json(t, &v)
	if v.Name != "region" || v.Type != "string" || v.Value.String() != "eu" || v.ValueVersion != 1 || v.Version != 1 || v.DeletedAt != nil {
		t.Fatalf("variable %+v", v)
	}
	path := baseURL + "/variables/" + v.Id

	t.Run("typed values", func(t *testing.T) {
		for key, tc := range map[string]struct {
			name  string
			value any
			typ   string
		}{
			"k-int": {"retries", 3, "integer"}, "k-bool": {"debug", true, "boolean"},
		} {
			res := e.createVariable(t, key, tc.name, tc.value)
			var got secretshttp.Variable
			res.json(t, &got)
			if res.Code != 201 || string(got.Type) != tc.typ {
				t.Fatalf("%s: %d %s", tc.name, res.Code, res.Body)
			}
		}
		for name, body := range map[string]string{
			"float":        `{"name":"ratio","value":1.5}`,
			"null":         `{"name":"nothing","value":null}`,
			"object":       `{"name":"obj","value":{"a":1}}`,
			"array":        `{"name":"arr","value":[1]}`,
			"missing":      `{"name":"gone"}`,
			"bad name":     `{"name":"Bad_Name","value":"x"}`,
			"unknown":      `{"name":"x","value":"y","secret":true}`,
			"unsafe int":   `{"name":"big","value":9007199254740993}`,
			"long string":  fmt.Sprintf(`{"name":"long","value":%q}`, strings.Repeat("x", 4097)),
			"empty object": `{}`,
		} {
			res := e.call("POST", baseURL+"/variables", "alice", body, "Idempotency-Key", "k-bad-"+strings.ReplaceAll(name, " ", "-"))
			if res.Code != 400 || res.errorCode(t) != "invalid_request" {
				t.Errorf("%s: %d %s", name, res.Code, res.Body)
			}
		}
	})

	t.Run("names are unique and never reused", func(t *testing.T) {
		res := e.createVariable(t, "k-var-dup", "region", "us")
		if res.Code != 409 || res.errorCode(t) != "name_conflict" {
			t.Fatalf("duplicate name: %d %s", res.Code, res.Body)
		}
	})

	t.Run("conditional edits", func(t *testing.T) {
		if res := e.call("PATCH", path, "alice", `{"value":"us"}`); res.Code != 428 || res.errorCode(t) != "version_required" {
			t.Fatalf("missing If-Match: %d %s", res.Code, res.Body)
		}
		if res := e.call("PATCH", path, "alice", `{"value":"us"}`, "If-Match", `W/"1"`); res.Code != 400 {
			t.Fatalf("weak tag: %d", res.Code)
		}
		if res := e.call("PATCH", path, "alice", `{"value":"us"}`, "If-Match", `"7"`); res.Code != 412 || res.errorCode(t) != "version_conflict" {
			t.Fatalf("stale: %d %s", res.Code, res.Body)
		}
		if res := e.call("PATCH", path, "alice", `{"value":7}`, "If-Match", `"1"`); res.Code != 400 {
			t.Fatalf("type change: %d %s", res.Code, res.Body)
		}
		if res := e.call("PATCH", path, "alice", `{}`, "If-Match", `"1"`); res.Code != 400 {
			t.Fatalf("empty edit: %d %s", res.Code, res.Body)
		}
		res := e.call("PATCH", path, "alice", `{"value":"us","description":"ERP region"}`, "If-Match", `"1"`)
		var got secretshttp.Variable
		res.json(t, &got)
		if res.Code != 200 || res.etag() != `"2"` || got.ValueVersion != 2 || got.Version != 2 || got.Value.String() != "us" {
			t.Fatalf("edit: %d %s", res.Code, res.Body)
		}
		// A description-only edit keeps the value version.
		res = e.call("PATCH", path, "alice", `{"description":"changed"}`, "If-Match", `"2"`)
		res.json(t, &got)
		if res.Code != 200 || got.ValueVersion != 2 || got.Version != 3 {
			t.Fatalf("description edit: %d %s", res.Code, res.Body)
		}
		if res := e.call("GET", path, "alice", ""); res.Code != 200 || res.etag() != `"3"` {
			t.Fatalf("get: %d %v", res.Code, res.Header)
		}
	})

	t.Run("lists are paged by name with scoped cursors", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			e.createVariable(t, fmt.Sprintf("k-page-%d", i), fmt.Sprintf("page-%d", i), i)
		}
		var names []string
		cursor := ""
		pages := 0
		for {
			q := "?limit=3"
			if cursor != "" {
				q += "&cursor=" + url.QueryEscape(cursor)
			}
			res := e.call("GET", baseURL+"/variables"+q, "alice", "")
			var page secretshttp.VariablePage
			res.json(t, &page)
			if res.Code != 200 {
				t.Fatalf("list: %d %s", res.Code, res.Body)
			}
			for _, it := range page.Items {
				names = append(names, it.Name)
			}
			pages++
			if page.NextCursor == nil {
				break
			}
			cursor = *page.NextCursor
		}
		if len(names) != 8 || pages != 3 {
			t.Fatalf("%d variables in %d pages: %v", len(names), pages, names)
		}
		for i := 1; i < len(names); i++ {
			if names[i-1] >= names[i] {
				t.Fatalf("not ordered by name: %v", names)
			}
		}
		// A cursor from one project cannot continue another project's list.
		first := e.call("GET", baseURL+"/variables?limit=1", "alice", "")
		var page secretshttp.VariablePage
		first.json(t, &page)
		other := e.call("GET", "/api/v1/tenants/t_acme/projects/p_billing/variables?cursor="+url.QueryEscape(*page.NextCursor), "alice", "")
		if other.Code != 400 || other.errorCode(t) != "invalid_cursor" {
			t.Fatalf("cursor reused across projects: %d %s", other.Code, other.Body)
		}
		for _, q := range []string{"?limit=0", "?limit=201", "?limit=x", "?filter=a", "?cursor=garbage"} {
			if res := e.call("GET", baseURL+"/variables"+q, "alice", ""); res.Code != 400 {
				t.Errorf("%s: %d", q, res.Code)
			}
		}
	})

	t.Run("delete", func(t *testing.T) {
		if res := e.call("DELETE", path, "alice", ""); res.Code != 428 {
			t.Fatalf("no If-Match: %d", res.Code)
		}
		if res := e.call("DELETE", path, "alice", "", "If-Match", `"1"`); res.Code != 412 {
			t.Fatalf("stale delete: %d", res.Code)
		}
		if res := e.call("DELETE", path, "alice", "", "If-Match", `"3"`); res.Code != 204 || res.Body != "" {
			t.Fatalf("delete: %d %q", res.Code, res.Body)
		}
		// Repeating with the new ETag is a no-op; the tombstone stays readable and unlisted.
		if res := e.call("DELETE", path, "alice", "", "If-Match", `"4"`); res.Code != 204 {
			t.Fatalf("repeat delete: %d", res.Code)
		}
		res := e.call("GET", path, "alice", "")
		var got secretshttp.Variable
		res.json(t, &got)
		if res.Code != 200 || got.DeletedAt == nil {
			t.Fatalf("tombstone: %d %s", res.Code, res.Body)
		}
		if res := e.call("PATCH", path, "alice", `{"value":"x"}`, "If-Match", `"4"`); res.Code != 409 || res.errorCode(t) != "resource_deleted" {
			t.Fatalf("edit after delete: %d %s", res.Code, res.Body)
		}
		if res := e.createVariable(t, "k-var-reuse", "region", "x"); res.Code != 409 || res.errorCode(t) != "name_conflict" {
			t.Fatalf("name reused after delete: %d %s", res.Code, res.Body)
		}
	})
}

func varID(t testing.TB, r result) string {
	t.Helper()
	var v secretshttp.Variable
	r.json(t, &v)
	return v.Id
}

func secretID(t testing.TB, r result) string {
	t.Helper()
	var s secretshttp.Secret
	r.json(t, &s)
	return s.Id
}

func Test_secret_routes(t *testing.T) {
	e := newEnv(t)

	created := e.createSecret(t, "k-sec-1", "erp-token", "first-SENTINEL")
	var s secretshttp.Secret
	created.json(t, &s)
	if created.Code != 201 || created.etag() != `"1"` || s.Ref != "secret://project/erp-token" || s.CurrentVersion != 1 || s.Version != 1 || s.Kind != "generic" {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	spath := baseURL + "/secrets/" + s.Id

	t.Run("validation", func(t *testing.T) {
		good := b64("x")
		for name, body := range map[string]string{
			"bad kind":          fmt.Sprintf(`{"name":"a","kind":"oauth","value":%q}`, good),
			"bad name":          fmt.Sprintf(`{"name":"A_b","kind":"generic","value":%q}`, good),
			"missing value":     `{"name":"a","kind":"generic"}`,
			"empty value":       `{"name":"a","kind":"generic","value":""}`,
			"not base64":        `{"name":"a","kind":"generic","value":"***not base64***"}`,
			"unknown field":     fmt.Sprintf(`{"name":"a","kind":"generic","value":%q,"extra":1}`, good),
			"oversized payload": fmt.Sprintf(`{"name":"a","kind":"generic","value":%q}`, base64.StdEncoding.EncodeToString(make([]byte, 128<<10+1))),
		} {
			res := e.call("POST", baseURL+"/secrets", "alice", body, "Idempotency-Key", "k-"+strings.ReplaceAll(name, " ", "-"))
			if res.Code != 400 && res.Code != 413 {
				t.Errorf("%s: %d %s", name, res.Code, res.Body)
			}
		}
		if res := e.call("POST", baseURL+"/secrets", "alice", fmt.Sprintf(`{"name":"a","kind":"generic","value":%q}`, good)); res.Code != 400 {
			t.Errorf("missing Idempotency-Key: %d", res.Code)
		}
		if res := e.call("POST", baseURL+"/secrets", "alice", fmt.Sprintf(`{"name":"a","kind":"generic","value":%q}`, good), "Idempotency-Key", "k", "Content-Type", "text/plain"); res.Code != 415 {
			t.Errorf("wrong media type: %d", res.Code)
		}
		for _, kind := range secrets.Kinds() {
			res := e.call("POST", baseURL+"/secrets", "alice", fmt.Sprintf(`{"name":"k-%s","kind":%q,"value":%q}`, kind, kind, good), "Idempotency-Key", "k-kind-"+string(kind))
			if res.Code != 201 {
				t.Errorf("kind %s: %d %s", kind, res.Code, res.Body)
			}
		}
		if res := e.createSecret(t, "k-sec-dup", "erp-token", "other"); res.Code != 409 || res.errorCode(t) != "name_conflict" {
			t.Errorf("duplicate name: %d %s", res.Code, res.Body)
		}
	})

	t.Run("get and list are metadata only", func(t *testing.T) {
		res := e.call("GET", spath, "alice", "")
		if res.Code != 200 || res.etag() != `"1"` || strings.Contains(res.Body, "value") || strings.Contains(res.Body, "ciphertext") {
			t.Fatalf("get: %d %s", res.Code, res.Body)
		}
		list := e.call("GET", baseURL+"/secrets?limit=2", "alice", "")
		var page secretshttp.SecretPage
		list.json(t, &page)
		if list.Code != 200 || len(page.Items) != 2 || page.NextCursor == nil {
			t.Fatalf("list: %d %s", list.Code, list.Body)
		}
	})

	t.Run("rotation requires the current ETag and is keyed", func(t *testing.T) {
		body := mustJSON(t, map[string]any{"value": b64("second-SENTINEL")})
		if res := e.call("POST", spath+"/versions", "alice", body, "Idempotency-Key", "k-rot-0"); res.Code != 428 {
			t.Fatalf("no If-Match: %d", res.Code)
		}
		if res := e.call("POST", spath+"/versions", "alice", body, "Idempotency-Key", "k-rot-0", "If-Match", `"9"`); res.Code != 412 {
			t.Fatalf("stale: %d %s", res.Code, res.Body)
		}
		res := e.call("POST", spath+"/versions", "alice", body, "Idempotency-Key", "k-rot-1", "If-Match", `"1"`)
		var out secretshttp.SecretVersionResult
		res.json(t, &out)
		if res.Code != 201 || res.etag() != `"2"` || out.Secret.CurrentVersion != 2 || out.Version.Version != 2 || out.Version.RevokedAt != nil {
			t.Fatalf("rotate: %d %s", res.Code, res.Body)
		}
		// The same key replays; a second rotation with the old ETag is stale.
		replay := e.call("POST", spath+"/versions", "alice", body, "Idempotency-Key", "k-rot-1", "If-Match", `"1"`)
		if replay.Code != 201 || replay.Header.Get("Idempotent-Replayed") != "true" || replay.Body != res.Body {
			t.Fatalf("replay: %d %v", replay.Code, replay.Header)
		}
		if res := e.call("POST", spath+"/versions", "alice", body, "Idempotency-Key", "k-rot-2", "If-Match", `"1"`); res.Code != 412 {
			t.Fatalf("second rotation with a stale ETag: %d", res.Code)
		}
		list := e.call("GET", spath+"/versions", "alice", "")
		var page secretshttp.SecretVersionPage
		list.json(t, &page)
		if list.Code != 200 || len(page.Items) != 2 || page.Items[0].Version != 1 || page.Items[1].Version != 2 || page.Items[0].Algorithm == "" || page.Items[0].KeyGeneration != 1 {
			t.Fatalf("versions: %d %s", list.Code, list.Body)
		}
		// Version pages are scoped and ordered.
		p1 := e.call("GET", spath+"/versions?limit=1", "alice", "")
		var pg secretshttp.SecretVersionPage
		p1.json(t, &pg)
		if len(pg.Items) != 1 || pg.NextCursor == nil {
			t.Fatalf("page 1: %s", p1.Body)
		}
		p2 := e.call("GET", spath+"/versions?limit=1&cursor="+url.QueryEscape(*pg.NextCursor), "alice", "")
		p2.json(t, &pg)
		if len(pg.Items) != 1 || pg.Items[0].Version != 2 || pg.NextCursor != nil {
			t.Fatalf("page 2: %s", p2.Body)
		}
	})

	t.Run("revocation", func(t *testing.T) {
		res := e.call("POST", spath+"/versions/1/revoke", "alice", "")
		var out secretshttp.RevokeSecretVersionResult
		res.json(t, &out)
		if res.Code != 200 || out.Version.RevokedAt == nil || out.Secret.CurrentVersion != 2 || out.ConsumerHandling.Total != 0 || res.etag() != `"3"` {
			t.Fatalf("revoke: %d %s", res.Code, res.Body)
		}
		at := *out.Version.RevokedAt
		again := e.call("POST", spath+"/versions/1/revoke", "alice", `{"reason":"repeat"}`)
		var out2 secretshttp.RevokeSecretVersionResult
		again.json(t, &out2)
		if again.Code != 200 || !out2.Version.RevokedAt.Equal(at) || out2.Secret.Version != 3 {
			t.Fatalf("repeat: %d %s", again.Code, again.Body)
		}
		for _, p := range []string{"/versions/9/revoke", "/versions/0/revoke", "/versions/x/revoke", "/versions/01/revoke"} {
			if res := e.call("POST", spath+p, "alice", ""); res.Code != 404 && res.Code != 400 {
				t.Errorf("%s: %d", p, res.Code)
			}
		}
		if res := e.call("POST", spath+"/versions/1/revoke", "alice", `{"reason":1}`); res.Code != 400 {
			t.Errorf("bad body: %d", res.Code)
		}
	})

	t.Run("deletion reports retention", func(t *testing.T) {
		if res := e.call("DELETE", spath, "alice", ""); res.Code != 428 {
			t.Fatalf("no If-Match: %d", res.Code)
		}
		if res := e.call("DELETE", spath, "alice", "", "If-Match", `"1"`); res.Code != 412 {
			t.Fatalf("stale delete: %d", res.Code)
		}
		res := e.call("DELETE", spath, "alice", "", "If-Match", `"3"`)
		var out secretshttp.DeleteSecretResult
		res.json(t, &out)
		if res.Code != 200 || out.Secret.DeletedAt == nil || out.Retention.BackupRetention != "unresolved" || res.etag() != `"4"` {
			t.Fatalf("delete: %d %s", res.Code, res.Body)
		}
		// The tombstone is readable with its retention report, listed nowhere, and takes no new versions.
		get := e.call("GET", spath, "alice", "")
		var tomb secretshttp.Secret
		get.json(t, &tomb)
		if get.Code != 200 || tomb.DeletedAt == nil || tomb.Retention == nil || tomb.Retention.BackupRetention != "unresolved" {
			t.Fatalf("tombstone: %d %s", get.Code, get.Body)
		}
		if res := e.call("POST", spath+"/versions", "alice", mustJSON(t, map[string]any{"value": b64("x")}), "Idempotency-Key", "k-late", "If-Match", `"4"`); res.Code != 409 || res.errorCode(t) != "resource_deleted" {
			t.Fatalf("version after delete: %d %s", res.Code, res.Body)
		}
		if res := e.createSecret(t, "k-reuse", "erp-token", "x"); res.Code != 409 || res.errorCode(t) != "name_conflict" {
			t.Fatalf("deleted name reused: %d %s", res.Code, res.Body)
		}
		for _, it := range pageSecrets(t, e) {
			if it.Id == s.Id {
				t.Fatal("deleted secret listed")
			}
		}
	})
}

func pageSecrets(t testing.TB, e *env) []secretshttp.Secret {
	t.Helper()
	res := e.call("GET", baseURL+"/secrets?limit=200", "alice", "")
	var page secretshttp.SecretPage
	res.json(t, &page)
	return page.Items
}

func Test_authorization_and_isolation(t *testing.T) {
	e := newEnv(t)
	created := e.createSecret(t, "k1", "erp-token", "x")
	id := secretID(t, created)
	spath := baseURL + "/secrets/" + id

	t.Run("authentication and tenant authorization", func(t *testing.T) {
		if res := e.call("GET", spath, "", ""); res.Code != 401 {
			t.Fatalf("anonymous: %d", res.Code)
		}
		if res := e.call("GET", spath, "nobody", ""); res.Code != 401 {
			t.Fatalf("unknown token: %d", res.Code)
		}
		// bob belongs to the other tenant: hidden, indistinguishable from missing.
		res := e.call("GET", spath, "bob", "")
		missing := e.call("GET", baseURL+"/secrets/missing", "bob", "")
		if res.Code != 404 || res.errorCode(t) != "not_found" || missing.Code != 404 || res.Header.Get("Content-Type") != missing.Header.Get("Content-Type") {
			t.Fatalf("cross-tenant read: %d %s", res.Code, res.Body)
		}
		if res := e.call("GET", "/api/v1/tenants/t_globex/projects/p_ops/secrets/"+id, "bob", ""); res.Code != 404 {
			t.Fatalf("another tenant's scope with this tenant's secret ID: %d", res.Code)
		}
		// carol is a member of both tenants; the ID still belongs to one tenant/project only.
		if res := e.call("GET", "/api/v1/tenants/t_globex/projects/p_ops/secrets/"+id, "carol", ""); res.Code != 404 {
			t.Fatalf("secret ID resolved in another tenant: %d", res.Code)
		}
		if res := e.call("GET", "/api/v1/tenants/t_acme/projects/p_billing/secrets/"+id, "alice", ""); res.Code != 404 {
			t.Fatalf("secret ID resolved in another project: %d", res.Code)
		}
	})

	t.Run("per-action denial", func(t *testing.T) {
		e.auth.deny("secret.version.revoke", httpapi.ErrDenied)
		if res := e.call("POST", spath+"/versions/1/revoke", "alice", ""); res.Code != 403 {
			t.Fatalf("denied action: %d", res.Code)
		}
		e.auth.deny("secret.version.revoke", nil)
		e.auth.deny("secret.create", httpapi.ErrDenied)
		if res := e.createSecret(t, "k-denied", "other", "x"); res.Code != 403 {
			t.Fatalf("denied create: %d", res.Code)
		}
		if e.count(`SELECT count(*) FROM secrets WHERE name = 'other'`) != 0 {
			t.Fatal("a denied request created a secret")
		}
		e.auth.deny("secret.create", nil)
		// There is no plaintext route at all.
		for _, p := range []string{spath + "/value", spath + "/versions/1", spath + "/versions/1/value", spath + "/resolve"} {
			if res := e.call("GET", p, "alice", ""); res.Code != 404 && res.Code != 405 {
				t.Errorf("GET %s: %d", p, res.Code)
			}
		}
	})

	t.Run("routes carry authorization metadata", func(t *testing.T) {
		if len(e.h.Routes()) != 12 {
			t.Fatalf("%d routes", len(e.h.Routes()))
		}
		// Every route denies by default for an unknown principal.
		for _, r := range e.h.Routes() {
			p := strings.NewReplacer("{tenantId}", "t_acme", "{projectId}", "p_inventory", "{id}", id, "{version}", "1").Replace(r.Path)
			if res := e.call(r.Method, "/api/v1"+p, "", ""); res.Code != 401 {
				t.Errorf("%s %s without credentials: %d", r.Method, r.Path, res.Code)
			}
		}
	})
}

func Test_key_provider_failure_is_classified(t *testing.T) {
	e := newEnv(t)
	e.wrap.fail(&secrets.KeyError{Class: secrets.KeyUnavailable, Op: "wrap", Err: fmt.Errorf("kms timeout: secret-SENTINEL")})
	res := e.createSecret(t, "k-down", "erp-token", "kms-SENTINEL")
	if res.Code != 503 || res.errorCode(t) != "unavailable" || res.Header.Get("Retry-After") == "" {
		t.Fatalf("provider outage: %d %s %v", res.Code, res.Body, res.Header)
	}
	var env httpapi.ErrorEnvelope
	res.json(t, &env)
	if env.Error.Details["retryable"] != true || env.Error.Details["keyProvider"] != "unavailable" {
		t.Fatalf("details %v", env.Error.Details)
	}
	if strings.Contains(res.Body, "SENTINEL") || strings.Contains(e.logs.String(), "kms timeout") && strings.Contains(e.logs.String(), "kms-SENTINEL") {
		t.Fatal("the failure leaked the value")
	}
	if e.count(`SELECT count(*) FROM secrets`) != 0 || e.count(`SELECT count(*) FROM secret_scope_keys`) != 0 || e.count(`SELECT count(*) FROM idempotency`) != 0 {
		t.Fatal("an outage left state behind")
	}
	// Denial is not retryable.
	e.wrap.fail(&secrets.KeyError{Class: secrets.KeyDenied, Op: "wrap"})
	res = e.createSecret(t, "k-denied", "erp-token", "kms-SENTINEL")
	res.json(t, &env)
	if res.Code != 503 || env.Error.Details["retryable"] != false || env.Error.Details["keyProvider"] != "denied" || res.Header.Get("Retry-After") != "" {
		t.Fatalf("denial: %d %s %v", res.Code, res.Body, res.Header)
	}
	// Recovery: the same key now executes.
	e.wrap.fail(nil)
	if res := e.createSecret(t, "k-down", "erp-token", "kms-SENTINEL"); res.Code != 201 {
		t.Fatalf("after recovery: %d %s", res.Code, res.Body)
	}
	// Rotation during an outage changes nothing.
	id := secretID(t, e.call("GET", baseURL+"/secrets?limit=1", "alice", "").asPage(t))
	e.wrap.fail(&secrets.KeyError{Class: secrets.KeyUnavailable, Op: "unwrap"})
	before := e.count(`SELECT count(*) FROM secret_versions`)
	if res := e.call("POST", baseURL+"/secrets/"+id+"/versions", "alice", mustJSON(t, map[string]any{"value": b64("v2")}), "Idempotency-Key", "k-rot", "If-Match", `"1"`); res.Code != 503 {
		t.Fatalf("rotation during outage: %d %s", res.Code, res.Body)
	}
	if e.count(`SELECT count(*) FROM secret_versions`) != before {
		t.Fatal("rotation during an outage added a version")
	}
}

// asPage returns the first item of a list response as a one-item JSON result.
func (r result) asPage(t testing.TB) result {
	t.Helper()
	var page struct {
		Items []map[string]any `json:"items"`
	}
	r.json(t, &page)
	if len(page.Items) == 0 {
		t.Fatalf("empty list: %s", r.Body)
	}
	b, _ := json_marshal(page.Items[0])
	return result{Code: r.Code, Header: http.Header{}, Body: string(b)}
}

func Test_backpressure_maps_to_unavailable(t *testing.T) {
	e := newEnv(t, withBacklog(1))
	if res := e.createVariable(t, "k1", "a", "x"); res.Code != 201 {
		t.Fatalf("first: %d %s", res.Code, res.Body)
	}
	res := e.createVariable(t, "k2", "b", "x")
	if res.Code != 503 || res.errorCode(t) != "unavailable" || res.Header.Get("Retry-After") == "" {
		t.Fatalf("backlog full: %d %s", res.Code, res.Body)
	}
	if e.count(`SELECT count(*) FROM variables`) != 1 || e.count(`SELECT count(*) FROM idempotency`) != 1 {
		t.Fatal("a refused event left state behind")
	}
}
