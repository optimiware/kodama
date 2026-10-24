// Package factoryfake is a controlled double of the software factory's runtime
// deployment callbacks, the one boundary where this application's delivery tests
// use a fixture instead of the real service. It follows the published contract
// (api/delivery.yaml and the T-035 fixtures): registration idempotent by logical
// action, a recorded apply intent, structured outcomes, lookup by action key,
// the outcome_unknown refusal and revoked authority.
package factoryfake

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"example.invalid/inventory/internal/delivery"
	"example.invalid/inventory/test/support/clusterfake"
)

// Server is a running fake.
type Server struct {
	HTTP *httptest.Server
	Log  *clusterfake.Timeline

	mu       sync.Mutex
	contexts map[string]map[string]any // attemptId -> context document
	revoked  map[string]bool
	deps     map[string]*record // actionKey -> record
	drop     map[string]int     // route -> responses to lose after processing
	down     map[string]int     // route -> requests to refuse with 503 before processing
	revokeAt map[string]string  // route -> attempt revoked when that route is next called
	Calls    map[string]int
}

type record struct {
	rec         delivery.Record
	annotations map[string]string
	version     int
}

// New starts the fake.
func New(t testing.TB, log *clusterfake.Timeline) *Server {
	t.Helper()
	s := &Server{Log: log, contexts: map[string]map[string]any{}, revoked: map[string]bool{}, deps: map[string]*record{}, drop: map[string]int{}, down: map[string]int{}, revokeAt: map[string]string{}, Calls: map[string]int{}}
	s.HTTP = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.HTTP.Close)
	return s
}

// URL is the runtime origin.
func (s *Server) URL() string { return s.HTTP.URL }

// SetContext registers an attempt's immutable pinned context.
func (s *Server) SetContext(attemptID string, doc map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contexts[attemptID] = doc
}

// Revoke ends an attempt's authority.
func (s *Server) Revoke(attemptID string) { s.mu.Lock(); s.revoked[attemptID] = true; s.mu.Unlock() }

// RevokeAt revokes the attempt's authority just before the named route is next processed.
func (s *Server) RevokeAt(route, attemptID string) {
	s.mu.Lock()
	s.revokeAt[route] = attemptID
	s.mu.Unlock()
}

// LoseResponse makes the next n responses of a route vanish after the request was processed.
func (s *Server) LoseResponse(route string, n int) { s.mu.Lock(); s.drop[route] = n; s.mu.Unlock() }

// Unavailable makes the next n requests of a route fail with 503 before any processing.
func (s *Server) Unavailable(route string, n int) { s.mu.Lock(); s.down[route] = n; s.mu.Unlock() }

// Record returns the deployment recorded for an action key.
func (s *Server) Record(actionKey string) (delivery.Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.deps[actionKey]
	if !ok {
		return delivery.Record{}, false
	}
	return r.rec, true
}

// Plant stores a deployment record directly (for example an unknown outcome).
func (s *Server) Plant(actionKey string, rec delivery.Record, annotations map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deps[actionKey] = &record{rec: rec, annotations: annotations, version: 1}
}

// DeploymentID derives the stable deployment identifier of a logical action.
func DeploymentID(tenant, project, actionKey string) string {
	sum := sha256.Sum256([]byte(tenant + "\x00" + project + "\x00" + actionKey))
	return "dep_" + hex.EncodeToString(sum[:])[:32]
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, code string) {
	write(w, status, map[string]any{"error": map[string]any{"code": code, "message": code, "requestId": "req_00000000000000000000000000000001", "details": map[string]any{}}})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 5 || parts[0] != "runtime" || parts[1] != "v1" || parts[2] != "attempts" {
		http.NotFound(w, r)
		return
	}
	attempt, rest := parts[3], parts[4:]
	route := ""
	switch {
	case r.Method == http.MethodGet && len(rest) == 1 && rest[0] == "context":
		route = "context"
	case r.Method == http.MethodPost && len(rest) == 1 && rest[0] == "deployments":
		route = "register"
	case r.Method == http.MethodGet && len(rest) == 3 && rest[0] == "deployments" && rest[1] == "by-action":
		route = "lookup"
	case r.Method == http.MethodPost && len(rest) == 3 && rest[0] == "deployments" && rest[2] == "outcome":
		route = "outcome"
	default:
		http.NotFound(w, r)
		return
	}
	var body struct {
		Source  delivery.Source   `json:"source"`
		Image   delivery.Image    `json:"image"`
		State   string            `json:"state"`
		Reason  string            `json:"reason"`
		Objects []delivery.Object `json:"objects"`
	}
	if r.Method == http.MethodPost {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			fail(w, 400, "invalid_request")
			return
		}
	}
	if route == "outcome" {
		route = body.State // "applying", "succeeded", "failed", "unknown"
	}

	s.mu.Lock()
	s.Calls[route]++
	if a, ok := s.revokeAt[route]; ok {
		delete(s.revokeAt, route)
		s.revoked[a] = true
	}
	if s.down[route] > 0 {
		s.down[route]--
		s.mu.Unlock()
		s.Log.Add("factory:%s:unavailable", route)
		fail(w, 503, "unavailable")
		return
	}
	status, resp, errCode := s.handle(route, attempt, rest, body.Source, body.Image, body.State, body.Reason, body.Objects)
	lose := false
	if s.drop[route] > 0 && errCode == "" {
		s.drop[route]--
		lose = true
	}
	s.mu.Unlock()

	s.Log.Add("factory:%s:%d", route, status)
	if lose {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
	}
	if errCode != "" {
		fail(w, status, errCode)
		return
	}
	write(w, status, resp)
}

