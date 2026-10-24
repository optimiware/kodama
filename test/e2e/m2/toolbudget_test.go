package m2_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/gateway/gatewaytest"
	kit "github.com/optimiware/kodama/test/e2e/m2/kit"
	mcpfixture "github.com/optimiware/kodama/test/support/mcp"
)

// budgetRun runs the stub agent once with the inputs and returns the run, the
// step and the agent's trace (m<status> per model request, i/t<status> per MCP
// call).
func budgetRun(t *testing.T, e *kit.Env, profile, name string, in map[string]any) (client.Run, client.Step, string) {
	t.Helper()
	def := e.Register("budget-"+name, kit.AgentInputs(), e.AgentStepFor("implement", profile))
	item := e.Item("Budget "+name, "# "+name)
	run := e.StartRun("budget-"+name, item, def.Id, in)
	f := e.World.WaitRun(run.Id, 3*time.Minute)
	st := e.StepOf(run.Id, "implement")
	return f, st, kit.Output(st, "trace")
}

func inputs(e *kit.Env, kv ...string) map[string]any {
	in := e.AgentRunInputs("completed")
	in["model_calls"], in["tool_calls"] = "0", "0"
	for i := 0; i+1 < len(kv); i += 2 {
		in[kv[i]] = kv[i+1]
	}
	return in
}

func calls(e *kit.Env, run string) []string {
	return e.Dump(`SELECT state || ':' || coalesce(outcome_code, '') FROM model_requests WHERE run_id = $1 ORDER BY ordinal`, run)
}

