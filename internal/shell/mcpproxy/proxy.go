package mcpproxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// toolInvoke is the one registry operation the proxy authorizes. The proxy is
// an optional capability, not a second operation edge: it never invents an
// operation or a grant.
const toolInvoke = authorize.OperationName("tool.invoke")

// redactionMarker replaces every resolved credential value before content is
// returned or recorded.
const redactionMarker = "[redacted]"

// Proxy is the optional external MCP tool proxy. Every field is immutable after
// New, so concurrent calls share it safely.
type Proxy struct {
	enabled   bool
	scope     tenancy.Scope
	servers   []Server
	byServer  map[string]int
	byTool    map[string]map[string]int
	bounds    Bounds
	timeout   time.Duration
	retention Retention
	operation authorize.Operation

	credentials authorize.CurrentPrincipal
	authorizer  authorize.Authorizer
	authority   authorize.AuthorityReader
	executions  ExecutionReader
	secrets     secret.Resolver
	transport   Transport
	store       store.Executor
	clock       Clock
	ids         IDSource
}

// New validates the trusted configuration and returns the proxy. A disabled
// proxy validates nothing and initializes nothing, so an installation without
// the optional capability has no external surface at all.
func New(config Config) (*Proxy, error) {
	if !config.Enabled {
		return &Proxy{}, nil
	}
	if err := config.Scope.Validate(); err != nil {
		return nil, fault.New(fault.InvalidInput, "tool proxy scope is invalid")
	}
	if len(config.Servers) == 0 {
		return nil, fault.New(fault.InvalidInput, "tool proxy has no configured servers")
	}
	bounds, err := resolveBounds(config.Bounds)
	if err != nil {
		return nil, err
	}
	if config.Timeout <= 0 {
		return nil, fault.New(fault.InvalidInput, "tool proxy timeout is not positive")
	}
	if config.Retention.Duration < 0 {
		return nil, fault.New(fault.InvalidInput, "tool proxy retention duration is negative")
	}
	switch {
	case config.Credentials == nil:
		return nil, fault.New(fault.InvalidInput, "tool proxy requires a credential resolver")
	case config.Authorizer == nil:
		return nil, fault.New(fault.InvalidInput, "tool proxy requires an authorizer")
	case config.Authority == nil:
		return nil, fault.New(fault.InvalidInput, "tool proxy requires an authority reader")
	case config.Executions == nil:
		return nil, fault.New(fault.InvalidInput, "tool proxy requires an execution reader")
	case config.Secrets == nil:
		return nil, fault.New(fault.InvalidInput, "tool proxy requires a secret resolver")
	case config.Transport == nil:
		return nil, fault.New(fault.InvalidInput, "tool proxy requires a transport")
	case config.Store == nil:
		return nil, fault.New(fault.InvalidInput, "tool proxy requires a usage store")
	case config.Clock == nil:
		return nil, fault.New(fault.InvalidInput, "tool proxy requires a clock")
	}

	operation, ok := authorize.LookupOperation(toolInvoke)
	if !ok {
		return nil, fault.New(fault.InternalFailure, "tool.invoke is not registered")
	}

	servers := make([]Server, 0, len(config.Servers))
	byServer := make(map[string]int, len(config.Servers))
	byTool := make(map[string]map[string]int, len(config.Servers))
	for _, configured := range config.Servers {
		if configured.Name == "" {
			return nil, fault.New(fault.InvalidInput, "tool server name is empty")
		}
		if configured.Endpoint == "" {
			return nil, fault.New(fault.InvalidInput, "tool server endpoint is empty")
		}
		if _, exists := byServer[configured.Name]; exists {
			return nil, fault.New(fault.InvalidInput, "tool server name is configured twice")
		}
		if len(configured.Tools) == 0 {
			return nil, fault.New(fault.InvalidInput, "tool server has no configured tools")
		}
		secrets := append([]id.SecretName(nil), configured.Secrets...)
		seenSecrets := make(map[id.SecretName]bool, len(secrets))
		for _, name := range secrets {
			if _, err := id.ParseSecretName(string(name)); err != nil {
				return nil, fault.New(fault.InvalidInput, "tool server secret name is invalid")
			}
			if seenSecrets[name] {
				return nil, fault.New(fault.InvalidInput, "tool server secret name is configured twice")
			}
			seenSecrets[name] = true
		}
		tools := make([]Tool, 0, len(configured.Tools))
		toolIndex := make(map[string]int, len(configured.Tools))
		for _, configuredTool := range configured.Tools {
			if configuredTool.Name == "" {
				return nil, fault.New(fault.InvalidInput, "tool name is empty")
			}
			if configuredTool.Validate == nil {
				return nil, fault.New(fault.InvalidInput, "tool has no argument validator")
			}
			if _, exists := toolIndex[configuredTool.Name]; exists {
				return nil, fault.New(fault.InvalidInput, "tool name is configured twice on one server")
			}
			tool := configuredTool
			tool.Server = configured.Name
			toolIndex[tool.Name] = len(tools)
			tools = append(tools, tool)
		}
		byServer[configured.Name] = len(servers)
		byTool[configured.Name] = toolIndex
		servers = append(servers, Server{
			Name:     configured.Name,
			Endpoint: configured.Endpoint,
			Secrets:  secrets,
			Tools:    tools,
		})
	}

	return &Proxy{
		enabled:     true,
		scope:       config.Scope,
		servers:     servers,
		byServer:    byServer,
		byTool:      byTool,
		bounds:      bounds,
		timeout:     config.Timeout,
		retention:   config.Retention,
		operation:   operation,
		credentials: config.Credentials,
		authorizer:  config.Authorizer,
		authority:   config.Authority,
		executions:  config.Executions,
		secrets:     config.Secrets,
		transport:   config.Transport,
		store:       config.Store,
		clock:       config.Clock,
		ids:         config.IDs,
	}, nil
}

