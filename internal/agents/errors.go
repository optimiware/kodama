package agents

import (
	"errors"
	"fmt"
	"strings"
)

// Failure classes of the agents module. Each is a sentinel callers classify
// with errors.Is; transport adapters map them to their own status codes.
var (
	// ErrInvalid: the draft or a reference is malformed or violates a profile
	// rule. The concrete error is a *ValidationError with bounded diagnostics.
	ErrInvalid = errors.New("agents: invalid profile")
	// ErrUnsupported: the wrapper/harness/image tuple, or a claimed control, is
	// not covered by the installed compatibility evidence. Unsupported profiles
	// fail registration (AR-02) and expansion.
	ErrUnsupported = errors.New("agents: unsupported wrapper or control")
	// ErrNotFound: the profile does not exist in the caller's scope. Hidden and
	// foreign profiles are indistinguishable from missing ones.
	ErrNotFound = errors.New("agents: not found")
	// ErrDenied / ErrHidden: authorization refusals.
	ErrDenied = errors.New("agents: denied")
	ErrHidden = errors.New("agents: hidden")
	// ErrUnavailable: a required dependency (database, authority, compatibility
	// or approval policy) is unavailable or not configured, or a write outcome
	// is unknown. Nothing is replayed automatically.
	ErrUnavailable = errors.New("agents: unavailable")
	// ErrOutcomeUnknown refines ErrUnavailable: the transaction may or may not
	// have committed. Registration is content-addressed, so a retry converges.
	ErrOutcomeUnknown = errors.New("agents: outcome unknown")
	// ErrConflict: stored state contradicts the request in a way a retry cannot
	// fix, for example a digest collision with different content.
	ErrConflict = errors.New("agents: conflict")
)

// Diagnostic codes are stable identifiers; messages are constant text and never
// echo instruction content or other draft values.
const (
	CodeInvalidField    = "invalid_field"
	CodeLimitExceeded   = "limit_exceeded"
	CodeUnapprovedModel = "unapproved_model"
	CodeUnapprovedTool  = "unapproved_tool"
)

// Diagnostic is one bounded, client-safe finding at a draft path.
type Diagnostic struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

// MaxDiagnostics bounds how many findings one failure reports.
const MaxDiagnostics = 25

// ValidationError is the failure of validating a draft. It satisfies
// errors.Is(err, ErrInvalid).
type ValidationError struct {
	Diagnostics []Diagnostic
	Truncated   bool
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	b.WriteString(ErrInvalid.Error())
	for i, d := range e.Diagnostics {
		if i == 3 {
			fmt.Fprintf(&b, "; and %d more", len(e.Diagnostics)-3)
			break
		}
		fmt.Fprintf(&b, "; %s at %s", d.Code, d.Path)
	}
	return b.String()
}

// Is makes the error match ErrInvalid.
func (e *ValidationError) Is(target error) bool { return target == ErrInvalid }

type diagnostics struct {
	list      []Diagnostic
	truncated bool
}

func (d *diagnostics) add(code, path, format string, args ...any) {
	if len(d.list) >= MaxDiagnostics {
		d.truncated = true
		return
	}
	d.list = append(d.list, Diagnostic{Code: code, Path: path, Message: fmt.Sprintf(format, args...)})
}

func (d *diagnostics) err() error {
	if len(d.list) == 0 {
		return nil
	}
	return &ValidationError{Diagnostics: append([]Diagnostic(nil), d.list...), Truncated: d.truncated}
}

// Unavailablef builds an ErrUnavailable error wrapping cause.
func Unavailablef(cause error, format string, args ...any) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrUnavailable, fmt.Sprintf(format, args...))
	}
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, fmt.Sprintf(format, args...), cause)
}

// Unsupportedf builds an ErrUnsupported with a stable, content-free message.
func Unsupportedf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupported, fmt.Sprintf(format, args...))
}

// IsBusinessError reports whether err is one of this module's classified
// failures rather than an unclassified infrastructure error.
func IsBusinessError(err error) bool {
	for _, s := range []error{ErrInvalid, ErrUnsupported, ErrNotFound, ErrDenied, ErrHidden, ErrUnavailable, ErrConflict} {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}
