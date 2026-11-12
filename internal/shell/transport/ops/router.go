package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// binding is one immutable registry entry. It owns copies of the caller's
// codecs/handler values, so mutating the caller's Binding slice after
// construction cannot change a running router.
type binding struct {
	operation authorize.Operation
	input     Codec
	result    Codec
	handler   Handler
}

// Router is the immutable operation router. It holds no per-call state and is
// safe for concurrent use: every invocation keeps its own principal, scope,
// request and result.
type Router struct {
	principals authorize.CurrentPrincipal
	scopes     ScopeResolver
	executions ExecutionAuthority
	authorizer authorize.Authorizer
	bindings   map[authorize.OperationName]binding
}

// NewRouter builds the immutable, complete operation registry. It refuses a nil
// dependency, a missing or duplicate binding, an empty or unknown operation
// name, a nil input/result codec and a nil handler.
func NewRouter(config Config) (*Router, error) {
	if config.Principals == nil {
		return nil, fault.New(fault.InvalidInput, "router principals dependency is nil")
	}
	if config.Scopes == nil {
		return nil, fault.New(fault.InvalidInput, "router scope resolver dependency is nil")
	}
	if config.Executions == nil {
		return nil, fault.New(fault.InvalidInput, "router execution authority dependency is nil")
	}
	if len(config.Bindings) == 0 {
		return nil, fault.New(fault.InvalidInput, "router has no operation bindings")
	}

	router := &Router{
		principals: config.Principals,
		scopes:     config.Scopes,
		executions: config.Executions,
		authorizer: authorize.NewMatrix(),
		bindings:   make(map[authorize.OperationName]binding, len(config.Bindings)),
	}

	for index, candidate := range config.Bindings {
		operation, ok := authorize.LookupOperation(candidate.Operation)
		if !ok {
			return nil, fault.New(fault.InvalidInput,
				fmt.Sprintf("binding %d names unknown operation %q", index, candidate.Operation))
		}
		if _, duplicate := router.bindings[operation.Name]; duplicate {
			return nil, fault.New(fault.InvalidInput,
				fmt.Sprintf("operation %q has more than one binding", operation.Name))
		}
		if candidate.Input == nil {
			return nil, fault.New(fault.InvalidInput,
				fmt.Sprintf("operation %q has a nil input codec", operation.Name))
		}
		if candidate.Result == nil {
			return nil, fault.New(fault.InvalidInput,
				fmt.Sprintf("operation %q has a nil result codec", operation.Name))
		}
		if candidate.Handler == nil {
			return nil, fault.New(fault.InvalidInput,
				fmt.Sprintf("operation %q has a nil handler", operation.Name))
		}
		router.bindings[operation.Name] = binding{
			operation: operation,
			input:     candidate.Input,
			result:    candidate.Result,
			handler:   candidate.Handler,
		}
	}

	for _, operation := range authorize.Operations() {
		if _, ok := router.bindings[operation.Name]; !ok {
			return nil, fault.New(fault.InvalidInput,
				fmt.Sprintf("operation %q has no binding", operation.Name))
		}
	}
	return router, nil
}