// resolveBounds validates the configured bounds. The whole zero value means the
// declared default; a partly configured set must name both positive bounds
// inside the declared maximum.
func resolveBounds(configured Bounds) (Bounds, error) {
	if configured.MaxRequestBytes == 0 && configured.MaxResponseBytes == 0 {
		return DefaultBounds, nil
	}
	if configured.MaxRequestBytes <= 0 || configured.MaxRequestBytes > DefaultBounds.MaxRequestBytes {
		return Bounds{}, fault.New(fault.InvalidInput, "tool proxy request bound is outside (0, default]")
	}
	if configured.MaxResponseBytes <= 0 || configured.MaxResponseBytes > DefaultBounds.MaxResponseBytes {
		return Bounds{}, fault.New(fault.InvalidInput, "tool proxy response bound is outside (0, default]")
	}
	return configured, nil
}

// Call authenticates and authorizes the caller, looks up the configured
// server/tool, validates the arguments, resolves only the configured secrets,
// refreshes current authority immediately before the effect, performs exactly
// one bounded transport call and records one attributable usage row.
func (proxy *Proxy) Call(ctx context.Context, request CallRequest) (CallResult, error) {
	if proxy == nil || !proxy.enabled {
		return CallResult{}, fault.New(fault.UnsupportedCapability, "external MCP proxy is not enabled")
	}
	started := proxy.clock.Now()
	// Arm the configured timeout before any dependency work so a pre-effect
	// expiry is observed even while a slow dependency blocks the call.
	timeout := proxy.clock.After(proxy.timeout)

	if _, err := proxy.authorizeCaller(ctx, request.Credential); err != nil {
		return CallResult{}, err
	}

	server, ok := proxy.server(request.Server)
	if !ok {
		return CallResult{}, fault.New(fault.NotFound, "tool server is not configured")
	}
	tool, ok := proxy.tool(request.Server, request.Tool)
	if !ok {
		return CallResult{}, fault.New(fault.NotFound, "tool is not configured")
	}

	arguments := append(json.RawMessage(nil), request.Arguments...)
	if len(arguments) > proxy.bounds.MaxRequestBytes {
		return CallResult{}, fault.New(fault.InvalidInput, "tool arguments exceed the configured request bound")
	}
	if err := tool.Validate(arguments); err != nil {
		return CallResult{}, fault.New(fault.InvalidInput, "tool arguments are invalid")
	}

	resolved, err := proxy.resolveSecrets(ctx, server.Secrets)
	if err != nil {
		return CallResult{}, err
	}

	// Refresh current authority immediately before the effect. A grant, an
	// execution permission or a deadline read earlier is not durable.
	caller, err := proxy.authorizeCaller(ctx, request.Credential)
	if err != nil {
		return CallResult{}, err
	}

	select {
	case <-timeout:
		return CallResult{}, fault.New(fault.DeadlineExpired, "tool call deadline expired")
	default:
	}
	if err := ctx.Err(); err != nil {
		return CallResult{}, fault.New(fault.DeadlineExpired, "tool call deadline expired")
	}

	upstream := UpstreamRequest{
		Server:      server.Name,
		Endpoint:    server.Endpoint,
		Tool:        tool.Name,
		Arguments:   arguments,
		Credentials: resolved,
		Timeout:     proxy.timeout,
	}

	callCtx, cancel := context.WithCancel(ctx)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-timeout:
			cancel()
		case <-callCtx.Done():
		}
	}()
	defer func() {
		cancel()
		<-watchDone
	}()

	body, callErr := proxy.transport.Call(callCtx, upstream)
	closeBody := func() {}
	if body != nil {
		var once sync.Once
		closeBody = func() { once.Do(func() { _ = body.Close() }) }
		defer closeBody()
	}
	if callErr != nil {
		class, mapped := proxy.mapTransportError(callErr)
		_ = proxy.recordUsage(ctx, caller, resolved, server.Name, tool.Name, class, len(arguments), 0, started, arguments, nil)
		return CallResult{}, mapped
	}
	if body == nil {
		_ = proxy.recordUsage(ctx, caller, resolved, server.Name, tool.Name, "uncertain_external_effect", len(arguments), 0, started, arguments, nil)
		return CallResult{}, fault.New(fault.UncertainExternalEffect, "tool call outcome is uncertain")
	}

	data, err := proxy.readResponse(callCtx, body, closeBody)
	if err != nil {
		outcome := "uncertain_external_effect"
		_ = proxy.recordUsage(ctx, caller, resolved, server.Name, tool.Name, outcome, len(arguments), len(data), started, arguments, nil)
		return CallResult{}, fault.New(fault.UncertainExternalEffect, "tool call outcome is uncertain")
	}

	response, err := decodeResponse(data)
	if err != nil {
		_ = proxy.recordUsage(ctx, caller, resolved, server.Name, tool.Name, "uncertain_external_effect", len(arguments), len(data), started, arguments, nil)
		return CallResult{}, fault.New(fault.UncertainExternalEffect, "tool call outcome is uncertain")
	}

	content := redact(response.Content, resolved)
	outcome := "ok"
	if response.IsError {
		outcome = "tool_error"
	}
	if recordErr := proxy.recordUsage(ctx, caller, resolved, server.Name, tool.Name, outcome, len(arguments), len(data), started, arguments, content); recordErr != nil {
		// The upstream tool already ran, so an audit failure must never surface
		// as a retryable pre-effect class: the caller would discard a real
		// effect and replay it. The outcome stays explicitly uncertain and no
		// result is disclosed.
		return CallResult{}, fault.New(fault.UncertainExternalEffect, "tool call outcome is uncertain")
	}
	return CallResult{Content: content, IsError: response.IsError}, nil
}

