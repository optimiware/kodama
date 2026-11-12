// Service-account administration operations.
//
// Admin builds one complete commit unit per declared registry operation:
// service.create, service.update, service.enable and service.disable. Each
// command authorizes against the single operation matrix, re-reads the current
// principal (enablement, scope, role and the execution exclusion), validates
// the project scope, carries the registry authority reads, and applies exactly
// one AdminPort commit. The unit carries the receipt, audit and event intent so
// the adapter commits the service row and its evidence in one transaction.
//
// A disablement carries the mandatory RevokeAll obligation, which clears the
// service's one current nonce/expiry pair in that same transaction. Established
// connections are invalidated only after the commit succeeded; an unacknowledged
// invalidation is retryable intent, never evidence that it ran or did not run.
// An enablement carries no credential and can never resurrect a revoked one.
// Update replaces roles only: it has no enablement input and the built unit
// never carries a credential pair.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Clock is the injected authoritative time source for administration commits.
// Every receipt, audit row, event and unit timestamp comes from it.
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

// ServiceInvalidation names one disabled service whose established connections
// must stop being authorized. It carries no credential material and no durable
// permission: it is a post-commit effect for one committed disablement.
type ServiceInvalidation struct {
	Scope     tenancy.Scope
	ServiceID ServiceID
	At        time.Time
}

// Invalidator delivers one committed disablement to live consumers. It runs
// after the disable commit and is retried on failure; an unacknowledged
// invalidation is never treated as executed or as not executed.
type Invalidator interface {
	InvalidateService(context.Context, ServiceInvalidation) error
}

// AdminConfig binds every service-administration dependency. A missing port is
// a construction failure rather than a partially wired service.
type AdminConfig struct {
	Authorizer    authorize.Authorizer
	Authority     authorize.AuthorityReader
	Principals    identity.PrincipalReader
	Services      ServiceReader
	Port          AdminPort
	Invalidations Invalidator
	Clock         Clock
	IDs           IDSource
}

// Admin builds the complete atomic commit units for service administration. It
// holds no cached authority and no mutable state, so concurrent commands re-read
// current state independently and exactly one versioned commit wins.
type Admin struct {
	authorizer    authorize.Authorizer
	authority     authorize.AuthorityReader
	principals    identity.PrincipalReader
	services      ServiceReader
	port          AdminPort
	invalidations Invalidator
	clock         Clock
	ids           IDSource
}

// NewAdmin validates the injected boundaries and returns the administration
// service.
func NewAdmin(config AdminConfig) (*Admin, error) {
	switch {
	case config.Authorizer == nil:
		return nil, fault.New(fault.InternalFailure, "service administration requires an authorizer")
	case config.Authority == nil:
		return nil, fault.New(fault.InternalFailure, "service administration requires an authority reader")
	case config.Principals == nil:
		return nil, fault.New(fault.InternalFailure, "service administration requires a principal reader")
	case config.Services == nil:
		return nil, fault.New(fault.InternalFailure, "service administration requires a service reader")
	case config.Port == nil:
		return nil, fault.New(fault.InternalFailure, "service administration requires an admin port")
	case config.Invalidations == nil:
		return nil, fault.New(fault.InternalFailure, "service administration requires an invalidation port")
	case config.Clock == nil:
		return nil, fault.New(fault.InternalFailure, "service administration requires a clock")
	case config.IDs == nil:
		return nil, fault.New(fault.InternalFailure, "service administration requires an identity source")
	}
	return &Admin{
		authorizer:    config.Authorizer,
		authority:     config.Authority,
		principals:    config.Principals,
		services:      config.Services,
		port:          config.Port,
		invalidations: config.Invalidations,
		clock:         config.Clock,
		ids:           config.IDs,
	}, nil
}

// The declared registry names used by this capability. They are looked up
// rather than duplicated so the grant, checks and receipt policy always come
// from the single operation matrix.
const (
	operationServiceCreate  authorize.OperationName = "service.create"
	operationServiceUpdate  authorize.OperationName = "service.update"
	operationServiceEnable  authorize.OperationName = "service.enable"
	operationServiceDisable authorize.OperationName = "service.disable"
)

