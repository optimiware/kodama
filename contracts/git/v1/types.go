package gitv1

// Every DTO writes every member. A nullable member is a pointer without
// omitempty and is present as an explicit null; a member documented as optional
// is a pointer with omitempty and is omitted, never null. Identifiers are opaque
// (domain.IDPattern); commits and object IDs are lowercase hexadecimal.

// Hosting says who holds a repository's Git data.
type Hosting string

// Hosting values.
const (
	HostingInternal Hosting = "internal"
	HostingExternal Hosting = "external"
)

// RepositoryStatus is a repository's lifecycle state. Transitions:
// provisioning to ready, ready to archived, archived to ready. A provisioning
// failure is a Reason on a provisioning repository, never another status.
type RepositoryStatus string

// Repository statuses.
const (
	StatusProvisioning RepositoryStatus = "provisioning"
	StatusReady        RepositoryStatus = "ready"
	StatusArchived     RepositoryStatus = "archived"
)

// RepositoryPolicy is a repository's policy as stored and returned.
type RepositoryPolicy struct {
	DirectPushToDefault bool `json:"directPushToDefault"`
}

// RepositoryPolicyInput is the policy of a create or patch body. An omitted
// directPushToDefault is false on creation (O-33); a patch that names a policy
// must state it.
type RepositoryPolicyInput struct {
	DirectPushToDefault *bool `json:"directPushToDefault,omitempty"`
}

// Repository is the repository resource (P/repositories/{id}). Empty is
// reported by the tenant's worker (no refs at all); PostgreSQL stores the last
// report. Reason is a stable code, non-null only while provisioning has not
// completed or after an archive.
type Repository struct {
	ID            string           `json:"id"`
	Name          string           `json:"name"`
	DefaultBranch string           `json:"defaultBranch"`
	Hosting       Hosting          `json:"hosting"`
	Status        RepositoryStatus `json:"status"`
	Reason        *string          `json:"reason"`
	CloneURL      string           `json:"cloneUrl"`
	Policy        RepositoryPolicy `json:"policy"`
	Empty         bool             `json:"empty"`
	Version       int64            `json:"version"`
	CreatedAt     Timestamp        `json:"createdAt"`
}

// CreateRepositoryRequest is the body of POST P/repositories (idempotent on
// name; Idempotency-Key is an HTTP header). DefaultBranch defaults to
// DefaultBranchName, Policy to the all-false policy.
type CreateRepositoryRequest struct {
	Name          string                 `json:"name"`
	DefaultBranch *string                `json:"defaultBranch,omitempty"`
	Policy        *RepositoryPolicyInput `json:"policy,omitempty"`
}

// PatchRepositoryRequest is the body of PATCH P/repositories/{id} under If-Match.
// At least one member is present.
type PatchRepositoryRequest struct {
	DefaultBranch *string                `json:"defaultBranch,omitempty"`
	Policy        *RepositoryPolicyInput `json:"policy,omitempty"`
}

// RepositoryList is one page of P/repositories.
type RepositoryList struct {
	Items      []Repository `json:"items"`
	NextCursor *string      `json:"nextCursor"`
}

// RefTip is the current tip of one ref.
type RefTip struct {
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
}

// RefsResponse is one page of refs (the public GET P/repositories/{id}/refs
// and the worker's ref listing). Empty is true when the repository has no refs
// at all.
type RefsResponse struct {
	Refs       []RefTip `json:"refs"`
	NextCursor *string  `json:"nextCursor"`
	Empty      bool     `json:"empty"`
}

// ActorType is the kind of principal of a Git grant or push.
type ActorType string

// Actor types.
const (
	ActorUser     ActorType = "user"
	ActorWorkload ActorType = "workload"
)

// Actor is the principal a grant was issued to or a push was made by. For a
// workload, ID is the workload identity (the attempt ID for an attempt,
// seed or integration grant, the preparation resource's workload for a
// preparation grant), RunID is the run and AttemptID the attempt (null for a
// preparation workload). For a user both are null.
type Actor struct {
	Type      ActorType `json:"type"`
	ID        string    `json:"id"`
	RunID     *string   `json:"runId"`
	AttemptID *string   `json:"attemptId"`
}

