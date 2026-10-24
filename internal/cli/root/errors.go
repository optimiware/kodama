package root

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/optimiware/kodama/internal/cli/client"
)

// ExitCode is the process exit status of a command.
type ExitCode int

// Documented exit codes (docs/specs/api-cli-and-integrations.md).
const (
	ExitOK       ExitCode = 0 // success
	ExitFailure  ExitCode = 1 // remote or operation failure
	ExitUsage    ExitCode = 2 // usage or validation
	ExitAuth     ExitCode = 3 // authentication or authorization
	ExitConflict ExitCode = 4 // conflict
	ExitTimeout  ExitCode = 5 // timeout or interruption
)

// Failure is implemented by errors that choose their own exit code and stable
// machine-readable code. Contributions return such errors from their commands.
type Failure interface {
	error
	CLI() (ExitCode, string)
}

// Error is the general local failure. Cause is retained for errors.Is/As and is
// never rendered, so a wrapped lower-level message cannot leak into output.
type Error struct {
	Exit    ExitCode
	Code    string
	Message string
	Details map[string]any
	Cause   error
}

// NewError builds a local failure.
func NewError(exit ExitCode, code, format string, args ...any) *Error {
	return &Error{Exit: exit, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Usagef is a usage or validation failure (exit 2).
func Usagef(format string, args ...any) *Error {
	return NewError(ExitUsage, "usage", format, args...)
}

// WithCause returns a copy retaining cause for errors.Is/As only.
func (e *Error) WithCause(cause error) *Error {
	c := *e
	c.Cause = cause
	return &c
}

func (e *Error) Error() string { return e.Message }

// Unwrap exposes the retained cause.
func (e *Error) Unwrap() error { return e.Cause }

// CLI implements Failure.
func (e *Error) CLI() (ExitCode, string) { return e.Exit, e.Code }

// Problem is the classified, renderable form of a failure. RequestID is empty
// for local failures: the CLI never fabricates a server request ID.
type Problem struct {
	Exit              ExitCode
	Code              string
	Message           string
	RequestID         string
	Details           map[string]any
	Status            int   // HTTP status of a remote failure, else zero
	RetryAfterSeconds int64 // server retry guidance, else zero
	// OutcomeUncertain marks an operation whose effect is unknown (a mutation
	// that may have been applied, a credential that may have been saved);
	// Details then carries outcomeUncertain and recovery guidance.
	OutcomeUncertain bool
	// Recovery is the operator guidance for the specific uncertain operation.
	// Resource mutations use RecoveryGuidance; other operations (login) supply
	// their own, never the resource-mutation text.
	Recovery string
}

// Classify maps any error to its exit code and rendering.
//
//	remote 401/403 -> 3; 409/412 -> 4; 428 -> 2; every other remote status
//	(including 429 and 503) -> 1; missing credential -> 3; cancellation or
//	deadline -> 5; usage -> 2; anything else -> 1.
func Classify(err error) Problem {
	var re *client.RemoteError
	if errors.As(err, &re) {
		return classifyRemote(re)
	}
	var te *client.TransportError
	if errors.As(err, &te) {
		return classifyTransport(err, te)
	}
	var f Failure
	if errors.As(err, &f) {
		exit, code := f.CLI()
		p := Problem{Exit: exit, Code: code, Message: f.Error()}
		var le *Error
		if errors.As(err, &le) {
			p.Details = le.Details
			if le.Details["outcomeUncertain"] == true {
				p.OutcomeUncertain = true
				p.Recovery, _ = le.Details["recovery"].(string)
			}
		}
		return p
	}
	switch {
	case errors.Is(err, client.ErrNotAuthenticated):
		return Problem{Exit: ExitAuth, Code: "not_authenticated",
			Message: "no usable credential for this factory; run `kodama auth login`"}
	case errors.Is(err, client.ErrInvalidRequest):
		return Problem{Exit: ExitUsage, Code: "usage", Message: err.Error()}
	case errors.Is(err, context.DeadlineExceeded):
		return Problem{Exit: ExitTimeout, Code: "timeout", Message: "the operation timed out"}
	case errors.Is(err, context.Canceled):
		return Problem{Exit: ExitTimeout, Code: "interrupted", Message: "the operation was interrupted"}
	}
	return Problem{Exit: ExitFailure, Code: "operation_failed", Message: err.Error()}
}

// RecoveryGuidance tells the operator how to resolve a mutation whose outcome
// is unknown. It is independent of the exit code: a timed-out mutation exits 5
// and still carries it.
const RecoveryGuidance = "the request may have been applied: reread the resource, or replay the identical request with the identical --idempotency-key"

// markUncertain records that a mutation may have been applied.
func markUncertain(p *Problem) {
	d := make(map[string]any, len(p.Details)+2)
	for k, v := range p.Details {
		d[k] = v
	}
	d["outcomeUncertain"] = true
	d["recovery"] = RecoveryGuidance
	p.Details = d
	p.OutcomeUncertain = true
	p.Recovery = RecoveryGuidance
}

// UncertainDetails builds Error.Details for a local operation whose outcome is
// unknown, with operation-specific recovery guidance. Classify surfaces it in
// both output formats.
func UncertainDetails(recovery string) map[string]any {
	return map[string]any{"outcomeUncertain": true, "recovery": recovery}
}

// classifyTransport keeps the exit code (5 for cancellation and timeout, else
// 1) separate from outcome uncertainty, so neither hides the other.
func classifyTransport(err error, te *client.TransportError) Problem {
	p := Problem{Exit: ExitFailure, Code: "transport_error", Message: te.Error()}
	switch {
	case errors.Is(err, context.DeadlineExceeded), isNetTimeout(err):
		// Includes timeouts that do not wrap context.DeadlineExceeded, such
		// as a stalled TLS handshake or a transport-level deadline.
		p = Problem{Exit: ExitTimeout, Code: "timeout", Message: "the operation timed out"}
	case errors.Is(err, context.Canceled):
		p = Problem{Exit: ExitTimeout, Code: "interrupted", Message: "the operation was interrupted"}
	case errors.Is(err, client.ErrResponseTooLarge):
		p.Code = "response_too_large"
	case errors.Is(err, client.ErrMalformedResponse):
		p.Code = "malformed_response"
	}
	if te.Uncertain {
		markUncertain(&p)
	}
	return p
}

// isNetTimeout reports whether err wraps a net.Error that timed out.
func isNetTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func classifyRemote(re *client.RemoteError) Problem {
	p := Problem{
		Exit: ExitFailure, Code: re.Code, Message: re.Message, RequestID: re.RequestID,
		Details: re.Details, Status: re.Status, RetryAfterSeconds: int64(re.RetryAfter.Seconds()),
	}
	if re.Malformed {
		p.Code = "unexpected_response"
		p.Message = fmt.Sprintf("the factory answered %d with an unexpected response", re.Status)
	}
	if re.Malformed && re.Mutation {
		// No usable answer to a mutation (for example a gateway error page):
		// the server may still have applied it.
		markUncertain(&p)
	}
	switch re.Status {
	case http.StatusUnauthorized, http.StatusForbidden:
		p.Exit = ExitAuth
	case http.StatusConflict, http.StatusPreconditionFailed:
		p.Exit = ExitConflict
	case http.StatusPreconditionRequired:
		p.Exit = ExitUsage
	}
	return p
}
