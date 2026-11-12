// Package mcpproxy_test pins the optional external MCP proxy surface.
//
// The package declarations Surface, Bounds and DefaultBounds (1 MiB each way)
// are used as they are. This file is the test-side declaration of the minimal
// proxy seam the production package implements.
//
// Pinned package declarations:
//
//	type Proxy struct{ ... }
//	func New(Config) (*Proxy, error)
//	func (*Proxy) Call(context.Context, CallRequest) (CallResult, error)
//
//	type Config struct {
//	    Enabled     bool
//	    Scope       tenancy.Scope
//	    Servers     []Server
//	    Bounds      Bounds
//	    Timeout     time.Duration
//	    Retention   Retention
//	    Credentials authorize.CurrentPrincipal
//	    Authorizer  authorize.Authorizer
//	    Authority   authorize.AuthorityReader
//	    Executions  ExecutionReader
//	    Secrets     secret.Resolver
//	    Transport   Transport
//	    Store       store.Executor
//	    Clock       Clock
//	}
//
//	type Server struct {
//	    Name     string
//	    Endpoint string
//	    Secrets  []id.SecretName
//	    Tools    []Tool
//	}
//
//	type Tool struct {
//	    Server      string
//	    Name        string
//	    Description string
//	    Validate    func(json.RawMessage) error
//	}
//
//	type CallRequest struct {
//	    Credential string
//	    Server     string
//	    Tool       string
//	    Arguments  json.RawMessage
//	}
//
//	type CallResult struct {
//	    Content json.RawMessage
//	    IsError bool
//	}
//
//	type UpstreamRequest struct {
//	    Server      string
//	    Endpoint    string
//	    Tool        string
//	    Arguments   json.RawMessage
//	    Credentials []secret.Resolved
//	    Timeout     time.Duration
//	}
//
//	type UpstreamError struct {
//	    Class      fault.Class
//	    Message    string
//	    Dispatched bool
//	}
//	func (*UpstreamError) Error() string
//
//	type Retention struct {
//	    RawContent bool
//	    Duration   time.Duration
//	}
//	const DefaultRetention = 7 * 24 * time.Hour
//	func (Retention) EffectiveDuration() time.Duration
//
//	type Clock interface {
//	    Now() time.Time
//	    After(time.Duration) <-chan time.Time
//	}
//	type Transport interface {
//	    Call(context.Context, UpstreamRequest) (io.ReadCloser, error)
//	}
//	type ExecutionReader interface {
//	    ReadExecution(context.Context, id.ExecutionID) (authorize.AdmittedExecution, error)
//	}
//
// The proxy is an optional in-process Shell capability. It authenticates with
// authorize.CurrentPrincipal, authorizes the declared "tool.invoke" operation
// with authorize.AuthorizeExecution (execution callers) or
// authorize.Authorizer (human/service callers), re-reads the operation's
// current authority facts through authorize.AuthorityReader, resolves only the
// configured upstream secret names through secret.Resolver, and invokes the
// injected Transport. It never authenticates, authorizes or resolves secrets
// from a request field: CallRequest carries only the credential, configured
// server/tool names and arguments.
//
// Behavior rules pinned by these tests (S08 §1, S09 §5):
//   - Disabled proxy: New succeeds without validating any dependency and
//     initializes nothing. Call returns fault.UnsupportedCapability without
//     authenticating, authorizing, resolving secrets, writing usage or calling
//     the transport.
//   - Enabled proxy: New refuses an invalid trusted scope, no servers, an empty
//     server name/endpoint, no tools, an empty tool name, a nil per-tool
//     validator, duplicate server/tool names, an invalid configured secret name,
//     a request/response bound outside (0, DefaultBounds], a non-positive
//     timeout, a negative retention duration and every nil required dependency.
//     A zero Bounds uses DefaultBounds; a zero Retention.Duration uses
//     DefaultRetention.
//   - Call authenticates, authorizes and reads current authority before it
//     looks up the requested server/tool, resolves a secret, writes usage or
//     calls the transport. A failed credential is fault.Unauthenticated and
//     its error carries no server or tool name. An unknown server or tool
//     after authorization is fault.NotFound. A caller without current
//     authority is fault.Forbidden before the lookup, so it cannot use the
//     error to enumerate names.
//   - Call rejects malformed or oversized arguments with fault.InvalidInput
//     before resolving a secret or calling the transport. The exact
//     MaxRequestBytes is accepted; one byte more is refused.
//   - Call resolves exactly Server.Secrets in Config.Scope. A resolver failure,
//     a missing configured value, an extra returned name or a returned value
//     from another scope is fault.DependencyUnavailable and never reaches the
//     transport.
//   - Immediately before the transport call, Call refreshes current authority.
//     A credential that was revoked after the initial authorization, a caller
//     that lost its grant, an execution that stopped running or an action whose
//     deadline passed is refused with fault.Unauthenticated/fault.Forbidden/
//     fault.DeadlineExpired and makes ZERO transport calls.
//   - An execution caller's current-authority reads carry TargetID = the
//     admitted execution id for "execution_running" and TargetID = the admitted
//     action id for "action_deadline", on both the authorize path and the
//     pre-transport refresh, so a reader keyed on TargetID evaluates the exact
//     execution and action. An admitted execution with no action id is refused
//     with fault.Forbidden when the registry requires the action-deadline fact.
//   - The transport request carries the configured server, endpoint, tool, the
//     exact validated arguments, the resolved configured credentials and the
//     configured timeout. Request fields cannot override any of them.
//   - Failure mapping: a *UpstreamError with Dispatched=false and class
//     DependencyUnavailable is fault.DependencyUnavailable; a *UpstreamError
//     with Dispatched=true, any other transport error, a malformed response or a
//     response larger than MaxResponseBytes is fault.UncertainExternalEffect and
//     is never automatically retried. A pre-effect timeout or cancellation is
//     fault.DeadlineExpired. The exact MaxResponseBytes is accepted; one byte
//     more is refused.
//   - A valid MCP tool-error result (a JSON object with "isError": true) is
//     preserved as CallResult{IsError: true} with a nil Go error.
//   - The transport response body is always closed exactly once, including on
//     success, transport error, oversize response, malformed response,
//     timeout and cancellation.
//   - Resolved credential values are never returned in CallResult.Content, an
//     error message, a usage audit row or an event. If the upstream echoes a
//     credential in its result, the proxy redacts it before returning or
//     recording the content.
//   - Usage is one event.Audit row committed through Config.Store.Execute in a
//     store.Unit whose Scope is Config.Scope. The audit row has Operation
//     "tool.invoke", Actor = the authenticated principal id, At = Clock.Now()
//     and Detail = a metadata-only JSON object with "server", "tool",
//     "outcome" ("ok" or "tool_error"), "request_bytes", "response_bytes",
//     "duration_ms" and "content_retained". Raw content is OFF by default;
//     when Retention.RawContent is true and a settled tool result exists, the
//     same unit also inserts one content_records row (scope, principal,
//     nullable execution id, the configured server/tool identity, the redacted
//     request/response bytes, recorded_at and retain_until =
//     Clock.Now()+Retention.EffectiveDuration()). No credential value appears
//     in the audit detail or the content record.
//   - Every store.Unit the proxy builds conforms to store.SchemaDesign: each
//     written table and column is declared and every required column the
//     adapter does not supply is present, so schema drift is caught by a
//     package test.
//   - Once the transport was invoked, no failure is reported as a retryable
//     pre-effect class. A usage-record failure after a successful result is
//     fault.UncertainExternalEffect, the result is not disclosed and the call is
//     never replayed.
//
// The transport response is a JSON object {"content": <any JSON>,
// "isError": <optional bool>}. Unknown fields, a missing content member or
// invalid JSON is a malformed upstream response.
package mcpproxy_test

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/mcpproxy"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestNewDisabledInitializesNothing proves the optional capability has no
// effect on a plain installation: New validates nothing and every call refuses
// without touching a dependency.
func TestNewDisabledInitializesNothing(t *testing.T) {
	proxy, err := mcpproxy.New(mcpproxy.Config{Enabled: false})
	if err != nil {
		t.Fatalf("New(disabled) returned error: %v", err)
	}
	if proxy == nil {
		t.Fatal("New(disabled) returned a nil proxy")
	}
	if _, err := proxy.Call(context.Background(), mcpproxy.CallRequest{
		Credential: testToken,
		Server:     "files",
		Tool:       "read_file",
		Arguments:  []byte(`{"path":"/etc/hosts"}`),
	}); err == nil {
		t.Fatal("disabled Call returned no error")
	} else {
		assertFaultClass(t, err, fault.UnsupportedCapability)
	}

	// A disabled proxy must not validate a configuration it will never use.
	proxy, err = mcpproxy.New(mcpproxy.Config{
		Enabled: false,
		Scope:   tenancy.Scope{},
		Servers: []mcpproxy.Server{{}},
		Timeout: -time.Second,
	})
	if err != nil {
		t.Fatalf("New(disabled with unused invalid fields) returned error: %v", err)
	}
	if proxy == nil {
		t.Fatal("New(disabled with unused invalid fields) returned a nil proxy")
	}
}

