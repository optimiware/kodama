package pipeline

import (
	"errors"
	"fmt"
	"strings"
)

// Failure classes of the pipeline module. Each is a sentinel callers classify
// with errors.Is; transport adapters map them to their own status codes.
var (
	// ErrInvalid: the document is malformed or violates a definition rule. The
	// concrete error is a *ValidationError carrying bounded diagnostics.
	ErrInvalid = errors.New("pipeline: invalid definition")
	// ErrNotFound: the definition (or its project) does not exist in the
	// caller's scope. Hidden references are indistinguishable from missing ones.
	ErrNotFound = errors.New("pipeline: not found")
	// ErrDenied: the caller is authenticated but not permitted.
	ErrDenied = errors.New("pipeline: denied")
	// ErrHidden: the caller must not learn whether the resource exists.
	ErrHidden = errors.New("pipeline: hidden")
	// ErrUnavailable: a required dependency (database, authority, capability,
	// profile or reference lookup) is unavailable, or the outcome of a write is
	// unknown. Nothing is replayed automatically; the caller retries.
	ErrUnavailable = errors.New("pipeline: unavailable")
	// ErrOutcomeUnknown refines ErrUnavailable: the transaction may or may not
	// have committed. Registration is content-addressed, so a retry converges.
	ErrOutcomeUnknown = errors.New("pipeline: outcome unknown")
	// ErrConflict: stored state contradicts the request in a way a retry cannot
	// fix, for example a digest collision with different content.
	ErrConflict = errors.New("pipeline: conflict")

	// ErrReferenceNotFound is returned by the profile and reference ports when
	// the named profile, secret or environment does not exist in the scope.
	ErrReferenceNotFound = errors.New("pipeline: reference not found")
	// ErrReferenceDenied is returned by the reference policy when the actor may
	// not use an existing reference in this way.
	ErrReferenceDenied = errors.New("pipeline: reference denied")
)

// Stage tells whether a rejected document failed to parse into the contract's
// shape or failed a definition rule. Transports use it to separate malformed
// requests from semantic failures.
type Stage string

const (
	// StageSyntax: not one well-formed document of the contract's shape.
	StageSyntax Stage = "syntax"
	// StageSemantic: well-formed, but violates graph, reference, type, source,
	// capability or policy rules.
	StageSemantic Stage = "semantic"
)

// Diagnostic codes. They are stable identifiers; messages are constant text and
// never echo commands, environment values or other document content.
const (
	CodeInvalidDocument = "invalid_document"
	CodeTooLarge        = "document_too_large"
	CodeTooDeep         = "document_too_deep"
	CodeTooManyNodes    = "document_too_complex"
	CodeDuplicateKey    = "duplicate_key"
	CodeMultiple        = "multiple_documents"
	CodeUnsupportedYAML = "unsupported_yaml"
	CodeUnknownField    = "unknown_field"
	CodeMissingField    = "missing_field"
	CodeInvalidType     = "invalid_type"
	CodeInvalidValue    = "invalid_value"
	CodeInvalidFormat   = "invalid_format"
	CodeLimitExceeded   = "limit_exceeded"

	CodeDuplicateStep    = "duplicate_step"
	CodeReservedID       = "reserved_id"
	CodeUnknownStep      = "unknown_step"
	CodeCycle            = "cycle"
	CodeUnknownInput     = "unknown_input"
	CodeUnknownOutput    = "unknown_output"
	CodeNotAncestor      = "not_ancestor"
	CodeTypeMismatch     = "type_mismatch"
	CodeInterpolation    = "interpolation"
	CodeMutableImage     = "mutable_image"
	CodeParallelWriters  = "parallel_writers"
	CodeInvalidSource    = "invalid_source"
	CodeConditionalGate  = "conditional_gate"
	CodeGateField        = "gate_field"
	CodeInvalidWhen      = "invalid_when"
	CodeInvalidRetry     = "invalid_retry"
	CodeInvalidTimeout   = "invalid_timeout"
	CodeInvalidOutput    = "invalid_output"
	CodeReservedOutput   = "reserved_output"
	CodeKindDisabled     = "kind_disabled"
	CodeUnknownProfile   = "unknown_profile"
	CodeMutableProfile   = "mutable_profile"
	CodeSecretDenied     = "secret_denied"
	CodeInvalidSecretRef = "invalid_secret_ref"
	CodeInvalidEnv       = "invalid_environment"
)

// Diagnostic is one bounded, client-safe finding at a document path.
type Diagnostic struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

// MaxDiagnostics bounds how many findings one failure reports.
const MaxDiagnostics = 25

// ValidationError is the failure of parsing or validating a document. It
// satisfies errors.Is(err, ErrInvalid).
type ValidationError struct {
	Stage       Stage
	Diagnostics []Diagnostic
	// Truncated reports that more findings existed than MaxDiagnostics.
	Truncated bool
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

// diagnostics collects findings up to MaxDiagnostics.
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

func (d *diagnostics) any() bool { return len(d.list) > 0 }

func (d *diagnostics) err(stage Stage) error {
	if !d.any() {
		return nil
	}
	return &ValidationError{Stage: stage, Diagnostics: append([]Diagnostic(nil), d.list...), Truncated: d.truncated}
}

// Unavailablef builds an ErrUnavailable error wrapping cause.
func Unavailablef(cause error, format string, args ...any) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrUnavailable, fmt.Sprintf(format, args...))
	}
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, fmt.Sprintf(format, args...), cause)
}

// IsBusinessError reports whether err is one of this module's classified
// failures rather than an unclassified infrastructure error.
func IsBusinessError(err error) bool {
	for _, s := range []error{ErrInvalid, ErrNotFound, ErrDenied, ErrHidden, ErrUnavailable, ErrConflict} {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}
