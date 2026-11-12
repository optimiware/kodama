// Independent-service implementation API.
//
// This file is the test-side declaration of the behavior of
// internal/userland/services. The base declarations in types.go are not
// redefined here. Every test drives deterministic fakes (event source, public
// handler, coordination store, merge effects, clock) and real temporary
// directories for the file store. No process, container, database, broker,
// cloud call or factory-database access is involved.
//
// Declared in types.go and not pinned here: ServiceKind (webhook/email/timer/
// responder/merge_coordinator), ServiceAdapter, EffectState
// (not_started/applied/uncertain), EffectState.Releasable, MergeKey,
// MergeCoordinator, Claim.Expired.
//
// # Further declarations
//
//	type Claim struct {
//	    Key         MergeKey       `json:"key"`
//	    Owner       string         `json:"owner"`
//	    OperationID id.OperationID `json:"operation_id"`
//	    Generation  uint64         `json:"generation"`
//	    ExpiresAt   time.Time      `json:"expires_at"`
//	}
//	func (claim Claim) Validate() error
//
// Generation starts at 1 and increases on every successful claim of the same
// key. OperationID is the durable merge-operation identity that effects are
// idempotent under. A takeover of an unsettled intent (uncertain, or
// not_started with a request) reuses the previous OperationID; a settled or
// released record starts a new one. Validate requires a valid key, a non-empty
// owner, a parseable operation identity, a positive generation and a non-zero
// expiry.
//
//	type MergeResult struct {
//	    Key               MergeKey    `json:"key"`
//	    State             EffectState `json:"state"`
//	    ObservedBase      string      `json:"observed_base,omitempty"`
//	    ObservedCandidate string      `json:"observed_candidate,omitempty"`
//	    ObservedOutput    string      `json:"observed_output,omitempty"`
//	}
//
// MergeResult.Validate requires base and candidate for applied, forbids any
// observation for not_started, and accepts zero or more observations for
// uncertain: an unknown outcome is never fabricated. A merge result reports
// only observed state and carries no review, approval or role field.
//
// # Ports
//
//	type Clock interface { Now() time.Time }
//
//	type MergeRequest struct {
//	    Key       MergeKey
//	    Base      string
//	    Candidate string
//	}
//	func (request MergeRequest) Validate() error
//
//	type MergeOperation struct {
//	    Key         MergeKey
//	    OperationID id.OperationID
//	    Base        string
//	    Candidate   string
//	}
//
//	type MergeObservation struct {
//	    State     EffectState
//	    Base      string
//	    Candidate string
//	    Output    string
//	}
//	func (observation MergeObservation) Validate() error
//
// MergeObservation.Validate mirrors MergeResult: applied needs base and
// candidate, not_started forbids all observations, uncertain accepts any
// combination including none.
//
//	type MergeEffects interface {
//	    Execute(context.Context, MergeOperation) (MergeObservation, error)
//	    Observe(context.Context, MergeOperation) (MergeObservation, error)
//	}
//
// Execute performs the merge/rebase effect. Observe reports what the effect
// settled as without repeating it. Neither fabricates an observation.
//
//	type CoordinationRecord struct {
//	    Claim
//	    TenantID  id.TenantID
//	    State     EffectState
//	    Request   MergeRequest
//	    Result    MergeResult
//	    UpdatedAt time.Time
//	    Released  bool
//	    Reconciled bool
//	}
//	func (record CoordinationRecord) Validate() error
//
// Validate requires a valid claim and tenant, a known state, a non-zero
// UpdatedAt, a request whose key matches the claim key (or the zero request),
// a result whose key and state match the record when present, an applied
// record with an applied result, and a released record in a releasable state.
// Reconciled records that a Reconcile observed a persisted not_started intent
// as still not started, so a later Release of that intent is safe.
//
//	type CoordinationStore interface {
//	    Load(context.Context, id.TenantID, MergeKey) (CoordinationRecord, bool, error)
//	    CreateClaim(context.Context, id.TenantID, MergeKey, string, id.OperationID, time.Time, time.Time) (CoordinationRecord, error)
//	    Transition(context.Context, id.TenantID, CoordinationRecord, CoordinationRecord) error
//	}
//
// CreateClaim atomically creates a claim iff the stored record is missing,
// released, or expired at now. It assigns Generation = previous+1 (or 1). If
// the stored record has an unsettled intent (not released and either uncertain
// or not_started with a request) it reuses that record's OperationID, State,
// Request, Result and Reconciled; otherwise it starts not_started with the
// supplied OperationID. A live record (not released and not expired) returns
// ErrClaimed. Transition atomically replaces expected with next only when the
// stored record equals expected exactly and next has the same tenant, key,
// owner, OperationID and Generation; otherwise it returns ErrStaleClaim and
// leaves the store unchanged.
//
// Sentinel errors (errors.Is):
//
//	var (
//	    ErrClaimed
//	    ErrStaleClaim
//	    ErrNoClaim
//	    ErrOperationConflict
//	    ErrUncertain
//	    ErrStaleVersion
//	    ErrChangedContent
//	)
//
//	func NewFileCoordinationStore(directory string) (*FileCoordinationStore, error)
//	type FileCoordinationStore struct{ ... } // implements CoordinationStore
//
// The file store keeps one JSON record per (tenant, repository, ref), creates
// its directory (and parents) when missing, rejects an empty directory and
// invalid tenants/keys, and replaces records atomically. A new store instance
// over the same directory observes the same records, and two instances never
// both create a claim for the same tenant/key at the same time. Tenants
// sharing a directory and a repository/ref never observe each other's records.
//
//	type CoordinatorConfig struct {
//	    TenantID       id.TenantID
//	    Owner          string
//	    Lease          time.Duration
//	    Store          CoordinationStore
//	    Effects        MergeEffects
//	    Clock          Clock
//	    NewOperationID func() (id.OperationID, error)
//	}
//
// Every field is required; a nil boundary, invalid tenant, empty owner or
// non-positive lease refuses.
//
//	func NewCoordinator(CoordinatorConfig) (*Coordinator, error)
//	type Coordinator struct{ ... } // implements MergeCoordinator
//
//	func (*Coordinator) Claim(context.Context, MergeKey) (Claim, error)
//	func (*Coordinator) Begin(context.Context, Claim, MergeRequest) (CoordinationRecord, bool, error)
//	func (*Coordinator) Execute(context.Context, Claim) (MergeResult, error)
//	func (*Coordinator) Reconcile(context.Context, Claim) (MergeResult, error)
//	func (*Coordinator) Release(context.Context, Claim, EffectState) error
//	func (*Coordinator) Merge(context.Context, MergeRequest) (MergeResult, error)
//
// Every method validates the claim (key, owner, OperationID, Generation and an
// unexpired lease) against the durable record before acting; a claim that no
// longer matches the stored record is ErrStaleClaim.
//
// Claim returns the existing claim when the durable record is live and owned by
// this coordinator, refuses another live owner with ErrClaimed, and otherwise
// creates the next generation through the store. A failed operation-identity
// generator fails the claim before any record is created.
//
// Begin validates the claim (key, owner, OperationID, Generation, and an
// unexpired lease) against the durable record and persists the operation intent
// (state not_started plus the request) before any effect; the bool reports
// whether a new intent was persisted. A repeated Begin with identical content
// returns the existing record and false; different content is
// ErrOperationConflict and leaves the store unchanged. Begin never starts an
// effect.
//
// Execute loads the persisted intent and only then calls Effects.Execute; it
// persists the observed state before returning. An applied observation settles
// the operation even when the effect call also returned an error. An uncertain
// observation, or an effect error without a settled observation, is persisted
// as uncertain and returned with ErrUncertain. Execute refuses an uncertain
// intent with ErrUncertain and a missing/released/stale record with
// ErrStaleClaim, and never reports success when the observation could not be
// persisted.
//
// Reconcile observes an existing intent and never executes. It persists the
// observation; an uncertain observation stays uncertain and returns
// ErrUncertain. Observing not_started sets Reconciled on the record. Reconcile
// with no persisted intent is ErrNoClaim.
//
// Release refuses an uncertain record (ErrUncertain), a not_started record with
// a request that was not reconciled by this claim (ErrUncertain), a
// non-releasable or state-mismatched evidence value (ErrOperationConflict), and
// a stale claim (ErrStaleClaim). Release persists the released record; lease
// expiry alone is never evidence about an effect.
//
// Merge claims, begins, reconciles an already-persisted intent (including a
// not_started one) and only then executes. It never releases an uncertain
// operation. A settled operation is released with its state.
//
// # Service adapter API
//
//	type Assignment struct {
//	    TenantID      id.TenantID
//	    ProjectID     id.ProjectID
//	    ExecutionID   id.ExecutionID
//	    ActionID      id.ActionID
//	    ServiceID     id.PrincipalID
//	    Request       string        // "service/<id>" or "role/<role>"
//	    Version       uint64
//	    SourceEventID id.EventID
//	    At            time.Time
//	    Payload       json.RawMessage
//	}
//	func (assignment Assignment) Validate() error
//
// Assignment.Validate requires valid tenant/project/execution/action
// identities, a positive version, a non-zero time, the assignee grammar (exactly
// `service/<service id>` with a valid ServiceID, or `role/<role>` for a declared
// human role with no ServiceID), and an empty or JSON-object payload.
//
//	type Notification struct {
//	    EventID   id.EventID
//	    Type      string
//	    TenantID  id.TenantID
//	    ProjectID id.ProjectID
//	    ServiceID id.PrincipalID
//	    At        time.Time
//	}
//	type NotificationPage struct {
//	    Notifications []Notification
//	    Next          api.PageInfo
//	}
//	type EventSource interface {
//	    Poll(context.Context, string) (NotificationPage, error)
//	}
//
// Delivery may repeat or reorder. Notifications are hints only and grant
// neither assignment authority nor exclusivity.
//
//	const (
//	    OperationInbox   = "task.inbox"
//	    OperationInspect = "execution.inspect"
//	    OperationResolve = "execution.request.resolve"
//	    DecisionPass     = "pass"
//	)
//
// The adapter wire names are aliases of the served public declarations, not
// private DTOs:
//
//	type InboxInput   = api.TaskInboxRequest
//	type InboxResult  = api.TaskInboxResult
//	type InspectInput = api.ExecutionInspectRequest
//	type InspectResult = api.ExecutionInspectResult
//	type ResolveInput = api.ExecutionRequestResolveRequest
//
// A task.inbox input is therefore the served {project_id, kinds, page} shape:
// it carries no tenant_id, limit or cursor, and handing anything else to the
// strict public codec is refused.
//
//	type AdapterConfig struct {
//	    Kind           ServiceKind
//	    ServiceID      id.PrincipalID
//	    TenantID       id.TenantID
//	    ProjectID      id.ProjectID
//	    Handler        api.Handler
//	    Events         EventSource
//	    Clock          Clock
//	    Schedule       time.Duration // required for timer; ignored otherwise
//	    RescanInterval time.Duration // optional; defaults to 30s
//	}
//	func NewAdapter(AdapterConfig) (*Adapter, error)
//	type Adapter struct{ ... } // implements ServiceAdapter
//	func (*Adapter) RunOnce(context.Context) error
//
// Adapter.RunOnce behavior:
//
//   - Poll the EventSource to its final page. Notifications are hints only:
//     they never cause a resolve and never grant assignment authority.
//   - Scan the current inbox with OperationInbox using the served
//     api.TaskInboxRequest{ProjectID, Kinds: []string{"request"}, Page}, request
//     a positive page limit and follow NextCursor until it is empty or repeats.
//     Ignore (without inspecting) every item that is not the request kind or
//     whose assignment is not exactly "service/<service id>". Human-role and
//     foreign-service assignments are never answered.
//   - Revalidate each candidate with OperationInspect by execution identity. A
//     not_found error, a different current execution, a missing current action,
//     or an answered or non-running current action skips the candidate. A
//     current action version lower than the scanned version is ErrStaleVersion.
//     The resolution uses the current execution and action versions, never the
//     scanned source version.
//   - Resolve with OperationResolve: the action-only
//     api.ExecutionRequestResolveRequest carries the current
//     expected_action_version and DecisionPass, and the shared envelope
//     expected_version carries the current execution version. The adapter
//     submits no outputs; an explicit service-supplied response is a separately
//     specified decision boundary. A version_conflict refusal is ErrStaleVersion;
//     a changed_idempotency_content refusal is ErrChangedContent. Other errors
//     are returned unchanged so a later pass retries with identical operation,
//     content and key.
//   - Process every candidate even when an earlier one fails; return the first
//     scan error after the pass, otherwise a notification-drain error that Run
//     treats as retryable.
//   - Idempotency key: the adapter always derives
//     "<kind>:v<action version>:<32 lowercase hex characters of the SHA-256 of
//     the joined scope>", where the scope is (kind, execution, action, current
//     execution version, current action version); timer prepends the Unix second
//     of Clock.Now().Truncate(Schedule). Redelivery of the same current state
//     reuses the key, while a newer current version changes it.
//   - The resolve input carries no outputs and never copies a payload out of
//     inbox metadata. Identical current state therefore produces byte-identical
//     inputs.
//
// Adapter.Run performs RunOnce at startup and then periodically at
// RescanInterval until ctx is done; it returns nil on cancellation.
package services_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/services"
)

