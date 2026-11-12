package authorize

import (
	"context"

	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// CurrentPrincipal authenticates one presented credential and returns the
// verified principal. The token is the raw bearer material; implementations
// must verify it in constant time against the current tenant key and the
// current principal nonce/expiry, and must never log or persist it.
//
// Authentication has one bootstrap step that is not authority: an
// implementation may call the store's credential lookup seam
// (store.CredentialLookup) with the presented kind and identity to obtain a
// candidate tenant/project/principal scope, then verify the MAC and the current
// credential state in that candidate scope before returning a Principal. The
// candidate scope is never permission; a missing or mismatched lookup is the
// same unauthenticated refusal as a bad MAC, and the lookup has no listing or
// scan form. The implementation owns that orchestration; this port keeps its
// single Resolve signature.
type CurrentPrincipal interface {
	Resolve(context.Context, string) (Principal, error)
}

// Authorizer answers whether the verified principal currently holds the grant
// for one operation in one scope.
//
// Authorize only answers the grant question. The commit boundary must still
// read the operation's Authority checks, because a previously calculated allow
// is not durable permission.
type Authorizer interface {
	Authorize(context.Context, Principal, OperationName, tenancy.Scope) (Decision, error)
}

// AuthorityReader reads the current facts named by AuthorityRead values inside
// the caller's atomic commit unit. Implementations must observe committed
// state, not a cache, and must return one fact per read in input order.
type AuthorityReader interface {
	Read(context.Context, []AuthorityRead) ([]AuthorityFact, error)
}
