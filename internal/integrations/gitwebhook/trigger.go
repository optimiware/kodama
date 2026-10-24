package gitwebhook

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/workspace"
)

// TriggerID identifies a trigger and addresses its hook endpoint. It is an
// address, never authority: the signature under the trigger's secret is.
type TriggerID string

var triggerIDRegexp = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// Validate checks the identifier syntax.
func (id TriggerID) Validate() error {
	if !triggerIDRegexp.MatchString(string(id)) {
		return invalidf("trigger id must match %s", triggerIDRegexp)
	}
	return nil
}

// Trigger binds one receive hook to a project, its configured repository, a
// registered pipeline definition, an explicit branch filter and the secret that
// authenticates the hook. Tenant and project come only from this record: a
// notification never names them.
type Trigger struct {
	ID    TriggerID
	Scope domain.Scope
	// RepositoryID is the project's configured repository at installation. It
	// must still be the project's repository on every delivery.
	RepositoryID workspace.RepositoryID
	DefinitionID domain.DefinitionID
	// WorkItemID is the optional work item whose workspace the run attaches
	// to; run admission decides whether it is actionable.
	WorkItemID domain.WorkItemID
	// Branches are exact branch names ("main") or one-level-wildcard prefixes
	// ("release/*"). A bare "*" is not accepted.
	Branches []string
	// SecretRef references the shared hook secret. It is never a Git credential.
	SecretRef string
	// InstalledBy is the principal whose current run authority every delivery
	// is checked against; the hook secret itself carries no user authority.
	InstalledBy string
	Enabled     bool
	Version     int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const (
	maxBranchPatterns = 16
	maxSecretRefLen   = 256
	maxPrincipalLen   = 256
)

// Validate checks the invariants every stored trigger satisfies.
func (t Trigger) Validate() error {
	if err := t.ID.Validate(); err != nil {
		return err
	}
	if err := t.Scope.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := t.RepositoryID.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := t.DefinitionID.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if t.WorkItemID != "" {
		if err := t.WorkItemID.Validate(); err != nil {
			return invalidf("%v", err)
		}
	}
	if err := ValidateBranchPatterns(t.Branches); err != nil {
		return err
	}
	if t.SecretRef == "" || len(t.SecretRef) > maxSecretRefLen {
		return invalidf("secret reference must be 1..%d bytes", maxSecretRefLen)
	}
	if t.InstalledBy == "" || len(t.InstalledBy) > maxPrincipalLen {
		return invalidf("installing principal must be 1..%d bytes", maxPrincipalLen)
	}
	return nil
}

// ValidateBranchPatterns requires a non-empty, bounded, duplicate-free list of
// safe branch names or "prefix/*" patterns.
func ValidateBranchPatterns(patterns []string) error {
	if len(patterns) == 0 || len(patterns) > maxBranchPatterns {
		return invalidf("between 1 and %d branch filters are required", maxBranchPatterns)
	}
	seen := map[string]bool{}
	for _, p := range patterns {
		if seen[p] {
			return invalidf("duplicate branch filter %q", p)
		}
		seen[p] = true
		name := strings.TrimSuffix(p, "/*")
		if strings.Contains(name, "*") || name == "" {
			return invalidf("branch filter %q must be a branch name or prefix/*", p)
		}
		if err := workspace.ValidateBranch("refs/heads/" + name); err != nil {
			return invalidf("branch filter %q is not a safe branch name", p)
		}
	}
	return nil
}

// MatchesRef reports whether the full branch reference passes the filter. A
// reference that is not a valid branch never matches.
func (t Trigger) MatchesRef(ref string) bool {
	if workspace.ValidateBranch(ref) != nil {
		return false
	}
	branch := strings.TrimPrefix(ref, "refs/heads/")
	for _, p := range t.Branches {
		if prefix, ok := strings.CutSuffix(p, "/*"); ok {
			if strings.HasPrefix(branch, prefix+"/") && len(branch) > len(prefix)+1 {
				return true
			}
			continue
		}
		if branch == p {
			return true
		}
	}
	return false
}

// withEnabled returns the trigger with a new enabled state; the store advances
// the version.
func (t Trigger) withEnabled(enabled bool, now time.Time) (Trigger, error) {
	if t.Enabled == enabled {
		return t, fmt.Errorf("%w: trigger is already in that state", ErrConflict)
	}
	t.Branches = append([]string(nil), t.Branches...)
	t.Enabled = enabled
	t.UpdatedAt = now
	return t, nil
}