// Compile-time pins for the new implementation surface.
var (
	_ services.ServiceAdapter    = (*services.Adapter)(nil)
	_ services.MergeCoordinator  = (*services.Coordinator)(nil)
	_ services.MergeRunner       = (*services.Coordinator)(nil)
	_ services.CoordinationStore = (*services.FileCoordinationStore)(nil)
	_ services.MergeEffects      = (*fakeMergeEffects)(nil)
	_ services.EventSource       = (*fakeEventSource)(nil)
	_ services.Clock             = (*fakeClock)(nil)
	_ api.Handler                = (*fakeHandler)(nil)
)

func TestClaimGenerationAndOperationIdentity(t *testing.T) {
	claim := validClaim(t)
	if err := claim.Validate(); err != nil {
		t.Fatalf("valid claim refused: %v", err)
	}
	if claim.Expired(testNow) {
		t.Fatal("a claim expiring in the future reports expired")
	}
	if !claim.Expired(testNow.Add(2 * time.Minute)) {
		t.Fatal("an elapsed claim does not report expired")
	}

	for name, invalid := range map[string]services.Claim{
		"empty owner":          {Key: validMergeKey(), OperationID: "op-1", Generation: 1, ExpiresAt: testNow},
		"empty operation id":   {Key: validMergeKey(), Owner: "w", Generation: 1, ExpiresAt: testNow},
		"invalid operation id": {Key: validMergeKey(), Owner: "w", OperationID: "op/../1", Generation: 1, ExpiresAt: testNow},
		"zero generation":      {Key: validMergeKey(), Owner: "w", OperationID: "op-1", ExpiresAt: testNow},
		"zero expiry":          {Key: validMergeKey(), Owner: "w", OperationID: "op-1", Generation: 1},
		"invalid key":          {Key: services.MergeKey{}, Owner: "w", OperationID: "op-1", Generation: 1, ExpiresAt: testNow},
	} {
		t.Run(name, func(t *testing.T) {
			if err := invalid.Validate(); err == nil {
				t.Fatalf("Claim.Validate accepted %+v", invalid)
			}
		})
	}
}

