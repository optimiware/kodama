package harnessopencode_test

// The deterministic model fixture: an OpenAI-compatible streaming chat
// completions upstream that the REAL gateway forwards to. It scripts a sequence
// of replies (text, tool calls, a stalled stream) and records every request it
// received, so a test can assert what the harness actually sent and saw. It is a
// control, not a provider: nothing here says any real model behaves this way.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/optimiware/kodama/internal/gateway/gatewaytest"
)

type msgView struct {
	Role       string
	Content    string
	ToolCalls  []string
	ToolCallID string
}

type reqView struct {
	Model     string
	Messages  []msgView
	Tools     []string
	MaxTokens int64
	Members   []string
	Auth      string
}

// toolResults returns the content of every tool message, in order.
func (r reqView) toolResults() []string {
	var out []string
	for _, m := range r.Messages {
		if m.Role == "tool" {
			out = append(out, m.Content)
		}
	}
	return out
}

func (r reqView) hasTool(name string) bool {
	for _, t := range r.Tools {
		if t == name {
			return true
		}
	}
	return false
}

// firstUser is the first user message: the prompt the wrapper composed.
func (r reqView) firstUser() string {
	for _, m := range r.Messages {
		if m.Role == "user" {
			return m.Content
		}
	}
	return ""
}

type reply struct {
	text string
	tool string
	args map[string]any
	hang *hang
	fail int
	// again answers this request without advancing the script: the same step
	// answers the next one.
	again bool
}

type hang struct {
	started chan struct{}
	release chan struct{}
}

func newHang() *hang { return &hang{started: make(chan struct{}), release: make(chan struct{})} }

func say(s string) reply                          { return reply{text: s} }
func call(tool string, args map[string]any) reply { return reply{tool: tool, args: args} }

type modelScript struct {
	p *gatewaytest.Provider

	cancelled atomic.Int64

	mu    sync.Mutex
	fns   []func(reqView) reply
	n     int
	reqs  []reqView
	title int
}

func newModelScript(p *gatewaytest.Provider) *modelScript {
	m := &modelScript{p: p}
	p.Default(m.serve)
	return m
}

// Steps installs the replies for the harness's substantive requests (the ones
// that carry tools), in order; a request beyond the script gets "done". The
// session-title request OpenCode makes without tools is answered separately and
// does not consume a step.
func (m *modelScript) Steps(fns ...func(reqView) reply) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fns, m.n, m.reqs, m.title = fns, 0, nil, 0
}

// Seq is Steps over constant replies.
func (m *modelScript) Seq(rs ...reply) {
	var fns []func(reqView) reply
	for _, r := range rs {
		r := r
		fns = append(fns, func(reqView) reply { return r })
	}
	m.Steps(fns...)
}

func (m *modelScript) Requests() []reqView {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]reqView(nil), m.reqs...)
}

// Substantive returns the requests that carried tools.
func (m *modelScript) Substantive() []reqView {
	var out []reqView
	for _, r := range m.Requests() {
		if len(r.Tools) > 0 {
			out = append(out, r)
		}
	}
	return out
}

func parseReq(body []byte, auth string) reqView {
	var raw struct {
		Model    string `json:"model"`
		Messages []struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_calls"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		MaxTokens           int64 `json:"max_tokens"`
		MaxCompletionTokens int64 `json:"max_completion_tokens"`
	}
	_ = json.Unmarshal(body, &raw)
	var members map[string]json.RawMessage
	_ = json.Unmarshal(body, &members)
	v := reqView{Model: raw.Model, MaxTokens: raw.MaxTokens + raw.MaxCompletionTokens, Auth: auth}
	for k := range members {
		v.Members = append(v.Members, k)
	}
	for _, m := range raw.Messages {
		mv := msgView{Role: m.Role, ToolCallID: m.ToolCallID}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			mv.Content = s
		}
		for _, tc := range m.ToolCalls {
			mv.ToolCalls = append(mv.ToolCalls, tc.Function.Name)
		}
		v.Messages = append(v.Messages, mv)
	}
	for _, t := range raw.Tools {
		v.Tools = append(v.Tools, t.Function.Name)
	}
	return v
}