// Invoke routes one decoded edge call through the fixed boundary order:
// structural operation lookup, authentication, scope/execution ports,
// authorization, request envelope and receipt policy, path/body agreement,
// input codec, handler, result validation.
func (router *Router) Invoke(ctx context.Context, invocation Invocation) (api.Result, error) {
	entry, ok := router.bindings[invocation.Operation]
	if !ok {
		return api.Result{}, fault.New(fault.InvalidInput,
			fmt.Sprintf("unknown operation %q", invocation.Operation))
	}

	// Authentication is the first boundary: a failed authentication never
	// reaches a scope lookup, input validation or a handler, so no resource
	// lookup can expose scoped information.
	principal, err := router.principals.Resolve(ctx, invocation.Credential)
	if err != nil {
		return api.Result{}, sanitize(err)
	}
	if err := principal.Validate(); err != nil {
		return api.Result{}, sanitize(err)
	}

	// Scope/targets are resolved by the injected ownership boundary. A resolver
	// refusal is final; there is no fallback to an empty scope.
	resolution, err := router.scopes.Resolve(ctx, principal, Call{
		Operation: entry.operation.Name,
		Request:   invocation.Request,
		Params:    invocation.Params,
	})
	if err != nil {
		return api.Result{}, sanitize(err)
	}

	if err := router.authorize(ctx, principal, entry.operation.Name, resolution.Scope); err != nil {
		return api.Result{}, err
	}

	// The envelope and receipt policy are shared by both edges and are checked
	// before any effect. A receipt-bearing mutation never reaches its handler
	// without the caller key the capability needs for its atomic receipt.
	if err := invocation.Request.Validate(); err != nil {
		return api.Result{}, refusal(err)
	}
	if err := receiptPolicy(entry.operation, invocation.Request); err != nil {
		return api.Result{}, err
	}

	// A REST path parameter and a same-named top-level input field must agree;
	// the path never silently overrides or duplicates the body.
	if err := validatePathBody(invocation.Request.Input, invocation.Params); err != nil {
		return api.Result{}, err
	}

	if err := entry.input.Validate(invocation.Request.Input); err != nil {
		return api.Result{}, refusal(err)
	}

	result, err := entry.handler.Handle(ctx, AuthorizedCall{
		Principal: principal,
		Operation: entry.operation,
		Scope:     resolution.Scope,
		Targets:   resolution.Targets,
		Request:   invocation.Request,
	})
	if err != nil {
		return api.Result{}, sanitize(err)
	}
	if result.Operation != string(entry.operation.Name) {
		return api.Result{}, fault.New(fault.InternalFailure, "handler result names a different operation")
	}
	if err := result.Validate(); err != nil {
		return api.Result{}, sanitize(err)
	}
	if err := entry.result.Validate(result.Body); err != nil {
		return api.Result{}, sanitize(err)
	}
	return result, nil
}

// authorize runs the single shared grant evaluation. A human or service
// principal is decided by the grant matrix; an execution principal is admitted
// by the injected port and decided by its closed execution permission. An
// execution principal never falls back to a human membership grant.
func (router *Router) authorize(ctx context.Context, principal authorize.Principal, name authorize.OperationName, scope tenancy.Scope) error {
	if principal.Kind == identity.KindExecution {
		admitted, err := router.executions.Admit(ctx, principal)
		if err != nil {
			return sanitize(err)
		}
		if admitted.Principal.ID != principal.ID || admitted.ExecutionID != principal.ExecutionID {
			return fault.New(fault.Forbidden, "execution authority does not match the authenticated principal")
		}
		decision, err := authorize.AuthorizeExecution(admitted, name, scope)
		if err != nil {
			return sanitize(err)
		}
		if decision != authorize.DecisionAllow {
			return fault.New(fault.Forbidden,
				fmt.Sprintf("execution is not authorized for operation %q", name))
		}
		return nil
	}

	decision, err := router.authorizer.Authorize(ctx, principal, name, scope)
	if err != nil {
		return sanitize(err)
	}
	if decision != authorize.DecisionAllow {
		return fault.New(fault.Forbidden,
			fmt.Sprintf("principal %q is not authorized for operation %q", principal.ID, name))
	}
	return nil
}

// receiptPolicy enforces the registry's idempotency-receipt requirement. The
// router owns no receipt store: it only refuses a required key that is missing
// and otherwise preserves the caller's key, content and expected version for the
// capability's atomic effect+receipt commit.
func receiptPolicy(operation authorize.Operation, request api.Request) error {
	switch operation.Receipt {
	case authorize.ReceiptRequired:
		if request.IdempotencyKey == "" {
			return fault.New(fault.InvalidInput,
				fmt.Sprintf("operation %q requires an idempotency_key", operation.Name))
		}
	case authorize.ReceiptNone, authorize.ReceiptRequestScoped, authorize.ReceiptPushScoped:
	default:
		return fault.New(fault.InternalFailure,
			fmt.Sprintf("operation %q has an unknown receipt policy", operation.Name))
	}
	return nil
}

