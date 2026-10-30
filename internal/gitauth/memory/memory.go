// Package memory is the in-memory grant store of gitauth over the memdb kernel
// (ADR 0009, SD-13/SD-14): the `development` environment's adapter. Do is one
// memdb.Update, so everything a transaction wrote commits or is discarded
// together, and a caller that already holds a memdb.Tx joins it with Bind. It is
// not evidence for SQL, row-level security or lock behavior; those obligations
// stay with the PostgreSQL adapter and its tests.
package memory

import (
	"context"
	"errors"
	"fmt"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitauth"
	"github.com/optimiware/kodama/internal/memdb"
)

var grantsT = memdb.NewTable("git_grants",
	func(g gitauth.Grant) string { return g.ID() },
	func(g gitauth.Grant) string { return g.Digest.Hex() },
	func(g gitauth.Grant) string { return g.IssueKey },
)

// auditsT holds the audit records of tenant-wide revocations (the PostgreSQL
// adapter appends to the shared audit table).
var auditsT = memdb.NewTable("git_grant_audit",
	func(r gitauth.AuditRecord) string { return r.ID },
)

// Store implements gitauth.Store over memdb. It is development-only.
type Store struct{ db *memdb.Store }

var _ gitauth.Store = (*Store)(nil)

// New requires the memdb kernel.
func New(db *memdb.Store) (*Store, error) {
	if db == nil {
		return nil, errors.New("gitauth/memory: a memdb store is required")
	}
	return &Store{db: db}, nil
}

// DevelopmentOnly marks the adapter for the composition root: production
// refuses it (SD-12).
func (*Store) DevelopmentOnly() bool { return true }

// Count reports how many grants the tenant holds. It is a test and diagnostics
// aid; the use cases never list grants.
func (s *Store) Count(ctx context.Context, tenant domain.TenantID) (n int, err error) {
	err = s.db.View(ctx, tenant, func(tx memdb.Tx) error {
		grantsT.Scan(tx, func(gitauth.Grant) bool { n++; return true })
		return nil
	})
	return n, err
}

// Audits returns the audit records the tenant's grant transactions committed,
// in no particular order. It is a test and diagnostics aid.
func (s *Store) Audits(ctx context.Context, tenant domain.TenantID) (out []gitauth.AuditRecord, err error) {
	err = s.db.View(ctx, tenant, func(tx memdb.Tx) error {
		auditsT.Scan(tx, func(r gitauth.AuditRecord) bool { out = append(out, r); return true })
		return nil
	})
	return out, err
}

// Do runs fn in one tenant transaction. A commit that fails after fn succeeded
// has an unknown outcome and is reported as ErrUnavailable.
func (s *Store) Do(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx gitauth.GrantTx) error) error {
	ran := false
	err := s.db.Update(ctx, tenant, func(tx memdb.Tx) error {
		if err := fn(ctx, Bind(tx)); err != nil {
			return err
		}
		ran = true
		return nil
	})
	if err != nil && ran {
		return errors.Join(gitauth.ErrUnavailable, err)
	}
	return err
}

// Bind adapts a Tx that already carries the tenant (one opened by
// memdb.Store.Update, or the execution owner's own) to gitauth.GrantTx: the
// integration seam, as postgres.Bind is for a pgx.Tx.
func Bind(tx memdb.Tx) gitauth.GrantTx { return &grantTx{tx: tx, tenant: tx.Tenant()} }

type grantTx struct {
	tx     memdb.Tx
	tenant domain.TenantID
}

var _ gitauth.GrantTx = (*grantTx)(nil)

// LockSeed is a no-op: memdb serializes writers, so concurrent seed requests
// are already decided one after the other.
func (t *grantTx) LockSeed(context.Context, string) error { return nil }

func (t *grantTx) GrantByDigest(_ context.Context, d gitauth.Digest) (gitauth.Grant, error) {
	var out gitauth.Grant
	found := false
	grantsT.Scan(t.tx, func(g gitauth.Grant) bool {
		if g.Digest.Equal(d) {
			out, found = g.Clone(), true
			return false
		}
		return true
	})
	if !found {
		return gitauth.Grant{}, gitauth.ErrNotFound
	}
	return out, nil
}

func (t *grantTx) GrantByID(_ context.Context, id string, _ bool) (gitauth.Grant, error) {
	g, ok := grantsT.Get(t.tx, id)
	if !ok {
		return gitauth.Grant{}, gitauth.ErrNotFound
	}
	return g.Clone(), nil
}

func (t *grantTx) GrantByIssueKey(_ context.Context, key string, _ bool) (gitauth.Grant, error) {
	var out gitauth.Grant
	found := false
	grantsT.Scan(t.tx, func(g gitauth.Grant) bool {
		if g.IssueKey == key && key != "" {
			out, found = g.Clone(), true
			return false
		}
		return true
	})
	if !found {
		return gitauth.Grant{}, gitauth.ErrNotFound
	}
	return out, nil
}

