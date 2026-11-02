package eventsv1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Errors returned by decoding and validation. Callers classify with errors.Is:
// ErrUnsupportedVersion and ErrUnsupportedType mean "valid for a newer or
// different contract" and must never be treated as a successful no-op.
var (
	ErrMalformed          = errors.New("malformed event")
	ErrTooLarge           = errors.New("event too large")
	ErrInvalid            = errors.New("invalid event")
	ErrUnsupportedVersion = errors.New("unsupported event schema version")
	ErrUnsupportedType    = errors.New("unsupported event type")
)

// Patterns are the string syntaxes shared by the Go validators and
// schema.json; a test requires the schema to carry the same expressions.
var Patterns = map[string]string{
	"ID":        domain.IDPattern,
	"Digest":    `^sha256:[0-9a-f]{64}$`,
	"GitCommit": `^([0-9a-f]{40}|[0-9a-f]{64})$`,
	"Timestamp": `^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?Z$`,
	"Code":      `^[a-z][a-z0-9_]{0,63}$`,
	"FieldName": `^[A-Za-z][A-Za-z0-9_.-]{0,63}$`,
	// RunResourceKey: the fixed run-resource keys or service.NAME, NAME being
	// the pipeline service-name syntax (pipeline-format-v1alpha2).
	"RunResourceKey": `^(network|workspace|preparation|service\.[a-z][a-z0-9-]{0,29})$`,
	// The Git service names, shared with contracts/git/v1 (internal Git service
	// events): the repository name, a default branch and a full ref name. The
	// schema additionally forbids the ref sequences gitv1.Forbidden.
	"RepositoryName": gitv1.Patterns["RepositoryName"],
	"BranchName":     gitv1.Patterns["BranchName"],
	"RefName":        gitv1.Patterns["RefName"],
}

var compiled = func() map[string]*regexp.Regexp {
	m := make(map[string]*regexp.Regexp, len(Patterns))
	for k, v := range Patterns {
		m[k] = regexp.MustCompile(v)
	}
	return m
}()

// TypeSpec is the catalog's aggregate binding of one event type.
type TypeSpec struct {
	Type         string
	ResourceType string
	// ProjectRequired is false only for events that may be tenant-wide.
	ProjectRequired bool
	validate        func(json.RawMessage) error
}

var specs = map[string]TypeSpec{}

// envelopeChecks are optional per-type rules that bind the envelope to its
// payload (for example a derived resource ID); they run after the payload
// validates.
var envelopeChecks = map[string]func(Envelope) error{}

func register[T any, P interface {
	*T
	validate() error
}](eventType, resourceType string, projectRequired bool) {
	specs[eventType] = TypeSpec{Type: eventType, ResourceType: resourceType, ProjectRequired: projectRequired, validate: func(raw json.RawMessage) error {
		var v T
		if err := unmarshalExact(raw, &v); err != nil {
			return fmt.Errorf("%w: data: %v", ErrInvalid, err)
		}
		// Typed decoding cannot tell an omitted member from an explicit null or
		// false, which the schema distinguishes: check member presence on the
		// raw object (exact names, as in the schema) before the typed rules.
		var members map[string]json.RawMessage
		if err := json.Unmarshal(raw, &members); err != nil {
			return fmt.Errorf("%w: data: %v", ErrInvalid, err)
		}
		if err := rejectNullOptionals(members, reflect.TypeOf(v)); err != nil {
			return err
		}
		if mv, ok := any(&v).(memberValidator); ok {
			if err := mv.validateMembers(members); err != nil {
				return err
			}
		}
		return P(&v).validate()
	}}
}

// memberValidator is implemented by payloads whose schema constrains which
// members may be present together.
type memberValidator interface {
	validateMembers(members map[string]json.RawMessage) error
}

// rejectNullOptionals enforces the schema's rule for optional members: a
// member tagged omitempty may be absent, but when present it must have a value
// of its type, never null. Members that are nullable by contract (such as a
// checkpoint's previousCommit) carry no omitempty tag and are exempt.
func rejectNullOptionals(members map[string]json.RawMessage, t reflect.Type) error {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" || !strings.Contains(","+opts+",", ",omitempty,") {
			continue
		}
		if raw, present := members[name]; present && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return invalidf("%s must not be null (omit the member instead)", name)
		}
	}
	return nil
}

