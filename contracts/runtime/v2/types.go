package runtimev2

import (
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Types reused unchanged from v1 by the attempt routes. They are aliases, not
// copies: the v1 validators, decoders and canonical rules apply as they are.
type (
	Digest    = runtimev1.Digest
	GitCommit = runtimev1.GitCommit
	Timestamp = runtimev1.Timestamp
	Value     = runtimev1.Value

	AdmitRequest             = runtimev1.AdmitRequest
	JobIdentity              = runtimev1.JobIdentity
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
	SourceProposalRequest    = runtimev1.SourceProposalRequest
	SourceProposalResponse   = runtimev1.SourceProposalResponse
	Result                   = runtimev1.Result
	ResultRequest            = runtimev1.ResultRequest
	ResultResponse           = runtimev1.ResultResponse
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
	SecretDelivery           = runtimev1.SecretDelivery
)

// Access values and the one canonical result hash, unchanged from v1.
const (
	AccessNone  = runtimev1.AccessNone
	AccessRead  = runtimev1.AccessRead
	AccessWrite = runtimev1.AccessWrite
)

// ResultHash is byte-for-byte the v1 canonicalization of the result object, so
// every historical hash vector stays valid under v2.
func ResultHash(r Result) (Digest, error) { return runtimev1.ResultHash(r) }

// Enumerations of v2. A client that meets an unknown value must treat it as
// unknown, never as success.
type (
	// WorkspaceState is the server-derived state of the scratch at an
	// attempt's command begin.
	WorkspaceState string
	// ResourceKind is the kind of a runtime resource subject.
	ResourceKind string
	// ServiceRole is the role of a run service.
	ServiceRole string
)

const (
	// StateNone: a source-free attempt (no repository exists).
	StateNone WorkspaceState = "none"
	// StateClean: head equals the context's headCommit, tracked files are clean
	// and there are no untracked files.
	StateClean WorkspaceState = "clean"
	// StateModified: anything else.
	StateModified WorkspaceState = "modified"

	ResourcePreparation ResourceKind = "preparation"
	ResourceService     ResourceKind = "service"

	RoleGeneral ServiceRole = "general"
	RoleBuilder ServiceRole = "builder"

	// SubmodulesSkipped is the only value a preparation receipt may carry.
	SubmodulesSkipped = "skipped"
)

// ---- POST /runtime/v2/attempts/{attemptId}/admit, GET .../context ----

// WorkspaceSource is the exact source of a source-backed run. InputCommit is the
// run's immutable pinned commit; HeadCommit is the latest commit published in
// this run by a successful writer (InputCommit before any). Branch and
// ExpectedTip are non-null only for a writer.
type WorkspaceSource struct {
	RepositoryURL string     `json:"repositoryUrl"`
	InputCommit   GitCommit  `json:"inputCommit"`
	HeadCommit    GitCommit  `json:"headCommit"`
	Branch        *string    `json:"branch"`
	ExpectedTip   *GitCommit `json:"expectedTip"`
}

// PreparationPin is the recorded trusted checkout receipt of a source-backed run.
type PreparationPin struct {
	Commit        GitCommit `json:"commit"`
	TreeID        GitCommit `json:"treeId"`
	ReceiptDigest Digest    `json:"receiptDigest"`
}

// LineageEntry is one prior attempt of the run that began a command. WorkspaceState
// and IgnoredPresent are the server-derived values of that attempt's begin,
// never client-asserted.
type LineageEntry struct {
	StepID          string           `json:"stepId"`
	AttemptID       domain.AttemptID `json:"attemptId"`
	Access          WorkspaceAccess  `json:"access"`
	BeganAt         Timestamp        `json:"beganAt"`
	WorkspaceState  WorkspaceState   `json:"workspaceState"`
	IgnoredPresent  bool             `json:"ignoredPresent"`
	PublishedCommit *GitCommit       `json:"publishedCommit"`
}

// WorkspaceV2 is the effective workspace of an attempt. Source and Preparation
// are null exactly for a source-free run; Lineage is empty exactly when no
// command has begun.
type WorkspaceV2 struct {
	Access      WorkspaceAccess  `json:"access"`
	MountPath   string           `json:"mountPath"`
	Source      *WorkspaceSource `json:"source"`
	Preparation *PreparationPin  `json:"preparation"`
	Lineage     []LineageEntry   `json:"lineage"`
}

// Endpoint is the typed endpoint of a run service: the run-unique host and the
// declared port numbers by name.
type Endpoint struct {
	Host  string           `json:"host"`
	Ports map[string]int64 `json:"ports"`
}

// RunWindow is the run's pinned time window; the deadline covers gate waits.
type RunWindow struct {
	StartedAt  Timestamp `json:"startedAt"`
	DeadlineAt Timestamp `json:"deadlineAt"`
}

// PolicyPin identifies the pinned execution policy.
type PolicyPin struct {
	ID     string `json:"id"`
	Digest Digest `json:"digest"`
}

// ContextV2 is the immutable, fully resolved description of one attempt of a
// run_shared run. It keeps every v1 field with its meaning and adds the
// run-resource, workspace, policy and endpoint pins. Every field is present;
// nullable fields are explicit null.
type ContextV2 struct {
	ContextVersion int64               `json:"contextVersion"`
	RunModel       string              `json:"runModel"`
	TenantID       domain.TenantID     `json:"tenantId"`
	ProjectID      domain.ProjectID    `json:"projectId"`
	RunID          domain.RunID        `json:"runId"`
	StepID         string              `json:"stepId"`
	AttemptID      domain.AttemptID    `json:"attemptId"`
	ActionKey      string              `json:"actionKey"`
	Definition     Definition          `json:"definition"`
	WorkItem       *WorkItem           `json:"workItem"`
	DesignBaseline *DesignBaseline     `json:"designBaseline"`
	Profile        *Profile            `json:"profile"`
	Command        Command             `json:"command"`
	Workspace      WorkspaceV2         `json:"workspace"`
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

// AdmitResponseV2 carries the attempt token (the unchanged v1 TokenInfo, audience
// kodama-runtime) and the ContextV2.
type AdmitResponseV2 struct {
	Token   TokenInfo `json:"token"`
	Context ContextV2 `json:"context"`
}

// ---- POST /runtime/v2/attempts/{attemptId}/begin ----

// BeginWorkspace is the entrypoint's observation of /workspace at command begin.
type BeginWorkspace struct {
	Head             GitCommit `json:"head"`
	TrackedClean     bool      `json:"trackedClean"`
	UntrackedPresent bool      `json:"untrackedPresent"`
	IgnoredPresent   bool      `json:"ignoredPresent"`
}

// BeginRequest is made once, immediately before the child process starts. The
// workspace is null exactly for a source-free attempt.
type BeginRequest struct {
	Workspace *BeginWorkspace `json:"workspace"`
}

// BeginResponse acknowledges the durable begin record. WorkspaceState and
// IgnoredPresent are derived by the server.
type BeginResponse struct {
	AttemptID      domain.AttemptID `json:"attemptId"`
	BeganAt        Timestamp        `json:"beganAt"`
	WorkspaceState WorkspaceState   `json:"workspaceState"`
	IgnoredPresent bool             `json:"ignoredPresent"`
}

// ---- resource subjects ----

// ResourceIdentity is the provider-neutral instance a process observes about
// itself. WorkloadName and WorkloadUID are the committed workload's name and
// bound instance UID; PodName and PodUID are both non-null only when the backend
// runs the workload beneath a controller object, and both null otherwise.
// The values are evidence the server verifies against committed state, never
// authority.
type ResourceIdentity struct {
	WorkloadName string  `json:"workloadName"`
	WorkloadUID  string  `json:"workloadUid"`
	PodName      *string `json:"podName"`
	PodUID       *string `json:"podUid"`
}

// ResourceAdmitRequest exchanges the one-time admission credential of a
// preparation or service workload; replay rules are those of v1's AdmitRequest.
type ResourceAdmitRequest struct {
	AdmissionCredential string           `json:"admissionCredential"`
	Nonce               string           `json:"nonce"`
	ClientVersion       string           `json:"clientVersion"`
	Identity            ResourceIdentity `json:"identity"`
}

// ResourceTokenInfo describes a resource token: audience kodama-runtime-resource,
// bound to (tenant, project, run, resourceKey, workloadUid), never beyond the
// resource's deadline.
type ResourceTokenInfo struct {
	Token        string    `json:"token"`
	TokenType    string    `json:"tokenType"`
	Audience     string    `json:"audience"`
	ExpiresAt    Timestamp `json:"expiresAt"`
	MaxExpiresAt Timestamp `json:"maxExpiresAt"`
}

// ResourceAdmitResponse carries the resource token and the ResourceContext.
type ResourceAdmitResponse struct {
	Token   ResourceTokenInfo `json:"token"`
	Context ResourceContext   `json:"context"`
}

// ResourceTokenRequest is deliberately empty; the bearer token is the credential.
type ResourceTokenRequest struct{}

// ResourceTokenResponse carries a renewed resource token.
type ResourceTokenResponse struct {
	Token ResourceTokenInfo `json:"token"`
}

// PreparationContext is the checkout assignment of the one-shot preparation
// workload. MaxAttempts is the number of checkout tries the program may make
// inside this one workload; TimeoutSeconds bounds all tries together.
type PreparationContext struct {
	RepositoryURL  string    `json:"repositoryUrl"`
	Commit         GitCommit `json:"commit"`
	MountPath      string    `json:"mountPath"`
	TimeoutSeconds int64     `json:"timeoutSeconds"`
	MaxBytes       int64     `json:"maxBytes"`
	MaxAttempts    int64     `json:"maxAttempts"`
	DeadlineAt     Timestamp `json:"deadlineAt"`
}

// ServiceCommand is a service's resolved process. Command is null when the image
// defaults run, which is permitted only without secret bindings; Args follows
// the pipeline format's argv matrix.
type ServiceCommand struct {
	Image   string            `json:"image"`
	Command []string          `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// ServiceContext is the assignment of one run service.
type ServiceContext struct {
	Name       string          `json:"name"`
	Role       ServiceRole     `json:"role"`
	Command    ServiceCommand  `json:"command"`
	Secrets    []SecretBinding `json:"secrets"`
	DeadlineAt Timestamp       `json:"deadlineAt"`
}

// ResourceContext is the immutable context of a preparation or service resource.
// Exactly one of Preparation and Service is non-null, matching ResourceKind.
type ResourceContext struct {
	ContextVersion int64               `json:"contextVersion"`
	ResourceKind   ResourceKind        `json:"resourceKind"`
	TenantID       domain.TenantID     `json:"tenantId"`
	ProjectID      domain.ProjectID    `json:"projectId"`
	RunID          domain.RunID        `json:"runId"`
	ResourceKey    string              `json:"resourceKey"`
	Preparation    *PreparationContext `json:"preparation"`
	Service        *ServiceContext     `json:"service"`
	Policy         PolicyPin           `json:"policy"`
}

// PreparationReceiptRequest records the trusted checkout. The three safety
// fields have literal required values; Attempts is the internal try count of the
// one workload, 1..maxAttempts, not an attempt row.
type PreparationReceiptRequest struct {
	Commit             GitCommit `json:"commit"`
	TreeID             GitCommit `json:"treeId"`
	HooksDisabled      bool      `json:"hooksDisabled"`
	Submodules         string    `json:"submodules"`
	CredentialsRemoved bool      `json:"credentialsRemoved"`
	Attempts           int64     `json:"attempts"`
}

// PreparationReceiptResponse acknowledges the recorded receipt. ReceiptDigest is
// the canonical digest of the request plus the run, the resource key and the
// bound volume UID the server observed.
type PreparationReceiptResponse struct {
	ReceiptDigest Digest    `json:"receiptDigest"`
	RecordedAt    Timestamp `json:"recordedAt"`
}

// ErrorResponse is the v2 error envelope. It reuses the v1 ErrorBody and differs
// only in the codes it accepts (the v1 ErrorResponse rejects the new ones).
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}
