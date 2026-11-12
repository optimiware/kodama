package verify

import (
	"context"
	"slices"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Clock supplies the current time. Verification records carry the injected
// time of the attempt, never a clock read hidden inside an adapter.
type Clock interface {
	Now() time.Time
}

// Config is the complete verifier composition. Every dependency except the
// optional membership reader is required; New refuses a partial composition
// instead of guessing a substitute.
//
// Membership is the consumer-owned membership reader. When it is present, a
// verified human's returned roles are the current union in the verification
// scope (S05 §4 "current roles in resource scope"), and the record unit pins
// both applicable membership rows so the commit boundary re-reads and confirms
// them. A composition that does not supply it keeps the resolver-scope roles;
// the production composition always supplies it.
type Config struct {
	Principals authorize.CurrentPrincipal
	Bindings   BindingReader
	Evidence   EvidenceReader
	Membership authorize.MembershipReader
	Records    RecordPort
	Clock      Clock
	NewID      func() (VerificationID, error)
}

// Attempt is one verification request. The calling service is already
// authenticated by the caller's transport boundary; Service is that verified
// service principal. Credential is the independently supplied actor
// credential. Approval is the optional human role approval to match. ActionID
// is the current running action identity: it is the action-deadline
// authority-read target and the recorded verification action, never an
// approval-matching key.
type Attempt struct {
	Scope      tenancy.Scope
	Service    identity.Principal
	Credential string
	ResourceID resource.ResourceID
	Operation  resource.Operation
	Subject    Subject
	Approval   request.Assignment
	ActionID   id.ActionID
}

// Verifier orchestrates one actor verification attempt. It holds only its
// immutable dependencies, so concurrent attempts do not share per-attempt
// state and every retry authenticates, reads the binding and matches evidence
// afresh.
type Verifier struct {
	principals authorize.CurrentPrincipal
	bindings   BindingReader
	evidence   EvidenceReader
	membership authorize.MembershipReader
	records    RecordPort
	clock      Clock
	newID      func() (VerificationID, error)
}

// New validates the composition and returns a verifier.
func New(config Config) (*Verifier, error) {
	switch {
	case config.Principals == nil:
		return nil, fault.New(fault.InvalidInput, "verifier requires a current-principal resolver")
	case config.Bindings == nil:
		return nil, fault.New(fault.InvalidInput, "verifier requires a binding reader")
	case config.Evidence == nil:
		return nil, fault.New(fault.InvalidInput, "verifier requires an evidence reader")
	case config.Records == nil:
		return nil, fault.New(fault.InvalidInput, "verifier requires a verification record port")
	case config.Clock == nil:
		return nil, fault.New(fault.InvalidInput, "verifier requires a clock")
	case config.NewID == nil:
		return nil, fault.New(fault.InvalidInput, "verifier requires a verification identity generator")
	}
	return &Verifier{
		principals: config.Principals,
		bindings:   config.Bindings,
		evidence:   config.Evidence,
		membership: config.Membership,
		records:    config.Records,
		clock:      config.Clock,
		newID:      config.NewID,
	}, nil
}

// Verify authenticates the calling service and the actor independently, checks
// the current binding, matches optional same-execution human-role approval
// evidence, and commits one durable verification fact.
//
// The returned Record is a fact, never a bearer grant. A refused attempt that
// reached authorization is itself recorded as a refused fact and returns a
// forbidden fault; structural, authentication, binding and dependency
// failures create no fact.
func (verifier *Verifier) Verify(ctx context.Context, input Attempt) (Record, error) {
	if err := validateAttempt(input); err != nil {
		return Record{}, err
	}

	actor, err := verifier.principals.Resolve(ctx, input.Credential)
	if err != nil {
		return Record{}, err
	}
	if err := actor.Validate(); err != nil {
		return Record{}, fault.Wrap(fault.Unauthenticated, "resolved actor principal is invalid", err)
	}

	binding, err := verifier.bindings.ReadBinding(ctx, BindingQuery{
		Scope:      input.Scope,
		ResourceID: input.ResourceID,
		ServiceID:  service.ServiceID(input.Service.ID),
	})
	if err != nil {
		return Record{}, err
	}
	if err := authorizeCallingService(input, binding); err != nil {
		return Record{}, err
	}

	var refusal error
	var membershipTargets []authorize.AuthorityTarget
	approved := false
	if err := authorizeActor(input, actor); err != nil {
		refusal = err
	} else if verifier.membership != nil && actor.Kind == identity.KindHuman {
		roles, targets, err := verifier.scopedRoles(ctx, input.Scope, actor.ID)
		if err != nil {
			return Record{}, err
		}
		actor.Roles = roles
		membershipTargets = targets
	} else if input.Approval != (request.Assignment{}) {
		// ActionID is the current action, passed only as context for the
		// adapter's authority read. It is not an approval-matching key:
		// S05 §4 approval evidence is an earlier succeeded request in the
		// same execution, so it normally carries a different action id.
		evidence, err := verifier.evidence.ReadEvidence(ctx, EvidenceQuery{
			Scope:       input.Scope,
			ExecutionID: actor.ExecutionID,
			ActionID:    input.ActionID,
			Assignment:  input.Approval,
			Operation:   string(input.Operation),
			ResourceID:  input.ResourceID,
			Subject:     input.Subject,
		})
		if err != nil {
			return Record{}, err
		}
		approved = matchesApproval(input, actor, evidence)
		if !approved {
			refusal = fault.New(fault.Forbidden, "no matching approval evidence")
		}
	}

	verificationID, err := verifier.newID()
	if err != nil {
		return Record{}, err
	}
	if _, err := ParseVerificationID(string(verificationID)); err != nil {
		return Record{}, fault.Wrap(fault.InternalFailure, "verification identity generator returned an invalid id", err)
	}

	result := ResultRefused
	if refusal == nil {
		result = ResultApproved
	}
	record := Record{
		ID:                verificationID,
		Scope:             input.Scope,
		CallingService:    service.ServiceID(input.Service.ID),
		BindingID:         binding.ID,
		Actor:             actor,
		ExecutionID:       actor.ExecutionID,
		ActionID:          input.ActionID,
		ResourceID:        input.ResourceID,
		Operation:         string(input.Operation),
		Subject:           input.Subject,
		RequestedApproval: input.Approval,
		Result:            result,
		Approved:          approved,
		At:                verifier.clock.Now(),
	}
	unit := RecordUnit{
		Owner:     input.Scope,
		Record:    record,
		Authority: authorityReads(input, actor, binding, membershipTargets),
	}
	committed, err := verifier.records.Record(ctx, unit)
	if err != nil {
		return Record{}, err
	}
	if _, err := ParseVerificationID(string(committed.ID)); err != nil {
		return Record{}, fault.New(fault.InternalFailure, "verification record port returned no valid committed id")
	}
	record.ID = committed.ID
	if refusal != nil {
		return record, refusal
	}
	return record, nil
}

// validateAttempt checks the structural request shape before any dependency is
// touched. The verification scope is always project-scoped; the calling
// service must be a structurally valid service principal; and a requested
// approval must be an explicit human role assignment.
func validateAttempt(input Attempt) error {
	if err := input.Scope.Validate(); err != nil {
		return err
	}
	if !input.Scope.ProjectScoped() {
		return fault.New(fault.InvalidInput, "verification scope must be project-scoped")
	}
	if input.Credential == "" {
		return fault.New(fault.InvalidInput, "actor credential is empty")
	}
	if _, err := resource.ParseResourceID(string(input.ResourceID)); err != nil {
		return err
	}
	if _, err := resource.ParseOperation(string(input.Operation)); err != nil {
		return err
	}
	if input.Operation == resource.OperationGitRead {
		// A repository read is repository-level verification only: it carries
		// no effect subject and requests no role approval.
		if len(input.Subject.Fields) != 0 {
			return fault.New(fault.InvalidInput, "git.read verification requires an empty subject")
		}
		if input.Approval != (request.Assignment{}) {
			return fault.New(fault.InvalidInput, "git.read verification does not accept an approval")
		}
	}
	if err := input.Service.Validate(); err != nil {
		return err
	}
	if input.Service.Kind != identity.KindService {
		return fault.New(fault.InvalidInput, "calling principal is not a service account")
	}
	if input.Approval != (request.Assignment{}) {
		if !input.Approval.Valid() {
			return fault.New(fault.InvalidInput, "requested approval assignment is invalid")
		}
		if input.Approval.Kind != request.AssignmentRole {
			return fault.New(fault.InvalidInput, "requested approval must be a human role assignment")
		}
	}
	return nil
}

// authorizeCallingService checks that the calling service is currently enabled,
// inside the verification scope, and covered by the current binding for the
// exact resource and attempted operation. Service permission never substitutes
// for actor authority.
func authorizeCallingService(input Attempt, binding resource.Binding) error {
	if !input.Service.Enabled {
		return fault.New(fault.Forbidden, "calling service is disabled")
	}
	if !scopeContains(input.Service.Scope, input.Scope) {
		return fault.New(fault.Forbidden, "calling service is outside the verification scope")
	}
	if binding.EnforcingService != service.ServiceID(input.Service.ID) {
		return fault.New(fault.Forbidden, "binding does not authorize the calling service")
	}
	if binding.Scope != input.Scope {
		return fault.New(fault.Forbidden, "binding is outside the verification scope")
	}
	if binding.ResourceID != input.ResourceID {
		return fault.New(fault.Forbidden, "binding does not cover the requested resource")
	}
	if !declaresOperation(binding, input.Operation) {
		return fault.New(fault.Forbidden, "binding does not declare the attempted operation")
	}
	return nil
}

// authorizeActor checks the current actor state and, when approval is
// requested, that the actor is an eligible execution. Humans and services may
// authenticate but can never carry execution approval evidence.
func authorizeActor(input Attempt, actor identity.Principal) error {
	if !actor.Enabled {
		return fault.New(fault.Forbidden, "actor is currently disabled")
	}
	if !scopeContains(actor.Scope, input.Scope) {
		return fault.New(fault.Forbidden, "actor is outside the verification scope")
	}
	if input.Approval != (request.Assignment{}) && actor.Kind != identity.KindExecution {
		return fault.New(fault.Forbidden, "approval evidence requires an execution actor")
	}
	return nil
}

// scopedRoles resolves one human actor's current roles in the verification
// scope. The effective roles are the union of the tenant-wide row and the
// exact-project row, so a project grant adds to a tenant-wide grant and never
// shadows it. The returned targets are the observed pins of both rows; the
// record unit carries them so the commit boundary re-reads and confirms the
// exact rows before persisting the fact.
func (verifier *Verifier) scopedRoles(ctx context.Context, scope tenancy.Scope, principal id.PrincipalID) ([]tenancy.Role, []authorize.AuthorityTarget, error) {
	rows, err := verifier.membership.ReadMembership(ctx, scope, principal)
	if err != nil {
		return nil, nil, err
	}
	var tenantWide, project *tenancy.Membership
	if rows.TenantWide.Membership != nil {
		tenantWide = rows.TenantWide.Membership
	}
	if rows.Project.Membership != nil {
		project = rows.Project.Membership
	}
	roles, applicable := tenancy.EffectiveRoles(scope, tenantWide, project)
	if !applicable || len(roles) == 0 {
		return nil, nil, fault.New(fault.Forbidden, "actor has no current membership roles in the verification scope")
	}
	targets := authorize.MembershipTargets(scope, tenantWide, project, rows.TenantWide.RowID, rows.Project.RowID)
	return roles, targets, nil
}

// matchesApproval reports whether any durable prior request fact is the exact
// same-execution human-role approval for this operation, resource and effect
// subject. S05 §4 approval evidence is an earlier succeeded request in the
// actor's own execution, so it carries a different action id than the current
// running action and the action identity is not part of the match. Display
// context and a replayed receipt never substitute for a matching approval.
func matchesApproval(input Attempt, actor identity.Principal, evidence []Evidence) bool {
	for _, candidate := range evidence {
		if candidate.Decision != request.DecisionPass {
			continue
		}
		if candidate.ExecutionID != actor.ExecutionID {
			continue
		}
		if candidate.Assignment != input.Approval {
			continue
		}
		if candidate.Operation != string(input.Operation) {
			continue
		}
		if candidate.ResourceID != input.ResourceID {
			continue
		}
		if !SubjectMatches(input.Subject, candidate.Subject) {
			continue
		}
		return true
	}
	return false
}

// declaresOperation reports whether the binding declares the attempted resource
// operation.
func declaresOperation(binding resource.Binding, operation resource.Operation) bool {
	return slices.Contains(binding.Operations, operation)
}

// scopeContains reports whether the owner scope covers the target scope. A
// tenant-scoped owner covers every project in its tenant; a project-scoped
// owner covers only that same project.
func scopeContains(owner, target tenancy.Scope) bool {
	if owner.TenantID != target.TenantID {
		return false
	}
	if owner.ProjectID == "" {
		return true
	}
	return owner.ProjectID == target.ProjectID
}

// authorityReads builds the actor.verify current-authority reads in registry
// order. The adapter re-evaluates them inside the record commit, so a grant or
// binding observed here is never durable permission. When a human actor's
// scoped membership rows were observed, the principal-enablement read carries
// their pins and the observed role set, so the adapter confirms both rows
// inside the same commit.
func authorityReads(input Attempt, actor identity.Principal, binding resource.Binding, membershipTargets []authorize.AuthorityTarget) []authorize.AuthorityRead {
	operation, ok := authorize.LookupOperation("actor.verify")
	if !ok {
		return nil
	}
	reads := make([]authorize.AuthorityRead, 0, len(operation.Authority))
	for _, check := range operation.Authority {
		read := authorize.AuthorityRead{
			Operation:   operation.Name,
			Required:    operation.Requires,
			Check:       check,
			Scope:       input.Scope,
			PrincipalID: actor.ID,
		}
		switch check {
		case authorize.CheckServiceEnabled:
			read.PrincipalID = id.PrincipalID(input.Service.ID)
		case authorize.CheckBindingVersion:
			read.TargetID = string(binding.ID)
			read.ExpectedVersion = binding.Version
		case authorize.CheckExecutionRunning:
			read.TargetID = string(actor.ExecutionID)
		case authorize.CheckActionDeadline:
			read.TargetID = string(input.ActionID)
		case authorize.CheckPrincipalEnabled:
			if len(membershipTargets) > 0 {
				read.Required = authorize.Requires{HumanRoles: append([]tenancy.Role(nil), actor.Roles...)}
				read.Targets = append([]authorize.AuthorityTarget(nil), membershipTargets...)
			}
		}
		reads = append(reads, read)
	}
	return reads
}