// GrantKind says what a Git grant is for.
type GrantKind string

// Grant kinds.
const (
	GrantHuman       GrantKind = "human"
	GrantSeed        GrantKind = "seed"
	GrantPreparation GrantKind = "preparation"
	GrantAttempt     GrantKind = "attempt"
	GrantIntegration GrantKind = "integration"
)

// IntegrationConstraint is the single update an integration grant permits:
// Ref from From to To, once. GateID names the gate that approved To.
type IntegrationConstraint struct {
	Ref    string `json:"ref"`
	From   string `json:"from"`
	To     string `json:"to"`
	GateID string `json:"gateId"`
}

// GrantRepository is one repository a grant names. Update lists the ref
// patterns the grant may update; Seed permits creating the default branch of an
// empty repository; Integration constrains an integration grant to one update.
type GrantRepository struct {
	ID          string                 `json:"id"`
	Read        bool                   `json:"read"`
	Update      []string               `json:"update"`
	Seed        bool                   `json:"seed"`
	Integration *IntegrationConstraint `json:"integration"`
}

// GrantClaims is the server's record of a Git grant; the credential is opaque.
// Every grant names its tenant, project and repositories explicitly (GR-06). A
// human grant names exactly the one project and repository the credential was
// requested for (the credential helper knows them from the URL path), with read
// access; its recorded Update is the maximum class set (HumanUpdateCeiling, or
// empty for a read-only person) and the person's current project roles and the
// ref policy further restrict every update at use time. Workload grants carry
// the exact patterns the run's access allows.
type GrantClaims struct {
	GrantID      string            `json:"grantId"`
	Audience     string            `json:"audience"`
	TenantID     string            `json:"tenantId"`
	ProjectID    string            `json:"projectId"`
	Actor        Actor             `json:"actor"`
	Kind         GrantKind         `json:"kind"`
	Repositories []GrantRepository `json:"repositories"`
	IssuedAt     Timestamp         `json:"issuedAt"`
	ExpiresAt    Timestamp         `json:"expiresAt"`
}

// CredentialRequest is the body of POST /tenants/{tenantId}/git-credentials: the
// project and repository the credential is for, and whether it is the
// single-use seed grant of an empty repository (false: an ordinary human
// credential). The Git credential helper derives both IDs from the URL path
// (credential.useHttpPath=true), so a credential never spans repositories.
type CredentialRequest struct {
	ProjectID    string `json:"projectId"`
	RepositoryID string `json:"repositoryId"`
	Seed         bool   `json:"seed"`
}

// CredentialResponse carries the credential: HTTP Basic with Username and
// Password (any username is accepted at use time; the one returned is the
// conventional one), or Password as a bearer token. Password is opaque and
// secret; it is shown once.
type CredentialResponse struct {
	Username  string    `json:"username"`
	Password  string    `json:"password"`
	GrantID   string    `json:"grantId"`
	ExpiresAt Timestamp `json:"expiresAt"`
}

// ForwardingAssertion is the claim set of the signed token the Kodama server
// sends with an authorized request to the tenant's worker. It never contains
// the caller's credential.
type ForwardingAssertion struct {
	Audience     string    `json:"audience"`
	TenantID     string    `json:"tenantId"`
	RepositoryID string    `json:"repositoryId"`
	GrantID      string    `json:"grantId"`
	ReceivePack  bool      `json:"receivePack"`
	IssuedAt     Timestamp `json:"issuedAt"`
	ExpiresAt    Timestamp `json:"expiresAt"`
	RequestID    string    `json:"requestId"`
}

// WorkerIdentity is the claim set of a worker's tenant-bound identity for the
// control plane's internal routes.
type WorkerIdentity struct {
	Audience       string    `json:"audience"`
	TenantID       string    `json:"tenantId"`
	WorkerInstance string    `json:"workerInstance"`
	IssuedAt       Timestamp `json:"issuedAt"`
	ExpiresAt      Timestamp `json:"expiresAt"`
}

