// Package mcpfixture is a local MCP protocol server for the T-033 tests. It is a
// real Streamable HTTP server built on the official Go SDK, served over TLS on
// the loopback interface, with fault controls: a hidden tool, pagination, a
// blocking tool that records cancellation, a side-effecting tool whose reply can
// be lost after the effect, redirects, recipient-bound bearer checking and a
// pinned-protocol restriction. It proves the declared MCP combination only; it
// is not evidence that any production server or harness is supported.
//
// Resource identifiers carry the task prefix t033.
package mcpfixture

import (
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Prefix is this task's resource prefix.
const Prefix = "t033"

// DefaultToken is the bearer a fixture accepts unless configured otherwise.
const DefaultToken = "t033-remote-token-SENTINEL-5c1e"

// Options configure a fixture. Zero values are usable.
type Options struct {
	// Token is the bearer the server accepts (DefaultToken when empty).
	Token string
	// Protocols restricts the protocol revisions the server negotiates
	// (SDK default when empty).
	Protocols []string
	// PageSize is the tools/list page size (0: one page).
	PageSize int
	// ExtraTools adds filler tools named "filler_<n>" to exercise pagination.
	ExtraTools int
}

// Request records one HTTP request the fixture received.
type Request struct {
	Method        string
	Path          string
	Authorization string
	SessionID     string
	Body          string
}

// Server is one running fixture.
type Server struct {
	Srv   *httptest.Server
	token string

	mu        sync.Mutex
	requests  []Request
	cancelled []string
	started   []string
	loseNext  int
	sink      *Sink

	// blockList makes tools/list block until its request is cancelled.
	blockList     atomic.Bool
	listStarted   atomic.Int64
	listCancelled atomic.Int64

	// Mutations counts executed "mutate" calls: the observable side effect.
	Mutations atomic.Int64
	// Calls counts executed tools/call requests by tool name.
	mcalls sync.Map

	server *mcp.Server
}

type mutateIn struct {
	Key string `json:"key" jsonschema:"idempotency key"`
}
type textOut struct {
	Text string `json:"text,omitempty"`
}
type echoIn struct {
	Text string `json:"text"`
}
type slowIn struct {
	Label string `json:"label,omitempty"`
}
type noIn struct{}

// New starts a fixture and registers its cleanup.
func New(t testing.TB, o Options) *Server {
	t.Helper()
	s := &Server{token: o.Token}
	if s.token == "" {
		s.token = DefaultToken
	}
	opts := &mcp.ServerOptions{SupportedProtocolVersions: o.Protocols, PageSize: o.PageSize}
	s.server = mcp.NewServer(&mcp.Implementation{Name: Prefix + "-remote", Version: "1"}, opts)

	s.server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/list" && s.blockList.Load() {
				s.listStarted.Add(1)
				select {
				case <-ctx.Done():
					s.listCancelled.Add(1)
					return nil, ctx.Err()
				case <-time.After(30 * time.Second):
				}
			}
			return next(ctx, method, req)
		}
	})
	mcp.AddTool(s.server, &mcp.Tool{Name: "echo", Description: "echoes text"}, func(ctx context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, textOut, error) {
		s.count("echo")
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo:" + in.Text}}}, textOut{Text: in.Text}, nil
	})
	mcp.AddTool(s.server, &mcp.Tool{Name: "admin_secret", Description: "a tool the registration and profile never expose"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ noIn) (*mcp.CallToolResult, textOut, error) {
		s.count("admin_secret")
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "admin"}}}, textOut{}, nil
	})
	mcp.AddTool(s.server, &mcp.Tool{Name: "mutate", Description: "performs a side effect"}, func(ctx context.Context, _ *mcp.CallToolRequest, in mutateIn) (*mcp.CallToolResult, textOut, error) {
		s.count("mutate")
		n := s.Mutations.Add(1)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("mutated:%d", n)}}}, textOut{}, nil
	})
	mcp.AddTool(s.server, &mcp.Tool{Name: "slow", Description: "blocks until cancelled"}, func(ctx context.Context, _ *mcp.CallToolRequest, in slowIn) (*mcp.CallToolResult, textOut, error) {
		s.count("slow")
		s.mu.Lock()
		s.started = append(s.started, in.Label)
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.cancelled = append(s.cancelled, in.Label)
			s.mu.Unlock()
			return nil, textOut{}, ctx.Err()
		case <-time.After(30 * time.Second):
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "slow done"}}}, textOut{}, nil
		}
	})
	mcp.AddTool(s.server, &mcp.Tool{Name: "fail", Description: "answers with a protocol error"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ noIn) (*mcp.CallToolResult, textOut, error) {
		s.count("fail")
		return nil, textOut{}, fmt.Errorf("deliberate failure")
	})
	for i := 0; i < o.ExtraTools; i++ {
		name := fmt.Sprintf("filler_%02d", i)
		mcp.AddTool(s.server, &mcp.Tool{Name: name, Description: "filler"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ noIn) (*mcp.CallToolResult, textOut, error) {
			s.count(name)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: name}}}, textOut{}, nil
		})
	}

	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.server }, &mcp.StreamableHTTPOptions{JSONResponse: true, DisableLocalhostProtection: true})
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.guard(sdk))
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		s.record(r, nil)
		s.mu.Lock()
		sink := s.sink
		s.mu.Unlock()
		if sink == nil {
			http.Error(w, "no redirect target", http.StatusNotFound)
			return
		}
		http.Redirect(w, r, sink.URL()+"/mcp", http.StatusTemporaryRedirect)
	})
	s.Srv = httptest.NewTLSServer(mux)
	t.Cleanup(s.Srv.Close)
	return s
}