// validatePathBody refuses a REST path parameter that appears in the request
// input with a different value or a non-string value. A parameter absent from
// the body is fine: the capability receives both the path parameters and the
// unchanged envelope.
func validatePathBody(input json.RawMessage, params map[string]string) error {
	if len(params) == 0 {
		return nil
	}
	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return fault.New(fault.InvalidInput, "request input is not a JSON object")
	}
	for name, want := range params {
		raw, present := fields[name]
		if !present {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return fault.New(fault.InvalidInput,
				fmt.Sprintf("path parameter %q must be a string that agrees with the request input", name))
		}
		if value != want {
			return fault.New(fault.InvalidInput,
				fmt.Sprintf("path parameter %q disagrees with the request input", name))
		}
	}
	return nil
}

// sanitize keeps a typed fault's public class/code/message and converts any
// other error into a generic internal failure that never renders its cause.
func sanitize(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := fault.ClassOf(err); ok {
		return err
	}
	// The client gets a generic failure; the operator gets the original, which
	// is an untyped adapter error (for example a database diagnostic). The
	// rendered text is redacted so a connection string, header or password that
	// an adapter put into its error text never reaches the log.
	slog.Error("untyped operation error sanitized", "error", RedactForLog(err.Error()))
	return fault.New(fault.InternalFailure, "internal failure")
}