// CreateCommand asks to create one service account in the actor's project.
type CreateCommand struct {
	Actor     authorize.Principal `json:"actor"`
	ID        ServiceID           `json:"id"`
	Scope     tenancy.Scope       `json:"scope"`
	State     State               `json:"state"`
	Roles     []tenancy.Role      `json:"roles,omitempty"`
	CallerKey string              `json:"caller_key"`
	Content   receipt.Content     `json:"content"`
}

// UpdateCommand asks to replace one service account's complete role set at an
// expected version. It deliberately has no enablement or credential input:
// enablement is StatusCommand's job and credentials are token.MutationUnit's.
type UpdateCommand struct {
	Actor           authorize.Principal `json:"actor"`
	Scope           tenancy.Scope       `json:"scope"`
	ServiceID       ServiceID           `json:"service_id"`
	ExpectedVersion uint64              `json:"expected_version"`
	Roles           []tenancy.Role      `json:"roles"`
	CallerKey       string              `json:"caller_key"`
	Content         receipt.Content     `json:"content"`
}

// StatusCommand asks to enable or disable one service account at an expected
// version.
type StatusCommand struct {
	Actor           authorize.Principal `json:"actor"`
	Scope           tenancy.Scope       `json:"scope"`
	ServiceID       ServiceID           `json:"service_id"`
	ExpectedVersion uint64              `json:"expected_version"`
	CallerKey       string              `json:"caller_key"`
	Content         receipt.Content     `json:"content"`
}