func (s *Server) ctx(attempt string) (map[string]any, string) {
	if s.revoked[attempt] {
		return nil, "attempt_revoked"
	}
	c, ok := s.contexts[attempt]
	if !ok {
		return nil, "forbidden"
	}
	return c, ""
}

func str(m map[string]any, k string) string { v, _ := m[k].(string); return v }

func (s *Server) handle(route, attempt string, rest []string, src delivery.Source, img delivery.Image, state, reason string, objects []delivery.Object) (int, any, string) {
	cx, code := s.ctx(attempt)
	if code != "" {
		return 403, nil, code
	}
	actionKey := str(cx, "actionKey")
	env, _ := cx["environment"].(map[string]any)
	if route == "context" {
		return 200, cx, ""
	}
	if env == nil {
		return 403, nil, "pin_required"
	}
	switch route {
	case "register":
		if !referencesArtifact(cx, img.ArtifactID) {
			return 422, nil, "image_provenance_rejected"
		}
		if r, ok := s.deps[actionKey]; ok {
			if r.rec.Source != src || r.rec.Image != img {
				return 409, nil, "conflict"
			}
			return 200, s.receipt(r, false), ""
		}
		id := DeploymentID(str(cx, "tenantId"), str(cx, "projectId"), actionKey)
		r := &record{version: 1, rec: delivery.Record{ID: id, EnvironmentID: str(env, "id"), ConfigurationDigest: str(env, "configDigest"), Namespace: s.namespaceOf(cx),
			Source: src, Image: img, ActionKey: actionKey, Status: "pending", Reason: "registered", Objects: []delivery.Object{}},
			annotations: map[string]string{delivery.AnnotationDeploymentID: id, delivery.AnnotationActionKey: actionKey, delivery.AnnotationConfigDigest: str(env, "configDigest")}}
		if kind, _ := cx["x-kind"].(string); kind == "preview" {
			r.rec.ExpiresAt = "2026-01-03T03:04:05Z"
		}
		s.deps[actionKey] = r
		return 201, s.receipt(r, true), ""
	case "lookup":
		if rest[2] != actionKey {
			return 403, nil, "pin_required"
		}
		r, ok := s.deps[actionKey]
		if !ok {
			return 404, nil, "not_found"
		}
		return 200, s.receipt(r, false), ""
	}
	// outcome
	r, ok := s.deps[actionKey]
	if !ok || rest[1] != r.rec.ID {
		return 404, nil, "not_found"
	}
	settled := r.rec.Status == "succeeded" || r.rec.Status == "failed"
	switch state {
	case "applying":
		for _, o := range objects {
			if o.UID != "" || o.Ready != nil || o.Namespace != r.rec.Namespace {
				return 422, nil, "invalid_transition"
			}
		}
		switch {
		case settled:
			return 409, nil, "conflict"
		case r.rec.OutcomeUnknown:
			return 409, nil, "outcome_unknown"
		case r.rec.Status == "running":
			if !sameIdentity(r.rec.Objects, objects) {
				return 409, nil, "conflict"
			}
			return 200, s.receipt(r, false), ""
		}
		r.rec.Status, r.rec.Reason, r.rec.Objects = "running", "applying", objects
	case "succeeded":
		for _, o := range objects {
			if o.UID == "" || o.Ready == nil || !*o.Ready || o.Namespace != r.rec.Namespace {
				return 422, nil, "invalid_transition"
			}
		}
		if r.rec.Status == "succeeded" {
			if !reflect.DeepEqual(r.rec.Objects, objects) {
				return 409, nil, "conflict"
			}
			return 200, s.receipt(r, false), ""
		}
		if r.rec.Status != "running" {
			return 422, nil, "invalid_transition"
		}
		r.rec.Status, r.rec.Reason, r.rec.OutcomeUnknown, r.rec.Objects = "succeeded", "ready", false, objects
	case "failed":
		if reason == "" {
			return 400, nil, "invalid_request"
		}
		if r.rec.Status == "failed" {
			if r.rec.Reason != reason {
				return 409, nil, "conflict"
			}
			return 200, s.receipt(r, false), ""
		}
		if r.rec.Status == "succeeded" {
			return 409, nil, "conflict"
		}
		r.rec.Status, r.rec.Reason, r.rec.OutcomeUnknown = "failed", reason, false
		if len(objects) > 0 {
			r.rec.Objects = objects
		}
	case "unknown":
		if r.rec.Status != "running" {
			return 422, nil, "invalid_transition"
		}
		r.rec.Reason, r.rec.OutcomeUnknown = "outcome_unknown", true
	default:
		return 400, nil, "invalid_request"
	}
	r.version++
	return 200, s.receipt(r, false), ""
}

func (s *Server) namespaceOf(cx map[string]any) string {
	ns, _ := cx["x-namespace"].(string)
	return ns
}

func (s *Server) receipt(r *record, created bool) map[string]any {
	return map[string]any{"deployment": r.rec, "annotations": r.annotations, "created": created}
}

func sameIdentity(a, b []delivery.Object) bool {
	if len(a) != len(b) {
		return false
	}
	set := map[string]bool{}
	for _, o := range a {
		set[o.APIVersion+"/"+o.Kind+"/"+o.Namespace+"/"+o.Name] = true
	}
	for _, o := range b {
		if !set[o.APIVersion+"/"+o.Kind+"/"+o.Namespace+"/"+o.Name] {
			return false
		}
	}
	return true
}

// referencesArtifact mirrors the factory rule: the image must be one of the
// attempt's pinned input values.
func referencesArtifact(cx map[string]any, id string) bool {
	inputs, _ := cx["inputs"].(map[string]any)
	for _, v := range inputs {
		if s, ok := v.(string); ok && s == id {
			return true
		}
	}
	return false
}