func init() {
	register[WorkItemCreated](TypeWorkItemCreated, "work_item", true)
	register[WorkItemUpdated](TypeWorkItemUpdated, "work_item", true)
	register[WorkItemTransitioned](TypeWorkItemTransitioned, "work_item", true)
	register[WorkspaceStateChanged](TypeWorkspaceStateChanged, "workspace", true)
	register[WorkspaceCheckpointCreated](TypeWorkspaceCheckpoint, "workspace", true)
	register[RunCreated](TypeRunCreated, "run", true)
	register[RunStateChanged](TypeRunStateChanged, "run", true)
	register[StepAttemptStateChanged](TypeStepAttemptStateChange, "step_attempt", true)
	register[GateRequested](TypeGateRequested, "gate", true)
	register[GateDecided](TypeGateDecided, "gate", true)
	register[GateInvalidated](TypeGateInvalidated, "gate", true)
	register[DeploymentStateChanged](TypeDeploymentStateChanged, "deployment", true)
	register[MembershipChanged](TypeMembershipChanged, "membership", false)
	register[ProjectSettingsChanged](TypeProjectSettingsChanged, "project", true)
	register[ConfigurationChanged](TypeConfigurationChanged, "configuration", false)
	register[UsageRecorded](TypeUsageRecorded, "usage_record", true)
}

// Types lists the supported event types in sorted order.
func Types() []string {
	out := make([]string, 0, len(specs))
	for t := range specs {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Spec returns the aggregate binding of a supported event type.
func Spec(eventType string) (TypeSpec, bool) {
	s, ok := specs[eventType]
	return s, ok
}

// Validate checks the envelope and its per-type payload. An unknown schema
// version or event type is reported first, as unsupported, because a newer
// contract may legitimately change everything else.
func (e Envelope) Validate() error {
	if e.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: %d (supported: %d)", ErrUnsupportedVersion, e.SchemaVersion, SchemaVersion)
	}
	spec, ok := specs[e.Type]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnsupportedType, truncate(e.Type, 64))
	}
	if err := matchID("id", e.ID); err != nil {
		return err
	}
	if e.OccurredAt.IsZero() || e.OccurredAt.Year() < 2000 || e.OccurredAt.Year() > 9999 {
		return invalidf("occurredAt out of range")
	}
	if err := e.TenantID.Validate(); err != nil {
		return invalidf("tenantId: %v", err)
	}
	if e.ProjectID == "" {
		if spec.ProjectRequired {
			return invalidf("projectId is required for %s", e.Type)
		}
	} else if err := e.ProjectID.Validate(); err != nil {
		return invalidf("projectId: %v", err)
	}
	if e.Resource.Type != spec.ResourceType {
		return invalidf("resource.type %q does not match %s (want %q)", truncate(e.Resource.Type, 64), e.Type, spec.ResourceType)
	}
	if err := matchID("resource.id", e.Resource.ID); err != nil {
		return err
	}
	if e.Resource.Version < 1 || e.Resource.Version > MaxSafeInteger {
		return invalidf("resource.version must be between 1 and 2^53-1")
	}
	if err := validateActor("actor", e.Actor); err != nil {
		return err
	}
	if err := validateFreeText("correlationId", e.CorrelationID, 256); err != nil {
		return err
	}
	if e.CausationID != nil {
		if err := matchID("causationId", *e.CausationID); err != nil {
			return err
		}
	}
	if len(e.Data) > MaxDataBytes {
		return fmt.Errorf("%w: data is %d bytes (limit %d)", ErrTooLarge, len(e.Data), MaxDataBytes)
	}
	trimmed := strings.TrimSpace(string(e.Data))
	if !strings.HasPrefix(trimmed, "{") {
		return invalidf("data must be an object")
	}
	if err := spec.validate(e.Data); err != nil {
		return err
	}
	if check := envelopeChecks[e.Type]; check != nil {
		return check(e)
	}
	return nil
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func matchID(field, v string) error     { return matchPattern(field, "ID", v) }
func matchCode(field, v string) error   { return matchPattern(field, "Code", v) }
func matchDigest(field, v string) error { return matchPattern(field, "Digest", v) }
func matchCommit(field, v string) error { return matchPattern(field, "GitCommit", v) }
func matchStamp(field, v string) error {
	if err := matchPattern(field, "Timestamp", v); err != nil {
		return err
	}
	if _, err := time.Parse(time.RFC3339Nano, v); err != nil {
		return invalidf("%s: %v", field, err)
	}
	return nil
}

