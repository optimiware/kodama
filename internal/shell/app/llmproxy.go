package app

// Streaming credential-injecting LLM proxy for external coding agents.
//
// Workloads running inside container actions authenticate with their mounted
// Kodama execution credential (either `Authorization: Bearer kodex...` or
// `x-api-key: kodex...`) against `/llm/{provider}/{upstream_path...}` (such as
// `POST /llm/anthropic/v1/messages` or `POST /llm/openai/v1/responses`).
//
// Before any upstream connection is opened or any secret is resolved, the proxy
// authenticates the caller, requires an active workload execution principal,
// re-reads current execution/action state and `ExecConfiguredModels` authority,
// enforces the active action's remaining deadline, strips the caller's Kodama
// credential, resolves the provider secret through the gateway secret policy in
// the execution's scope, and streams response chunks (including SSE
// `text/event-stream` events) immediately without following redirects.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/gateway"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

const (
	// llmProxyPathPrefix is the HTTP path prefix of the streaming LLM proxy.
	llmProxyPathPrefix = "/llm/"
	// operationLLMProxyInvoke is the registry operation whose execution
	// permission (ExecConfiguredModels) and live authority checks govern
	// workload LLM proxy access.
	operationLLMProxyInvoke authorize.OperationName = "model.invoke"
	// maxLLMProxyResponseBytes bounds one streamed upstream response body.
	maxLLMProxyResponseBytes int64 = 64 << 20
	// defaultLLMProxyWatchInterval is how often an open stream re-checks that
	// its execution and container action remain active and within deadline.
	defaultLLMProxyWatchInterval = 100 * time.Millisecond
)

var errLLMProxyRedirectRefused = errors.New("llm proxy refuses upstream redirects")

// llmProxyDependencies is the complete set of trusted configuration and
// injected boundaries one enabled LLM proxy needs.
type llmProxyDependencies struct {
	Config         *ModelsConfig
	Profile        Profile
	InstallationID string
	Controller     store.Controller
	Executions     gateway.ExecutionReader
	Principals     authorize.CurrentPrincipal
	Authority      authorize.AuthorityReader
	Lookup         secret.NameLookup
	Cipher         secret.EnvelopeCipher
	Clock          Clock
	Transport      http.RoundTripper
	WatchInterval  time.Duration
}

type resolvedProvider struct {
	scope            tenancy.Scope
	name             string
	baseURL          *url.URL
	secretName       id.SecretName
	useXAPIKey       bool
	anthropicVersion string
	maxRequestBytes  int
	timeout          time.Duration
}

// disabledLLMProxyHandler fails closed with 404 when no providers are enabled.
type disabledLLMProxyHandler struct{}

func (disabledLLMProxyHandler) ServeHTTP(writer http.ResponseWriter, _ *http.Request) {
	writeHTTPError(writer, fault.New(fault.NotFound, "the optional LLM proxy is not enabled"))
}

// llmProxyHandler serves `/llm/{provider}/{upstream_path...}` for active
// workload executions holding ExecConfiguredModels.
type llmProxyHandler struct {
	providers     map[providerScopeKey]resolvedProvider
	principals    authorize.CurrentPrincipal
	authority     authorize.AuthorityReader
	executions    gateway.ExecutionReader
	secrets       secret.Resolver
	client        *http.Client
	clock         Clock
	watchInterval time.Duration
}

