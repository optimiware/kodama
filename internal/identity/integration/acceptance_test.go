package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	idoidc "github.com/optimiware/kodama/internal/identity/oidc"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
	oidctest "github.com/optimiware/kodama/test/support/oidc"
)

func (e *env) userCount() int { return e.stack.Count(`SELECT count(*) FROM users`) }

// Test_wrong_audience: IT-A01 / IT-01. A real oauthsonas access token is accepted
// for its API audience and refused for the wrong audience, the wrong issuer, and
// as an ID token, before any user, membership or resource is touched.
func Test_wrong_audience(t *testing.T) {
	e := newEnv(t)
	good := e.fx.Login("engineer")
	e.bind("engineer", tenantA, projectA, identity.RoleViewer)

	t.Run("the API access token is accepted and maps its person", func(t *testing.T) {
		r := e.do(good.Access, http.MethodGet, "/me", "")
		if r.code != 200 {
			t.Fatalf("%d %s", r.code, r.body)
		}
		var me struct{ Issuer, Subject string }
		r.json(t, &me)
		if me.Issuer != e.fx.Issuer || me.Subject != oidctest.Subject("engineer") {
			t.Fatalf("%+v", me)
		}
		if r := e.do(good.Access, http.MethodGet, "/tenants/"+tenantA+"/projects/"+projectA, ""); r.code != 200 {
			t.Fatalf("bound access: %d %s", r.code, r.body)
		}
	})

	users := e.userCount()
	audit := e.stack.Count(`SELECT count(*) FROM audit`)

	t.Run("the client's ID token is not an API credential", func(t *testing.T) {
		for _, path := range []string{"/me", "/tenants", "/tenants/" + tenantA + "/projects/" + projectA, "/tenants/" + tenantA + "/projects/" + projectA + "/work-items"} {
			r := e.do(good.ID, http.MethodGet, path, "")
			if r.code != 401 || r.errCode(t) != "invalid_credential" {
				t.Errorf("%s with the ID token: %d %s", path, r.code, r.body)
			}
		}
	})

	t.Run("an installation configured for another audience refuses the very same tokens", func(t *testing.T) {
		other := newEnvOn(t, e.fx, func(c *idoidc.Config) { c.Audience = "https://other.example.test/api" })
		for name, tok := range map[string]string{"access": good.Access, "id": good.ID} {
			if r := other.do(tok, http.MethodGet, "/me", ""); r.code != 401 || r.errCode(t) != "invalid_credential" {
				t.Errorf("wrong audience accepted (%s): %d %s", name, r.code, r.body)
			}
		}
		if n := other.userCount(); n != 0 {
			t.Fatalf("a refused credential mapped %d users", n)
		}
		// The configured audience is the only thing that differs: the original installation still accepts the access token.
		if r := e.do(good.Access, http.MethodGet, "/me", ""); r.code != 200 {
			t.Fatalf("%d", r.code)
		}
	})

	t.Run("a token of another issuer is refused", func(t *testing.T) {
		foreign := oidctest.Start(t) // an independent instance: its own issuer URL and signing key
		tok := foreign.Login("engineer")
		for name, token := range map[string]string{"access": tok.Access, "id": tok.ID} {
			if r := e.do(token, http.MethodGet, "/me", ""); r.code != 401 {
				t.Errorf("%s token of another issuer: %d %s", name, r.code, r.body)
			}
		}
		// And the reverse: this issuer's token against a verifier of the other issuer.
		v, err := idoidc.NewVerifier(idoidc.Config{Issuer: foreign.Issuer, Audience: foreign.Audience, AllowInsecureHTTP: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.Verify(context.Background(), good.Access); err == nil {
			t.Fatal("a token verified against another issuer")
		} else if !strings.Contains(err.Error(), "unauthenticated") {
			t.Fatalf("expected a credential failure, got %v", err)
		}
	})

	t.Run("tampered and unsigned credentials are refused", func(t *testing.T) {
		parts := strings.Split(good.Access, ".")
		forgedClaims := e.fx.Login("admin").Access // another person's valid payload under this signature
		swapped := parts[0] + "." + strings.Split(forgedClaims, ".")[1] + "." + parts[2]
		for name, token := range map[string]string{
			"payload swap":   swapped,
			"signature flip": good.Access[:len(good.Access)-3] + "AAA",
			"alg none":       oidctest.UnsignedToken(map[string]any{"iss": e.fx.Issuer, "aud": []string{e.fx.Audience}, "sub": oidctest.Subject("admin"), "exp": 4102444800}),
		} {
			if r := e.do(token, http.MethodGet, "/me", ""); r.code != 401 {
				t.Errorf("%s: %d %s", name, r.code, r.body)
			}
		}
	})

	t.Run("refused credentials left no user, audit or work behind", func(t *testing.T) {
		if got := e.userCount(); got != users {
			t.Fatalf("users %d -> %d", users, got)
		}
		if got := e.stack.Count(`SELECT count(*) FROM audit`); got != audit {
			t.Fatalf("audit %d -> %d", audit, got)
		}
		if got := e.stack.Count(`SELECT count(*) FROM work_items`); got != 0 {
			t.Fatalf("%d work items", got)
		}
	})
}

// Test_membership_required: IT-A01 / IT-01 / IT-11. Valid credentials (even with
// powerful role claims) establish identity only; an explicit local binding
// enables exactly its scope; foreign references stay nondisclosing.
func Test_membership_required(t *testing.T) {
	e := newEnv(t)
	outsider := e.signIn("outsider") // role claims: admin and reviewer
	e.signIn("viewer")
	e.signIn("foreign")
	e.bind("foreign", tenantB, "", identity.RoleAdmin)
	foreignItem, _ := e.itemInReviewAs(t, "foreign", tenantB, projectB)

	t.Run("valid credentials and persona role claims grant nothing", func(t *testing.T) {
		if got := idClaims(t, outsider)["roles"]; fmt.Sprint(got) != "[admin reviewer]" {
			t.Fatalf("fixture claims: %v", got)
		}
		if r := e.do(outsider, http.MethodGet, "/me", ""); r.code != 200 {
			t.Fatalf("identity: %d", r.code)
		}
		var page struct{ Items []any }
		r := e.do(outsider, http.MethodGet, "/tenants", "")
		r.json(t, &page)
		if r.code != 200 || len(page.Items) != 0 || !strings.Contains(r.body, `"items":[]`) {
			t.Fatalf("discovery: %d %s", r.code, r.body)
		}
		for name, tc := range map[string][3]string{
			"projects":     {http.MethodGet, "/tenants/" + tenantA + "/projects", ""},
			"project":      {http.MethodGet, "/tenants/" + tenantA + "/projects/" + projectA, ""},
			"work list":    {http.MethodGet, workPath(tenantA, projectA), ""},
			"work create":  {http.MethodPost, workPath(tenantA, projectA), `{"kind":"story","title":"x"}`},
			"memberships":  {http.MethodGet, "/tenants/" + tenantA + "/memberships", ""},
			"grant":        {http.MethodPost, "/tenants/" + tenantA + "/memberships", fmt.Sprintf(`{"userId":%q,"role":"admin"}`, e.users["outsider"])},
			"project make": {http.MethodPost, "/tenants/" + tenantA + "/projects", `{"key":"mine"}`},
		} {
			if r := e.do(outsider, tc[0], tc[1], tc[2]); r.code != 404 || r.errCode(t) != "not_found" {
				t.Errorf("%s: %d %s", name, r.code, r.body)
			}
		}
		if e.stack.Count(`SELECT count(*) FROM memberships WHERE user_id = $1`, string(e.users["outsider"])) != 0 {
			t.Fatal("the outsider holds a membership")
		}
	})

	t.Run("an explicit binding enables exactly its scope", func(t *testing.T) {
		viewer := e.tokens["viewer"]
		e.bind("viewer", tenantA, projectA, identity.RoleViewer)
		if r := e.do(viewer, http.MethodGet, "/tenants/"+tenantA+"/projects/"+projectA, ""); r.code != 200 {
			t.Fatalf("own project: %d %s", r.code, r.body)
		}
		if r := e.do(viewer, http.MethodGet, workPath(tenantA, projectA), ""); r.code != 200 {
			t.Fatalf("work read: %d %s", r.code, r.body)
		}
		// A viewer reads; it cannot write.
		if r := e.do(viewer, http.MethodPost, workPath(tenantA, projectA), `{"kind":"story","title":"x"}`); r.code != 403 || r.errCode(t) != "forbidden" {
			t.Fatalf("viewer create: %d %s", r.code, r.body)
		}
		// Other project and other tenant: hidden.
		for name, path := range map[string]string{
			"other project": "/tenants/" + tenantA + "/projects/" + projectA2,
			"other tenant":  "/tenants/" + tenantB + "/projects/" + projectB,
			"other work":    workPath(tenantA, projectA2),
		} {
			if r := e.do(viewer, http.MethodGet, path, ""); r.code != 404 {
				t.Errorf("%s: %d %s", name, r.code, r.body)
			}
		}
		// Discovery shows the bound tenant and only the bound project.
		var tenants struct{ Items []struct{ Id string } }
		e.do(viewer, http.MethodGet, "/tenants", "").json(t, &tenants)
		if len(tenants.Items) != 1 || tenants.Items[0].Id != tenantA {
			t.Fatalf("%+v", tenants)
		}
		var projects struct{ Items []struct{ Id string } }
		e.do(viewer, http.MethodGet, "/tenants/"+tenantA+"/projects", "").json(t, &projects)
		if len(projects.Items) != 1 || projects.Items[0].Id != projectA {
			t.Fatalf("%+v", projects)
		}
	})

	t.Run("foreign references are nondisclosing", func(t *testing.T) {
		viewer := e.tokens["viewer"]
		// The viewer addresses tenant B's real work item and a made-up one in an
		// unbound tenant, and also a made-up tenant: all indistinguishable.
		real := e.do(viewer, http.MethodGet, foreignItem, "")
		missingItem := e.do(viewer, http.MethodGet, strings.Replace(foreignItem, "wi_", "wi_0", 1), "")
		missingTenant := e.do(viewer, http.MethodGet, strings.Replace(foreignItem, tenantB, "t010_nowhere", 1), "")
		for name, r := range map[string]response{"real": real, "missing item": missingItem, "missing tenant": missingTenant} {
			if r.code != 404 || r.errCode(t) != "not_found" {
				t.Errorf("%s: %d %s", name, r.code, r.body)
			}
			if strings.Contains(r.body, "wi_") || strings.Contains(r.body, tenantB) || strings.Contains(r.body, "globex") {
				t.Errorf("%s discloses a reference: %s", name, r.body)
			}
		}
		normalize := func(r response) string {
			var env struct {
				Error struct{ Code, Message string }
			}
			r.json(t, &env)
			return env.Error.Code + "|" + env.Error.Message
		}
		if normalize(real) != normalize(missingItem) || normalize(real) != normalize(missingTenant) {
			t.Fatalf("distinguishable: %q %q %q", normalize(real), normalize(missingItem), normalize(missingTenant))
		}
		// A viewer of tenant A addressing tenant A's route with tenant B's item ID finds nothing either.
		id := foreignItem[strings.LastIndex(foreignItem, "/")+1:]
		if r := e.do(viewer, http.MethodGet, workPath(tenantA, projectA)+"/"+id, ""); r.code != 404 {
			t.Fatalf("cross-tenant item id in own scope: %d %s", r.code, r.body)
		}
		// A binding in tenant A never reaches tenant B's membership list either.
		if r := e.do(viewer, http.MethodGet, "/tenants/"+tenantB+"/memberships", ""); r.code != 404 {
			t.Fatalf("%d", r.code)
		}
	})

	t.Run("removing the binding removes the access, and a different user is unaffected", func(t *testing.T) {
		viewer := e.tokens["viewer"]
		e.stack.OwnerExec(`DELETE FROM memberships WHERE user_id = $1`, string(e.users["viewer"]))
		if r := e.do(viewer, http.MethodGet, "/tenants/"+tenantA+"/projects/"+projectA, ""); r.code != 404 {
			t.Fatalf("%d %s", r.code, r.body)
		}
		if r := e.do(e.tokens["foreign"], http.MethodGet, "/tenants/"+tenantB+"/projects/"+projectB, ""); r.code != 200 {
			t.Fatalf("%d", r.code)
		}
	})
}

// itemInReviewAs creates an item as the persona (who must hold the needed roles).
func (e *env) itemInReviewAs(t *testing.T, persona string, tenant, project string) (path, etag string) {
	t.Helper()
	e.bind(persona, domainTenant(tenant), "", identity.RoleEngineer)
	return e.itemInReview(e.tokens[persona], tenant, project, "Item of "+persona)
}

// idClaims decodes the (unverified) payload of a token so the fixture's role
// claims can be shown to exist; no decision is ever made from them.
func idClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal("not a JWT")
	}
	raw, err := decodeSegment(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

func (e *env) membershipPayload(user identity.UserID, project, role string) string {
	if project == "" {
		return fmt.Sprintf(`{"userId":%q,"role":%q}`, user, role)
	}
	return fmt.Sprintf(`{"userId":%q,"projectId":%q,"role":%q}`, user, project, role)
}

func (e *env) grantVia(adminToken, tenant string, user identity.UserID, project, role string) (id string, etag string) {
	e.t.Helper()
	r := e.do(adminToken, http.MethodPost, "/tenants/"+tenant+"/memberships", e.membershipPayload(user, project, role))
	if r.code != 201 {
		e.t.Fatalf("grant %s: %d %s", role, r.code, r.body)
	}
	var m struct{ Id string }
	r.json(e.t, &m)
	return m.Id, r.header.Get("ETag")
}

func (e *env) revokeVia(adminToken, tenant, id, etag string) response {
	return e.do(adminToken, http.MethodDelete, "/tenants/"+tenant+"/memberships/"+id, "", "If-Match", etag)
}

// gateAllowed asks the gate permission seam (the effective-access Authority every
// later human-gate module asks) whether the user may decide gates now.
func (e *env) gateAllowed(user identity.UserID) error {
	e.t.Helper()
	standalone, err := identity.NewStandaloneAuthority(e.stack.Store)
	if err != nil {
		e.t.Fatal(err)
	}
	target := identity.Target{TenantID: tenantA, ProjectID: projectA}
	direct := standalone.Authorize(context.Background(), identity.UserActor(user), target, identity.PermGateDecide)
	// The transaction-bound variant must agree.
	var bound error
	err = storage.WithTenant(context.Background(), e.stack.Runtime, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		bound = identitypg.NewTxAuthority(tx).Authorize(ctx, identity.UserActor(user), target, identity.PermGateDecide)
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
	if (direct == nil) != (bound == nil) || (direct != nil && bound != nil && direct.Error() != bound.Error()) {
		e.t.Fatalf("authority variants disagree: %v vs %v", direct, bound)
	}
	return direct
}

// Test_role_revocation: IT-A07 / IT-09 / IT-10. The same still-valid token loses
// reviewer access immediately after the committed revocation, through the merged
// work consumer port and the gate permission seam; admin or engineer alone cannot
// substitute for the reviewer.
func Test_role_revocation(t *testing.T) {
	e := newEnv(t)
	adminTok, engTok, revTok := e.signIn("admin"), e.signIn("engineer"), e.signIn("reviewer")
	e.bind("admin", tenantA, "", identity.RoleAdmin)
	e.bind("engineer", tenantA, "", identity.RoleEngineer)
	e.bind("reviewer", tenantA, "", identity.RoleViewer) // a member who can read but is not yet a reviewer

	item1, etag1 := e.itemInReview(engTok, tenantA, projectA, "first")
	item2, etag2 := e.itemInReview(engTok, tenantA, projectA, "second")
	item3, etag3 := e.itemInReview(engTok, tenantA, projectA, "third")

	t.Run("neither admin nor engineer can stand in for the reviewer", func(t *testing.T) {
		for name, tok := range map[string]string{"admin": adminTok, "engineer": engTok, "viewer-only": revTok} {
			if r := e.complete(tok, item1, etag1); r.code != 403 || r.errCode(t) != "forbidden" {
				t.Errorf("%s completed or was misreported: %d %s", name, r.code, r.body)
			}
		}
		if e.itemState(item1) != "review" {
			t.Fatal("an unauthorized completion changed the item")
		}
		for name, user := range map[string]string{"admin": "admin", "engineer": "engineer", "viewer-only": "reviewer"} {
			if err := e.gateAllowed(e.users[user]); !errorsIs(err, identity.ErrDenied) {
				t.Errorf("gate seam for %s: %v", name, err)
			}
		}
	})

	var reviewerBinding, reviewerETag string
	t.Run("an administrator grants the reviewer role and completion works", func(t *testing.T) {
		reviewerBinding, reviewerETag = e.grantVia(adminTok, tenantA, e.users["reviewer"], "", "reviewer")
		if err := e.gateAllowed(e.users["reviewer"]); err != nil {
			t.Fatalf("gate seam after grant: %v", err)
		}
		r := e.complete(revTok, item1, etag1)
		if r.code != 200 || e.itemState(item1) != "done" {
			t.Fatalf("%d %s", r.code, r.body)
		}
		var it struct {
			Completion struct{ CompletedBy struct{ Id, Type string } }
		}
		r.json(t, &it)
		if it.Completion.CompletedBy.Id != string(e.users["reviewer"]) || it.Completion.CompletedBy.Type != "user" {
			t.Fatalf("%+v", it)
		}
	})

	t.Run("revocation takes effect for the same token at the next request", func(t *testing.T) {
		if r := e.revokeVia(adminTok, tenantA, reviewerBinding, reviewerETag); r.code != 204 {
			t.Fatalf("%d %s", r.code, r.body)
		}
		if r := e.complete(revTok, item2, etag2); r.code != 403 || r.errCode(t) != "forbidden" {
			t.Fatalf("revoked reviewer: %d %s", r.code, r.body)
		}
		if e.itemState(item2) != "review" {
			t.Fatal("the revoked completion changed the item")
		}
		if err := e.gateAllowed(e.users["reviewer"]); !errorsIs(err, identity.ErrDenied) {
			t.Fatalf("gate seam after revocation: %v", err)
		}
		// Through the merged consumer port, below the HTTP route gate.
		err := e.transitionDone(e.users["reviewer"], item2, etag2)
		if !errorsIs(err, work.ErrDenied) {
			t.Fatalf("work service after revocation: %v", err)
		}
		// Reading still works through the remaining viewer binding; the token itself is still valid.
		if r := e.do(revTok, http.MethodGet, item2, ""); r.code != 200 {
			t.Fatalf("read after revocation: %d", r.code)
		}
	})

	t.Run("the revocation is audited and announced with a higher effective version", func(t *testing.T) {
		events := e.stack.Outbox(tenantA, "membership.changed")
		var versions []int64
		for _, payload := range events {
			var env struct {
				Data struct {
					UserID           string `json:"userId"`
					EffectiveVersion int64  `json:"effectiveVersion"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(payload), &env); err != nil {
				t.Fatal(err)
			}
			if env.Data.UserID == string(e.users["reviewer"]) {
				versions = append(versions, env.Data.EffectiveVersion)
			}
		}
		// The seeded viewer binding is revision 1; the grant made 2 and the revocation 3.
		if len(versions) != 2 || versions[0] != 2 || versions[1] != 3 {
			t.Fatalf("effective versions of the reviewer: %v", versions)
		}
		if got := e.stack.CountIn(tenantA, `SELECT count(*) FROM audit WHERE tenant_id = $1 AND action = 'membership.revoked' AND resource_id = $2 AND actor_id = $3`,
			string(tenantA), reviewerBinding, string(e.users["admin"])); got != 1 {
			t.Fatalf("%d revocation audit rows", got)
		}
	})

	t.Run("removing the last binding hides the tenant; a regrant restores access with no stale denial", func(t *testing.T) {
		viewerBinding := e.stack.Count(`SELECT count(*) FROM memberships WHERE user_id = $1`, string(e.users["reviewer"]))
		if viewerBinding != 1 {
			t.Fatalf("%d bindings", viewerBinding)
		}
		var id string
		if err := e.stack.Owner.QueryRow(context.Background(), `SELECT id FROM memberships WHERE user_id = $1`, string(e.users["reviewer"])).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if r := e.revokeVia(adminTok, tenantA, id, `"1"`); r.code != 204 {
			t.Fatalf("%d %s", r.code, r.body)
		}
		if r := e.do(revTok, http.MethodGet, item3, ""); r.code != 404 {
			t.Fatalf("after the last revocation: %d %s", r.code, r.body)
		}
		if r := e.do(revTok, http.MethodGet, "/me", ""); r.code != 200 {
			t.Fatalf("the credential is still valid: %d", r.code)
		}
		e.grantVia(adminTok, tenantA, e.users["reviewer"], "", "reviewer")
		if r := e.complete(revTok, item3, etag3); r.code != 200 {
			t.Fatalf("after regrant: %d %s", r.code, r.body)
		}
	})
}

func (e *env) transitionDone(user identity.UserID, itemPath, etag string) error {
	e.t.Helper()
	id := itemPath[strings.LastIndex(itemPath, "/")+1:]
	v, err := httpapi.ParseETag(etag)
	if err != nil {
		e.t.Fatal(err)
	}
	_, err = e.work.Transition(context.Background(), work.TransitionCommand{
		Scope: domain.Scope{TenantID: tenantA, ProjectID: projectA}, Caller: work.Caller{PrincipalID: string(user)}, ID: domain.WorkItemID(id), Expected: v,
		Request: work.TransitionRequest{To: work.StateDone, Reason: "accepted", Completion: &work.CompletionClaim{ContentRevision: 1, CandidateCommit: commit, AcceptedCriteria: []string{"AC-1"}}},
	})
	return err
}

func errorsIs(err, target error) bool { return errors.Is(err, target) }

// Test_principal_discovery_scope: IT-01 / IT-11 / AC-04. /me, tenant and project
// discovery, and continuation pages disclose only the authenticated principal's
// authorized scopes; caller-supplied identity selectors and copied cursors cannot
// widen access. Every person has an independently issued real token set and a
// separate database membership fixture.
func Test_principal_discovery_scope(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"admin", "engineer", "viewer", "foreign", "outsider"} {
		e.signIn(p)
	}
	e.bind("admin", tenantA, "", identity.RoleAdmin)
	e.bind("foreign", tenantB, "", identity.RoleViewer)
	adminTok := e.tokens["admin"]
	// The engineer administers one project only; the viewer reads one project.
	e.grantVia(adminTok, tenantA, e.users["engineer"], projectA, "admin")
	e.grantVia(adminTok, tenantA, e.users["viewer"], projectA2, "viewer")
	for i := 0; i < 5; i++ {
		if r := e.do(adminTok, http.MethodPost, "/tenants/"+tenantA+"/projects", fmt.Sprintf(`{"key":"extra%d"}`, i)); r.code != 201 {
			t.Fatalf("create project: %d %s", r.code, r.body)
		}
	}

	type page struct {
		Items []struct {
			Id string `json:"id"`
		} `json:"items"`
		NextCursor *string `json:"nextCursor"`
	}
	ids := func(p page) []string {
		var out []string
		for _, it := range p.Items {
			out = append(out, it.Id)
		}
		return out
	}
	walk := func(token, path string, limit int) (all []string, cursors []string) {
		cursor := ""
		for i := 0; i < 20; i++ {
			q := fmt.Sprintf("%s?limit=%d", path, limit)
			if cursor != "" {
				q += "&cursor=" + cursor
			}
			r := e.do(token, http.MethodGet, q, "")
			if r.code != 200 {
				t.Fatalf("%s: %d %s", q, r.code, r.body)
			}
			var p page
			r.json(t, &p)
			all = append(all, ids(p)...)
			if p.NextCursor == nil {
				return all, cursors
			}
			cursor = *p.NextCursor
			cursors = append(cursors, cursor)
		}
		t.Fatal("pagination did not terminate")
		return nil, nil
	}

	t.Run("/me is the verified caller and nobody else", func(t *testing.T) {
		for _, p := range []string{"admin", "engineer", "viewer", "foreign", "outsider"} {
			r := e.do(e.tokens[p], http.MethodGet, "/me", "")
			var me struct{ Id, Subject string }
			r.json(t, &me)
			if r.code != 200 || me.Subject != oidctest.Subject(p) || me.Id != string(e.users[p]) {
				t.Errorf("%s: %d %s", p, r.code, r.body)
			}
		}
		// A second, independently issued token set of the same person is the same person.
		again := e.fx.Login("engineer")
		if again.Access == e.tokens["engineer"] {
			t.Fatal("tokens are not independent")
		}
		var me struct{ Id string }
		e.do(again.Access, http.MethodGet, "/me", "").json(t, &me)
		if me.Id != string(e.users["engineer"]) {
			t.Fatalf("a second login created another user: %s", me.Id)
		}
		// Caller-supplied selectors never redirect it.
		for _, q := range []string{"?userId=" + string(e.users["admin"]), "?subject=" + oidctest.Subject("admin"), "?tenantId=" + tenantA} {
			if r := e.do(e.tokens["outsider"], http.MethodGet, "/me"+q, ""); r.code != 400 {
				t.Errorf("/me%s: %d", q, r.code)
			}
		}
		r := e.do(e.tokens["outsider"], http.MethodGet, "/me", "", "X-User-Id", string(e.users["admin"]), "X-Forwarded-User", oidctest.Subject("admin"))
		var got struct{ Id string }
		r.json(t, &got)
		if got.Id != string(e.users["outsider"]) {
			t.Fatalf("a header selected another user: %s", got.Id)
		}
	})

	t.Run("tenant discovery shows only the caller's tenants", func(t *testing.T) {
		want := map[string][]string{"admin": {tenantA}, "engineer": {tenantA}, "viewer": {tenantA}, "foreign": {tenantB}, "outsider": nil}
		for p, w := range want {
			got, _ := walk(e.tokens[p], "/tenants", 1)
			if fmt.Sprint(got) != fmt.Sprint(w) && !(len(got) == 0 && len(w) == 0) {
				t.Errorf("%s discovers %v, want %v", p, got, w)
			}
		}
		for _, q := range []string{"?userId=" + string(e.users["admin"]), "?tenantId=" + tenantA, "?principal=" + string(e.users["admin"])} {
			if r := e.do(e.tokens["outsider"], http.MethodGet, "/tenants"+q, ""); r.code != 400 {
				t.Errorf("/tenants%s: %d", q, r.code)
			}
		}
	})

	t.Run("project discovery and continuation pages follow each person's scope", func(t *testing.T) {
		base := "/tenants/" + tenantA + "/projects"
		all, _ := walk(adminTok, base, 2)
		if len(all) != 7 {
			t.Fatalf("tenant administrator sees %d projects: %v", len(all), all)
		}
		if got, _ := walk(e.tokens["engineer"], base, 2); fmt.Sprint(got) != fmt.Sprint([]string{projectA}) {
			t.Fatalf("project administrator sees %v", got)
		}
		if got, _ := walk(e.tokens["viewer"], base, 1); fmt.Sprint(got) != fmt.Sprint([]string{projectA2}) {
			t.Fatalf("project viewer sees %v", got)
		}
		for _, p := range []string{"foreign", "outsider"} {
			if r := e.do(e.tokens[p], http.MethodGet, base, ""); r.code != 404 {
				t.Errorf("%s: %d %s", p, r.code, r.body)
			}
		}
	})

	t.Run("copied cursors cannot widen access", func(t *testing.T) {
		base := "/tenants/" + tenantA + "/projects"
		_, cursors := walk(adminTok, base, 2)
		if len(cursors) == 0 {
			t.Fatal("no continuation cursor")
		}
		for _, p := range []string{"engineer", "viewer", "foreign", "outsider"} {
			r := e.do(e.tokens[p], http.MethodGet, base+"?limit=2&cursor="+cursors[0], "")
			if r.code != 400 && r.code != 404 {
				t.Errorf("%s used the administrator's project cursor: %d %s", p, r.code, r.body)
			}
			if r.code == 400 && r.errCode(t) != "invalid_cursor" {
				t.Errorf("%s: %s", p, r.body)
			}
			if strings.Contains(r.body, `"p_`) && r.code == 200 {
				t.Errorf("%s was served content from a copied cursor", p)
			}
		}
		// The same cursor under another route, tenant or limit-independent scope is invalid too.
		if r := e.do(adminTok, http.MethodGet, "/tenants/"+tenantA+"/memberships?limit=2&cursor="+cursors[0], ""); r.code != 400 {
			t.Errorf("project cursor on the membership list: %d", r.code)
		}
		// Tenant discovery cursors are bound to their person as well.
		e.bind("outsider", tenantB, "", identity.RoleViewer)
		e.bind("outsider", tenantA, projectA, identity.RoleViewer)
		_, tcur := walk(e.tokens["outsider"], "/tenants", 1)
		if len(tcur) == 0 {
			t.Fatal("no tenant cursor")
		}
		if r := e.do(e.tokens["foreign"], http.MethodGet, "/tenants?limit=1&cursor="+tcur[0], ""); r.code != 400 || r.errCode(t) != "invalid_cursor" {
			t.Errorf("tenant cursor copied across people: %d %s", r.code, r.body)
		}
		// A page is authorized again: after losing access the owner of a valid cursor gets nothing.
		e.stack.OwnerExec(`DELETE FROM memberships WHERE user_id = $1`, string(e.users["engineer"]))
		_, cur := walk(adminTok, base, 3)
		e.stack.OwnerExec(`DELETE FROM memberships WHERE user_id = $1`, string(e.users["admin"]))
		if r := e.do(adminTok, http.MethodGet, base+"?limit=3&cursor="+cur[0], ""); r.code != 404 {
			t.Errorf("a revoked administrator continued a page: %d %s", r.code, r.body)
		}
	})

	t.Run("membership lists reveal only administered scopes", func(t *testing.T) {
		e.bind("admin", tenantA, "", identity.RoleAdmin)
		e.grantVia(adminTok, tenantA, e.users["engineer"], projectA, "admin")
		r := e.do(e.tokens["engineer"], http.MethodGet, "/tenants/"+tenantA+"/memberships", "")
		var mp struct{ Items []struct{ ProjectId *string } }
		r.json(t, &mp)
		if r.code != 200 || len(mp.Items) == 0 {
			t.Fatalf("%d %s", r.code, r.body)
		}
		for _, m := range mp.Items {
			if m.ProjectId == nil || *m.ProjectId != projectA {
				t.Errorf("project administrator sees %+v", m)
			}
		}
		if r := e.do(e.tokens["viewer"], http.MethodGet, "/tenants/"+tenantA+"/memberships", ""); r.code != 403 {
			t.Errorf("viewer: %d", r.code)
		}
		if r := e.do(e.tokens["foreign"], http.MethodGet, "/tenants/"+tenantA+"/memberships", ""); r.code != 404 {
			t.Errorf("foreign: %d", r.code)
		}
	})
}