func matchPattern(field, pattern, v string) error {
	if !compiled[pattern].MatchString(v) {
		return invalidf("%s must match %s", field, Patterns[pattern])
	}
	return nil
}

func validateFreeText(field, v string, max int) error {
	if n := len([]rune(v)); n < 1 || n > max {
		return invalidf("%s must be 1 to %d characters", field, max)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return invalidf("%s contains a control character", field)
		}
	}
	return nil
}

func validateActor(field string, a Actor) error {
	switch a.Type {
	case ActorUser, ActorService, ActorWorkload, ActorOperator:
	default:
		return invalidf("%s.type %q is not supported", field, truncate(string(a.Type), 32))
	}
	return validateFreeText(field+".id", a.ID, 256)
}

func validateOptional(field string, v *string, check func(string, string) error) error {
	if v == nil {
		return nil
	}
	return check(field, *v)
}

func validateRevision(field string, v int64) error {
	if v < 1 || v > MaxSafeInteger {
		return invalidf("%s must be between 1 and 2^53-1", field)
	}
	return nil
}

func validateNames(field string, names []string, max int) error {
	if len(names) < 1 || len(names) > max {
		return invalidf("%s must list 1 to %d entries", field, max)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if err := matchPattern(field, "FieldName", n); err != nil {
			return err
		}
		if seen[n] {
			return invalidf("%s repeats %q", field, n)
		}
		seen[n] = true
	}
	return nil
}

func oneOf(field, v string, allowed ...string) error {
	for _, a := range allowed {
		if v == a {
			return nil
		}
	}
	return invalidf("%s %q is not one of %s", field, truncate(v, 64), strings.Join(allowed, ", "))
}

// firstError returns the first non-nil error.
func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (v *WorkItemCreated) validate() error {
	return firstError(matchCode("kind", v.Kind), matchCode("state", v.State))
}

func (v *WorkItemUpdated) validate() error {
	if err := validateNames("changedFields", v.ChangedFields, 64); err != nil {
		return err
	}
	if v.ContentRevision != nil {
		return validateRevision("contentRevision", *v.ContentRevision)
	}
	return nil
}

func (v *WorkItemTransitioned) validate() error {
	return firstError(matchCode("from", v.From), matchCode("to", v.To), matchID("transitionId", v.TransitionID))
}

func (v *WorkspaceStateChanged) validate() error {
	return firstError(matchCode("from", v.From), matchCode("to", v.To), matchCode("reason", v.Reason))
}

func (v *WorkspaceCheckpointCreated) validate() error {
	if err := firstError(matchID("repository", v.Repository), validateOptional("previousCommit", v.PreviousCommit, matchCommit), matchCommit("newCommit", v.NewCommit),
		validateOptional("publishingAttemptId", v.PublishingAttemptID, matchID)); err != nil {
		return err
	}
	if (v.PublishingAttemptID != nil) == v.HumanSynchronization {
		return invalidf("exactly one of publishingAttemptId and humanSynchronization is required")
	}
	return nil
}

// validateMembers applies the schema's oneOf: a checkpoint names either the
// publishing attempt or, as the literal true, a human synchronization; never
// both members, and a false or null humanSynchronization is not an origin.
func (v *WorkspaceCheckpointCreated) validateMembers(members map[string]json.RawMessage) error {
	_, attempt := members["publishingAttemptId"]
	human, hasHuman := members["humanSynchronization"]
	if attempt == hasHuman {
		return invalidf("exactly one of publishingAttemptId and humanSynchronization is required")
	}
	if hasHuman && string(bytes.TrimSpace(human)) != "true" {
		return invalidf("humanSynchronization must be true when present")
	}
	return nil
}