// newLLMProxyHandler validates and constructs the optional streaming LLM proxy.
// When Config is nil or declares no Providers, the returned handler answers 404
// and adopts no transport.
func newLLMProxyHandler(deps llmProxyDependencies) (http.Handler, func(context.Context) error, error) {
	if deps.Config == nil {
		return disabledLLMProxyHandler{}, nil, nil
	}
	if err := deps.Config.validate(deps.Profile); err != nil {
		return nil, nil, err
	}
	if len(deps.Config.Providers) == 0 {
		return disabledLLMProxyHandler{}, nil, nil
	}
	switch {
	case deps.InstallationID == "":
		return nil, nil, fault.New(fault.InvalidInput, "LLM proxy requires an installation id")
	case deps.Controller == nil && deps.Executions == nil:
		return nil, nil, fault.New(fault.InvalidInput, "LLM proxy requires a controller or execution reader")
	case deps.Principals == nil:
		return nil, nil, fault.New(fault.InvalidInput, "LLM proxy requires a principal resolver")
	case deps.Authority == nil:
		return nil, nil, fault.New(fault.InvalidInput, "LLM proxy requires an authority reader")
	case deps.Lookup == nil:
		return nil, nil, fault.New(fault.InvalidInput, "LLM proxy requires a secret name lookup")
	case deps.Cipher == nil:
		return nil, nil, fault.New(fault.InvalidInput, "LLM proxy requires an envelope cipher")
	case deps.Clock == nil:
		return nil, nil, fault.New(fault.InvalidInput, "LLM proxy requires a clock")
	}

	policy, err := bridges.NewGatewayNamePolicy(deps.Config.SecretRules)
	if err != nil {
		return nil, nil, err
	}
	secrets, err := secret.NewResolver(secret.ResolverConfig{
		InstallationID: deps.InstallationID,
		Policy:         policy,
		Lookup:         deps.Lookup,
		Cipher:         deps.Cipher,
	})
	if err != nil {
		return nil, nil, err
	}

	executions := deps.Executions
	if executions == nil {
		permissions, err := bridges.NewPermissionSource(authorize.ExecConfiguredModels, deps.Clock)
		if err != nil {
			return nil, nil, err
		}
		reader, err := platform.NewExecutionReader(deps.Controller, permissions)
		if err != nil {
			return nil, nil, err
		}
		executions = reader
	}

	providers := make(map[providerScopeKey]resolvedProvider, len(deps.Config.Providers))
	for _, entry := range deps.Config.Providers {
		baseURL, err := deriveProviderBaseURL(entry.BaseURL, entry.Endpoint)
		if err != nil {
			return nil, nil, err
		}
		useXAPIKey := providerUsesXAPIKey(entry, baseURL)
		anthropicVersion := strings.TrimSpace(entry.AnthropicVersion)
		if anthropicVersion == "" && entry.Name == "anthropic" {
			anthropicVersion = DefaultAnthropicVersion
		}
		maxReq := entry.MaxRequestBytes
		if maxReq == 0 {
			maxReq = DefaultLLMProxyMaxRequestBytes
		}
		providers[providerScopeKey{scope: entry.Scope, name: entry.Name}] = resolvedProvider{
			scope:            entry.Scope,
			name:             entry.Name,
			baseURL:          baseURL,
			secretName:       entry.SecretName,
			useXAPIKey:       useXAPIKey,
			anthropicVersion: anthropicVersion,
			maxRequestBytes:  maxReq,
			timeout:          entry.Timeout,
		}
	}

	transport := deps.Transport
	var stop func(context.Context) error
	if transport == nil {
		transport, stop = deps.Config.transportFor()
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errLLMProxyRedirectRefused
		},
	}

	watchInterval := deps.WatchInterval
	if watchInterval <= 0 {
		watchInterval = defaultLLMProxyWatchInterval
	}

	return &llmProxyHandler{
		providers:     providers,
		principals:    deps.Principals,
		authority:     deps.Authority,
		executions:    executions,
		secrets:       secrets,
		client:        client,
		clock:         deps.Clock,
		watchInterval: watchInterval,
	}, stop, nil
}

func providerUsesXAPIKey(entry ModelProviderConfig, baseURL *url.URL) bool {
	switch strings.ToLower(strings.TrimSpace(entry.AuthHeader)) {
	case "x-api-key":
		return true
	case "authorization":
		return false
	}
	if entry.Name == "anthropic" {
		return true
	}
	host := strings.ToLower(baseURL.Hostname())
	return host == "api.anthropic.com" || strings.HasSuffix(host, ".anthropic.com")
}

