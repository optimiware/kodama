package client

// Source compatibility of the generated client.
//
// oapi-codegen prefixes an enum constant with its type name only when two enums
// of the document would otherwise declare the same constant (T-170 configures it
// to always prefix, which makes the generated names stable). Adding a fragment
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

// Enum constants the generator used to emit unprefixed. The generator is
// configured to prefix every enum constant with its type name
// (oapi-codegen.yaml), so the names no longer depend on which other enums of
// the document share a value; these aliases keep the earlier names compiling.

// ArtifactDownloadMethod.
const (
	GET = ArtifactDownloadMethodGET
)

// ArtifactRole.
const (
	Bundle     = ArtifactRoleBundle
	Diagnostic = ArtifactRoleDiagnostic
	Output     = ArtifactRoleOutput
)

// ArtifactState.
const (
	Registered = ArtifactStateRegistered
	Reserved   = ArtifactStateReserved
	Usable     = ArtifactStateUsable
)

// ConformanceStatus.
const (
	Blocked  = ConformanceStatusBlocked
	Verified = ConformanceStatusVerified
)

// EnvironmentKind.
const (
	Preview = EnvironmentKindPreview
	Stage   = EnvironmentKindStage
)

// HistoryEntryKind.
const (
	Created      = HistoryEntryKindCreated
	Transitioned = HistoryEntryKindTransitioned
	Updated      = HistoryEntryKindUpdated
)

// LogMetaType.
const (
	Meta = LogMetaTypeMeta
)

// LogSummaryType.
const (
	Summary = LogSummaryTypeSummary
)

// ObservedEventSchemaVersion.
const (
	N1 = ObservedEventSchemaVersionN1
)

// PinnedInputSource.
const (
	Default         = PinnedInputSourceDefault
	ProjectVariable = PinnedInputSourceProjectVariable
	RunInput        = PinnedInputSourceRunInput
)

// PipelineDocumentApiVersion.
const (
	Kodamav1alpha1 = PipelineDocumentApiVersionKodamav1alpha1
)

// PipelineDocumentKind.
const (
	Pipeline = PipelineDocumentKindPipeline
)

// RequiredEvidenceRole.
const (
	Evidence = RequiredEvidenceRoleEvidence
	Report   = RequiredEvidenceRoleReport
	Support  = RequiredEvidenceRoleSupport
)

// RetentionReportBackupRetention.
const (
	Unresolved = RetentionReportBackupRetentionUnresolved
)

// Role.
const (
	Admin    = RoleAdmin
	Analyst  = RoleAnalyst
	Engineer = RoleEngineer
	Reviewer = RoleReviewer
	Viewer   = RoleViewer
)

// SecretDelivery.
const (
	Env  = SecretDeliveryEnv
	File = SecretDeliveryFile
)

// VariableType.
const (
	Boolean = VariableTypeBoolean
	Integer = VariableTypeInteger
	String  = VariableTypeString
)

// WorkKind.
const (
	Defect = WorkKindDefect
	Epic   = WorkKindEpic
	Story  = WorkKindStory
	Task   = WorkKindTask
)

// Enum constants that were unprefixed in the client generated before the
// kernel Git document (T-170), restored as typed aliases of their prefixed names.

// FailedReason.
const (
	Exhausted = FailedReasonExhausted
	Permanent = FailedReasonPermanent
	Refused   = FailedReasonRefused
)

// FailureClass.
const (
	BadResponse                 = FailureClassBadResponse
	ConnectionLost              = FailureClassConnectionLost
	CredentialSourceUnavailable = FailureClassCredentialSourceUnavailable
	CredentialUnavailable       = FailureClassCredentialUnavailable
	DestinationChanged          = FailureClassDestinationChanged
	Internal                    = FailureClassInternal
	LeaseExpired                = FailureClassLeaseExpired
	PolicyRefused               = FailureClassPolicyRefused
	RateLimited                 = FailureClassRateLimited
	ReceiverError               = FailureClassReceiverError
	RedirectRefused             = FailureClassRedirectRefused
	Rejected                    = FailureClassRejected
	Timeout                     = FailureClassTimeout
	Unreachable                 = FailureClassUnreachable
)

// PipelineDocumentApiVersion.
const (
	Kodamav1alpha2 = PipelineDocumentApiVersionKodamav1alpha2
)

// RunsActorType.
const (
	Operator = RunsActorTypeOperator
	Service  = RunsActorTypeService
	User     = RunsActorTypeUser
	Workload = RunsActorTypeWorkload
)

// WorkspaceState.
const (
	Archived     = WorkspaceStateArchived
	Available    = WorkspaceStateAvailable
	Provisioning = WorkspaceStateProvisioning
)
