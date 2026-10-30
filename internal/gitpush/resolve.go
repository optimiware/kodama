package gitpush

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
	"unicode"
	"unicode/utf8"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Audit actions of the operator actions (T-138).
const (
	AuditOperatorResolved = "git.push.operator_resolved"
	AuditHeadReset        = "git.journal.head_reset"
)

// MaxResolutionReason bounds the explanation an operator records.
const MaxResolutionReason = 1024

// ResolveCommand is the operator's decision on one unknown entry of a push
// record, taken after inspecting the repository.
type ResolveCommand struct {
	// Principal is the administrator; the use case asks Dependencies.Admin again
	// with current state.
	Principal    string
	Scope        domain.Scope
	RepositoryID string
	PushID       string
	// Ref names the unknown entry; Resolution is applied or refused.
	Ref        string
	Resolution gitv1.EntryState
	// Reason is the operator's explanation (required, at most MaxResolutionReason
	// bytes of text): what was inspected and found.
	Reason string
	// Expected is the record's version (the If-Match of the route), compared with
	// the locked record: a record that changed since the operator read it is
	// version_conflict.
	Expected domain.Version
}

// Resolved is the record after the resolution with its new version (the ETag).
type Resolved struct {
	Record  gitv1.PushRecord
	Version domain.Version
}

func validReason(s string) error {
	switch {
	case s == "" || len(s) > MaxResolutionReason || !utf8.ValidString(s):
		return refuse(domain.CodeInvalidRequest, "the reason must be 1 to %d bytes of text", MaxResolutionReason)
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return refuse(domain.CodeInvalidRequest, "the reason must not contain control characters")
		}
	}
	return nil
}

func (s *Service) authorizeResolve(ctx context.Context, principal string, scope domain.Scope, repositoryID string) error {
	if s.d.Admin == nil {
		return unavailable(ErrNotConfigured, "administrative authority")
	}
	err := s.d.Admin.AuthorizeResolve(ctx, principal, scope, repositoryID)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound):
		return notFound()
	case errors.Is(err, ErrDenied):
		return refuse(domain.CodeForbidden, "only a project administrator may resolve an unknown push entry")
	}
	return unavailable(err, "administrative authority")
}

// ResolveUnknown is the audited operator action on an entry the reconciler left
// unknown: after inspecting the repository, an administrator records that the
// update was applied or refused, with a reason. Unknown is otherwise terminal
// (the database refuses any other move out of it), so this is what unblocks the
// writer of the ref (UnknownBlocksWriter) and new seed grants (UnsettledPush).
//
//   - The repository's and record's scope is compared first; a foreign or
//     unknown one is not_found, and a person who is not an administrator is
//     forbidden. The principal is the actor of the audit row, never a body field.
//   - The record is locked and its version must equal Expected (version_conflict
//     otherwise): the operator decides against the record they read.
//   - The entry must still be unknown (push_settled otherwise): a second
//     resolution of the same entry changes nothing.
//   - The new state, the resolution row (who, why, which value of the ref the
//     control plane observed), an audit row and, when the record's status becomes
//     applied, the one git.refs_updated event (deterministic identity per
//     record, so it is emitted exactly once whichever path settles the record)
//     commit in one transaction. Resolving one of several unknown entries leaves
//     the record unknown and emits nothing.
//   - The decision never depends on the worker: the ref's current value is read
//     before the transaction as supporting evidence and recorded when readable.
func (s *Service) ResolveUnknown(ctx context.Context, cmd ResolveCommand) (Resolved, error) {
	if err := cmd.Scope.Validate(); err != nil {
		return Resolved{}, notFound()
	}
	if cmd.Resolution != gitv1.EntryApplied && cmd.Resolution != gitv1.EntryRefused {
		return Resolved{}, refuse(domain.CodeInvalidRequest, "the resolution is applied or refused")
	}
	if err := gitv1.ValidateRefName(cmd.Ref); err != nil {
		return Resolved{}, refuse(domain.CodeInvalidRequest, "%v", err)
	}
	if err := validReason(cmd.Reason); err != nil {
		return Resolved{}, err
	}
	if err := cmd.Expected.Validate(); err != nil {
		return Resolved{}, refuse(domain.CodeVersionRequired, "the record's version is required")
	}
	if cmd.Principal == "" {
		return Resolved{}, refuse(domain.CodeForbidden, "an authenticated administrator is required")
	}
	if err := s.authorizeResolve(ctx, cmd.Principal, cmd.Scope, cmd.RepositoryID); err != nil {
		return Resolved{}, err
	}
	// Supporting evidence, read with no lock held; never a condition.
	observed := ""
	if s.d.Worker != nil {
		if resp, err := s.d.Worker.ReadRefValues(ctx, cmd.Scope.TenantID, gitv1.ReadRefValuesRequest{RepositoryID: cmd.RepositoryID, Refs: []string{cmd.Ref}}); err == nil {
			for _, v := range resp.Values {
				if v.Ref == cmd.Ref && v.Commit != nil {
					observed = *v.Commit
				}
			}
		}
	}

	now := s.now()
	var rec *Record
	err := s.d.Store.Do(ctx, cmd.Scope.TenantID, func(ctx context.Context, tx Tx) error {
		cur, err := tx.ByID(ctx, cmd.PushID, true)
		if err != nil {
			return err
		}
		if cur.Scope != cmd.Scope || cur.RepositoryID != cmd.RepositoryID {
			return ErrNotFound
		}
		if domain.Version(cur.Version) != cmd.Expected {
			return refuse(domain.CodeVersionConflict, "the push record changed; reread it and retry with its current version")
		}
		before := cur.Status()
		if err := cur.OperatorResolve(cmd.Ref, cmd.Resolution, now); err != nil {
			return err
		}
		actor := AuditActor{Type: "user", ID: cmd.Principal}
		fx := settlementEffects(before, cur, actor, nil)
		md := cur.metadata()
		md["ref"], md["resolution"], md["observed"] = cmd.Ref, string(cmd.Resolution), observed
		fx.Audit = append(fx.Audit, Audit{Action: AuditOperatorResolved, Actor: actor, Reason: cmd.Reason, Metadata: md})
		fx.Resolutions = []Resolution{{Ref: cmd.Ref, State: cmd.Resolution, By: cmd.Principal, Reason: cmd.Reason, Observed: observed, At: now}}
		if err := tx.Save(ctx, cur, fx); err != nil {
			return err
		}
		rec = cur
		return nil
	})
	if err != nil {
		return Resolved{}, mapStoreErr(err)
	}
	_ = s.flushEmptyReport(ctx, cmd.Scope.TenantID, rec) // best effort: Reconcile repeats it
	dto, err := rec.DTO()
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{Record: dto, Version: domain.Version(rec.Version)}, nil
}

