package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Gateway is the optional in-process model surface. It holds only immutable
// configuration and the injected dependencies; every invocation keeps its own
// scope, credential, secret and usage identity, so concurrent calls share no
// per-call state.
type Gateway struct {
	enabled    bool
	deps       Dependencies
	executions ExecutionReader
	models     map[modelKey]ModelConfig
}

// operationModelInvoke is the one registry operation this capability invokes.
const operationModelInvoke authorize.OperationName = "model.invoke"

// New validates the injected composition and configuration and returns the
// optional gateway. A disabled gateway accepts an empty composition and is
// inert; an enabled gateway refuses a missing dependency, a missing execution
// reader option or an invalid configuration instead of guessing a substitute.
func New(config Config, dependencies Dependencies, configure ...Option) (*Gateway, error) {
	if !config.Enabled {
		return &Gateway{}, nil
	}
	if err := validateDependencies(dependencies); err != nil {
		return nil, err
	}
	var resolved options
	for _, option := range configure {
		if option == nil {
			return nil, fault.New(fault.InvalidInput, "model gateway option is nil")
		}
		if err := option(&resolved); err != nil {
			return nil, err
		}
	}
	if resolved.executions == nil {
		return nil, fault.New(fault.InvalidInput, "model gateway requires an execution reader")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	models := make(map[modelKey]ModelConfig, len(config.Models))
	for _, model := range config.Models {
		models[modelKey{scope: model.Scope, model: model.Model}] = model
	}
	return &Gateway{enabled: true, deps: dependencies, executions: resolved.executions, models: models}, nil
}

// validateDependencies refuses an incomplete enabled composition.
func validateDependencies(dependencies Dependencies) error {
	switch {
	case dependencies.Principals == nil:
		return fault.New(fault.InternalFailure, "model gateway requires a principal resolver")
	case dependencies.Authorizer == nil:
		return fault.New(fault.InternalFailure, "model gateway requires an authorizer")
	case dependencies.Authority == nil:
		return fault.New(fault.InternalFailure, "model gateway requires an authority reader")
	case dependencies.Secrets == nil:
		return fault.New(fault.InternalFailure, "model gateway requires a secret resolver")
	case dependencies.Store == nil:
		return fault.New(fault.InternalFailure, "model gateway requires a store executor")
	case dependencies.Content == nil:
		return fault.New(fault.InternalFailure, "model gateway requires a content recorder")
	case dependencies.Transport == nil:
		return fault.New(fault.InternalFailure, "model gateway requires a transport")
	case dependencies.Clock == nil:
		return fault.New(fault.InternalFailure, "model gateway requires a clock")
	case dependencies.NewID == nil:
		return fault.New(fault.InternalFailure, "model gateway requires a usage identity source")
	}
	return nil
}

// Invoke performs one bounded, non-streaming model invocation.
//
// The order is the authority order: validate the declared request subset,
// resolve the presented credential, authorize model.invoke in the project
// derived from the resolved principal and the claimed project, match one
// configured model, re-read the registry row's current authority facts, and
// only then resolve the configured secret and forward. Every refusal before
// forwarding performs no provider call, no secret resolve and no usage write.
func (gateway *Gateway) Invoke(ctx context.Context, invocation Invocation) (api.ChatCompletionResponse, error) {
	if gateway == nil || !gateway.enabled {
		return api.ChatCompletionResponse{}, fault.New(fault.NotFound, "the optional model gateway is not enabled")
	}
	if err := ctx.Err(); err != nil {
		return api.ChatCompletionResponse{}, deadlineFault()
	}
	if err := invocation.Request.Validate(); err != nil {
		return api.ChatCompletionResponse{}, fault.New(fault.InvalidInput, "model request is outside the frozen non-streaming subset")
	}

	principal, err := gateway.authenticate(ctx, invocation.Credential)
	if err != nil {
		return api.ChatCompletionResponse{}, err
	}
	if _, err := id.ParseProjectID(string(invocation.ProjectID)); err != nil {
		return api.ChatCompletionResponse{}, fault.Wrap(fault.InvalidInput, "model invocation project id is invalid", err)
	}
	scope := tenancy.Scope{TenantID: principal.Scope.TenantID, ProjectID: invocation.ProjectID}
	current, err := gateway.authorize(ctx, principal, scope)
	if err != nil {
		return api.ChatCompletionResponse{}, err
	}

	model, ok := gateway.models[modelKey{scope: scope, model: invocation.Request.Model}]
	if !ok {
		return api.ChatCompletionResponse{}, fault.New(fault.NotFound, "the requested model is not configured in this project")
	}
	if err := gateway.refreshAuthority(ctx, current, scope); err != nil {
		return api.ChatCompletionResponse{}, err
	}

	body, err := encodeRequest(model, invocation.Request)
	if err != nil {
		return api.ChatCompletionResponse{}, err
	}
	if err := ctx.Err(); err != nil {
		return api.ChatCompletionResponse{}, deadlineFault()
	}
	credential, err := gateway.resolveSecret(ctx, scope, model.SecretName)
	if err != nil {
		return api.ChatCompletionResponse{}, err
	}
	if err := ctx.Err(); err != nil {
		return api.ChatCompletionResponse{}, deadlineFault()
	}
	usageID, err := gateway.deps.NewID()
	if err != nil {
		return api.ChatCompletionResponse{}, fault.Wrap(fault.InternalFailure, "model usage identity could not be allocated", err)
	}
	if usageID == "" || len(usageID) > id.MaxIDLength {
		return api.ChatCompletionResponse{}, fault.New(fault.InternalFailure, "model usage identity source returned an unusable identity")
	}
	if gateway.deps.Clock.Now().IsZero() {
		return api.ChatCompletionResponse{}, fault.New(fault.InternalFailure, "model gateway clock returned no usable time")
	}

	callCtx, cancel := context.WithTimeout(ctx, model.Timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(callCtx, http.MethodPost, model.Endpoint, bytes.NewReader(body))
	if err != nil {
		return api.ChatCompletionResponse{}, fault.Wrap(fault.InternalFailure, "model request could not be constructed", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential)

	response, err := gateway.deps.Transport.RoundTrip(request)
	if err != nil {
		if cancelled(ctx, callCtx) {
			return api.ChatCompletionResponse{}, gateway.settleFailure(ctx, scope, current.principal, model, usageID, UsageUncertain, api.ChatUsage{}, deadlineFault())
		}
		// A transport error that is not provably pre-write may have followed a
		// written request: a reset, an EOF waiting for headers or a GOAWAY after
		// the body left the process all leave the provider's work unknown. Such
		// an attempt is never replay-safe, so it settles as uncertain rather
		// than as a definitive failure. Only the transport, which owns the dial
		// and the socket, can prove that no request bytes were written.
		if preWrite, ok := gateway.deps.Transport.(PreWriteFailure); ok && preWrite.PrecedesWrite(err) {
			return api.ChatCompletionResponse{}, gateway.settleFailure(ctx, scope, current.principal, model, usageID, UsageFailed, api.ChatUsage{},
				fault.Wrap(fault.DependencyUnavailable, "the configured model provider is unreachable", err))
		}
		return api.ChatCompletionResponse{}, gateway.settleFailure(ctx, scope, current.principal, model, usageID, UsageUncertain, api.ChatUsage{},
			fault.Wrap(fault.UncertainExternalEffect, "the model provider call was lost after dispatch", err))
	}
	if response == nil || response.Body == nil {
		// A transport that reports no response after being handed the request
		// cannot prove the provider saw nothing, so the attempt stays uncertain
		// and non-replayable rather than a definitive failure.
		return api.ChatCompletionResponse{}, gateway.settleFailure(ctx, scope, current.principal, model, usageID, UsageUncertain, api.ChatUsage{},
			fault.New(fault.UncertainExternalEffect, "the configured model provider returned no response after dispatch"))
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		return api.ChatCompletionResponse{}, gateway.settleFailure(ctx, scope, current.principal, model, usageID, UsageFailed, api.ChatUsage{},
			fault.New(fault.DependencyUnavailable, "the configured model provider refused the call"))
	}
	if !isJSONContentType(response.Header.Get("Content-Type")) {
		return api.ChatCompletionResponse{}, gateway.settleFailure(ctx, scope, current.principal, model, usageID, UsageFailed, api.ChatUsage{},
			fault.New(fault.DependencyUnavailable, "the configured model provider returned a non-JSON body"))
	}

	raw, readErr := io.ReadAll(io.LimitReader(response.Body, int64(model.MaxResponseBytes)+1))
	if readErr != nil {
		if cancelled(ctx, callCtx) {
			return api.ChatCompletionResponse{}, gateway.settleFailure(ctx, scope, current.principal, model, usageID, UsageUncertain, api.ChatUsage{}, deadlineFault())
		}
		return api.ChatCompletionResponse{}, gateway.settleFailure(ctx, scope, current.principal, model, usageID, UsageUncertain, api.ChatUsage{},
			fault.Wrap(fault.UncertainExternalEffect, "the model provider response was lost after dispatch", readErr))
	}
	if len(raw) > model.MaxResponseBytes {
		return api.ChatCompletionResponse{}, gateway.settleFailure(ctx, scope, current.principal, model, usageID, UsageFailed, api.ChatUsage{},
			fault.New(fault.DependencyUnavailable, "the model provider response exceeds the configured bound"))
	}
	var completion api.ChatCompletionResponse
	if err := json.Unmarshal(raw, &completion); err != nil {
		return api.ChatCompletionResponse{}, gateway.settleFailure(ctx, scope, current.principal, model, usageID, UsageFailed, api.ChatUsage{},
			fault.Wrap(fault.DependencyUnavailable, "the model provider response is malformed", err))
	}
	if err := completion.Validate(); err != nil {
		return api.ChatCompletionResponse{}, gateway.settleFailure(ctx, scope, current.principal, model, usageID, UsageFailed, api.ChatUsage{},
			fault.Wrap(fault.DependencyUnavailable, "the model provider completion is invalid", err))
	}
	if err := gateway.settle(ctx, scope, current.principal, model, usageID, UsageSucceeded, completion.Usage); err != nil {
		return api.ChatCompletionResponse{}, unattributed(nil, err)
	}
	gateway.recordContent(ctx, scope, current.principal, model, body, raw)
	return completion, nil
}

// authenticate resolves the presented credential currently. A missing or
// unverifiable credential is a redacted unauthenticated refusal.
func (gateway *Gateway) authenticate(ctx context.Context, credential string) (authorize.Principal, error) {
	if credential == "" {
		return authorize.Principal{}, fault.New(fault.Unauthenticated, "no credential was presented")
	}
	principal, err := gateway.deps.Principals.Resolve(ctx, credential)
	if err != nil {
		if ctx.Err() != nil {
			return authorize.Principal{}, deadlineFault()
		}
		return authorize.Principal{}, fault.Wrap(fault.Unauthenticated, "the presented credential could not be verified", err)
	}
	if err := principal.Validate(); err != nil {
		return authorize.Principal{}, fault.Wrap(fault.Unauthenticated, "the presented credential did not verify a usable principal", err)
	}
	return principal, nil
}

// caller is one authenticated and currently authorized request context. The
// admitted execution context is populated only for an execution principal and
// is the sole source of the execution permission; the caller never supplies it.
type caller struct {
	principal authorize.Principal
	admitted  authorize.AdmittedExecution
	execution bool
}

// authorize answers the model.invoke grant through the shared authorizer. A
// human or service caller is decided by the operation registry. An execution
// caller is re-read from the consumer-owned execution reader and decided by its
// current admitted execution context, so neither the presented permission field
// nor a stale resolved principal can grant execution authority.
func (gateway *Gateway) authorize(ctx context.Context, principal authorize.Principal, scope tenancy.Scope) (caller, error) {
	switch principal.Kind {
	case identity.KindExecution:
		return gateway.authorizeExecution(ctx, principal, scope)
	case identity.KindHuman, identity.KindService:
		decision, err := gateway.deps.Authorizer.Authorize(ctx, principal, operationModelInvoke, scope)
		if err != nil {
			return caller{}, classifyAuthorizationError(ctx, err)
		}
		if decision != authorize.DecisionAllow {
			return caller{}, fault.New(fault.Forbidden, "the caller does not hold the model.invoke grant in this project")
		}
		return caller{principal: principal}, nil
	default:
		return caller{}, fault.New(fault.Forbidden, "the principal kind cannot invoke a configured model")
	}
}

// authorizeExecution re-reads the admitted execution and evaluates the current
// execution permission plus the current authority facts. A stopped execution, a
// withdrawn permission, a mismatched execution identity or an execution without
// an active action is refused before any provider effect.
func (gateway *Gateway) authorizeExecution(ctx context.Context, principal authorize.Principal, scope tenancy.Scope) (caller, error) {
	if principal.ExecutionID == "" {
		return caller{}, fault.New(fault.Unauthenticated, "an execution credential did not verify an execution identity")
	}
	admitted, err := gateway.executions.ReadExecution(ctx, principal.ExecutionID)
	if err != nil {
		return caller{}, classifyAuthorizationError(ctx, err)
	}
	if admitted.ExecutionID != principal.ExecutionID || admitted.Principal.ID != principal.ID {
		return caller{}, fault.New(fault.Forbidden, "the admitted execution does not match the presented principal")
	}
	decision, err := authorize.AuthorizeExecution(admitted, operationModelInvoke, scope)
	if err != nil {
		return caller{}, classifyAuthorizationError(ctx, err)
	}
	if decision != authorize.DecisionAllow {
		return caller{}, fault.New(fault.Forbidden, "the caller does not hold the model.invoke grant in this project")
	}
	// The action-deadline fact is read for the active action. An admitted
	// execution without one cannot be evaluated against a real target, so it is
	// refused instead of asking for an unbounded deadline.
	if admitted.ActionID == "" && gateway.requiresCheck(authorize.CheckActionDeadline) {
		return caller{}, fault.New(fault.Forbidden, "an execution without an active action cannot invoke a configured model")
	}
	return caller{principal: admitted.Principal, admitted: admitted, execution: true}, nil
}

// classifyAuthorizationError keeps a context deadline a deadline refusal, a
// declared fault class intact and every other boundary failure an internal one.
func classifyAuthorizationError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return deadlineFault()
	}
	if _, ok := fault.ClassOf(err); ok {
		return err
	}
	return fault.Wrap(fault.InternalFailure, "model invocation authorization failed", err)
}

// refreshAuthority re-reads the registry row's current authority facts and
// refuses unless every fact still holds. It runs before any secret resolve or
// provider call, so a revocation is never overtaken by a forwarded request.
//
// A human or service caller never evaluates execution-only facts. An execution
// caller binds the execution-scoped reads to the admitted execution and action,
// so a real authority reader evaluates the exact running execution and the
// exact action deadline rather than an empty target.
func (gateway *Gateway) refreshAuthority(ctx context.Context, current caller, scope tenancy.Scope) error {
	operation, ok := authorize.LookupOperation(operationModelInvoke)
	if !ok {
		return fault.New(fault.InternalFailure, "model.invoke is not registered")
	}
	reads := authorityReads(operation, current, scope)
	facts, err := gateway.deps.Authority.Read(ctx, reads)
	if err != nil {
		if ctx.Err() != nil {
			return deadlineFault()
		}
		if _, ok := fault.ClassOf(err); ok {
			return err
		}
		return fault.Wrap(fault.InternalFailure, "current model authority could not be read", err)
	}
	if ctx.Err() != nil {
		return deadlineFault()
	}
	if err := authorize.Confirm(reads, facts); err != nil {
		if class, ok := fault.ClassOf(err); ok && class == fault.Forbidden {
			for _, fact := range facts {
				if fact.Allowed {
					continue
				}
				if fact.Check == authorize.CheckActionDeadline {
					return deadlineFault()
				}
				break
			}
		}
		return err
	}
	if current.execution && !current.admitted.Deadline.IsZero() && !gateway.deps.Clock.Now().Before(current.admitted.Deadline) {
		return deadlineFault()
	}
	return nil
}

// authorityReads builds the registry authority reads for one caller. A human or
// service caller never evaluates execution-only facts; an execution caller
// names the admitted execution and action targets.
func authorityReads(operation authorize.Operation, current caller, scope tenancy.Scope) []authorize.AuthorityRead {
	reads := make([]authorize.AuthorityRead, 0, len(operation.Authority))
	for _, check := range operation.Authority {
		if !current.execution && executionOnly(check) {
			continue
		}
		read := authorize.AuthorityRead{
			Operation:   operation.Name,
			Required:    operation.Requires,
			Check:       check,
			Scope:       scope,
			PrincipalID: current.principal.ID,
		}
		if current.execution {
			switch check {
			case authorize.CheckExecutionRunning:
				read.TargetID = string(current.admitted.ExecutionID)
			case authorize.CheckActionDeadline:
				read.TargetID = string(current.admitted.ActionID)
			}
		}
		reads = append(reads, read)
	}
	return reads
}

// requiresCheck reports whether the model.invoke registry row names check.
func (gateway *Gateway) requiresCheck(check authorize.AuthorityCheck) bool {
	operation, ok := authorize.LookupOperation(operationModelInvoke)
	if !ok {
		return false
	}
	return slices.Contains(operation.Authority, check)
}

// executionOnly reports whether a registry check is meaningful only for an
// execution caller.
func executionOnly(check authorize.AuthorityCheck) bool {
	switch check {
	case authorize.CheckExecutionRunning, authorize.CheckActionDeadline, authorize.CheckExecutionVersion:
		return true
	default:
		return false
	}
}

// encodeRequest maps the caller's configured model name to the upstream model,
// applies the configured completion cap and refuses an encoded request above
// the configured request bound. The forwarded body is always non-streaming.
func encodeRequest(model ModelConfig, request api.ChatCompletionRequest) ([]byte, error) {
	forwarded := request
	forwarded.Model = model.UpstreamModel
	switch {
	case forwarded.MaxTokens == 0:
		forwarded.MaxTokens = model.MaxCompletionTokens
	case forwarded.MaxTokens > model.MaxCompletionTokens:
		return nil, fault.New(fault.InvalidInput, "requested max_tokens exceeds the configured completion cap")
	}
	forwarded.Stream = false
	body, err := json.Marshal(forwarded)
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "model request could not be encoded", err)
	}
	if len(body) > model.MaxRequestBytes {
		return nil, fault.New(fault.InvalidInput, "model request exceeds the configured request bound")
	}
	return body, nil
}