func TestMergeResultUncertainWithoutFabricatedObservations(t *testing.T) {
	key := validMergeKey()

	// An uncertain result carries exactly what was observed, which may be
	// nothing at all. The coordinator must not invent a base or candidate.
	uncertain := services.MergeResult{Key: key, State: services.EffectUncertain}
	if err := uncertain.Validate(); err != nil {
		t.Fatalf("uncertain result without observations refused: %v", err)
	}
	partial := services.MergeResult{Key: key, State: services.EffectUncertain, ObservedBase: "base-1"}
	if err := partial.Validate(); err != nil {
		t.Fatalf("partially observed uncertain result refused: %v", err)
	}
	outputOnly := services.MergeResult{Key: key, State: services.EffectUncertain, ObservedOutput: "conflict while rebasing"}
	if err := outputOnly.Validate(); err != nil {
		t.Fatalf("output-only uncertain result refused: %v", err)
	}

	applied := services.MergeResult{Key: key, State: services.EffectApplied, ObservedBase: "base-1", ObservedCandidate: "cand-1", ObservedOutput: "Fast-forward"}
	if err := applied.Validate(); err != nil {
		t.Fatalf("applied result refused: %v", err)
	}
	if err := (services.MergeResult{Key: key, State: services.EffectApplied, ObservedBase: "base-1"}).Validate(); err == nil {
		t.Fatal("applied result without a candidate accepted")
	}
	if err := (services.MergeResult{Key: key, State: services.EffectNotStarted, ObservedOutput: "x"}).Validate(); err == nil {
		t.Fatal("not_started result with an observation accepted")
	}
	if err := (services.MergeResult{Key: key, State: "maybe"}).Validate(); err == nil {
		t.Fatal("unknown result state accepted")
	}

	// A result never carries review authority.
	assertNoFieldMatching(t, reflect.TypeFor[services.MergeResult](), "approval", "role", "reviewer", "authority", "decision")
}