// GetVersioned is Get with the record's version, the ETag of the route that
// resolves an unknown entry.
func (s *Service) GetVersioned(ctx context.Context, principal string, scope domain.Scope, repositoryID, pushID string) (Resolved, error) {
	if err := s.authorizeRead(ctx, principal, scope, repositoryID); err != nil {
		return Resolved{}, err
	}
	var rec *Record
	err := s.d.Store.View(ctx, scope.TenantID, func(ctx context.Context, tx Tx) (err error) {
		rec, err = tx.ByID(ctx, pushID, false)
		return err
	})
	switch {
	case errors.Is(err, ErrNotFound):
		return Resolved{}, notFound()
	case err != nil:
		return Resolved{}, depErr(err, "push record")
	case rec.Scope != scope || rec.RepositoryID != repositoryID:
		return Resolved{}, notFound()
	}
	dto, err := rec.DTO()
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{Record: dto, Version: domain.Version(rec.Version)}, nil
}

// ---- restore ----

// Settlement is what the restore reconciliation needs to know about a
// repository's push records.
type Settlement struct {
	// InFlight entries are authorized or committing: Reconcile has not settled them.
	InFlight int
	// Unknown entries are visible, block their writers and wait for an operator.
	Unknown int
	// KnownHead is the highest journal head the control plane has seen.
	KnownHead int64
}

// Settlement reports the repository's unsettled entries and the known journal
// head (the restore reconciliation of T-138 reads it after Reconcile).
func (s *Service) Settlement(ctx context.Context, tenant domain.TenantID, repositoryID string) (Settlement, error) {
	if err := tenant.Validate(); err != nil {
		return Settlement{}, notFound()
	}
	var out Settlement
	err := s.d.Store.View(ctx, tenant, func(ctx context.Context, tx Tx) error {
		c, err := tx.EntryCounts(ctx, repositoryID)
		if err != nil {
			return err
		}
		h, err := tx.KnownHead(ctx, repositoryID)
		if err != nil {
			return err
		}
		out = Settlement{InFlight: c.InFlight, Unknown: c.Unknown, KnownHead: h}
		return nil
	})
	if err != nil {
		return Settlement{}, depErr(err, "push records")
	}
	return out, nil
}

// AcknowledgeRestore accepts a restored volume's older journal as the new
// baseline: it lowers the repository's known journal head to head so later
// receives settle against the restored journal instead of being read as a
// regression for good. It is the restore procedure's step after Reconcile (T-138)
// and is refused (push_settled) while any entry of the repository is in flight,
// because the regression latch must first have turned every committing entry
// into unknown. A head not below the known one changes nothing. The reset, its
// reason and the head it replaces are recorded (git_journal_resets and an audit
// row) in the transaction of the decrease; it reports whether the head changed.
func (s *Service) AcknowledgeRestore(ctx context.Context, scope domain.Scope, repositoryID string, head int64, by, reason string) (bool, error) {
	if err := scope.Validate(); err != nil {
		return false, notFound()
	}
	if head < 0 || head > gitv1.MaxSafeInteger {
		return false, refuse(domain.CodeInvalidRequest, "the journal head is out of range")
	}
	if by == "" {
		return false, refuse(domain.CodeForbidden, "the actor of a restore acknowledgement is required")
	}
	if err := validReason(reason); err != nil {
		return false, err
	}
	now := s.now()
	changed := false
	err := s.d.Store.Do(ctx, scope.TenantID, func(ctx context.Context, tx Tx) error {
		known, err := tx.KnownHead(ctx, repositoryID)
		if err != nil {
			return err
		}
		if head >= known {
			return nil
		}
		c, err := tx.EntryCounts(ctx, repositoryID)
		if err != nil {
			return err
		}
		if c.InFlight > 0 {
			return refuse(gitv1.CodePushSettled, "entries of the repository are still in flight; reconcile first")
		}
		if err := tx.ResetHead(ctx, scope, repositoryID, head, HeadReset{By: by, Reason: reason, At: now}); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, mapStoreErr(err)
	}
	return changed, nil
}

// HeadResetAuditID is the audit row identity of a journal head reset: it is
// deterministic per (repository, replaced head, new head, time), so a retried
// transaction never writes a second row for one committed change. The adapters
// use it.
func HeadResetAuditID(tenant domain.TenantID, repositoryID string, from, to int64, at time.Time) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "gitpush/head-reset/%s/%s/%d/%d/%d", tenant, repositoryID, from, to, at.UnixMicro()))
	return "aud_hrst_" + hex.EncodeToString(sum[:20])
}
