// Package services declares the independent Userland service adapters and the
// merge coordinator port.
//
// Email, webhook, timer, responder and merge-coordinator adapters call public
// APIs with stable keys. They never access the factory database and never submit
// commands over NATS. The declarations start no adapter.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
)

// ServiceKind is one independent service adapter kind.
type ServiceKind string

const (
	// KindWebhook answers its explicit request assignments. It delivers no
	// outbound webhook and opens no listener.
	KindWebhook ServiceKind = "webhook"
	// KindEmail answers its explicit request assignments and carries the
	// email-to-task intake. It sends no email.
	KindEmail ServiceKind = "email"
	// KindTimer schedules durable timers.
	KindTimer ServiceKind = "timer"
	// KindResponder answers explicit service-ID assignments.
	KindResponder ServiceKind = "responder"
	// KindMergeCoordinator serializes merges per repository/ref.
	KindMergeCoordinator ServiceKind = "merge_coordinator"
)

// serviceKinds is the declaration order.
var serviceKinds = []ServiceKind{
	KindWebhook,
	KindEmail,
	KindTimer,
	KindResponder,
	KindMergeCoordinator,
}

// Valid reports whether the kind is one of the declared kinds.
func (kind ServiceKind) Valid() bool {
	for _, candidate := range serviceKinds {
		if candidate == kind {
			return true
		}
	}
	return false
}

// ServiceAdapter is one independent service adapter.
type ServiceAdapter interface {
	Kind() ServiceKind
	Run(ctx context.Context) error
}

// EffectState is the observed state of one external effect.
type EffectState string

const (
	// EffectNotStarted proves the effect never started.
	EffectNotStarted EffectState = "not_started"
	// EffectApplied proves the effect settled.
	EffectApplied EffectState = "applied"
	// EffectUncertain means the effect outcome is unknown.
	EffectUncertain EffectState = "uncertain"
)

// effectStates is the declaration order.
var effectStates = []EffectState{EffectNotStarted, EffectApplied, EffectUncertain}

// Valid reports whether the state is one of the declared states.
func (state EffectState) Valid() bool {
	for _, candidate := range effectStates {
		if candidate == state {
			return true
		}
	}
	return false
}

// Releasable reports whether the effect may release its claim. An uncertain
// effect must be reconciled first; lease expiry alone is never evidence.
func (state EffectState) Releasable() bool {
	return state == EffectNotStarted || state == EffectApplied
}

// MergeKey identifies one serialized repository/ref.
type MergeKey struct {
	Repository string `json:"repository"`
	Ref        string `json:"ref"`
}

// Validate checks the tenant-relative repository and the anchored ref.
func (key MergeKey) Validate() error {
	if !validRepository(key.Repository) {
		return fmt.Errorf("merge repository %q is invalid", key.Repository)
	}
	if !validMergeRef(key.Ref) {
		return fmt.Errorf("merge ref %q is invalid", key.Ref)
	}
	return nil
}

// validRepository reports whether value is a tenant-relative repository name.
func validRepository(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	if strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") {
		return false
	}
	if strings.ContainsAny(value, "*?[]\\ ") || strings.Contains(value, "..") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" {
			return false
		}
		for index := 0; index < len(segment); index++ {
			character := segment[index]
			switch {
			case character >= 'a' && character <= 'z':
			case character >= '0' && character <= '9':
			case character == '-', character == '_', character == '.':
			default:
				return false
			}
		}
	}
	return true
}

// validMergeRef reports whether value is an anchored, non-glob, concrete ref.
func validMergeRef(value string) bool {
	if value == "" || !strings.HasPrefix(value, "refs/") {
		return false
	}
	if strings.HasSuffix(value, "/") {
		return false
	}
	if strings.ContainsAny(value, "*?[]\\ ") || strings.Contains(value, "..") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" {
			return false
		}
	}
	return true
}

