// Package work implements Kodama's work-management model: work items, their
// immutable history, explicit business transitions, human completion and the
// single active work-in-progress limit (docs/specs/work-management.md).
//
// The package holds entities, value objects, use cases and small consumer-owned
// ports. It imports neither pgx, net/http nor any broker or Git client; the
// PostgreSQL and HTTP adapters live in the postgres and http sub-packages and
// are wired by the composition root.
package work

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/optimiware/kodama/internal/domain"
)

// Kind is the immutable type of a work item.
type Kind string

const (
	KindEpic   Kind = "epic"
	KindStory  Kind = "story"
	KindTask   Kind = "task"
	KindDefect Kind = "defect"
)

// Valid reports whether k is a supported kind.
func (k Kind) Valid() bool {
	switch k {
	case KindEpic, KindStory, KindTask, KindDefect:
		return true
	}
	return false
}

// Actionable reports whether items of this kind can acquire a workspace and
// produce deliverables. An epic only groups children and has no workspace.
func (k Kind) Actionable() bool { return k.Valid() && k != KindEpic }

// State is the business state of a work item.
type State string

const (
	StateBacklog   State = "backlog"
	StateReady     State = "ready"
	StateActive    State = "active"
	StateReview    State = "review"
	StateDone      State = "done"
	StateCancelled State = "cancelled"
)

// Valid reports whether s is a supported state.
func (s State) Valid() bool {
	switch s {
	case StateBacklog, StateReady, StateActive, StateReview, StateDone, StateCancelled:
		return true
	}
	return false
}

// Unfinished reports whether the item may still be cancelled.
func (s State) Unfinished() bool {
	switch s {
	case StateBacklog, StateReady, StateActive, StateReview:
		return true
	}
	return false
}

// Bounds of editable content. They keep history rows and events bounded.
const (
	MaxTitleChars       = 200
	MaxSpecBytes        = 64 << 10
	MaxCriteria         = 100
	MaxCriterionChars   = 2000
	MaxReasonChars      = 2048
	MaxEvidenceLinks    = 50
	MaxEvidenceNote     = 500
	MaxFindings         = 50
	MaxFindingSummary   = 500
	MaxChangedFieldName = 64
)