func (handler *llmProxyHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if handler == nil || request == nil || request.URL == nil {
		writeHTTPError(writer, fault.New(fault.NotFound, "the optional LLM proxy is not enabled"))
		return
	}
	if request.Method != http.MethodPost && request.Method != http.MethodGet {
		writeHTTPError(writer, fault.New(fault.NotFound, "no operation is bound to this method and path"))
		return
	}
	if request.URL.Fragment != "" {
		writeHTTPError(writer, fault.New(fault.InvalidInput, "fragments are not permitted on LLM proxy paths"))
		return
	}
	if strings.ContainsAny(request.URL.RawQuery, "\r\n\x00") {
		writeHTTPError(writer, fault.New(fault.InvalidInput, "query string contains invalid control characters"))
		return
	}
	lowerEscaped := strings.ToLower(request.URL.EscapedPath())
	if strings.Contains(lowerEscaped, "%2f") || strings.Contains(lowerEscaped, "%5c") || strings.Contains(lowerEscaped, "%00") {
		writeHTTPError(writer, fault.New(fault.InvalidInput, "LLM proxy upstream path contains an encoded path separator"))
		return
	}

	providerName, upstreamPath, err := parseLLMProxyPath(request.URL.Path)
	if err != nil {
		writeHTTPError(writer, err)
		return
	}

	credential, err := extractLLMProxyCredential(request.Header)
	if err != nil {
		writeHTTPError(writer, err)
		return
	}
	if strings.Contains(request.URL.RawQuery, credential) || strings.Contains(request.URL.RawQuery, token.ExecutionPrefix) {
		writeHTTPError(writer, fault.New(fault.InvalidInput, "workload credentials must not be sent in query parameters"))
		return
	}

	ctx := request.Context()
	if ctx.Err() != nil {
		writeHTTPError(writer, llmProxyDeadlineFault())
		return
	}

	admitted, scope, err := handler.authenticateAndAuthorize(ctx, credential)
	if err != nil {
		writeHTTPError(writer, err)
		return
	}

	provider, ok := handler.lookupProvider(scope, providerName)
	if !ok {
		writeHTTPError(writer, fault.New(fault.NotFound, "the requested model provider is not configured in this project"))
		return
	}

	var body []byte
	if request.Body != nil {
		data, readErr := io.ReadAll(io.LimitReader(request.Body, int64(provider.maxRequestBytes)+1))
		if readErr != nil {
			if ctx.Err() != nil {
				writeHTTPError(writer, llmProxyDeadlineFault())
				return
			}
			writeHTTPError(writer, fault.New(fault.InvalidInput, "LLM proxy request body could not be read"))
			return
		}
		if len(data) > provider.maxRequestBytes {
			writeLLMProxyEntityTooLarge(writer)
			return
		}
		body = data
	}

	if ctx.Err() != nil {
		writeHTTPError(writer, llmProxyDeadlineFault())
		return
	}

	providerSecret, err := handler.resolveProviderSecret(ctx, scope, provider.secretName)
	if err != nil {
		writeHTTPError(writer, err)
		return
	}

	now := handler.clock.Now()
	if now.IsZero() {
		writeHTTPError(writer, fault.New(fault.InternalFailure, "LLM proxy clock returned no usable time"))
		return
	}
	remaining := admitted.Deadline.Sub(now)
	if remaining <= 0 {
		writeHTTPError(writer, llmProxyDeadlineFault())
		return
	}
	if provider.timeout > 0 && provider.timeout < remaining {
		remaining = provider.timeout
	}

	callCtx, cancel := context.WithTimeout(ctx, remaining)
	stopWatch := handler.startExecutionWatch(callCtx, cancel, admitted.Principal.ID, admitted.ExecutionID, admitted.ActionID, admitted.Deadline, scope)
	defer stopWatch()
	// Deferred last so it runs first: stopWatch waits for the watcher, whose
	// in-flight reads end only when callCtx is cancelled.
	defer cancel()

	targetURL, err := joinLLMProviderURL(provider.baseURL, upstreamPath, request.URL.RawQuery)
	if err != nil {
		writeHTTPError(writer, err)
		return
	}

	var bodyReader io.Reader
	if len(body) > 0 || request.Method == http.MethodPost {
		bodyReader = bytes.NewReader(body)
	}
	outbound, err := http.NewRequestWithContext(callCtx, request.Method, targetURL, bodyReader)
	if err != nil {
		writeHTTPError(writer, fault.New(fault.InternalFailure, "LLM proxy request could not be constructed"))
		return
	}
	if bodyReader != nil {
		outbound.ContentLength = int64(len(body))
	}

	copySafeOutboundHeaders(outbound.Header, request.Header, credential)
	if provider.useXAPIKey {
		outbound.Header.Set("x-api-key", providerSecret)
	} else {
		outbound.Header.Set("Authorization", "Bearer "+providerSecret)
	}
	if outbound.Header.Get("anthropic-version") == "" && provider.anthropicVersion != "" {
		outbound.Header.Set("anthropic-version", provider.anthropicVersion)
	}

	response, err := handler.client.Do(outbound)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if errors.Is(err, errLLMProxyRedirectRefused) {
			writeHTTPError(writer, fault.New(fault.DependencyUnavailable, "the configured model provider returned a redirect, which is refused"))
			return
		}
		if callCtx.Err() != nil || ctx.Err() != nil {
			writeHTTPError(writer, llmProxyDeadlineFault())
			return
		}
		writeHTTPError(writer, fault.New(fault.DependencyUnavailable, "the configured model provider is unreachable"))
		return
	}
	if response == nil || response.Body == nil {
		writeHTTPError(writer, fault.New(fault.DependencyUnavailable, "the configured model provider returned no response"))
		return
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode >= 300 && response.StatusCode <= 399 {
		writeHTTPError(writer, fault.New(fault.DependencyUnavailable, "the configured model provider returned a redirect, which is refused"))
		return
	}

	if callCtx.Err() != nil {
		writeHTTPError(writer, llmProxyDeadlineFault())
		return
	}

	copySafeResponseHeaders(writer.Header(), response.Header, providerSecret)
	if isEventStream(response.Header.Get("Content-Type")) {
		if writer.Header().Get("Cache-Control") == "" {
			writer.Header().Set("Cache-Control", "no-cache")
		}
		writer.Header().Set("X-Accel-Buffering", "no")
	}

	flusher, canFlush := writer.(http.Flusher)
	writer.WriteHeader(response.StatusCode)
	if canFlush {
		flusher.Flush()
	}

	// The status is already sent, so a truncated stream aborts the response
	// instead of ending it as a clean EOF.
	buffer := make([]byte, 32*1024)
	var total int64
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			if callCtx.Err() != nil {
				panic(http.ErrAbortHandler)
			}
			total += int64(n)
			if total > maxLLMProxyResponseBytes {
				panic(http.ErrAbortHandler)
			}
			chunk := buffer[:n]
			if providerSecret != "" && bytes.Contains(chunk, []byte(providerSecret)) {
				chunk = bytes.ReplaceAll(chunk, []byte(providerSecret), []byte("[REDACTED]"))
			}
			if _, writeErr := writer.Write(chunk); writeErr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			return
		}
		if readErr != nil {
			panic(http.ErrAbortHandler)
		}
	}
}