// resolveSecret resolves exactly the one configured secret name for this call.
// A missing, empty or unreadable value is a redacted dependency_unavailable
// refusal, and the value never enters a message, row or record.
func (gateway *Gateway) resolveSecret(ctx context.Context, scope tenancy.Scope, name id.SecretName) (string, error) {
	resolved, err := gateway.deps.Secrets.Resolve(ctx, secret.ResolveRequest{
		Scope: scope,
		Names: []id.SecretName{name},
	})
	if err != nil {
		if ctx.Err() != nil {
			return "", deadlineFault()
		}
		return "", fault.Wrap(fault.DependencyUnavailable, "the configured model credential is unavailable", err)
	}
	if len(resolved) != 1 || resolved[0].Name != name || resolved[0].Value == "" {
		return "", fault.New(fault.DependencyUnavailable, "the configured model credential is unavailable")
	}
	if resolved[0].Scope != scope {
		return "", fault.New(fault.DependencyUnavailable, "the configured model credential was resolved outside its project")
	}
	if err := secret.DefaultBounds.CheckValue(len(resolved[0].Value)); err != nil {
		return "", fault.Wrap(fault.DependencyUnavailable, "the configured model credential is unusable", err)
	}
	return resolved[0].Value, nil
}

// settleFailure persists one dispatched call's usage row and returns the
// primary refusal, or the settlement failure when attribution cannot be
// persisted.
func (gateway *Gateway) settleFailure(
	ctx context.Context,
	scope tenancy.Scope,
	principal authorize.Principal,
	model ModelConfig,
	usageID UsageID,
	outcome UsageOutcome,
	usage api.ChatUsage,
	primary error,
) error {
	if err := gateway.settle(ctx, scope, principal, model, usageID, outcome, usage); err != nil {
		return unattributed(primary, err)
	}
	return primary
}

