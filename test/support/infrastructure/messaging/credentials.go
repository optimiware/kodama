package messaging

import (
	"context"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// credentialEntry is one credential-to-principal binding with its live state.
type credentialEntry struct {
	credential string
	principal  identity.Principal
	revoked    bool
	expired    bool
}

// Directory is a mutable test credential directory. It implements the broker's
// authorize.CurrentPrincipal and authorize.Authorizer ports so a real
// subscription can be revoked, rotated or expired while it is open.
type Directory struct {
	mu           sync.Mutex
	byCredential map[string]*credentialEntry
	byPrincipal  map[id.PrincipalID]*credentialEntry
	grants       map[id.PrincipalID]map[authorize.OperationName]bool
}

// NewDirectory builds an empty credential directory.
func NewDirectory() *Directory {
	return &Directory{
		byCredential: map[string]*credentialEntry{},
		byPrincipal:  map[id.PrincipalID]*credentialEntry{},
		grants:       map[id.PrincipalID]map[authorize.OperationName]bool{},
	}
}

// Add registers one credential for one enabled principal and grants
// event.subscribe and event.publish by default.
func (directory *Directory) Add(credential string, principal identity.Principal) *Directory {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	entry := &credentialEntry{credential: credential, principal: principal}
	directory.byCredential[credential] = entry
	directory.byPrincipal[principal.ID] = entry
	directory.grants[principal.ID] = map[authorize.OperationName]bool{
		"event.subscribe": true,
		"event.publish":   true,
	}
	return directory
}

// Resolve implements authorize.CurrentPrincipal. A missing, revoked, expired
// or disabled credential is unauthenticated.
func (directory *Directory) Resolve(_ context.Context, credential string) (identity.Principal, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	entry, ok := directory.byCredential[credential]
	if !ok || entry.revoked || entry.expired || !entry.principal.Enabled {
		return identity.Principal{}, fault.New(fault.Unauthenticated, "credential is not current")
	}
	return entry.principal, nil
}

// Authorize implements authorize.Authorizer. It re-reads the live entry so a
// revoked, expired or disabled principal loses the grant immediately.
func (directory *Directory) Authorize(_ context.Context, principal identity.Principal, operation authorize.OperationName, scope tenancy.Scope) (authorize.Decision, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	entry, ok := directory.byPrincipal[principal.ID]
	if !ok || entry.revoked || entry.expired || !entry.principal.Enabled {
		return authorize.DecisionDeny, nil
	}
	if entry.principal.Scope != scope {
		return authorize.DecisionDeny, nil
	}
	if directory.grants[principal.ID][operation] {
		return authorize.DecisionAllow, nil
	}
	return authorize.DecisionDeny, nil
}

// Revoke makes one credential unusable while keeping the principal record.
func (directory *Directory) Revoke(credential string) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if entry, ok := directory.byCredential[credential]; ok {
		entry.revoked = true
	}
}

// Rotate replaces one credential with a new value. The old value stops
// resolving; the new value resolves to the same principal.
func (directory *Directory) Rotate(oldCredential, newCredential string) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	entry, ok := directory.byCredential[oldCredential]
	if !ok {
		return
	}
	delete(directory.byCredential, oldCredential)
	entry.credential = newCredential
	directory.byCredential[newCredential] = entry
}

// Expire marks one credential past its validity window.
func (directory *Directory) Expire(credential string) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if entry, ok := directory.byCredential[credential]; ok {
		entry.expired = true
	}
}

// SetEnabled disables or re-enables the principal behind one credential.
func (directory *Directory) SetEnabled(credential string, enabled bool) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if entry, ok := directory.byCredential[credential]; ok {
		entry.principal.Enabled = enabled
	}
}

// Grant sets or clears one operation grant for a principal.
func (directory *Directory) Grant(principalID id.PrincipalID, operation authorize.OperationName, allow bool) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if directory.grants[principalID] == nil {
		directory.grants[principalID] = map[authorize.OperationName]bool{}
	}
	directory.grants[principalID][operation] = allow
}

// ServicePrincipal builds one enabled service principal in a scope.
func ServicePrincipal(principalID id.PrincipalID, scope tenancy.Scope) identity.Principal {
	return identity.Principal{
		ID:      principalID,
		Kind:    identity.KindService,
		Scope:   scope,
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	}
}

// ServiceInvalidation builds one committed invalidation for a service
// principal. The time is explicit and never zero.
func ServiceInvalidation(scope tenancy.Scope, principalID id.PrincipalID, serviceID service.ServiceID, reason token.InvalidationReason, at time.Time) token.Invalidation {
	return token.Invalidation{
		Scope:       scope,
		PrincipalID: principalID,
		ServiceID:   serviceID,
		Reason:      reason,
		At:          at,
	}
}