// RefUpdate is one intended or applied ref update. An all-zero Old is a create,
// an all-zero New a delete.
type RefUpdate struct {
	Ref string `json:"ref"`
	Old string `json:"old"`
	New string `json:"new"`
}

// GitFact is a fact only Git can supply, read by the worker so that the control
// plane never calls Git while holding SQL locks. FastForward is null for a
// create or delete. RepositoryEmpty is true when the repository had no refs when
// the receive began; it is the same in every fact of one request.
type GitFact struct {
	Ref             string `json:"ref"`
	FastForward     *bool  `json:"fastForward"`
	RepositoryEmpty bool   `json:"repositoryEmpty"`
}

// PushAuthorizeRequest is sent by pre-receive (POST ...pushes). ReceiveID is
// generated by the worker once per receive operation and makes every hook call
// idempotent. PushOptions are recorded and never interpreted as authority.
type PushAuthorizeRequest struct {
	RepositoryID string      `json:"repositoryId"`
	GrantID      string      `json:"grantId"`
	ReceiveID    string      `json:"receiveId"`
	Updates      []RefUpdate `json:"updates"`
	PushOptions  []string    `json:"pushOptions"`
	GitFacts     []GitFact   `json:"gitFacts"`
}

// Verdict is the control plane's decision on one update.
type Verdict string

// Verdicts.
const (
	VerdictAllow Verdict = "allow"
	VerdictDeny  Verdict = "deny"
)

// UpdateVerdict is the verdict on one update. Reason is null for allow and one
// of the push refusal reasons for deny.
type UpdateVerdict struct {
	Ref     string  `json:"ref"`
	Verdict Verdict `json:"verdict"`
	Reason  *string `json:"reason"`
}

// PushAuthorizeResponse creates (or, on a repeated receiveId, returns) the push
// record. Deadline is the receive deadline the worker enforces by terminating an
// overrunning receive. Any deny fails the whole receive.
type PushAuthorizeResponse struct {
	PushID   string          `json:"pushId"`
	Deadline Timestamp       `json:"deadline"`
	Updates  []UpdateVerdict `json:"updates"`
}

// CommitIntentRequest is sent by the prepared reference-transaction hook for the
// ref transaction's updates, with the refs locked and the prepared journal entry
// already durable.
type CommitIntentRequest struct {
	ReceiveID string      `json:"receiveId"`
	Updates   []RefUpdate `json:"updates"`
}

// CommitIntentResponse is the 200 answer: Committing is always true. A refusal is
// a 409 error with code push_settled, attempt_inactive or receive_expired, after
// which the hook exits non-zero so Git aborts the transaction.
type CommitIntentResponse struct {
	Committing bool `json:"committing"`
}

// OutcomeRequest is sent by post-receive with the updates that were applied
// (possibly none).
//
// JournalSequence is optional and omitted, never null: the worker's highest
// journal sequence at or below which every receive of the repository has been
// reported, so the control plane can acknowledge it (repositories.journal_ack).
// The worker omits it while another receive may still be unreported.
//
// JournalPosition is optional and omitted, never null: the journal sequence of
// the receive's own first entry (T-168). A receive appends its prepared entries
// while it holds the locks of every ref it updates, so two receives that update a
// common ref are ordered by this position exactly as their updates were applied,
// which neither the time an outcome is reported nor the acknowledgement above
// says (a lost report is recovered later than newer receives). A re-report of the
// same receive carries the same value. It is omitted only when the worker cannot
// read its journal.
type OutcomeRequest struct {
	ReceiveID       string      `json:"receiveId"`
	Applied         []RefUpdate `json:"applied"`
	JournalSequence *int64      `json:"journalSequence,omitempty"`
	JournalPosition *int64      `json:"journalPosition,omitempty"`
}

// OutcomeResponse reports the record after the outcome is recorded.
type OutcomeResponse struct {
	PushID string     `json:"pushId"`
	Status PushStatus `json:"status"`
}

// InitializedRequest is the worker's confirmation that a repository was
// initialized (POST ...repositories/{id}/initialized). Empty is true when it
// has no refs.
type InitializedRequest struct {
	RepositoryID  string `json:"repositoryId"`
	DefaultBranch string `json:"defaultBranch"`
	Empty         bool   `json:"empty"`
}

