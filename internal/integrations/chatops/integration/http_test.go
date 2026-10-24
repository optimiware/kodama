package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	identityhttp "github.com/optimiware/kodama/internal/identity/http"
	"github.com/optimiware/kodama/internal/integrations/chatops"
	"github.com/optimiware/kodama/internal/integrations/chatops/chatopstest"
	chatopshttp "github.com/optimiware/kodama/internal/integrations/chatops/http"
)

// bearerUser maps "Authorization: Bearer <userId>" to that user, standing in for
// the OIDC authenticator the identity module owns.
type bearerUser struct{}

func (bearerUser) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: tok}, nil
}

type api struct {
	*httptest.Server
	admin, viewer, stranger string
}

// newAPI serves the real handlers over the real store, with the real identity
// authority behind both the router rules and the store's transaction-bound check.
func newAPI(t *testing.T, s *stack) *api {
	t.Helper()
	authority, err := identity.NewStandaloneAuthority(s.Identity)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := identity.SelectAuthority(identity.AuthorityConfig{Profile: identity.ProfileStandalone, Standalone: authority})
	if err != nil {
		t.Fatal(err)
	}
	rules := identityhttp.DefaultRules()
	for a, r := range chatopshttp.Rules() {
		rules[a] = r
	}
	authz, err := identityhttp.NewAuthorizer(selected, rules)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: bearerUser{}, Authorizer: authz, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	codec, err := httpapi.NewCursorCodec([]byte("t041-integration-cursor-key-000000000000"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := chatopshttp.NewHandler(s.Svc, codec)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RegisterRoutes(rt); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	return &api{Server: srv, admin: s.Actors.Admin.Actor.ID, viewer: s.Actors.Viewer.Actor.ID, stranger: string(s.User("stranger-http"))}
}

func (a *api) do(t *testing.T, method, path, who string, headers map[string]string, body any) (int, http.Header, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.URL+"/api/v1/tenants/"+string(scopeA.TenantID)+"/projects/"+string(scopeA.ProjectID)+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if who != "" {
		req.Header.Set("Authorization", "Bearer "+who)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, out
}

// TestHTTPOverRealStack: the management routes over the real router rules, the
// real transaction-bound authority, the real store and migrations, and the real
// worker: configuration is settings-only, inspection is read-only and sanitized,
// replay needs If-Match and a reason, and a stranger learns nothing. PostgreSQL and
// NATS; no S3 or Git.
func TestHTTPOverRealStack(t *testing.T) {
	s := newStack(t, chatopstest.Status(503))
	a := newAPI(t, s)
	match := func(v int64) map[string]string {
		return map[string]string{"If-Match": fmt.Sprintf("%q", fmt.Sprint(v))}
	}

	// Configuration is project:settings only.
	body := map[string]any{"name": "http-channel", "endpoint": s.RC.URL(), "events": []string{"gate.requested", "gate.decided"}}
	if code, _, _ := a.do(t, http.MethodPost, "/chatops-destinations", a.viewer, nil, body); code != http.StatusForbidden {
		t.Fatalf("viewer create = %d", code)
	}
	if code, _, _ := a.do(t, http.MethodPost, "/chatops-destinations", a.stranger, nil, body); code != http.StatusNotFound {
		t.Fatalf("stranger create = %d", code)
	}
	if code, _, _ := a.do(t, http.MethodPost, "/chatops-destinations", "", nil, body); code != http.StatusUnauthorized {
		t.Fatalf("anonymous create = %d", code)
	}
	code, hdr, raw := a.do(t, http.MethodPost, "/chatops-destinations", a.admin, nil, body)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, raw)
	}
	var dest chatopshttp.Destination
	if err := json.Unmarshal(raw, &dest); err != nil || dest.Version != 1 || hdr.Get("ETag") != `"1"` || !dest.Enabled || dest.BindingRevision != 1 {
		t.Fatalf("destination = %+v %q %v", dest, hdr.Get("ETag"), err)
	}
	if code, _, raw := a.do(t, http.MethodPost, "/chatops-destinations", a.admin, nil, body); code != http.StatusConflict || !strings.Contains(string(raw), "name_taken") {
		t.Fatalf("duplicate name = %d %s", code, raw)
	}
	if code, _, raw := a.do(t, http.MethodPost, "/chatops-destinations", a.admin, nil, map[string]any{"name": "x", "endpoint": "https://127.0.0.1:1/h", "events": []string{"gate.requested"}}); code != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "destination_refused") {
		t.Fatalf("private endpoint = %d %s", code, raw)
	}
	if code, _, _ := a.do(t, http.MethodGet, "/chatops-destinations/"+string(dest.Id), a.viewer, nil, nil); code != http.StatusForbidden {
		t.Fatalf("viewer read configuration = %d", code)
	}
	if code, _, raw := a.do(t, http.MethodGet, "/chatops-destinations/"+string(dest.Id), a.admin, nil, nil); code != http.StatusOK || !strings.Contains(string(raw), s.RC.URL()) {
		t.Fatalf("admin read configuration = %d %s", code, raw)
	}

	// A review event becomes a delivery that fails against the outage and is visible.
	s.Append(chatopstest.GateRequested(scopeA, "evt_http_1", "gate_1", 1, 'a'))
	s.Bus.Pump(t, 15*time.Second, "the intents", func() bool {
		return s.Count(`SELECT count(*) FROM chatops_deliveries WHERE destination_id = $1`, string(dest.Id)) == 1
	})
	w := s.Worker(s.Client(s.RC))
	did := chatops.DeliveryIDFor(scopeA, chatops.DestinationID(dest.Id), "evt_http_1")
	drain(t, w, 15*time.Second, "exhaustion", func() bool { return s.Delivery(scopeA, did).State == chatops.StateFailed })

	if code, _, _ := a.do(t, http.MethodGet, "/chatops-deliveries", a.stranger, nil, nil); code != http.StatusNotFound {
		t.Fatalf("stranger list = %d", code)
	}
	code, _, raw = a.do(t, http.MethodGet, "/chatops-deliveries?state=failed&destinationId="+string(dest.Id), a.viewer, nil, nil)
	var page chatopshttp.DeliveryPage
	if code != http.StatusOK || json.Unmarshal(raw, &page) != nil || len(page.Items) != 1 || string(page.Items[0].Id) != string(did) {
		t.Fatalf("list = %d %s", code, raw)
	}
	code, hdr, raw = a.do(t, http.MethodGet, "/chatops-deliveries/"+string(did), a.viewer, nil, nil)
	var detail chatopshttp.DeliveryDetail
	if code != http.StatusOK || json.Unmarshal(raw, &detail) != nil || detail.State != "failed" || len(detail.AttemptHistory) != chatopstest.FastRetry().MaxAttempts {
		t.Fatalf("detail = %d %s", code, raw)
	}
	for _, leak := range []string{s.RC.URL(), "hooks", "payload", "diagnostic", "Bearer"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("the sanitized detail leaks %q: %s", leak, raw)
		}
	}
	version := int64(detail.Version)
	if hdr.Get("ETag") != fmt.Sprintf("%q", fmt.Sprint(version)) {
		t.Fatalf("etag = %q", hdr.Get("ETag"))
	}

	// Replay: settings only, If-Match and a reason; one replay however many requests.
	reason := map[string]any{"reason": "receiver is back"}
	s.RC.SetBehavior(chatopstest.Accept("rcpt-http"))
	if code, _, _ := a.do(t, http.MethodPost, "/chatops-deliveries/"+string(did)+"/replay", a.viewer, match(version), reason); code != http.StatusForbidden {
		t.Fatalf("viewer replay = %d", code)
	}
	if code, _, _ := a.do(t, http.MethodPost, "/chatops-deliveries/"+string(did)+"/replay", a.admin, nil, reason); code != http.StatusPreconditionRequired {
		t.Fatalf("replay without If-Match = %d", code)
	}
	if code, _, _ := a.do(t, http.MethodPost, "/chatops-deliveries/"+string(did)+"/replay", a.admin, match(version), map[string]any{"reason": " "}); code != http.StatusBadRequest {
		t.Fatalf("replay without a reason = %d", code)
	}
	if code, _, _ := a.do(t, http.MethodPost, "/chatops-deliveries/"+string(did)+"/replay", a.admin, match(version+5), reason); code != http.StatusPreconditionFailed {
		t.Fatalf("stale replay = %d", code)
	}
	if code, _, raw := a.do(t, http.MethodPost, "/chatops-deliveries/"+string(did)+"/replay", a.admin, match(version), reason); code != http.StatusOK || !strings.Contains(string(raw), `"state":"pending"`) {
		t.Fatalf("replay = %d %s", code, raw)
	}
	if code, _, _ := a.do(t, http.MethodPost, "/chatops-deliveries/"+string(did)+"/replay", a.admin, match(version), reason); code != http.StatusPreconditionFailed {
		t.Fatalf("a duplicate replay = %d, want the version conflict", code)
	}
	drain(t, w, 10*time.Second, "the replayed delivery", func() bool { return s.Delivery(scopeA, did).State == chatops.StateDelivered })
	if code, _, raw := a.do(t, http.MethodPost, "/chatops-deliveries/"+string(did)+"/replay", a.admin, match(int64(s.Delivery(scopeA, did).Version)), reason); code != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "invalid_transition") {
		t.Fatalf("replay of a delivered notification = %d %s", code, raw)
	}

	// Disabling is a versioned edit and stops new intents.
	code, _, raw = a.do(t, http.MethodPatch, "/chatops-destinations/"+string(dest.Id), a.admin, match(1), map[string]any{"enabled": false})
	if code != http.StatusOK || !strings.Contains(string(raw), `"enabled":false`) {
		t.Fatalf("disable = %d %s", code, raw)
	}
	if code, _, _ := a.do(t, http.MethodPatch, "/chatops-destinations/"+string(dest.Id), a.admin, match(1), map[string]any{"enabled": true}); code != http.StatusPreconditionFailed {
		t.Fatalf("stale edit = %d", code)
	}
	s.Append(chatopstest.GateDecided(scopeA, "evt_http_2", "gate_1", 2, 'a'))
	s.Bus.Pump(t, 15*time.Second, "the second event consumed", func() bool {
		return s.CountTenant(scopeA.TenantID, `SELECT count(*) FROM event_inbox WHERE consumer = 'chatops' AND event_id = 'evt_http_2' AND state = 'completed'`) == 1
	})
	if n := s.Count(`SELECT count(*) FROM chatops_deliveries WHERE destination_id = $1`, string(dest.Id)); n != 1 {
		t.Fatalf("a disabled destination received a new intent (%d deliveries)", n)
	}
}
