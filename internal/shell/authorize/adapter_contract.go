// Package authorize declares the OAuth-shaped hosted-authority seam.
//
// The Platform integration is two OAuth applications behind Auth0 SSO. Each
// application matches the OAuth tenant claim and role string against its own
// RBAC table; RBAC is not shared between applications. This package declares
// the client claim match, the per-application RBAC table and the context-first
// Check shape only. The exact Auth0 tenant URLs, client ids and callback paths
// are installation configuration, never hardcoded here.
//
// A positive response is current authority evidence for one commit, never
// durable permission. No HTTP client, vendor SDK or policy engine exists in
// this declaration.
package authorize

import (
	"context"
	"fmt"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// RBACTable is one application's flexible role -> access-string table.
//
// It is deliberately data, not a shared policy engine: each OAuth application
// owns its own table and no role or access vocabulary is merged across
// applications. Validation refuses an empty table, an empty role name, a role
// with no access strings, and empty or duplicate access strings.
type RBACTable map[string][]string

// Validate checks that every role has at least one unique, non-empty access
// string and that role names contain no whitespace.
func (table RBACTable) Validate() error {
	if len(table) == 0 {
		return fault.New(fault.InvalidInput, "RBAC table is empty")
	}
	for role, access := range table {
		if !validRBACToken(role) {
			return fault.New(fault.InvalidInput, fmt.Sprintf("invalid RBAC role %q", role))
		}
		if len(access) == 0 {
			return fault.New(fault.InvalidInput, fmt.Sprintf("RBAC role %q has no access strings", role))
		}
		seen := make(map[string]bool, len(access))
		for _, value := range access {
			if !validRBACToken(value) {
				return fault.New(fault.InvalidInput, fmt.Sprintf("RBAC role %q has an invalid access string %q", role, value))
			}
			if seen[value] {
				return fault.New(fault.InvalidInput, fmt.Sprintf("RBAC role %q repeats access string %q", role, value))
			}
			seen[value] = true
		}
	}
	return nil
}

// Lookup returns a copy of the access strings granted to one role. An unknown
// role returns false and no access.
func (table RBACTable) Lookup(role string) ([]string, bool) {
	access, ok := table[role]
	if !ok {
		return nil, false
	}
	return append([]string(nil), access...), true
}

// Allows reports whether the role's own table grants one access string.
func (table RBACTable) Allows(role, access string) bool {
	for _, granted := range table[role] {
		if granted == access {
			return true
		}
	}
	return false
}

func validRBACToken(value string) bool {
	return value != "" && !strings.ContainsAny(value, " \t\r\n")
}

// HostedAuthorityRequest is one OAuth-shaped hosted-authority check.
//
// The request carries the OAuth protocol, verified ownership scope, principal,
// operation, the declared claim match and the access strings the operation
// requires. The hosted application matches the tenant claim and role string
// against its own RBAC table; the Shell never supplies or merges that table.
type HostedAuthorityRequest struct {
	Protocol       identity.OAuthProtocol
	Scope          tenancy.Scope
	PrincipalID    id.PrincipalID
	Operation      string
	Claims         identity.OAuthClaims
	RequiredAccess []string
}

// Validate checks the OAuth protocol, scope, principal, operation, claim match
// and the unique non-empty required access strings.
func (request HostedAuthorityRequest) Validate() error {
	if !request.Protocol.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown hosted authority OAuth protocol %q", request.Protocol))
	}
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if _, err := id.ParsePrincipalID(string(request.PrincipalID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "hosted authority principal id is invalid", err)
	}
	if !validRBACToken(request.Operation) {
		return fault.New(fault.InvalidInput, "hosted authority operation is empty or contains whitespace")
	}
	if err := request.Claims.Validate(); err != nil {
		return err
	}
	if len(request.RequiredAccess) == 0 {
		return fault.New(fault.InvalidInput, "hosted authority required access is empty")
	}
	seen := make(map[string]bool, len(request.RequiredAccess))
	for _, access := range request.RequiredAccess {
		if !validRBACToken(access) {
			return fault.New(fault.InvalidInput, fmt.Sprintf("invalid hosted authority required access %q", access))
		}
		if seen[access] {
			return fault.New(fault.InvalidInput, fmt.Sprintf("hosted authority required access %q is repeated", access))
		}
		seen[access] = true
	}
	return nil
}

// HostedAuthorityDecision is the application's OAuth RBAC decision for one
// check. Allowed is the authority result, Role is the matched application role
// and Access is the application-owned access list the role grants.
type HostedAuthorityDecision struct {
	Allowed bool
	Role    string
	Access  []string
}

// Validate checks the decision shape. An allowed decision names a matched role
// and at least one access string; a refusal may omit both. Access strings are
// unique and non-empty.
func (decision HostedAuthorityDecision) Validate() error {
	if decision.Allowed {
		if !validRBACToken(decision.Role) {
			return fault.New(fault.InvalidInput, "allowed hosted authority decision has no matched role")
		}
		if len(decision.Access) == 0 {
			return fault.New(fault.InvalidInput, "allowed hosted authority decision has no access")
		}
	}
	seen := make(map[string]bool, len(decision.Access))
	for _, access := range decision.Access {
		if !validRBACToken(access) {
			return fault.New(fault.InvalidInput, fmt.Sprintf("invalid hosted authority decision access %q", access))
		}
		if seen[access] {
			return fault.New(fault.InvalidInput, fmt.Sprintf("hosted authority decision access %q is repeated", access))
		}
		seen[access] = true
	}
	return nil
}

// HostedAuthorityResponse is one OAuth-shaped hosted-authority answer. A
// response without a valid OAuth protocol cannot be interpreted and is refused.
type HostedAuthorityResponse struct {
	Protocol identity.OAuthProtocol
	Decision HostedAuthorityDecision
}

// Validate checks the response protocol and decision shape.
func (response HostedAuthorityResponse) Validate() error {
	if !response.Protocol.Valid() {
		return fault.New(fault.InvalidInput, "hosted authority response OAuth protocol is unknown")
	}
	return response.Decision.Validate()
}

// HostedAuthority checks one operation against one OAuth application's hosted
// authority. It is a single Check call, not a vendor-specific protocol surface
// and not a shared policy engine.
type HostedAuthority interface {
	Check(context.Context, HostedAuthorityRequest) (HostedAuthorityResponse, error)
}
