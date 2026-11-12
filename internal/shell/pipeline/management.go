package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Clock supplies the authoritative time for one management commit.
type Clock interface {
	Now() time.Time
}

// IDSource supplies stable event/audit and receipt identities.
type IDSource interface {
	NewEventID() (id.EventID, error)
	NewReceiptID() (id.ReceiptID, error)
}

// ApplyCommand creates a pipeline or replaces an existing same-name pipeline.
type ApplyCommand struct {
	Actor     authorize.Principal
	Scope     tenancy.Scope
	Name      string
	YAML      []byte
	CallerKey string
	Content   receipt.Content
}

// UpdateCommand replaces one selected pipeline at an expected version.
type UpdateCommand struct {
	Actor           authorize.Principal
	Scope           tenancy.Scope
	PipelineID      PipelineID
	Name            string
	YAML            []byte
	ExpectedVersion uint64
	CallerKey       string
	Content         receipt.Content
}

// DeleteCommand removes one selected pipeline at an expected version.
type DeleteCommand struct {
	Actor           authorize.Principal
	Scope           tenancy.Scope
	PipelineID      PipelineID
	ExpectedVersion uint64
	CallerKey       string
	Content         receipt.Content
}

// ReadCommand reads one pipeline definition.
type ReadCommand struct {
	Actor      authorize.Principal
	Scope      tenancy.Scope
	PipelineID PipelineID
}

// ListCommand lists the pipeline definitions of one project.
type ListCommand struct {
	Actor authorize.Principal
	Scope tenancy.Scope
}

// ManagementConfig supplies every pipeline-management boundary. A nil
// dependency is refused rather than defaulted.
type ManagementConfig struct {
	Clock      Clock
	IDs        IDSource
	Authorizer authorize.Authorizer
	Authority  authorize.AuthorityReader
	Changes    ChangePort
	Reader     Reader
}

// Management orchestrates authorized pipeline authoring. Each mutation builds
// one complete ChangeUnit whose authority reads are re-read and confirmed
// before the unit commits.
type Management struct {
	clock      Clock
	ids        IDSource
	authorizer authorize.Authorizer
	authority  authorize.AuthorityReader
	changes    ChangePort
	reader     Reader
}

// NewManagement validates the management boundary dependencies.
func NewManagement(config ManagementConfig) (*Management, error) {
	if config.Clock == nil || config.IDs == nil || config.Authorizer == nil || config.Authority == nil || config.Changes == nil || config.Reader == nil {
		return nil, fault.New(fault.InternalFailure, "management requires clock, ids, authorizer, authority, changes and reader")
	}
	return &Management{
		clock:      config.Clock,
		ids:        config.IDs,
		authorizer: config.Authorizer,
		authority:  config.Authority,
		changes:    config.Changes,
		reader:     config.Reader,
	}, nil
}

// Apply authorizes and commits one pipeline creation or same-name replacement.
func (management *Management) Apply(ctx context.Context, command ApplyCommand) (ChangeResult, error) {
	const operation authorize.OperationName = "pipeline.apply"
	if err := management.authorize(ctx, command.Actor, operation, command.Scope); err != nil {
		return ChangeResult{}, err
	}
	if err := validatePipelineName(command.Name); err != nil {
		return ChangeResult{}, err
	}
	parsed, err := decodeDocument(command.YAML, DefaultLimits)
	if err != nil {
		return ChangeResult{}, err
	}
	request := changeRequest{
		kind:        ChangeApply,
		operation:   operation,
		scope:       command.Scope,
		actor:       command.Actor,
		name:        command.Name,
		yaml:        command.YAML,
		callerKey:   command.CallerKey,
		content:     command.Content,
		subjectID:   command.Name,
		subjectBase: 0,
	}
	current, err := management.reader.ByName(ctx, command.Scope, command.Name)
	if err != nil {
		if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
			return ChangeResult{}, err
		}
	} else {
		projected, err := withPushProjection(current)
		if err != nil {
			return ChangeResult{}, err
		}
		current = projected
		request.pipelineID = current.ID
		request.expected = current.Version
		request.subjectID = string(current.ID)
		request.subjectBase = current.Version
		request.includePush = !pushEqual(pushOf(current), parsed.push)
	}
	if request.pipelineID == "" {
		request.includePush = parsed.push != nil
	}
	request.push = parsed.push
	unit, err := management.buildChangeUnit(request)
	if err != nil {
		return ChangeResult{}, err
	}
	return management.commit(ctx, unit)
}

