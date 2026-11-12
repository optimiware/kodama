// Package mcpproxy declares the optional external MCP tool surface.
//
// The proxy is an optional in-process Shell capability. A surface fixes the
// operation name, the configured tool allowlist and the request-size bound; the
// proxy never exposes tools outside that allowlist and never invents authority
// beyond the shared operation registry.
package mcpproxy

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Surface is one configured external MCP tool surface.
type Surface struct {
	Operation       string   `json:"operation"`
	AllowedTools    []string `json:"allowed_tools"`
	MaxRequestBytes int      `json:"max_request_bytes"`
}

// Bounds is the MCP proxy request/response bound set.
type Bounds struct {
	MaxRequestBytes  int `json:"max_request_bytes"`
	MaxResponseBytes int `json:"max_response_bytes"`
}

// DefaultBounds is the declared MCP proxy bound set: 1 MiB each way.
var DefaultBounds = Bounds{
	MaxRequestBytes:  1 << 20,
	MaxResponseBytes: 1 << 20,
}

// Validate checks that the surface names an operation, at least one configured
// tool and a request bound inside the proxy bound.
func (surface Surface) Validate() error {
	if surface.Operation == "" {
		return fault.New(fault.InvalidInput, "tool surface operation is empty")
	}
	if len(surface.AllowedTools) == 0 {
		return fault.New(fault.InvalidInput, "tool surface has no allowed tools")
	}
	for _, tool := range surface.AllowedTools {
		if tool == "" {
			return fault.New(fault.InvalidInput, "tool surface has an empty allowed tool")
		}
	}
	if surface.MaxRequestBytes <= 0 || surface.MaxRequestBytes > DefaultBounds.MaxRequestBytes {
		return fault.New(fault.InvalidInput, fmt.Sprintf("tool surface request bound %d is outside (0, %d]", surface.MaxRequestBytes, DefaultBounds.MaxRequestBytes))
	}
	return nil
}

// Clock is the injected authoritative time source and timeout signal. Every
// audit timestamp and the pre-effect timeout come from it, so a test can pin a
// deterministic time and fire the configured timeout without a wall clock.
type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

// IDSource allocates the durable audit identity one usage commit carries.
// Identity generation is injectable so production can use secure randomness
// while a test can use a deterministic sequence. A nil source uses crypto/rand.
type IDSource interface {
	NewEventID() (id.EventID, error)
}

// Config is the complete set of trusted configuration and injected boundaries
// one optional proxy needs. The zero value with Enabled false initializes
// nothing and validates nothing.
type Config struct {
	Enabled     bool
	Scope       tenancy.Scope
	Servers     []Server
	Bounds      Bounds
	Timeout     time.Duration
	Retention   Retention
	Credentials authorize.CurrentPrincipal
	Authorizer  authorize.Authorizer
	Authority   authorize.AuthorityReader
	Executions  ExecutionReader
	Secrets     secret.Resolver
	Transport   Transport
	Store       store.Executor
	Clock       Clock
	IDs         IDSource
}

// Server is one configured external MCP upstream and its allowed tools.
type Server struct {
	Name     string
	Endpoint string
	Secrets  []id.SecretName
	Tools    []Tool
}

// Tool is one configured upstream tool. Validate admits the tool arguments
// before any secret is resolved or any effect is dispatched.
type Tool struct {
	Server      string
	Name        string
	Description string
	Validate    func(json.RawMessage) error
}

// CallRequest is one caller request. It carries only the presented credential,
// configured server/tool names and arguments; it can never supply authority,
// an endpoint or a secret name.
type CallRequest struct {
	Credential string
	Server     string
	Tool       string
	Arguments  json.RawMessage
}

// CallResult is one settled upstream tool result. Content is the redacted inner
// MCP content value.
type CallResult struct {
	Content json.RawMessage
	IsError bool
}

// UpstreamRequest is the exact trusted request one Transport receives. Every
// field comes from configuration or the validated request, never from an
// unvalidated caller-supplied override.
type UpstreamRequest struct {
	Server      string
	Endpoint    string
	Tool        string
	Arguments   json.RawMessage
	Credentials []secret.Resolved
	Timeout     time.Duration
}

// UpstreamError is one classified transport failure. Dispatched reports whether
// the request may already have reached the upstream; an ambiguous dispatched
// outcome is never replayed.
type UpstreamError struct {
	Class      fault.Class
	Message    string
	Dispatched bool
}

// Error renders the diagnostic message. It is transport diagnostics only and is
// never returned verbatim to a proxy caller.
func (upstream *UpstreamError) Error() string {
	if upstream == nil {
		return ""
	}
	return upstream.Message
}

// Retention fixes whether raw tool content is retained and for how long. Raw
// content is off by default; an explicit opt-in retains redacted content for a
// bounded duration.
type Retention struct {
	RawContent bool
	Duration   time.Duration
}

// DefaultRetention is the seven-day default retention window.
const DefaultRetention = 7 * 24 * time.Hour

// EffectiveDuration returns the configured retention duration, or the declared
// seven-day default when none is configured.
func (retention Retention) EffectiveDuration() time.Duration {
	if retention.Duration <= 0 {
		return DefaultRetention
	}
	return retention.Duration
}
