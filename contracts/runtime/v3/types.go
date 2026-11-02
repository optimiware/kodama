package runtimev3

import (
	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
)

// Types reused unchanged from v1 and v2. They are aliases, not copies: the
// earlier validators, decoders and canonical rules apply as they are.
type (
	Digest    = runtimev1.Digest
	GitCommit = runtimev1.GitCommit
	Timestamp = runtimev1.Timestamp
	Value     = runtimev1.Value

	AdmitRequest             = runtimev1.AdmitRequest
	TokenInfo                = runtimev1.TokenInfo
	TokenRequest             = runtimev1.TokenRequest
	TokenResponse            = runtimev1.TokenResponse
	SecretResolveRequest     = runtimev1.SecretResolveRequest
	SecretResolveResponse    = runtimev1.SecretResolveResponse
	SecretBinding            = runtimev1.SecretBinding
	ArtifactReserveRequest   = runtimev1.ArtifactReserveRequest
	ArtifactReserveResponse  = runtimev1.ArtifactReserveResponse
	ArtifactCompleteRequest  = runtimev1.ArtifactCompleteRequest
	ArtifactCompleteResponse = runtimev1.ArtifactCompleteResponse
	ResultResponse           = runtimev1.ResultResponse
	Outcome                  = runtimev1.Outcome
	ErrorBody                = runtimev1.ErrorBody
	Limits                   = runtimev1.Limits
	Command                  = runtimev1.Command
	Definition               = runtimev1.Definition
	WorkItem                 = runtimev1.WorkItem
	Profile                  = runtimev1.Profile
	OutputDecl               = runtimev1.OutputDecl
	EnvironmentPin           = runtimev1.EnvironmentPin
	DesignBaseline           = runtimev1.DesignBaseline
	WorkspaceAccess          = runtimev1.WorkspaceAccess
	ContractError            = runtimev1.ContractError
	ValidationError          = runtimev1.ValidationError

	ResourceKind               = runtimev2.ResourceKind
	ResourceIdentity           = runtimev2.ResourceIdentity
	ResourceAdmitRequest       = runtimev2.ResourceAdmitRequest
	ResourceTokenInfo          = runtimev2.ResourceTokenInfo
	ResourceTokenRequest       = runtimev2.ResourceTokenRequest
	ResourceTokenResponse      = runtimev2.ResourceTokenResponse
	ServiceContext             = runtimev2.ServiceContext
	PreparationReceiptResponse = runtimev2.PreparationReceiptResponse

	Endpoint       = runtimev2.Endpoint
	RunWindow      = runtimev2.RunWindow
	PolicyPin      = runtimev2.PolicyPin
	WorkspaceState = runtimev2.WorkspaceState
)

// Access and state values, unchanged from v1 and v2.
const (
	AccessNone  = runtimev1.AccessNone
	AccessRead  = runtimev1.AccessRead
	AccessWrite = runtimev1.AccessWrite

	ResourcePreparation = runtimev2.ResourcePreparation
	ResourceService     = runtimev2.ResourceService
	SubmodulesSkipped   = runtimev2.SubmodulesSkipped

	StateNone     = runtimev2.StateNone
	StateClean    = runtimev2.StateClean
	StateModified = runtimev2.StateModified

	OutcomeCompleted  = runtimev1.OutcomeCompleted
	OutcomeFailed     = runtimev1.OutcomeFailed
	OutcomeNeedsInput = runtimev1.OutcomeNeedsInput
)

// OriginKind is how a run came to exist (PX-23–PX-25).
type OriginKind string

// Origin kinds.
const (
	// OriginWorkItem: started for an actionable work item; carries the pinned
	// work-item snapshot and consumes WIP.
	OriginWorkItem OriginKind = "work_item"
	// OriginPush: started by one applied push record and ref of a trigger; needs
	// no work item and consumes no WIP.
	OriginPush OriginKind = "push"
	// OriginManual: started by a person or API call without a work item.
	OriginManual OriginKind = "manual"
	// OriginProvision: a provisioning run that may start before its repository
	// exists; its Git grant is the seed authority of the repositories it creates.
	OriginProvision OriginKind = "provision"
)