// caller is one authenticated and currently authorized request context.
type caller struct {
	principal identity.Principal
	admitted  authorize.AdmittedExecution
	execution bool
}

// authorizeCaller authenticates the presented credential, evaluates the
// registry grant for the verified principal and re-reads the operation's
// current authority facts. A failure never discloses a configured server or
// tool name.
func (proxy *Proxy) authorizeCaller(ctx context.Context, credential string) (caller, error) {
	principal, err := proxy.credentials.Resolve(ctx, credential)
	if err != nil {
		return caller{}, unauthenticated()
	}
	if err := principal.Validate(); err != nil {
		return caller{}, unauthenticated()
	}
	if !principal.Enabled {
		return caller{}, forbidden()
	}
	if principal.Kind == identity.KindExecution {
		return proxy.authorizeExecution(ctx, principal)
	}
	if !scopeContains(principal.Scope, proxy.scope) {
		return caller{}, forbidden()
	}
	decision, err := proxy.authorizer.Authorize(ctx, principal, proxy.operation.Name, proxy.scope)
	if err != nil {
		return caller{}, classifyBoundaryError(err)
	}
	if decision != authorize.DecisionAllow {
		return caller{}, forbidden()
	}
	if err := proxy.readAuthority(ctx, proxy.authorityReads(authorize.AdmittedExecution{Principal: principal}, false)); err != nil {
		return caller{}, err
	}
	return caller{principal: principal}, nil
}

