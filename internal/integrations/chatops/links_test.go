package chatops_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/integrations/chatops"
)

// gateTarget is a module-local authenticated target fixture. It stands in for
// the real gate route (`GET P/gates/{id}`) only so the emitted link can be
// followed: it reloads the current record and the *follower's* current
// authorization on every request. It is not a gate implementation and proves
// nothing about the real producer, which T-046/T-044 verify.
type gateTarget struct {
	mu      sync.Mutex
	members map[string]map[string]bool // user -> permission set (project members only)
	gate    gateRecord
	decided int
}

type gateRecord struct {
	ID              string `json:"id"`
	Version         int64  `json:"version"`
	State           string `json:"state"` // pending | approved | rejected (the published gate states)
	SubjectDigest   string `json:"subjectDigest"`
	ContentRevision int64  `json:"contentRevision"`
	Deadline        string `json:"deadline"`
	Expired         bool   `json:"expired"`
	// Invalidation is applicability metadata, not a state: an invalidated gate
	// keeps its state and records when and why it stopped applying.
	Invalidation *gateInvalidation `json:"invalidation,omitempty"`
	// CanDecide is computed per request from the follower's current authority
	// and the record's current applicability. It is information, not a grant.
	CanDecide bool `json:"canDecide"`
}

type gateInvalidation struct {
	At                  string `json:"at"`
	Reason              string `json:"reason"`
	SupersedingRevision int64  `json:"supersedingRevision"`
}

func (g *gateTarget) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: tok}, nil
}

// Authorize reads current membership on every call.
func (g *gateTarget) Authorize(_ context.Context, p httpapi.Principal, action httpapi.Action, _ httpapi.Target) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	perms, member := g.members[p.ID]
	switch {
	case !member:
		return httpapi.ErrHidden
	case action == "gate.decide" && !perms["gate:decide"]:
		return httpapi.ErrDenied
	}
	return nil
}

func (g *gateTarget) current(now time.Time, who string) gateRecord {
	rec := g.gate
	if dl, err := time.Parse(time.RFC3339, rec.Deadline); err == nil && !now.Before(dl) {
		rec.Expired = true
	}
	rec.CanDecide = g.members[who]["gate:decide"] && rec.State == "pending" && !rec.Expired && rec.Invalidation == nil
	return rec
}

func (g *gateTarget) get(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	if err := httpapi.CheckQuery(r); err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	g.mu.Lock()
	rec := g.current(time.Now().UTC(), c.Principal.ID)
	g.mu.Unlock()
	if rec.ID != r.PathValue("id") {
		httpapi.WriteError(w, r, httpapi.NotFoundError())
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, rec)
}

// decide applies a decision only against the record as it is now: the digest the
// caller presents (for example from an old notification) must equal the current
// subject, and the gate must be pending, unexpired and not invalidated. A refusal
// uses the published `409 stale_gate_subject` convention and leaves the gate as
// it was.
func (g *gateTarget) decide(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	var req struct {
		SubjectDigest string `json:"subjectDigest"`
	}
	if err := httpapi.DecodeJSONBytes(body, &req); err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	rec := g.current(time.Now().UTC(), c.Principal.ID)
	if req.SubjectDigest != rec.SubjectDigest || rec.State != "pending" || rec.Expired || rec.Invalidation != nil {
		httpapi.WriteError(w, r, httpapi.NewError(http.StatusConflict, "stale_gate_subject", "the gate no longer applies to that subject"))
		return
	}
	g.gate.State, g.gate.Version = "approved", g.gate.Version+1
	g.decided++
	httpapi.WriteJSON(w, r, http.StatusOK, g.current(time.Now().UTC(), c.Principal.ID))
}

type fixtureServer struct {
	*httptest.Server
	target *gateTarget
	links  chatops.LinkBuilder
}

