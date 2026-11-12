// Registry-aware service credential operations.
//
// service.credential.issue, service.credential.rotate and
// service.credential.revoke are public registry operations, not raw Manager
// calls. CredentialAdmin authorizes the human Admin against the declared
// matrix, re-reads the current principal and the target service/credential
// state, builds the receipt, audit, event and authority-read intent, and
// delegates the atomic nonce/expiry mutation to the existing token.Manager. The
// Manager keeps the TTL, nonce, derivation, delivery and replay rules
// unchanged.
//
// Authorization, current-principal confirmation and the disabled-service
// refusal run before any mutation port call. The durable receipt is then
// consulted before the credential-presence preflight, so an equal-content retry
// after a lost response replays the original secret-free result instead of
// failing on the credential its own commit installed.
//
// The freshly derived token is returned only by the issuing response. Receipts,
// audit rows, events and errors never carry a stored secret: the durable receipt
// body is the secret-free result, and a replayed mutation returns no token.
package token

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
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// IDSource allocates the durable audit, receipt and event identities a
// credential commit unit carries.
type IDSource interface {
	NewEventID() (id.EventID, error)
	NewReceiptID() (id.ReceiptID, error)
}

// CredentialManager is the existing credential mutation path. The Manager
// implements it; a test can substitute a recording fake.
type CredentialManager interface {
	Issue(context.Context, IssueRequest) (IssueResult, error)
	Rotate(context.Context, IssueRequest) (IssueResult, error)
	Revoke(context.Context, RevokeRequest) (RevokeResult, error)
}

// CredentialAdminConfig binds every registry-aware credential dependency.
type CredentialAdminConfig struct {
	Authorizer  authorize.Authorizer
	Authority   authorize.AuthorityReader
	Principals  identity.PrincipalReader
	Services    service.ServiceReader
	Credentials CredentialReader
	Manager     CredentialManager
	Receipts    ReceiptLookup
	Clock       Clock
	IDs         IDSource
}

// CredentialAdmin authorizes and builds the receipt/audit/event/authority-read
// intent for service credential operations before delegating the atomic
// mutation. It holds no cached authority or mutable state.
type CredentialAdmin struct {
	authorizer  authorize.Authorizer
	authority   authorize.AuthorityReader
	principals  identity.PrincipalReader
	services    service.ServiceReader
	credentials CredentialReader
	manager     CredentialManager
	receipts    ReceiptLookup
	clock       Clock
	ids         IDSource
}

// NewCredentialAdmin validates the injected boundaries.
func NewCredentialAdmin(config CredentialAdminConfig) (*CredentialAdmin, error) {
	switch {
	case config.Authorizer == nil:
		return nil, fault.New(fault.InternalFailure, "credential administration requires an authorizer")
	case config.Authority == nil:
		return nil, fault.New(fault.InternalFailure, "credential administration requires an authority reader")
	case config.Principals == nil:
		return nil, fault.New(fault.InternalFailure, "credential administration requires a principal reader")
	case config.Services == nil:
		return nil, fault.New(fault.InternalFailure, "credential administration requires a service reader")
	case config.Credentials == nil:
		return nil, fault.New(fault.InternalFailure, "credential administration requires a credential reader")
	case config.Manager == nil:
		return nil, fault.New(fault.InternalFailure, "credential administration requires a credential manager")
	case config.Receipts == nil:
		return nil, fault.New(fault.InternalFailure, "credential administration requires a receipt lookup")
	case config.Clock == nil:
		return nil, fault.New(fault.InternalFailure, "credential administration requires a clock")
	case config.IDs == nil:
		return nil, fault.New(fault.InternalFailure, "credential administration requires an identity source")
	}
	return &CredentialAdmin{
		authorizer:  config.Authorizer,
		authority:   config.Authority,
		principals:  config.Principals,
		services:    config.Services,
		credentials: config.Credentials,
		manager:     config.Manager,
		receipts:    config.Receipts,
		clock:       config.Clock,
		ids:         config.IDs,
	}, nil
}

// The declared registry names used by this capability.
const (
	operationCredentialIssue  authorize.OperationName = "service.credential.issue"
	operationCredentialRotate authorize.OperationName = "service.credential.rotate"
	operationCredentialRevoke authorize.OperationName = "service.credential.revoke"
)