func TestMergeObservationAndRequestValidation(t *testing.T) {
	key := validMergeKey()
	for name, observation := range map[string]services.MergeObservation{
		"applied":     {State: services.EffectApplied, Base: "base-1", Candidate: "cand-1", Output: "Fast-forward"},
		"not started": {State: services.EffectNotStarted},
		"uncertain":   {State: services.EffectUncertain},
		"partial":     {State: services.EffectUncertain, Base: "base-1", Output: "conflict"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := observation.Validate(); err != nil {
				t.Fatalf("valid observation refused: %v", err)
			}
		})
	}
	for name, observation := range map[string]services.MergeObservation{
		"applied without base":      {State: services.EffectApplied, Candidate: "cand-1"},
		"applied without candidate": {State: services.EffectApplied, Base: "base-1"},
		"not_started with base":     {State: services.EffectNotStarted, Base: "base-1"},
		"not_started with output":   {State: services.EffectNotStarted, Output: "x"},
		"unknown state":             {State: "maybe"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := observation.Validate(); err == nil {
				t.Fatalf("MergeObservation.Validate accepted %+v", observation)
			}
		})
	}

	if err := (services.MergeRequest{Key: key, Assignment: services.MergeAssignment{ExecutionID: "exec-1", ActionID: "action-1"}, Base: "base-1", Candidate: "cand-1"}).Validate(); err != nil {
		t.Fatalf("valid merge request refused: %v", err)
	}
	for name, request := range map[string]services.MergeRequest{
		"invalid key":        {Base: "base-1", Candidate: "cand-1", Assignment: services.MergeAssignment{ExecutionID: "exec-1", ActionID: "action-1"}},
		"missing assignment": {Key: key, Base: "base-1", Candidate: "cand-1"},
		"invalid assignment": {Key: key, Assignment: services.MergeAssignment{ExecutionID: "../escape", ActionID: "action-1"}, Base: "base-1", Candidate: "cand-1"},
		"empty base":         {Key: key, Assignment: services.MergeAssignment{ExecutionID: "exec-1", ActionID: "action-1"}, Candidate: "cand-1"},
		"empty candidate":    {Key: key, Assignment: services.MergeAssignment{ExecutionID: "exec-1", ActionID: "action-1"}, Base: "base-1"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := request.Validate(); err == nil {
				t.Fatalf("MergeRequest.Validate accepted %+v", request)
			}
		})
	}

	// A merge request carries no human role, approval or reviewer field.
	assertNoFieldMatching(t, reflect.TypeFor[services.MergeRequest](), "approval", "role", "reviewer", "authority")
}