// Update authorizes and commits one versioned pipeline replacement.
func (management *Management) Update(ctx context.Context, command UpdateCommand) (ChangeResult, error) {
	const operation authorize.OperationName = "pipeline.update"
	if err := management.authorize(ctx, command.Actor, operation, command.Scope); err != nil {
		return ChangeResult{}, err
	}
	if err := validatePipelineName(command.Name); err != nil {
		return ChangeResult{}, err
	}
	parsed, err := decodeDocument(command.YAML, DefaultLimits)
	if err != nil {
		return ChangeResult{}, err
	}
	current, err := management.reader.Get(ctx, command.Scope, command.PipelineID)
	if err != nil {
		return ChangeResult{}, err
	}
	projected, err := withPushProjection(current)
	if err != nil {
		return ChangeResult{}, err
	}
	current = projected
	unit, err := management.buildChangeUnit(changeRequest{
		kind:        ChangeUpdate,
		operation:   operation,
		scope:       command.Scope,
		actor:       command.Actor,
		pipelineID:  command.PipelineID,
		name:        command.Name,
		yaml:        command.YAML,
		expected:    command.ExpectedVersion,
		callerKey:   command.CallerKey,
		content:     command.Content,
		subjectID:   string(command.PipelineID),
		subjectBase: command.ExpectedVersion,
		includePush: !pushEqual(pushOf(current), parsed.push),
		push:        parsed.push,
	})
	if err != nil {
		return ChangeResult{}, err
	}
	return management.commit(ctx, unit)
}

// Delete authorizes and commits one versioned pipeline deletion.
func (management *Management) Delete(ctx context.Context, command DeleteCommand) (ChangeResult, error) {
	const operation authorize.OperationName = "pipeline.delete"
	if err := management.authorize(ctx, command.Actor, operation, command.Scope); err != nil {
		return ChangeResult{}, err
	}
	current, err := management.reader.Get(ctx, command.Scope, command.PipelineID)
	if err != nil {
		if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
			return ChangeResult{}, err
		}
		// The row may already be gone because this exact delete committed and
		// its response was lost. The receipt-first change port still has to
		// observe the unit so the retry converges on the original result
		// instead of failing not_found before the replay check runs.
		current = Definition{ID: command.PipelineID}
	} else {
		projected, err := withPushProjection(current)
		if err != nil {
			return ChangeResult{}, err
		}
		current = projected
	}
	unit, err := management.buildChangeUnit(changeRequest{
		kind:        ChangeDelete,
		operation:   operation,
		scope:       command.Scope,
		actor:       command.Actor,
		pipelineID:  command.PipelineID,
		name:        current.Name,
		expected:    command.ExpectedVersion,
		callerKey:   command.CallerKey,
		content:     command.Content,
		subjectID:   string(command.PipelineID),
		subjectBase: command.ExpectedVersion,
		includePush: pushOf(current) != nil,
	})
	if err != nil {
		return ChangeResult{}, err
	}
	return management.commit(ctx, unit)
}

// Read authorizes, rechecks current membership and returns one definition.
func (management *Management) Read(ctx context.Context, command ReadCommand) (Definition, error) {
	const operation authorize.OperationName = "pipeline.read"
	if err := management.authorize(ctx, command.Actor, operation, command.Scope); err != nil {
		return Definition{}, err
	}
	requires, err := registryRequires(operation)
	if err != nil {
		return Definition{}, err
	}
	reads := []authorize.AuthorityRead{
		newAuthorityRead(operation, requires, authorize.CheckMembership, command.Scope, command.Actor.ID, "", 0),
	}
	if err := management.confirm(ctx, reads); err != nil {
		return Definition{}, err
	}
	current, err := management.reader.Get(ctx, command.Scope, command.PipelineID)
	if err != nil {
		return Definition{}, err
	}
	return withPushProjection(current)
}

// List authorizes, rechecks current membership and returns the scoped
// definitions.
func (management *Management) List(ctx context.Context, command ListCommand) ([]Definition, error) {
	const operation authorize.OperationName = "pipeline.read"
	if err := management.authorize(ctx, command.Actor, operation, command.Scope); err != nil {
		return nil, err
	}
	requires, err := registryRequires(operation)
	if err != nil {
		return nil, err
	}
	reads := []authorize.AuthorityRead{
		newAuthorityRead(operation, requires, authorize.CheckMembership, command.Scope, command.Actor.ID, "", 0),
	}
	if err := management.confirm(ctx, reads); err != nil {
		return nil, err
	}
	definitions, err := management.reader.List(ctx, command.Scope)
	if err != nil {
		return nil, err
	}
	for index := range definitions {
		projected, err := withPushProjection(definitions[index])
		if err != nil {
			return nil, err
		}
		definitions[index] = projected
	}
	return definitions, nil
}

