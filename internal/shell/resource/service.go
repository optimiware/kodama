package resource

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
)

// Clock is the injected authoritative time source for binding commits. Every
// receipt, audit row and event timestamp comes from it.
type Clock interface {
	Now() time.Time
}

// IDSource allocates the durable audit, receipt and event identities a binding
// commit unit carries.
type IDSource interface {
	NewEventID() (id.EventID, error)
	NewReceiptID() (id.ReceiptID, error)
}

// Config is the complete set of injected boundaries the binding service needs.
// Every field is required: a missing port is a construction failure rather than
// a partially wired service.
type Config struct {
	Clock    Clock
	IDs      IDSource
	Bindings BindingPort
	Deletes  BindingDeletePort
	Reads    BindingReadPort
	Queries  BindingQueryPort
}

// Service builds complete atomic binding register/update/delete commit units
// and performs scoped reads and queries. It never treats a cached role snapshot
// as authority: every mutation carries the registry grant and the
// current-authority reads the adapter must re-evaluate inside the transaction.
type Service struct {
	clock    Clock
	ids      IDSource
	bindings BindingPort
	deletes  BindingDeletePort
	reads    BindingReadPort
	queries  BindingQueryPort
}

// NewService validates the injected boundaries and returns the binding service.
func NewService(config Config) (*Service, error) {
	switch {
	case config.Clock == nil:
		return nil, fault.New(fault.InternalFailure, "resource binding service requires a clock")
	case config.IDs == nil:
		return nil, fault.New(fault.InternalFailure, "resource binding service requires an identity source")
	case config.Bindings == nil:
		return nil, fault.New(fault.InternalFailure, "resource binding service requires an apply port")
	case config.Deletes == nil:
		return nil, fault.New(fault.InternalFailure, "resource binding service requires a delete port")
	case config.Reads == nil:
		return nil, fault.New(fault.InternalFailure, "resource binding service requires a read port")
	case config.Queries == nil:
		return nil, fault.New(fault.InternalFailure, "resource binding service requires a query port")
	}
	return &Service{
		clock:    config.Clock,
		ids:      config.IDs,
		bindings: config.Bindings,
		deletes:  config.Deletes,
		reads:    config.Reads,
		queries:  config.Queries,
	}, nil
}

// The declared registry names used by this capability. They are looked up
// rather than duplicated so the grant, checks and receipt policy always come
// from the single operation matrix.
const (
	operationBindingRegister authorize.OperationName = "resource.binding.register"
	operationBindingUpdate   authorize.OperationName = "resource.binding.update"
	operationBindingDelete   authorize.OperationName = "resource.binding.delete"
	operationBindingRead     authorize.OperationName = "resource.binding.read"
)

