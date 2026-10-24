// Package git provides isolated, real Git fixtures for integration tests: an
// HTTPS git-http-backend with distinct read and write tokens, bare repositories
// created per test, human-style commits and entrypoint-style bundles, and
// deterministic fault injection around the receive-pack endpoint.
//
// Each Server owns a private temporary root and a TLS listener on a loopback
// port chosen by the OS, so parallel tests and tasks never share a repository,
// ref or port. Nothing here touches shared services.
package git

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Username is the HTTP Basic user the server expects with either token.
const Username = "kodama"

// Role identifies which token a request presented.
type Role string

const (
	RoleNone  Role = "none"
	RoleRead  Role = "read"
	RoleWrite Role = "write"
)

// Request is one request the server received.
type Request struct {
	Method  string
	Path    string
	Service string // git-upload-pack or git-receive-pack, when applicable
	Role    Role
}

// Server serves bare repositories under Root over HTTPS.
type Server struct {
	// URL is the server origin, for example https://127.0.0.1:41234.
	URL string
	// CAFile is a PEM file trusting the server certificate.
	CAFile string
	// ReadToken authorizes fetch only; WriteToken authorizes fetch and push.
	ReadToken, WriteToken string
	// Root holds the bare repositories (<Root>/<name>.git).
	Root string

	t       testing.TB
	srv     *httptest.Server
	backend http.Handler
	mu      sync.Mutex
	reqs    []Request
	faults  faults
}

// NewServer starts a server and removes it when the test ends. It skips the
// test when Git or git-http-backend is not installed.
func NewServer(t testing.TB) *Server {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	execPath, err := exec.Command(gitBin, "--exec-path").Output()
	if err != nil {
		t.Skipf("git --exec-path: %v", err)
	}
	backendPath := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backendPath); err != nil {
		t.Skip("git-http-backend is not available")
	}
	root := t.TempDir()
	s := &Server{
		t: t, Root: root, ReadToken: token(t), WriteToken: token(t),
		backend: &cgi.Handler{Path: backendPath, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}},
	}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.close)
	s.URL = s.srv.URL
	s.CAFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(s.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return s
}

func token(t testing.TB) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func (s *Server) close() {
	s.faults.releaseAll()
	s.srv.Close()
}

// Requests returns a copy of the request log.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.reqs...)
}

// CountRequests counts logged requests matching the predicate.
func (s *Server) CountRequests(match func(Request) bool) int {
	n := 0
	for _, r := range s.Requests() {
		if match(r) {
			n++
		}
	}
	return n
}

func (s *Server) classify(r *http.Request) Request {
	req := Request{Method: r.Method, Path: r.URL.Path, Role: RoleNone}
	user, pass, ok := r.BasicAuth()
	if ok && user == Username {
		switch pass {
		case s.WriteToken:
			req.Role = RoleWrite
		case s.ReadToken:
			req.Role = RoleRead
		}
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/git-receive-pack") || r.URL.Query().Get("service") == "git-receive-pack":
		req.Service = "git-receive-pack"
	case strings.HasSuffix(r.URL.Path, "/git-upload-pack") || r.URL.Query().Get("service") == "git-upload-pack":
		req.Service = "git-upload-pack"
	}
	return req
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	req := s.classify(r)
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()

	allowed := req.Role == RoleWrite || (req.Role == RoleRead && req.Service == "git-upload-pack")
	if !allowed {
		if req.Role == RoleNone {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
		} else {
			w.WriteHeader(http.StatusForbidden)
		}
		return
	}
	if req.Service == "git-upload-pack" && r.Method == http.MethodGet {
		if !s.faults.delayInfoRefs(r) {
			return
		}
	}
	if req.Service == "git-receive-pack" && r.Method == http.MethodPost {
		s.faults.receivePack(s, w, r)
		return
	}
	s.backend.ServeHTTP(w, r)
}

// serveBuffered forwards r with a pre-read body, so the backend can finish
// after the client went away.
func (s *Server) serveBuffered(w http.ResponseWriter, r *http.Request, body []byte) {
	clone := r.Clone(r.Context())
	clone.Body = io.NopCloser(bytes.NewReader(body))
	clone.ContentLength = int64(len(body))
	s.backend.ServeHTTP(w, clone)
}
