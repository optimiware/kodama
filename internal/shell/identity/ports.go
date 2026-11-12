package identity

import (
	"context"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// This package declares no effectful identity mutation port.
//
// The Shell authenticates a bearer credential through authorize.CurrentPrincipal
// and re-reads current membership/service/execution state through
// authorize.AuthorityReader. Concrete OIDC, platform and key adapters sit
// behind their own seams; a second identity repository here would duplicate the
// authorization boundary S05 requires to stay single.
//
// PrincipalReader is the one read seam needed to resolve a verified principal's
// current roles, enablement, scope and version before deriving or checking a
// credential. It is a read, never a mutation split.

// PrincipalRequest asks for one principal's current committed state.
type PrincipalRequest struct {
	Scope       tenancy.Scope  `json:"scope"`
	PrincipalID id.PrincipalID `json:"principal_id"`
}

// PrincipalReader reads one principal's current roles, enablement, scope and
// version. The fact is current authority evidence for one commit, never
// durable permission.
type PrincipalReader interface {
	ReadPrincipal(context.Context, PrincipalRequest) (Principal, error)
}