func TestCoordinationRecordValidation(t *testing.T) {
	claim := validClaim(t)
	fresh := services.CoordinationRecord{
		Claim:     claim,
		TenantID:  testTenant,
		State:     services.EffectNotStarted,
		UpdatedAt: testNow,
	}
	if err := fresh.Validate(); err != nil {
		t.Fatalf("fresh record refused: %v", err)
	}

	intent := fresh
	intent.Request = services.MergeRequest{Key: claim.Key, Assignment: services.MergeAssignment{ExecutionID: "exec-1", ActionID: "action-1"}, Base: "base-1", Candidate: "cand-1"}
	if err := intent.Validate(); err != nil {
		t.Fatalf("intent record refused: %v", err)
	}

	applied := intent
	applied.State = services.EffectApplied
	applied.Result = services.MergeResult{Key: claim.Key, State: services.EffectApplied, ObservedBase: "base-1", ObservedCandidate: "cand-1"}
	if err := applied.Validate(); err != nil {
		t.Fatalf("applied record refused: %v", err)
	}
	applied.Result = services.MergeResult{}
	if err := applied.Validate(); err == nil {
		t.Fatal("applied record without a result accepted")
	}

	mismatched := intent
	mismatched.State = services.EffectApplied
	mismatched.Result = services.MergeResult{Key: claim.Key, State: services.EffectUncertain}
	if err := mismatched.Validate(); err == nil {
		t.Fatal("record whose result state disagrees with its state accepted")
	}

	releasedUncertain := intent
	releasedUncertain.State = services.EffectUncertain
	releasedUncertain.Result = services.MergeResult{Key: claim.Key, State: services.EffectUncertain}
	releasedUncertain.Released = true
	if err := releasedUncertain.Validate(); err == nil {
		t.Fatal("released uncertain record accepted")
	}

	for name, record := range map[string]services.CoordinationRecord{
		"missing tenant":    {Claim: claim, State: services.EffectNotStarted, UpdatedAt: testNow},
		"invalid tenant":    {Claim: claim, TenantID: id.TenantID("../escape"), State: services.EffectNotStarted, UpdatedAt: testNow},
		"unknown state":     {Claim: claim, TenantID: testTenant, State: "maybe", UpdatedAt: testNow},
		"missing timestamp": {Claim: claim, TenantID: testTenant, State: services.EffectNotStarted},
		"foreign request":   {Claim: claim, TenantID: testTenant, State: services.EffectNotStarted, UpdatedAt: testNow, Request: services.MergeRequest{Key: services.MergeKey{Repository: "other/repo", Ref: "refs/heads/main"}, Assignment: services.MergeAssignment{ExecutionID: "exec-1", ActionID: "action-1"}, Base: "a", Candidate: "b"}},
		"invalid claim":     {Claim: services.Claim{}, TenantID: testTenant, State: services.EffectNotStarted, UpdatedAt: testNow},
	} {
		t.Run(name, func(t *testing.T) {
			if err := record.Validate(); err == nil {
				t.Fatalf("CoordinationRecord.Validate accepted %+v", record)
			}
		})
	}
}