// Origin is the run's immutable record of how it started. Exactly the members
// of its kind are non-null: workItemId for work_item; pushId, repositoryId and
// ref (the update's ref in that repository's applied push record) for push.
type Origin struct {
	Kind         OriginKind         `json:"kind"`
	WorkItemID   *domain.WorkItemID `json:"workItemId"`
	PushID       *string            `json:"pushId"`
	RepositoryID *string            `json:"repositoryId"`
	Ref          *string            `json:"ref"`
}

// SourceV3 is one repository of the run's source set as an attempt sees it. The
// embedded gitv1.Source is the set's entry exactly as run admission pinned it
// (repositoryId, the pinned input commit, the checkout path, the run-level
// access and, for a write source, the branch the writer updates). The remaining
// members are the attempt's view: the clone URL (never with credentials), the
// latest commit published to this source in this run (the pinned commit before
// any) and, only for the attempt holding the writer reservation, the tip it
// expects the branch to have.
type SourceV3 struct {
	gitv1.Source
	RepositoryURL string     `json:"repositoryUrl"`
	HeadCommit    GitCommit  `json:"headCommit"`
	ExpectedTip   *GitCommit `json:"expectedTip"`
}

// PreparedSource is the trusted checkout receipt of one source: the commit the
// preparation checked out at the source's path and the tree it observed.
type PreparedSource struct {
	Path   string    `json:"path"`
	Commit GitCommit `json:"commit"`
	TreeID GitCommit `json:"treeId"`
}

// PreparationV3 is the recorded checkout receipt of a source-backed run: one
// entry per source in source order, and the receipt's digest.
type PreparationV3 struct {
	Sources       []PreparedSource `json:"sources"`
	ReceiptDigest Digest           `json:"receiptDigest"`
}

// CommitOutput is one ref an attempt updated in one repository: the commit the
// ref pointed to after the update and the applied push record that made it. It
// is a reference to evidence, never evidence: the server checks it against the
// push record and the grant before it counts.
type CommitOutput struct {
	RepositoryID string    `json:"repositoryId"`
	Ref          string    `json:"ref"`
	Commit       GitCommit `json:"commit"`
	PushID       string    `json:"pushId"`
}

// LineageEntryV3 is one prior attempt of the run that began a command.
// WorkspaceState and IgnoredPresent are server-derived at that attempt's begin
// over every source; Published lists the commits the attempt reported for the
// refs it updated (always empty unless the attempt wrote).
type LineageEntryV3 struct {
	StepID         string           `json:"stepId"`
	AttemptID      domain.AttemptID `json:"attemptId"`
	Access         WorkspaceAccess  `json:"access"`
	BeganAt        Timestamp        `json:"beganAt"`
	WorkspaceState WorkspaceState   `json:"workspaceState"`
	IgnoredPresent bool             `json:"ignoredPresent"`
	Published      []CommitOutput   `json:"published"`
}

// WorkspaceV3 is the effective workspace of an attempt. Sources is empty
// exactly for a source-free run, and then SourceSetDigest and Preparation are
// null; otherwise SourceSetDigest is the digest of the set the sources project
// to (gitv1.SourceSet.Digest) and Preparation is set.
type WorkspaceV3 struct {
	Access          WorkspaceAccess  `json:"access"`
	MountPath       string           `json:"mountPath"`
	Sources         []SourceV3       `json:"sources"`
	SourceSetDigest *Digest          `json:"sourceSetDigest"`
	Preparation     *PreparationV3   `json:"preparation"`
	Lineage         []LineageEntryV3 `json:"lineage"`
}

// GitRepositoryAccess is what the attempt's Git grant allows in one repository:
// Update lists its ref patterns exactly as the grant records them, the run's
// agent-branch namespace first and, only for the attempt holding the writer
// reservation, the exact branch of each write source of the repository.
type GitRepositoryAccess struct {
	RepositoryID string   `json:"repositoryId"`
	Update       []string `json:"update"`
}