func (s *Server) count(tool string) {
	v, _ := s.mcalls.LoadOrStore(tool, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

// Executed returns how many times a tool's handler ran.
func (s *Server) Executed(tool string) int64 {
	if v, ok := s.mcalls.Load(tool); ok {
		return v.(*atomic.Int64).Load()
	}
	return 0
}

func (s *Server) record(r *http.Request, body []byte) {
	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Authorization: r.Header.Get("Authorization"),
		SessionID: r.Header.Get("Mcp-Session-Id"), Body: string(body)})
	s.mu.Unlock()
}

// guard checks the recipient-bound bearer, records the request and applies the
// "lose the reply after the effect" fault.
func (s *Server) guard(next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		s.record(r, body)
		if r.Header.Get("Authorization") != "Bearer "+s.token {
			w.Header().Set("WWW-Authenticate", `Bearer realm="t033-remote"`)
			http.Error(w, "wrong audience or token", http.StatusUnauthorized)
			return
		}
		lose := false
		if r.Method == http.MethodPost && bytes.Contains(body, []byte(`"tools/call"`)) {
			s.mu.Lock()
			if s.loseNext > 0 {
				s.loseNext--
				lose = true
			}
			s.mu.Unlock()
		}
		if !lose {
			next.ServeHTTP(w, r)
			return
		}
		// Run the handler (the effect happens), then drop the connection with no reply.
		next.ServeHTTP(discard{header: http.Header{}}, r)
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close()
		}
	}
}

type discard struct{ header http.Header }

func (d discard) Header() http.Header         { return d.header }
func (d discard) Write(b []byte) (int, error) { return len(b), nil }
func (d discard) WriteHeader(int)             {}

// LoseNextReplies makes the next n tools/call replies vanish after the tool ran.
func (s *Server) LoseNextReplies(n int) { s.mu.Lock(); s.loseNext = n; s.mu.Unlock() }

// RedirectTo sets the sink the /redirect path forwards to.
func (s *Server) RedirectTo(sink *Sink) { s.mu.Lock(); s.sink = sink; s.mu.Unlock() }

// URL is the MCP endpoint ("https://127.0.0.1:port/mcp").
func (s *Server) URL() string { return s.Srv.URL + "/mcp" }

// RedirectURL is an endpoint that redirects to the configured sink.
func (s *Server) RedirectURL() string { return s.Srv.URL + "/redirect" }

// Origin is the credential recipient of this server.
func (s *Server) Origin() string { return s.Srv.URL }

// HostPort is "127.0.0.1:port".
func (s *Server) HostPort() string { return strings.TrimPrefix(s.Srv.URL, "https://") }

// Port is the listening port.
func (s *Server) Port() string {
	_, p, _ := net.SplitHostPort(s.HostPort())
	return p
}

// Roots is a pool trusting the fixture's certificate.
func (s *Server) Roots() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(s.Srv.Certificate())
	return pool
}

// Token is the bearer the server accepts.
func (s *Server) Token() string { return s.token }

// Requests returns a copy of the recorded HTTP requests.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// BlockListing makes tools/list requests block until cancelled (a slow remote
// discovery); false restores normal listing. ListingStarted and
// ListingCancelled count the blocked requests that began and that observed
// cancellation.
func (s *Server) BlockListing(on bool)    { s.blockList.Store(on) }
func (s *Server) ListingStarted() int64   { return s.listStarted.Load() }
func (s *Server) ListingCancelled() int64 { return s.listCancelled.Load() }

// Started and Cancelled return the labels of "slow" calls that began and that
// observed cancellation.
func (s *Server) Started() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.started...)
}
func (s *Server) Cancelled() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cancelled...)
}

// Sink is a second server that records every request it receives, standing in
// for a redirect target an attacker controls. It never answers with MCP.
type Sink struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []Request
}

// NewSink starts a sink.
func NewSink(t testing.TB) *Sink {
	t.Helper()
	k := &Sink{}
	k.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		k.got = append(k.got, Request{Method: r.Method, Path: r.URL.Path, Authorization: r.Header.Get("Authorization")})
		k.mu.Unlock()
		http.Error(w, "sink", http.StatusTeapot)
	}))
	t.Cleanup(k.srv.Close)
	return k
}

// URL is the sink's origin.
func (k *Sink) URL() string { return k.srv.URL }

// Requests returns what the sink received.
func (k *Sink) Requests() []Request {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]Request(nil), k.got...)
}