func TestAssignmentValidation(t *testing.T) {
	assignment := validAssignment()
	if err := assignment.Validate(); err != nil {
		t.Fatalf("valid assignment refused: %v", err)
	}

	human := validAssignment()
	human.Request = "role/reviewer"
	human.ServiceID = ""
	if err := human.Validate(); err != nil {
		t.Fatalf("valid human-role assignment refused: %v", err)
	}

	for name, invalid := range map[string]services.Assignment{
		"missing tenant":       {ProjectID: testProject, ExecutionID: "exec-1", ActionID: "action-1", ServiceID: "email-svc", Request: "service/email-svc", Version: 1, At: testNow},
		"invalid tenant":       {TenantID: "../escape", ProjectID: testProject, ExecutionID: "exec-1", ActionID: "action-1", ServiceID: "email-svc", Request: "service/email-svc", Version: 1, At: testNow},
		"missing project":      {TenantID: testTenant, ExecutionID: "exec-1", ActionID: "action-1", ServiceID: "email-svc", Request: "service/email-svc", Version: 1, At: testNow},
		"missing execution":    {TenantID: testTenant, ProjectID: testProject, ActionID: "action-1", ServiceID: "email-svc", Request: "service/email-svc", Version: 1, At: testNow},
		"missing action":       {TenantID: testTenant, ProjectID: testProject, ExecutionID: "exec-1", ServiceID: "email-svc", Request: "service/email-svc", Version: 1, At: testNow},
		"missing service":      {TenantID: testTenant, ProjectID: testProject, ExecutionID: "exec-1", ActionID: "action-1", Request: "service/email-svc", Version: 1, At: testNow},
		"zero version":         {TenantID: testTenant, ProjectID: testProject, ExecutionID: "exec-1", ActionID: "action-1", ServiceID: "email-svc", Request: "service/email-svc", At: testNow},
		"missing time":         {TenantID: testTenant, ProjectID: testProject, ExecutionID: "exec-1", ActionID: "action-1", ServiceID: "email-svc", Request: "service/email-svc", Version: 1},
		"bad assignee":         {TenantID: testTenant, ProjectID: testProject, ExecutionID: "exec-1", ActionID: "action-1", ServiceID: "email-svc", Request: "email-svc", Version: 1, At: testNow},
		"empty assignee name":  {TenantID: testTenant, ProjectID: testProject, ExecutionID: "exec-1", ActionID: "action-1", ServiceID: "email-svc", Request: "service/", Version: 1, At: testNow},
		"unknown role":         {TenantID: testTenant, ProjectID: testProject, ExecutionID: "exec-1", ActionID: "action-1", ServiceID: "email-svc", Request: "role/robot", Version: 1, At: testNow},
		"service as role":      {TenantID: testTenant, ProjectID: testProject, ExecutionID: "exec-1", ActionID: "action-1", ServiceID: "email-svc", Request: "role/email-svc", Version: 1, At: testNow},
		"invalid source event": {TenantID: testTenant, ProjectID: testProject, ExecutionID: "exec-1", ActionID: "action-1", ServiceID: "email-svc", Request: "service/email-svc", Version: 1, SourceEventID: "event/../1", At: testNow},
		"non-object payload":   {TenantID: testTenant, ProjectID: testProject, ExecutionID: "exec-1", ActionID: "action-1", ServiceID: "email-svc", Request: "service/email-svc", Version: 1, At: testNow, Payload: json.RawMessage(`[1,2]`)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := invalid.Validate(); err == nil {
				t.Fatalf("Assignment.Validate accepted %+v", invalid)
			}
		})
	}
}