// GitRuntimeGrant advertises a Git grant the attempt may obtain **during the
// attempt** through a runtime route, and where the entrypoint writes the
// credential the route returns. It is a capability, never a credential: the
// grant does not exist until the control plane answers the route.
type GitRuntimeGrant struct {
	CredentialFile string `json:"credentialFile"`
}

// GitRuntimeGrants are the runtime-issued grants an attempt may request. Seed is
// non-null exactly for a provisioning run (POST .../git/seed, PX-25: only for a
// repository this run created in its project). Integration is non-null exactly
// when the source set has a write source, the repository an approved candidate
// would be integrated into (POST .../git/integration, after a gate approved it).
type GitRuntimeGrants struct {
	Seed        *GitRuntimeGrant `json:"seed"`
	Integration *GitRuntimeGrant `json:"integration"`
}

// GitDelivery tells the entrypoint where to find what it needs to talk to the
// Git service: the service's base URL, the file holding the attempt's Git
// credential (and the CA bundle to trust, or null for the system roots), the
// scope of the grant, and the runtime grants it may request. It never contains a
// secret value: the credential is a file written by the runtime adapter before
// the entrypoint starts, readable only by the workload, revoked with the
// attempt, and the context is not secret.
//
// GrantID and CredentialFile are null exactly when the attempt holds no static
// grant, which is the provisioning run without a source set: a grant must name a
// repository and the repositories it will create do not exist at admission, so
// its only Git authority is the seed capability in Runtime. Otherwise they are
// set and Repositories is non-empty.
//
// The username is GitUsername. Every source's clone URL lies beneath BaseURL, so
// a credential helper scoped to BaseURL never offers the credential elsewhere.
type GitDelivery struct {
	GrantID        *string               `json:"grantId"`
	BaseURL        string                `json:"baseUrl"`
	CredentialFile *string               `json:"credentialFile"`
	CAFile         *string               `json:"caFile"`
	Repositories   []GitRepositoryAccess `json:"repositories"`
	Runtime        GitRuntimeGrants      `json:"runtimeGrants"`
}

// ContextV3 is the immutable, fully resolved description of one attempt of a
// run_shared run with a source set. It keeps every v2 field with its meaning
// except the workspace, and adds the origin and the Git credential delivery.
// Every field is present; nullable fields are explicit null. WorkItem is
// non-null exactly for a work_item origin; Git is non-null exactly when the
// attempt holds a Git grant (a source-backed run, or a provisioning run).
type ContextV3 struct {
	ContextVersion int64               `json:"contextVersion"`
	RunModel       string              `json:"runModel"`
	TenantID       domain.TenantID     `json:"tenantId"`
	ProjectID      domain.ProjectID    `json:"projectId"`
	RunID          domain.RunID        `json:"runId"`
	StepID         string              `json:"stepId"`
	AttemptID      domain.AttemptID    `json:"attemptId"`
	ActionKey      string              `json:"actionKey"`
	Definition     Definition          `json:"definition"`
	Origin         Origin              `json:"origin"`
	WorkItem       *WorkItem           `json:"workItem"`
	DesignBaseline *DesignBaseline     `json:"designBaseline"`
	Profile        *Profile            `json:"profile"`
	Command        Command             `json:"command"`
	Workspace      WorkspaceV3         `json:"workspace"`
	Git            *GitDelivery        `json:"git"`
	Endpoints      map[string]Endpoint `json:"endpoints"`
	Egress         []string            `json:"egress"`
	Run            RunWindow           `json:"run"`
	Policy         PolicyPin           `json:"policy"`
	Inputs         map[string]Value    `json:"inputs"`
	Outputs        []OutputDecl        `json:"outputs"`
	Secrets        []SecretBinding     `json:"secrets"`
	Environment    *EnvironmentPin     `json:"environment"`
	Limits         Limits              `json:"limits"`
}