// unattributed is the refusal for a call the transport already carried whose
// usage row could not be persisted. The provider may have processed the
// request while the ledger cannot say so, therefore the outcome is unknown and
// never replay-safe: the class is uncertain_external_effect, not a bare
// internal_failure. Both the lost outcome and the settlement cause stay
// reachable through errors.Is/errors.As for diagnostics.
func unattributed(outcome, cause error) error {
	if outcome == nil {
		return fault.Wrap(fault.UncertainExternalEffect,
			"the model provider call outcome could not be attributed", cause)
	}
	return fault.Wrap(fault.UncertainExternalEffect,
		"the model provider call outcome could not be attributed", errors.Join(outcome, cause))
}

// settle persists exactly one attributable usage row on a settlement context
// that survives caller cancellation. Unknown usage is written as honest zero
// counts, never a fabricated total. The row is the declared UsageRecord.Unit,
// proved to conform to the declared schema before any statement runs.
func (gateway *Gateway) settle(
	ctx context.Context,
	scope tenancy.Scope,
	principal authorize.Principal,
	model ModelConfig,
	usageID UsageID,
	outcome UsageOutcome,
	usage api.ChatUsage,
) error {
	known := outcome == UsageSucceeded
	if !known {
		usage = api.ChatUsage{}
	}
	record := UsageRecord{
		ID:            usageID,
		Scope:         scope,
		PrincipalID:   principal.ID,
		ExecutionID:   principal.ExecutionID,
		Model:         model.Model,
		UpstreamModel: model.UpstreamModel,
		Outcome:       outcome,
		UsageKnown:    known,
		Usage:         usage,
		At:            gateway.deps.Clock.Now(),
	}
	unit, err := record.Unit()
	if err != nil {
		return fault.Wrap(fault.InternalFailure, "model usage record is unattributable", err)
	}
	if err := conformUnit(unit, UsageTable); err != nil {
		return fault.Wrap(fault.InternalFailure, "model usage unit does not conform to the frozen schema", err)
	}
	if _, err := gateway.deps.Store.Execute(context.WithoutCancel(ctx), scope, unit); err != nil {
		return fault.Wrap(fault.InternalFailure, "model usage could not be persisted", err)
	}
	return nil
}

