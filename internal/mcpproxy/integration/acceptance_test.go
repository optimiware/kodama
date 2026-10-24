// Package integration_test exercises the MCP proxy end to end: real PostgreSQL,
// the real runtime-authority and secrets modules, the pinned MCP SDK on both
// legs and a local TLS MCP server. No NATS, S3 or Git is involved.
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/optimiware/kodama/internal/mcpproxy"
	"github.com/optimiware/kodama/internal/mcpproxy/mcptest"
	"github.com/optimiware/kodama/internal/mcpproxy/protocol"
)

func toolNames(res *mcp.ListToolsResult) []string {
	var out []string
	for _, t := range res.Tools {
		out = append(out, t.Name)
	}
	return out
}

func textOf(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func call(ctx context.Context, cs *mcp.ClientSession, name string, args any) (*mcp.CallToolResult, error) {
	return cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
}

// Test_protocol_smoke: the pinned SDK and protocol revision support
// initialization, the initialized notification, filtered discovery, a call,
// cancellation and session termination; unsupported negotiation fails.
func Test_protocol_smoke(t *testing.T) {
	s := mcptest.New(t, mcptest.Config{Fixture: mcpfixtureOptions(5, 2)})
	r := s.Setup([]string{"echo", "slow", "filler_00", "filler_03"}, []string{"echo", "slow", "filler_03"}, nil)
	ctx := s.Context()

	cs, err := s.Connect(ctx, s.Endpoint(r.W.Attempt, r.Reg.ID), r.W.Token)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()
	if v := cs.InitializeResult().ProtocolVersion; v != protocol.ProtocolRevision {
		t.Fatalf("negotiated %s, want the pinned %s", v, protocol.ProtocolRevision)
	}
	caps := cs.InitializeResult().Capabilities
	if caps.Tools == nil || caps.Resources != nil || caps.Prompts != nil {
		t.Fatalf("only tools may be advertised: %+v", caps)
	}

	t.Run("discovery follows remote pages and filters to the intersection", func(t *testing.T) {
		list, err := cs.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := toolNames(list)
		want := map[string]bool{"echo": true, "slow": true, "filler_03": true}
		if len(got) != len(want) {
			t.Fatalf("tools %v, want %v", got, want)
		}
		for _, n := range got {
			if !want[n] {
				t.Errorf("unexpected tool %s", n)
			}
		}
		if list.NextCursor != "" {
			t.Errorf("the proxy must not forward a remote cursor: %q", list.NextCursor)
		}
		if _, err := cs.ListTools(ctx, &mcp.ListToolsParams{Cursor: "remote-cursor"}); err == nil {
			t.Error("a client-supplied cursor must be refused")
		}
	})

	t.Run("a call reaches the remote with only the integration credential", func(t *testing.T) {
		res, err := call(ctx, cs, "echo", map[string]any{"text": "hi"})
		if err != nil || res.IsError {
			t.Fatalf("call: %v %+v", err, res)
		}
		if textOf(res) != "echo:hi" {
			t.Fatalf("result %q", textOf(res))
		}
		for _, q := range s.Fixture.Requests() {
			if q.Authorization != "Bearer "+s.Fixture.Token() {
				t.Fatalf("the remote saw %q", q.Authorization)
			}
			if strings.Contains(q.Authorization, r.W.Token) {
				t.Fatal("the attempt token was forwarded")
			}
		}
	})

	t.Run("resources and prompts are not relayed", func(t *testing.T) {
		if _, err := cs.ListResources(ctx, nil); err == nil {
			t.Error("resources/list must fail")
		}
		if _, err := cs.ListPrompts(ctx, nil); err == nil {
			t.Error("prompts/list must fail")
		}
	})

	t.Run("cancellation stops local forwarding and propagates to the remote", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			_, err := call(cctx, cs, "slow", map[string]any{"label": "smoke"})
			done <- err
		}()
		waitFor(t, "the remote call to start", func() bool { return len(s.Fixture.Started()) == 1 })
		cancel()
		if err := <-done; err == nil {
			t.Fatal("a cancelled call returned success")
		}
		waitFor(t, "the remote to observe cancellation", func() bool { return len(s.Fixture.Cancelled()) == 1 })
	})

	t.Run("termination closes the session; it cannot be used again", func(t *testing.T) {
		if err := cs.Close(); err != nil {
			t.Logf("close: %v", err)
		}
		cs2, err := s.Connect(ctx, s.Endpoint(r.W.Attempt, r.Reg.ID), r.W.Token)
		if err != nil {
			t.Fatal(err)
		}
		id := cs2.ID()
		if err := cs2.Close(); err != nil {
			t.Logf("close: %v", err)
		}
		status, _ := rawPost(t, s.Endpoint(r.W.Attempt, r.Reg.ID), r.W.Token, id, `{"jsonrpc":"2.0","id":9,"method":"tools/list"}`)
		if status != 404 {
			t.Fatalf("a terminated session answered %d", status)
		}
	})

	t.Run("a remote that negotiates another revision is rejected", func(t *testing.T) {
		old := mcptest.New(t, mcptest.Config{Fixture: mcpfixtureProtocols("2024-11-05")})
		or := old.Setup([]string{"echo"}, []string{"echo"}, nil)
		_, err := old.Connect(old.Context(), old.Endpoint(or.W.Attempt, or.Reg.ID), or.W.Token)
		if err == nil {
			t.Fatal("a session was opened against a remote on an unsupported revision")
		}
		if n := old.Fixture.Executed("echo"); n != 0 {
			t.Fatalf("a tool ran: %d", n)
		}
	})
}