// InitializedResponse reports the repository after the confirmation.
type InitializedResponse struct {
	RepositoryID string           `json:"repositoryId"`
	Status       RepositoryStatus `json:"status"`
	Version      int64            `json:"version"`
}

// AssignmentKind is work the control plane hands a worker.
type AssignmentKind string

// Assignment kinds.
const (
	AssignInitialize AssignmentKind = "initialize"
	AssignArchive    AssignmentKind = "archive"
	AssignMaintain   AssignmentKind = "maintain"
	AssignReconcile  AssignmentKind = "reconcile"
)

// Assignment is one repository the worker should initialize, archive, maintain
// or reconcile. Repeated polls repeat an assignment until its completion is
// confirmed.
//
// JournalAck is optional and omitted, never null: for a reconcile assignment, the
// journal sequence the control plane has acknowledged (repositories.journal_ack),
// so the worker re-reports every receive of its journal after it.
type Assignment struct {
	Kind          AssignmentKind `json:"kind"`
	RepositoryID  string         `json:"repositoryId"`
	DefaultBranch string         `json:"defaultBranch"`
	JournalAck    *int64         `json:"journalAck,omitempty"`
}

// AssignmentsResponse is the answer of GET ...assignments.
type AssignmentsResponse struct {
	Items []Assignment `json:"items"`
}

// EntryState is the state of one ref entry of a push record.
type EntryState string

// Entry states. authorized and committing are in flight; applied and refused
// are final; unknown waits for an operator.
const (
	EntryAuthorized EntryState = "authorized"
	EntryCommitting EntryState = "committing"
	EntryApplied    EntryState = "applied"
	EntryRefused    EntryState = "refused"
	EntryUnknown    EntryState = "unknown"
)

// PushStatus is a push record's status, derived from its entries (DeriveStatus).
type PushStatus string

// Push statuses.
const (
	PushAuthorized PushStatus = "authorized"
	PushApplied    PushStatus = "applied"
	PushRefused    PushStatus = "refused"
	PushUnknown    PushStatus = "unknown"
)

// PushEntry is one ref update of a push record with its own state. Kind is
// persisted at authorization from the worker's git facts (NewPushEntries) and
// never changes: create (all-zero old), delete (all-zero new), or, for an update
// of an existing ref to an existing commit, fast_forward or rewrite as the worker
// reported. A consumer that reloads the record after the reference-only
// git.refs_updated event reads each ref's classification from here. Reason is
// null unless the entry is refused or unknown.
type PushEntry struct {
	Ref    string     `json:"ref"`
	Old    string     `json:"old"`
	New    string     `json:"new"`
	Kind   UpdateKind `json:"kind"`
	State  EntryState `json:"state"`
	Reason *string    `json:"reason"`
}

// PushRecord is the durable intent and outcome of one receive operation, keyed
// by ReceiveID. OriginRunID is the run that made the push (the actor's run), null
// for a person.
type PushRecord struct {
	PushID       string      `json:"pushId"`
	RepositoryID string      `json:"repositoryId"`
	ReceiveID    string      `json:"receiveId"`
	Actor        Actor       `json:"actor"`
	GrantID      string      `json:"grantId"`
	OriginRunID  *string     `json:"originRunId"`
	Status       PushStatus  `json:"status"`
	Entries      []PushEntry `json:"entries"`
	CreatedAt    Timestamp   `json:"createdAt"`
}

// PushRecordList is one page of P/repositories/{id}/pushes.
type PushRecordList struct {
	Items      []PushRecord `json:"items"`
	NextCursor *string      `json:"nextCursor"`
}

// JournalState is the state a journal entry records.
type JournalState string

// Journal states.
const (
	JournalPrepared  JournalState = "prepared"
	JournalCommitted JournalState = "committed"
	JournalAborted   JournalState = "aborted"
)