// authorizeExecution re-reads the admitted execution and evaluates the
// operation's execution permission plus the current authority facts.
//
// The consumer type is exactly the admitted context: execution identity and
// owning scope, the current container action identity and deadline, and the
// permission the reader derived from trusted configuration plus the
// executions/actions facts. No execution permission is read from a caller field
// or a dedicated column, and the permission is bounded by the declared
// tool.invoke registry row inside authorize.AuthorizeExecution.
func (proxy *Proxy) authorizeExecution(ctx context.Context, principal identity.Principal) (caller, error) {
	if principal.ExecutionID == "" {
		return caller{}, unauthenticated()
	}
	admitted, err := proxy.executions.ReadExecution(ctx, principal.ExecutionID)
	if err != nil {
		return caller{}, classifyBoundaryError(err)
	}
	if admitted.ExecutionID != principal.ExecutionID || admitted.Principal.ID != principal.ID {
		return caller{}, forbidden()
	}
	decision, err := authorize.AuthorizeExecution(admitted, proxy.operation.Name, proxy.scope)
	if err != nil {
		return caller{}, classifyBoundaryError(err)
	}
	if decision != authorize.DecisionAllow {
		return caller{}, forbidden()
	}
	if !scopeContains(admitted.Principal.Scope, proxy.scope) {
		return caller{}, forbidden()
	}
	// The action-deadline fact is read for the active action. An admitted
	// execution without one cannot be evaluated against a real target, so it is
	// refused instead of asking for an unbounded deadline.
	if admitted.ActionID == "" && proxy.requiresCheck(authorize.CheckActionDeadline) {
		return caller{}, forbidden()
	}
	if err := proxy.readAuthority(ctx, proxy.authorityReads(admitted, true)); err != nil {
		return caller{}, err
	}
	if !admitted.Deadline.IsZero() && !proxy.clock.Now().Before(admitted.Deadline) {
		return caller{}, fault.New(fault.DeadlineExpired, "tool call deadline expired")
	}
	return caller{principal: admitted.Principal, admitted: admitted, execution: true}, nil
}

// authorityReads builds the registry authority reads for one caller. A human or
// service caller never evaluates execution-only facts. An execution caller
// binds the execution-scoped reads to the admitted execution and action, so a
// real authority reader evaluates the exact running execution and the exact
// action deadline rather than an empty target.
func (proxy *Proxy) authorityReads(admitted authorize.AdmittedExecution, execution bool) []authorize.AuthorityRead {
	reads := make([]authorize.AuthorityRead, 0, len(proxy.operation.Authority))
	for _, check := range proxy.operation.Authority {
		if !execution && executionOnly(check) {
			continue
		}
		read := authorize.AuthorityRead{
			Operation:   proxy.operation.Name,
			Required:    proxy.operation.Requires,
			Check:       check,
			Scope:       proxy.scope,
			PrincipalID: admitted.Principal.ID,
		}
		if execution {
			switch check {
			case authorize.CheckExecutionRunning:
				read.TargetID = string(admitted.ExecutionID)
			case authorize.CheckActionDeadline:
				read.TargetID = string(admitted.ActionID)
			}
		}
		reads = append(reads, read)
	}
	return reads
}

