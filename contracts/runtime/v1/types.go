package runtimev1

import "github.com/optimiware/kodama/internal/domain"

// Enumerations. A client that meets an unknown value must treat it as unknown,
// never as success.
type (
	Outcome         string
	WorkspaceAccess string
	OutputType      string
	SecretDelivery  string
	ArtifactKind    string
	ArtifactRole    string
	ArtifactState   string
	PublishStatus   string
	AttemptStatus   string
)

const (
	OutcomeCompleted  Outcome = "completed"
	OutcomeFailed     Outcome = "failed"
	OutcomeNeedsInput Outcome = "needs_input"

	AccessNone  WorkspaceAccess = "none"
	AccessRead  WorkspaceAccess = "read"
	AccessWrite WorkspaceAccess = "write"

	OutputString      OutputType = "string"
	OutputBoolean     OutputType = "boolean"
	OutputInteger     OutputType = "integer"
	OutputArtifact    OutputType = "artifact"
	OutputOCIArtifact OutputType = "ociArtifact"

	DeliveryFile SecretDelivery = "file"
	DeliveryEnv  SecretDelivery = "env"

	KindFile ArtifactKind = "file"
	KindOCI  ArtifactKind = "oci"

	RoleOutput     ArtifactRole = "output"
	RoleDiagnostic ArtifactRole = "diagnostic"
	RoleBundle     ArtifactRole = "bundle"

	StateReserved   ArtifactState = "reserved"
	StateRegistered ArtifactState = "registered"
	StateUsable     ArtifactState = "usable"

	PublishAccepted PublishStatus = "accepted"
	PublishPending  PublishStatus = "pending"

	AttemptSucceeded AttemptStatus = "succeeded"
	AttemptFailed    AttemptStatus = "failed"
	AttemptCancelled AttemptStatus = "cancelled"
)

// ---- POST /admit, POST /token ----

// JobIdentity is the Kubernetes identity the entrypoint observes for itself.
// It is evidence the server verifies against the Job it created, not authority.
type JobIdentity struct {
	Name    string `json:"name"`
	UID     string `json:"uid"`
	PodName string `json:"podName"`
	PodUID  string `json:"podUid"`
}

// AdmitRequest exchanges the one-time admission credential. Nonce makes the
// exchange repeatable after a lost response: the same credential and nonce
// return a fresh token under the same maximum lifetime; a different nonce
// after consumption is admission_consumed.
type AdmitRequest struct {
	AdmissionCredential string      `json:"admissionCredential"`
	Nonce               string      `json:"nonce"`
	ClientVersion       string      `json:"clientVersion"`
	Job                 JobIdentity `json:"job"`
}

// TokenInfo describes a runtime token. MaxExpiresAt bounds all renewals.
type TokenInfo struct {
	Token        string    `json:"token"`
	TokenType    string    `json:"tokenType"`
	Audience     string    `json:"audience"`
	ExpiresAt    Timestamp `json:"expiresAt"`
	MaxExpiresAt Timestamp `json:"maxExpiresAt"`
}

type AdmitResponse struct {
	Token   TokenInfo `json:"token"`
	Context Context   `json:"context"`
}

// TokenRequest is deliberately empty; the bearer token is the credential.
type TokenRequest struct{}

type TokenResponse struct {
	Token TokenInfo `json:"token"`
}

// ---- GET /context ----

type Definition struct {
	ID     domain.DefinitionID `json:"id"`
	Digest Digest              `json:"digest"`
}

type AcceptanceCriterion struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// WorkItem is the pinned specification snapshot. Cleanup runs have none.
type WorkItem struct {
	ID                 domain.WorkItemID      `json:"id"`
	ContentRevision    domain.ContentRevision `json:"contentRevision"`
	Spec               string                 `json:"spec"`
	AcceptanceCriteria []AcceptanceCriterion  `json:"acceptanceCriteria"`
}

// Profile pins the agent profile and wrapper version a step runs under.
type Profile struct {
	Name           string `json:"name"`
	Version        int64  `json:"version"`
	WrapperVersion string `json:"wrapperVersion"`
}