// admit validates the presented principal and pins that its kind can satisfy
// the registry grant. It never treats the principal's role snapshot as current
// authority.
func admit(actor authorize.Principal, name authorize.OperationName) (authorize.Operation, error) {
	if err := actor.Validate(); err != nil {
		return authorize.Operation{}, err
	}
	op, ok := authorize.LookupOperation(name)
	if !ok {
		return authorize.Operation{}, fault.New(fault.InternalFailure, "resource binding operation is not registered")
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
// Every read names the operation and its required grant; versioned checks carry
// the target and expected version.
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

// withinScope reports whether target is inside the actor's verified scope. A
// tenant-scoped actor covers every project of its tenant; a project-scoped
// actor covers exactly its own project and never widens to the tenant.
func withinScope(actor, target tenancy.Scope) bool {
	if actor.TenantID == "" || target.TenantID == "" {
		return false
	}
	if actor.TenantID != target.TenantID {
		return false
	}
	if actor.ProjectID == "" {
		return true
	}
	return actor.ProjectID == target.ProjectID
}

// requireProjectScope validates the binding scope and pins it inside the actor
// scope. A binding is always project-scoped, so a tenant-scoped target is a
// malformed request rather than a scope mismatch.
func requireProjectScope(actor, scope tenancy.Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if !scope.ProjectScoped() {
		return fault.New(fault.InvalidInput, "resource bindings are project-scoped")
	}
	if !withinScope(actor, scope) {
		return fault.New(fault.Forbidden, "resource binding scope is outside the actor scope")
	}
	return nil
}

// validateBindingForWrite checks the next binding value: identity and resource
// linkage, scope, enforcing service, declared resource operations, trusted
// origins, reportable event declarations and metadata bounds.
func validateBindingForWrite(binding Binding) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	if err := ValidateTrustedOrigins(binding.TrustedOrigins); err != nil {
		return err
	}
	return validateReportableEvents(binding.ReportableEvents)
}

// buildReceipt builds the scoped idempotency receipt for one binding commit.
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
		return receipt.Record{}, fault.Wrap(fault.InternalFailure, "binding receipt result could not be encoded", err)
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
// the binding change.
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

// publishEffect requests post-commit publication of already-committed events.
func publishEffect() event.PostCommitEffect {
	return event.PostCommitEffect{Kind: event.PostPublishEvent}
}

// RegisterBinding commits one new binding at version 1. It never provisions
// resource bytes or stores access rules.
func (s *Service) RegisterBinding(ctx context.Context, command RegisterBindingCommand) (BindingResult, error) {
	op, err := admit(command.Actor, operationBindingRegister)
	if err != nil {
		return BindingResult{}, err
	}
	if err := requireProjectScope(command.Actor.Scope, command.Scope); err != nil {
		return BindingResult{}, err
	}
	binding := Binding{
		ID:               command.BindingID,
		ResourceID:       command.ResourceID,
		Scope:            command.Scope,
		EnforcingService: command.EnforcingService,
		TrustedOrigins:   append([]string(nil), command.TrustedOrigins...),
		Operations:       append([]Operation(nil), command.Operations...),
		ReportableEvents: append([]string(nil), command.ReportableEvents...),
		Version:          1,
		Metadata:         cloneMetadata(command.Metadata),
	}
	if err := validateBindingForWrite(binding); err != nil {
		return BindingResult{}, err
	}
	scope := binding.Scope
	at := s.clock.Now()
	result := BindingResult{Version: binding.Version}
	record, err := s.buildReceipt(scope, command.Actor.ID, string(operationBindingRegister), command.CallerKey, command.Content, at, result)
	if err != nil {
		return BindingResult{}, err
	}
	audit, err := s.buildAudit(scope, command.Actor.ID, string(operationBindingRegister), at)
	if err != nil {
		return BindingResult{}, err
	}
	envelope, err := s.buildConfigEvent(scope, string(binding.ID), binding.Version, command.Actor.ID, at)
	if err != nil {
		return BindingResult{}, err
	}
	unit := BindingUnit{
		Owner:           scope,
		BindingID:       binding.ID,
		ExpectedVersion: 0,
		Binding:         binding,
		Receipt:         record,
		Audit:           []event.Audit{audit},
		Events:          []event.Envelope{envelope},
		Authority:       authorityReads(op, scope, command.Actor.ID, string(binding.ResourceID), 0),
		PostCommit:      []event.PostCommitEffect{publishEffect()},
	}
	committed, err := s.bindings.Apply(ctx, unit)
	if err != nil {
		return BindingResult{}, err
	}
	return committed, nil
}

// UpdateBinding commits one versioned binding change.
func (s *Service) UpdateBinding(ctx context.Context, command UpdateBindingCommand) (BindingResult, error) {
	op, err := admit(command.Actor, operationBindingUpdate)
	if err != nil {
		return BindingResult{}, err
	}
	if err := requireProjectScope(command.Actor.Scope, command.Scope); err != nil {
		return BindingResult{}, err
	}
	if command.ExpectedVersion == 0 {
		return BindingResult{}, fault.New(fault.InvalidInput, "binding update expected version is zero")
	}
	binding := Binding{
		ID:               command.BindingID,
		ResourceID:       command.ResourceID,
		Scope:            command.Scope,
		EnforcingService: command.EnforcingService,
		TrustedOrigins:   append([]string(nil), command.TrustedOrigins...),
		Operations:       append([]Operation(nil), command.Operations...),
		ReportableEvents: append([]string(nil), command.ReportableEvents...),
		Version:          command.ExpectedVersion + 1,
		Metadata:         cloneMetadata(command.Metadata),
	}
	if err := validateBindingForWrite(binding); err != nil {
		return BindingResult{}, err
	}
	scope := binding.Scope
	at := s.clock.Now()
	result := BindingResult{Version: binding.Version}
	record, err := s.buildReceipt(scope, command.Actor.ID, string(operationBindingUpdate), command.CallerKey, command.Content, at, result)
	if err != nil {
		return BindingResult{}, err
	}
	audit, err := s.buildAudit(scope, command.Actor.ID, string(operationBindingUpdate), at)
	if err != nil {
		return BindingResult{}, err
	}
	envelope, err := s.buildConfigEvent(scope, string(binding.ID), binding.Version, command.Actor.ID, at)
	if err != nil {
		return BindingResult{}, err
	}
	unit := BindingUnit{
		Owner:           scope,
		BindingID:       binding.ID,
		ExpectedVersion: command.ExpectedVersion,
		Binding:         binding,
		Receipt:         record,
		Audit:           []event.Audit{audit},
		Events:          []event.Envelope{envelope},
		Authority:       authorityReads(op, scope, command.Actor.ID, string(binding.ID), command.ExpectedVersion),
		PostCommit:      []event.PostCommitEffect{publishEffect()},
	}
	committed, err := s.bindings.Apply(ctx, unit)
	if err != nil {
		return BindingResult{}, err
	}
	return committed, nil
}

// DeleteBinding commits one versioned binding removal.
func (s *Service) DeleteBinding(ctx context.Context, command DeleteBindingCommand) (BindingDeleteResult, error) {
	op, err := admit(command.Actor, operationBindingDelete)
	if err != nil {
		return BindingDeleteResult{}, err
	}
	if err := requireProjectScope(command.Actor.Scope, command.Scope); err != nil {
		return BindingDeleteResult{}, err
	}
	if _, err := ParseBindingID(string(command.BindingID)); err != nil {
		return BindingDeleteResult{}, err
	}
	if command.ExpectedVersion == 0 {
		return BindingDeleteResult{}, fault.New(fault.InvalidInput, "binding delete expected version is zero")
	}
	scope := command.Scope
	version := command.ExpectedVersion + 1
	at := s.clock.Now()
	result := BindingDeleteResult{Version: version}
	record, err := s.buildReceipt(scope, command.Actor.ID, string(operationBindingDelete), command.CallerKey, command.Content, at, result)
	if err != nil {
		return BindingDeleteResult{}, err
	}
	audit, err := s.buildAudit(scope, command.Actor.ID, string(operationBindingDelete), at)
	if err != nil {
		return BindingDeleteResult{}, err
	}
	envelope, err := s.buildConfigEvent(scope, string(command.BindingID), version, command.Actor.ID, at)
	if err != nil {
		return BindingDeleteResult{}, err
	}
	unit := BindingDeleteUnit{
		Owner:           scope,
		BindingID:       command.BindingID,
		ExpectedVersion: command.ExpectedVersion,
		At:              at,
		Receipt:         record,
		Audit:           []event.Audit{audit},
		Events:          []event.Envelope{envelope},
		Authority:       authorityReads(op, scope, command.Actor.ID, string(command.BindingID), command.ExpectedVersion),
		PostCommit:      []event.PostCommitEffect{publishEffect()},
	}
	committed, err := s.deletes.DeleteBinding(ctx, unit)
	if err != nil {
		return BindingDeleteResult{}, err
	}
	return committed, nil
}

// ReadBinding reads one binding inside the caller's current project scope and
// refuses an adapter result that escapes that scope.
func (s *Service) ReadBinding(ctx context.Context, command ReadBindingCommand) (Binding, error) {
	if _, err := admit(command.Actor, operationBindingRead); err != nil {
		return Binding{}, err
	}
	if err := requireProjectScope(command.Actor.Scope, command.Scope); err != nil {
		return Binding{}, err
	}
	request := BindingReadRequest{Scope: command.Scope, BindingID: command.BindingID}
	if err := request.Validate(); err != nil {
		return Binding{}, err
	}
	binding, err := s.reads.ReadBinding(ctx, request)
	if err != nil {
		return Binding{}, err
	}
	if binding.Scope != request.Scope {
		return Binding{}, fault.New(fault.Forbidden, "binding read returned a binding outside the caller scope")
	}
	if binding.ID != request.BindingID {
		return Binding{}, fault.New(fault.InternalFailure, "binding read returned a different binding identity")
	}
	if err := binding.Validate(); err != nil {
		return Binding{}, fault.Wrap(fault.InternalFailure, "binding read returned an invalid binding", err)
	}
	return cloneBinding(binding), nil
}

// QueryBindings returns one authoritative, stably paginated binding page and
// refuses any adapter row that escapes the caller's current project scope.
func (s *Service) QueryBindings(ctx context.Context, command QueryBindingsCommand) (BindingPage, error) {
	if _, err := admit(command.Actor, operationBindingRead); err != nil {
		return BindingPage{}, err
	}
	if err := requireProjectScope(command.Actor.Scope, command.Scope); err != nil {
		return BindingPage{}, err
	}
	query := BindingQuery{
		Scope:            command.Scope,
		EnforcingService: command.EnforcingService,
		ResourceID:       command.ResourceID,
		Cursor:           command.Cursor,
		Limit:            command.Limit,
	}
	if err := query.Validate(); err != nil {
		return BindingPage{}, err
	}
	page, err := s.queries.QueryBindings(ctx, query)
	if err != nil {
		return BindingPage{}, err
	}
	for _, binding := range page.Bindings {
		if binding.Scope != query.Scope {
			return BindingPage{}, fault.New(fault.Forbidden, "binding query returned a binding outside the caller scope")
		}
	}
	return cloneBindingPage(page), nil
}
