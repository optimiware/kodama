package tenancyadmin

import (
	"context"
	"encoding/json"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// Clock is the injected authoritative time source for administration commits.
// Every receipt, audit row, event and unit timestamp comes from it, so a test
// can pin one deterministic commit time instead of reading the wall clock.
type Clock interface {
	Now() time.Time
}

// IDSource allocates the durable audit, receipt and event identities a commit
// unit carries. Identity generation is injected so production uses secure
// randomness while a test can use a deterministic sequence.
type IDSource interface {
	NewEventID() (id.EventID, error)
	NewReceiptID() (id.ReceiptID, error)
}

// Config is the complete set of injected boundaries a tenancy administration
// service needs. Every field is required: a missing port is a construction
// failure rather than a partially wired service.
type Config struct {
	Clock             Clock
	IDs               IDSource
	TenantCreate      TenantCreatePort
	TenantKeyRotation TenantKeyRotationPort
	ProjectCreate     ProjectCreatePort
	ProjectUpdate     ProjectUpdatePort
	MembershipGrant   MembershipGrantPort
	MembershipRevoke  MembershipRevokePort
	MembershipQuery   MembershipQueryPort
}

// Service builds the complete atomic commit units for tenant, project and
// membership administration. It never authorizes from a cached role snapshot:
// each unit carries the registry operation's required grant and the
// current-authority reads the adapter must re-read inside the transaction.
type Service struct {
	clock             Clock
	ids               IDSource
	tenantCreate      TenantCreatePort
	tenantKeyRotation TenantKeyRotationPort
	projectCreate     ProjectCreatePort
	projectUpdate     ProjectUpdatePort
	membershipGrant   MembershipGrantPort
	membershipRevoke  MembershipRevokePort
	membershipQuery   MembershipQueryPort
}

// NewService validates the injected boundaries and returns the administration
// service. A missing clock, identity source or port is refused.
func NewService(config Config) (*Service, error) {
	switch {
	case config.Clock == nil:
		return nil, fault.New(fault.InternalFailure, "tenancy administration requires a clock")
	case config.IDs == nil:
		return nil, fault.New(fault.InternalFailure, "tenancy administration requires an identity source")
	case config.TenantCreate == nil:
		return nil, fault.New(fault.InternalFailure, "tenancy administration requires a tenant create port")
	case config.TenantKeyRotation == nil:
		return nil, fault.New(fault.InternalFailure, "tenancy administration requires a tenant key rotation port")
	case config.ProjectCreate == nil:
		return nil, fault.New(fault.InternalFailure, "tenancy administration requires a project create port")
	case config.ProjectUpdate == nil:
		return nil, fault.New(fault.InternalFailure, "tenancy administration requires a project update port")
	case config.MembershipGrant == nil:
		return nil, fault.New(fault.InternalFailure, "tenancy administration requires a membership grant port")
	case config.MembershipRevoke == nil:
		return nil, fault.New(fault.InternalFailure, "tenancy administration requires a membership revoke port")
	case config.MembershipQuery == nil:
		return nil, fault.New(fault.InternalFailure, "tenancy administration requires a membership query port")
	}
	return &Service{
		clock:             config.Clock,
		ids:               config.IDs,
		tenantCreate:      config.TenantCreate,
		tenantKeyRotation: config.TenantKeyRotation,
		projectCreate:     config.ProjectCreate,
		projectUpdate:     config.ProjectUpdate,
		membershipGrant:   config.MembershipGrant,
		membershipRevoke:  config.MembershipRevoke,
		membershipQuery:   config.MembershipQuery,
	}, nil
}

// The declared registry names used by this capability. They are looked up
// rather than duplicated so the grant, checks and receipt policy always come
// from the single operation matrix.
const (
	operationTenantCreate     authorize.OperationName = "tenant.create"
	operationTenantRotateKey  authorize.OperationName = "tenant.rotate_key"
	operationProjectCreate    authorize.OperationName = "project.create"
	operationProjectUpdate    authorize.OperationName = "project.update"
	operationMembershipGrant  authorize.OperationName = "membership.grant"
	operationMembershipRevoke authorize.OperationName = "membership.revoke"
	operationMembershipList   authorize.OperationName = "membership.list"
)

// admit validates the presented principal and pins that its kind can satisfy
// the registry grant. It never treats the principal's role snapshot as current
// authority: the returned unit carries the registry grant and the authority
// reads the transaction must re-evaluate.
func admit(actor authorize.Principal, name authorize.OperationName) (authorize.Operation, error) {
	if err := actor.Validate(); err != nil {
		return authorize.Operation{}, err
	}
	op, ok := authorize.LookupOperation(name)
	if !ok {
		return authorize.Operation{}, fault.New(fault.InternalFailure, "tenancy administration operation is not registered")
	}
	switch actor.Kind {
	case identity.KindHuman:
		if len(op.Requires.HumanRoles) == 0 {
			return authorize.Operation{}, fault.New(fault.Forbidden, "the operation grants no human role")
		}
	case identity.KindService:
		if len(op.Requires.ServiceRoles) == 0 {
			return authorize.Operation{}, fault.New(fault.Forbidden, "the operation grants no service role")
		}
	case identity.KindExecution:
		if op.Requires.Execution == authorize.ExecNone {
			return authorize.Operation{}, fault.New(fault.Forbidden, "the operation grants no execution permission")
		}
	default:
		return authorize.Operation{}, fault.New(fault.Forbidden, "unknown principal kind")
	}
	return op, nil
}

// authorityReads builds the current-authority reads for one registry operation.
// Every read names the operation and its required grant so the adapter can
// re-evaluate the exact grant instead of guessing from a bare check name.
// Versioned checks carry the target and expected version; other checks leave
// them empty.
func authorityReads(
	op authorize.Operation,
	scope tenancy.Scope,
	principal id.PrincipalID,
	target string,
	expected uint64,
) []authorize.AuthorityRead {
	reads := make([]authorize.AuthorityRead, 0, len(op.Authority))
	for _, check := range op.Authority {
		read := authorize.AuthorityRead{
			Operation:   op.Name,
			Required:    op.Requires,
			Check:       check,
			Scope:       scope,
			PrincipalID: principal,
		}
		switch check {
		case authorize.CheckBindingVersion,
			authorize.CheckTaskVersion,
			authorize.CheckExecutionVersion,
			authorize.CheckPipelineVersion:
			read.TargetID = target
			read.ExpectedVersion = expected
		case authorize.CheckResourceOwnership:
			read.TargetID = target
		}
		reads = append(reads, read)
	}
	return reads
}

// buildReceipt builds the scoped idempotency receipt for one commit unit. The
// durable result is encoded with the unit so an identical retry converges on
// the same body without repeating the effect.
func (s *Service) buildReceipt(
	scope tenancy.Scope,
	principal id.PrincipalID,
	operation string,
	callerKey string,
	content receipt.Content,
	at time.Time,
	result any,
) (receipt.Record, error) {
	key := receipt.Key{
		TenantID:    scope.TenantID,
		ProjectID:   scope.ProjectID,
		PrincipalID: principal,
		Operation:   operation,
		CallerKey:   callerKey,
	}
	if err := key.Validate(); err != nil {
		return receipt.Record{}, err
	}
	fingerprint, err := receipt.Fingerprint(content)
	if err != nil {
		return receipt.Record{}, err
	}
	receiptID, err := s.ids.NewReceiptID()
	if err != nil {
		return receipt.Record{}, err
	}
	body, err := json.Marshal(result)
	if err != nil {
		return receipt.Record{}, fault.Wrap(fault.InternalFailure, "administration receipt result could not be encoded", err)
	}
	return receipt.Record{ID: receiptID, Key: key, ContentHash: fingerprint, Result: body, At: at}, nil
}

// buildAudit builds the single immutable audit row committed with the change.
func (s *Service) buildAudit(
	scope tenancy.Scope,
	actor id.PrincipalID,
	operation string,
	at time.Time,
) (event.Audit, error) {
	eventID, err := s.ids.NewEventID()
	if err != nil {
		return event.Audit{}, err
	}
	return event.Audit{ID: eventID, Scope: scope, Actor: actor, Operation: operation, At: at}, nil
}

// buildConfigEvent builds the config.changed publication intent committed with
// the change. It is intent, never evidence of publication.
func (s *Service) buildConfigEvent(
	scope tenancy.Scope,
	subject string,
	subjectVersion uint64,
	actor id.PrincipalID,
	at time.Time,
) (event.Envelope, error) {
	eventID, err := s.ids.NewEventID()
	if err != nil {
		return event.Envelope{}, err
	}
	envelope := event.Envelope{
		ID:             eventID,
		Type:           event.TypeConfigChanged,
		Source:         event.Source{Kind: event.SourceShell, PrincipalID: actor},
		At:             at,
		Scope:          scope,
		SubjectID:      subject,
		SubjectVersion: subjectVersion,
	}
	if err := envelope.Validate(); err != nil {
		return event.Envelope{}, err
	}
	return envelope, nil
}

// publishEffect requests the post-commit publication of already-committed event
// rows. It is retryable intent.
func publishEffect() event.PostCommitEffect {
	return event.PostCommitEffect{Kind: event.PostPublishEvent}
}

// CreateTenant commits one new tenant together with its initial encrypted
// derivation key, receipt, audit and event intent.
func (s *Service) CreateTenant(ctx context.Context, command TenantCreateCommand) (TenantCreateResult, error) {
	op, err := admit(command.Actor, operationTenantCreate)
	if err != nil {
		return TenantCreateResult{}, err
	}
	if !command.Actor.Scope.TenantScoped() {
		return TenantCreateResult{}, fault.New(fault.Forbidden, "tenant creation requires a tenant-scoped actor")
	}
	tenant := tenancy.Tenant{ID: command.TenantID, Name: command.Name, Version: 1}
	if err := tenant.Validate(); err != nil {
		return TenantCreateResult{}, err
	}
	key := cloneTenantKey(command.Key)
	if key.TenantID != tenant.ID {
		return TenantCreateResult{}, fault.New(fault.InvalidInput, "tenant creation key tenant id does not match the tenant")
	}
	if key.Version != 1 {
		return TenantCreateResult{}, fault.New(fault.InvalidInput, "tenant creation key must start at version 1")
	}
	if err := key.Validate(); err != nil {
		return TenantCreateResult{}, err
	}
	scope := tenancy.Scope{TenantID: tenant.ID}
	at := s.clock.Now()
	result := TenantCreateResult{TenantID: tenant.ID, Version: tenant.Version}
	record, err := s.buildReceipt(scope, command.Actor.ID, string(operationTenantCreate), command.CallerKey, command.Content, at, result)
	if err != nil {
		return TenantCreateResult{}, err
	}
	audit, err := s.buildAudit(scope, command.Actor.ID, string(operationTenantCreate), at)
	if err != nil {
		return TenantCreateResult{}, err
	}
	envelope, err := s.buildConfigEvent(scope, string(tenant.ID), tenant.Version, command.Actor.ID, at)
	if err != nil {
		return TenantCreateResult{}, err
	}
	unit := TenantCreateUnit{
		Tenant:     tenant,
		Key:        key,
		At:         at,
		Receipt:    record,
		Audit:      []event.Audit{audit},
		Events:     []event.Envelope{envelope},
		Authority:  authorityReads(op, scope, command.Actor.ID, "", 0),
		PostCommit: []event.PostCommitEffect{publishEffect()},
	}
	if err := unit.Validate(); err != nil {
		return TenantCreateResult{}, err
	}
	committed, err := s.tenantCreate.CreateTenant(ctx, unit)
	if err != nil {
		return TenantCreateResult{}, err
	}
	return committed, nil
}

// RotateTenantKey commits the next tenant derivation-key version together with
// the next tenant version, current-authority reads, receipt, audit, event and
// credential-refresh intent.
func (s *Service) RotateTenantKey(ctx context.Context, command TenantKeyRotationCommand) (TenantKeyRotationResult, error) {
	op, err := admit(command.Actor, operationTenantRotateKey)
	if err != nil {
		return TenantKeyRotationResult{}, err
	}
	if !command.Actor.Scope.TenantScoped() {
		return TenantKeyRotationResult{}, fault.New(fault.Forbidden, "tenant key rotation requires a tenant-scoped actor")
	}
	if command.TenantID != command.Actor.Scope.TenantID {
		return TenantKeyRotationResult{}, fault.New(fault.Forbidden, "tenant key rotation target is outside the actor scope")
	}
	if command.ExpectedVersion == 0 {
		return TenantKeyRotationResult{}, fault.New(fault.InvalidInput, "tenant key rotation expected version is zero")
	}
	tenant := tenancy.Tenant{ID: command.TenantID, Name: command.Name, Version: command.ExpectedVersion + 1}
	if err := tenant.Validate(); err != nil {
		return TenantKeyRotationResult{}, err
	}
	write := token.TenantKeyWrite{
		TenantID:        tenant.ID,
		ExpectedVersion: command.ExpectedVersion,
		Key:             cloneTenantKey(command.Key),
	}
	if err := write.Validate(); err != nil {
		return TenantKeyRotationResult{}, err
	}
	scope := tenancy.Scope{TenantID: tenant.ID}
	at := s.clock.Now()
	result := TenantKeyRotationResult{TenantVersion: tenant.Version, KeyVersion: write.Key.Version}
	record, err := s.buildReceipt(scope, command.Actor.ID, string(operationTenantRotateKey), command.CallerKey, command.Content, at, result)
	if err != nil {
		return TenantKeyRotationResult{}, err
	}
	audit, err := s.buildAudit(scope, command.Actor.ID, string(operationTenantRotateKey), at)
	if err != nil {
		return TenantKeyRotationResult{}, err
	}
	envelope, err := s.buildConfigEvent(scope, string(tenant.ID), tenant.Version, command.Actor.ID, at)
	if err != nil {
		return TenantKeyRotationResult{}, err
	}
	unit := TenantKeyRotationUnit{
		Tenant:          tenant,
		Key:             write,
		ExpectedVersion: command.ExpectedVersion,
		At:              at,
		Receipt:         record,
		Audit:           []event.Audit{audit},
		Events:          []event.Envelope{envelope},
		Authority:       authorityReads(op, scope, command.Actor.ID, "", 0),
		PostCommit: []event.PostCommitEffect{
			publishEffect(),
			{Kind: event.PostRefreshCredential, Target: string(tenant.ID)},
		},
	}
	if err := unit.Validate(); err != nil {
		return TenantKeyRotationResult{}, err
	}
	committed, err := s.tenantKeyRotation.RotateTenantKey(ctx, unit)
	if err != nil {
		return TenantKeyRotationResult{}, err
	}
	return committed, nil
}

// CreateProject commits one new project in the actor's tenant.
func (s *Service) CreateProject(ctx context.Context, command ProjectCreateCommand) (ProjectCreateResult, error) {
	op, err := admit(command.Actor, operationProjectCreate)
	if err != nil {
		return ProjectCreateResult{}, err
	}
	if !command.Actor.Scope.TenantScoped() {
		return ProjectCreateResult{}, fault.New(fault.Forbidden, "project creation requires a tenant-scoped actor")
	}
	project := tenancy.Project{
		ID:       command.ProjectID,
		TenantID: command.Actor.Scope.TenantID,
		Name:     command.Name,
		Version:  1,
	}
	if err := project.Validate(); err != nil {
		return ProjectCreateResult{}, err
	}
	scope := tenancy.Scope{TenantID: project.TenantID}
	at := s.clock.Now()
	result := ProjectCreateResult{ProjectID: project.ID, Version: project.Version}
	record, err := s.buildReceipt(scope, command.Actor.ID, string(operationProjectCreate), command.CallerKey, command.Content, at, result)
	if err != nil {
		return ProjectCreateResult{}, err
	}
	audit, err := s.buildAudit(scope, command.Actor.ID, string(operationProjectCreate), at)
	if err != nil {
		return ProjectCreateResult{}, err
	}
	envelope, err := s.buildConfigEvent(scope, string(project.ID), project.Version, command.Actor.ID, at)
	if err != nil {
		return ProjectCreateResult{}, err
	}
	unit := ProjectCreateUnit{
		Project:    project,
		At:         at,
		Receipt:    record,
		Audit:      []event.Audit{audit},
		Events:     []event.Envelope{envelope},
		Authority:  authorityReads(op, scope, command.Actor.ID, "", 0),
		PostCommit: []event.PostCommitEffect{publishEffect()},
	}
	committed, err := s.projectCreate.CreateProject(ctx, unit)
	if err != nil {
		return ProjectCreateResult{}, err
	}
	return committed, nil
}

// UpdateProject commits one versioned project change in the actor's tenant.
func (s *Service) UpdateProject(ctx context.Context, command ProjectUpdateCommand) (ProjectUpdateResult, error) {
	op, err := admit(command.Actor, operationProjectUpdate)
	if err != nil {
		return ProjectUpdateResult{}, err
	}
	if !command.Actor.Scope.TenantScoped() {
		return ProjectUpdateResult{}, fault.New(fault.Forbidden, "project update requires a tenant-scoped actor")
	}
	if command.ExpectedVersion == 0 {
		return ProjectUpdateResult{}, fault.New(fault.InvalidInput, "project update expected version is zero")
	}
	project := tenancy.Project{
		ID:       command.ProjectID,
		TenantID: command.Actor.Scope.TenantID,
		Name:     command.Name,
		Version:  command.ExpectedVersion + 1,
	}
	if err := project.Validate(); err != nil {
		return ProjectUpdateResult{}, err
	}
	scope := tenancy.Scope{TenantID: project.TenantID}
	at := s.clock.Now()
	result := ProjectUpdateResult{Version: project.Version}
	record, err := s.buildReceipt(scope, command.Actor.ID, string(operationProjectUpdate), command.CallerKey, command.Content, at, result)
	if err != nil {
		return ProjectUpdateResult{}, err
	}
	audit, err := s.buildAudit(scope, command.Actor.ID, string(operationProjectUpdate), at)
	if err != nil {
		return ProjectUpdateResult{}, err
	}
	envelope, err := s.buildConfigEvent(scope, string(project.ID), project.Version, command.Actor.ID, at)
	if err != nil {
		return ProjectUpdateResult{}, err
	}
	unit := ProjectUpdateUnit{
		Owner:           scope,
		ProjectID:       project.ID,
		ExpectedVersion: command.ExpectedVersion,
		Project:         project,
		At:              at,
		Receipt:         record,
		Audit:           []event.Audit{audit},
		Events:          []event.Envelope{envelope},
		Authority:       authorityReads(op, scope, command.Actor.ID, "", 0),
		PostCommit:      []event.PostCommitEffect{publishEffect()},
	}
	committed, err := s.projectUpdate.UpdateProject(ctx, unit)
	if err != nil {
		return ProjectUpdateResult{}, err
	}
	return committed, nil
}

// GrantMembership creates or replaces one membership grant and advances the
// membership version.
func (s *Service) GrantMembership(ctx context.Context, command MembershipGrantCommand) (MembershipGrantResult, error) {
	op, err := admit(command.Actor, operationMembershipGrant)
	if err != nil {
		return MembershipGrantResult{}, err
	}
	membership := tenancy.Membership{
		Scope:       command.Membership.Scope,
		PrincipalID: command.Membership.PrincipalID,
		Roles:       append([]tenancy.Role(nil), command.Membership.Roles...),
		Version:     command.ExpectedVersion + 1,
	}
	if err := membership.Validate(); err != nil {
		return MembershipGrantResult{}, err
	}
	if !withinScope(command.Actor.Scope, membership.Scope) {
		return MembershipGrantResult{}, fault.New(fault.Forbidden, "membership grant scope is outside the actor scope")
	}
	scope := membership.Scope
	at := s.clock.Now()
	result := MembershipGrantResult{Version: membership.Version}
	record, err := s.buildReceipt(scope, command.Actor.ID, string(operationMembershipGrant), command.CallerKey, command.Content, at, result)
	if err != nil {
		return MembershipGrantResult{}, err
	}
	audit, err := s.buildAudit(scope, command.Actor.ID, string(operationMembershipGrant), at)
	if err != nil {
		return MembershipGrantResult{}, err
	}
	envelope, err := s.buildConfigEvent(scope, string(membership.PrincipalID), membership.Version, command.Actor.ID, at)
	if err != nil {
		return MembershipGrantResult{}, err
	}
	unit := MembershipGrantUnit{
		Owner:           scope,
		Membership:      membership,
		ExpectedVersion: command.ExpectedVersion,
		At:              at,
		Receipt:         record,
		Audit:           []event.Audit{audit},
		Events:          []event.Envelope{envelope},
		Authority:       authorityReads(op, scope, command.Actor.ID, "", 0),
		PostCommit:      []event.PostCommitEffect{publishEffect()},
	}
	committed, err := s.membershipGrant.GrantMembership(ctx, unit)
	if err != nil {
		return MembershipGrantResult{}, err
	}
	return committed, nil
}

// RevokeMembership removes one current membership grant and advances its
// version.
func (s *Service) RevokeMembership(ctx context.Context, command MembershipRevokeCommand) (MembershipRevokeResult, error) {
	op, err := admit(command.Actor, operationMembershipRevoke)
	if err != nil {
		return MembershipRevokeResult{}, err
	}
	scope := command.Scope
	if err := scope.Validate(); err != nil {
		return MembershipRevokeResult{}, err
	}
	if _, err := id.ParsePrincipalID(string(command.PrincipalID)); err != nil {
		return MembershipRevokeResult{}, fault.Wrap(fault.InvalidInput, "membership revocation principal id is invalid", err)
	}
	if command.ExpectedVersion == 0 {
		return MembershipRevokeResult{}, fault.New(fault.InvalidInput, "membership revocation expected version is zero")
	}
	if !withinScope(command.Actor.Scope, scope) {
		return MembershipRevokeResult{}, fault.New(fault.Forbidden, "membership revocation scope is outside the actor scope")
	}
	version := command.ExpectedVersion + 1
	at := s.clock.Now()
	result := MembershipRevokeResult{Version: version}
	record, err := s.buildReceipt(scope, command.Actor.ID, string(operationMembershipRevoke), command.CallerKey, command.Content, at, result)
	if err != nil {
		return MembershipRevokeResult{}, err
	}
	audit, err := s.buildAudit(scope, command.Actor.ID, string(operationMembershipRevoke), at)
	if err != nil {
		return MembershipRevokeResult{}, err
	}
	envelope, err := s.buildConfigEvent(scope, string(command.PrincipalID), version, command.Actor.ID, at)
	if err != nil {
		return MembershipRevokeResult{}, err
	}
	unit := MembershipRevokeUnit{
		Owner:           scope,
		PrincipalID:     command.PrincipalID,
		ExpectedVersion: command.ExpectedVersion,
		At:              at,
		Receipt:         record,
		Audit:           []event.Audit{audit},
		Events:          []event.Envelope{envelope},
		Authority:       authorityReads(op, scope, command.Actor.ID, "", 0),
		PostCommit:      []event.PostCommitEffect{publishEffect()},
	}
	committed, err := s.membershipRevoke.RevokeMembership(ctx, unit)
	if err != nil {
		return MembershipRevokeResult{}, err
	}
	return committed, nil
}

// ListMemberships returns one authoritative, stably paginated membership page
// for the caller's current scope.
func (s *Service) ListMemberships(ctx context.Context, command MembershipListCommand) (MembershipPage, error) {
	if _, err := admit(command.Actor, operationMembershipList); err != nil {
		return MembershipPage{}, err
	}
	query := MembershipQuery{
		Scope:       command.Scope,
		PrincipalID: command.PrincipalID,
		Cursor:      command.Cursor,
		Limit:       command.Limit,
	}
	if err := query.Validate(); err != nil {
		return MembershipPage{}, err
	}
	if !withinScope(command.Actor.Scope, query.Scope) {
		return MembershipPage{}, fault.New(fault.Forbidden, "membership query scope is outside the actor scope")
	}
	page, err := s.membershipQuery.ListMemberships(ctx, query)
	if err != nil {
		return MembershipPage{}, err
	}
	return cloneMembershipPage(page), nil
}