// ServiceCredentialCommand asks to issue or rotate one service credential.
type ServiceCredentialCommand struct {
	Actor           authorize.Principal `json:"actor"`
	Scope           tenancy.Scope       `json:"scope"`
	ServiceID       service.ServiceID   `json:"service_id"`
	TTL             time.Duration       `json:"ttl"`
	ExpectedVersion uint64              `json:"expected_version"`
	CallerKey       string              `json:"caller_key"`
	Content         receipt.Content     `json:"content"`
}

// ServiceCredentialRevokeCommand asks to revoke one service credential.
type ServiceCredentialRevokeCommand struct {
	Actor           authorize.Principal `json:"actor"`
	Scope           tenancy.Scope       `json:"scope"`
	ServiceID       service.ServiceID   `json:"service_id"`
	ExpectedVersion uint64              `json:"expected_version"`
	CallerKey       string              `json:"caller_key"`
	Content         receipt.Content     `json:"content"`
}

// Issue commits a first service credential pair and returns the fresh token.
func (admin *CredentialAdmin) Issue(ctx context.Context, command ServiceCredentialCommand) (IssueResult, error) {
	return admin.mutate(ctx, command, MutationIssue)
}

// Rotate replaces the current service credential pair and returns the fresh
// token. An absent pair is refused rather than silently issued.
func (admin *CredentialAdmin) Rotate(ctx context.Context, command ServiceCredentialCommand) (IssueResult, error) {
	return admin.mutate(ctx, command, MutationRotate)
}

func (admin *CredentialAdmin) mutate(ctx context.Context, command ServiceCredentialCommand, kind MutationKind) (IssueResult, error) {
	name := operationCredentialIssue
	if kind == MutationRotate {
		name = operationCredentialRotate
	}
	op, err := admin.authorizeCommand(ctx, command.Actor, name, command.Scope)
	if err != nil {
		return IssueResult{}, err
	}
	if command.ExpectedVersion == 0 {
		return IssueResult{}, fault.New(fault.InvalidInput, "credential command expected version is zero")
	}
	at := admin.clock.Now()
	if at.IsZero() {
		return IssueResult{}, fault.New(fault.InvalidInput, "credential administration clock returned zero time")
	}
	if err := admin.readEnabledService(ctx, command.Scope, command.ServiceID); err != nil {
		return IssueResult{}, err
	}
	// A lost-response retry must reach the durable receipt before the
	// credential-presence preflight: the original attempt already installed (or
	// cleared) the pair, so refusing on the now-current state would hide the
	// committed result. The Manager performs the receipt-first replay decision;
	// this lookup only tells the preflight to stand aside. A changed-content
	// retry is refused by the Manager with changed_idempotency_content.
	replayed, err := admin.receiptExists(ctx, command.Scope, command.Actor.ID, string(name), command.CallerKey)
	if err != nil {
		return IssueResult{}, err
	}
	if !replayed {
		fact, err := admin.readFact(ctx, command.Scope, command.ServiceID)
		if err != nil {
			return IssueResult{}, err
		}
		switch kind {
		case MutationIssue:
			if fact.Credential.State(at) == StateCurrent {
				return IssueResult{}, fault.New(fault.InvalidInput, "service already has a current credential; rotate it instead")
			}
		case MutationRotate:
			if !fact.Credential.Present() {
				return IssueResult{}, fault.New(fault.Unauthenticated, "no current service credential to rotate")
			}
		}
	}
	reads := credentialAuthorityReads(op, command.Scope, command.Actor.ID)
	if err := admin.confirm(ctx, reads); err != nil {
		return IssueResult{}, err
	}
	result := IssueResult{State: StateCurrent, Version: command.ExpectedVersion + 1}
	record, err := admin.buildReceipt(command.Scope, command.Actor.ID, string(name), command.CallerKey, command.Content, at, result)
	if err != nil {
		return IssueResult{}, err
	}
	audit, err := admin.buildAudit(command.Scope, command.Actor.ID, string(name), at)
	if err != nil {
		return IssueResult{}, err
	}
	envelope, err := admin.buildEvent(command.Scope, string(command.ServiceID), result.Version, command.Actor.ID, at)
	if err != nil {
		return IssueResult{}, err
	}
	request := IssueRequest{
		Scope:           command.Scope,
		PrincipalID:     id.PrincipalID(command.ServiceID),
		ServiceID:       command.ServiceID,
		TTL:             command.TTL,
		ExpectedVersion: command.ExpectedVersion,
		Receipt:         record,
		Audit:           []event.Audit{audit},
		Events:          []event.Envelope{envelope},
		Authority:       reads,
	}
	if kind == MutationRotate {
		return admin.manager.Rotate(ctx, request)
	}
	return admin.manager.Issue(ctx, request)
}