// Claim is one durable merge-coordination claim. Its owner is an identity, never
// a credential. Generation starts at 1 and increases on every successful claim
// of the same key. OperationID is the durable merge-operation identity that
// effects are idempotent under; a takeover of an unsettled intent reuses it.
type Claim struct {
	Key         MergeKey       `json:"key"`
	Owner       string         `json:"owner"`
	OperationID id.OperationID `json:"operation_id"`
	Generation  uint64         `json:"generation"`
	ExpiresAt   time.Time      `json:"expires_at"`
}

// Validate checks the key, owner, operation identity, generation and expiry.
func (claim Claim) Validate() error {
	if err := claim.Key.Validate(); err != nil {
		return err
	}
	if claim.Owner == "" {
		return fmt.Errorf("claim owner is empty")
	}
	if _, err := id.ParseOperationID(string(claim.OperationID)); err != nil {
		return fmt.Errorf("claim operation id %q is invalid: %w", claim.OperationID, err)
	}
	if claim.Generation == 0 {
		return fmt.Errorf("claim generation is zero")
	}
	if claim.ExpiresAt.IsZero() {
		return fmt.Errorf("claim expiry is zero")
	}
	return nil
}

// Expired reports whether the claim lease has elapsed at now.
func (claim Claim) Expired(now time.Time) bool {
	return !now.Before(claim.ExpiresAt)
}

// MergeCoordinator serializes merges and records effect evidence on release.
type MergeCoordinator interface {
	Claim(ctx context.Context, key MergeKey) (Claim, error)
	Release(ctx context.Context, claim Claim, evidence EffectState) error
}

// PendingMerge is one durable, unresolved merge assignment. State is the
// persisted effect state the assignment was last observed in.
type PendingMerge struct {
	Request MergeRequest
	State   EffectState
}

// MergeRunner is the consumer-owned merge dependency of the merge-coordinator
// adapter. Merge performs at most one effect per exact assignment and keeps the
// intent and result durable until Settle reports that the request resolution
// converged. Pending lists the assignments that still need settlement.
type MergeRunner interface {
	Merge(context.Context, MergeRequest) (MergeResult, error)
	Settle(context.Context, MergeRequest, EffectState) error
	Pending(context.Context) ([]PendingMerge, error)
}

// MergeResult reports only observed merge state; it never carries review
// authority or an approval.
type MergeResult struct {
	Key               MergeKey    `json:"key"`
	State             EffectState `json:"state"`
	ObservedBase      string      `json:"observed_base,omitempty"`
	ObservedCandidate string      `json:"observed_candidate,omitempty"`
	ObservedOutput    string      `json:"observed_output,omitempty"`
}

// Validate checks the key, state and state-consistent observations. An
// uncertain result carries exactly what was observed, which may be nothing:
// an unknown outcome is never fabricated.
func (result MergeResult) Validate() error {
	if err := result.Key.Validate(); err != nil {
		return err
	}
	if !result.State.Valid() {
		return fmt.Errorf("merge result state %q is unknown", result.State)
	}
	switch result.State {
	case EffectApplied:
		if result.ObservedBase == "" || result.ObservedCandidate == "" {
			return fmt.Errorf("applied merge result needs an observed base and candidate")
		}
	case EffectNotStarted:
		if result.ObservedBase != "" || result.ObservedCandidate != "" || result.ObservedOutput != "" {
			return fmt.Errorf("not_started merge result must not report observations")
		}
	case EffectUncertain:
		// Any combination of observations, including none, is truthful.
	}
	return nil
}