func (handler *llmProxyHandler) authenticateAndAuthorize(
	ctx context.Context,
	credential string,
) (authorize.AdmittedExecution, tenancy.Scope, error) {
	principal, err := handler.principals.Resolve(ctx, credential)
	if err != nil {
		if ctx.Err() != nil {
			return authorize.AdmittedExecution{}, tenancy.Scope{}, llmProxyDeadlineFault()
		}
		return authorize.AdmittedExecution{}, tenancy.Scope{}, fault.Wrap(fault.Unauthenticated, "the presented credential could not be verified", err)
	}
	if err := principal.Validate(); err != nil {
		return authorize.AdmittedExecution{}, tenancy.Scope{}, fault.Wrap(fault.Unauthenticated, "the presented credential did not verify a usable principal", err)
	}
	if principal.Kind != identity.KindExecution {
		return authorize.AdmittedExecution{}, tenancy.Scope{}, fault.New(fault.Forbidden, "the LLM proxy accepts only workload execution credentials")
	}
	if principal.ExecutionID == "" {
		return authorize.AdmittedExecution{}, tenancy.Scope{}, fault.New(fault.Unauthenticated, "an execution credential did not verify an execution identity")
	}
	if !principal.Scope.ProjectScoped() {
		return authorize.AdmittedExecution{}, tenancy.Scope{}, fault.New(fault.Forbidden, "workload execution is not project scoped")
	}

	admitted, err := handler.executions.ReadExecution(ctx, principal.ExecutionID)
	if err != nil {
		return authorize.AdmittedExecution{}, tenancy.Scope{}, classifyLLMProxyAuthError(ctx, err)
	}
	if admitted.ExecutionID != principal.ExecutionID ||
		admitted.Principal.ID != principal.ID ||
		admitted.Principal.Scope != principal.Scope {
		return authorize.AdmittedExecution{}, tenancy.Scope{}, fault.New(fault.Forbidden, "the admitted execution does not match the presented principal")
	}

	scope := admitted.Principal.Scope
	decision, err := authorize.AuthorizeExecution(admitted, operationLLMProxyInvoke, scope)
	if err != nil {
		return authorize.AdmittedExecution{}, tenancy.Scope{}, classifyLLMProxyAuthError(ctx, err)
	}
	if decision != authorize.DecisionAllow {
		return authorize.AdmittedExecution{}, tenancy.Scope{}, fault.New(fault.Forbidden, "the execution does not hold the configured_models grant in this project")
	}
	if admitted.ActionID == "" || admitted.Deadline.IsZero() {
		return authorize.AdmittedExecution{}, tenancy.Scope{}, fault.New(fault.Forbidden, "an execution without an active bounded container action cannot invoke a configured model")
	}

	if err := handler.refreshExecutionAuthority(ctx, admitted.Principal.ID, admitted.ExecutionID, admitted.ActionID, admitted.Deadline, scope); err != nil {
		return authorize.AdmittedExecution{}, tenancy.Scope{}, err
	}
	return admitted, scope, nil
}