// TestM2ToolBudget (m2_tool_budget) is bounded model and tool use by a real
// agent attempt on Podman through the real gateway and MCP proxy over
// PostgreSQL, with a deterministic provider and remote: the attempt holds
// model.invoke/tool.invoke only because the operator's ceiling grants them and
// its pinned profile names a model and tools; request and token allowances
// stop NEW admission and keep the result and the usage; a hidden tool fails
// direct invocation; an unknown tool outcome is surfaced and never retried; an
// interrupted stream is a partial failure that keeps the run's slot; two
// replicas cannot admit concurrent requests of one run; cancellation reaches
// the provider and ends the authority; redirects carry no credential
// elsewhere; the provider credential stays server side; recording stays off
// while usage metadata remains. Covers AR-A02, AR-A03, AR-A06, GM-A01–GM-A08,
// AR-04, AR-05, AR-09, AR-10, GM-01–GM-12 (the pinned protocol revision).
func TestM2ToolBudget(t *testing.T) {
	calls3 := int64(3)
	e := kit.New(t, kit.Options{ProfileLimits: &agents.Limits{WallSeconds: 300, MaxToolCalls: &calls3}})

	t.Run("a profile claiming an allowance nothing binds is unsupported", func(t *testing.T) {
		req := int64(3)
		d := e.ProfileDraft("claims-requests", nil)
		d.Limits.MaxRequests = &req
		code, body := e.Do("engineer", "POST", e.ProjectPath()+"/agent-profiles", d, nil)
		if code != http.StatusUnprocessableEntity || kit.ErrorCode(body) != "profile_unsupported" {
			t.Fatalf("a claimed request allowance: %d %s: the gateway enforces project settings, so the control is not qualified (AR-02)", code, body)
		}
	})

	t.Run("the request allowance stops new admission and keeps the result and usage", func(t *testing.T) {
		e.SetModelLimits(&kit.ModelLimits{MaxRequests: 3, TokenAllowance: 100000, MaxRequestBytes: 8192, MaxInputTokens: 4096, MaxOutputTokens: 256})
		before := len(e.Provider.Calls())
		f, st, trace := budgetRun(t, e, e.Profile, "requests", inputs(e, "model_calls", "5"))
		if f.Status != client.RunStatusSucceeded || trace != "m200,m200,m200,m403,m403" {
			t.Fatalf("run %s (%s) trace %q", f.Status, f.Reason, trace)
		}
		if got := len(e.Provider.Calls()) - before; got != 3 {
			t.Fatalf("the provider received %d requests, want 3", got)
		}
		// Usage is retained per admitted request, observed and charged exactly.
		rows := e.Dump(`SELECT basis || ':' || uncertainty || ':' || input_tokens || ':' || output_tokens || ':' || charged_tokens || ':' || outcome FROM model_usage WHERE run_id = $1 ORDER BY recorded_at`, f.Id)
		if len(rows) != 3 || rows[0] != "observed:none:10:3:13:succeeded" {
			t.Fatalf("usage %v", rows)
		}
		if m2 := kit.Output(st, "summary"); m2 == "" || st.Status != client.StepStatusSucceeded {
			t.Fatalf("the agent's result was not preserved: %+v", st)
		}
		// The provider saw the provider key, and only it; the workload's token never leaves the server.
		for _, c := range e.Provider.Calls()[before:] {
			if c.Authorization != "Bearer "+kit.ProviderKey {
				t.Fatalf("the provider got authorization %q", c.Authorization)
			}
		}
	})

	t.Run("the token allowance is bounded and its overshoot is at most one request", func(t *testing.T) {
		e.SetModelLimits(&kit.ModelLimits{MaxRequests: 40, TokenAllowance: 100, MaxRequestBytes: 8192, MaxInputTokens: 4096, MaxOutputTokens: 256})
		f, _, trace := budgetRun(t, e, e.Profile, "tokens", inputs(e, "model_calls", "12"))
		want := strings.Repeat("m200,", 8) + "m403,m403,m403,m403"
		if f.Status != client.RunStatusSucceeded || trace != want {
			t.Fatalf("run %s trace %q, want %q", f.Status, trace, want)
		}
		var charged, bound int64
		e.MustScan(&charged, `SELECT sum(charged_tokens) FROM model_usage WHERE run_id = $1`, f.Id)
		e.MustScan(&bound, `SELECT max(charge_bound) FROM model_requests WHERE run_id = $1`, f.Id)
		// 13 tokens a request: the eighth is admitted below 100 (91), charged 104; the ninth is refused.
		if charged != 104 || charged >= 100-1+bound+bound {
			t.Fatalf("charged %d (request bound %d)", charged, bound)
		}
	})

	t.Run("hidden tools fail direct invocation and the call ceiling holds", func(t *testing.T) {
		e.SetModelLimits(nil)
		mutBefore, secretBefore := e.MCP.Mutations.Load(), e.MCP.Executed("admin_secret")
		f, _, trace := budgetRun(t, e, e.Profile, "tools", inputs(e, "tool_calls", "7", "tool", "echo,admin_secret,mutate,echo,echo,echo,echo"))
		if f.Status != client.RunStatusSucceeded {
			t.Fatalf("run %s (%s)", f.Status, f.Reason)
		}
		if e.MCP.Executed("admin_secret") != secretBefore || e.MCP.Mutations.Load() != mutBefore {
			t.Fatalf("a hidden tool reached the remote: admin_secret %d, mutate %d", e.MCP.Executed("admin_secret")-secretBefore, e.MCP.Mutations.Load()-mutBefore)
		}
		// The unregistered tool and the registered but profile-hidden one fail like any unknown tool and
		// are never admitted; the visible one works three times (the profile's ceiling); the rest are refused.
		want := "i200,t200,t200rpcerr,t200rpcerr,t200,t200,t200err,t200err"
		if trace != want {
			t.Fatalf("trace %q, want %q", trace, want)
		}
		var admitted int
		e.MustScan(&admitted, `SELECT calls FROM mcp_run_calls WHERE run_id = $1`, f.Id)
		if admitted != 3 {
			t.Fatalf("%d calls admitted, want the profile's ceiling of 3", admitted)
		}
	})

	t.Run("an unknown tool outcome is surfaced and never retried blindly", func(t *testing.T) {
		writer := e.RegisterProfileFor("stub-writer", nil, e.Registration+"/echo", e.Registration+"/mutate")
		before := e.MCP.Mutations.Load()
		e.MCP.LoseNextReplies(1)
		f, st, _ := budgetRun(t, e, writer, "unknown", inputs(e, "tool_calls", "1", "tool", "mutate"))
		if f.Status != client.RunStatusSucceeded {
			t.Fatalf("run %s (%s)", f.Status, f.Reason)
		}
		if got := e.MCP.Mutations.Load() - before; got != 1 {
			t.Fatalf("the side effect ran %d times for one call: a lost reply was retried", got)
		}
		logs := strings.Join(e.AttemptLogs("engineer", f.Id, st.Attempts[0].Id), "\n")
		if !strings.Contains(logs, `"kodama/outcome":"unknown"`) {
			t.Fatalf("the unknown outcome was not surfaced to the agent:\n%s", logs)
		}
		if rows := e.Dump(`SELECT state FROM mcp_invocations WHERE run_id = $1`, f.Id); len(rows) != 1 || rows[0] != "unknown" {
			t.Fatalf("invocation states %v", rows)
		}
	})

	t.Run("an interrupted stream is a partial failure that keeps the slot", func(t *testing.T) {
		e.SetModelLimits(nil)
		before := len(e.Provider.Calls())
		e.Provider.Script(gatewaytest.Truncated("t038-model", "partial"))
		f, _, trace := budgetRun(t, e, e.Profile, "interrupted", inputs(e, "model_calls", "2"))
		if f.Status != client.RunStatusSucceeded || trace != "m200x,m409" {
			t.Fatalf("run %s trace %q: the partial stream is exposed and the next request is refused while the outcome is unknown", f.Status, trace)
		}
		if got := len(e.Provider.Calls()) - before; got != 1 {
			t.Fatalf("the provider received %d requests: a retry or fallback replaced the stream", got)
		}
		if rows := calls(e, f.Id); len(rows) != 1 || rows[0] != "unknown:stream_truncated" {
			t.Fatalf("requests %v", rows)
		}
		rows := e.Dump(`SELECT basis || ':' || uncertainty || ':' || charged_tokens FROM model_usage WHERE run_id = $1`, f.Id)
		if len(rows) != 1 || !strings.HasPrefix(rows[0], "estimated:stream_interrupted:") {
			t.Fatalf("usage %v: an unknown completion is charged conservatively", rows)
		}
	})

	t.Run("two replicas cannot admit concurrent requests of one run", func(t *testing.T) {
		e.SetModelLimits(nil)
		replica := e.StartReplica()
		started, release := make(chan struct{}), make(chan struct{})
		e.Provider.Script(e.Provider.Hang("t038-model", started, release, "partial"))
		def := e.Register("budget-concurrent", kit.AgentInputs(), e.AgentStepFor("implement", e.Profile))
		item := e.Item("Concurrent requests", "# spec")
		run := e.StartRun("budget-concurrent", item, def.Id, inputs(e, "model_calls", "1"))
		live := e.WaitLive(run.Id)
		select {
		case <-started:
		case <-time.After(60 * time.Second):
			t.Fatal("the first request never reached the provider")
		}
		before := len(e.Provider.Calls())
		body := []byte(`{"model":"` + kit.ModelName + `","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"second"}]}`)
		path := "/gateway/v1/attempts/" + live.Attempt + "/chat/completions"
		for name, base := range map[string]string{"the other replica": "http://" + replica.Addr(), "the same replica": e.Base()} {
			if code, b := e.Workload(base, "POST", path, live.Token, body, nil); code != http.StatusConflict {
				t.Fatalf("a second in-flight request through %s: %d %s", name, code, b)
			}
		}
		if got := len(e.Provider.Calls()); got != before || e.Provider.MaxConcurrent() != 1 {
			t.Fatalf("the provider saw %d concurrent requests (calls %d -> %d)", e.Provider.MaxConcurrent(), before, got)
		}
		close(release)
		e.World.WaitRun(run.Id, 3*time.Minute)
	})

	t.Run("cancellation reaches the provider and ends the attempt's authority", func(t *testing.T) {
		e.SetModelLimits(nil)
		started := make(chan struct{})
		cancelledBefore := e.Provider.Cancelled()
		e.Provider.Script(e.Provider.Hang("t038-model", started, nil, "partial"))
		def := e.Register("budget-cancel", kit.AgentInputs(), e.AgentStepFor("implement", e.Profile))
		item := e.Item("Cancelled agent", "# spec")
		run := e.StartRun("budget-cancel", item, def.Id, inputs(e, "model_calls", "1", "tool_calls", "0"))
		live := e.WaitLive(run.Id)
		select {
		case <-started:
		case <-time.After(60 * time.Second):
			t.Fatal("the request never reached the provider")
		}
		if r := e.World.CancelRun(run.Id); r.StatusCode() != http.StatusOK && r.StatusCode() != http.StatusAccepted {
			t.Fatalf("cancel: %s %s", r.Status(), r.Body)
		}
		if f := e.World.WaitRun(run.Id, 2*time.Minute); f.Status != client.RunStatusCancelled {
			t.Fatalf("run %s (%s)", f.Status, f.Reason)
		}
		e.World.WaitRemoved(live.Attempt) // the wrapper and its process tree are gone
		kit.WaitFor(t, "the cancellation to reach the provider", 30*time.Second, func() bool { return e.Provider.Cancelled() > cancelledBefore })
		// The attempt's authority is revoked: its own token buys nothing, anywhere.
		body := []byte(`{"model":"` + kit.ModelName + `","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"late"}]}`)
		providerCalls := len(e.Provider.Calls())
		if code, b := e.Workload(e.Base(), "POST", "/gateway/v1/attempts/"+live.Attempt+"/chat/completions", live.Token, body, nil); code != http.StatusUnauthorized && code != http.StatusForbidden && code != http.StatusNotFound {
			t.Fatalf("a revoked attempt's model request: %d %s", code, b)
		}
		mcpBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"late","version":"1"}}}`)
		if code, b := e.Workload(e.Base(), "POST", "/internal/mcp/v1/attempts/"+live.Attempt+"/servers/"+e.Registration, live.Token, mcpBody, map[string]string{"Accept": "application/json, text/event-stream"}); code != http.StatusUnauthorized && code != http.StatusForbidden && code != http.StatusNotFound {
			t.Fatalf("a revoked attempt's MCP request: %d %s", code, b)
		}
		if len(e.Provider.Calls()) != providerCalls {
			t.Fatal("the provider received a request after the cancellation")
		}
		rows := calls(e, run.Id)
		if len(rows) != 1 || !strings.HasPrefix(rows[0], "cancelled:") && !strings.HasPrefix(rows[0], "unknown:") {
			t.Fatalf("the cancelled request is %v", rows)
		}
	})

	t.Run("a copied or foreign remote session is denied", func(t *testing.T) {
		e.SetModelLimits(nil)
		foreign := e.ForeignMCPRegistration()
		def := e.Register("budget-session", kit.AgentInputs(), e.AgentStepFor("implement", e.Profile))
		start := func(name string) kit.Live {
			item := e.Item("Session "+name, "# spec")
			run := e.StartRun("budget-session-"+name, item, def.Id, inputs(e, "tool_calls", "1", "hold", "40"))
			l := e.WaitLive(run.Id)
			kit.WaitFor(t, "the MCP session of "+name, time.Minute, func() bool { return e.ReadInContainer(l, "/tmp/trace") != "" })
			return l
		}
		x, y := start("x"), start("y")
		sx := e.ReadInContainer(x, "/tmp/mcp-session")
		if sx == "" {
			t.Fatal("attempt x has no session")
		}
		call := []byte(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"echo","arguments":{"text":"copied"}}}`)
		hdr := map[string]string{"Accept": "application/json, text/event-stream", "Mcp-Session-Id": sx}
		remote := len(e.MCP.Requests())
		// Another attempt's token with x's session id; x's session id under another registration;
		// and an attempt's own session on a registration of another tenant.
		if code, b := e.Workload(e.Base(), "POST", "/internal/mcp/v1/attempts/"+y.Attempt+"/servers/"+e.Registration, y.Token, call, hdr); code != http.StatusNotFound {
			t.Fatalf("a copied session under another attempt: %d %s", code, b)
		}
		if code, b := e.Workload(e.Base(), "POST", "/internal/mcp/v1/attempts/"+x.Attempt+"/servers/"+e.Registration, y.Token, call, hdr); code == http.StatusOK {
			t.Fatalf("another attempt's token on x's path: %d %s", code, b)
		}
		init := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"foreign","version":"1"}}}`)
		if code, b := e.Workload(e.Base(), "POST", "/internal/mcp/v1/attempts/"+x.Attempt+"/servers/"+foreign, x.Token, init, map[string]string{"Accept": "application/json, text/event-stream"}); code != http.StatusNotFound {
			t.Fatalf("a registration of another tenant: %d %s", code, b)
		}
		if got := len(e.MCP.Requests()); got != remote {
			t.Fatalf("the remote received %d requests for denied calls", got-remote)
		}
		// x's own session still works while its attempt lives, and ends with it.
		if code, b := e.Workload(e.Base(), "POST", "/internal/mcp/v1/attempts/"+x.Attempt+"/servers/"+e.Registration, x.Token, call, hdr); code != http.StatusOK {
			t.Fatalf("the owner's session: %d %s", code, b)
		}
		e.World.CancelRun(x.Run)
		e.World.CancelRun(y.Run)
		e.World.WaitRun(x.Run, time.Minute)
		e.World.WaitRun(y.Run, time.Minute)
		if code, _ := e.Workload(e.Base(), "POST", "/internal/mcp/v1/attempts/"+x.Attempt+"/servers/"+e.Registration, x.Token, call, hdr); code == http.StatusOK {
			t.Fatalf("an ended attempt's session still answers")
		}
	})

	t.Run("redirects carry no credential to another destination", func(t *testing.T) {
		e.SetModelLimits(nil)
		sink := mcpfixture.NewSink(t)
		e.MCP.RedirectTo(sink)
		redirecting := e.RegisterMCPServer(e.MCP.RedirectURL(), "echo")
		p := e.RegisterProfileFor("stub-redirect", nil, redirecting+"/echo")
		e.Provider.Script(gatewaytest.Redirect(http.StatusTemporaryRedirect, sink.URL()+"/v1/chat/completions"))
		in := inputs(e, "model_calls", "1", "tool_calls", "1", "registration", redirecting)
		f, _, trace := budgetRun(t, e, p, "redirect", in)
		if f.Status != client.RunStatusSucceeded || strings.Contains(trace, "m200") || strings.Contains(trace, "t200") || strings.Contains(trace, "i200") {
			t.Fatalf("run %s trace %q: a redirect must fail the request", f.Status, trace)
		}
		if got := sink.Requests(); len(got) != 0 {
			t.Fatalf("the redirect target received %d requests (credentials exfiltrated): %+v", len(got), got)
		}
	})

	t.Run("credentials stay behind the gateway and recording is off while usage remains", func(t *testing.T) {
		if code, b := e.Do("admin", "PUT", e.ProjectPath()+"/model-settings", map[string]any{
			"enabled": true, "endpoint": e.Provider.Endpoint(), "credential": map[string]any{"ref": "secret://project/" + kit.ModelSecret, "version": 1},
			"models": []string{kit.ModelName}, "limits": kit.DefaultLimits, "recording": true,
		}, map[string]string{"If-Match": fmt.Sprintf(`"%d"`, e.SettingsVersion())}); code < 400 {
			t.Fatalf("recording was enabled without a content recorder: %d %s", code, b)
		}
		// Everything the platform persisted or published carries usage metadata but never the
		// provider credential, the workload's token or the prompt text.
		var n int
		e.MustScan(&n, `SELECT count(*) FROM model_usage`)
		if n == 0 {
			t.Fatal("no usage metadata remained")
		}
		for _, secret := range []string{kit.ProviderKey, "implement the work item, request"} {
			for _, q := range []string{
				`SELECT count(*) FROM outbox WHERE position($1 in convert_from(payload, 'UTF8')) > 0`,
				`SELECT count(*) FROM audit WHERE position($1 in metadata::text) > 0 OR position($1 in coalesce(reason, '')) > 0`,
			} {
				if hits := e.TenantCount(kit.TenantA, q, secret); hits != 0 {
					t.Fatalf("%q appears in persisted events or audit (%s)", secret, q)
				}
			}
		}
	})
}