// Sentinel refusals. Every implementation returns them wrapped so callers can
// use errors.Is without depending on a message.
var (
	// ErrClaimed reports a live claim owned by another coordinator.
	ErrClaimed = errors.New("merge key is already claimed")
	// ErrStaleClaim reports a claim that no longer matches the durable record.
	ErrStaleClaim = errors.New("merge claim is stale")
	// ErrNoClaim reports a missing durable claim or operation intent.
	ErrNoClaim = errors.New("merge claim does not exist")
	// ErrOperationConflict reports content that differs from the persisted intent.
	ErrOperationConflict = errors.New("merge operation content conflicts")
	// ErrUncertain reports an effect whose outcome is unknown and must be
	// reconciled before it can be retried or released.
	ErrUncertain = errors.New("merge effect is uncertain")
	// ErrStaleVersion reports a current request version behind the scanned one.
	ErrStaleVersion = errors.New("assignment version is stale")
	// ErrChangedContent reports a retry whose idempotency content changed.
	ErrChangedContent = errors.New("idempotency content changed")
)

// Clock is the injected time boundary.
type Clock interface {
	Now() time.Time
}

// MergeAssignment is the exact served request identity one merge assignment
// settles: the execution and action that own the request. It is durable
// idempotency evidence, never an approval or a role.
type MergeAssignment struct {
	ExecutionID id.ExecutionID `json:"execution_id"`
	ActionID    id.ActionID    `json:"action_id"`
}

// Validate checks both exact identities.
func (assignment MergeAssignment) Validate() error {
	if _, err := id.ParseExecutionID(string(assignment.ExecutionID)); err != nil {
		return fmt.Errorf("merge assignment execution %q is invalid: %w", assignment.ExecutionID, err)
	}
	if _, err := id.ParseActionID(string(assignment.ActionID)); err != nil {
		return fmt.Errorf("merge assignment action %q is invalid: %w", assignment.ActionID, err)
	}
	return nil
}

// String renders the exact assignment identity for diagnostics and keys.
func (assignment MergeAssignment) String() string {
	return string(assignment.ExecutionID) + "/" + string(assignment.ActionID)
}

// MergeRequest is one requested merge or rebase of an exact candidate onto an
// exact base. The assignment identity makes the request replay-safe: the same
// assignment always converges on one durable effect and result.
type MergeRequest struct {
	Key        MergeKey        `json:"key"`
	Assignment MergeAssignment `json:"assignment"`
	Base       string          `json:"base"`
	Candidate  string          `json:"candidate"`
}

// Validate checks the key, the exact assignment identity and both exact
// revisions.
func (request MergeRequest) Validate() error {
	if err := request.Key.Validate(); err != nil {
		return err
	}
	if err := request.Assignment.Validate(); err != nil {
		return err
	}
	if request.Base == "" {
		return fmt.Errorf("merge request base is empty")
	}
	if request.Candidate == "" {
		return fmt.Errorf("merge request candidate is empty")
	}
	return nil
}

// The typed merge-assignment subject field names. A merge request carries
// exactly these four string fields; any other shape is a malformed mapping.
const (
	MergeFieldRepository = "repository"
	MergeFieldRef        = "ref"
	MergeFieldBase       = "base"
	MergeFieldCandidate  = "candidate"
)

// The declared merge observation names a response may request. The adapter
// fills exactly the declared names and never invents one.
const (
	MergeOutputBase      = "base"
	MergeOutputCandidate = "candidate"
	MergeOutputResult    = "output"
)

// MergeSubject is the typed mapping of one merge-assignment request subject.
type MergeSubject struct {
	Key       MergeKey
	Base      string
	Candidate string
}

