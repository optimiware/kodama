package integration_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/identity"
)

// TestReplayAfterRevocation: IT-09 / AC-02 / T-004's stored-response replay. The
// shared router asks the real identity authorizer again before a stored response
// leaves, so a revoked caller gets a refusal and none of the cached content.
func TestReplayAfterRevocation(t *testing.T) {
	e := newEnv(t)
	adminTok, engTok := e.signIn("admin"), e.signIn("engineer")
	e.bind("admin", tenantA, "", identity.RoleAdmin)
	e.bind("engineer", tenantA, "", identity.RoleViewer) // read-only floor, so refusals are 403
	bid, etag := e.grantVia(adminTok, tenantA, e.users["engineer"], "", "engineer")

	path := "/tenants/" + tenantA + "/projects/" + projectA + "/widgets"
	create := func(tok, key string) response {
		return e.do(tok, http.MethodPost, path, `{"name":"x"}`, "Idempotency-Key", key)
	}

	first := create(engTok, "key-1")
	if first.code != 201 || first.header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("create: %d %v %s", first.code, first.header, first.body)
	}
	var w struct{ Id, Name string }
	first.json(t, &w)
	if replay := create(engTok, "key-1"); replay.code != 201 || replay.header.Get("Idempotent-Replayed") != "true" || replay.body != first.body {
		t.Fatalf("replay: %d %v %s", replay.code, replay.header, replay.body)
	}
	if n := e.stack.Count(`SELECT count(*) FROM t010_widgets`); n != 1 {
		t.Fatalf("%d widgets", n)
	}

	if r := e.revokeVia(adminTok, tenantA, bid, etag); r.code != 204 {
		t.Fatalf("revocation: %d %s", r.code, r.body)
	}
	for name, body := range map[string]string{"same body": `{"name":"x"}`, "changed body": `{"name":"y"}`} {
		r := e.do(engTok, http.MethodPost, path, body, "Idempotency-Key", "key-1")
		if r.code != 403 || r.errCode(t) != "forbidden" {
			t.Errorf("%s after revocation: %d %s", name, r.code, r.body)
		}
		if strings.Contains(r.body, w.Id) || strings.Contains(r.body, "secret-widget-name") || r.header.Get("Idempotent-Replayed") != "" || r.header.Get("ETag") != "" || r.header.Get("Location") != "" {
			t.Errorf("%s: the refusal discloses cached content: %v %s", name, r.header, r.body)
		}
	}
	if n := e.stack.Count(`SELECT count(*) FROM t010_widgets`); n != 1 {
		t.Fatalf("a refused replay mutated state: %d widgets", n)
	}

	// With every binding gone the route is hidden, still without content.
	e.stack.OwnerExec(`DELETE FROM memberships WHERE user_id = $1`, string(e.users["engineer"]))
	if r := create(engTok, "key-1"); r.code != 404 || strings.Contains(r.body, w.Id) {
		t.Fatalf("hidden: %d %s", r.code, r.body)
	}

	// The stored record was never the authority: a new grant lets the same key replay again.
	e.grantVia(adminTok, tenantA, e.users["engineer"], "", "engineer")
	if r := create(engTok, "key-1"); r.code != 201 || r.header.Get("Idempotent-Replayed") != "true" || r.body != first.body {
		t.Fatalf("replay after regrant: %d %v %s", r.code, r.header, r.body)
	}
	// Another person cannot replay it under their own identity: the key belongs to its principal.
	other := e.signIn("viewer")
	e.bind("viewer", tenantA, "", identity.RoleEngineer)
	if r := create(other, "key-1"); r.code != 201 || r.header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("another principal: %d %v %s", r.code, r.header, r.body)
	}
	if n := e.stack.Count(`SELECT count(*) FROM t010_widgets`); n != 2 {
		t.Fatalf("%d widgets", n)
	}
}
