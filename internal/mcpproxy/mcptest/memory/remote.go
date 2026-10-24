package memory

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"github.com/optimiware/kodama/internal/mcpproxy"
)

// Tool scripts one remote tool: its deterministic answer. Exactly one of the
// outcomes applies, in this order: Hold (block until the call's context ends,
// like the SDK connection), Transport (the reply is lost: the effect is
// unknown), Reply (a definitive protocol error reply) and otherwise Result.
type Tool struct {
	// Result is the JSON result; IsError marks an application error result.
	Result  json.RawMessage
	IsError bool
	// Reply is a definitive protocol error code.
	Reply string
	// Transport makes the reply vanish after the send.
	Transport bool
	// Hold blocks the call until its context ends.
	Hold bool
}

// Call is one observed tool call. Arguments are kept so a test can prove what
// the remote saw; they are never persisted by the proxy.
type Call struct {
	Tool      string
	Arguments json.RawMessage
	Bearer    string
}

// Remote is the scripted deterministic remote MCP server. It satisfies
// mcpproxy.RemoteTools; it speaks no network protocol.
type Remote struct {
	mu       sync.Mutex
	tools    map[string]Tool
	listed   []string
	calls    []Call
	opened   []mcpproxy.RemoteTarget
	conns    []*Conn
	openErr  error
	protocol string
	started  chan struct{}
}

var _ mcpproxy.RemoteTools = (*Remote)(nil)

// NewRemote returns a remote with no tools.
func NewRemote() *Remote {
	return &Remote{tools: map[string]Tool{}, protocol: "2025-11-25", started: make(chan struct{}, 64)}
}

// Script defines (or replaces) a tool and advertises it in listings.
func (r *Remote) Script(name string, t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[name]; !ok {
		r.listed = append(r.listed, name)
		sort.Strings(r.listed)
	}
	r.tools[name] = t
}

// FailOpen makes opening a session fail with err; nil restores it.
func (r *Remote) FailOpen(err error) {
	r.mu.Lock()
	r.openErr = err
	r.mu.Unlock()
}

// SetProtocol sets the revision the next sessions negotiate.
func (r *Remote) SetProtocol(p string) {
	r.mu.Lock()
	r.protocol = p
	r.mu.Unlock()
}

// Calls returns the observed tool calls in order.
func (r *Remote) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.calls...)
}

// CallsOf counts the observed calls of one tool.
func (r *Remote) CallsOf(tool string) int {
	n := 0
	for _, c := range r.Calls() {
		if c.Tool == tool {
			n++
		}
	}
	return n
}

// Opened returns the targets sessions were opened against.
func (r *Remote) Opened() []mcpproxy.RemoteTarget {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]mcpproxy.RemoteTarget(nil), r.opened...)
}

// Conns returns the sessions opened so far.
func (r *Remote) Conns() []*Conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*Conn(nil), r.conns...)
}

// Started receives one value each time a held call begins.
func (r *Remote) Started() <-chan struct{} { return r.started }

// Open implements mcpproxy.RemoteTools.
func (r *Remote) Open(_ context.Context, t mcpproxy.RemoteTarget) (mcpproxy.RemoteConn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.openErr != nil {
		return nil, r.openErr
	}
	t.Bearer = append([]byte(nil), t.Bearer...)
	r.opened = append(r.opened, t)
	c := &Conn{r: r, bearer: string(t.Bearer), protocol: r.protocol}
	r.conns = append(r.conns, c)
	return c, nil
}

// Conn is one remote session.
type Conn struct {
	r        *Remote
	bearer   string
	protocol string

	mu      sync.Mutex
	revoked int
	closed  int
	// terminated counts session terminations that would present a credential.
	terminated int
}

var _ mcpproxy.RemoteConn = (*Conn)(nil)

// Protocol implements mcpproxy.RemoteConn.
func (c *Conn) Protocol() string { return c.protocol }

// Revoke implements mcpproxy.RemoteConn: the credential copy is erased.
func (c *Conn) Revoke() {
	c.mu.Lock()
	c.revoked++
	c.bearer = ""
	c.mu.Unlock()
}

// Close implements mcpproxy.RemoteConn. A close with the credential still held
// would terminate the remote session with it: the fixture records that.
func (c *Conn) Close() error {
	c.mu.Lock()
	if c.revoked == 0 {
		c.terminated++
	}
	c.closed++
	c.mu.Unlock()
	return nil
}

// Revoked, Closed and Terminated report what the service did to the handle.
func (c *Conn) Revoked() int    { c.mu.Lock(); defer c.mu.Unlock(); return c.revoked }
func (c *Conn) Closed() int     { c.mu.Lock(); defer c.mu.Unlock(); return c.closed }
func (c *Conn) Terminated() int { c.mu.Lock(); defer c.mu.Unlock(); return c.terminated }

// ListTools implements mcpproxy.RemoteConn: every advertised tool in one page.
func (c *Conn) ListTools(ctx context.Context, _ string) (mcpproxy.RemotePage, error) {
	if err := ctx.Err(); err != nil {
		return mcpproxy.RemotePage{}, err
	}
	c.r.mu.Lock()
	defer c.r.mu.Unlock()
	var page mcpproxy.RemotePage
	for _, n := range c.r.listed {
		page.Tools = append(page.Tools, mcpproxy.RemoteTool{Name: n})
	}
	return page, nil
}

// CallTool implements mcpproxy.RemoteConn.
func (c *Conn) CallTool(ctx context.Context, name string, args json.RawMessage) (mcpproxy.RemoteResult, error) {
	c.mu.Lock()
	bearer := c.bearer
	c.mu.Unlock()
	c.r.mu.Lock()
	tool, ok := c.r.tools[name]
	c.r.calls = append(c.r.calls, Call{Tool: name, Arguments: append(json.RawMessage(nil), args...), Bearer: bearer})
	c.r.mu.Unlock()
	if !ok {
		return mcpproxy.RemoteResult{}, &mcpproxy.RemoteFailure{Reply: true, Code: "unknown_tool"}
	}
	switch {
	case tool.Hold:
		c.r.started <- struct{}{}
		<-ctx.Done()
		return mcpproxy.RemoteResult{}, &mcpproxy.RemoteFailure{Code: "cancelled"}
	case tool.Transport:
		return mcpproxy.RemoteResult{}, &mcpproxy.RemoteFailure{Code: "transport"}
	case tool.Reply != "":
		return mcpproxy.RemoteResult{}, &mcpproxy.RemoteFailure{Reply: true, Code: tool.Reply}
	}
	res := tool.Result
	if res == nil {
		res = json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`)
	}
	return mcpproxy.RemoteResult{JSON: res, IsError: tool.IsError}, nil
}