// ParseMergeSubject decodes the exact typed merge subject. The mapping is
// strict: all four string fields must be present exactly once with a non-empty
// value, and the repository/ref/base/candidate must be valid. Everything else
// is a malformed mapping and is refused before any effect.
func ParseMergeSubject(subject *api.Subject) (MergeSubject, error) {
	if subject == nil {
		return MergeSubject{}, fmt.Errorf("merge request subject is missing")
	}
	if len(subject.Fields) != 4 {
		return MergeSubject{}, fmt.Errorf("merge request subject has %d fields; want exactly %d", len(subject.Fields), 4)
	}
	values := make(map[string]string, 4)
	for _, field := range subject.Fields {
		switch field.Name {
		case MergeFieldRepository, MergeFieldRef, MergeFieldBase, MergeFieldCandidate:
		default:
			return MergeSubject{}, fmt.Errorf("merge request subject carries unknown field %q", field.Name)
		}
		if field.Kind != "string" {
			return MergeSubject{}, fmt.Errorf("merge request subject field %q has kind %q; want string", field.Name, field.Kind)
		}
		if field.Value == "" {
			return MergeSubject{}, fmt.Errorf("merge request subject field %q is empty", field.Name)
		}
		if _, duplicate := values[field.Name]; duplicate {
			return MergeSubject{}, fmt.Errorf("merge request subject field %q is repeated", field.Name)
		}
		values[field.Name] = field.Value
	}
	mapped := MergeSubject{
		Key: MergeKey{
			Repository: values[MergeFieldRepository],
			Ref:        values[MergeFieldRef],
		},
		Base:      values[MergeFieldBase],
		Candidate: values[MergeFieldCandidate],
	}
	if mapped.Key.Repository == "" || mapped.Key.Ref == "" {
		return MergeSubject{}, fmt.Errorf("merge request subject is missing its repository or ref")
	}
	if err := mapped.Key.Validate(); err != nil {
		return MergeSubject{}, err
	}
	if mapped.Base == "" || mapped.Candidate == "" {
		return MergeSubject{}, fmt.Errorf("merge request subject is missing its base or candidate")
	}
	return mapped, nil
}

// ValidateMergeOutputs checks one declared merge observation name set. A merge
// answer must carry at least one observation, every name must be a declared
// merge observation and no name may repeat.
func ValidateMergeOutputs(outputs []string) error {
	if len(outputs) == 0 {
		return fmt.Errorf("merge request declares no outputs")
	}
	seen := make(map[string]bool, len(outputs))
	for _, output := range outputs {
		switch output {
		case MergeOutputBase, MergeOutputCandidate, MergeOutputResult:
		default:
			return fmt.Errorf("merge request declares unknown output %q", output)
		}
		if seen[output] {
			return fmt.Errorf("merge request declares output %q twice", output)
		}
		seen[output] = true
	}
	return nil
}