func Test_protocol_pin(t *testing.T) {
	if err := protocol.VerifyCompatibility(); err != nil {
		t.Fatal(err)
	}
	if protocol.SDKVersion != "v1.8.0" || protocol.ProtocolRevision != "2025-11-25" || protocol.Transport != "streamable-http" {
		t.Fatalf("the compatibility pin changed: %s %s %s", protocol.SDKVersion, protocol.ProtocolRevision, protocol.Transport)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var _ = errors.New
var _ = json.Marshal
var _ = mcpproxy.ErrInvalid

// Test_hidden_tool_direct_call: discovery and direct calls enforce the
// registration/profile intersection; foreign projects, missing policy and
// revoked attempts produce no remote call.
func Test_hidden_tool_direct_call(t *testing.T) {
	s := mcptest.New(t, mcptest.Config{})
	// echo: enabled and pinned. mutate: enabled, not pinned. admin_secret: pinned,
	// not enabled. add: neither.
	r := s.Setup([]string{"echo", "mutate"}, []string{"echo", "admin_secret"}, nil)
	ctx := s.Context()
	cs, err := s.Connect(ctx, s.Endpoint(r.W.Attempt, r.Reg.ID), r.W.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(list); len(got) != 1 || got[0] != "echo" {
		t.Fatalf("discovery exposed %v, want only echo", got)
	}
	for _, hidden := range []string{"mutate", "admin_secret", "add", "slow", "echo/../admin_secret", "*", ""} {
		if res, err := call(ctx, cs, hidden, map[string]any{}); err == nil && !res.IsError {
			t.Errorf("hidden tool %q was invoked", hidden)
		}
	}
	for _, tool := range []string{"mutate", "admin_secret", "slow"} {
		if n := s.Fixture.Executed(tool); n != 0 {
			t.Errorf("the remote executed hidden tool %s %d times", tool, n)
		}
	}
	if res, err := call(ctx, cs, "echo", map[string]any{"text": "ok"}); err != nil || res.IsError {
		t.Fatalf("the permitted tool failed: %v", err)
	}

	t.Run("a registration edit removing a tool denies it on the open session", func(t *testing.T) {
		if _, err := s.Service.Edit(ctx, r.W.Scope, mcptest.Manage(), r.Reg.ID, r.Reg.Version,
			mcpproxy.Draft{Endpoint: s.Fixture.URL(), Secret: r.Sec, Tools: []string{"mutate"}}); err != nil {
			t.Fatal(err)
		}
		before := s.Fixture.Executed("echo")
		if res, err := call(ctx, cs, "echo", map[string]any{"text": "again"}); err == nil && !res.IsError {
			t.Fatal("a tool removed from the registration was still invoked")
		}
		if s.Fixture.Executed("echo") != before {
			t.Fatal("the remote ran a tool removed from the registration")
		}
	})

	t.Run("a foreign project cannot open or use the registration", func(t *testing.T) {
		other := s.Scope()
		other.ProjectID = "p033_foreign"
		fw := s.NewWorkload(mcptest.WorkloadSpec{Scope: &other, Run: "run_t033_foreign"})
		s.Policy.Set(fw.Attempt, []string{mcpproxy.Qualify(r.Reg.ID, "echo")}, nil, false)
		before := len(s.Fixture.Requests())
		if _, err := s.Connect(ctx, s.Endpoint(fw.Attempt, r.Reg.ID), fw.Token); err == nil {
			t.Fatal("a foreign project opened the registration")
		}
		if len(s.Fixture.Requests()) != before {
			t.Fatal("a foreign project caused a remote request")
		}
	})

	t.Run("an attempt without a pinned policy gets no tools", func(t *testing.T) {
		nw := s.NewWorkload(mcptest.WorkloadSpec{})
		before := len(s.Fixture.Requests())
		if _, err := s.Connect(ctx, s.Endpoint(nw.Attempt, r.Reg.ID), nw.Token); err == nil {
			t.Fatal("an attempt without policy opened a session")
		}
		if len(s.Fixture.Requests()) != before {
			t.Fatal("missing policy still reached the remote")
		}
	})
}

// Test_revoked_attempt: authority is rechecked at every use; a cancelled attempt
// reaches nothing.
func Test_revoked_attempt(t *testing.T) {
	for _, revoke := range []bool{true, false} {
		name := map[bool]string{true: "revoked grants", false: "execution state only"}[revoke]
		t.Run(name, func(t *testing.T) {
			s := mcptest.New(t, mcptest.Config{})
			r := s.Setup([]string{"echo"}, []string{"echo"}, nil)
			ctx := s.Context()
			cs, err := s.Connect(ctx, s.Endpoint(r.W.Attempt, r.Reg.ID), r.W.Token)
			if err != nil {
				t.Fatal(err)
			}
			defer cs.Close()
			if _, err := call(ctx, cs, "echo", map[string]any{"text": "1"}); err != nil {
				t.Fatal(err)
			}
			if err := s.RT.Authority.Cancel(ctx, r.W.Scope, r.W.Attempt, revoke); err != nil {
				t.Fatal(err)
			}
			before := s.Fixture.Executed("echo")
			if res, err := call(ctx, cs, "echo", map[string]any{"text": "2"}); err == nil && !res.IsError {
				t.Fatal("a cancelled attempt invoked a tool")
			}
			if _, err := s.Connect(ctx, s.Endpoint(r.W.Attempt, r.Reg.ID), r.W.Token); err == nil {
				t.Fatal("a cancelled attempt opened a session")
			}
			if s.Fixture.Executed("echo") != before {
				t.Fatal("the remote ran a tool for a cancelled attempt")
			}
		})
	}
}

func sessionState(t *testing.T, s *mcptest.Stack, id string) string {
	t.Helper()
	var st string
	if err := s.RT.Owner.QueryRow(context.Background(), `SELECT state FROM mcp_sessions WHERE id = $1`, id).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

// Test_foreign_session: copied session identifiers fail across the tenant,
// project, attempt and registration boundaries; expired, closed and
// process-lost sessions cannot resume.
func Test_foreign_session(t *testing.T) {
	s := mcptest.New(t, mcptest.Config{Tune: func(o *mcpproxy.Options) { o.SessionTTL = 2 * time.Minute }})
	r := s.Setup([]string{"echo"}, []string{"echo"}, nil)
	ctx := s.Context()
	cs, err := s.Connect(ctx, s.Endpoint(r.W.Attempt, r.Reg.ID), r.W.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	id := cs.ID()
	body := `{"jsonrpc":"2.0","id":7,"method":"tools/list"}`
	if status, _ := rawPost(t, s.Endpoint(r.W.Attempt, r.Reg.ID), r.W.Token, id, body); status != 200 {
		t.Fatalf("the owner was refused: %d", status)
	}
	secretSeen := func(out string) {
		t.Helper()
		if strings.Contains(out, id) || strings.Contains(out, r.W.Token) {
			t.Fatalf("a response disclosed a session or token: %s", out)
		}
	}
	remoteBefore := len(s.Fixture.Requests())

	t.Run("another attempt of the project", func(t *testing.T) {
		w2 := s.NewWorkload(mcptest.WorkloadSpec{})
		s.Policy.Set(w2.Attempt, []string{mcpproxy.Qualify(r.Reg.ID, "echo")}, nil, false)
		status, out := rawPost(t, s.Endpoint(w2.Attempt, r.Reg.ID), w2.Token, id, body)
		if status != 404 {
			t.Fatalf("copied session accepted: %d %s", status, out)
		}
		secretSeen(out)
		// A token for one attempt on another attempt's route is not valid either.
		if status, _ := rawPost(t, s.Endpoint(r.W.Attempt, r.Reg.ID), w2.Token, id, body); status != 401 {
			t.Fatalf("cross-route token accepted: %d", status)
		}
	})
	t.Run("another tenant", func(t *testing.T) {
		scope := s.Scope()
		scope.TenantID = "t033_globex"
		scope.ProjectID = "p033_globex"
		w3 := s.NewWorkload(mcptest.WorkloadSpec{Scope: &scope, Run: "run_t033_globex"})
		s.Policy.Set(w3.Attempt, []string{mcpproxy.Qualify(r.Reg.ID, "echo")}, nil, false)
		if status, out := rawPost(t, s.Endpoint(w3.Attempt, r.Reg.ID), w3.Token, id, body); status != 404 {
			t.Fatalf("a foreign tenant used the session: %d %s", status, out)
		}
	})
	t.Run("another registration route of the same attempt", func(t *testing.T) {
		reg2 := s.Register(r.W.Scope, r.Sec, "echo")
		s.Policy.Set(r.W.Attempt, []string{mcpproxy.Qualify(r.Reg.ID, "echo"), mcpproxy.Qualify(reg2.ID, "echo")}, nil, false)
		if status, _ := rawPost(t, s.Endpoint(r.W.Attempt, reg2.ID), r.W.Token, id, body); status != 404 {
			t.Fatalf("session reused on another registration: %d", status)
		}
	})
	if len(s.Fixture.Requests()) != remoteBefore {
		t.Fatalf("a rejected request reached the remote server")
	}

	t.Run("a process that did not open it never adopts it", func(t *testing.T) {
		_, other := s.Restart("inc-t033-b")
		status, _ := rawPost(t, other.URL+"/internal/mcp/v1/attempts/"+string(r.W.Attempt)+"/servers/"+string(r.Reg.ID), r.W.Token, id, body)
		if status != 404 {
			t.Fatalf("a different process served the session: %d", status)
		}
		if st := sessionState(t, s, id); st != "invalid" {
			t.Fatalf("session state %s, want invalid", st)
		}
		if status, _ := rawPost(t, s.Endpoint(r.W.Attempt, r.Reg.ID), r.W.Token, id, body); status != 404 {
			t.Fatalf("an invalidated session revived: %d", status)
		}
	})

	t.Run("an expired session cannot resume", func(t *testing.T) {
		cs2, err := s.Connect(ctx, s.Endpoint(r.W.Attempt, r.Reg.ID), r.W.Token)
		if err != nil {
			t.Fatal(err)
		}
		defer cs2.Close()
		id2 := cs2.ID()
		s.RT.Clock.Advance(3 * time.Minute) // past the session lifetime, inside the token lifetime
		if status, _ := rawPost(t, s.Endpoint(r.W.Attempt, r.Reg.ID), r.W.Token, id2, body); status != 404 {
			t.Fatalf("an expired session answered %d", status)
		}
		if st := sessionState(t, s, id2); st != "expired" {
			t.Fatalf("session state %s, want expired", st)
		}
	})
}

// Test_remote_token_audience: only the recipient-bound integration credential
// reaches the approved server; wrong-recipient configuration and redirects
// disclose neither the integration token nor the factory token.
func Test_remote_token_audience(t *testing.T) {
	s := mcptest.New(t, mcptest.Config{})
	r := s.Setup([]string{"echo"}, []string{"echo"}, nil)
	ctx := s.Context()

	t.Run("the approved server receives exactly the integration credential", func(t *testing.T) {
		cs, err := s.Connect(ctx, s.Endpoint(r.W.Attempt, r.Reg.ID), r.W.Token)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		if _, err := call(ctx, cs, "echo", map[string]any{"text": "x"}); err != nil {
			t.Fatal(err)
		}
		reqs := s.Fixture.Requests()
		if len(reqs) == 0 {
			t.Fatal("no request reached the remote")
		}
		for _, q := range reqs {
			if q.Authorization != "Bearer "+s.Fixture.Token() {
				t.Fatalf("the remote saw authorization %q", q.Authorization)
			}
		}
	})

	t.Run("a credential addressed to another recipient is never sent", func(t *testing.T) {
		s2 := mcptest.New(t, mcptest.Config{})
		w := s2.NewWorkload(mcptest.WorkloadSpec{})
		sec := s2.Credential(w.Scope, "wrong-aud", "https://other.example.com", s2.Fixture.Token())
		reg := s2.Register(w.Scope, sec, "echo")
		s2.Policy.Set(w.Attempt, []string{mcpproxy.Qualify(reg.ID, "echo")}, nil, false)
		_, err := s2.Connect(s2.Context(), s2.Endpoint(w.Attempt, reg.ID), w.Token)
		if err == nil {
			t.Fatal("a session was opened with a mis-addressed credential")
		}
		if n := len(s2.Fixture.Requests()); n != 0 {
			t.Fatalf("the mis-addressed credential reached a server (%d requests)", n)
		}
		if strings.Contains(err.Error(), s2.Fixture.Token()) || strings.Contains(err.Error(), w.Token) {
			t.Fatalf("the error disclosed a token: %v", err)
		}
	})

	t.Run("a redirect cannot carry any credential elsewhere", func(t *testing.T) {
		s3 := mcptest.New(t, mcptest.Config{})
		w := s3.NewWorkload(mcptest.WorkloadSpec{})
		sink := mcpfixtureSink(t)
		s3.Fixture.RedirectTo(sink)
		sec := s3.Credential(w.Scope, "redirect-token", s3.Fixture.Origin(), s3.Fixture.Token())
		reg := s3.RegisterAt(w.Scope, s3.Fixture.RedirectURL(), sec, "echo")
		s3.Policy.Set(w.Attempt, []string{mcpproxy.Qualify(reg.ID, "echo")}, nil, false)
		_, err := s3.Connect(s3.Context(), s3.Endpoint(w.Attempt, reg.ID), w.Token)
		if err == nil {
			t.Fatal("a session was opened through a redirect")
		}
		if got := sink.Requests(); len(got) != 0 {
			t.Fatalf("the redirect target received %d requests: %+v", len(got), got)
		}
		if strings.Contains(err.Error(), s3.Fixture.Token()) || strings.Contains(err.Error(), w.Token) {
			t.Fatalf("the error disclosed a token: %v", err)
		}
	})

	t.Run("a revoked credential version is never released", func(t *testing.T) {
		s4 := mcptest.New(t, mcptest.Config{})
		w := s4.NewWorkload(mcptest.WorkloadSpec{})
		sec := s4.Credential(w.Scope, "to-revoke", s4.Fixture.Origin(), s4.Fixture.Token())
		reg := s4.Register(w.Scope, sec, "echo")
		s4.Policy.Set(w.Attempt, []string{mcpproxy.Qualify(reg.ID, "echo")}, nil, false)
		s4.RevokeCredential(w.Scope, "to-revoke", 1)
		if _, err := s4.Connect(s4.Context(), s4.Endpoint(w.Attempt, reg.ID), w.Token); err == nil {
			t.Fatal("a revoked credential opened a session")
		}
		if n := len(s4.Fixture.Requests()); n != 0 {
			t.Fatalf("a revoked credential reached the server: %d", n)
		}
	})
}

// Test_unknown_side_effect: a remote mutation followed by response loss yields an
// unknown outcome and exactly one dispatch, including duplicate delivery and
// recovery.
func Test_unknown_side_effect(t *testing.T) {
	s := mcptest.New(t, mcptest.Config{})
	r := s.Setup([]string{"mutate", "echo"}, []string{"mutate", "echo"}, nil)
	ctx := s.Context()
	cs, err := s.Connect(ctx, s.Endpoint(r.W.Attempt, r.Reg.ID), r.W.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	sid := mcpproxy.SessionID(cs.ID())

	s.Fixture.LoseNextReplies(1)
	res, err := call(ctx, cs, "mutate", map[string]any{"key": "k1"})
	if err != nil {
		t.Fatalf("an unknown outcome is surfaced as a classified result, not a protocol failure: %v", err)
	}
	if !res.IsError || res.Meta["kodama/outcome"] != "unknown" {
		t.Fatalf("outcome not surfaced as unknown: %+v", res)
	}
	reqID, _ := res.Meta["kodama/requestId"].(string)
	if reqID == "" || !strings.Contains(textOf(res), "may or may not") {
		t.Fatalf("the unknown outcome lacks correlation or an honest message: %+v", res)
	}
	if n := s.Fixture.Mutations.Load(); n != 1 {
		t.Fatalf("the effect happened %d times, want exactly once", n)
	}
	calls := 0
	for _, q := range s.Fixture.Requests() {
		if strings.Contains(q.Body, `"tools/call"`) {
			calls++
		}
	}
	if calls != 1 {
		t.Fatalf("the remote received %d tools/call requests: the proxy retried", calls)
	}
	inv, err := s.Store.GetInvocation(ctx, r.W.Scope, sid, mcpproxy.RequestID(reqID))
	if err != nil || inv.State != mcpproxy.InvocationUnknown {
		t.Fatalf("receipt %+v err %v, want unknown", inv, err)
	}

	t.Run("duplicate delivery never dispatches again", func(t *testing.T) {
		_, err := s.Service.Invoke(ctx, r.W.Caller, r.Reg.ID, sid, mcpproxy.InvokeRequest{ID: mcpproxy.RequestID(reqID), Tool: "mutate", Arguments: []byte(`{"key":"k"}`)})
		if !errors.Is(err, mcpproxy.ErrDuplicate) {
			t.Fatalf("duplicate: %v", err)
		}
		var oe *mcpproxy.OutcomeError
		if !errors.As(err, &oe) || oe.State != mcpproxy.InvocationUnknown {
			t.Fatalf("a duplicate must report the stored outcome: %+v", err)
		}
		if n := s.Fixture.Mutations.Load(); n != 1 {
			t.Fatalf("a duplicate dispatched: %d mutations", n)
		}
	})

	t.Run("an admitted call lost in a crash becomes unknown and is never requeued", func(t *testing.T) {
		sess, err := s.Store.GetSession(ctx, r.W.Scope, sid)
		if err != nil {
			t.Fatal(err)
		}
		inv, _ := mcpproxy.NewInvocation("req_t033_crash", sess, "mutate", s.RT.Clock.Now())
		_ = inv.Dispatch(s.RT.Clock.Now())
		if _, err := s.Store.Admit(ctx, mcpproxy.AdmitRequest{Invocation: inv, Incarnation: s.Opts.Incarnation, Now: s.RT.Clock.Now()}); err != nil {
			t.Fatal(err)
		}
		// The process died here: nothing was sent and no outcome was recorded.
		s.RT.Clock.Advance(0)
		time.Sleep(20 * time.Millisecond)
		n, err := s.Store.RecoverStale(ctx, r.W.Scope.TenantID, time.Now().Add(time.Minute), 10)
		if err != nil || n != 1 {
			t.Fatalf("recovered %d receipts, err %v", n, err)
		}
		got, err := s.Store.GetInvocation(ctx, r.W.Scope, sid, "req_t033_crash")
		if err != nil || got.State != mcpproxy.InvocationUnknown {
			t.Fatalf("recovered receipt %+v err %v", got, err)
		}
		if _, err := s.Service.Invoke(ctx, r.W.Caller, r.Reg.ID, sid, mcpproxy.InvokeRequest{ID: "req_t033_crash", Tool: "mutate", Arguments: []byte(`{"key":"k"}`)}); !errors.Is(err, mcpproxy.ErrDuplicate) {
			t.Fatalf("a recovered receipt was dispatched again: %v", err)
		}
		if n := s.Fixture.Mutations.Load(); n != 1 {
			t.Fatalf("a recovery or replay dispatched: %d mutations", n)
		}
		// An unknown receipt is final.
		if _, err := s.RT.Owner.Exec(ctx, `UPDATE mcp_invocations SET state = 'succeeded' WHERE id = 'req_t033_crash'`); err == nil {
			t.Fatal("an unknown outcome was rewritten")
		}
	})

	t.Run("a remote tool error is a final answer, not unknown, and is not retried", func(t *testing.T) {
		s2 := mcptest.New(t, mcptest.Config{})
		r2 := s2.Setup([]string{"fail"}, []string{"fail"}, nil)
		cs2, err := s2.Connect(s2.Context(), s2.Endpoint(r2.W.Attempt, r2.Reg.ID), r2.W.Token)
		if err != nil {
			t.Fatal(err)
		}
		defer cs2.Close()
		res, err := call(s2.Context(), cs2, "fail", map[string]any{})
		if err == nil && (res == nil || !res.IsError) {
			t.Fatalf("a remote failure was reported as success: %+v", res)
		}
		if n := s2.Fixture.Executed("fail"); n != 1 {
			t.Fatalf("the failing tool ran %d times", n)
		}
		var state string
		if err := s2.RT.Owner.QueryRow(context.Background(), `SELECT state FROM mcp_invocations WHERE run_id = $1`, string(r2.W.Run)).Scan(&state); err != nil {
			t.Fatal(err)
		}
		// The remote executed the tool and answered with an error result: the
		// receipt is final and passes the result through, never unknown. A
		// protocol-level error reply is recorded failed (unit-tested).
		if state != "succeeded" || res == nil || !res.IsError {
			t.Fatalf("a remote tool error was recorded %q (result %+v), want a final succeeded receipt with an error result", state, res)
		}
	})
}

// Test_endpoint_authorization: every request of the runtime endpoint
// authenticates before the SDK is involved, and only an initialize request can
// open a session.
func Test_endpoint_authorization(t *testing.T) {
	s := mcptest.New(t, mcptest.Config{})
	r := s.Setup([]string{"echo"}, []string{"echo"}, nil)
	ep := s.Endpoint(r.W.Attempt, r.Reg.ID)
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	before := len(s.Fixture.Requests())

	for name, tc := range map[string]struct {
		method, url, token, session, body string
		want                              int
	}{
		"no bearer":                    {"POST", ep, "", "", init, 401},
		"garbage bearer":               {"POST", ep, "not-a-token", "", init, 401},
		"stream without bearer":        {"GET", ep, "", "x", "", 401},
		"terminate without bearer":     {"DELETE", ep, "", "x", "", 401},
		"stream without a session":     {"GET", ep, r.W.Token, "", "", 400},
		"non-initialize opens nothing": {"POST", ep, r.W.Token, "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, 400},
		"not json":                     {"POST", ep, r.W.Token, "", `{`, 400},
		"unknown registration":         {"POST", s.Endpoint(r.W.Attempt, "mcp_t033_missing"), r.W.Token, "", init, 404},
		"unknown route":                {"POST", s.Server.URL + "/internal/mcp/v1/other", r.W.Token, "", init, 404},
		"invalid attempt identifier":   {"POST", s.Server.URL + "/internal/mcp/v1/attempts/a%20b/servers/" + string(r.Reg.ID), r.W.Token, "", init, 404},
		"oversized initialize":         {"POST", ep, r.W.Token, "", `{"method":"initialize","pad":"` + strings.Repeat("x", 2<<20) + `"}`, 413},
		"session that never existed":   {"POST", ep, r.W.Token, "mcs_t033_does_not_exist_0001", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, 404},
		"malformed session identifier": {"POST", ep, r.W.Token, "x", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, 404},
	} {
		status, out := raw(t, tc.method, tc.url, tc.token, tc.session, tc.body)
		if status != tc.want {
			t.Errorf("%s: %d, want %d: %s", name, status, tc.want, out)
		}
		if strings.Contains(out, r.W.Token) {
			t.Errorf("%s: the response disclosed the token", name)
		}
	}
	if len(s.Fixture.Requests()) != before {
		t.Fatalf("a rejected request reached the remote server")
	}
	if n := count(t, s, `SELECT count(*) FROM mcp_sessions`); n != 0 {
		t.Fatalf("a rejected request left %d sessions", n)
	}
}

func count(t *testing.T, s *mcptest.Stack, sql string) int {
	t.Helper()
	var n int
	if err := s.RT.Owner.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
