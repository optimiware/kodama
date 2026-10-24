package workspace

import (
	"errors"
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
)

// Sentinel errors. Use errors.Is; adapters translate them to HTTP/runtime
// codes. None of the messages carries credentials, repository contents or
// foreign identifiers.
var (
	// ErrNotFound covers an absent workspace and a reference owned by another
	// tenant/project; callers must not distinguish them.
	ErrNotFound = errors.New("workspace: not found")
	// ErrDenied is an authorization refusal.
	ErrDenied = errors.New("workspace: access denied")
	// ErrNotActionable: the work item cannot own a workspace (an epic).
	ErrNotActionable = errors.New("workspace: work item is not actionable")
	// ErrInvalidTransition: the entity does not allow the requested change.
	ErrInvalidTransition = errors.New("workspace: invalid state transition")
	// ErrVersionConflict: a stale expected version (If-Match).
	ErrVersionConflict = errors.New("workspace: version conflict")
	// ErrWriterBusy: another attempt already holds the publication slot.
	ErrWriterBusy = errors.New("workspace: a writer is already reserved")
	// ErrNotWriter: the attempt is not the reserved writer.
	ErrNotWriter = errors.New("workspace: attempt is not the reserved writer")
	// ErrAttemptRevoked: the attempt has no current authority to publish.
	ErrAttemptRevoked = errors.New("workspace: attempt authority revoked")
	// ErrPublicationPending: an unsettled publication blocks the operation.
	ErrPublicationPending = errors.New("workspace: a publication is unsettled")
	// ErrProposalChanged: the attempt already made a different proposal.
	ErrProposalChanged = errors.New("workspace: proposal differs from the recorded publication")
	// ErrAdmissionClosed: archive closed source admission.
	ErrAdmissionClosed = errors.New("workspace: admission is closed by archive")
	// ErrPublicationConflict is wrapped by *ConflictError.
	ErrPublicationConflict = errors.New("workspace: publication lost the expected-tip race")
	// ErrBundleIncomplete: the artifact is not a complete bundle of the attempt.
	ErrBundleIncomplete = errors.New("workspace: bundle artifact is not complete")
	// ErrBundleInvalid: the bundle failed verification (see BundleError).
	ErrBundleInvalid = errors.New("workspace: bundle is invalid")
	// ErrLimitExceeded: a transfer, expansion, scratch or time bound was hit.
	ErrLimitExceeded = errors.New("workspace: resource limit exceeded")
	// ErrSecretInSource: an injected secret value appears in new objects.
	ErrSecretInSource = errors.New("workspace: injected secret found in source")
	// ErrFenced: publication authority was fenced by installation recovery.
	ErrFenced = errors.New("workspace: publisher is fenced")
	// ErrOutputNotSelectable: no successful writer published this commit.
	ErrOutputNotSelectable = errors.New("workspace: commit is not the output of a successful writer")
	// ErrArchiveUncertain: archive admission is closed but settlement is not
	// proven yet; the request is retained and reconciled later.
	ErrArchiveUncertain = errors.New("workspace: archive settlement is not complete")
	// ErrRemovalDenied: retention policy or a live reference forbids removal.
	ErrRemovalDenied = errors.New("workspace: removal denied")
	// ErrRemovalDeferred: removal must wait for settlement; retry later.
	ErrRemovalDeferred = errors.New("workspace: removal deferred")
	// ErrRetentionBusy: a concurrent pin/removal on the same commit; retry.
	ErrRetentionBusy = errors.New("workspace: retention is busy for this commit")
	// ErrUnavailable wraps a dependency failure; the operation may be retried.
	ErrUnavailable = errors.New("workspace: dependency unavailable")
	// ErrNotConfigured: a required adapter was not injected (fail closed).
	ErrNotConfigured = errors.New("workspace: required capability is not configured")
	// ErrInvalidConfiguration: a constructor option is missing or invalid.
	ErrInvalidConfiguration = errors.New("workspace: invalid configuration")
	// ErrBranchExists: the work branch exists but was not created by this
	// workspace; it is never overwritten.
	ErrBranchExists = errors.New("workspace: work branch already exists independently")
	// ErrDiverged: the remote branch is not a fast-forward of the accepted
	// commit; Kodama never merges or rewinds.
	ErrDiverged = errors.New("workspace: remote branch diverged from the accepted commit")
	// ErrInvalidValue is returned for malformed commits, refs or identifiers;
	// it is the shared domain sentinel so one errors.Is covers every value.
	ErrInvalidValue = domain.ErrInvalidValue
	// ErrStale: an observation raced with another change; retry.
	ErrStale = errors.New("workspace: concurrent change; retry")
)

// ConflictError is a lost expected-tip race. ObservedTip is the remote tip
// seen when the conflict was established ("" if the branch is missing).
type ConflictError struct {
	ObservedTip Commit
	Reason      string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%v (%s)", ErrPublicationConflict, e.Reason)
}
func (e *ConflictError) Unwrap() error { return ErrPublicationConflict }

// BundleError classifies a bundle rejection with a stable, safe reason code.
type BundleError struct {
	Reason string
	Cause  error
}

func (e *BundleError) Error() string { return fmt.Sprintf("%v: %s", ErrBundleInvalid, e.Reason) }
func (e *BundleError) Unwrap() error { return ErrBundleInvalid }

// Bundle rejection reasons.
const (
	ReasonTruncated        = "truncated"
	ReasonDigestMismatch   = "digest_mismatch"
	ReasonBadPrerequisite  = "bad_prerequisite"
	ReasonCommitMismatch   = "commit_mismatch"
	ReasonNotDescendant    = "not_descendant"
	ReasonCorruptObjects   = "corrupt_objects"
	ReasonUnsupportedBound = "unsupported_format"
)

// UnavailableError wraps a dependency failure without exposing its text.
func unavailable(op string, cause error) error {
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, op, cause)
}
