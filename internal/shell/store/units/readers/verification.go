package readers

import (
	"context"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// VerificationReader is the store-backed verify.VerificationReader.
//
// It reads one committed verification fact by identity inside the report
// command's own owner scope. A record outside that scope and an unknown
// identity are uniformly not_found; the reader never widens the read and never
// returns another project's fact. The decoded value is rebuilt only from the
// columns the platform record adapter writes, so a returned Record is a durable
// fact, never a bearer grant or cached permission.
type VerificationReader struct {
	executor store.Executor
}

// NewVerificationReader builds the verification reader.
func NewVerificationReader(executor store.Executor) (*VerificationReader, error) {
	if executor == nil {
		return nil, fault.New(fault.InvalidInput, "verification reader requires an executor")
	}
	return &VerificationReader{executor: executor}, nil
}

var _ verify.VerificationReader = (*VerificationReader)(nil)

// ReadVerification returns one committed verification record inside scope.
func (reader *VerificationReader) ReadVerification(ctx context.Context, scope tenancy.Scope, verificationID verify.VerificationID) (verify.Record, error) {
	if err := ctx.Err(); err != nil {
		return verify.Record{}, err
	}
	if err := scope.Validate(); err != nil {
		return verify.Record{}, err
	}
	if scope.TenantScoped() {
		return verify.Record{}, fault.New(fault.InvalidInput, "a verification read requires a project scope")
	}
	if _, err := verify.ParseVerificationID(string(verificationID)); err != nil {
		return verify.Record{}, err
	}
	row, err := reader.executor.Read(ctx, scope, "verifications", string(verificationID))
	if err != nil {
		return verify.Record{}, err
	}
	return verificationFromRow(row, scope)
}

// verificationFromRow rebuilds one verification record from the exact columns
// the platform record adapter writes. The row does not persist the actor's
// roles, enablement or version; those are current-authority facts, not durable
// verification facts. The actor identity, scope and kind are reconstructed from
// the stored actor id, row scope and nullable execution id, and the record is
// validated so a corrupt row is an internal failure rather than a usable fact.
func verificationFromRow(row store.Row, scope tenancy.Scope) (verify.Record, error) {
	tenant, err := rowString(row, "tenant_id")
	if err != nil {
		return verify.Record{}, err
	}
	project, err := rowString(row, "project_id")
	if err != nil {
		return verify.Record{}, err
	}
	rowScope := tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)}
	if rowScope != scope {
		return verify.Record{}, fault.New(fault.NotFound, "verification record is outside the read scope")
	}
	verificationID, err := rowString(row, "id")
	if err != nil {
		return verify.Record{}, err
	}
	callingService, err := rowString(row, "service_id")
	if err != nil {
		return verify.Record{}, err
	}
	bindingID, err := rowString(row, "binding_id")
	if err != nil {
		return verify.Record{}, err
	}
	resourceID, err := rowString(row, "resource_id")
	if err != nil {
		return verify.Record{}, err
	}
	operation, err := rowString(row, "operation")
	if err != nil {
		return verify.Record{}, err
	}
	resultText, err := rowString(row, "result")
	if err != nil {
		return verify.Record{}, err
	}
	result := verify.Result(resultText)
	if !result.Valid() {
		return verify.Record{}, fault.New(fault.InternalFailure, "stored verification result is unknown")
	}
	approved, err := rowBool(row, "approved")
	if err != nil {
		return verify.Record{}, err
	}
	at, err := rowTime(row, "created_at")
	if err != nil {
		return verify.Record{}, err
	}
	executionText, err := rowNullableString(row, "execution_id")
	if err != nil {
		return verify.Record{}, err
	}
	actionText, err := rowNullableString(row, "action_id")
	if err != nil {
		return verify.Record{}, err
	}
	subjectData, err := rowBytes(row, "subject")
	if err != nil {
		return verify.Record{}, err
	}
	var subject verify.Subject
	if err := decodeJSON(subjectData, &subject); err != nil {
		return verify.Record{}, err
	}
	approvalData, err := rowBytes(row, "requested_approval")
	if err != nil {
		return verify.Record{}, err
	}
	var approval request.Assignment
	if len(approvalData) > 0 {
		if err := decodeJSON(approvalData, &approval); err != nil {
			return verify.Record{}, err
		}
	}
	actor, err := verificationActor(row, rowScope, id.ExecutionID(executionText))
	if err != nil {
		return verify.Record{}, err
	}
	record := verify.Record{
		ID:                verify.VerificationID(verificationID),
		Scope:             rowScope,
		CallingService:    service.ServiceID(callingService),
		BindingID:         resource.BindingID(bindingID),
		Actor:             actor,
		ExecutionID:       id.ExecutionID(executionText),
		ActionID:          id.ActionID(actionText),
		ResourceID:        resource.ResourceID(resourceID),
		Operation:         operation,
		Subject:           subject,
		RequestedApproval: approval,
		Result:            result,
		Approved:          approved,
		At:                at,
	}
	if err := record.Actor.Validate(); err != nil {
		return verify.Record{}, fault.Wrap(fault.InternalFailure, "stored verification actor is invalid", err)
	}
	return record, nil
}

// verificationActor reconstructs the actor identity the row records. Only the
// actor id, scope and nullable execution id are durable; a present execution id
// identifies an execution principal, otherwise the actor is a human membership
// holder. Roles, enablement and version are deliberately not re-read: the
// verification record is a fact about an accepted effect, not current
// authority.
func verificationActor(row store.Row, scope tenancy.Scope, executionID id.ExecutionID) (identity.Principal, error) {
	actorID, err := rowString(row, "actor_id")
	if err != nil {
		return identity.Principal{}, err
	}
	actor := identity.Principal{
		ID:      id.PrincipalID(actorID),
		Kind:    identity.KindHuman,
		Scope:   scope,
		Enabled: true,
	}
	if executionID != "" {
		actor.Kind = identity.KindExecution
		actor.ExecutionID = executionID
	}
	return actor, nil
}
