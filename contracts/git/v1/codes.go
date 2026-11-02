package gitv1

import (
	"net/http"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Codes new in this contract. Every shared code keeps its meaning and status.
const (
	// Repository management and bootstrap.
	CodeRepositoryNameTaken domain.ErrorCode = "repository_name_taken" // 409
	CodeRepositoryNotReady  domain.ErrorCode = "repository_not_ready"  // 409
	CodeRepositoryArchived  domain.ErrorCode = "repository_archived"   // 409
	CodeHostingMismatch     domain.ErrorCode = "hosting_mismatch"      // 422
	CodeRepositoryNotEmpty  domain.ErrorCode = "repository_not_empty"  // 409: seed for a repository with any ref
	CodeSeedPending         domain.ErrorCode = "seed_pending"          // 409: an earlier seed push record is unsettled

	// Commit intent refusals (worker interface); the hook aborts the transaction.
	CodePushSettled     domain.ErrorCode = "push_settled"     // 409: the record was already settled
	CodeAttemptInactive domain.ErrorCode = "attempt_inactive" // 409: the grant's attempt is no longer live
	CodeRunInactive     domain.ErrorCode = "run_inactive"     // 409: the credential's run is no longer live, or the credential was revoked (kernel, T-167)
	CodeReceiveExpired  domain.ErrorCode = "receive_expired"  // 409: the receive deadline passed

	// Worker operations.
	CodeRefChanged        domain.ErrorCode = "ref_changed"         // 409: the ref no longer has the expected value
	CodeRetainedRefExists domain.ErrorCode = "retained_ref_exists" // 409: a retained ref exists at another commit
	CodeBranchExists      domain.ErrorCode = "branch_exists"       // 409: the branch exists at another commit

	// CodeJournalUnreadable is the definite answer of the journal read when the
	// worker affirmatively found the repository's ref journal corrupt, undecodable
	// or missing where it must exist. It is distinct from the transient 503
	// unavailable (nothing could be read now; retry): this one is a verdict about
	// the journal itself, and the control plane settles the affected entries
	// unknown with the reason journal_unreadable.
	CodeJournalUnreadable domain.ErrorCode = "journal_unreadable" // 409

	// Object operations (T-160).
	CodeCommitNotFound       domain.ErrorCode = "commit_not_found"       // 404: the repository exists but lacks the commit
	CodeRepositoryEmpty      domain.ErrorCode = "repository_empty"       // 409: a bundle of a repository without refs
	CodeBundleTooLarge       domain.ErrorCode = "bundle_too_large"       // 422: the bundle exceeds the installation's size or ref bound
	CodeRestoreFenceRequired domain.ErrorCode = "restore_fence_required" // 409: a bundle replay while the repository is not fenced for restore

	// Push limits (GR-16).
	CodeTooManyUpdates domain.ErrorCode = "too_many_updates" // 422: the receive exceeds the installation's update bound
)

var statuses = map[domain.ErrorCode]int{
	domain.CodeInvalidRequest:      http.StatusBadRequest,
	domain.CodeInvalidCredential:   http.StatusUnauthorized,
	domain.CodeForbidden:           http.StatusForbidden,
	domain.CodeNotFound:            http.StatusNotFound,
	domain.CodeIdempotencyConflict: http.StatusConflict,
	domain.CodeVersionConflict:     http.StatusPreconditionFailed,
	domain.CodeVersionRequired:     http.StatusPreconditionRequired,
	domain.CodeRateLimited:         http.StatusTooManyRequests,
	domain.CodeUnavailable:         http.StatusServiceUnavailable,
	CodeRepositoryNameTaken:        http.StatusConflict,
	CodeRepositoryNotReady:         http.StatusConflict,
	CodeRepositoryArchived:         http.StatusConflict,
	CodeHostingMismatch:            http.StatusUnprocessableEntity,
	CodeRepositoryNotEmpty:         http.StatusConflict,
	CodeSeedPending:                http.StatusConflict,
	CodePushSettled:                http.StatusConflict,
	CodeAttemptInactive:            http.StatusConflict,
	CodeRunInactive:                http.StatusConflict,
	CodeReceiveExpired:             http.StatusConflict,
	CodeRefChanged:                 http.StatusConflict,
	CodeRetainedRefExists:          http.StatusConflict,
	CodeBranchExists:               http.StatusConflict,
	CodeJournalUnreadable:          http.StatusConflict,
	CodeCommitNotFound:             http.StatusNotFound,
	CodeRepositoryEmpty:            http.StatusConflict,
	CodeBundleTooLarge:             http.StatusUnprocessableEntity,
	CodeRestoreFenceRequired:       http.StatusConflict,
	CodeTooManyUpdates:             http.StatusUnprocessableEntity,
}

// Status returns the HTTP status of a code and whether the code is known.
func Status(code domain.ErrorCode) (int, bool) {
	s, ok := statuses[code]
	return s, ok
}

// ContractError is a contract violation carrying a stable code (the runtime v1
// type, so a consumer handles one error type).
type ContractError = runtimev1.ContractError

// ErrorBody is the runtime v1 error body.
type ErrorBody = runtimev1.ErrorBody

// Reasons name why an update was refused or left unknown. They are stable codes
// stored on push entries and returned to the client on stderr.
const (
	// Push refusal reasons (a deny verdict, an entry settled refused by the
	// control plane's own decision).
	ReasonRefProtected    = "ref_protected"
	ReasonNotFastForward  = "not_fast_forward"
	ReasonGrantScope      = "grant_scope"
	ReasonAttemptInactive = "attempt_inactive"
	// ReasonRunInactive is the kernel's form of attempt_inactive: the run the
	// credential belongs to is no longer queued or running (T-167, O-36).
	ReasonRunInactive         = "run_inactive"
	ReasonRepositoryArchived  = "repository_archived"
	ReasonSeedOnly            = "seed_only"
	ReasonSeedConsumed        = "seed_consumed"
	ReasonIntegrationMismatch = "integration_mismatch"

	// Settlement refusal reasons: the entry never changed its ref.
	ReasonNoCommitIntent = "no_commit_intent" // refused while still authorized
	ReasonReceiveAborted = "receive_aborted"  // the transaction's own aborted entry
	ReasonRefUnchanged   = "ref_unchanged"    // the ref's next value shows the transaction did not commit

	// Settlement unknown reasons: the evidence rules cannot decide; an operator
	// resolves the entry.
	ReasonJournalUnreadable   = "journal_unreadable"
	ReasonJournalRegressed    = "journal_regressed" // older than the control plane already saw: a restore
	ReasonEvidenceMissing     = "evidence_missing"
	ReasonConflictingEvidence = "conflicting_evidence"
	ReasonRefDiverged         = "ref_diverged" // the ref has neither the old nor the new value
	ReasonOperatorRequired    = "operator_required"

	// Wait reasons: retry reconciliation later.
	ReasonReceiveRunning      = "receive_running"
	ReasonLivenessUnavailable = "liveness_unavailable"
	ReasonRefNotQuiescent     = "ref_not_quiescent"
)

var (
	pushRefusalReasons = []string{ReasonRefProtected, ReasonNotFastForward, ReasonGrantScope, ReasonAttemptInactive, ReasonRunInactive,
		ReasonRepositoryArchived, ReasonSeedOnly, ReasonSeedConsumed, ReasonIntegrationMismatch}
	settlementRefusalReasons = []string{ReasonNoCommitIntent, ReasonReceiveAborted, ReasonRefUnchanged}
	unknownReasons           = []string{ReasonJournalUnreadable, ReasonJournalRegressed, ReasonEvidenceMissing,
		ReasonConflictingEvidence, ReasonRefDiverged, ReasonOperatorRequired}
)

// PushRefusalReasons lists the reasons a push verdict may deny with.
func PushRefusalReasons() []string { return append([]string{}, pushRefusalReasons...) }

// IsPushRefusalReason reports whether code is a push refusal reason.
func IsPushRefusalReason(code string) bool { return contains(pushRefusalReasons, code) }

// IsRefusedEntryReason reports whether code may be the reason of a refused
// entry: a push refusal reason or a settlement refusal reason.
func IsRefusedEntryReason(code string) bool {
	return contains(pushRefusalReasons, code) || contains(settlementRefusalReasons, code)
}

// IsUnknownEntryReason reports whether code may be the reason of an unknown entry.
func IsUnknownEntryReason(code string) bool { return contains(unknownReasons, code) }

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// Caller is the principal family of a route.
type Caller string

// Callers.
const (
	CallerPublic  Caller = "public"  // a person or workload with an access token; paths are relative to the HTTP contract's tenant root
	CallerWorker  Caller = "worker"  // a worker with its identity; paths relative to ControlBasePath
	CallerControl Caller = "control" // the control plane; paths relative to WorkerBasePath
)

// Operation describes one route of the contract. Request and the Responses
// values name DTO types ("" means no body). Errors lists the codes the route may
// return in addition to the transport ones (invalid_credential, unavailable).
// Path parameters ({repositoryId}, {pushId}, {receiveId}) are not part of any body.
type Operation struct {
	ID        string
	Caller    Caller
	Method    string
	Path      string
	Request   string
	Responses map[int]string
	Errors    []domain.ErrorCode
}

// Operations is the route table of the Git service contract. The public routes
// are listed for their bodies; their full semantics (pagination, If-Match,
// Idempotency-Key) are in the HTTP contract and are added to the public API by
// the integration task.
var Operations = []Operation{
	{ID: "listRepositories", Caller: CallerPublic, Method: "GET", Path: "/tenants/{tenantId}/projects/{projectId}/repositories", Responses: map[int]string{200: "RepositoryList"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound}},
	{ID: "createRepository", Caller: CallerPublic, Method: "POST", Path: "/tenants/{tenantId}/projects/{projectId}/repositories", Request: "CreateRepositoryRequest",
		Responses: map[int]string{200: "Repository", 201: "Repository"},
		Errors:    []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound, domain.CodeIdempotencyConflict, CodeRepositoryNameTaken}},
	{ID: "getRepository", Caller: CallerPublic, Method: "GET", Path: "/tenants/{tenantId}/projects/{projectId}/repositories/{repositoryId}", Responses: map[int]string{200: "Repository"},
		Errors: []domain.ErrorCode{domain.CodeForbidden, domain.CodeNotFound}},
	{ID: "patchRepository", Caller: CallerPublic, Method: "PATCH", Path: "/tenants/{tenantId}/projects/{projectId}/repositories/{repositoryId}", Request: "PatchRepositoryRequest",
		Responses: map[int]string{200: "Repository"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound, domain.CodeVersionConflict, domain.CodeVersionRequired,
			CodeRepositoryNotReady, CodeRepositoryArchived, CodeHostingMismatch}},
	{ID: "archiveRepository", Caller: CallerPublic, Method: "POST", Path: "/tenants/{tenantId}/projects/{projectId}/repositories/{repositoryId}/archive", Responses: map[int]string{200: "Repository"},
		Errors: []domain.ErrorCode{domain.CodeForbidden, domain.CodeNotFound, CodeRepositoryNotReady, CodeHostingMismatch}},
	{ID: "restoreRepository", Caller: CallerPublic, Method: "POST", Path: "/tenants/{tenantId}/projects/{projectId}/repositories/{repositoryId}/restore", Responses: map[int]string{200: "Repository"},
		Errors: []domain.ErrorCode{domain.CodeForbidden, domain.CodeNotFound, CodeHostingMismatch}},
	{ID: "listRefs", Caller: CallerPublic, Method: "GET", Path: "/tenants/{tenantId}/projects/{projectId}/repositories/{repositoryId}/refs", Responses: map[int]string{200: "RefsResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound, CodeRepositoryNotReady, CodeHostingMismatch, domain.CodeUnavailable}},
	{ID: "listPushes", Caller: CallerPublic, Method: "GET", Path: "/tenants/{tenantId}/projects/{projectId}/repositories/{repositoryId}/pushes", Responses: map[int]string{200: "PushRecordList"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound}},
	{ID: "getPush", Caller: CallerPublic, Method: "GET", Path: "/tenants/{tenantId}/projects/{projectId}/repositories/{repositoryId}/pushes/{pushId}", Responses: map[int]string{200: "PushRecord"},
		Errors: []domain.ErrorCode{domain.CodeForbidden, domain.CodeNotFound}},
	{ID: "exchangeCredential", Caller: CallerPublic, Method: "POST", Path: "/tenants/{tenantId}/git-credentials", Request: "CredentialRequest", Responses: map[int]string{200: "CredentialResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound, CodeRepositoryNotReady, CodeRepositoryArchived,
			CodeHostingMismatch, CodeRepositoryNotEmpty, CodeSeedPending, domain.CodeRateLimited}},

	{ID: "authorizePush", Caller: CallerWorker, Method: "POST", Path: "/pushes", Request: "PushAuthorizeRequest", Responses: map[int]string{200: "PushAuthorizeResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound, domain.CodeIdempotencyConflict, CodeTooManyUpdates}},
	{ID: "commitIntent", Caller: CallerWorker, Method: "POST", Path: "/pushes/{pushId}/commit-intent", Request: "CommitIntentRequest", Responses: map[int]string{200: "CommitIntentResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound, CodePushSettled, CodeAttemptInactive, CodeReceiveExpired}},
	{ID: "pushOutcome", Caller: CallerWorker, Method: "POST", Path: "/pushes/{pushId}/outcome", Request: "OutcomeRequest", Responses: map[int]string{200: "OutcomeResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound}},
	{ID: "repositoryInitialized", Caller: CallerWorker, Method: "POST", Path: "/repositories/{repositoryId}/initialized", Request: "InitializedRequest", Responses: map[int]string{200: "InitializedResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound}},
	{ID: "getAssignments", Caller: CallerWorker, Method: "GET", Path: "/assignments", Responses: map[int]string{200: "AssignmentsResponse"},
		Errors: []domain.ErrorCode{domain.CodeForbidden}},

	{ID: "initializeRepository", Caller: CallerControl, Method: "POST", Path: "/repositories/{repositoryId}/initialize", Request: "InitializeRepositoryRequest", Responses: map[int]string{200: "InitializeRepositoryResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeUnavailable}},
	{ID: "readRefs", Caller: CallerControl, Method: "POST", Path: "/repositories/{repositoryId}/refs/list", Request: "ReadRefsRequest", Responses: map[int]string{200: "RefsResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound, domain.CodeUnavailable}},
	{ID: "readRefValues", Caller: CallerControl, Method: "POST", Path: "/repositories/{repositoryId}/refs/values", Request: "ReadRefValuesRequest", Responses: map[int]string{200: "ReadRefValuesResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound, domain.CodeUnavailable}},
	{ID: "readJournal", Caller: CallerControl, Method: "POST", Path: "/repositories/{repositoryId}/journal", Request: "ReadJournalRequest", Responses: map[int]string{200: "ReadJournalResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound, CodeJournalUnreadable, domain.CodeUnavailable}},
	{ID: "receiveLiveness", Caller: CallerControl, Method: "GET", Path: "/repositories/{repositoryId}/receives/{receiveId}", Responses: map[int]string{200: "ReceiveLivenessResponse"},
		Errors: []domain.ErrorCode{domain.CodeForbidden, domain.CodeNotFound, domain.CodeUnavailable}},
	{ID: "setRetainedRef", Caller: CallerControl, Method: "POST", Path: "/repositories/{repositoryId}/retained-refs/set", Request: "SetRetainedRefRequest", Responses: map[int]string{200: "SetRetainedRefResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound, CodeRetainedRefExists, domain.CodeUnavailable}},
	{ID: "deleteRetainedRef", Caller: CallerControl, Method: "POST", Path: "/repositories/{repositoryId}/retained-refs/delete", Request: "DeleteRetainedRefRequest", Responses: map[int]string{200: "DeleteRetainedRefResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound, CodeRefChanged, domain.CodeUnavailable}},
	{ID: "createBranch", Caller: CallerControl, Method: "POST", Path: "/repositories/{repositoryId}/branches/create", Request: "CreateBranchRequest", Responses: map[int]string{200: "CreateBranchResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound, CodeBranchExists, domain.CodeUnavailable}},
	{ID: "deleteBranch", Caller: CallerControl, Method: "POST", Path: "/repositories/{repositoryId}/branches/delete", Request: "DeleteBranchRequest", Responses: map[int]string{200: "DeleteBranchResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound, CodeRefChanged, domain.CodeUnavailable}},
	{ID: "maintain", Caller: CallerControl, Method: "POST", Path: "/repositories/{repositoryId}/maintenance", Request: "MaintainRequest", Responses: map[int]string{200: "MaintainResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound, domain.CodeUnavailable}},
	{ID: "isAncestor", Caller: CallerControl, Method: "POST", Path: "/repositories/{repositoryId}/objects/is-ancestor", Request: "IsAncestorRequest", Responses: map[int]string{200: "IsAncestorResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound, CodeCommitNotFound, domain.CodeUnavailable}},
	{ID: "hasCommit", Caller: CallerControl, Method: "POST", Path: "/repositories/{repositoryId}/objects/has-commit", Request: "HasCommitRequest", Responses: map[int]string{200: "HasCommitResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound, domain.CodeUnavailable}},
	{ID: "commitTime", Caller: CallerControl, Method: "POST", Path: "/repositories/{repositoryId}/objects/commit-time", Request: "CommitTimeRequest", Responses: map[int]string{200: "CommitTimeResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound, CodeCommitNotFound, domain.CodeUnavailable}},
	{ID: "readFiles", Caller: CallerControl, Method: "POST", Path: "/repositories/{repositoryId}/files", Request: "ReadFilesRequest", Responses: map[int]string{200: "ReadFilesResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound, CodeCommitNotFound, domain.CodeUnavailable}},
	// createBundle answers with BundleMediaType: one JSON line holding the
	// CreateBundleResponse manifest, then the bundle bytes (see FrameLine).
	{ID: "createBundle", Caller: CallerControl, Method: "POST", Path: "/repositories/{repositoryId}/bundles/create", Request: "CreateBundleRequest", Responses: map[int]string{200: "CreateBundleResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound, CodeRepositoryEmpty, CodeBundleTooLarge, domain.CodeRateLimited, domain.CodeUnavailable}},
	// replayBundle takes BundleMediaType: one JSON line holding the
	// ReplayBundleRequest, then the bundle bytes (see FrameLine).
	{ID: "replayBundle", Caller: CallerControl, Method: "POST", Path: "/repositories/{repositoryId}/bundles/replay", Request: "ReplayBundleRequest", Responses: map[int]string{200: "ReplayBundleResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound, CodeRestoreFenceRequired, CodeBundleTooLarge, domain.CodeRateLimited, domain.CodeUnavailable}},
	{ID: "setRestoreFence", Caller: CallerControl, Method: "POST", Path: "/repositories/{repositoryId}/restore-fence", Request: "RestoreFenceRequest", Responses: map[int]string{200: "RestoreFenceResponse"},
		Errors: []domain.ErrorCode{domain.CodeInvalidRequest, domain.CodeForbidden, domain.CodeNotFound, domain.CodeUnavailable}},
}
