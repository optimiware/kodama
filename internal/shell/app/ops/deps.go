package opsapp

import (
	"context"
	"encoding/json"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/inbox"
	"github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/mcpproxy"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	tenancyadmin "github.com/optimiware/kodama/internal/shell/tenancy/admin"
	"github.com/optimiware/kodama/internal/shell/token"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// Dependencies is the consumer-owned set of capability services and small
// ports one operation-binding table needs. Every field is required: Bindings
// refuses a nil dependency instead of silently binding an operation to a
// placeholder.
//
// The interfaces are declared here, not imported from the capabilities, so the
// binding table depends only on the exact methods it calls. The production
// implementations are the real capability services (and the composition-root
// adapters for the three principal-bound optional surfaces); tests bind the
// real services over the memory-store adapters.
type Dependencies struct {
	// Tenancy is the real tenancy administration service.
	Tenancy TenancyAdmin
	// Bindings is the real resource-binding service.
	Bindings BindingAdmin
	// Pipelines is the real pipeline management service.
	Pipelines PipelineManagement
	// Admission is the real pipeline admission service used by pipeline.start.
	Admission PipelineAdmission
	// Executions is the real execution-operation service.
	Executions ExecutionOperations
	// Receipts is the durable idempotency lookup. pipeline.start consults it
	// before admitting the currently stored definition, so an equal retry
	// replays the committed receipt even after the pipeline was edited or
	// deleted.
	Receipts ReceiptLookup
	// Secrets is the real secret management service.
	Secrets SecretManagement
	// Authority is the optional current-authority reader used by handlers whose
	// underlying read seam does not confirm registry authority checks itself.
	Authority authorize.AuthorityReader
	// Services is the real service-account administration service.
	Services ServiceAdministration
	// Credentials is the real service-credential administration service.
	Credentials CredentialAdministration
	// Tasks is the real task-operation service.
	Tasks TaskOperations
	// Inbox is the real inbox query service.
	Inbox InboxQueries
	// Requests is the real request-resolution service.
	Requests RequestResolution
	// Logs is the real retained-log read service.
	Logs RetainedLogs
	// Verifier is the real actor-verification service.
	Verifier Verification
	// ActorActions resolves the current running container action of the actor
	// credential forwarded to actor.verify. The action identity is resolved
	// server-side from the credential and is never taken from the request.
	ActorActions ActorAction
	// Reports is the real verified-resource-report service.
	Reports Reports
	// Events is the real custom-observation ingress, bound to the already
	// authenticated principal because the router does not forward the
	// credential.
	Events EventPublication
	// EventRead is the real authorized event-read service.
	EventRead EventReads
	// EventSubscribe is the real authorized subscription-setup service.
	EventSubscribe EventSubscriptions
	// Models is the real optional model gateway.
	Models ModelInvocation
	// Tools is the real optional external MCP tool proxy.
	Tools ToolInvocation
	// IDs allocates the identities the handlers themselves must mint: the
	// execution principal of a task-free start and a verification identity.
	IDs IdentitySource
	// TenantKeys mints the encrypted derivation key of a tenant creation or key
	// rotation. It is the KMS boundary; the handlers never see plaintext.
	TenantKeys TenantKeySource
	// ActionTimeout is the installation default action timeout used by the
	// pipeline.start admission. It must be positive.
	ActionTimeout time.Duration
}

// TenancyAdmin is the tenancy administration command surface.
type TenancyAdmin interface {
	CreateTenant(context.Context, tenancyadmin.TenantCreateCommand) (tenancyadmin.TenantCreateResult, error)
	RotateTenantKey(context.Context, tenancyadmin.TenantKeyRotationCommand) (tenancyadmin.TenantKeyRotationResult, error)
	CreateProject(context.Context, tenancyadmin.ProjectCreateCommand) (tenancyadmin.ProjectCreateResult, error)
	UpdateProject(context.Context, tenancyadmin.ProjectUpdateCommand) (tenancyadmin.ProjectUpdateResult, error)
	GrantMembership(context.Context, tenancyadmin.MembershipGrantCommand) (tenancyadmin.MembershipGrantResult, error)
	RevokeMembership(context.Context, tenancyadmin.MembershipRevokeCommand) (tenancyadmin.MembershipRevokeResult, error)
	ListMemberships(context.Context, tenancyadmin.MembershipListCommand) (tenancyadmin.MembershipPage, error)
}

// BindingAdmin is the resource-binding command and read surface.
type BindingAdmin interface {
	RegisterBinding(context.Context, resource.RegisterBindingCommand) (resource.BindingResult, error)
	UpdateBinding(context.Context, resource.UpdateBindingCommand) (resource.BindingResult, error)
	DeleteBinding(context.Context, resource.DeleteBindingCommand) (resource.BindingDeleteResult, error)
	ReadBinding(context.Context, resource.ReadBindingCommand) (resource.Binding, error)
}

// PipelineManagement is the pipeline authoring command and read surface.
type PipelineManagement interface {
	Apply(context.Context, pipeline.ApplyCommand) (pipeline.ChangeResult, error)
	Update(context.Context, pipeline.UpdateCommand) (pipeline.ChangeResult, error)
	Delete(context.Context, pipeline.DeleteCommand) (pipeline.ChangeResult, error)
	Read(context.Context, pipeline.ReadCommand) (pipeline.Definition, error)
}

// PipelineAdmission is the immutable admission seam.
type PipelineAdmission interface {
	Admit(context.Context, pipeline.AdmitRequest) (pipeline.AdmissionResult, error)
}

// ExecutionOperations is the execution operation and read surface.
type ExecutionOperations interface {
	Start(context.Context, execution.StartCommand) (execution.StartResult, error)
	Inspect(context.Context, execution.InspectCommand) (execution.View, error)
	List(context.Context, execution.ListCommand) (execution.Page, error)
	Cancel(context.Context, execution.CancelCommand) (execution.TransitionResult, error)
	StageOutputs(context.Context, execution.OutputsCommand) (execution.TransitionResult, error)
}

// ReceiptLookup returns the durable receipt for one scoped idempotency
// identity. A missing receipt is (Record{}, false, nil).
type ReceiptLookup interface {
	Lookup(context.Context, receipt.Key) (receipt.Record, bool, error)
}

// SecretManagement is the secret metadata, write and delete surface.
type SecretManagement interface {
	Put(context.Context, secret.PutCommand) (secret.Metadata, error)
	Delete(context.Context, secret.DeleteCommand) error
	Get(context.Context, tenancy.Scope, secret.SecretID) (secret.Metadata, error)
	GetByName(context.Context, tenancy.Scope, id.SecretName) (secret.Metadata, error)
}

// ServiceAdministration is the service-account administration surface.
type ServiceAdministration interface {
	Create(context.Context, service.CreateCommand) (service.CreateResult, error)
	Update(context.Context, service.UpdateCommand) (service.UpdateResult, error)
	Enable(context.Context, service.StatusCommand) (service.StatusResult, error)
	Disable(context.Context, service.StatusCommand) (service.StatusResult, error)
}

// CredentialAdministration is the service-credential lifecycle surface. Only
// issue and rotate return a one-time token.
type CredentialAdministration interface {
	Issue(context.Context, token.ServiceCredentialCommand) (token.IssueResult, error)
	Rotate(context.Context, token.ServiceCredentialCommand) (token.IssueResult, error)
	Revoke(context.Context, token.ServiceCredentialRevokeCommand) (token.RevokeResult, error)
}

// TaskOperations is the task command and read surface.
type TaskOperations interface {
	Create(context.Context, task.CreateCommand) (task.CreateResult, error)
	Edit(context.Context, task.EditCommand) (task.EditResult, error)
	Start(context.Context, task.StartCommand) (task.StartResult, error)
	Ask(context.Context, task.AskCommand) (task.AskResult, error)
	Reply(context.Context, task.ReplyCommand) (task.ReplyResult, error)
	Cancel(context.Context, task.CancelCommand) (task.CancelResult, error)
	History(context.Context, task.HistoryCommand) (task.HistoryPage, error)
	Read(context.Context, task.ReadCommand) (task.Task, error)
}

// InboxQueries is the read-only inbox query surface.
type InboxQueries interface {
	Query(context.Context, inbox.QueryCommand) (inbox.Page, error)
}

// RequestResolution is the authorized request-resolution surface.
type RequestResolution interface {
	Resolve(context.Context, request.ResolveCommand) (request.ResolveResult, error)
}

// RetainedLogs is the authorized retained-log read surface.
type RetainedLogs interface {
	Read(context.Context, log.ReadCommand) (log.ReadPage, error)
}

// Verification is the actor-verification surface.
type Verification interface {
	Verify(context.Context, verify.Attempt) (verify.Record, error)
}

// ActorAction resolves the current running container action of one forwarded
// actor credential. A human or service actor names no execution action and
// returns the zero action, while an execution actor whose execution has no
// running container action is refused instead of falling back to no action.
type ActorAction interface {
	ResolveActorAction(context.Context, string) (id.ActionID, error)
}

// Reports is the verified-resource-report surface.
type Reports interface {
	Report(context.Context, verify.ReportCommand) (verify.ReportResult, error)
}

// EventPublication publishes one custom observation for an already
// authenticated principal. The real event.Ingress authenticates from a
// credential the router does not forward, so composition binds the verified
// principal for that request; the ingress still re-checks authorization and
// current authority itself.
type EventPublication interface {
	Publish(context.Context, authorize.Principal, event.PublishAttempt) (event.IngressResult, error)
}

// EventReads is the authorized committed-event read surface.
type EventReads interface {
	Read(context.Context, event.ReadCommand) (event.ReadPage, error)
}

// EventSubscriptions is the authorized subscription-setup surface.
type EventSubscriptions interface {
	Setup(context.Context, event.SubscribeCommand) (event.SubscribeGrant, error)
}

// ModelInvocation is the optional model surface, bound to the already
// authenticated principal.
type ModelInvocation interface {
	Invoke(context.Context, authorize.Principal, id.ProjectID, api.ChatCompletionRequest) (api.ChatCompletionResponse, error)
}

// ToolInvocation is the optional external tool surface, bound to the already
// authenticated principal and resolved tool name.
type ToolInvocation interface {
	Call(context.Context, authorize.Principal, id.ProjectID, string, json.RawMessage) (mcpproxy.CallResult, error)
}

// IdentitySource mints the handler-owned identities.
type IdentitySource interface {
	NewPrincipalID() (id.PrincipalID, error)
	NewVerificationID() (verify.VerificationID, error)
}

// TenantKeySource mints the encrypted derivation key of one tenant. It is the
// KMS boundary; the returned key never carries plaintext.
type TenantKeySource interface {
	NewTenantKey(context.Context, id.TenantID, uint64) (token.TenantKey, error)
}