// AdmitResponseV3 carries the attempt token (the unchanged v1 TokenInfo,
// audience kodama-runtime) and the ContextV3.
type AdmitResponseV3 struct {
	Token   TokenInfo `json:"token"`
	Context ContextV3 `json:"context"`
}

// ---- POST /runtime/v3/attempts/{attemptId}/begin ----

// BeginSource is the entrypoint's observation of one source at command begin.
type BeginSource struct {
	Path             string    `json:"path"`
	Head             GitCommit `json:"head"`
	TrackedClean     bool      `json:"trackedClean"`
	UntrackedPresent bool      `json:"untrackedPresent"`
	IgnoredPresent   bool      `json:"ignoredPresent"`
}

// BeginRequestV3 is made once, immediately before the child process starts. It
// observes every source in source order; Sources is empty exactly for a
// source-free attempt.
type BeginRequestV3 struct {
	Sources []BeginSource `json:"sources"`
}

// BeginResponseV3 acknowledges the durable begin record. WorkspaceState and
// IgnoredPresent are derived by the server over every source.
type BeginResponseV3 struct {
	AttemptID      domain.AttemptID `json:"attemptId"`
	BeganAt        Timestamp        `json:"beganAt"`
	WorkspaceState WorkspaceState   `json:"workspaceState"`
	IgnoredPresent bool             `json:"ignoredPresent"`
}

// ---- POST /runtime/v3/attempts/{attemptId}/result ----

// ResultV3 is the normalized final result of an attempt; its canonical form is
// what ResultHash covers. It is the v1 Result with Commits in place of
// SourceCommit: the refs the attempt updated, strictly ordered by repository and
// ref, so one result has one hash. Every field is always present.
type ResultV3 struct {
	Outcome               Outcome             `json:"outcome"`
	ExitCode              *int64              `json:"exitCode"`
	Reason                *string             `json:"reason"`
	Question              *string             `json:"question"`
	Commits               []CommitOutput      `json:"commits"`
	Outputs               map[string]Value    `json:"outputs"`
	DiagnosticArtifactIDs []domain.ArtifactID `json:"diagnosticArtifactIds"`
}

// ResultRequestV3 carries the normalized result and its hash.
type ResultRequestV3 struct {
	ResultHash Digest   `json:"resultHash"`
	Result     ResultV3 `json:"result"`
}

// Scope returns the owning tenant and project.
func (c ContextV3) Scope() domain.Scope {
	return domain.Scope{TenantID: c.TenantID, ProjectID: c.ProjectID}
}

// ---- resource subjects (/runtime/v3/runs/{runId}/resources/{resourceKey}) ----

// PreparationSource is one checkout the preparation workload makes: the
// repository, its clone URL (never with credentials), the pinned commit and the
// path beneath /workspace. The entries are the run's source set in path order,
// without the access and branch, which a read-only checkout never uses.
type PreparationSource struct {
	RepositoryID  string    `json:"repositoryId"`
	RepositoryURL string    `json:"repositoryUrl"`
	Commit        GitCommit `json:"commit"`
	Path          string    `json:"path"`
}

// PreparationGit is the preparation workload's Git credential delivery: the
// attempt's GitDelivery without any update right. The credential is a read-only
// preparation grant (kind preparation, no update patterns anywhere), delivered
// as a file under /kodama/git like the attempt's, never a secret in the context
// and revoked when the preparation ends. Repositories lists the repositories of
// the source set once each, in order of first appearance.
type PreparationGit struct {
	GrantID        string   `json:"grantId"`
	BaseURL        string   `json:"baseUrl"`
	CredentialFile string   `json:"credentialFile"`
	CAFile         *string  `json:"caFile"`
	Repositories   []string `json:"repositories"`
}

