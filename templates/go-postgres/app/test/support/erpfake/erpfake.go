// Package erpfake is the controlled ERP peer for the sample protocol in
// internal/erp/protocol.md. It enforces tenant-scoped credentials, applies each
// operation key at most once per tenant, detects payload conflicts, and injects
// the faults a real ERP link exhibits. It holds no real credentials or data.
package erpfake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Effect is one applied adjustment.
type Effect struct {
	Payload   Payload
	Reference string
	Applied   int // times the effect was applied: must stay 1
	Calls     int // requests received for the key
}

// Payload is the apply body.
type Payload struct {
	ItemRef     string `json:"itemRef"`
	Quantity    int64  `json:"quantity"`
	Reason      string `json:"reason"`
	PayloadHash string `json:"payloadHash"`
}

// Server is a running fake ERP.
type Server struct {
	HTTP *httptest.Server

	mu      sync.Mutex
	tokens  map[string]string // token -> tenant
	effects map[string]*Effect
	seq     int

	loseAck    int    // apply, then drop the connection, this many times
	rejectNext string // reject the next apply with this reason
	applyDown  bool   // apply fails with 503 before any effect
	lookupDown bool

	hold    bool          // block applies after their effect until released or the caller gives up
	Applied chan struct{} // receives once per effect applied while holding
}

// New starts the fake; tokens maps each tenant to its credential.
func New(t testing.TB, tenantTokens map[string]string) *Server {
	t.Helper()
	s := &Server{tokens: map[string]string{}, effects: map[string]*Effect{}}
	for tenant, tok := range tenantTokens {
		s.tokens[tok] = tenant
	}
	s.HTTP = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.HTTP.Close)
	return s
}

// URL is the base URL.
func (s *Server) URL() string { return s.HTTP.URL }

// LoseAcknowledgments makes the next n applies take effect and then drop the
// connection without answering.
func (s *Server) LoseAcknowledgments(n int) { s.mu.Lock(); s.loseAck = n; s.mu.Unlock() }

// RejectNext makes the next apply a definitive rejection.
func (s *Server) RejectNext(reason string) { s.mu.Lock(); s.rejectNext = reason; s.mu.Unlock() }

// HoldAfterEffect makes applies take effect and then stall without answering,
// until the caller disconnects: a worker killed mid-request.
func (s *Server) HoldAfterEffect(v bool) {
	s.mu.Lock()
	s.hold = v
	if v && s.Applied == nil {
		s.Applied = make(chan struct{}, 16)
	}
	s.mu.Unlock()
}

// SetApplyDown makes applies fail with 503 before any effect.
func (s *Server) SetApplyDown(v bool) { s.mu.Lock(); s.applyDown = v; s.mu.Unlock() }

// SetLookupDown makes lookups fail with 503.
func (s *Server) SetLookupDown(v bool) { s.mu.Lock(); s.lookupDown = v; s.mu.Unlock() }

// Effect returns the recorded effect, if any.
func (s *Server) Effect(tenant, key string) (Effect, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.effects[tenant+"/"+key]
	if !ok {
		return Effect{}, false
	}
	return *e, true
}

// TotalEffects counts distinct applied effects across tenants.
func (s *Server) TotalEffects() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.effects)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 5 || parts[0] != "v1" || parts[1] != "tenants" || parts[3] != "adjustments" {
		http.NotFound(w, r)
		return
	}
	tenant, key := parts[2], parts[4]
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	owner, ok := s.tokens[tok]
	s.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	if owner != tenant {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	id := tenant + "/" + key
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.lookupDown {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		e, ok := s.effects[id]
		if !ok {
			http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
			return
		}
		e.Calls++
		writeJSON(w, http.StatusOK, map[string]string{"status": "acknowledged", "reference": e.Reference})
	case http.MethodPut:
		var p Payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, `{"error":"bad_request"}`, http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		if s.applyDown {
			s.mu.Unlock()
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if e, ok := s.effects[id]; ok {
			e.Calls++
			same := e.Payload == p
			ref := e.Reference
			s.mu.Unlock()
			if !same {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "payload_conflict"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "acknowledged", "reference": ref})
			return
		}
		if s.rejectNext != "" {
			reason := s.rejectNext
			s.rejectNext = ""
			s.mu.Unlock()
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"status": "rejected", "reason": reason})
			return
		}
		s.seq++
		e := &Effect{Payload: p, Reference: fmt.Sprintf("erp-%04d", s.seq), Applied: 1, Calls: 1}
		s.effects[id] = e
		drop := s.loseAck > 0
		if drop {
			s.loseAck--
		}
		ref := e.Reference
		hold, applied := s.hold, s.Applied
		s.mu.Unlock()
		if hold {
			applied <- struct{}{}
			<-r.Context().Done() // the caller died; the effect stands
			return
		}
		if drop {
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close() // the effect happened; the acknowledgment never arrives
					return
				}
			}
			http.Error(w, "dropped", http.StatusBadGateway)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"status": "acknowledged", "reference": ref})
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