// logCredentialPatterns are the credential shapes a private cause may carry.
// They are matched case-insensitively so a differently cased header or label
// cannot reach the server log. Redaction keeps the label readable.
var logCredentialPatterns = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	// A whole PEM private key block. It runs first: the base64 body may
	// otherwise look like a labelled value (padding ends with '=').
	{regexp.MustCompile(`(?is)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`), `[redacted private key]`},
	// URL userinfo for ANY scheme (postgres, https, nats, amqp, ...). The
	// match is greedy to the LAST '@' so a password containing '@' does not
	// leak its tail; user and host stay readable for diagnostics. The user
	// part may be empty (tls://:pw@host), which still carries a credential.
	// A userinfo token with an EMPTY password (https://TOKEN:@host) is handled
	// first: the "user" there is the whole credential.
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^\s:/@]+:@`), `${1}[redacted]@`},
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^\s:/@]*:)[^\s]*@`), `${1}[redacted]@`},
	// A user-only URL userinfo (https://TOKEN@host). A token is often the
	// whole user part (a GitHub PAT, for example), so it is redacted too.
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^\s:/@]+@`), `${1}[redacted]@`},
	// An Authorization header value, including its scheme (Bearer, Basic,
	// Token). Only the credential is removed so a diagnostic that follows the
	// header stays readable.
	{regexp.MustCompile(`(?i)\b((?:proxy-)?authorization)["']?[ \t]*[=:](?:["'][ \t\r\n]+|[ \t\r\n]*)(?:[A-Za-z]+[ \t\r\n]+)?("[^"\r\n]*"|'[^'\r\n]*'|[^\s,;'"]+)`), `${1}: [redacted]`},
	// A Cookie or Set-Cookie header value: cookie pairs carry spaces and
	// semicolons, so the whole line is the credential.
	{regexp.MustCompile(`(?i)\b((?:set-)?cookie)["']?[ \t]*[=:][ \t]*[^\r\n]+`), `${1}: [redacted]`},
	// An unlabelled scheme value: "Bearer x", "Basic x", "ApiKey x",
	// "Token x". Authorization: Token x is covered above; this covers the
	// value a diagnostic echoed without its label, including one the renderer
	// wrapped onto the next line.
	{regexp.MustCompile(`(?i)\b(bearer|basic|apikey|api[_-]key|token)[ \t\r\n]+[A-Za-z0-9._~+/=-]+`), `${1} [redacted]`},
	// A SQL password literal, with an optional E prefix and doubled quotes:
	// ALTER ROLE ... PASSWORD 'x', PASSWORD E'x' and PASSWORD 'it''s x'.
	{regexp.MustCompile(`(?i)\b(password[ \t\r\n]+E?)'((?:[^']|'')*)'`), `${1}'[redacted]'`},
	// A SQL password literal in dollar quoting: PASSWORD $$x$$. It spans
	// lines because a wrapped diagnostic may split the quoted value.
	{regexp.MustCompile(`(?is)\b(password[ \t\r\n]+E?)\$\$.*?\$\$`), `${1}[redacted]`},
	// A password-family label whose value starts on the next line, as a
	// wrapped diagnostic renders it: password:\nvalue. Only the password
	// family crosses the newline, so a following unrelated line stays
	// readable.
	{regexp.MustCompile(`(?i)\b(password|passwd|passphrase|pass|pwd)["']?[ \t]*[=:][ \t\r\n]+("[^"\r\n]*"|'[^'\r\n]*'|[^\s,;]+)`), `${1}: [redacted]`},
	// A short "pw" assignment. It is separate from the general label rule
	// because "pw" as a substring would over-match words such as "upward";
	// the word boundary keeps the label exact.
	{regexp.MustCompile(`(?i)\b(pw)["']?[ \t]*[=:][ \t]*("(?:[^"\r\n]|"")*"|'(?:[^'\r\n]|'')*'|[^\s,;]+)`), `${1}: [redacted]`},
	// A quoted credential label followed by a newline and its quoted value,
	// as a multi-line diagnostic renders it: 'PASSWORD'\n'x'.
	{regexp.MustCompile(`(?i)(['"](?:secret|password|passwd|passphrase|pwd|pass|token|key|credential|cookie)['"][ \t\r\n]+)("[^"\r\n]*"|'[^'\r\n]*')`), `${1}[redacted]`},
	// A labelled credential assignment. The label is any key CONTAINING one
	// of the credential words, so the product's own SCW_SECRET_KEY and
	// secret_key shapes are covered, not only keys ending in "secret".
	{regexp.MustCompile(`(?i)((?:^|[^A-Za-z0-9])(?:[A-Za-z0-9_-]*(?:secret|password|passwd|passphrase|pass|pwd|token|key|credential|cookie)[A-Za-z0-9_-]*)["']?[ \t]*[=:][ \t]*)("(?:[^"\r\n]|"")*"|'(?:[^'\r\n]|'')*'|[^\s,;]+)`), `${1}[redacted]`},
	// An unlabelled JWT: three base64url segments, the first a JSON header.
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`), `[redacted jwt]`},
}

// RedactForLog renders one private diagnostic for the server log with DSN
// passwords, Authorization values, password literals and private keys
// replaced. It is used by both edges: the sanitized client response is
// unchanged, only the operator log is filtered.
func RedactForLog(text string) string {
	for _, rule := range logCredentialPatterns {
		text = rule.pattern.ReplaceAllString(text, rule.replacement)
	}
	return text
}

// refusal keeps a typed fault and maps an api.Error refusal (the schema
// package's public code vocabulary is the same one-to-one class table) to the
// matching fault class with its public message. Anything else is an internal
// failure.
func refusal(err error) error {
	if err == nil {
		return nil
	}
	var publicError *api.Error
	if errors.As(err, &publicError) && publicError != nil {
		if class, ok := fault.ClassFor(fault.Code(publicError.Code)); ok {
			message := publicError.Message
			if message == "" {
				message = string(class)
			}
			return fault.New(class, message)
		}
	}
	return sanitize(err)
}
