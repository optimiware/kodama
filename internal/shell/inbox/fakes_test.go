package inbox_test

// Deterministic fakes for the inbox service tests. Nothing here touches a
// clock, network, store or broker: the inbox is a pure query over an injected
// QueryPort, so a recorder is enough to assert the exact scoped query and the
// absence of any mutation seam.

import (
	"context"
	"sync"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/inbox"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

type authorizeCall struct {
	Principal authorize.Principal
	Operation authorize.OperationName
	Scope     tenancy.Scope
}

type fakeAuthorizer struct {
	mu        sync.Mutex
	calls     []authorizeCall
	authorize func(context.Context, authorize.Principal, authorize.OperationName, tenancy.Scope) (authorize.Decision, error)
}

func (authorizer *fakeAuthorizer) Authorize(ctx context.Context, principal authorize.Principal, operation authorize.OperationName, scope tenancy.Scope) (authorize.Decision, error) {
	authorizer.mu.Lock()
	authorizer.calls = append(authorizer.calls, authorizeCall{Principal: principal, Operation: operation, Scope: scope})
	callback := authorizer.authorize
	authorizer.mu.Unlock()
	if callback != nil {
		return callback(ctx, principal, operation, scope)
	}
	return registryDecision(principal, operation), nil
}

func (authorizer *fakeAuthorizer) callCount() int {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	return len(authorizer.calls)
}

func (authorizer *fakeAuthorizer) captured() []authorizeCall {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	return append([]authorizeCall(nil), authorizer.calls...)
}

// registryDecision answers from the one operation registry so the fake never
// invents a grant the production authorizer would refuse (for example an
// execution principal for an ExecNone operation).
func registryDecision(principal authorize.Principal, operation authorize.OperationName) authorize.Decision {
	registered, ok := authorize.LookupOperation(operation)
	if !ok {
		return authorize.DecisionDeny
	}
	switch principal.Kind {
	case identity.KindHuman:
		for _, role := range registered.Requires.HumanRoles {
			if principal.HasRole(role) {
				return authorize.DecisionAllow
			}
		}
	case identity.KindService:
		for _, role := range registered.Requires.ServiceRoles {
			if principal.HasRole(role) {
				return authorize.DecisionAllow
			}
		}
	case identity.KindExecution:
		if principal.Enabled && registered.Requires.Execution != authorize.ExecNone {
			return authorize.DecisionAllow
		}
	}
	return authorize.DecisionDeny
}

// fakeAuthority answers every requested current fact. By default the fact
// holds; tests override read to model a removed role, a disabled principal or
// a read that fails.
type fakeAuthority struct {
	mu    sync.Mutex
	calls [][]authorize.AuthorityRead
	read  func([]authorize.AuthorityRead) ([]authorize.AuthorityFact, error)
}

func (authority *fakeAuthority) Read(ctx context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	authority.mu.Lock()
	authority.calls = append(authority.calls, append([]authorize.AuthorityRead(nil), reads...))
	callback := authority.read
	authority.mu.Unlock()
	if callback != nil {
		return callback(reads)
	}
	return allowFacts(reads, nil), nil
}

func allowFacts(reads []authorize.AuthorityRead, deny func(authorize.AuthorityRead) bool) []authorize.AuthorityFact {
	facts := make([]authorize.AuthorityFact, 0, len(reads))
	for _, read := range reads {
		allowed := true
		if deny != nil {
			allowed = !deny(read)
		}
		facts = append(facts, authorize.AuthorityFact{
			Check:       read.Check,
			Scope:       read.Scope,
			PrincipalID: read.PrincipalID,
			TargetID:    read.TargetID,
			Version:     read.ExpectedVersion,
			Allowed:     allowed,
		})
	}
	return facts
}

func (authority *fakeAuthority) lastReads() []authorize.AuthorityRead {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if len(authority.calls) == 0 {
		return nil
	}
	return append([]authorize.AuthorityRead(nil), authority.calls[len(authority.calls)-1]...)
}

// fakeQueryPort records the exact query the service passes and answers with a
// scripted authoritative page or error.
type fakeQueryPort struct {
	mu      sync.Mutex
	queries []inbox.Query
	query   func(inbox.Query) (inbox.Page, error)
}

func (port *fakeQueryPort) Query(ctx context.Context, query inbox.Query) (inbox.Page, error) {
	port.mu.Lock()
	port.queries = append(port.queries, query)
	callback := port.query
	port.mu.Unlock()
	if callback != nil {
		return callback(query)
	}
	return inbox.Page{}, nil
}

func (port *fakeQueryPort) callCount() int {
	port.mu.Lock()
	defer port.mu.Unlock()
	return len(port.queries)
}

func (port *fakeQueryPort) captured() []inbox.Query {
	port.mu.Lock()
	defer port.mu.Unlock()
	return append([]inbox.Query(nil), port.queries...)
}

// fakePrincipals resolves the caller's current committed principal at read
// time. By default it reflects the authorized actor in the requested scope;
// tests override read to model role demotion, revocation or disablement.
type fakePrincipals struct {
	mu         sync.Mutex
	calls      []identity.PrincipalRequest
	authorizer *fakeAuthorizer
	read       func(context.Context, identity.PrincipalRequest) (identity.Principal, error)
}

func (principals *fakePrincipals) ReadPrincipal(ctx context.Context, request identity.PrincipalRequest) (identity.Principal, error) {
	principals.mu.Lock()
	principals.calls = append(principals.calls, request)
	callback := principals.read
	authorizer := principals.authorizer
	principals.mu.Unlock()
	if callback != nil {
		return callback(ctx, request)
	}
	if authorizer != nil {
		calls := authorizer.captured()
		for index := len(calls) - 1; index >= 0; index-- {
			if calls[index].Principal.ID == request.PrincipalID {
				current := calls[index].Principal
				current.Scope = request.Scope
				current.Roles = append([]tenancy.Role(nil), current.Roles...)
				return current, nil
			}
		}
	}
	return identity.Principal{}, fault.New(fault.NotFound, "principal not found")
}