var (
	criterionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	commitPattern      = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	findingIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// Criterion is one structured acceptance criterion. IDs are unique within an
// item so acceptance can name each criterion.
type Criterion struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// EvidenceType names the kind of an evidence reference.
type EvidenceType string

const (
	// EvidenceArtifact references a retained artifact by its ID.
	EvidenceArtifact EvidenceType = "artifact"
	// EvidenceCommit references a source commit (40 or 64 lowercase hex digits).
	EvidenceCommit EvidenceType = "commit"
)

// EvidenceRef is a bounded reference to existing evidence. It never carries
// content: artifacts and commits are owned by other modules.
type EvidenceRef struct {
	Type EvidenceType `json:"type"`
	ID   string       `json:"id"`
	Note string       `json:"note,omitempty"`
}

// Validate checks the reference's syntax. Ownership is verified separately by
// the evidence guard, inside the mutating transaction.
func (e EvidenceRef) Validate() error {
	switch e.Type {
	case EvidenceArtifact:
		if err := domain.ArtifactID(e.ID).Validate(); err != nil {
			return invalid("evidence artifact id: %v", err)
		}
	case EvidenceCommit:
		if !commitPattern.MatchString(e.ID) {
			return invalid("evidence commit must be 40 or 64 lowercase hex digits")
		}
	default:
		return invalid("evidence type must be artifact or commit")
	}
	return validateText("evidence note", e.Note, MaxEvidenceNote, true)
}

// Finding is a business finding named by its identifier, as acknowledged at
// completion. Findings are not resources of their own.
type Finding struct {
	ID      string `json:"id"`
	Summary string `json:"summary,omitempty"`
}

// Validate checks the finding's syntax.
func (f Finding) Validate() error {
	if !findingIDPattern.MatchString(f.ID) {
		return invalid("finding id must match %s", findingIDPattern)
	}
	return validateText("finding summary", f.Summary, MaxFindingSummary, true)
}

// ActorType classifies who performed a mutation. The type comes from the
// injected authority, never from request JSON or the shape of an identifier.
type ActorType string

const (
	ActorUser     ActorType = "user"
	ActorService  ActorType = "service"
	ActorWorkload ActorType = "workload"
	ActorOperator ActorType = "operator"
)

// Actor is the verified identity behind a mutation.
type Actor struct {
	ID   string    `json:"id"`
	Type ActorType `json:"type"`
}

// Validate checks that the actor is attributable.
func (a Actor) Validate() error {
	switch a.Type {
	case ActorUser, ActorService, ActorWorkload, ActorOperator:
	default:
		return invalid("unknown actor type %q", string(a.Type))
	}
	if a.ID == "" || utf8.RuneCountInString(a.ID) > 256 || strings.ContainsFunc(a.ID, isControl) {
		return invalid("actor id must be 1 to 256 characters without control characters")
	}
	return nil
}

// IsHuman reports whether the actor is a person. Completion is a human-only
// decision.
func (a Actor) IsHuman() bool { return a.Type == ActorUser }

// Permission names an action the injected authority grants. The fixed role to
// permission mapping belongs to the identity module.
type Permission string

const (
	PermRead          Permission = "work_item:read"
	PermCreate        Permission = "work_item:create"
	PermEditSpec      Permission = "work_item:edit_spec"      // title, specification, acceptance, parent, evidence links
	PermEditTechnical Permission = "work_item:edit_technical" // assignee, blocked reason
	PermRefine        Permission = "work_item:refine"         // backlog <-> ready
	PermWork          Permission = "work_item:work"           // ready/active/review movement
	PermComplete      Permission = "work_item:complete"       // review -> done (human only)
	PermReopen        Permission = "work_item:reopen"         // done -> active
	PermCancel        Permission = "work_item:cancel"
)

var opaqueIDPattern = regexp.MustCompile(domain.IDPattern)

func validateOpaqueID(name, v string) error {
	if !opaqueIDPattern.MatchString(v) {
		return invalid("%s must match %s", name, domain.IDPattern)
	}
	return nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// validateText bounds a text value and rejects control characters other than
// newline and tab.
func validateText(name, v string, maxChars int, allowEmpty bool) error {
	if v == "" {
		if allowEmpty {
			return nil
		}
		return invalid("%s must not be empty", name)
	}
	if !utf8.ValidString(v) {
		return invalid("%s must be valid UTF-8", name)
	}
	if utf8.RuneCountInString(v) > maxChars {
		return invalid("%s must be at most %d characters", name, maxChars)
	}
	for _, r := range v {
		if isControl(r) && r != '\n' && r != '\t' {
			return invalid("%s contains a control character", name)
		}
	}
	if strings.TrimSpace(v) == "" && !allowEmpty {
		return invalid("%s must not be blank", name)
	}
	return nil
}

func validateCriteria(cs []Criterion) error {
	if len(cs) > MaxCriteria {
		return invalid("at most %d acceptance criteria are allowed", MaxCriteria)
	}
	seen := make(map[string]struct{}, len(cs))
	for _, c := range cs {
		if !criterionIDPattern.MatchString(c.ID) {
			return invalid("criterion id must match %s", criterionIDPattern)
		}
		if _, dup := seen[c.ID]; dup {
			return invalid("criterion id %q is repeated", c.ID)
		}
		seen[c.ID] = struct{}{}
		if err := validateText("criterion text", c.Text, MaxCriterionChars, false); err != nil {
			return err
		}
	}
	return nil
}

func validateEvidenceLinks(links []EvidenceRef) error {
	if len(links) > MaxEvidenceLinks {
		return invalid("at most %d evidence links are allowed", MaxEvidenceLinks)
	}
	seen := make(map[EvidenceRef]struct{}, len(links))
	for _, l := range links {
		if err := l.Validate(); err != nil {
			return err
		}
		key := EvidenceRef{Type: l.Type, ID: l.ID}
		if _, dup := seen[key]; dup {
			return invalid("evidence %s %q is repeated", l.Type, l.ID)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateReason(reason string) error {
	return validateText("reason", reason, MaxReasonChars, true)
}

// IsCommit reports whether s is a syntactically valid Git commit identifier.
func IsCommit(s string) bool { return commitPattern.MatchString(s) }

func (k Kind) String() string  { return string(k) }
func (s State) String() string { return string(s) }

func (c Criterion) String() string { return fmt.Sprintf("%s: %s", c.ID, c.Text) }
