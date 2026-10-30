package gitauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/optimiware/kodama/internal/domain"
)

// Tenant-wide revocation (T-161): offboarding revokes every Git grant of a
// tenant and then observes that none remains usable (IT-12). The tenant must
// already be fenced against new issuance (T-138 closes Git access before it
// revokes); a grant inserted after the revoking transaction commits is a new
// grant, and Active is how the caller sees it.

// Audit vocabulary of the tenant-wide revocation.
const (
	// AuditActionRevokeAll is the audit action recorded when a tenant-wide
	// revocation revoked at least one grant.
	AuditActionRevokeAll = "git.grants.revoked_all"
	// AuditResourceType is the audit resource type; the resource ID is the tenant.
	AuditResourceType = "git_grants"

	// MaxAuditReason and MaxAuditActorID bound the audit columns.
	MaxAuditReason  = 2048
	MaxAuditActorID = 256
)

// AuditActorType is the audit table's actor vocabulary.
type AuditActorType string

// The audit table's actor types.
const (
	AuditActorUser     AuditActorType = "user"
	AuditActorWorkload AuditActorType = "workload"
	AuditActorSystem   AuditActorType = "system"
	AuditActorOperator AuditActorType = "operator"
)

// AuditActor is who asked for a revocation, as the audit trail records it.
type AuditActor struct {
	Type AuditActorType
	ID   string
}

// Validate checks the actor against the audit table's bounds.
func (a AuditActor) Validate() error {
	switch a.Type {
	case AuditActorUser, AuditActorWorkload, AuditActorSystem, AuditActorOperator:
	default:
		return invalidRequest("the audit actor type is not known")
	}
	if a.ID == "" || len(a.ID) > MaxAuditActorID || !utf8.ValidString(a.ID) {
		return invalidRequest("the audit actor ID must be 1 to %d bytes of text", MaxAuditActorID)
	}
	return nil
}

// AuditRecord is one audit row the grant store appends in the revoking
// transaction. Metadata is safe to disclose to every reader of the audit trail:
// counts only, never a credential or a digest.
type AuditRecord struct {
	// ID is unique per record (NewAuditID).
	ID         string
	Action     string
	Actor      AuditActor
	Reason     string
	OccurredAt time.Time
	// Revoked is how many grants the revocation revoked.
	Revoked int
}

// NewAuditID returns a fresh audit record ID; a nil reader is crypto/rand.
func NewAuditID(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	b := make([]byte, 16)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", unavailable(err, "no randomness available")
	}
	return "aud_gitauth_" + hex.EncodeToString(b), nil
}

func validateRevocation(reason string, actor AuditActor) error {
	if strings.TrimSpace(reason) == "" || len(reason) > MaxAuditReason || !utf8.ValidString(reason) {
		return invalidRequest("a revocation reason of 1 to %d bytes is required", MaxAuditReason)
	}
	return actor.Validate()
}

// RevokeAll revokes every unrevoked grant of the tenant, of every kind (human,
// seed, preparation, attempt and integration), in its own transaction, and
// records one audit entry when it revoked any. It is idempotent: a repeat
// revokes nothing and records nothing, so a retried offboarding never doubles
// the trail. A revoked grant fails Verify at once. Offboarding, which revokes
// with its own state change, calls RevokeAllIn in that transaction instead,
// through the adapter's Bind. It returns how many grants it revoked.
func (s *Service) RevokeAll(ctx context.Context, tenant domain.TenantID, reason string, actor AuditActor) (int, error) {
	if tenant.Validate() != nil {
		return 0, invalidRequest("the tenant ID is malformed")
	}
	if err := validateRevocation(reason, actor); err != nil {
		return 0, err
	}
	auditID, err := NewAuditID(s.random)
	if err != nil {
		return 0, err
	}
	var n int
	err = s.store.Do(ctx, tenant, func(ctx context.Context, tx GrantTx) (err error) {
		n, err = RevokeAllIn(ctx, tx, reason, actor, s.now(), auditID)
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// RevokeAllIn revokes every unrevoked grant of the transaction's tenant inside
// a transaction the caller owns (joined with postgres.Bind or memory.Bind) and
// appends the audit record, with auditID, in the same transaction when it
// revoked any: the revocation and its audit commit or are discarded together.
// It returns how many grants it revoked; zero means nothing was left (and
// nothing was recorded).
func RevokeAllIn(ctx context.Context, tx GrantTx, reason string, actor AuditActor, now time.Time, auditID string) (int, error) {
	if err := validateRevocation(reason, actor); err != nil {
		return 0, err
	}
	if !validID(auditID) {
		return 0, invalidRequest("the audit ID is malformed")
	}
	now = now.UTC()
	n, err := tx.RevokeTenant(ctx, now)
	if err != nil || n == 0 {
		return n, err
	}
	rec := AuditRecord{ID: auditID, Action: AuditActionRevokeAll, Actor: actor, Reason: reason, OccurredAt: now, Revoked: n}
	if err := tx.RecordAudit(ctx, rec); err != nil {
		return 0, err
	}
	return n, nil
}

// Active counts the tenant's grants that are still usable now: unrevoked and
// unexpired (a consumed seed grant counts until it is revoked or expires). Zero
// after RevokeAll, with issuance fenced, is the offboarding proof.
func (s *Service) Active(ctx context.Context, tenant domain.TenantID) (int, error) {
	if tenant.Validate() != nil {
		return 0, invalidRequest("the tenant ID is malformed")
	}
	var n int
	err := s.store.Do(ctx, tenant, func(ctx context.Context, tx GrantTx) (err error) {
		n, err = tx.ActiveGrants(ctx, s.now().UTC())
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}