func (handler *llmProxyHandler) refreshExecutionAuthority(
	ctx context.Context,
	principalID id.PrincipalID,
	executionID id.ExecutionID,
	actionID id.ActionID,
	deadline time.Time,
	scope tenancy.Scope,
) error {
	operation, ok := authorize.LookupOperation(operationLLMProxyInvoke)
	if !ok {
		return fault.New(fault.InternalFailure, "model.invoke is not registered")
	}
	reads := make([]authorize.AuthorityRead, 0, len(operation.Authority))
	for _, check := range operation.Authority {
		read := authorize.AuthorityRead{
			Operation:   operation.Name,
			Required:    operation.Requires,
			Check:       check,
			Scope:       scope,
			PrincipalID: principalID,
		}
		switch check {
		case authorize.CheckExecutionRunning:
			read.TargetID = string(executionID)
		case authorize.CheckActionDeadline:
			read.TargetID = string(actionID)
		}
		reads = append(reads, read)
	}
	facts, err := handler.authority.Read(ctx, reads)
	if err != nil {
		if ctx.Err() != nil {
			return llmProxyDeadlineFault()
		}
		if _, ok := fault.ClassOf(err); ok {
			return err
		}
		return fault.Wrap(fault.InternalFailure, "current model authority could not be read", err)
	}
	if ctx.Err() != nil {
		return llmProxyDeadlineFault()
	}
	if err := authorize.Confirm(reads, facts); err != nil {
		if class, ok := fault.ClassOf(err); ok && class == fault.Forbidden {
			for _, fact := range facts {
				if fact.Allowed {
					continue
				}
				if fact.Check == authorize.CheckActionDeadline {
					return llmProxyDeadlineFault()
				}
				break
			}
		}
		return err
	}
	if !deadline.IsZero() && !handler.clock.Now().Before(deadline) {
		return llmProxyDeadlineFault()
	}
	return nil
}