func TestAdapterConfigRejectsIncompleteConfiguration(t *testing.T) {
	base := services.AdapterConfig{
		Kind:      services.KindEmail,
		ServiceID: testService,
		TenantID:  testTenant,
		ProjectID: testProject,
		Handler:   &fakeHandler{log: &callLog{}},
		Events:    &fakeEventSource{},
		Clock:     &fakeClock{now: testNow},
	}
	if _, err := services.NewAdapter(base); err != nil {
		t.Fatalf("NewAdapter with a complete config: %v", err)
	}

	for name, mutate := range map[string]func(*services.AdapterConfig){
		"invalid kind":         func(config *services.AdapterConfig) { config.Kind = "artifact_publisher" },
		"missing service id":   func(config *services.AdapterConfig) { config.ServiceID = "" },
		"invalid service id":   func(config *services.AdapterConfig) { config.ServiceID = "svc/../1" },
		"invalid tenant":       func(config *services.AdapterConfig) { config.TenantID = "../escape" },
		"missing project":      func(config *services.AdapterConfig) { config.ProjectID = "" },
		"missing handler":      func(config *services.AdapterConfig) { config.Handler = nil },
		"missing events":       func(config *services.AdapterConfig) { config.Events = nil },
		"missing clock":        func(config *services.AdapterConfig) { config.Clock = nil },
		"negative rescan":      func(config *services.AdapterConfig) { config.RescanInterval = -time.Second },
		"timer without period": func(config *services.AdapterConfig) { config.Kind = services.KindTimer },
	} {
		t.Run(name, func(t *testing.T) {
			config := base
			mutate(&config)
			if _, err := services.NewAdapter(config); err == nil {
				t.Fatalf("NewAdapter accepted a config with %s", name)
			}
		})
	}

	timer := base
	timer.Kind = services.KindTimer
	timer.Schedule = time.Minute
	if _, err := services.NewAdapter(timer); err != nil {
		t.Fatalf("NewAdapter refused a timer with a schedule: %v", err)
	}

	// A merge coordinator is a real adapter only when its merge dependency is
	// configured. A missing dependency is refused; the configured positive
	// control sits beside it.
	merge := base
	merge.Kind = services.KindMergeCoordinator
	if _, err := services.NewAdapter(merge); err == nil {
		t.Fatal("NewAdapter accepted a merge coordinator without a merge dependency")
	}
	coordinator, err := services.NewCoordinator(validCoordinatorConfig(
		newFakeCoordinationStore(&callLog{}), &fakeMergeEffects{log: &callLog{}}, &fakeClock{now: testNow}))
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	merge.Merges = coordinator
	if _, err := services.NewAdapter(merge); err != nil {
		t.Fatalf("NewAdapter refused a configured merge coordinator: %v", err)
	}
}