type changeRequest struct {
	kind        ChangeKind
	operation   authorize.OperationName
	scope       tenancy.Scope
	actor       authorize.Principal
	pipelineID  PipelineID
	name        string
	yaml        []byte
	expected    uint64
	callerKey   string
	content     receipt.Content
	subjectID   string
	subjectBase uint64
	includePush bool
	push        *PushTrigger
}

func (management *Management) buildChangeUnit(request changeRequest) (ChangeUnit, error) {
	requires, err := registryRequires(request.operation)
	if err != nil {
		return ChangeUnit{}, err
	}
	contentHash, err := receipt.Fingerprint(request.content)
	if err != nil {
		return ChangeUnit{}, fault.Wrap(fault.InvalidInput, "receipt content is invalid", err)
	}
	receiptID, err := management.ids.NewReceiptID()
	if err != nil {
		return ChangeUnit{}, err
	}
	auditID, err := management.ids.NewEventID()
	if err != nil {
		return ChangeUnit{}, err
	}
	eventID, err := management.ids.NewEventID()
	if err != nil {
		return ChangeUnit{}, err
	}
	now := management.clock.Now()
	reads := []authorize.AuthorityRead{
		newAuthorityRead(request.operation, requires, authorize.CheckMembership, request.scope, request.actor.ID, "", 0),
		newAuthorityRead(request.operation, requires, authorize.CheckPipelineVersion, request.scope, request.actor.ID, string(request.pipelineID), request.expected),
	}
	if request.includePush {
		reads = append(reads, newAuthorityRead(request.operation, requires, authorize.CheckPushServiceAdmin, request.scope, request.actor.ID, "", 0))
	}
	unit := ChangeUnit{
		Owner:           request.scope,
		Kind:            request.kind,
		PipelineID:      request.pipelineID,
		Name:            request.name,
		YAML:            append([]byte(nil), request.yaml...),
		ExpectedVersion: request.expected,
		PushTriggers:    pushProjection(request.push),
		Receipt: receipt.Record{
			ID: receiptID,
			Key: receipt.Key{
				TenantID:    request.scope.TenantID,
				ProjectID:   request.scope.ProjectID,
				PrincipalID: request.actor.ID,
				Operation:   string(request.operation),
				CallerKey:   request.callerKey,
			},
			ContentHash: contentHash,
			At:          now,
		},
		Audit: []event.Audit{{
			ID:        auditID,
			Scope:     request.scope,
			Actor:     request.actor.ID,
			Operation: string(request.operation),
			At:        now,
		}},
		Events: []event.Envelope{{
			ID:             eventID,
			Type:           event.TypeConfigChanged,
			Source:         event.Source{Kind: event.SourceShell, PrincipalID: request.actor.ID},
			At:             now,
			Scope:          request.scope,
			SubjectID:      request.subjectID,
			SubjectVersion: request.subjectBase + 1,
		}},
		Authority:  reads,
		PostCommit: []event.PostCommitEffect{{Kind: event.PostPublishEvent}},
	}
	if err := unit.Receipt.Key.Validate(); err != nil {
		return ChangeUnit{}, err
	}
	return unit, nil
}

func (management *Management) commit(ctx context.Context, unit ChangeUnit) (ChangeResult, error) {
	// A pipeline-version read is enforced atomically by the change port after
	// its receipt-first replay check. Confirming it here would reject a
	// lost-response retry whose committed write already advanced the version,
	// so the replay would never be reached. Current membership (and the push
	// grant) are still rechecked before the port is invoked.
	if err := management.confirm(ctx, preCommitReads(unit.Authority)); err != nil {
		return ChangeResult{}, err
	}
	return management.changes.Change(ctx, unit)
}

// preCommitReads drops versioned reads that the change port must observe inside
// its atomic unit. Everything else is confirmed against current authority
// before the commit is attempted.
func preCommitReads(reads []authorize.AuthorityRead) []authorize.AuthorityRead {
	filtered := make([]authorize.AuthorityRead, 0, len(reads))
	for _, read := range reads {
		if read.Check == authorize.CheckPipelineVersion {
			continue
		}
		filtered = append(filtered, read)
	}
	return filtered
}