// TestNewRejectsInvalidConfiguration pins the trusted configuration boundary.
// Every failure is a public invalid_input refusal, never a panic or a partial
// proxy.
func TestNewRejectsInvalidConfiguration(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*mcpproxy.Config)
	}{
		{"empty scope", func(config *mcpproxy.Config) { config.Scope = tenancy.Scope{} }},
		{"no servers", func(config *mcpproxy.Config) { config.Servers = nil }},
		{"empty server name", func(config *mcpproxy.Config) { config.Servers[0].Name = "" }},
		{"empty endpoint", func(config *mcpproxy.Config) { config.Servers[0].Endpoint = "" }},
		{"no tools", func(config *mcpproxy.Config) { config.Servers[0].Tools = nil }},
		{"empty tool name", func(config *mcpproxy.Config) { config.Servers[0].Tools[0].Name = "" }},
		{"nil tool validator", func(config *mcpproxy.Config) { config.Servers[0].Tools[0].Validate = nil }},
		{"duplicate server", func(config *mcpproxy.Config) {
			config.Servers = append(config.Servers, config.Servers[0])
		}},
		{"duplicate tool", func(config *mcpproxy.Config) {
			config.Servers[0].Tools = append(config.Servers[0].Tools, config.Servers[0].Tools[0])
		}},
		{"invalid secret name", func(config *mcpproxy.Config) {
			config.Servers[0].Secrets = []id.SecretName{"lower-case"}
		}},
		{"request bound over the declared maximum", func(config *mcpproxy.Config) {
			config.Bounds.MaxRequestBytes = mcpproxy.DefaultBounds.MaxRequestBytes + 1
		}},
		{"response bound over the declared maximum", func(config *mcpproxy.Config) {
			config.Bounds.MaxResponseBytes = mcpproxy.DefaultBounds.MaxResponseBytes + 1
		}},
		{"negative request bound", func(config *mcpproxy.Config) {
			config.Bounds.MaxRequestBytes = -1
		}},
		{"negative response bound", func(config *mcpproxy.Config) {
			config.Bounds.MaxResponseBytes = -1
		}},
		{"zero timeout", func(config *mcpproxy.Config) { config.Timeout = 0 }},
		{"negative timeout", func(config *mcpproxy.Config) { config.Timeout = -time.Second }},
		{"negative retention", func(config *mcpproxy.Config) {
			config.Retention.Duration = -time.Second
		}},
		{"nil credentials", func(config *mcpproxy.Config) { config.Credentials = nil }},
		{"nil authorizer", func(config *mcpproxy.Config) { config.Authorizer = nil }},
		{"nil authority reader", func(config *mcpproxy.Config) { config.Authority = nil }},
		{"nil execution reader", func(config *mcpproxy.Config) { config.Executions = nil }},
		{"nil secret resolver", func(config *mcpproxy.Config) { config.Secrets = nil }},
		{"nil transport", func(config *mcpproxy.Config) { config.Transport = nil }},
		{"nil store", func(config *mcpproxy.Config) { config.Store = nil }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := validConfig()
			testCase.mutate(&config)
			proxy, err := mcpproxy.New(config)
			if err == nil {
				t.Fatalf("New accepted %s", testCase.name)
			}
			if proxy != nil {
				t.Fatalf("New returned a proxy together with the %s refusal", testCase.name)
			}
			assertFaultClass(t, err, fault.InvalidInput)
		})
	}
}