type Command struct {
	Image   string            `json:"image"`
	Command []string          `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// Source is the exact source to check out. The URL never carries credentials.
// Readers have a null Branch and ExpectedTip; a writer has both.
type Source struct {
	RepositoryURL string     `json:"repositoryUrl"`
	Commit        GitCommit  `json:"commit"`
	Branch        *string    `json:"branch"`
	ExpectedTip   *GitCommit `json:"expectedTip"`
}

// Workspace.Source is null exactly when Access is "none".
type Workspace struct {
	Access WorkspaceAccess `json:"access"`
	Source *Source         `json:"source"`
}

// OutputDecl declares one output. Path is non-null only for file artifacts.
type OutputDecl struct {
	Name string     `json:"name"`
	Type OutputType `json:"type"`
	Path *string    `json:"path"`
}

type SecretBinding struct {
	Ref      string         `json:"ref"`
	Version  int64          `json:"version"`
	Name     string         `json:"name"`
	Delivery SecretDelivery `json:"delivery"`
}

type EnvironmentPin struct {
	ID           domain.EnvironmentID `json:"id"`
	ConfigDigest Digest               `json:"configDigest"`
}

type Limits struct {
	TimeoutSeconds     int64 `json:"timeoutSeconds"`
	MaxOutputBytes     int64 `json:"maxOutputBytes"`
	MaxOutputFiles     int64 `json:"maxOutputFiles"`
	MaxDiagnosticBytes int64 `json:"maxDiagnosticBytes"`
}

// Context is the immutable, fully resolved description of one attempt.
type Context struct {
	TenantID       domain.TenantID  `json:"tenantId"`
	ProjectID      domain.ProjectID `json:"projectId"`
	RunID          domain.RunID     `json:"runId"`
	StepID         string           `json:"stepId"`
	AttemptID      domain.AttemptID `json:"attemptId"`
	ActionKey      string           `json:"actionKey"`
	Definition     Definition       `json:"definition"`
	WorkItem       *WorkItem        `json:"workItem"`
	DesignBaseline *DesignBaseline  `json:"designBaseline"`
	Profile        *Profile         `json:"profile"`
	Command        Command          `json:"command"`
	Workspace      Workspace        `json:"workspace"`
	Inputs         map[string]Value `json:"inputs"`
	Outputs        []OutputDecl     `json:"outputs"`
	Secrets        []SecretBinding  `json:"secrets"`
	Environment    *EnvironmentPin  `json:"environment"`
	Limits         Limits           `json:"limits"`
}

// Scope returns the owning tenant and project.
func (c Context) Scope() domain.Scope {
	return domain.Scope{TenantID: c.TenantID, ProjectID: c.ProjectID}
}

// ---- POST /secrets/resolve ----

type SecretResolveRequest struct {
	Ref     string `json:"ref"`
	Version int64  `json:"version"`
}

// SecretResolveResponse carries a secret value. It must never be logged.
type SecretResolveResponse struct {
	Ref         string         `json:"ref"`
	Version     int64          `json:"version"`
	Name        string         `json:"name"`
	Delivery    SecretDelivery `json:"delivery"`
	ValueBase64 string         `json:"valueBase64"`
}

// ---- artifacts ----

// ArtifactReserveRequest reserves a file upload (Kind "file": MediaType and
// SizeBytes set, Registry null) or registers an OCI image (Kind "oci": Registry
// set, MediaType and SizeBytes null, Role "output").
type ArtifactReserveRequest struct {
	Name      string       `json:"name"`
	Kind      ArtifactKind `json:"kind"`
	Role      ArtifactRole `json:"role"`
	Digest    Digest       `json:"digest"`
	MediaType *string      `json:"mediaType"`
	SizeBytes *int64       `json:"sizeBytes"`
	Registry  *string      `json:"registry"`
}

// Upload carries short-lived transfer instructions; it is not an identity.
type Upload struct {
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt Timestamp         `json:"expiresAt"`
}

// ArtifactReserveResponse: a file starts "reserved" with an Upload; an OCI
// artifact is "registered" with a null Upload.
type ArtifactReserveResponse struct {
	ArtifactID domain.ArtifactID `json:"artifactId"`
	State      ArtifactState     `json:"state"`
	Upload     *Upload           `json:"upload"`
}

type ArtifactCompleteRequest struct {
	Digest    Digest `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
}

type ArtifactCompleteResponse struct {
	ArtifactID domain.ArtifactID `json:"artifactId"`
	State      ArtifactState     `json:"state"`
	Digest     Digest            `json:"digest"`
	SizeBytes  int64             `json:"sizeBytes"`
}

// ---- POST /source ----

type SourceProposalRequest struct {
	Commit           GitCommit         `json:"commit"`
	ExpectedTip      GitCommit         `json:"expectedTip"`
	BundleArtifactID domain.ArtifactID `json:"bundleArtifactId"`
}

// SourceProposalResponse: "accepted" (HTTP 200) has AcceptedCommit == Commit;
// "pending" (HTTP 202) has a null AcceptedCommit and must settle before the
// attempt may succeed. A lost expected-tip race is a 409 publication_conflict.
type SourceProposalResponse struct {
	Status         PublishStatus `json:"status"`
	Commit         GitCommit     `json:"commit"`
	AcceptedCommit *GitCommit    `json:"acceptedCommit"`
}

// ---- POST /result ----

// Result is the normalized final result of an attempt; its canonical form is
// what ResultHash covers. Every field is always present.
type Result struct {
	Outcome               Outcome             `json:"outcome"`
	ExitCode              *int64              `json:"exitCode"`
	Reason                *string             `json:"reason"`
	Question              *string             `json:"question"`
	SourceCommit          *GitCommit          `json:"sourceCommit"`
	Outputs               map[string]Value    `json:"outputs"`
	DiagnosticArtifactIDs []domain.ArtifactID `json:"diagnosticArtifactIds"`
}

type ResultRequest struct {
	ResultHash Digest `json:"resultHash"`
	Result     Result `json:"result"`
}

// ResultResponse acknowledges durable acceptance. AttemptStatus is derived by
// the server; cancellation or timeout can supersede a completed result.
type ResultResponse struct {
	AttemptID     domain.AttemptID `json:"attemptId"`
	ResultHash    Digest           `json:"resultHash"`
	AttemptStatus AttemptStatus    `json:"attemptStatus"`
	AcceptedAt    Timestamp        `json:"acceptedAt"`
}

// ---- errors ----

type ErrorBody struct {
	Code      domain.ErrorCode `json:"code"`
	Message   string           `json:"message"`
	RequestID string           `json:"requestId"`
	Details   map[string]any   `json:"details"`
}

type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}