// requiresCheck reports whether the registry operation names check.
func (proxy *Proxy) requiresCheck(check authorize.AuthorityCheck) bool {
	return slices.Contains(proxy.operation.Authority, check)
}

func executionOnly(check authorize.AuthorityCheck) bool {
	switch check {
	case authorize.CheckExecutionRunning, authorize.CheckActionDeadline, authorize.CheckExecutionVersion:
		return true
	default:
		return false
	}
}

// readAuthority re-reads the requested facts and refuses a denied fact. A
// missing, extra or reordered fact is an unavailable dependency rather than an
// implicit allow.
func (proxy *Proxy) readAuthority(ctx context.Context, reads []authorize.AuthorityRead) error {
	if len(reads) == 0 {
		return nil
	}
	facts, err := proxy.authority.Read(ctx, reads)
	if err != nil {
		return classifyBoundaryError(err)
	}
	if len(facts) != len(reads) {
		return dependencyUnavailable()
	}
	for index, fact := range facts {
		if fact.Check != reads[index].Check {
			return dependencyUnavailable()
		}
		if fact.Allowed {
			continue
		}
		if reads[index].Check == authorize.CheckActionDeadline {
			return fault.New(fault.DeadlineExpired, "tool call deadline expired")
		}
		return forbidden()
	}
	return nil
}

// resolveSecrets resolves exactly the configured secret names in the trusted
// scope. A failure, a missing value, an extra name or a foreign-scope value is
// an unavailable dependency and never reaches the transport.
func (proxy *Proxy) resolveSecrets(ctx context.Context, names []id.SecretName) ([]secret.Resolved, error) {
	if len(names) == 0 {
		return nil, nil
	}
	resolved, err := proxy.secrets.Resolve(ctx, secret.ResolveRequest{
		Scope: proxy.scope,
		Names: append([]id.SecretName(nil), names...),
	})
	if err != nil {
		return nil, classifyBoundaryError(err)
	}
	if len(resolved) != len(names) {
		return nil, dependencyUnavailable()
	}
	configured := make(map[id.SecretName]bool, len(names))
	for _, name := range names {
		configured[name] = true
	}
	seen := make(map[id.SecretName]bool, len(resolved))
	values := make([]secret.Resolved, 0, len(resolved))
	for _, value := range resolved {
		if !configured[value.Name] || seen[value.Name] {
			return nil, dependencyUnavailable()
		}
		if value.Scope != proxy.scope {
			return nil, dependencyUnavailable()
		}
		seen[value.Name] = true
		values = append(values, value)
	}
	return values, nil
}

// readResponse reads at most one byte beyond the configured response bound so
// the exact bound is accepted and one byte more is refused. Cancellation closes
// the body to unblock a read that is already in flight.
func (proxy *Proxy) readResponse(ctx context.Context, body io.ReadCloser, closeBody func()) ([]byte, error) {
	type outcome struct {
		data []byte
		err  error
	}
	results := make(chan outcome, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(body, int64(proxy.bounds.MaxResponseBytes)+1))
		results <- outcome{data: data, err: err}
	}()
	select {
	case result := <-results:
		if result.err != nil {
			return nil, result.err
		}
		if len(result.data) > proxy.bounds.MaxResponseBytes {
			return nil, errResponseTooLarge
		}
		return result.data, nil
	case <-ctx.Done():
		closeBody()
		<-results
		return nil, ctx.Err()
	}
}

var errResponseTooLarge = errors.New("upstream response exceeds the configured bound")