func (m *modelScript) serve(w http.ResponseWriter, r *http.Request) {
	// The provider already read and recorded the body for its own Calls(); read
	// the copy it keeps.
	calls := m.p.Calls()
	last := calls[len(calls)-1]
	v := parseReq(last.Body, last.Authorization)
	m.mu.Lock()
	m.reqs = append(m.reqs, v)
	var rep reply
	switch {
	case len(v.Tools) == 0:
		m.title++
		rep = say("Kodama task")
	case m.n < len(m.fns):
		fn := m.fns[m.n]
		m.mu.Unlock()
		rep = fn(v)
		m.mu.Lock()
		if !rep.again {
			m.n++
		}
	default:
		m.n++
		rep = say("done")
	}
	m.mu.Unlock()
	m.write(w, r, rep)
}

func chunk(delta map[string]any, finish any) string {
	c := map[string]any{"id": "chatcmpl-t031", "object": "chat.completion.chunk", "created": 1760000000, "model": modelName,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	b, _ := json.Marshal(c)
	return "data: " + string(b) + "\n\n"
}

func usageChunk(in, out int64) string {
	c := map[string]any{"id": "chatcmpl-t031", "object": "chat.completion.chunk", "created": 1760000000, "model": modelName, "choices": []any{},
		"usage": map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out}}
	b, _ := json.Marshal(c)
	return "data: " + string(b) + "\n\n"
}

func (m *modelScript) write(w http.ResponseWriter, r *http.Request, rep reply) {
	if rep.fail != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.fail)
		_, _ = io.WriteString(w, `{"error":{"message":"scripted failure"}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	f := w.(http.Flusher)
	f.Flush()
	if rep.hang != nil {
		_, _ = io.WriteString(w, chunk(map[string]any{"role": "assistant", "content": "working"}, nil))
		f.Flush()
		close(rep.hang.started)
		select {
		case <-r.Context().Done():
			m.cancelled.Add(1)
		case <-rep.hang.release:
		case <-time.After(5 * time.Minute):
		}
		return
	}
	switch {
	case rep.tool != "":
		args, _ := json.Marshal(rep.args)
		_, _ = io.WriteString(w, chunk(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
			"index": 0, "id": fmt.Sprintf("call_%d", time.Now().UnixNano()), "type": "function",
			"function": map[string]any{"name": rep.tool, "arguments": string(args)},
		}}}, nil))
		_, _ = io.WriteString(w, chunk(map[string]any{}, "tool_calls"))
	default:
		_, _ = io.WriteString(w, chunk(map[string]any{"role": "assistant", "content": rep.text}, nil))
		_, _ = io.WriteString(w, chunk(map[string]any{}, "stop"))
	}
	_, _ = io.WriteString(w, usageChunk(40, 8))
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	f.Flush()
}

// resultFile is the completion the agent writes through OpenCode's own write
// tool, as the wrapper's protocol asks.
func resultFile(doc string) reply {
	return call("write", map[string]any{"path": ".kodama-result.json", "content": doc})
}

func quote(s string) string { return strings.ReplaceAll(s, `"`, `\"`) }

// awaitTool answers with a short shell wait, without advancing the script, until
// OpenCode offers the named tool (MCP servers connect asynchronously, so the
// first turn can precede their catalog), then with then.
func awaitTool(name string, then reply) func(reqView) reply {
	return func(r reqView) reply {
		if r.hasTool(name) {
			return then
		}
		return reply{tool: "shell", args: map[string]any{"command": "sleep 1"}, again: true}
	}
}

func constant(r reply) func(reqView) reply { return func(reqView) reply { return r } }

// Cancelled counts the stalled streams whose request context ended (the gateway
// cancelled its upstream call).
func (m *modelScript) Cancelled() int64 { return m.cancelled.Load() }
