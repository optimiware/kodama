package executiontest

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/memdb"
)

// ContractClaim is the runtime session the shared contract suites record begins
// and receipts under. Their stores carry the permissive fences below: the suites
// prove the recording semantics, and the grant check itself is proved by its own
// tests against a real fence (MemGrants in memory, SQLGrantFence in PostgreSQL).
var ContractClaim = execution.GrantClaim{Session: "ses_contractcontractcontract", Generation: 1}

// OpenMemFence is the memory fence of the shared contract suites: every claim is
// live. Never a composition's fence.
type OpenMemFence struct{}

// AttemptGrantLive implements memory.GrantFencer.
func (OpenMemFence) AttemptGrantLive(memdb.Tx, domain.Scope, domain.AttemptID, execution.GrantClaim) error {
	return nil
}

// ResourceGrantLive implements memory.GrantFencer.
func (OpenMemFence) ResourceGrantLive(memdb.Tx, domain.Scope, domain.RunID, string, execution.GrantClaim) error {
	return nil
}

// OpenSQLFence is the PostgreSQL counterpart of OpenMemFence.
type OpenSQLFence struct{}

// AttemptGrantLive implements postgres.GrantFencer.
func (OpenSQLFence) AttemptGrantLive(context.Context, pgx.Tx, domain.Scope, domain.AttemptID, execution.GrantClaim) error {
	return nil
}

// ResourceGrantLive implements postgres.GrantFencer.
func (OpenSQLFence) ResourceGrantLive(context.Context, pgx.Tx, domain.Scope, domain.RunID, string, execution.GrantClaim) error {
	return nil
}

type grantRec struct {
	Scope      domain.Scope
	Run        domain.RunID
	Key        string // "" for an attempt's grant
	Attempt    domain.AttemptID
	Claim      execution.GrantClaim
	Revoked    bool
	identifier string
}

var memGrantsT = memdb.NewTable("executiontest_runtime_grants", func(r grantRec) string { return r.identifier })

// MemGrants is a memory runtime-grant table on the SAME kernel as an execution
// store, joined to the store's transactions as a memory.GrantFencer. Its
// revocations are standalone memdb transactions, like the runtime authority's:
// they touch nothing of execution's.
type MemGrants struct {
	db *memdb.Store
	mu sync.Mutex
}

// NewMemGrants returns the table over the kernel.
func NewMemGrants(db *memdb.Store) *MemGrants { return &MemGrants{db: db} }

func attemptKey(s domain.Scope, a domain.AttemptID) string {
	return fmt.Sprintf("a|%s|%s|%s", s.TenantID, s.ProjectID, a)
}

func resourceKey(s domain.Scope, r domain.RunID, key string) string {
	return fmt.Sprintf("r|%s|%s|%s|%s", s.TenantID, s.ProjectID, r, key)
}

// IssueAttempt registers a consumed grant of an attempt.
func (g *MemGrants) IssueAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, claim execution.GrantClaim) error {
	return g.db.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		return memGrantsT.Put(tx, grantRec{Scope: scope, Attempt: attempt, Claim: claim, identifier: attemptKey(scope, attempt)})
	})
}

// IssueResource registers a consumed grant of a run resource.
func (g *MemGrants) IssueResource(ctx context.Context, scope domain.Scope, run domain.RunID, key string, claim execution.GrantClaim) error {
	return g.db.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		return memGrantsT.Put(tx, grantRec{Scope: scope, Run: run, Key: key, Claim: claim, identifier: resourceKey(scope, run, key)})
	})
}

func (g *MemGrants) revoke(ctx context.Context, scope domain.Scope, id string) error {
	return g.db.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		r, ok := memGrantsT.Get(tx, id)
		if !ok {
			return nil
		}
		r.Revoked = true
		return memGrantsT.Put(tx, r)
	})
}

// RevokeAttempt is the runtime authority's standalone revocation of an attempt's
// grant: its own transaction, no execution state.
func (g *MemGrants) RevokeAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) error {
	return g.revoke(ctx, scope, attemptKey(scope, attempt))
}

// RevokeResource is the standalone revocation of a resource's grant.
func (g *MemGrants) RevokeResource(ctx context.Context, scope domain.Scope, run domain.RunID, key string) error {
	return g.revoke(ctx, scope, resourceKey(scope, run, key))
}

// AttemptGrantLive implements memory.GrantFencer.
func (g *MemGrants) AttemptGrantLive(tx memdb.Tx, scope domain.Scope, attempt domain.AttemptID, claim execution.GrantClaim) error {
	r, ok := memGrantsT.Get(tx, attemptKey(scope, attempt))
	if !ok || r.Revoked || r.Claim != claim {
		return errors.Join(execution.ErrAttemptRevoked, errors.New("the runtime grant is not live"))
	}
	return nil
}

// ResourceGrantLive implements memory.GrantFencer.
func (g *MemGrants) ResourceGrantLive(tx memdb.Tx, scope domain.Scope, run domain.RunID, key string, claim execution.GrantClaim) error {
	r, ok := memGrantsT.Get(tx, resourceKey(scope, run, key))
	if !ok || r.Revoked || r.Claim != claim {
		return errors.Join(execution.ErrResourceRevoked, errors.New("the runtime grant is not live"))
	}
	return nil
}
