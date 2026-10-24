package observation

import (
	"encoding/json"
	"sort"
	"strings"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Family groups the observed resource kinds that share an access rule. The
// projection is an explicit allowlist: an event type that is not listed here is
// never forwarded, however it was retained.
type Family string

const (
	// FamilyWork covers work-item events and snapshot entries.
	FamilyWork Family = "work_item"
	// FamilyRun covers run events and snapshot entries.
	FamilyRun Family = "run"
	// FamilyStepAttempt covers step-attempt events and snapshot entries. A step
	// attempt belongs to the run named by its typed runId payload member.
	FamilyStepAttempt Family = "step_attempt"
)

// typeFamilies is the initial projection allowlist (docs/contracts/event-catalog.md).
var typeFamilies = map[string]Family{
	eventsv1.TypeWorkItemCreated:        FamilyWork,
	eventsv1.TypeWorkItemUpdated:        FamilyWork,
	eventsv1.TypeWorkItemTransitioned:   FamilyWork,
	eventsv1.TypeRunCreated:             FamilyRun,
	eventsv1.TypeRunStateChanged:        FamilyRun,
	eventsv1.TypeStepAttemptStateChange: FamilyStepAttempt,
}

// SupportedTypes lists the event types the streams can forward, sorted.
func SupportedTypes() []string {
	out := make([]string, 0, len(typeFamilies))
	for t := range typeFamilies {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// FamilyOfType returns the family of a supported event type.
func FamilyOfType(eventType string) (Family, bool) {
	f, ok := typeFamilies[eventType]
	return f, ok
}

// Scope is what a stream observes: one project of one tenant, optionally
// narrowed to one run, and optionally to a set of event types. The value is
// immutable once built; every public cursor is bound to it.
type Scope struct {
	Tenant  domain.TenantID
	Project domain.ProjectID
	// Run narrows the stream to one run and its step attempts. Empty observes
	// the project.
	Run domain.RunID
	// Types is the normalized (sorted, unique) event-type filter. Empty means
	// every supported type.
	Types []string
}

// NewScope validates and normalizes a scope. Unsupported event types are
// rejected rather than ignored, so a filter can never silently match nothing.
func NewScope(tenant domain.TenantID, project domain.ProjectID, run domain.RunID, types []string) (Scope, error) {
	if err := tenant.Validate(); err != nil {
		return Scope{}, invalid("%v", err)
	}
	if err := project.Validate(); err != nil {
		return Scope{}, invalid("%v", err)
	}
	if run != "" {
		if err := run.Validate(); err != nil {
			return Scope{}, invalid("%v", err)
		}
	}
	seen := map[string]bool{}
	var norm []string
	for _, t := range types {
		t = strings.TrimSpace(t)
		if _, ok := typeFamilies[t]; !ok {
			return Scope{}, invalid("event type %q is not observable", t)
		}
		if !seen[t] {
			seen[t] = true
			norm = append(norm, t)
		}
	}
	sort.Strings(norm)
	s := Scope{Tenant: tenant, Project: project, Run: run, Types: norm}
	if len(s.Families()) == 0 {
		return Scope{}, invalid("the event type filter selects nothing in this stream")
	}
	return s, nil
}

// Validate re-checks a scope built outside NewScope.
func (s Scope) Validate() error {
	_, err := NewScope(s.Tenant, s.Project, s.Run, s.Types)
	return err
}

func (s Scope) typeSelected(t string) bool {
	if len(s.Types) == 0 {
		return true
	}
	i := sort.SearchStrings(s.Types, t)
	return i < len(s.Types) && s.Types[i] == t
}

// Families lists the families this stream can carry, in a stable order: all
// three for a project stream, run and step attempts for a run stream, further
// narrowed by the type filter.
func (s Scope) Families() []Family {
	var out []Family
	for _, f := range []Family{FamilyWork, FamilyRun, FamilyStepAttempt} {
		if s.Run != "" && f == FamilyWork {
			continue
		}
		for t, tf := range typeFamilies {
			if tf == f && s.typeSelected(t) {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

// IncludesFamily reports whether resources of f belong to this stream.
func (s Scope) IncludesFamily(f Family) bool {
	for _, x := range s.Families() {
		if x == f {
			return true
		}
	}
	return false
}

// Match decides, from the committed envelope alone, whether an event belongs to
// this stream and which family's access rule applies. It never grants access:
// the caller still authorizes the family. Anything unexpected is excluded.
//
// Run events are matched by the run's own resource ID. Step-attempt events are
// matched by the typed runId payload member, never by a correlation ID.
func (s Scope) Match(env eventsv1.Envelope) (Family, bool) {
	fam, ok := typeFamilies[env.Type]
	if !ok || !s.typeSelected(env.Type) {
		return "", false
	}
	if env.TenantID != s.Tenant || env.ProjectID != s.Project {
		return "", false
	}
	if s.Run == "" {
		return fam, true
	}
	switch fam {
	case FamilyRun:
		if env.Resource.Type == "run" && env.Resource.ID == string(s.Run) {
			return fam, true
		}
	case FamilyStepAttempt:
		var d eventsv1.StepAttemptStateChanged
		if err := json.Unmarshal(env.Data, &d); err != nil {
			return "", false
		}
		if d.RunID == string(s.Run) {
			return fam, true
		}
	}
	return "", false
}

// Filters returns the normalized filter members a cursor is bound to.
func (s Scope) Filters() map[string][]string {
	if len(s.Types) == 0 {
		return map[string][]string{}
	}
	return map[string][]string{"type": append([]string(nil), s.Types...)}
}
