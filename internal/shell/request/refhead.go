package request

import (
	"context"
	"fmt"

	"github.com/optimiware/kodema/internal/kernel/definition"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

const (
	// gitRefUpdateOperation is the canonical resource operation name for a
	// protected Git ref update approval subject.
	gitRefUpdateOperation = "git.ref.update"

	gitZeroOIDSHA1   = "0000000000000000000000000000000000000000"
	gitZeroOIDSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
)

// RefHead is the latest verified ref state projected from committed
// resource.verified / git.ref.updated events for one (scope, resource, ref).
type RefHead struct {
	// Commit is the latest verified commit OID on the ref. It is empty when
	// Found is false or when Deleted is true.
	Commit string
	// Deleted reports that the latest verified event deleted the ref (or set
	// its candidate to an empty/all-zero OID).
	Deleted bool
	// Found reports whether at least one committed verified git.ref.updated
	// event exists for the (scope, resource, ref) tuple.
	Found bool
}

// RefHeadReader reads the latest verified ref head for one (scope, resource,
// ref) strictly from committed verified git.ref.updated events in the Shell's
// event store. It never calls Git synchronously.
type RefHeadReader interface {
	LatestRefHead(ctx context.Context, scope tenancy.Scope, resourceID, ref string) (RefHead, error)
}

// GitRefUpdateSubject holds the canonical resolved literal fields of one
// git.ref.update approval subject.
type GitRefUpdateSubject struct {
	Resource     string
	Ref          string
	ExpectedBase string
	Candidate    string
}

// IsUnbornGitOID reports whether value represents an absent/unborn Git ref
// (empty string, 40-zero SHA-1 null OID, or 64-zero SHA-256 null OID).
func IsUnbornGitOID(value string) bool {
	return value == "" || value == gitZeroOIDSHA1 || value == gitZeroOIDSHA256
}

// ExtractGitRefUpdateSubject extracts the canonical git.ref.update fields from
// a resolved request subject. It returns ok=true only when operation is
// "git.ref.update" and resource, ref, and expected_base are present as
// resolved literals (expected_base may be empty for an unborn ref).
func ExtractGitRefUpdateSubject(subject *definition.Subject) (GitRefUpdateSubject, bool) {
	if subject == nil {
		return GitRefUpdateSubject{}, false
	}
	fields := subject.Fields()
	op, ok := literalField(fields, "operation")
	if !ok || op != gitRefUpdateOperation {
		return GitRefUpdateSubject{}, false
	}
	resource, ok := literalField(fields, "resource")
	if !ok || resource == "" {
		return GitRefUpdateSubject{}, false
	}
	ref, ok := literalField(fields, "ref")
	if !ok || ref == "" {
		return GitRefUpdateSubject{}, false
	}
	expectedBase, ok := literalField(fields, "expected_base")
	if !ok {
		return GitRefUpdateSubject{}, false
	}
	candidate, _ := literalField(fields, "candidate")
	return GitRefUpdateSubject{
		Resource:     resource,
		Ref:          ref,
		ExpectedBase: expectedBase,
		Candidate:    candidate,
	}, true
}

func literalField(fields map[string]definition.SubjectValue, name string) (string, bool) {
	value, present := fields[name]
	if !present || value.Kind != definition.SubjectLiteral {
		return "", false
	}
	return value.Literal, true
}

// EvaluateGitRefStaleness compares a git.ref.update request's expected_base
// against the latest verified ref head from committed git.ref.updated events.
//
// When no verified event exists for (resource, ref), the request is not stale.
// When the latest verified event deleted the ref, an unborn expected_base is
// not stale, while a non-unborn expected_base is stale. When a verified commit
// exists and differs from expected_base (including when expected_base is
// unborn), the request is stale and returns the current verified commit and an
// actionable diagnostic reason.
func EvaluateGitRefStaleness(target GitRefUpdateSubject, head RefHead) (stale bool, currentCommit string, reason string) {
	if !head.Found {
		return false, "", ""
	}
	if head.Deleted || IsUnbornGitOID(head.Commit) {
		if IsUnbornGitOID(target.ExpectedBase) {
			return false, "", ""
		}
		return true, "", fmt.Sprintf("ref %s on %s was deleted (expected_base %s)", target.Ref, target.Resource, target.ExpectedBase)
	}
	if target.ExpectedBase == head.Commit {
		return false, "", ""
	}
	expectedDisplay := target.ExpectedBase
	if IsUnbornGitOID(expectedDisplay) {
		expectedDisplay = "<unborn>"
	}
	return true, head.Commit, fmt.Sprintf("ref %s on %s moved from expected_base %s to %s", target.Ref, target.Resource, expectedDisplay, head.Commit)
}