// upstreamResponse is the exact declared transport response shape.
type upstreamResponse struct {
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"isError"`
}

// decodeResponse strictly decodes one transport response. Unknown fields, a
// missing content member, trailing content and invalid JSON are all malformed.
func decodeResponse(data []byte) (upstreamResponse, error) {
	if len(data) == 0 {
		return upstreamResponse{}, errors.New("upstream response is empty")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var response upstreamResponse
	if err := decoder.Decode(&response); err != nil {
		return upstreamResponse{}, err
	}
	if response.Content == nil {
		return upstreamResponse{}, errors.New("upstream response has no content member")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return upstreamResponse{}, errors.New("upstream response has trailing content")
	}
	return response, nil
}

// mapTransportError maps one transport failure. Only a known pre-effect
// unavailability is a dependency error; every other failure may have reached
// the upstream and is an uncertain effect that is never replayed.
func (proxy *Proxy) mapTransportError(err error) (string, error) {
	var upstream *UpstreamError
	if errors.As(err, &upstream) && !upstream.Dispatched && upstream.Class == fault.DependencyUnavailable {
		return "dependency_unavailable", fault.New(fault.DependencyUnavailable, "tool dependency is unavailable")
	}
	return "uncertain_external_effect", fault.New(fault.UncertainExternalEffect, "tool call outcome is uncertain")
}

// redact removes every resolved credential value from the returned content. A
// value is replaced both raw and in its JSON-escaped form.
func redact(content json.RawMessage, resolved []secret.Resolved) json.RawMessage {
	redacted := append(json.RawMessage(nil), content...)
	for _, value := range resolved {
		if value.Value == "" {
			continue
		}
		redacted = bytes.ReplaceAll(redacted, []byte(value.Value), []byte(redactionMarker))
		if escaped, err := json.Marshal(value.Value); err == nil {
			form := escaped[1 : len(escaped)-1]
			redacted = bytes.ReplaceAll(redacted, form, []byte(redactionMarker))
		}
	}
	return redacted
}

// usageDetail is the bounded, attributable usage row detail. It is metadata
// only: an authorized raw-content capture is the separate content_records row,
// so the audit detail never carries retained tool bytes or a retention deadline
// that the audit table cannot expire.
type usageDetail struct {
	Server          string `json:"server"`
	Tool            string `json:"tool"`
	Outcome         string `json:"outcome"`
	RequestBytes    int    `json:"request_bytes"`
	ResponseBytes   int    `json:"response_bytes"`
	DurationMS      int64  `json:"duration_ms"`
	ContentRetained bool   `json:"content_retained"`
}

// recordUsage commits one attributable usage unit through the shared store
// seam in the trusted configuration scope: an audit.detail row and, only when
// raw content was explicitly opted in and a settled tool result exists, one
// content_records capture with the writer-enforced seven-day default retention.
// It never records a credential value in either row.
func (proxy *Proxy) recordUsage(
	ctx context.Context,
	caller caller,
	resolved []secret.Resolved,
	server string,
	tool string,
	outcome string,
	requestBytes int,
	responseBytes int,
	started time.Time,
	arguments []byte,
	content []byte,
) error {
	now := proxy.clock.Now()
	detail := usageDetail{
		Server:        server,
		Tool:          tool,
		Outcome:       outcome,
		RequestBytes:  requestBytes,
		ResponseBytes: responseBytes,
		DurationMS:    now.Sub(started).Milliseconds(),
	}
	unit := store.Unit{}
	if proxy.retention.RawContent && content != nil {
		detail.ContentRetained = true
		write, err := proxy.contentRecord(caller, resolved, server, tool, arguments, content, now)
		if err != nil {
			return err
		}
		unit.Writes = append(unit.Writes, write)
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return fault.New(fault.InternalFailure, "tool usage detail could not be encoded")
	}
	eventID, err := proxy.newEventID()
	if err != nil {
		return fault.New(fault.InternalFailure, "tool usage identity could not be generated")
	}
	audit := event.Audit{
		ID:        eventID,
		Scope:     proxy.scope,
		Actor:     caller.principal.ID,
		Operation: string(proxy.operation.Name),
		At:        now,
		Detail:    string(encoded),
	}
	unit.Audit = []event.Audit{audit}
	if _, err := proxy.store.Execute(context.WithoutCancel(ctx), proxy.scope, unit); err != nil {
		return classifyBoundaryError(err)
	}
	return nil
}

// contentRecord builds the one authorized raw-content capture. The
// content_records row carries the scope, the acting principal and the nullable
// execution identity, the invoked capability identity, the redacted request and
// response bytes, the recording time and the retention expiry. A tool capture
// has no model, so the declared model column carries the configured server/tool
// identity; the writer supplies the row id and scope columns.
func (proxy *Proxy) contentRecord(
	caller caller,
	resolved []secret.Resolved,
	server string,
	tool string,
	arguments []byte,
	content []byte,
	now time.Time,
) (store.Write, error) {
	recordID, err := proxy.newEventID()
	if err != nil {
		return store.Write{}, fault.New(fault.InternalFailure, "tool content identity could not be generated")
	}
	// A non-execution caller has no execution identity, so the nullable
	// execution_id column is an explicit SQL NULL rather than an empty string
	// the text column would accept as a value.
	var executionID any
	if caller.principal.ExecutionID != "" {
		executionID = string(caller.principal.ExecutionID)
	}
	values := map[string]any{
		"principal_id":  string(caller.principal.ID),
		"execution_id":  executionID,
		"model":         server + "/" + tool,
		"request_body":  []byte(redact(arguments, resolved)),
		"response_body": []byte(redact(content, resolved)),
		"recorded_at":   now,
		"retain_until":  now.Add(proxy.retention.EffectiveDuration()),
		"version":       uint64(1),
	}
	return store.Write{
		Table:  store.ContentRecordsTable,
		ID:     string(recordID),
		Insert: true,
		Values: values,
	}, nil
}

// newEventID uses the injected identity source, or secure randomness when the
// optional source was not configured.
func (proxy *Proxy) newEventID() (id.EventID, error) {
	if proxy.ids != nil {
		return proxy.ids.NewEventID()
	}
	return newAuditID()
}

// newAuditID generates one secure, bounded audit identity.
func newAuditID() (id.EventID, error) {
	var material [16]byte
	if _, err := rand.Read(material[:]); err != nil {
		return "", err
	}
	return id.EventID("audit-" + hex.EncodeToString(material[:])), nil
}

func (proxy *Proxy) server(name string) (Server, bool) {
	index, ok := proxy.byServer[name]
	if !ok {
		return Server{}, false
	}
	return proxy.servers[index], true
}

func (proxy *Proxy) tool(server, name string) (Tool, bool) {
	index, ok := proxy.byTool[server][name]
	if !ok {
		return Tool{}, false
	}
	return proxy.servers[proxy.byServer[server]].Tools[index], true
}

func scopeContains(owner, target tenancy.Scope) bool {
	if owner.TenantID != target.TenantID {
		return false
	}
	if owner.ProjectID == "" {
		return true
	}
	return owner.ProjectID == target.ProjectID
}

func unauthenticated() error {
	return fault.New(fault.Unauthenticated, "credential is not valid")
}

func forbidden() error {
	return fault.New(fault.Forbidden, "caller is not authorized for the configured tool operation")
}

func dependencyUnavailable() error {
	return fault.New(fault.DependencyUnavailable, "tool dependency is unavailable")
}

// classifyBoundaryError keeps an authentication, authorization or deadline
// refusal class while replacing any dependency diagnostic with a public
// message that cannot carry credential material.
func classifyBoundaryError(err error) error {
	switch class, ok := fault.ClassOf(err); {
	case ok && class == fault.Unauthenticated:
		return unauthenticated()
	case ok && class == fault.Forbidden:
		return forbidden()
	case ok && class == fault.DeadlineExpired:
		return fault.New(fault.DeadlineExpired, "tool call deadline expired")
	default:
		return dependencyUnavailable()
	}
}