func newGateTarget(t *testing.T) *fixtureServer {
	t.Helper()
	g := &gateTarget{
		members: map[string]map[string]bool{
			"alice": {"gate:decide": true, "project:read": true}, // the reviewer the notification was meant for
			"bob":   {"project:read": true},                      // a colleague the link is forwarded to
		},
		gate: gateRecord{ID: "gate_1", Version: 1, State: "pending", SubjectDigest: "sha256:" + strings.Repeat("a", 64), ContentRevision: 3,
			Deadline: time.Now().UTC().Add(time.Hour).Format(time.RFC3339)},
	}
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: g, Authorizer: g, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	const base = "/tenants/{tenantId}/projects/{projectId}/gates/{id}"
	for _, r := range []httpapi.Route{
		{Method: http.MethodGet, Path: base, Action: "gate.read", ResourceKind: "gate", ResourceParam: "id", Handler: g.get},
		{Method: http.MethodPost, Path: base + "/decisions", Action: "gate.decide", ResourceKind: "gate", ResourceParam: "id", Handler: g.decide},
	} {
		if err := rt.Handle(r); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewTLSServer(rt)
	t.Cleanup(srv.Close)
	links, err := chatops.NewLinkBuilder(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &fixtureServer{Server: srv, target: g, links: links}
}

func (f *fixtureServer) do(t *testing.T, method, rawURL, who string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, rawURL, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if who != "" {
		req.Header.Set("Authorization", "Bearer "+who)
	}
	resp, err := f.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (f *fixtureServer) record(t *testing.T, rawURL, who string) gateRecord {
	t.Helper()
	code, body := f.do(t, http.MethodGet, rawURL, who, nil)
	if code != http.StatusOK {
		t.Fatalf("GET as %s = %d %s", who, code, body)
	}
	var rec gateRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

var gateScope = domain.Scope{TenantID: "t041_acme", ProjectID: "p041_inventory"}

func gateEvent(subject byte) eventsv1.Envelope {
	data, _ := json.Marshal(eventsv1.GateRequested{SubjectDigest: "sha256:" + strings.Repeat(string(subject), 64), ContentRevision: 3, ReviewerScope: "project", Deadline: "2027-01-01T00:00:00Z"})
	return eventsv1.Envelope{ID: "evt_gate_1", SchemaVersion: 1, Type: eventsv1.TypeGateRequested, OccurredAt: t0, TenantID: gateScope.TenantID, ProjectID: gateScope.ProjectID,
		Resource: eventsv1.Resource{Type: "gate", ID: "gate_1", Version: 1}, Actor: eventsv1.Actor{Type: eventsv1.ActorUser, ID: "usr_engineer"},
		CorrelationID: "req_gate_1", Data: data}
}

// emitted builds the notification payload of the gate event exactly as the
// planner would and returns its link.
func emitted(t *testing.T, f *fixtureServer) (chatops.Payload, string) {
	t.Helper()
	p, err := chatops.Project(gateEvent('a'), chatops.DeliveryIDFor(gateScope, "cdst_t041_1", "evt_gate_1"), f.links)
	if err != nil {
		t.Fatal(err)
	}
	return p, p.URL
}

// Test_forwarded_review_link: the emitted URL contains only scoped resource
// identity; following it uses the current follower's authorization, never the
// originating actor's, and a GET cannot approve. The real gate target is T-046/
// T-044; this proves the link grants no authority.
func Test_forwarded_review_link(t *testing.T) {
	f := newGateTarget(t)
	payload, link := emitted(t, f)

	t.Run("the link names only scoped resource identity", func(t *testing.T) {
		u, err := url.Parse(link)
		if err != nil {
			t.Fatal(err)
		}
		want := "/api/v1/tenants/t041_acme/projects/p041_inventory/gates/gate_1"
		if u.Path != want || u.RawQuery != "" || u.Fragment != "" || u.User != nil || u.Scheme != "https" {
			t.Fatalf("link = %q", link)
		}
		if u.Host != strings.TrimPrefix(f.URL, "https://") {
			t.Fatalf("link host %q is not the configured canonical origin", u.Host)
		}
		raw, err := payload.Encode()
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"usr_engineer", "Bearer", "token", "password", "secret", "signature"} {
			if strings.Contains(strings.ToLower(string(raw)), strings.ToLower(secret)) {
				t.Fatalf("the payload mentions %q: %s", secret, raw)
			}
		}
	})

	t.Run("each follower is authenticated and authorized as themselves", func(t *testing.T) {
		if code, _ := f.do(t, http.MethodGet, link, "", nil); code != http.StatusUnauthorized {
			t.Fatalf("an unauthenticated follower got %d", code)
		}
		if code, _ := f.do(t, http.MethodGet, link, "mallory", nil); code != http.StatusNotFound {
			t.Fatalf("a non-member got %d; the record must be hidden", code)
		}
		alice := f.record(t, link, "alice")
		bob := f.record(t, link, "bob")
		if !alice.CanDecide || bob.CanDecide {
			t.Fatalf("forwarding changed authority: alice=%v bob=%v", alice.CanDecide, bob.CanDecide)
		}
		// Possession of the link is not a permission: bob cannot decide.
		if code, _ := f.do(t, http.MethodPost, link+"/decisions", "bob", map[string]string{"subjectDigest": payload.Attributes["subjectDigest"]}); code != http.StatusForbidden {
			t.Fatalf("a forwarded link decided for another actor: %d", code)
		}
		if code, _ := f.do(t, http.MethodPost, link+"/decisions", "mallory", map[string]string{"subjectDigest": payload.Attributes["subjectDigest"]}); code != http.StatusNotFound {
			t.Fatalf("a stranger's decision got %d", code)
		}
	})

	t.Run("a GET can never approve", func(t *testing.T) {
		before := f.record(t, link, "alice")
		for _, rawURL := range []string{link + "?decision=approved", link + "?approve=1&subjectDigest=" + url.QueryEscape(payload.Attributes["subjectDigest"])} {
			if code, _ := f.do(t, http.MethodGet, rawURL, "alice", nil); code != http.StatusBadRequest {
				t.Fatalf("a decision-bearing query was accepted: %d", code)
			}
		}
		for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
			if code, _ := f.do(t, method, link, "alice", nil); code == http.StatusOK {
				t.Fatalf("%s on the review link succeeded", method)
			}
		}
		after := f.record(t, link, "alice")
		if after.State != "pending" || after.Version != before.Version || f.target.decided != 0 {
			t.Fatalf("the record changed through GET/other verbs: %+v -> %+v", before, after)
		}
	})

	t.Run("authority is read at follow time", func(t *testing.T) {
		f.target.mu.Lock()
		delete(f.target.members, "alice") // alice is offboarded after receiving the notification
		f.target.mu.Unlock()
		if code, _ := f.do(t, http.MethodGet, link, "alice", nil); code != http.StatusNotFound {
			t.Fatalf("a revoked reviewer still followed the link: %d", code)
		}
		if code, _ := f.do(t, http.MethodPost, link+"/decisions", "alice", map[string]string{"subjectDigest": payload.Attributes["subjectDigest"]}); code != http.StatusNotFound {
			t.Fatalf("a revoked reviewer decided: %d", code)
		}
		if f.target.decided != 0 {
			t.Fatal("a decision was recorded for a revoked reviewer")
		}
	})
}

// Test_changed_subject_link: the same emitted URL reloads the fixture's changed
// current record, including invalidation and expiry metadata; the historical
// notification content cannot authorize a decision. Real subject applicability is
// T-046/T-044.
func Test_changed_subject_link(t *testing.T) {
	f := newGateTarget(t)
	payload, link := emitted(t, f)
	staleDigest := payload.Attributes["subjectDigest"]

	first := f.record(t, link, "alice")
	if first.State != "pending" || first.SubjectDigest != staleDigest || !first.CanDecide {
		t.Fatalf("initial record = %+v", first)
	}

	// The subject changes: the gate is invalidated by a superseding revision. Its
	// state stays pending; invalidation is applicability metadata.
	f.target.mu.Lock()
	f.target.gate.Version = 2
	f.target.gate.Invalidation = &gateInvalidation{At: t0.Format(time.RFC3339), Reason: "content_changed", SupersedingRevision: 4}
	f.target.mu.Unlock()

	changed := f.record(t, link, "alice") // the very same URL as emitted
	if changed.State != "pending" || changed.Invalidation == nil || changed.Invalidation.Reason != "content_changed" || changed.Invalidation.SupersedingRevision != 4 ||
		changed.CanDecide || changed.Version != 2 {
		t.Fatalf("the link did not reload the changed record: %+v", changed)
	}
	// The historical notification content is not an authority: presenting its
	// subject digest to the decision route is refused.
	code, body := f.do(t, http.MethodPost, link+"/decisions", "alice", map[string]string{"subjectDigest": staleDigest})
	if code != http.StatusConflict || !strings.Contains(string(body), "stale_gate_subject") {
		t.Fatalf("the stale subject decided: %d %s", code, body)
	}

	// Re-requested on a new subject: the old URL now shows the new subject and the
	// old notification's digest still cannot decide.
	newDigest := "sha256:" + strings.Repeat("b", 64)
	f.target.mu.Lock()
	f.target.gate.Version, f.target.gate.SubjectDigest, f.target.gate.ContentRevision, f.target.gate.Invalidation = 3, newDigest, 4, nil
	f.target.mu.Unlock()
	renewed := f.record(t, link, "alice")
	if renewed.SubjectDigest != newDigest || renewed.ContentRevision != 4 || !renewed.CanDecide {
		t.Fatalf("renewed = %+v", renewed)
	}
	if code, _ := f.do(t, http.MethodPost, link+"/decisions", "alice", map[string]string{"subjectDigest": staleDigest}); code != http.StatusConflict {
		t.Fatalf("an old notification's subject decided after renewal: %d", code)
	}
	if f.target.decided != 0 {
		t.Fatal("a historical notification authorized a decision")
	}

	// Expiry is an applicability check against the deadline, not a transition.
	f.target.mu.Lock()
	f.target.gate.Deadline = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	f.target.mu.Unlock()
	expired := f.record(t, link, "alice")
	if !expired.Expired || expired.CanDecide || expired.State != "pending" {
		t.Fatalf("expired = %+v", expired)
	}
	if code, _ := f.do(t, http.MethodPost, link+"/decisions", "alice", map[string]string{"subjectDigest": newDigest}); code != http.StatusConflict {
		t.Fatalf("an expired gate was decided: %d", code)
	}

	// The notification payload itself is unchanged history.
	if payload.Attributes["subjectDigest"] != staleDigest || payload.Resource.Version != 1 {
		t.Fatalf("history mutated: %+v", payload)
	}
}

// TestLinkBuilder pins the origin and resource rules.
func TestLinkBuilder(t *testing.T) {
	for _, bad := range []string{"", "http://api.example", "https://", "https://u:p@api.example", "https://api.example/prefix", "https://api.example?x=1", "https://api.example#f", "api.example", "https://api example"} {
		if _, err := chatops.NewLinkBuilder(bad); err == nil {
			t.Errorf("origin %q accepted", bad)
		}
	}
	lb, err := chatops.NewLinkBuilder("https://api.example:8443/")
	if err != nil {
		t.Fatal(err)
	}
	got, err := lb.URL(gateScope, "run", "run_9")
	if err != nil || got != "https://api.example:8443/api/v1/tenants/t041_acme/projects/p041_inventory/runs/run_9" {
		t.Fatalf("url = %q %v", got, err)
	}
	for _, kind := range []string{"", "usage_record", "membership", "workspace"} {
		if _, err := lb.URL(gateScope, kind, "x1"); err == nil {
			t.Errorf("kind %q produced a link", kind)
		}
	}
	for _, id := range []string{"", "a/b", "../x", "a b", "a?b"} {
		if _, err := lb.URL(gateScope, "gate", id); err == nil {
			t.Errorf("id %q produced a link", id)
		}
	}
	if _, err := (chatops.LinkBuilder{}).URL(gateScope, "gate", "g1"); err == nil {
		t.Error("the zero builder produced a link")
	}
}
