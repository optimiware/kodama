package gitv1

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Timestamp is a UTC RFC 3339 time with a literal "Z" suffix (the runtime v1
// type).
type Timestamp = runtimev1.Timestamp

// ValidationError reports the first structural or semantic problem found.
type ValidationError = runtimev1.ValidationError

// Refname building blocks shared by the patterns below. A component starts with
// a letter, digit or underscore (never "." or "-", so no component can be read
// as an option or a hidden name) and continues with letters, digits and
// "._-". The syntax is a deliberately conservative subset of git-check-ref-format:
// anything outside it is refused explicitly by the hooks, never rewritten.
const (
	refComponent = `[A-Za-z0-9_][A-Za-z0-9._-]*`
	defComponent = `[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*`
	// refForbidden is the set of sequences the character-class syntax still lets
	// through but git-check-ref-format forbids: "..", a component ending in ".lock"
	// and a component ending in ".".
	refForbidden = `\.\.|\.lock(/|$)|\.(/|$)`
)

// Patterns are the string syntaxes shared by the Go validators and schema.json.
// A test requires the schema's patterns to equal these.
var Patterns = map[string]string{
	"ID":             domain.IDPattern,
	"Timestamp":      `^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?Z$`,
	"ObjectID":       `^([0-9a-f]{40}|[0-9a-f]{64})$`,
	"Code":           `^[a-z][a-z0-9_]{0,63}$`,
	"SHA256":         `^[0-9a-f]{64}$`,
	"RepositoryName": `^[a-z][a-z0-9-]{0,62}$`,
	"RepositoryURL":  `^https://[^\s@/?#]+(/[^\s@?#]*)?$`,
	"BranchName":     `^` + refComponent + `(/` + refComponent + `)*$`,
	"RefName":        `^refs/` + refComponent + `(/` + refComponent + `)+$`,
	"RefPattern":     `^refs/(heads|tags)/` + refComponent + `(/` + refComponent + `)*(/\*)?$`,
	"BranchRef":      `^refs/heads/` + refComponent + `(/` + refComponent + `)*$`,
	// DefinitionPath is a pipeline definition: a .yaml or .yml file under
	// .kodama/ whose components are letters, digits, "_" and "-" joined by single
	// dots, so no component is empty, hidden, "." or "..".
	"DefinitionPath": `^\.kodama/` + defComponent + `(/` + defComponent + `)*\.ya?ml$`,
	"SourcePath":     `^(\.|` + refComponent + `(/` + refComponent + `)*)$`,
	"Label":          `^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`,
	"Username":       `^[A-Za-z0-9._-]{1,64}$`,
	"Credential":     `^[A-Za-z0-9._~+/=-]{16,1000}$`,
	"Cursor":         `^[A-Za-z0-9._~+/=-]{1,512}$`,
	"WorkerAudience": `^kodama-git-worker:[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`,
	"PushOption":     `^[^\x00-\x1f\x7f]*$`,
}

// Forbidden is the pattern the schema applies (negated) to RefName, RefPattern,
// BranchRef, BranchName and SourcePath in addition to their own pattern.
const Forbidden = refForbidden

var compiled = func() map[string]*regexp.Regexp {
	m := make(map[string]*regexp.Regexp, len(Patterns)+1)
	for name, p := range Patterns {
		m[name] = regexp.MustCompile(p)
	}
	m["forbidden"] = regexp.MustCompile(refForbidden)
	return m
}()

// Bounds. Where the specification is silent these are engineering bounds chosen
// here and frozen with the contract.
const (
	// MaxRefNameBytes bounds a full ref name; MaxBranchNameBytes a default branch.
	MaxRefNameBytes    = 255
	MaxBranchNameBytes = 200
	// MaxPushUpdates is the contract ceiling on the ref updates of one receive
	// (GR-16). An installation may configure a lower bound; a receive above it is
	// refused explicitly with too_many_updates (CheckUpdateBound), and a larger
	// push is sent in batches of refs. It does not depend on event size: the
	// git.refs_updated event carries a count, not the updates.
	MaxPushUpdates = 1024
	// MaxRefValues bounds one exact-ref read (ReadRefValuesRequest); a
	// reconciler batches larger records.
	MaxRefValues       = 64
	MaxPushOptions     = 16
	MaxPushOptionBytes = 1024
	// MaxPathBytes and MaxSources bound a source set.
	MaxPathBytes = 255
	MaxSources   = 32
	// MaxGrantRepositories and MaxGrantPatterns bound one grant.
	MaxGrantRepositories = 64
	MaxGrantPatterns     = 32
	MaxTriggerPatterns   = 32
	// MaxPage bounds list, ref and journal pages; MaxAssignments one assignment poll.
	MaxPage        = 1000
	MaxAssignments = 256
	// MaxSafeInteger is the largest integer carried anywhere in the contract.
	MaxSafeInteger = domain.MaxSafeInteger
)

// Lifetimes (grants and assertions are short-lived).
const (
	MaxForwardingLifetime      = 5 * time.Minute
	MaxWorkerIdentityLifetime  = time.Hour
	MaxSeedLifetime            = 15 * time.Minute
	MaxHumanGrantLifetime      = time.Hour
	MaxWorkloadGrantLifetime   = 24 * time.Hour
	MaxReceiveDeadlineLifetime = time.Hour
)

// ZeroObjectID returns the all-zero object ID of the given length (40 or 64).
func ZeroObjectID(length int) string { return strings.Repeat("0", length) }

// IsZeroObjectID reports whether id is an all-zero object ID: a missing ref
// (create old value, delete new value, or an absent observed value).
func IsZeroObjectID(id string) bool {
	return (len(id) == 40 || len(id) == 64) && strings.Trim(id, "0") == ""
}

func fail(path, format string, args ...any) error {
	return &ValidationError{Path: path, Message: fmt.Sprintf(format, args...)}
}

func matches(path, pattern, value string) error {
	if !compiled[pattern].MatchString(value) {
		return fail(path, "must match %s syntax", pattern)
	}
	return nil
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func wrap(path string, err error) error {
	if err == nil {
		return nil
	}
	return fail(path, "%v", err)
}

func inRange(path string, v, lo, hi int64) error {
	if v < lo || v > hi {
		return fail(path, "must be between %d and %d", lo, hi)
	}
	return nil
}

func oneOf(path, value string, allowed ...string) error {
	for _, a := range allowed {
		if value == a {
			return nil
		}
	}
	return fail(path, "%q is not one of %s", truncate(value, 64), strings.Join(allowed, ", "))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func checkID(path, v string) error { return matches(path, "ID", v) }

func checkOptionalID(path string, v *string) error {
	if v == nil {
		return nil
	}
	return checkID(path, *v)
}

func checkObjectID(path, v string) error { return matches(path, "ObjectID", v) }

func checkCode(path, v string) error { return matches(path, "Code", v) }

func checkTimestamp(path string, t Timestamp) error { return wrap(path, t.Validate()) }

// ValidateObjectID checks the syntax of a Git object ID (SHA-1 or SHA-256,
// lowercase hexadecimal; the all-zero value is a syntactically valid ID).
func ValidateObjectID(id string) error { return checkObjectID("objectId", id) }

// ValidateRefName checks a full ref name: refs/ and at least two components in
// the conservative syntax, no "..", no component ending in ".lock" or ".", at
// most MaxRefNameBytes.
func ValidateRefName(ref string) error { return checkRef("ref", "RefName", ref, MaxRefNameBytes) }

// ValidateBranchRef checks a full branch ref name (refs/heads/...).
func ValidateBranchRef(ref string) error { return checkRef("ref", "BranchRef", ref, MaxRefNameBytes) }

// ValidateBranchName checks a branch name that is not a ref path: the same
// component syntax, not "HEAD", not beginning with "refs/", and outside the
// reserved kodama namespace ("kodama" or "kodama/...") that the server owns.
func ValidateBranchName(name string) error {
	if err := checkRef("defaultBranch", "BranchName", name, MaxBranchNameBytes); err != nil {
		return err
	}
	if name == "HEAD" || strings.HasPrefix(name, "refs/") || name == "kodama" || strings.HasPrefix(name, "kodama/") {
		return fail("defaultBranch", "%q is reserved or a ref path", truncate(name, 64))
	}
	return nil
}

func checkRef(path, pattern, v string, max int) error {
	if len(v) > max {
		return fail(path, "longer than %d bytes", max)
	}
	if err := matches(path, pattern, v); err != nil {
		return err
	}
	if compiled["forbidden"].MatchString(v) {
		return fail(path, "is not a valid Git ref name")
	}
	return nil
}

// ValidateRefPattern checks a grant or trigger ref pattern: a full ref name under
// refs/heads/ or refs/tags/ with an optional trailing "/*" that matches one or
// more further characters. At least one component precedes the wildcard, so
// "refs/heads/*" is not a pattern, and refs/kodama/* can never be named.
func ValidateRefPattern(pattern string) error {
	if err := checkRef("refPattern", "RefPattern", pattern, MaxRefNameBytes+2); err != nil {
		return err
	}
	return nil
}

// MatchRefPattern reports whether ref is selected by pattern: equal to it, or,
// for a pattern ending in "/*", beginning with its prefix and at least one more
// character. Both arguments must be valid; an invalid argument never matches.
func MatchRefPattern(pattern, ref string) bool {
	if ValidateRefPattern(pattern) != nil || ValidateRefName(ref) != nil {
		return false
	}
	if prefix, wild := strings.CutSuffix(pattern, "*"); wild {
		return strings.HasPrefix(ref, prefix) && len(ref) > len(prefix)
	}
	return ref == pattern
}

func checkRefPatterns(path string, patterns []string, max int) error {
	if patterns == nil {
		return fail(path, "must be a list")
	}
	if len(patterns) > max {
		return fail(path, "lists more than %d patterns", max)
	}
	seen := map[string]bool{}
	for i, p := range patterns {
		if err := ValidateRefPattern(p); err != nil {
			return fail(fmt.Sprintf("%s[%d]", path, i), "%v", err)
		}
		if seen[p] {
			return fail(fmt.Sprintf("%s[%d]", path, i), "repeats %q", truncate(p, 64))
		}
		seen[p] = true
	}
	return nil
}