func TestCoordinatorConfigRejectsIncompleteConfiguration(t *testing.T) {
	base := validCoordinatorConfig(&fakeCoordinationStore{log: &callLog{}}, &fakeMergeEffects{log: &callLog{}}, &fakeClock{now: testNow})
	if _, err := services.NewCoordinator(base); err != nil {
		t.Fatalf("NewCoordinator with a complete config: %v", err)
	}

	for name, mutate := range map[string]func(*services.CoordinatorConfig){
		"missing tenant":    func(config *services.CoordinatorConfig) { config.TenantID = "" },
		"invalid tenant":    func(config *services.CoordinatorConfig) { config.TenantID = "../escape" },
		"empty owner":       func(config *services.CoordinatorConfig) { config.Owner = "" },
		"zero lease":        func(config *services.CoordinatorConfig) { config.Lease = 0 },
		"negative lease":    func(config *services.CoordinatorConfig) { config.Lease = -time.Second },
		"missing store":     func(config *services.CoordinatorConfig) { config.Store = nil },
		"missing effects":   func(config *services.CoordinatorConfig) { config.Effects = nil },
		"missing clock":     func(config *services.CoordinatorConfig) { config.Clock = nil },
		"missing operation": func(config *services.CoordinatorConfig) { config.NewOperationID = nil },
	} {
		t.Run(name, func(t *testing.T) {
			config := base
			mutate(&config)
			if _, err := services.NewCoordinator(config); err == nil {
				t.Fatalf("NewCoordinator accepted a config with %s", name)
			}
		})
	}
}

func TestInvalidOperationIdentityRefusedAtClaim(t *testing.T) {
	store := &fakeCoordinationStore{log: &callLog{}}
	effects := &fakeMergeEffects{log: &callLog{}}
	config := validCoordinatorConfig(store, effects, &fakeClock{now: testNow})
	config.NewOperationID = func() (id.OperationID, error) { return "", errors.New("no operation id") }
	coordinator, err := services.NewCoordinator(config)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	if _, err := coordinator.Claim(context.Background(), validMergeKey()); err == nil {
		t.Fatal("Claim accepted a failing operation identity generator")
	}
	if _, ok, _ := store.Load(context.Background(), testTenant, validMergeKey()); ok {
		t.Fatal("a failed operation identity generator still created a claim")
	}
}

// assertNoFieldMatching fails when a struct type declares a field whose name
// contains one of the forbidden fragments. It keeps review authority out of
// effect results without inspecting private implementation steps.
func assertNoFieldMatching(t *testing.T, structType reflect.Type, forbidden ...string) {
	t.Helper()
	for field := range structType.Fields() {
		name := strings.ToLower(field.Name)
		for _, fragment := range forbidden {
			if strings.Contains(name, fragment) {
				t.Fatalf("%s declares field %q; effect results carry no %s authority", structType, field.Name, fragment)
			}
		}
	}
}

// assertNoMethodMatching fails when a type exposes a method that could satisfy a
// human review or approval step.
func assertNoMethodMatching(t *testing.T, typeOf reflect.Type, forbidden ...string) {
	t.Helper()
	for method := range typeOf.Methods() {
		name := strings.ToLower(method.Name)
		for _, fragment := range forbidden {
			if strings.Contains(name, fragment) {
				t.Fatalf("%s exposes method %q; a merge coordinator never approves or reviews", typeOf, method.Name)
			}
		}
	}
}