func (handler *llmProxyHandler) startExecutionWatch(
	ctx context.Context,
	cancel context.CancelFunc,
	principalID id.PrincipalID,
	executionID id.ExecutionID,
	actionID id.ActionID,
	deadline time.Time,
	scope tenancy.Scope,
) func() {
	done := make(chan struct{})
	var once sync.Once
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(handler.watchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !deadline.IsZero() && !handler.clock.Now().Before(deadline) {
					cancel()
					return
				}
				admitted, err := handler.executions.ReadExecution(ctx, executionID)
				if err != nil {
					cancel()
					return
				}
				if admitted.ExecutionID != executionID ||
					admitted.Principal.ID != principalID ||
					admitted.ActionID != actionID {
					cancel()
					return
				}
				decision, err := authorize.AuthorizeExecution(admitted, operationLLMProxyInvoke, scope)
				if err != nil || decision != authorize.DecisionAllow {
					cancel()
					return
				}
				if err := handler.refreshExecutionAuthority(ctx, principalID, executionID, actionID, admitted.Deadline, scope); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	return func() {
		once.Do(func() {
			close(done)
			wg.Wait()
		})
	}
}

func (handler *llmProxyHandler) lookupProvider(scope tenancy.Scope, name string) (resolvedProvider, bool) {
	if provider, ok := handler.providers[providerScopeKey{scope: scope, name: name}]; ok {
		return provider, true
	}
	if scope.ProjectID != "" {
		tenantScope := tenancy.Scope{TenantID: scope.TenantID}
		if provider, ok := handler.providers[providerScopeKey{scope: tenantScope, name: name}]; ok {
			return provider, true
		}
	}
	return resolvedProvider{}, false
}

func (handler *llmProxyHandler) resolveProviderSecret(ctx context.Context, scope tenancy.Scope, name id.SecretName) (string, error) {
	resolved, err := handler.secrets.Resolve(ctx, secret.ResolveRequest{
		Scope: scope,
		Names: []id.SecretName{name},
	})
	if err != nil {
		if ctx.Err() != nil {
			return "", llmProxyDeadlineFault()
		}
		return "", fault.Wrap(fault.DependencyUnavailable, "the configured model provider credential is unavailable", err)
	}
	if len(resolved) != 1 || resolved[0].Name != name || resolved[0].Value == "" {
		return "", fault.New(fault.DependencyUnavailable, "the configured model provider credential is unavailable")
	}
	if resolved[0].Scope.TenantID != scope.TenantID ||
		(resolved[0].Scope.ProjectID != "" && resolved[0].Scope.ProjectID != scope.ProjectID) {
		return "", fault.New(fault.DependencyUnavailable, "the configured model provider credential was resolved outside its scope")
	}
	if err := secret.DefaultBounds.CheckValue(len(resolved[0].Value)); err != nil {
		return "", fault.Wrap(fault.DependencyUnavailable, "the configured model provider credential is unusable", err)
	}
	return resolved[0].Value, nil
}

func parseLLMProxyPath(rawPath string) (string, string, error) {
	if !strings.HasPrefix(rawPath, llmProxyPathPrefix) {
		return "", "", fault.New(fault.NotFound, "no operation is bound to this method and path")
	}
	rest := strings.TrimPrefix(rawPath, llmProxyPathPrefix)
	provider, upstreamRel, ok := strings.Cut(rest, "/")
	if !ok || provider == "" || upstreamRel == "" {
		return "", "", fault.New(fault.NotFound, "no operation is bound to this method and path")
	}
	if _, err := id.ParseActionName(provider); err != nil {
		return "", "", fault.New(fault.NotFound, "the requested model provider is not configured in this project")
	}
	for index := 0; index < len(upstreamRel); index++ {
		ch := upstreamRel[index]
		if ch < 0x20 || ch == 0x7f || ch == '\\' || ch == '@' {
			return "", "", fault.New(fault.InvalidInput, "LLM proxy upstream path contains invalid characters")
		}
	}
	segments := strings.Split(upstreamRel, "/")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return "", "", fault.New(fault.InvalidInput, "LLM proxy upstream path contains an empty or relative segment")
		}
	}
	return provider, "/" + upstreamRel, nil
}

func extractLLMProxyCredential(header http.Header) (string, error) {
	var bearer string
	for _, value := range header.Values("Authorization") {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		extracted := bearerToken(trimmed)
		if extracted == "" {
			return "", fault.New(fault.Unauthenticated, "authorization header must use the Bearer scheme")
		}
		if bearer != "" && bearer != extracted {
			return "", fault.New(fault.Unauthenticated, "conflicting credentials were presented")
		}
		bearer = extracted
	}

	var apiKey string
	for _, value := range header.Values("X-Api-Key") {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if apiKey != "" && apiKey != trimmed {
			return "", fault.New(fault.Unauthenticated, "conflicting credentials were presented")
		}
		apiKey = trimmed
	}

	if bearer != "" && apiKey != "" && bearer != apiKey {
		return "", fault.New(fault.Unauthenticated, "conflicting credentials were presented")
	}
	if bearer != "" {
		return bearer, nil
	}
	if apiKey != "" {
		return apiKey, nil
	}
	return "", fault.New(fault.Unauthenticated, "no credential was presented")
}