func (v *RunCreated) validate() error {
	return firstError(matchID("workItemId", v.WorkItemID), matchID("definitionId", v.DefinitionID), matchDigest("inputSnapshotDigest", v.InputSnapshotDigest))
}

func (v *RunStateChanged) validate() error {
	return firstError(matchCode("from", v.From), matchCode("to", v.To), matchCode("reason", v.Reason))
}

func (v *StepAttemptStateChanged) validate() error {
	return firstError(matchID("runId", v.RunID), matchID("stepId", v.StepID), matchCode("from", v.From), matchCode("to", v.To), matchCode("reason", v.Reason))
}

func (v *GateRequested) validate() error {
	return firstError(matchDigest("subjectDigest", v.SubjectDigest), validateRevision("contentRevision", v.ContentRevision),
		matchCode("reviewerScope", v.ReviewerScope), matchStamp("deadline", v.Deadline))
}

func (v *GateDecided) validate() error {
	return firstError(validateActor("decidedBy", v.DecidedBy), oneOf("decision", v.Decision, "approved", "rejected"),
		matchCode("state", v.State), matchDigest("subjectDigest", v.SubjectDigest))
}

func (v *GateInvalidated) validate() error {
	if err := firstError(matchDigest("subjectDigest", v.SubjectDigest), matchCode("reason", v.Reason), validateOptional("supersedingCommit", v.SupersedingCommit, matchCommit)); err != nil {
		return err
	}
	if v.SupersedingRevision != nil {
		return validateRevision("supersedingRevision", *v.SupersedingRevision)
	}
	return nil
}

func (v *DeploymentStateChanged) validate() error {
	if err := firstError(matchID("environmentId", v.EnvironmentID), matchCode("from", v.From), matchCode("to", v.To),
		validateOptional("sourceCommit", v.SourceCommit, matchCommit), validateOptional("imageDigest", v.ImageDigest, matchDigest)); err != nil {
		return err
	}
	if len(v.ArtifactIDs) > 64 {
		return invalidf("artifactIds lists more than 64 entries")
	}
	for _, id := range v.ArtifactIDs {
		if err := matchID("artifactIds", id); err != nil {
			return err
		}
	}
	if v.SourceCommit == nil && v.ImageDigest == nil && len(v.ArtifactIDs) == 0 {
		return invalidf("a source commit, image digest or artifact reference is required")
	}
	return nil
}

func (v *MembershipChanged) validate() error {
	if err := matchID("userId", v.UserID); err != nil {
		return err
	}
	if len(v.BindingIDs) < 1 || len(v.BindingIDs) > 64 {
		return invalidf("bindingIds must list 1 to 64 entries")
	}
	for _, id := range v.BindingIDs {
		if err := matchID("bindingIds", id); err != nil {
			return err
		}
	}
	return validateRevision("effectiveVersion", v.EffectiveVersion)
}

func (v *ProjectSettingsChanged) validate() error {
	if err := validateNames("changedSettings", v.ChangedSettings, 64); err != nil {
		return err
	}
	return validateRevision("version", v.Version)
}

func (v *ConfigurationChanged) validate() error {
	return firstError(oneOf("kind", v.Kind, "variable", "secret"), validateRevision("version", v.Version),
		oneOf("change", v.Change, "created", "updated", "deleted", "rotated"), oneOf("scope", v.Scope, "tenant", "project", "environment"))
}

func (v *UsageRecorded) validate() error {
	if err := firstError(matchID("runId", v.RunID), matchID("attemptId", v.AttemptID), validateFreeText("model", v.Model, 128),
		oneOf("basis", v.Basis, "observed", "estimated"), matchCode("uncertainty", v.Uncertainty)); err != nil {
		return err
	}
	for _, n := range []*int64{v.InputTokens, v.OutputTokens} {
		if n != nil && (*n < 0 || *n > MaxSafeInteger) {
			return invalidf("token counts must be between 0 and 2^53-1")
		}
	}
	return nil
}
