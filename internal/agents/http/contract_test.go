package agentshttp_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/agents"
	agentshttp "github.com/optimiware/kodama/internal/agents/http"
)

type profileBody struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Version   int64           `json:"version"`
	Reference string          `json:"reference"`
	Digest    string          `json:"digest"`
	Profile   json.RawMessage `json:"profile"`
	CreatedBy struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	} `json:"createdBy"`
	CreatedAt string `json:"createdAt"`
}

type pageBody struct {
	Items      []profileBody `json:"items"`
	NextCursor *string       `json:"nextCursor"`
}

// TestProfileHTTPContract verifies that the OpenAPI fragment and the handlers
// agree: the documented routes and nothing more, strict decoding, validation
// and authorization before any state, standard error mapping, content-free
// errors and scoped pagination. PostgreSQL-backed route tests are separate.
func TestProfileHTTPContract(t *testing.T) {
	sp := loadSpec(t)

	t.Run("the documented examples validate against the fragment's own schemas", func(t *testing.T) {
		if err := sp.validate(t, "AgentProfileDraft", sp.example(t, "ProfileDraft")); err != nil {
			t.Fatalf("draft example: %v", err)
		}
		if err := sp.validate(t, "AgentProfile", sp.example(t, "RegisteredProfile")); err != nil {
			t.Fatalf("registered example: %v", err)
		}
	})

	t.Run("the documented draft example registers and the response matches the schema", func(t *testing.T) {
		a := newAPI(t)
		draft, err := agents.DecodeDraft(sp.example(t, "ProfileDraft"))
		if err != nil {
			t.Fatalf("the documented example is not a valid draft shape: %v", err)
		}
		a.draft = draft
		a.approvals.AllowModel("example-model")
		// Rebuild the service with a catalogue covering the example's tuple.
		a = rebuild(t, a, draft)
		res := a.do("tok-engineer", http.MethodPost, base(tenantA, projectA), string(sp.example(t, "ProfileDraft")))
		if res.Code != http.StatusCreated {
			t.Fatalf("status %d: %s", res.Code, res.Body.String())
		}
		if err := sp.validate(t, "AgentProfile", res.Body.Bytes()); err != nil {
			t.Fatalf("the response violates the schema: %v\n%s", err, res.Body.String())
		}
	})

	a := newAPI(t)
	good := a.body(nil)

	t.Run("registration returns an immutable version and a content-addressed duplicate", func(t *testing.T) {
		res := a.do("tok-engineer", http.MethodPost, base(tenantA, projectA), good)
		if res.Code != http.StatusCreated {
			t.Fatalf("status %d: %s", res.Code, res.Body.String())
		}
		if err := sp.validate(t, "AgentProfile", res.Body.Bytes()); err != nil {
			t.Fatalf("schema: %v\n%s", err, res.Body.String())
		}
		var p profileBody
		res.decode(t, &p)
		if p.Name != "coding" || p.Version != 1 || p.Reference != "coding@1" || p.CreatedBy.Type != "user" || p.CreatedBy.ID != "u_engineer" {
			t.Fatalf("body: %+v", p)
		}
		sum := sha256.Sum256(p.Profile)
		if p.Digest != "sha256:"+hex.EncodeToString(sum[:]) {
			t.Fatal("the digest must be recomputable from the returned canonical profile")
		}
		if res.Header().Get("ETag") != `"1"` || !strings.HasPrefix(res.Header().Get("Location"), "/api/v1"+base(tenantA, projectA)) ||
			res.Header().Get("Cache-Control") != "no-store" || res.Header().Get("X-Request-ID") == "" {
			t.Fatalf("headers: %v", res.Header())
		}
		dup := a.do("tok-engineer", http.MethodPost, base(tenantA, projectA), good)
		if dup.Code != http.StatusOK {
			t.Fatalf("an identical profile is 200, got %d: %s", dup.Code, dup.Body.String())
		}
		var d profileBody
		dup.decode(t, &d)
		if d.ID != p.ID || d.Version != 1 || len(a.repo.Versions()) != 1 {
			t.Fatal("a duplicate creates nothing")
		}
		changed := a.do("tok-engineer", http.MethodPost, base(tenantA, projectA), a.body(func(m map[string]any) { m["instructions"] = "Different instructions." }))
		if changed.Code != http.StatusCreated {
			t.Fatalf("changed content: %d %s", changed.Code, changed.Body.String())
		}
		var c profileBody
		changed.decode(t, &c)
		if c.Version != 2 || c.Reference != "coding@2" || changed.Header().Get("ETag") != `"2"` {
			t.Fatalf("the next version: %+v", c)
		}
	})

	t.Run("strict decoding rejects malformed bodies before any state", func(t *testing.T) {
		before := len(a.repo.Versions())
		raw := a.body(nil)
		for name, tc := range map[string]struct {
			body string
			want int
		}{
			"an unknown field":       {a.body(func(m map[string]any) { m["roles"] = []string{"reviewer"} }), 400},
			"an unknown nested one":  {a.body(func(m map[string]any) { m["limits"].(map[string]any)["maxCost"] = 1 }), 400},
			"a duplicate key":        {strings.Replace(raw, `"name":"coding"`, `"name":"coding","name":"other"`, 1), 400},
			"a nested duplicate key": {strings.Replace(raw, `"wallSeconds":1800`, `"wallSeconds":1800,"wallSeconds":1`, 1), 400},
			"trailing data":          {raw + ` {}`, 400},
			"a non-object":           {`[]`, 400},
			"not JSON":               {`not json`, 400},
			"a string for a number":  {a.body(func(m map[string]any) { m["limits"].(map[string]any)["wallSeconds"] = "1800" }), 400},
		} {
			res := a.do("tok-engineer", http.MethodPost, base(tenantA, projectA), tc.body)
			if res.Code != tc.want || res.errCode(t) != "invalid_request" {
				t.Errorf("%s: %d %s", name, res.Code, res.Body.String())
			}
		}
		if res := a.do("tok-engineer", http.MethodPost, base(tenantA, projectA), ""); res.Code != http.StatusUnsupportedMediaType && res.Code != http.StatusBadRequest {
			t.Errorf("an empty body: %d", res.Code)
		}
		req := a.do("tok-engineer", http.MethodPost, base(tenantA, projectA), raw, "Content-Type", "text/plain")
		if req.Code != http.StatusUnsupportedMediaType {
			t.Errorf("a non-JSON media type is 415, got %d", req.Code)
		}
		big := a.body(func(m map[string]any) { m["instructions"] = strings.Repeat("a", 2<<20) })
		if res := a.do("tok-engineer", http.MethodPost, base(tenantA, projectA), big); res.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("an oversized body is 413, got %d", res.Code)
		}
		if len(a.repo.Versions()) != before {
			t.Fatal("no malformed request may create a profile")
		}
	})

	t.Run("semantic failures are 422 with stable, content-free diagnostics", func(t *testing.T) {
		secret := "SUPERSECRET-INSTRUCTION-TEXT"
		for name, tc := range map[string]struct {
			mutate func(map[string]any)
			code   string
			diag   string
		}{
			"a mutable image": {func(m map[string]any) { m["image"] = "registry.example.invalid/a:latest"; m["instructions"] = secret }, "invalid_profile", "image"},
			"credentials": {func(m map[string]any) {
				m["model"].(map[string]any)["endpoint"] = "https://u:p@gateway.example.invalid/"
			}, "invalid_profile", "model.endpoint"},
			"a zero limit":        {func(m map[string]any) { m["limits"].(map[string]any)["wallSeconds"] = 0 }, "invalid_profile", "limits.wallSeconds"},
			"an unapproved tool":  {func(m map[string]any) { m["tools"] = []string{"repo.read", "shell.exec"} }, "invalid_profile", "tools"},
			"an unapproved model": {func(m map[string]any) { m["model"].(map[string]any)["name"] = "not-approved" }, "invalid_profile", "model"},
		} {
			res := a.do("tok-engineer", http.MethodPost, base(tenantA, projectA), a.body(tc.mutate))
			if res.Code != http.StatusUnprocessableEntity {
				t.Errorf("%s: %d %s", name, res.Code, res.Body.String())
				continue
			}
			e := res.errEnvelope(t)
			if e.Code != tc.code {
				t.Errorf("%s: code %s", name, e.Code)
			}
			if strings.Contains(res.Body.String(), secret) || strings.Contains(res.Body.String(), "u:p@") {
				t.Errorf("%s: an error must never echo profile content: %s", name, res.Body.String())
			}
			diags, _ := e.Details["diagnostics"].([]any)
			found := false
			for _, d := range diags {
				found = found || d.(map[string]any)["path"] == tc.diag
			}
			if !found {
				t.Errorf("%s: no diagnostic at %s: %s", name, tc.diag, res.Body.String())
			}
		}
	})

	t.Run("an unsupported wrapper is profile_unsupported and writes nothing", func(t *testing.T) {
		before := len(a.repo.Versions())
		res := a.do("tok-engineer", http.MethodPost, base(tenantA, projectA), a.body(func(m map[string]any) {
			m["wrapper"].(map[string]any)["version"] = "9.9.9"
		}))
		if res.Code != http.StatusUnprocessableEntity || res.errCode(t) != "profile_unsupported" {
			t.Fatalf("%d %s", res.Code, res.Body.String())
		}
		res = a.do("tok-engineer", http.MethodPost, base(tenantA, projectA), a.body(func(m map[string]any) {
			m["image"] = "registry.example.invalid/other@sha256:" + strings.Repeat("2", 64)
		}))
		if res.Code != http.StatusUnprocessableEntity || res.errCode(t) != "profile_unsupported" {
			t.Fatalf("a mismatched image: %d %s", res.Code, res.Body.String())
		}
		if len(a.repo.Versions()) != before {
			t.Fatal("nothing may be written")
		}
	})

	t.Run("authorization precedes validation and never discloses", func(t *testing.T) {
		before := len(a.repo.Versions())
		if res := a.do("", http.MethodPost, base(tenantA, projectA), good); res.Code != http.StatusUnauthorized {
			t.Errorf("no token: %d", res.Code)
		}
		if res := a.do("tok-stranger", http.MethodPost, base(tenantA, projectA), good); res.Code != http.StatusNotFound {
			t.Errorf("a stranger learns nothing: %d", res.Code)
		}
		if res := a.do("tok-stranger", http.MethodGet, base(tenantA, projectA), ""); res.Code != http.StatusNotFound {
			t.Errorf("a stranger learns nothing on read: %d", res.Code)
		}
		if res := a.do("tok-reader", http.MethodPost, base(tenantA, projectA), `{"broken":`); res.Code != http.StatusForbidden && res.Code != http.StatusBadRequest {
			t.Errorf("a reader may not register: %d", res.Code)
		}
		if res := a.do("tok-reader", http.MethodPost, base(tenantA, projectA), good); res.Code != http.StatusForbidden || res.errCode(t) != "forbidden" {
			t.Errorf("a reader may not register: %d", res.Code)
		}
		if res := a.do("tok-reader", http.MethodGet, base(tenantA, projectA), ""); res.Code != http.StatusOK {
			t.Errorf("a reader may list: %d", res.Code)
		}
		if res := a.do("tok-engineer", http.MethodGet, base(tenantA, "p030_other"), ""); res.Code != http.StatusNotFound {
			t.Errorf("another project: %d", res.Code)
		}
		if res := a.do("tok-engineer", http.MethodPost, base("t.x", projectA), good); res.Code != http.StatusBadRequest && res.Code != http.StatusNotFound {
			t.Errorf("a malformed scope: %d", res.Code)
		}
		if len(a.repo.Versions()) != before {
			t.Fatal("a refused request must write nothing")
		}
		if len(a.authz.calls) == 0 {
			t.Fatal("the router authorization must run")
		}
		for _, c := range a.authz.calls {
			if c != agentshttp.ActionList && c != agentshttp.ActionRegister {
				t.Errorf("unexpected route action %s", c)
			}
		}
	})

	t.Run("tenants and projects are isolated", func(t *testing.T) {
		res := a.do("tok-engineer", http.MethodPost, base(tenantB, projectB), good)
		if res.Code != http.StatusCreated {
			t.Fatalf("%d %s", res.Code, res.Body.String())
		}
		var page pageBody
		a.do("tok-engineer", http.MethodGet, base(tenantB, projectB), "").decode(t, &page)
		if len(page.Items) != 1 || page.Items[0].Version != 1 {
			t.Fatalf("tenant B has its own versions: %+v", page)
		}
	})

	t.Run("only the documented routes exist", func(t *testing.T) {
		for _, m := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
			if res := a.do("tok-engineer", m, base(tenantA, projectA), good); res.Code != http.StatusMethodNotAllowed && res.Code != http.StatusNotFound {
				t.Errorf("%s on the collection: %d", m, res.Code)
			}
		}
		for _, p := range []string{"/p1", "/coding@1", "/p1/sessions", "/p1/transcript", "/p1/continue", "/p1/versions"} {
			for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
				if res := a.do("tok-engineer", m, base(tenantA, projectA)+p, ""); res.Code != http.StatusNotFound && res.Code != http.StatusMethodNotAllowed {
					t.Errorf("%s %s must not exist: %d", m, p, res.Code)
				}
			}
		}
		paths := sp.raw["paths"].(map[string]any)
		if len(paths) != 1 {
			t.Fatalf("the fragment documents exactly one path, got %d", len(paths))
		}
		item := paths["/tenants/{tenantId}/projects/{projectId}/agent-profiles"].(map[string]any)
		for _, forbidden := range []string{"put", "patch", "delete"} {
			if _, ok := item[forbidden]; ok {
				t.Errorf("the fragment must not document %s", forbidden)
			}
		}
		if item["get"].(map[string]any)["operationId"] != "listAgentProfiles" || item["post"].(map[string]any)["operationId"] != "registerAgentProfile" {
			t.Fatal("operation IDs")
		}
	})

	t.Run("the generated draft type and the domain draft are the same wire shape", func(t *testing.T) {
		ex := sp.example(t, "ProfileDraft")
		var gen agentshttp.AgentProfileDraft
		dec := json.NewDecoder(strings.NewReader(string(ex)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&gen); err != nil {
			t.Fatalf("generated type: %v", err)
		}
		draft, err := agents.DecodeDraft(ex)
		if err != nil {
			t.Fatalf("domain draft: %v", err)
		}
		g, _ := json.Marshal(gen)
		d, _ := json.Marshal(draft)
		var gm, dm map[string]any
		_ = json.Unmarshal(g, &gm)
		_ = json.Unmarshal(d, &dm)
		// The domain draft always carries the baseline digest the server computes;
		// everything else must agree key for key.
		delete(dm["designBaseline"].(map[string]any), "digest")
		delete(gm["designBaseline"].(map[string]any), "digest")
		if !jsonEqual(gm, dm) {
			t.Fatalf("wire shapes differ:\n%s\n%s", g, d)
		}
	})
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// rebuild returns an API whose catalogue qualifies the given draft's tuple.
func rebuild(t *testing.T, old *api, draft agents.Draft) *api {
	t.Helper()
	a := newAPIWith(t, draft, old.approvals)
	return a
}

func TestProfilePagination(t *testing.T) {
	a := newAPI(t)
	for i := 0; i < 5; i++ {
		body := a.body(func(m map[string]any) { m["instructions"] = "Instructions " + string(rune('A'+i)) })
		if res := a.do("tok-engineer", http.MethodPost, base(tenantA, projectA), body); res.Code != http.StatusCreated {
			t.Fatalf("seed %d: %d %s", i, res.Code, res.Body.String())
		}
	}
	other := a.body(func(m map[string]any) { m["name"] = "review" })
	a.do("tok-engineer", http.MethodPost, base(tenantA, projectA), other)
	a.do("tok-engineer", http.MethodPost, base(tenantB, projectB), a.body(nil))

	collect := func(token, path string) ([]string, int) {
		var ids []string
		pages := 0
		next := path
		for next != "" {
			if pages > 20 {
				t.Fatal("pagination does not terminate")
			}
			res := a.do(token, http.MethodGet, next, "")
			if res.Code != http.StatusOK {
				t.Fatalf("page %d: %d %s", pages, res.Code, res.Body.String())
			}
			var page pageBody
			res.decode(t, &page)
			for _, it := range page.Items {
				ids = append(ids, it.ID)
			}
			pages++
			next = ""
			if page.NextCursor != nil {
				sep := "?"
				if strings.Contains(path, "?") {
					sep = "&"
				}
				next = path + sep + "cursor=" + *page.NextCursor
			}
		}
		return ids, pages
	}

	t.Run("pages cover every row exactly once", func(t *testing.T) {
		ids, pages := collect("tok-engineer", base(tenantA, projectA)+"?limit=2")
		seen := map[string]bool{}
		for _, id := range ids {
			if seen[id] {
				t.Fatalf("duplicate %s", id)
			}
			seen[id] = true
		}
		if len(ids) != 6 || pages != 3 {
			t.Fatalf("%d rows in %d pages", len(ids), pages)
		}
	})

	t.Run("the name filter narrows the list and is bound to its cursor", func(t *testing.T) {
		ids, _ := collect("tok-engineer", base(tenantA, projectA)+"?name=coding&limit=2")
		if len(ids) != 5 {
			t.Fatalf("filtered rows: %d", len(ids))
		}
		first := a.do("tok-engineer", http.MethodGet, base(tenantA, projectA)+"?name=coding&limit=2", "")
		var page pageBody
		first.decode(t, &page)
		if page.NextCursor == nil {
			t.Fatal("expected a continuation")
		}
		// Another filter, another project and another tenant must not accept it.
		for name, path := range map[string]string{
			"another filter":  base(tenantA, projectA) + "?name=review&cursor=" + *page.NextCursor,
			"no filter":       base(tenantA, projectA) + "?cursor=" + *page.NextCursor,
			"another tenant":  base(tenantB, projectB) + "?name=coding&cursor=" + *page.NextCursor,
			"a forged cursor": base(tenantA, projectA) + "?name=coding&cursor=" + *page.NextCursor + "x",
		} {
			res := a.do("tok-engineer", http.MethodGet, path, "")
			if res.Code != http.StatusBadRequest && res.Code != http.StatusNotFound {
				t.Errorf("%s: a foreign cursor must be refused, got %d", name, res.Code)
			}
		}
	})

	t.Run("invalid queries are rejected, never clamped", func(t *testing.T) {
		for _, q := range []string{"?limit=0", "?limit=201", "?limit=abc", "?name=Bad%20Name", "?name=a&name=b", "?unknown=1", "?cursor=bogus"} {
			res := a.do("tok-engineer", http.MethodGet, base(tenantA, projectA)+q, "")
			if res.Code != http.StatusBadRequest {
				t.Errorf("%s: %d %s", q, res.Code, res.Body.String())
			}
		}
	})

	t.Run("a cursor is authorized again on every page", func(t *testing.T) {
		first := a.do("tok-engineer", http.MethodGet, base(tenantA, projectA)+"?limit=2", "")
		var page pageBody
		first.decode(t, &page)
		if res := a.do("tok-stranger", http.MethodGet, base(tenantA, projectA)+"?limit=2&cursor="+*page.NextCursor, ""); res.Code != http.StatusNotFound {
			t.Fatalf("a cursor grants no permission: %d", res.Code)
		}
	})
}

func TestProfileErrorMapping(t *testing.T) {
	cases := map[string]struct {
		err    error
		status int
		code   string
	}{
		"conflict":    {agents.ErrConflict, http.StatusConflict, "profile_conflict"},
		"unavailable": {agents.Unavailablef(errors.New("db down"), "x"), http.StatusServiceUnavailable, "unavailable"},
		"unknown":     {agents.Unavailablef(agents.ErrOutcomeUnknown, "commit unknown"), http.StatusServiceUnavailable, "unavailable"},
		"not found":   {agents.ErrNotFound, http.StatusNotFound, "not_found"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a := newAPI(t)
			a.repo.Err = tc.err
			res := a.do("tok-engineer", http.MethodPost, base(tenantA, projectA), a.body(nil))
			if res.Code != tc.status || res.errCode(t) != tc.code {
				t.Fatalf("%d %s", res.Code, res.Body.String())
			}
			if strings.Contains(res.Body.String(), "db down") || strings.Contains(res.Body.String(), "SUPERSECRET") {
				t.Fatal("infrastructure detail reached the client")
			}
			if tc.status == http.StatusServiceUnavailable && res.Header().Get("Retry-After") == "" {
				t.Fatal("a 503 carries Retry-After")
			}
		})
	}
	t.Run("a handler requires its collaborators", func(t *testing.T) {
		if _, err := agentshttp.NewHandler(nil, nil); err == nil {
			t.Fatal("a service and a codec are required")
		}
	})
}