// TestNewAcceptsZeroBoundsAndRetention proves the documented defaults are not
// mistaken for invalid zero values.
func TestNewAcceptsZeroBoundsAndRetention(t *testing.T) {
	config := validConfig()
	config.Bounds = mcpproxy.Bounds{}
	config.Retention = mcpproxy.Retention{}
	proxy, err := mcpproxy.New(config)
	if err != nil {
		t.Fatalf("New with zero bounds/retention returned error: %v", err)
	}
	if proxy == nil {
		t.Fatal("New with zero bounds/retention returned a nil proxy")
	}
}

// TestDefaultRetention pins the seven-day default and the explicit override.
func TestDefaultRetention(t *testing.T) {
	if mcpproxy.DefaultRetention != 7*24*time.Hour {
		t.Fatalf("DefaultRetention = %v; want 7 days", mcpproxy.DefaultRetention)
	}
	if got := (mcpproxy.Retention{}).EffectiveDuration(); got != mcpproxy.DefaultRetention {
		t.Fatalf("zero Retention effective duration = %v; want %v", got, mcpproxy.DefaultRetention)
	}
	if got := (mcpproxy.Retention{Duration: time.Hour}).EffectiveDuration(); got != time.Hour {
		t.Fatalf("explicit Retention effective duration = %v; want 1h", got)
	}
}