// Revoke clears the current service credential pair and invalidates live
// consumers through the Manager's existing commit-then-invalidate path. It never
// returns a token.
func (admin *CredentialAdmin) Revoke(ctx context.Context, command ServiceCredentialRevokeCommand) (RevokeResult, error) {
	op, err := admin.authorizeCommand(ctx, command.Actor, operationCredentialRevoke, command.Scope)
	if err != nil {
		return RevokeResult{}, err
	}
	if command.ExpectedVersion == 0 {
		return RevokeResult{}, fault.New(fault.InvalidInput, "credential revocation expected version is zero")
	}
	at := admin.clock.Now()
	if at.IsZero() {
		return RevokeResult{}, fault.New(fault.InvalidInput, "credential administration clock returned zero time")
	}
	if err := admin.readEnabledService(ctx, command.Scope, command.ServiceID); err != nil {
		return RevokeResult{}, err
	}
	if _, err := admin.readFact(ctx, command.Scope, command.ServiceID); err != nil {
		return RevokeResult{}, err
	}
	reads := credentialAuthorityReads(op, command.Scope, command.Actor.ID)
	if err := admin.confirm(ctx, reads); err != nil {
		return RevokeResult{}, err
	}
	result := RevokeResult{Mutation: MutationResult{State: StateAbsent, Version: command.ExpectedVersion + 1}}
	record, err := admin.buildReceipt(command.Scope, command.Actor.ID, string(operationCredentialRevoke), command.CallerKey, command.Content, at, result)
	if err != nil {
		return RevokeResult{}, err
	}
	audit, err := admin.buildAudit(command.Scope, command.Actor.ID, string(operationCredentialRevoke), at)
	if err != nil {
		return RevokeResult{}, err
	}
	envelope, err := admin.buildEvent(command.Scope, string(command.ServiceID), result.Mutation.Version, command.Actor.ID, at)
	if err != nil {
		return RevokeResult{}, err
	}
	return admin.manager.Revoke(ctx, RevokeRequest{
		Scope:           command.Scope,
		PrincipalID:     id.PrincipalID(command.ServiceID),
		ServiceID:       command.ServiceID,
		ExpectedVersion: command.ExpectedVersion,
		Reason:          InvalidationRevoked,
		Receipt:         record,
		Audit:           []event.Audit{audit},
		Events:          []event.Envelope{envelope},
		Authority:       reads,
	})
}