// admit validates the presented principal and pins that its kind can satisfy
// the registry grant. It never treats the principal's role snapshot as current
// authority.
func admit(actor authorize.Principal, name authorize.OperationName) (authorize.Operation, error) {
	if err := actor.Validate(); err != nil {
		return authorize.Operation{}, err
	}
	op, ok := authorize.LookupOperation(name)
	if !ok {
		return authorize.Operation{}, fault.New(fault.InternalFailure, "service administration operation is not registered")
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

// authorizeCommand runs the matrix authorization first, then confirms the
// current principal. The current read is authority evidence for this command,
// never durable permission.
func (admin *Admin) authorizeCommand(
	ctx context.Context,
	actor authorize.Principal,
	name authorize.OperationName,
	scope tenancy.Scope,
) (authorize.Operation, error) {
	if err := scope.Validate(); err != nil {
		return authorize.Operation{}, err
	}
	if !scope.ProjectScoped() {
		return authorize.Operation{}, fault.New(fault.InvalidInput, "service administration requires a project scope")
	}
	if actor.Scope != scope {
		return authorize.Operation{}, fault.New(fault.Forbidden, "actor scope does not match the command scope")
	}
	op, err := admit(actor, name)
	if err != nil {
		return authorize.Operation{}, err
	}
	decision, err := admin.authorizer.Authorize(ctx, actor, name, scope)
	if err != nil {
		return authorize.Operation{}, err
	}
	if decision != authorize.DecisionAllow {
		return authorize.Operation{}, fault.New(fault.Forbidden, fmt.Sprintf("principal is not authorized for %q", name))
	}
	if err := admin.confirmActor(ctx, actor, op); err != nil {
		return authorize.Operation{}, err
	}
	return op, nil
}

// confirmActor re-reads the actor's current principal so a previously calculated
// allow never substitutes for current enablement and role. Execution principals
// are refused because every service-administration operation grants no execution
// permission.
func (admin *Admin) confirmActor(ctx context.Context, actor authorize.Principal, op authorize.Operation) error {
	principal, err := identity.Current(ctx, admin.principals, identity.PrincipalRequest{
		Scope:       actor.Scope,
		PrincipalID: actor.ID,
	})
	if err != nil {
		return err
	}
	if principal.Kind != actor.Kind {
		return fault.New(fault.Forbidden, "current principal kind does not match the authenticated actor")
	}
	if principal.Kind == identity.KindExecution {
		if op.Requires.Execution == authorize.ExecNone {
			return fault.New(fault.Forbidden, fmt.Sprintf("execution principal has no permission for %q", op.Name))
		}
		if principal.ExecutionID == "" || principal.ExecutionID != actor.ExecutionID {
			return fault.New(fault.Forbidden, "execution principal identity does not match the authenticated actor")
		}
		return nil
	}
	if !grantHeld(principal, op.Requires) {
		return fault.New(fault.Forbidden, fmt.Sprintf("principal does not currently hold the grant for %q", op.Name))
	}
	return nil
}

func grantHeld(principal identity.Principal, requires authorize.Requires) bool {
	switch principal.Kind {
	case identity.KindHuman:
		if slices.ContainsFunc(requires.HumanRoles, principal.HasRole) {
			return true
		}
	case identity.KindService:
		if slices.ContainsFunc(requires.ServiceRoles, principal.HasRole) {
			return true
		}
	}
	return false
}

// authorityReads builds the current-authority reads for one registry operation.
// Every read names the operation and its required grant so the adapter can
// re-evaluate the exact grant inside the transaction.
func authorityReads(op authorize.Operation, scope tenancy.Scope, principal id.PrincipalID) []authorize.AuthorityRead {
	reads := make([]authorize.AuthorityRead, 0, len(op.Authority))
	for _, check := range op.Authority {
		reads = append(reads, authorize.AuthorityRead{
			Operation:   op.Name,
			Required:    op.Requires,
			Check:       check,
			Scope:       scope,
			PrincipalID: principal,
		})
	}
	return reads
}

// confirm reads the supplied non-versioned authority facts before the commit.
// The same reads travel in the unit so the adapter re-evaluates them inside its
// transaction.
func (admin *Admin) confirm(ctx context.Context, reads []authorize.AuthorityRead) error {
	if len(reads) == 0 {
		return nil
	}
	facts, err := admin.authority.Read(ctx, reads)
	if err != nil {
		return err
	}
	return authorize.Confirm(reads, facts)
}

// buildReceipt builds the scoped idempotency receipt for one commit unit. The
// durable result is encoded with the unit so an identical retry converges on
// the same body without repeating the effect.
func (admin *Admin) buildReceipt(
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
	receiptID, err := admin.ids.NewReceiptID()
	if err != nil {
		return receipt.Record{}, err
	}
	body, err := json.Marshal(result)
	if err != nil {
		return receipt.Record{}, fault.Wrap(fault.InternalFailure, "service administration receipt result could not be encoded", err)
	}
	return receipt.Record{ID: receiptID, Key: key, ContentHash: fingerprint, Result: body, At: at}, nil
}

// buildAudit builds the single immutable audit row committed with the change.
func (admin *Admin) buildAudit(scope tenancy.Scope, actor id.PrincipalID, operation string, at time.Time) (event.Audit, error) {
	eventID, err := admin.ids.NewEventID()
	if err != nil {
		return event.Audit{}, err
	}
	return event.Audit{ID: eventID, Scope: scope, Actor: actor, Operation: operation, At: at}, nil
}

// buildEvent builds the service.changed publication intent committed with the
// change. It is intent, never evidence of publication.
func (admin *Admin) buildEvent(scope tenancy.Scope, subject string, subjectVersion uint64, actor id.PrincipalID, at time.Time) (event.Envelope, error) {
	eventID, err := admin.ids.NewEventID()
	if err != nil {
		return event.Envelope{}, err
	}
	envelope := event.Envelope{
		ID:             eventID,
		Type:           event.TypeServiceChanged,
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

func publishEffect() event.PostCommitEffect {
	return event.PostCommitEffect{Kind: event.PostPublishEvent}
}

// readTarget reads one service's current committed state and confirms it is the
// command's target in the command scope. The fact is current evidence for this
// command, never durable permission.
func (admin *Admin) readTarget(ctx context.Context, scope tenancy.Scope, serviceID ServiceID) (Service, error) {
	if _, err := ParseServiceID(string(serviceID)); err != nil {
		return Service{}, err
	}
	current, err := admin.services.ReadService(ctx, ServiceReadRequest{Scope: scope, ServiceID: serviceID})
	if err != nil {
		return Service{}, err
	}
	if err := current.Validate(); err != nil {
		return Service{}, fault.Wrap(fault.InvalidInput, "current service is invalid", err)
	}
	if current.ID != serviceID || current.Scope != scope {
		return Service{}, fault.New(fault.Forbidden, "service is outside the command scope")
	}
	return current, nil
}

// Create commits one new service account together with its receipt, audit and
// event intent. The created service starts at version 1 with no credential.
func (admin *Admin) Create(ctx context.Context, command CreateCommand) (CreateResult, error) {
	op, err := admin.authorizeCommand(ctx, command.Actor, operationServiceCreate, command.Scope)
	if err != nil {
		return CreateResult{}, err
	}
	created, err := NewService(CreateRequest{
		ID:    command.ID,
		Scope: command.Scope,
		State: command.State,
		Roles: command.Roles,
	})
	if err != nil {
		return CreateResult{}, err
	}
	reads := authorityReads(op, command.Scope, command.Actor.ID)
	if err := admin.confirm(ctx, reads); err != nil {
		return CreateResult{}, err
	}
	at := admin.clock.Now()
	if at.IsZero() {
		return CreateResult{}, fault.New(fault.InvalidInput, "service administration clock returned zero time")
	}
	result := CreateResult{ServiceID: created.ID, Version: created.Version}
	record, err := admin.buildReceipt(command.Scope, command.Actor.ID, string(operationServiceCreate), command.CallerKey, command.Content, at, result)
	if err != nil {
		return CreateResult{}, err
	}
	audit, err := admin.buildAudit(command.Scope, command.Actor.ID, string(operationServiceCreate), at)
	if err != nil {
		return CreateResult{}, err
	}
	envelope, err := admin.buildEvent(command.Scope, string(created.ID), created.Version, command.Actor.ID, at)
	if err != nil {
		return CreateResult{}, err
	}
	unit := CreateUnit{
		Owner:      command.Scope,
		Service:    created,
		Actor:      command.Actor.ID,
		At:         at,
		Receipt:    record,
		Audit:      []event.Audit{audit},
		Events:     []event.Envelope{envelope},
		Authority:  reads,
		PostCommit: []event.PostCommitEffect{publishEffect()},
	}
	if err := unit.Validate(); err != nil {
		return CreateResult{}, err
	}
	return admin.port.Create(ctx, unit)
}

// Update commits one complete role replacement at the expected version. The
// next value keeps the current enablement and never carries a credential pair.
func (admin *Admin) Update(ctx context.Context, command UpdateCommand) (UpdateResult, error) {
	op, err := admin.authorizeCommand(ctx, command.Actor, operationServiceUpdate, command.Scope)
	if err != nil {
		return UpdateResult{}, err
	}
	if command.ExpectedVersion == 0 {
		return UpdateResult{}, fault.New(fault.InvalidInput, "service update expected version is zero")
	}
	if command.Roles == nil {
		return UpdateResult{}, fault.New(fault.InvalidInput, "service update requires the complete role set")
	}
	current, err := admin.readTarget(ctx, command.Scope, command.ServiceID)
	if err != nil {
		return UpdateResult{}, err
	}
	next := Service{
		ID:      command.ServiceID,
		Scope:   command.Scope,
		State:   current.State,
		Roles:   copyRoles(command.Roles),
		Version: command.ExpectedVersion + 1,
	}
	if err := next.Validate(); err != nil {
		return UpdateResult{}, err
	}
	reads := authorityReads(op, command.Scope, command.Actor.ID)
	if err := admin.confirm(ctx, reads); err != nil {
		return UpdateResult{}, err
	}
	at := admin.clock.Now()
	if at.IsZero() {
		return UpdateResult{}, fault.New(fault.InvalidInput, "service administration clock returned zero time")
	}
	result := UpdateResult{Version: next.Version}
	record, err := admin.buildReceipt(command.Scope, command.Actor.ID, string(operationServiceUpdate), command.CallerKey, command.Content, at, result)
	if err != nil {
		return UpdateResult{}, err
	}
	audit, err := admin.buildAudit(command.Scope, command.Actor.ID, string(operationServiceUpdate), at)
	if err != nil {
		return UpdateResult{}, err
	}
	envelope, err := admin.buildEvent(command.Scope, string(command.ServiceID), next.Version, command.Actor.ID, at)
	if err != nil {
		return UpdateResult{}, err
	}
	unit := UpdateUnit{
		Owner:           command.Scope,
		ServiceID:       command.ServiceID,
		ExpectedVersion: command.ExpectedVersion,
		ExpectedState:   current.State,
		Service:         next,
		Actor:           command.Actor.ID,
		At:              at,
		Receipt:         record,
		Audit:           []event.Audit{audit},
		Events:          []event.Envelope{envelope},
		Authority:       reads,
		PostCommit:      []event.PostCommitEffect{publishEffect()},
	}
	if err := unit.Validate(); err != nil {
		return UpdateResult{}, err
	}
	return admin.port.Update(ctx, unit)
}

// Enable marks the service enabled. It never carries a credential and cannot
// resurrect a revoked one.
func (admin *Admin) Enable(ctx context.Context, command StatusCommand) (StatusResult, error) {
	return admin.setStatus(ctx, command, operationServiceEnable, StateEnabled)
}

// Disable marks the service disabled with the mandatory RevokeAll obligation,
// then invalidates established connections after the commit.
func (admin *Admin) Disable(ctx context.Context, command StatusCommand) (StatusResult, error) {
	return admin.setStatus(ctx, command, operationServiceDisable, StateDisabled)
}

func (admin *Admin) setStatus(
	ctx context.Context,
	command StatusCommand,
	name authorize.OperationName,
	state State,
) (StatusResult, error) {
	op, err := admin.authorizeCommand(ctx, command.Actor, name, command.Scope)
	if err != nil {
		return StatusResult{}, err
	}
	if command.ExpectedVersion == 0 {
		return StatusResult{}, fault.New(fault.InvalidInput, "service status expected version is zero")
	}
	current, err := admin.readTarget(ctx, command.Scope, command.ServiceID)
	if err != nil {
		return StatusResult{}, err
	}
	reads := authorityReads(op, command.Scope, command.Actor.ID)
	if err := admin.confirm(ctx, reads); err != nil {
		return StatusResult{}, err
	}
	at := admin.clock.Now()
	if at.IsZero() {
		return StatusResult{}, fault.New(fault.InvalidInput, "service administration clock returned zero time")
	}
	result := StatusResult{State: state, Version: command.ExpectedVersion + 1}
	if state == StateDisabled && serviceHasCredential(current) {
		result.RevokedCredentials = 1
	}
	record, err := admin.buildReceipt(command.Scope, command.Actor.ID, string(name), command.CallerKey, command.Content, at, result)
	if err != nil {
		return StatusResult{}, err
	}
	audit, err := admin.buildAudit(command.Scope, command.Actor.ID, string(name), at)
	if err != nil {
		return StatusResult{}, err
	}
	envelope, err := admin.buildEvent(command.Scope, string(command.ServiceID), result.Version, command.Actor.ID, at)
	if err != nil {
		return StatusResult{}, err
	}
	unit := StatusUnit{
		Owner:           command.Scope,
		ServiceID:       command.ServiceID,
		ExpectedVersion: command.ExpectedVersion,
		State:           state,
		Actor:           command.Actor.ID,
		At:              at,
		Receipt:         record,
		Audit:           []event.Audit{audit},
		Events:          []event.Envelope{envelope},
		Authority:       reads,
		PostCommit:      []event.PostCommitEffect{publishEffect()},
	}
	if state == StateDisabled {
		unit.RevokeAll = &RevokeAll{}
	}
	if err := unit.Validate(); err != nil {
		return StatusResult{}, err
	}
	committed, err := admin.port.SetStatus(ctx, unit)
	if err != nil {
		return StatusResult{}, err
	}
	if state == StateDisabled {
		invalidation := ServiceInvalidation{Scope: command.Scope, ServiceID: command.ServiceID, At: at}
		if err := admin.invalidations.InvalidateService(ctx, invalidation); err != nil {
			return committed, fault.Wrap(fault.UncertainExternalEffect, "service invalidation failed", err)
		}
	}
	return committed, nil
}