// JournalEntry is one record of the per-repository ref journal on the tenant
// volume. Exactly one of ReceiveID and OperationID is set (a receive or a
// server operation). Observed is the ref's value read while the prepared hook
// holds the lock (all-zero when the ref is absent); it is set for prepared
// entries and null for committed and aborted entries. Sequence is the
// repository-wide append order, from 1.
type JournalEntry struct {
	ReceiveID   *string      `json:"receiveId"`
	OperationID *string      `json:"operationId"`
	Ref         string       `json:"ref"`
	Old         string       `json:"old"`
	New         string       `json:"new"`
	Observed    *string      `json:"observed"`
	State       JournalState `json:"state"`
	Sequence    int64        `json:"sequence"`
}

// InitializeRepositoryRequest asks a worker to initialize a bare repository at
// the path it derives from RepositoryID. Idempotent on the repository ID.
type InitializeRepositoryRequest struct {
	RepositoryID  string `json:"repositoryId"`
	DefaultBranch string `json:"defaultBranch"`
}

// InitializeRepositoryResponse reports the repository: Created is false when it
// already existed, Empty whether it has no refs.
type InitializeRepositoryResponse struct {
	RepositoryID  string `json:"repositoryId"`
	DefaultBranch string `json:"defaultBranch"`
	Empty         bool   `json:"empty"`
	Created       bool   `json:"created"`
}

// ReadRefsRequest lists refs under one namespace: refs/heads/, refs/tags/ or
// refs/kodama/ (the last for the control plane only).
type ReadRefsRequest struct {
	RepositoryID string  `json:"repositoryId"`
	Prefix       string  `json:"prefix"`
	Cursor       *string `json:"cursor"`
	Limit        int64   `json:"limit"`
}

// ReadRefValuesRequest reads the current value of exact refs.
type ReadRefValuesRequest struct {
	RepositoryID string   `json:"repositoryId"`
	Refs         []string `json:"refs"`
}

// RefValue is the current value of one ref; Commit is null when it is absent.
type RefValue struct {
	Ref    string  `json:"ref"`
	Commit *string `json:"commit"`
}

// ReadRefValuesResponse carries the values. Quiescent is true only when they were
// read under the worker's per-repository mutation lock with no receive or server
// operation in progress: only then is a value a consistent boundary for
// settlement. JournalHead is the highest journal sequence at the read.
type ReadRefValuesResponse struct {
	Values         []RefValue `json:"values"`
	Quiescent      bool       `json:"quiescent"`
	JournalHead    int64      `json:"journalHead"`
	WorkerInstance string     `json:"workerInstance"`
}

// ReadJournalRequest reads journal entries with a sequence greater than
// SinceSequence, either those of one receive or those of one ref (every
// attribution). Exactly one of ReceiveID and Ref is set.
type ReadJournalRequest struct {
	RepositoryID  string  `json:"repositoryId"`
	ReceiveID     *string `json:"receiveId"`
	Ref           *string `json:"ref"`
	SinceSequence int64   `json:"sinceSequence"`
	Limit         int64   `json:"limit"`
}

// ReadJournalResponse returns entries in strictly increasing sequence order.
// HeadSequence is the highest sequence in the journal; HasMore is true when the
// page was cut by the limit.
type ReadJournalResponse struct {
	Entries        []JournalEntry `json:"entries"`
	HeadSequence   int64          `json:"headSequence"`
	HasMore        bool           `json:"hasMore"`
	WorkerInstance string         `json:"workerInstance"`
}

// ReceiveLivenessResponse is the answer of GET .../receives/{receiveId}. Running
// is true while the worker still runs the receive. A receive the worker never
// heard of is not running. WorkerInstance identifies the answering instance so
// the control plane can tell a replacement from the original.
type ReceiveLivenessResponse struct {
	ReceiveID      string `json:"receiveId"`
	Running        bool   `json:"running"`
	WorkerInstance string `json:"workerInstance"`
}

// SetRetainedRefRequest creates the hidden ref refs/kodama/runs/<runId>/<n> at
// Commit. Idempotent on OperationID; a different existing value is a conflict.
type SetRetainedRefRequest struct {
	RepositoryID string `json:"repositoryId"`
	OperationID  string `json:"operationId"`
	Ref          string `json:"ref"`
	Commit       string `json:"commit"`
}