func joinLLMProviderURL(base *url.URL, upstreamPath, rawQuery string) (string, error) {
	if base == nil {
		return "", fault.New(fault.InternalFailure, "provider base URL is missing")
	}
	basePath := strings.TrimSuffix(base.Path, "/")
	relPath := upstreamPath
	if strings.HasSuffix(basePath, "/v1") && (relPath == "/v1" || strings.HasPrefix(relPath, "/v1/")) {
		relPath = strings.TrimPrefix(relPath, "/v1")
	}
	target := *base
	target.Path = basePath + relPath
	target.RawPath = ""
	target.RawQuery = rawQuery
	target.Fragment = ""
	return target.String(), nil
}

var strippedOutboundHeaders = map[string]bool{
	// The transport negotiates and decodes gzip itself, so a caller cannot
	// pick an encoding that hides the body from the response redaction scan.
	"Accept-Encoding":     true,
	"Authorization":       true,
	"X-Api-Key":           true,
	"Proxy-Authorization": true,
	"Proxy-Authenticate":  true,
	"Cookie":              true,
	"Connection":          true,
	"Keep-Alive":          true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Host":                true,
	"Content-Length":      true,
}

var strippedResponseHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Content-Length":      true,
	"Set-Cookie":          true,
	"Www-Authenticate":    true,
	"Location":            true,
}

func copySafeOutboundHeaders(dst, src http.Header, callerCredential string) {
	connectionHop := connectionScopedHeaders(src)
	for rawName, values := range src {
		canonical := textproto.CanonicalMIMEHeaderKey(rawName)
		if strippedOutboundHeaders[canonical] || connectionHop[canonical] {
			continue
		}
		for _, value := range values {
			if strings.ContainsAny(value, "\r\n\x00") {
				continue
			}
			if (callerCredential != "" && strings.Contains(value, callerCredential)) ||
				strings.Contains(value, token.ExecutionPrefix) {
				continue
			}
			dst.Add(canonical, value)
		}
	}
}

func copySafeResponseHeaders(dst, src http.Header, providerSecret string) {
	connectionHop := connectionScopedHeaders(src)
	for rawName, values := range src {
		canonical := textproto.CanonicalMIMEHeaderKey(rawName)
		if strippedResponseHeaders[canonical] || connectionHop[canonical] {
			continue
		}
		for _, value := range values {
			if strings.ContainsAny(value, "\r\n\x00") {
				continue
			}
			if providerSecret != "" && strings.Contains(value, providerSecret) {
				value = strings.ReplaceAll(value, providerSecret, "[REDACTED]")
			}
			dst.Add(canonical, value)
		}
	}
}

func connectionScopedHeaders(header http.Header) map[string]bool {
	hop := make(map[string]bool)
	for _, value := range header.Values("Connection") {
		for tokenName := range strings.SplitSeq(value, ",") {
			trimmed := strings.TrimSpace(tokenName)
			if trimmed != "" {
				hop[textproto.CanonicalMIMEHeaderKey(trimmed)] = true
			}
		}
	}
	return hop
}

func isEventStream(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), "text/event-stream")
}

func classifyLLMProxyAuthError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return llmProxyDeadlineFault()
	}
	if _, ok := fault.ClassOf(err); ok {
		return err
	}
	return fault.Wrap(fault.InternalFailure, "LLM proxy authorization failed", err)
}

func llmProxyDeadlineFault() error {
	return fault.New(fault.DeadlineExpired, "model invocation deadline expired")
}

func writeLLMProxyEntityTooLarge(writer http.ResponseWriter) {
	publicError := api.Error{
		Code:    api.CodeInvalidInput,
		Message: "LLM proxy request exceeds the configured request bound",
	}
	body, err := json.Marshal(publicError)
	if err != nil {
		body = []byte(`{"code":"invalid_input","message":"LLM proxy request exceeds the configured request bound"}`)
	}
	writer.Header().Set("Content-Type", api.MediaType)
	writer.WriteHeader(http.StatusRequestEntityTooLarge)
	_, _ = writer.Write(body)
}