// recordContent writes raw request/response bytes only when the configured
// model opts in. A capture failure never rewrites an already-settled call
// outcome, and the provider credential is not part of either byte string.
func (gateway *Gateway) recordContent(
	ctx context.Context,
	scope tenancy.Scope,
	principal authorize.Principal,
	model ModelConfig,
	request []byte,
	response []byte,
) {
	if !model.RecordContent {
		return
	}
	retention := model.ContentRetention
	if retention <= 0 {
		retention = DefaultContentRetention
	}
	at := gateway.deps.Clock.Now()
	_ = gateway.deps.Content.Record(ctx, ContentRecord{
		Scope:       scope,
		PrincipalID: principal.ID,
		ExecutionID: principal.ExecutionID,
		Model:       model.Model,
		Request:     request,
		Response:    response,
		At:          at,
		RetainUntil: at.Add(retention),
	})
}

// deadlineFault is the stable refusal for an expired or cancelled invocation.
func deadlineFault() error {
	return fault.New(fault.DeadlineExpired, "the model invocation deadline expired or the caller cancelled")
}

// cancelled reports whether the caller context or the configured timeout
// context ended the attempt.
func cancelled(ctx, callCtx context.Context) bool {
	return ctx.Err() != nil || callCtx.Err() != nil
}

// isJSONContentType accepts an application/json media type with parameters.
func isJSONContentType(value string) bool {
	if index := strings.IndexByte(value, ';'); index >= 0 {
		value = value[:index]
	}
	return strings.EqualFold(strings.TrimSpace(value), "application/json")
}