func (management *Management) confirm(ctx context.Context, reads []authorize.AuthorityRead) error {
	facts, err := management.authority.Read(ctx, reads)
	if err != nil {
		return err
	}
	return authorize.Confirm(reads, facts)
}

func (management *Management) authorize(ctx context.Context, actor authorize.Principal, operation authorize.OperationName, scope tenancy.Scope) error {
	if actor.Scope != scope {
		return fault.New(fault.Forbidden, "actor scope does not match command scope")
	}
	decision, err := management.authorizer.Authorize(ctx, actor, operation, scope)
	if err != nil {
		return err
	}
	if decision != authorize.DecisionAllow {
		return fault.New(fault.Forbidden, fmt.Sprintf("principal is not authorized for %q", operation))
	}
	return nil
}

func registryRequires(operation authorize.OperationName) (authorize.Requires, error) {
	registered, ok := authorize.LookupOperation(operation)
	if !ok {
		return authorize.Requires{}, fault.New(fault.InternalFailure, fmt.Sprintf("operation %q is not registered", operation))
	}
	return registered.Requires, nil
}

func newAuthorityRead(operation authorize.OperationName, requires authorize.Requires, check authorize.AuthorityCheck, scope tenancy.Scope, principal id.PrincipalID, target string, expected uint64) authorize.AuthorityRead {
	return authorize.AuthorityRead{
		Operation:       operation,
		Required:        requires,
		Check:           check,
		Scope:           scope,
		PrincipalID:     principal,
		TargetID:        target,
		ExpectedVersion: expected,
	}
}

func validatePipelineName(name string) error {
	if name == "" || len(name) > id.MaxIDLength || name == "." || name == ".." {
		return fault.New(fault.InvalidInput, fmt.Sprintf("pipeline name %q is invalid", name))
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_', character == '.':
		default:
			return fault.New(fault.InvalidInput, fmt.Sprintf("pipeline name %q is invalid", name))
		}
	}
	return nil
}

func pushEqual(left, right *PushTrigger) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	if left.Repository != right.Repository || left.Service != right.Service {
		return false
	}
	if len(left.Refs) != len(right.Refs) {
		return false
	}
	for index := range left.Refs {
		if left.Refs[index] != right.Refs[index] {
			return false
		}
	}
	return true
}

// pushOf returns the single effective push trigger a definition carries: the
// semantic Push form when a reader populated it, otherwise the first entry of
// the persisted PushTriggers projection.
func pushOf(definition Definition) *PushTrigger {
	if definition.Push != nil {
		return definition.Push
	}
	if len(definition.PushTriggers) == 0 {
		return nil
	}
	trigger := definition.PushTriggers[0]
	return &trigger
}

// pushProjection maps one validated on.push trigger to the persisted
// projection. It copies the exact refs in declaration order so neither the
// change unit nor a read definition aliases the decoded document.
func pushProjection(push *PushTrigger) []PushTrigger {
	if push == nil {
		return nil
	}
	return []PushTrigger{{
		Repository: push.Repository,
		Service:    push.Service,
		Refs:       append([]string(nil), push.Refs...),
	}}
}

func clonePushTriggers(triggers []PushTrigger) []PushTrigger {
	if triggers == nil {
		return nil
	}
	cloned := make([]PushTrigger, len(triggers))
	for index, trigger := range triggers {
		cloned[index] = PushTrigger{
			Repository: trigger.Repository,
			Service:    trigger.Service,
			Refs:       append([]string(nil), trigger.Refs...),
		}
	}
	return cloned
}

// withPushProjection fills the validated on.push projection on a definition a
// reader returned. A reader may expose the semantic Push form or the persisted
// PushTriggers column; when it exposes neither, the exact admitted YAML bytes
// are decoded with the same validation admission applies so every caller sees
// one deterministic projection. The projection is always a copy; the reader's
// Push form, when present, is left untouched.
func withPushProjection(definition Definition) (Definition, error) {
	if len(definition.PushTriggers) > 0 {
		definition.PushTriggers = clonePushTriggers(definition.PushTriggers)
		return definition, nil
	}
	if definition.Push != nil {
		definition.PushTriggers = pushProjection(definition.Push)
		return definition, nil
	}
	if len(definition.YAML) == 0 {
		return definition, nil
	}
	parsed, err := decodeDocument(definition.YAML, DefaultLimits)
	if err != nil {
		return Definition{}, err
	}
	definition.PushTriggers = pushProjection(parsed.push)
	return definition, nil
}
