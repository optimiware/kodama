package service

import (
	"context"

	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// AdminPort applies the three complete service-administration commit units.
//
// Create, Update and SetStatus are separate named commits, never a generic
// Save/Update/SetState split. Each adapter must compare the expected version
// (and the expected state for Update), re-read the supplied current authority
// and commit the service row, receipt, audit and event publication intent in
// one transaction. SetStatus must additionally apply the StatusUnit.RevokeAll
// obligation for a disablement: it clears the service's one current nonce/expiry
// pair in that same transaction, guarded by StatusUnit.ExpectedVersion on the
// same row, and invalidates established connections afterwards. A disablement is
// valid whether or not the service currently holds a credential. Enablement
// carries no credential and can never resurrect a revoked one.
//
// Credential issue/rotate/revoke are deliberately absent: they remain the
// token.MutationUnit commit owned by the token package. OIDC/key material and
// service discovery sit behind adapter seams; a second service store here would
// split an atomic credential commit S05 requires.
//
// ServiceReader is the one read seam needed to resolve a service's current
// state, roles and version before authenticating or mutating it. It is a read,
// never a mutation split.

// AdminPort applies one complete service-administration commit unit per method.
type AdminPort interface {
	Create(context.Context, CreateUnit) (CreateResult, error)
	Update(context.Context, UpdateUnit) (UpdateResult, error)
	SetStatus(context.Context, StatusUnit) (StatusResult, error)
}

// ServiceReadRequest asks for one service account's current committed state.
type ServiceReadRequest struct {
	Scope     tenancy.Scope `json:"scope"`
	ServiceID ServiceID     `json:"service_id"`
}

// ServiceReader reads one service account's current state, roles, scope,
// version and credential metadata. The fact is current evidence for one
// authentication or commit, never durable permission.
type ServiceReader interface {
	ReadService(context.Context, ServiceReadRequest) (Service, error)
}
