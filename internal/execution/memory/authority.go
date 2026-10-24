package memory

import (
	"context"
	"sync"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/memdb"
)

// Authority is a table-driven execution.Authority and TxAuthority: the fake
// authority of the unit layer. A principal is resolvable only in the scopes
// where it holds a grant; a missing permission is ErrDenied; SetErr makes every
// call an outage. Revoking takes effect for the next check, including a check
// made inside an open transaction (the authorization-revision recheck of the
// real authority).
type Authority struct {
	mu     sync.Mutex
	grants map[grantKey]map[execution.Permission]bool
	err    error
	checks int
}

type grantKey struct {
	scope     domain.Scope
	principal string
}

var (
	_ execution.Authority   = (*Authority)(nil)
	_ execution.TxAuthority = (*Authority)(nil)
)

// NewAuthority returns an authority that knows no principal.
func NewAuthority() *Authority {
	return &Authority{grants: map[grantKey]map[execution.Permission]bool{}}
}

// Grant makes the principal resolvable in the scope with exactly perms (run
// read and manage when none is given).
func (a *Authority) Grant(scope domain.Scope, principal string, perms ...execution.Permission) {
	if len(perms) == 0 {
		perms = []execution.Permission{execution.PermRunRead, execution.PermRunManage}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	k := grantKey{scope, principal}
	a.grants[k] = map[execution.Permission]bool{}
	for _, p := range perms {
		a.grants[k][p] = true
	}
}

// Revoke removes every grant of the principal in the scope.
func (a *Authority) Revoke(scope domain.Scope, principal string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.grants, grantKey{scope, principal})
}

// SetErr makes the authority fail like an outage (nil recovers).
func (a *Authority) SetErr(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.err = err
}

// Checks reports how many permission checks were answered.
func (a *Authority) Checks() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.checks
}

// ResolveActor implements execution.Authority. A principal with no grant in the
// scope is hidden.
func (a *Authority) ResolveActor(_ context.Context, scope domain.Scope, principal string) (execution.Actor, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return execution.Actor{}, a.err
	}
	if _, ok := a.grants[grantKey{scope, principal}]; !ok {
		return execution.Actor{}, execution.ErrHidden
	}
	return execution.Actor{Type: execution.ActorUser, ID: principal}, nil
}

// Authorize implements execution.Authority and execution.TxAuthority.
func (a *Authority) Authorize(_ context.Context, actor execution.Actor, scope domain.Scope, perm execution.Permission) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.checks++
	if a.err != nil {
		return a.err
	}
	if !a.grants[grantKey{scope, actor.ID}][perm] {
		return execution.ErrDenied
	}
	return nil
}

// Bind is the AuthorityBinder of a Store: the check bound to one transaction.
func (a *Authority) Bind(memdb.Tx) execution.TxAuthority { return a }