// SetRetainedRefResponse reports the ref; Created is false on a repeat.
type SetRetainedRefResponse struct {
	Ref     string `json:"ref"`
	Commit  string `json:"commit"`
	Created bool   `json:"created"`
}

// DeleteRetainedRefRequest deletes a retained ref whose value is
// ExpectedCommit. Idempotent on OperationID.
type DeleteRetainedRefRequest struct {
	RepositoryID   string `json:"repositoryId"`
	OperationID    string `json:"operationId"`
	Ref            string `json:"ref"`
	ExpectedCommit string `json:"expectedCommit"`
}

// DeleteRetainedRefResponse reports the deletion; Deleted is false when the ref
// was already absent.
type DeleteRetainedRefResponse struct {
	Ref     string `json:"ref"`
	Deleted bool   `json:"deleted"`
}

// DeleteBranchRequest prunes a managed branch (task, agent or run branch) whose
// value is ExpectedCommit. The control plane sends it only after the branch's
// commits are reachable from a retained ref. Idempotent on OperationID.
type DeleteBranchRequest struct {
	RepositoryID   string `json:"repositoryId"`
	OperationID    string `json:"operationId"`
	Ref            string `json:"ref"`
	ExpectedCommit string `json:"expectedCommit"`
}

// DeleteBranchResponse reports the deletion.
type DeleteBranchResponse struct {
	Ref     string `json:"ref"`
	Deleted bool   `json:"deleted"`
}

// CreateBranchRequest creates a managed branch (a task, agent or run branch,
// never the default branch, a tag or a server ref) at Commit. The worker
// records the creation in the ref journal under OperationID. Idempotent on
// OperationID and on the ref: the branch already at Commit is success, at
// another commit the branch_exists conflict.
type CreateBranchRequest struct {
	RepositoryID string `json:"repositoryId"`
	OperationID  string `json:"operationId"`
	Ref          string `json:"ref"`
	Commit       string `json:"commit"`
}

// CreateBranchResponse reports the branch; Created is false when it already
// existed at Commit.
type CreateBranchResponse struct {
	Ref     string `json:"ref"`
	Commit  string `json:"commit"`
	Created bool   `json:"created"`
}

// MaintainRequest runs git maintenance on a repository, serialized with pushes.
// Idempotent on OperationID.
type MaintainRequest struct {
	RepositoryID string `json:"repositoryId"`
	OperationID  string `json:"operationId"`
}

// MaintainResponse reports the completed run.
type MaintainResponse struct {
	OperationID string    `json:"operationId"`
	CompletedAt Timestamp `json:"completedAt"`
}

// SourceAccess is a source's access in a run.
type SourceAccess string

// Source accesses.
const (
	AccessRead  SourceAccess = "read"
	AccessWrite SourceAccess = "write"
)

// Source is one repository of a run's source set: the pinned commit, the
// checkout path relative to /workspace, the access the run declared and, for a
// write source, the branch its writer updates (null for a read source).
type Source struct {
	RepositoryID string       `json:"repositoryId"`
	Commit       string       `json:"commit"`
	Path         string       `json:"path"`
	Access       SourceAccess `json:"access"`
	Branch       *string      `json:"branch"`
}

// SourceSet is a run's pinned source set, ordered by path.
type SourceSet struct {
	Sources []Source `json:"sources"`
}

// TriggerConfig selects the pushes a pipeline reacts to. ExcludeOriginRuns is
// optional and defaults to true: a push made by a run does not trigger it.
type TriggerConfig struct {
	TriggerID         string   `json:"triggerId"`
	RepositoryID      string   `json:"repositoryId"`
	RefPatterns       []string `json:"refPatterns"`
	ExcludeOriginRuns *bool    `json:"excludeOriginRuns,omitempty"`
	DefinitionID      string   `json:"definitionId"`
	Enabled           bool     `json:"enabled"`
}

// ErrorResponse is the error body of every route of this contract, the shape of
// the runtime and HTTP contracts, with codes from Status.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}
