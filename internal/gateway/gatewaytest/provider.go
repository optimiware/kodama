package gatewaytest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Call is one request the controlled provider received.
type Call struct {
	Path          string
	Authorization string
	Body          []byte
	Header        http.Header
}

// Behavior answers one provider request.
type Behavior func(w http.ResponseWriter, r *http.Request)

// Provider is a controlled TLS server standing in for an OpenAI-compatible
// provider. It records every request, plays scripted behaviors in order and
// counts the requests whose context ended while it was still streaming (the
// observable proof of upstream cancellation).
type Provider struct {
	Srv *httptest.Server

	mu        sync.Mutex
	calls     []Call
	script    []Behavior
	fallback  Behavior
	cancelled atomic.Int64
	inflight  atomic.Int64
	maxFlight atomic.Int64
}

// NewProvider starts the server. Its default behavior is a successful reply.
func NewProvider() *Provider {
	p := &Provider{fallback: OK("gpt-a-2026", 10, 3, "ok")}
	p.Srv = httptest.NewTLSServer(http.HandlerFunc(p.serve))
	return p
}

// Close stops the server.
func (p *Provider) Close() { p.Srv.Close() }

// Origin is "https://127.0.0.1:port".
func (p *Provider) Origin() string { return p.Srv.URL }

// Endpoint is the approved base URL with a path prefix.
func (p *Provider) Endpoint() string { return p.Srv.URL + "/v1" }

// Script queues behaviors for the next requests, one per request.
func (p *Provider) Script(bs ...Behavior) {
	p.mu.Lock()
	p.script = append(p.script, bs...)
	p.mu.Unlock()
}

// Default replaces the behavior used when the script is empty.
func (p *Provider) Default(b Behavior) {
	p.mu.Lock()
	p.fallback = b
	p.mu.Unlock()
}

// Calls returns the requests received so far.
func (p *Provider) Calls() []Call {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Call(nil), p.calls...)
}

// Cancelled counts requests whose context ended during a Hang behavior.
func (p *Provider) Cancelled() int { return int(p.cancelled.Load()) }

// MaxConcurrent is the highest number of simultaneously open requests.
func (p *Provider) MaxConcurrent() int { return int(p.maxFlight.Load()) }

func (p *Provider) serve(w http.ResponseWriter, r *http.Request) {
	n := p.inflight.Add(1)
	defer p.inflight.Add(-1)
	for {
		m := p.maxFlight.Load()
		if n <= m || p.maxFlight.CompareAndSwap(m, n) {
			break
		}
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	p.mu.Lock()
	p.calls = append(p.calls, Call{Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), Body: body, Header: r.Header.Clone()})
	b := p.fallback
	if len(p.script) > 0 {
		b, p.script = p.script[0], p.script[1:]
	}
	p.mu.Unlock()
	b(w, r)
}

// ---- stream builders ------------------------------------------------------------

func frame(model, content string, finish string, usage [2]int64, withUsage bool) string {
	chunk := map[string]any{"id": "chatcmpl-t032", "object": "chat.completion.chunk", "created": 1760000000, "model": model, "system_fingerprint": "fp_dropped"}
	choices := []any{}
	if !withUsage {
		var fr any
		if finish != "" {
			fr = finish
		}
		choices = append(choices, map[string]any{"index": 0, "delta": map[string]any{"content": content}, "finish_reason": fr})
	}
	chunk["choices"] = choices
	if withUsage {
		chunk["usage"] = map[string]any{"prompt_tokens": usage[0], "completion_tokens": usage[1], "total_tokens": usage[0] + usage[1]}
	}
	raw, _ := json.Marshal(chunk)
	return "data: " + string(raw) + "\n\n"
}

func startStream(w http.ResponseWriter) http.Flusher {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	f := w.(http.Flusher)
	f.Flush()
	return f
}

// OK streams the texts, a finish chunk, the usage chunk and [DONE].
func OK(model string, in, out int64, texts ...string) Behavior {
	return func(w http.ResponseWriter, r *http.Request) {
		f := startStream(w)
		for _, t := range texts {
			_, _ = io.WriteString(w, frame(model, t, "", [2]int64{}, false))
			f.Flush()
		}
		_, _ = io.WriteString(w, frame(model, "", "stop", [2]int64{}, false))
		_, _ = io.WriteString(w, frame(model, "", "", [2]int64{in, out}, true))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}
}

// NoUsage streams a complete reply without a usage chunk.
func NoUsage(model string, texts ...string) Behavior {
	return func(w http.ResponseWriter, r *http.Request) {
		f := startStream(w)
		for _, t := range texts {
			_, _ = io.WriteString(w, frame(model, t, "", [2]int64{}, false))
		}
		_, _ = io.WriteString(w, frame(model, "", "stop", [2]int64{}, false))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}
}

// InvalidUsage streams a complete reply whose usage object is inconsistent.
func InvalidUsage(model string) Behavior {
	return func(w http.ResponseWriter, r *http.Request) {
		f := startStream(w)
		_, _ = io.WriteString(w, frame(model, "x", "stop", [2]int64{}, false))
		_, _ = io.WriteString(w, `data: {"id":"c","object":"chat.completion.chunk","model":"`+model+`","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":99}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}
}

// Truncated writes the texts, then drops the connection without [DONE].
func Truncated(model string, texts ...string) Behavior {
	return func(w http.ResponseWriter, r *http.Request) {
		f := startStream(w)
		for _, t := range texts {
			_, _ = io.WriteString(w, frame(model, t, "", [2]int64{}, false))
			f.Flush()
		}
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
	}
}

// Hang writes the texts, then holds the stream open until the request context
// ends or release is closed. Started (if non-nil) is closed after the texts.
func (p *Provider) Hang(model string, started chan<- struct{}, release <-chan struct{}, texts ...string) Behavior {
	return func(w http.ResponseWriter, r *http.Request) {
		f := startStream(w)
		for _, t := range texts {
			_, _ = io.WriteString(w, frame(model, t, "", [2]int64{}, false))
			f.Flush()
		}
		if started != nil {
			close(started)
		}
		select {
		case <-r.Context().Done():
			p.cancelled.Add(1)
		case <-release:
		case <-time.After(30 * time.Second):
		}
	}
}

// Status answers with an error status and a body that quotes the credential
// and prompt it received, as a careless provider might.
func Status(code int) Behavior {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, `{"error":{"message":"bad key %s","echo":%q}}`, r.Header.Get("Authorization"), strings.TrimSpace(string(body)))
	}
}

// Redirect answers with a redirect to target.
func Redirect(code int, target string) Behavior {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, code)
	}
}

// Gate signals started on arrival and waits for release before running inner.
func Gate(started chan<- struct{}, release <-chan struct{}, inner Behavior) Behavior {
	return func(w http.ResponseWriter, r *http.Request) {
		if started != nil {
			started <- struct{}{}
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		inner(w, r)
	}
}