func (t *grantTx) Insert(_ context.Context, g gitauth.Grant) error {
	if g.Claims.TenantID != string(t.tenant) {
		return fmt.Errorf("gitauth/memory: a grant of another tenant cannot be stored")
	}
	if err := g.Claims.Validate(); err != nil || g.Digest.IsZero() {
		return fmt.Errorf("gitauth/memory: invalid grant: %v", err)
	}
	if _, exists := grantsT.Get(t.tx, g.ID()); exists {
		return errors.Join(gitauth.ErrConflict, errors.New("duplicate grant"))
	}
	if err := grantsT.Put(t.tx, g.Clone()); err != nil {
		if errors.Is(err, memdb.ErrConflict) {
			return errors.Join(gitauth.ErrConflict, err)
		}
		return err
	}
	return nil
}

// Update stores the changeable part of a grant only: the digest of an unrevoked
// grant, a write-once consumption and an irreversible revocation.
func (t *grantTx) Update(_ context.Context, next gitauth.Grant) error {
	cur, ok := grantsT.Get(t.tx, next.ID())
	if !ok {
		return gitauth.ErrNotFound
	}
	cur = cur.Clone()
	if next.Digest != cur.Digest {
		if cur.Revoked() || cur.Consumption != nil {
			return errors.New("gitauth/memory: the credential of a revoked or consumed grant cannot be replaced")
		}
		cur.Digest = next.Digest
	}
	switch {
	case next.Consumption != nil && cur.Consumption == nil:
		c := *next.Consumption
		cur.Consumption = &c
	case next.Consumption == nil && cur.Consumption != nil,
		next.Consumption != nil && (next.Consumption.ReceiveID != cur.Consumption.ReceiveID || !next.Consumption.At.Equal(cur.Consumption.At)):
		return errors.New("gitauth/memory: the consumption is write-once")
	}
	switch {
	case next.RevokedAt != nil && cur.RevokedAt == nil:
		r := *next.RevokedAt
		cur.RevokedAt = &r
	case next.RevokedAt == nil && cur.RevokedAt != nil:
		return errors.New("gitauth/memory: the revocation is irreversible")
	}
	return grantsT.Put(t.tx, cur)
}

func (t *grantTx) RevokeAttempt(_ context.Context, scope domain.Scope, attempt domain.AttemptID, now time.Time) (int, error) {
	if scope.TenantID != t.tenant {
		return 0, errors.New("gitauth/memory: another tenant")
	}
	return t.revokeWhere(now, func(g gitauth.Grant) bool {
		id, ok := g.AttemptID()
		return ok && id == attempt && g.Claims.ProjectID == string(scope.ProjectID)
	})
}

func (t *grantTx) RevokeSeeds(_ context.Context, repositoryID string, now time.Time) (int, error) {
	return t.revokeWhere(now, func(g gitauth.Grant) bool {
		return g.Claims.Kind == gitv1.GrantSeed && g.Consumption == nil && g.Names(repositoryID)
	})
}

func (t *grantTx) RevokeTenant(_ context.Context, now time.Time) (int, error) {
	return t.revokeWhere(now, func(gitauth.Grant) bool { return true })
}

func (t *grantTx) ActiveGrants(_ context.Context, now time.Time) (int, error) {
	n := 0
	grantsT.Scan(t.tx, func(g gitauth.Grant) bool {
		if !g.Revoked() && now.Before(g.ExpiresAt()) {
			n++
		}
		return true
	})
	return n, nil
}

func (t *grantTx) RecordAudit(_ context.Context, r gitauth.AuditRecord) error {
	if _, exists := auditsT.Get(t.tx, r.ID); exists {
		return errors.Join(gitauth.ErrConflict, errors.New("duplicate audit record"))
	}
	return auditsT.Put(t.tx, r)
}

func (t *grantTx) revokeWhere(now time.Time, match func(gitauth.Grant) bool) (int, error) {
	var hits []gitauth.Grant
	grantsT.Scan(t.tx, func(g gitauth.Grant) bool {
		if !g.Revoked() && match(g) {
			hits = append(hits, g.Clone())
		}
		return true
	})
	for _, g := range hits {
		g.Revoke(now)
		if err := grantsT.Put(t.tx, g); err != nil {
			return 0, err
		}
	}
	return len(hits), nil
}

// RevokeAttempt revokes every grant of the attempt in tx and reports how many
// were newly revoked, as postgres.RevokeAttempt does.
func RevokeAttempt(tx memdb.Tx, scope domain.Scope, attempt domain.AttemptID, now time.Time) (int, error) {
	return gitauth.RevokeAttemptIn(context.Background(), Bind(tx), scope, attempt, now)
}

// RevokeAll revokes every grant of the transaction's tenant in tx and records
// the audit entry, as postgres.RevokeAll does, and returns how many it revoked.
func RevokeAll(tx memdb.Tx, reason string, actor gitauth.AuditActor, now time.Time) (int, error) {
	id, err := gitauth.NewAuditID(nil)
	if err != nil {
		return 0, err
	}
	return gitauth.RevokeAllIn(context.Background(), Bind(tx), reason, actor, now, id)
}