func (admin *CredentialAdmin) authorizeCommand(
	ctx context.Context,
	actor authorize.Principal,
	name authorize.OperationName,
	scope tenancy.Scope,
) (authorize.Operation, error) {
	if err := scope.Validate(); err != nil {
		return authorize.Operation{}, err
	}
	if !scope.ProjectScoped() {
		return authorize.Operation{}, fault.New(fault.InvalidInput, "service credential operations require a project scope")
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

func (admin *CredentialAdmin) confirmActor(ctx context.Context, actor authorize.Principal, op authorize.Operation) error {
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

func admit(actor authorize.Principal, name authorize.OperationName) (authorize.Operation, error) {
	if err := actor.Validate(); err != nil {
		return authorize.Operation{}, err
	}
	op, ok := authorize.LookupOperation(name)
	if !ok {
		return authorize.Operation{}, fault.New(fault.InternalFailure, "credential administration operation is not registered")
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

func credentialAuthorityReads(op authorize.Operation, scope tenancy.Scope, principal id.PrincipalID) []authorize.AuthorityRead {
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

func (admin *CredentialAdmin) confirm(ctx context.Context, reads []authorize.AuthorityRead) error {
	if len(reads) == 0 {
		return nil
	}
	facts, err := admin.authority.Read(ctx, reads)
	if err != nil {
		return err
	}
	return authorize.Confirm(reads, facts)
}

func credentialReceiptKey(scope tenancy.Scope, principal id.PrincipalID, operation string, callerKey string) receipt.Key {
	return receipt.Key{
		TenantID:    scope.TenantID,
		ProjectID:   scope.ProjectID,
		PrincipalID: principal,
		Operation:   operation,
		CallerKey:   callerKey,
	}
}

// receiptExists reports whether the operation already committed a receipt for
// this caller key. It does not decode or trust the result; the Manager performs
// the content comparison and replay decision.
func (admin *CredentialAdmin) receiptExists(ctx context.Context, scope tenancy.Scope, principal id.PrincipalID, operation string, callerKey string) (bool, error) {
	if admin.receipts == nil || callerKey == "" {
		return false, nil
	}
	key := credentialReceiptKey(scope, principal, operation, callerKey)
	if err := key.Validate(); err != nil {
		return false, err
	}
	_, found, err := admin.receipts.Lookup(ctx, key)
	if err != nil {
		return false, publicFault("credential receipt is unavailable", err, fault.DependencyUnavailable)
	}
	return found, nil
}

func (admin *CredentialAdmin) buildReceipt(
	scope tenancy.Scope,
	principal id.PrincipalID,
	operation string,
	callerKey string,
	content receipt.Content,
	at time.Time,
	result any,
) (receipt.Record, error) {
	key := credentialReceiptKey(scope, principal, operation, callerKey)
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
		return receipt.Record{}, fault.Wrap(fault.InternalFailure, "credential receipt result could not be encoded", err)
	}
	return receipt.Record{ID: receiptID, Key: key, ContentHash: fingerprint, Result: body, At: at}, nil
}

func (admin *CredentialAdmin) buildAudit(scope tenancy.Scope, actor id.PrincipalID, operation string, at time.Time) (event.Audit, error) {
	eventID, err := admin.ids.NewEventID()
	if err != nil {
		return event.Audit{}, err
	}
	return event.Audit{ID: eventID, Scope: scope, Actor: actor, Operation: operation, At: at}, nil
}

func (admin *CredentialAdmin) buildEvent(scope tenancy.Scope, subject string, subjectVersion uint64, actor id.PrincipalID, at time.Time) (event.Envelope, error) {
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

// readEnabledService reads the target service and refuses a missing, foreign or
// disabled service before any mutation. The fact is current evidence, never
// durable permission.
func (admin *CredentialAdmin) readEnabledService(ctx context.Context, scope tenancy.Scope, serviceID service.ServiceID) error {
	if _, err := service.ParseServiceID(string(serviceID)); err != nil {
		return err
	}
	current, err := admin.services.ReadService(ctx, service.ServiceReadRequest{Scope: scope, ServiceID: serviceID})
	if err != nil {
		return err
	}
	if err := current.Validate(); err != nil {
		return publicFault("current service is invalid", err, fault.InvalidInput)
	}
	if current.ID != serviceID || current.Scope != scope {
		return fault.New(fault.Forbidden, "service is outside the command scope")
	}
	if current.State != service.StateEnabled {
		return fault.New(fault.Forbidden, "service is disabled")
	}
	return nil
}

// readFact reads the target service's current credential fact. The fact never
// carries token plaintext.
func (admin *CredentialAdmin) readFact(ctx context.Context, scope tenancy.Scope, serviceID service.ServiceID) (CredentialFact, error) {
	request := CredentialRequest{
		Scope:       scope,
		PrincipalID: id.PrincipalID(serviceID),
		ServiceID:   serviceID,
	}
	if err := request.Validate(); err != nil {
		return CredentialFact{}, err
	}
	fact, err := admin.credentials.ReadCredential(ctx, request)
	if err != nil {
		return CredentialFact{}, publicFault("credential state is unavailable", err, fault.DependencyUnavailable)
	}
	if fact.Scope != scope || fact.PrincipalID != request.PrincipalID || fact.ServiceID != serviceID || fact.ExecutionID != "" {
		return CredentialFact{}, fault.New(fault.Unauthenticated, "credential fact is for another service")
	}
	if !fact.Enabled {
		return CredentialFact{}, fault.New(fault.Forbidden, "service credential is disabled")
	}
	return fact, nil
}