// PreparationContextV3 is the checkout assignment of the one-shot preparation
// workload for the whole source set: one preparation for all sources.
// MaxAttempts is the number of checkout tries the program may make inside this
// one workload; TimeoutSeconds bounds all tries and all sources together.
type PreparationContextV3 struct {
	Sources        []PreparationSource `json:"sources"`
	MountPath      string              `json:"mountPath"`
	TimeoutSeconds int64               `json:"timeoutSeconds"`
	MaxBytes       int64               `json:"maxBytes"`
	MaxAttempts    int64               `json:"maxAttempts"`
	DeadlineAt     Timestamp           `json:"deadlineAt"`
	Git            PreparationGit      `json:"git"`
}

// ResourceContextV3 is the immutable context of a preparation or service
// resource of a v3 run. Exactly one of Preparation and Service is non-null,
// matching ResourceKind. The service member is the unchanged v2 ServiceContext:
// a service never mounts /workspace, never holds a Git credential and is
// independent of the source set, so its message does not change; only the
// context version does, so a v3 run's service is never served as a v2 one.
type ResourceContextV3 struct {
	ContextVersion int64                 `json:"contextVersion"`
	ResourceKind   ResourceKind          `json:"resourceKind"`
	TenantID       domain.TenantID       `json:"tenantId"`
	ProjectID      domain.ProjectID      `json:"projectId"`
	RunID          domain.RunID          `json:"runId"`
	ResourceKey    string                `json:"resourceKey"`
	Preparation    *PreparationContextV3 `json:"preparation"`
	Service        *ServiceContext       `json:"service"`
	Policy         PolicyPin             `json:"policy"`
}

// ResourceAdmitResponseV3 carries the resource token (the unchanged v2
// ResourceTokenInfo, audience kodama-runtime-resource) and the ResourceContextV3.
type ResourceAdmitResponseV3 struct {
	Token   ResourceTokenInfo `json:"token"`
	Context ResourceContextV3 `json:"context"`
}

// PreparationReceiptRequestV3 records the trusted checkout of every source. The
// three safety fields have literal required values; Attempts is the internal try
// count of the one workload, 1..maxAttempts, not an attempt row.
type PreparationReceiptRequestV3 struct {
	Sources            []PreparedSource `json:"sources"`
	HooksDisabled      bool             `json:"hooksDisabled"`
	Submodules         string           `json:"submodules"`
	CredentialsRemoved bool             `json:"credentialsRemoved"`
	Attempts           int64            `json:"attempts"`
}

// ---- runtime-issued Git grants (POST .../git/seed, POST .../git/integration) ----

// GitSeedRequest asks for the single-use seed grant of an empty repository this
// run created. The control plane answers only for a repository the run created
// in its admitted project (anything else is not_found, so a foreign repository
// is indistinguishable from an unknown one).
type GitSeedRequest struct {
	RepositoryID string `json:"repositoryId"`
}

// GitIntegrationRequest asks for the integration grant of an approved gate: one
// update of the repository's default branch from the approved base to the
// approved candidate. The control plane answers only after the gate named by
// GateID approved a subject that names that candidate for this run.
type GitIntegrationRequest struct {
	RepositoryID string `json:"repositoryId"`
	GateID       string `json:"gateId"`
}

// GitGrantResponse carries the grant the control plane recorded (its claims are
// not secret), the opaque credential (a secret: shown once, never logged,
// written by the entrypoint to CredentialFile with the workload's permissions
// only) and the file the context advertised for it. The grant is revoked with the
// attempt like the attempt's own.
type GitGrantResponse struct {
	Grant          gitv1.GrantClaims `json:"grant"`
	Credential     string            `json:"credential"`
	CredentialFile string            `json:"credentialFile"`
}

// IntegrationApproval is what the control plane knows when an attempt asks for an
// integration grant: the approved gate's facts, supplied by the delivery module
// from the gate and the repository and never by the caller. Ref is the
// repository's default branch, Base the tip the approval was given over,
// Candidate the approved commit and CurrentTip the default branch's tip now.
type IntegrationApproval struct {
	GateID       string
	RepositoryID string
	Ref          string
	Base         string
	Candidate    string
	CurrentTip   string
}

// ErrorResponse is the v3 error envelope: the v2 envelope accepting the v2 codes
// and the Git contract's codes that the Git grant routes can return.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}
