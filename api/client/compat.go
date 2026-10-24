package client

// Source compatibility of the generated client.
//
// oapi-codegen prefixes an enum constant with its type name only when two enums
// of the document would otherwise declare the same constant. Adding a fragment
// whose enums share values with an earlier one (the business-loop composition,
// T-075, adds "user"/"active"-style values) therefore renames constants that
// existing consumers already compile against. The aliases below keep the names
// the client exported before that happened; they are hand-written, are never
// regenerated and must not be removed (a removal breaks callers at compile time).
// compat_test.go pins every exported identifier of the earlier client, so a later
// fragment cannot silently rename another one.

// Work item states, before WorkState constants were prefixed.
const (
	Active    = WorkStateActive
	Backlog   = WorkStateBacklog
	Cancelled = WorkStateCancelled
	Done      = WorkStateDone
	Ready     = WorkStateReady
	Review    = WorkStateReview
)

// Observable event types, before they were prefixed.
const (
	RunCreated              = ObservableEventTypeRunCreated
	RunStateChanged         = ObservableEventTypeRunStateChanged
	StepAttemptStateChanged = ObservableEventTypeStepAttemptStateChanged
	WorkItemCreated         = ObservableEventTypeWorkItemCreated
	WorkItemTransitioned    = ObservableEventTypeWorkItemTransitioned
	WorkItemUpdated         = ObservableEventTypeWorkItemUpdated
)

// Actor types of run records, before the unprefixed names were taken by the
// dependency actor type.
const (
	RunsActorTypeOperator = Operator
	RunsActorTypeService  = Service
	RunsActorTypeUser     = User
	RunsActorTypeWorkload = Workload
)

// Workspace states, before the unprefixed names were taken.
const (
	WorkspaceStateArchived     = Archived
	WorkspaceStateAvailable    = Available
	WorkspaceStateProvisioning = Provisioning
)