// MergeOutputValues maps an applied observed result onto exactly the declared
// observation names. Only observed values are copied; nothing is inferred.
func MergeOutputValues(result MergeResult, declared []string) (map[string]string, error) {
	if err := ValidateMergeOutputs(declared); err != nil {
		return nil, err
	}
	if result.State != EffectApplied {
		return nil, fmt.Errorf("merge result state %q is not applied", result.State)
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	values := make(map[string]string, len(declared))
	for _, name := range declared {
		switch name {
		case MergeOutputBase:
			values[name] = result.ObservedBase
		case MergeOutputCandidate:
			values[name] = result.ObservedCandidate
		case MergeOutputResult:
			values[name] = result.ObservedOutput
		}
	}
	return values, nil
}

// MergeOperation is the durable, idempotent effect identity of one merge.
type MergeOperation struct {
	Key         MergeKey       `json:"key"`
	OperationID id.OperationID `json:"operation_id"`
	Base        string         `json:"base"`
	Candidate   string         `json:"candidate"`
}

// MergeObservation is one observed effect outcome. It reports only what the
// effect boundary observed, never an approval or review result.
type MergeObservation struct {
	State     EffectState `json:"state"`
	Base      string      `json:"base,omitempty"`
	Candidate string      `json:"candidate,omitempty"`
	Output    string      `json:"output,omitempty"`
}

// Validate mirrors MergeResult: applied needs base and candidate, not_started
// forbids every observation, and uncertain accepts any combination.
func (observation MergeObservation) Validate() error {
	if !observation.State.Valid() {
		return fmt.Errorf("merge observation state %q is unknown", observation.State)
	}
	switch observation.State {
	case EffectApplied:
		if observation.Base == "" || observation.Candidate == "" {
			return fmt.Errorf("applied merge observation needs a base and candidate")
		}
	case EffectNotStarted:
		if observation.Base != "" || observation.Candidate != "" || observation.Output != "" {
			return fmt.Errorf("not_started merge observation must not report observations")
		}
	case EffectUncertain:
		// Any combination of observations, including none, is truthful.
	}
	return nil
}

// MergeEffects is the consumer-owned merge/rebase boundary. Execute performs
// the effect; Observe reports what it settled as without repeating it. Neither
// fabricates an observation.
type MergeEffects interface {
	Execute(context.Context, MergeOperation) (MergeObservation, error)
	Observe(context.Context, MergeOperation) (MergeObservation, error)
}

// CoordinationRecord is one durable merge-coordination row.
type CoordinationRecord struct {
	Claim
	TenantID  id.TenantID  `json:"tenant_id"`
	State     EffectState  `json:"state"`
	Request   MergeRequest `json:"request"`
	Result    MergeResult  `json:"result"`
	UpdatedAt time.Time    `json:"updated_at"`
	Released  bool         `json:"released"`
	// Reconciled records that a Reconcile observed a persisted not_started
	// intent as still not started, so a later Release of that intent is safe.
	Reconciled bool `json:"reconciled"`
}

// Validate checks the claim, tenant, state, timestamp, request and result
// consistency of one durable record.
func (record CoordinationRecord) Validate() error {
	if err := record.Claim.Validate(); err != nil {
		return err
	}
	if _, err := id.ParseTenantID(string(record.TenantID)); err != nil {
		return fmt.Errorf("coordination tenant %q is invalid: %w", record.TenantID, err)
	}
	if !record.State.Valid() {
		return fmt.Errorf("coordination state %q is unknown", record.State)
	}
	if record.UpdatedAt.IsZero() {
		return fmt.Errorf("coordination record has no update timestamp")
	}
	if record.Request != (MergeRequest{}) {
		if err := record.Request.Validate(); err != nil {
			return err
		}
		if record.Request.Key != record.Claim.Key {
			return fmt.Errorf("coordination request key %+v does not match the claim key %+v", record.Request.Key, record.Claim.Key)
		}
	}
	if record.Result != (MergeResult{}) {
		if err := record.Result.Validate(); err != nil {
			return err
		}
		if record.Result.Key != record.Claim.Key {
			return fmt.Errorf("coordination result key %+v does not match the claim key %+v", record.Result.Key, record.Claim.Key)
		}
		if record.Result.State != record.State {
			return fmt.Errorf("coordination result state %q disagrees with record state %q", record.Result.State, record.State)
		}
	}
	if record.State == EffectApplied && record.Result.State != EffectApplied {
		return fmt.Errorf("applied coordination record has no applied result")
	}
	if record.Released && !record.State.Releasable() {
		return fmt.Errorf("released coordination record is in non-releasable state %q", record.State)
	}
	return nil
}

// CoordinationStore is the durable, tenant-isolated merge-coordination store.
type CoordinationStore interface {
	Load(context.Context, id.TenantID, MergeKey) (CoordinationRecord, bool, error)
	CreateClaim(context.Context, id.TenantID, MergeKey, string, id.OperationID, time.Time, time.Time) (CoordinationRecord, error)
	Transition(context.Context, id.TenantID, CoordinationRecord, CoordinationRecord) error
}

// CoordinationScanner is the optional enumeration capability of a durable
// coordination store. The coordinator uses it to converge assignments whose
// request resolution committed but whose response was lost, so they can be
// settled without re-executing the effect.
type CoordinationScanner interface {
	Scan(context.Context, id.TenantID) ([]CoordinationRecord, error)
}

// FileCoordinationStore keeps one JSON record per (tenant, repository, ref)
// under one directory and replaces records atomically.
type FileCoordinationStore struct {
	directory string
	// syncDir flushes one directory so a renamed record or a newly created lock
	// file survives a crash. It is injected by tests to observe and fault the
	// flush; a nil value uses the operating-system flush.
	syncDir func(string) error
}

// CoordinatorConfig is the complete merge-coordinator configuration. Every
// field is required.
type CoordinatorConfig struct {
	TenantID       id.TenantID
	Owner          string
	Lease          time.Duration
	Store          CoordinationStore
	Effects        MergeEffects
	Clock          Clock
	NewOperationID func() (id.OperationID, error)
}

// Coordinator serializes merges and records effect evidence on release.
type Coordinator struct {
	tenant         id.TenantID
	owner          string
	lease          time.Duration
	store          CoordinationStore
	effects        MergeEffects
	clock          Clock
	newOperationID func() (id.OperationID, error)
}

// Assignment is one explicit request assignment to a service or human role.
type Assignment struct {
	TenantID      id.TenantID     `json:"tenant_id"`
	ProjectID     id.ProjectID    `json:"project_id"`
	ExecutionID   id.ExecutionID  `json:"execution_id"`
	ActionID      id.ActionID     `json:"action_id"`
	ServiceID     id.PrincipalID  `json:"service_id,omitempty"`
	Request       string          `json:"request"`
	Version       uint64          `json:"version"`
	SourceEventID id.EventID      `json:"source_event_id,omitempty"`
	At            time.Time       `json:"at"`
	Payload       json.RawMessage `json:"payload,omitempty"`
}

// humanRoleNames is the human-role vocabulary. A human role is never a
// service identity and a service adapter never answers one.
var humanRoleNames = []string{"admin", "engineer", "reviewer", "operator", "viewer"}

// validHumanRoleName reports whether value is a declared human role name.
func validHumanRoleName(value string) bool {
	for _, candidate := range humanRoleNames {
		if candidate == value {
			return true
		}
	}
	return false
}

// Validate checks the identities, assignee grammar, version, time and payload.
func (assignment Assignment) Validate() error {
	if _, err := id.ParseTenantID(string(assignment.TenantID)); err != nil {
		return fmt.Errorf("assignment tenant %q is invalid: %w", assignment.TenantID, err)
	}
	if _, err := id.ParseProjectID(string(assignment.ProjectID)); err != nil {
		return fmt.Errorf("assignment project %q is invalid: %w", assignment.ProjectID, err)
	}
	if _, err := id.ParseExecutionID(string(assignment.ExecutionID)); err != nil {
		return fmt.Errorf("assignment execution %q is invalid: %w", assignment.ExecutionID, err)
	}
	if _, err := id.ParseActionID(string(assignment.ActionID)); err != nil {
		return fmt.Errorf("assignment action %q is invalid: %w", assignment.ActionID, err)
	}
	if err := validateAssignee(assignment); err != nil {
		return err
	}
	if assignment.Version == 0 {
		return fmt.Errorf("assignment version is zero")
	}
	if assignment.At.IsZero() {
		return fmt.Errorf("assignment time is zero")
	}
	if assignment.SourceEventID != "" {
		if _, err := id.ParseEventID(string(assignment.SourceEventID)); err != nil {
			return fmt.Errorf("assignment source event %q is invalid: %w", assignment.SourceEventID, err)
		}
	}
	if len(assignment.Payload) > 0 {
		trimmed := strings.TrimSpace(string(assignment.Payload))
		if trimmed == "" || trimmed[0] != '{' || !json.Valid(assignment.Payload) {
			return fmt.Errorf("assignment payload is not a JSON object")
		}
	}
	return nil
}

// validateAssignee checks exactly `service/<service id>` or `role/<role>`.
func validateAssignee(assignment Assignment) error {
	const servicePrefix = "service/"
	const rolePrefix = "role/"
	switch {
	case strings.HasPrefix(assignment.Request, servicePrefix):
		if assignment.ServiceID == "" {
			return fmt.Errorf("service assignment has no service identity")
		}
		if _, err := id.ParsePrincipalID(string(assignment.ServiceID)); err != nil {
			return fmt.Errorf("assignment service %q is invalid: %w", assignment.ServiceID, err)
		}
		if assignment.Request != servicePrefix+string(assignment.ServiceID) {
			return fmt.Errorf("assignment request %q does not name its service %q", assignment.Request, assignment.ServiceID)
		}
		return nil
	case strings.HasPrefix(assignment.Request, rolePrefix):
		if assignment.ServiceID != "" {
			return fmt.Errorf("human-role assignment carries a service identity")
		}
		role := strings.TrimPrefix(assignment.Request, rolePrefix)
		if !validHumanRoleName(role) {
			return fmt.Errorf("assignment role %q is not a frozen human role", role)
		}
		return nil
	default:
		return fmt.Errorf("assignment request %q is not a service or role assignment", assignment.Request)
	}
}

// Notification is one retained hint that an assignment may exist. Delivery may
// repeat or reorder; a notification grants neither authority nor exclusivity.
type Notification struct {
	EventID   id.EventID     `json:"event_id"`
	Type      string         `json:"type"`
	TenantID  id.TenantID    `json:"tenant_id"`
	ProjectID id.ProjectID   `json:"project_id"`
	ServiceID id.PrincipalID `json:"service_id,omitempty"`
	At        time.Time      `json:"at"`
}

// NotificationPage is one page of retained notifications.
type NotificationPage struct {
	Notifications []Notification `json:"notifications"`
	Next          api.PageInfo   `json:"next"`
}

// EventSource is the retained-notification boundary. Notifications are hints
// only; they never cause a resolve and never own a grant.
type EventSource interface {
	Poll(context.Context, string) (NotificationPage, error)
}

// Public operations and the single service decision used by the adapters.
const (
	OperationInbox   = "task.inbox"
	OperationInspect = "execution.inspect"
	OperationResolve = "execution.request.resolve"
	DecisionPass     = "pass"
)

// The adapter wire types are aliases of the served public declarations, never
// private lookalikes. A value the services package hands to a public codec or
// handler is therefore the exact type the public edge decodes, so a private
// member such as the legacy tenant_id/limit/cursor inbox shape cannot reappear.
type (
	// InboxInput is the served task.inbox request: an optional project_id, the
	// optional kinds list and the optional page.
	InboxInput = api.TaskInboxRequest
	// InboxResult is the served task.inbox result.
	InboxResult = api.TaskInboxResult
	// InspectInput is the served execution.inspect request.
	InspectInput = api.ExecutionInspectRequest
	// InspectResult is the served execution.inspect result.
	InspectResult = api.ExecutionInspectResult
	// ResolveInput is the served action-only execution.request.resolve input.
	ResolveInput = api.ExecutionRequestResolveRequest
)

// AdapterConfig is the complete independent-service adapter configuration.
type AdapterConfig struct {
	Kind           ServiceKind
	ServiceID      id.PrincipalID
	TenantID       id.TenantID
	ProjectID      id.ProjectID
	Handler        api.Handler
	Events         EventSource
	Clock          Clock
	Schedule       time.Duration
	RescanInterval time.Duration
	// Merges is the required merge-coordinator dependency for
	// KindMergeCoordinator and is unused by every other kind.
	Merges MergeRunner
}

// Adapter is one independent service adapter.
type Adapter struct {
	kind      ServiceKind
	serviceID id.PrincipalID
	tenant    id.TenantID
	project   id.ProjectID
	handler   api.Handler
	events    EventSource
	clock     Clock
	schedule  time.Duration
	rescan    time.Duration
	merges    MergeRunner
}
