package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
)

// AppendResult describes the committed (or already committed) event.
type AppendResult struct {
	Scope events.Scope
	// Sequence is the event's observation sequence within its scope; 0 only if
	// the observation has already been pruned and the event is a repeat.
	Sequence int64
	Digest   string
	// Duplicate is true when the same event ID with identical content already
	// existed; nothing new was written.
	Duplicate bool
}

// Append validates env and joins the caller's open tenant transaction tx (from
// storage.WithTenant) to record, atomically with the caller's state and audit:
//
//   - the immutable outbox row (publication intent),
//   - the retained observation record with its scope sequence,
//   - the tenant backlog accounting.
//
// It never begins, commits or rolls back tx and makes no broker call. Locks are
// taken in a fixed order: event_backlog row, then event_scopes row; all
// appenders of a tenant therefore serialize on the backlog row until their
// transaction ends, which is what makes the limits exact and the sequences
// gapless in commit order. Keep producer transactions short.
//
// A repeated event ID with identical content returns the existing result; with
// different content it fails with events.ErrConflict. Any retained record
// counts as knowing the ID: the observation, the outbox row, or (outliving both)
// a consumer's inbox entry, so an event is never re-originated while evidence of
// its processing exists. When no retained record
// knows the ID, an event that occurred before the tenant's durable
// deduplication boundary fails with events.ErrStaleAppend: it may be a retry of
// an event that was already consumed and pruned, whose effect must not repeat. A full backlog fails with
// events.ErrBacklogFull before anything is written, leaving tx usable (the
// producer should still roll back its business change). tx must be READ
// COMMITTED, the default.
func (s *Store) Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (AppendResult, error) {
	payload, err := eventsv1.Encode(env)
	if err != nil {
		return AppendResult{}, err
	}
	digest, err := eventsv1.Digest(env)
	if err != nil {
		return AppendResult{}, err
	}
	scope := events.ScopeOf(env)
	res := AppendResult{Scope: scope, Digest: digest}

	// The caller's transaction decides the tenant; an event for another tenant
	// is refused rather than written under the wrong authority.
	var ctxTenant *string
	var isolation string
	if err := tx.QueryRow(ctx, `SELECT kodama_tenant_id(), current_setting('transaction_isolation')`).Scan(&ctxTenant, &isolation); err != nil {
		return res, fmt.Errorf("events: append context: %w", err)
	}
	if ctxTenant == nil || *ctxTenant != string(env.TenantID) {
		return res, fmt.Errorf("%w: transaction tenant context differs from the event's tenant", events.ErrScopeMismatch)
	}
	if isolation != "read committed" {
		return res, fmt.Errorf("events: append requires a READ COMMITTED transaction, got %s", isolation)
	}

	tenant := string(env.TenantID)
	// An event from the future would hold its own deduplication evidence
	// (completion is stamped no earlier than occurredAt) for longer than the
	// retention policy intends: bound the producer clock's lead.
	if limit := s.now().Add(s.opts.Retention.MaxClockSkew); env.OccurredAt.After(limit) {
		return res, fmt.Errorf("%w: event %s occurred at %s, later than %s", events.ErrFutureEvent, env.ID,
			env.OccurredAt.UTC().Format(time.RFC3339Nano), limit.UTC().Format(time.RFC3339Nano))
	}
	// 0. Admission lock, shared: appends run concurrently with each other (they
	// serialize on the backlog row below) but not with inbox pruning or replay
	// admission, which take it exclusively. The durable deduplication boundary
	// read in step 2 therefore cannot advance between this append's verdict and
	// its commit; a prune that waits for us sees our unpublished outbox row.
	// Taken before any row lock so the lock order stays acyclic.
	if err := lockAdmissionShared(ctx, tx, env.TenantID); err != nil {
		return res, err
	}
	// 1. Backlog row: created on first use, then locked for the transaction.
	if _, err := tx.Exec(ctx, `INSERT INTO event_backlog (tenant_id) VALUES ($1) ON CONFLICT DO NOTHING`, tenant); err != nil {
		return res, fmt.Errorf("events: backlog row: %w", err)
	}
	var count int
	var bytes int64
	if err := tx.QueryRow(ctx, `SELECT pending_count, pending_bytes FROM event_backlog WHERE tenant_id = $1 FOR UPDATE`, tenant).Scan(&count, &bytes); err != nil {
		return res, fmt.Errorf("events: lock backlog: %w", err)
	}

	// 2. Idempotency, decided under the lock so concurrent identical appends
	// observe each other's commit.
	if existing, found, err := s.lookupExisting(ctx, tx, tenant, env.ID); err != nil {
		return res, err
	} else if found {
		if existing.digest != digest {
			return res, fmt.Errorf("%w: event %s", events.ErrConflict, env.ID)
		}
		res.Duplicate, res.Sequence = true, existing.seq
		return res, nil
	}
	// Nothing retained knows this ID. That proves "new" only for an event that
	// is not older than what retention may already have forgotten: a re-append
	// of an event that was published, consumed and pruned everywhere must not
	// be given a fresh origin (a new unpublished outbox row would make its
	// delivery admissible again and repeat the effect).
	if err := s.checkAppendHorizon(ctx, tx, env); err != nil {
		return res, err
	}

	// 3. Ownership: the project must belong to the tenant. Checked here (RLS
	// restricts the lookup to the tenant) so a bad reference does not abort tx.
	if env.ProjectID != "" {
		var one int
		err := tx.QueryRow(ctx, `SELECT 1 FROM projects WHERE tenant_id = $1 AND id = $2`, tenant, string(env.ProjectID)).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return res, fmt.Errorf("%w: project %s is not owned by tenant %s", events.ErrScopeMismatch, env.ProjectID, env.TenantID)
		} else if err != nil {
			return res, fmt.Errorf("events: check project: %w", err)
		}
	}

	// 4. Backpressure, exact because the backlog row is locked.
	size := int64(len(payload))
	if count+1 > s.opts.Backlog.MaxEvents || bytes+size > s.opts.Backlog.MaxBytes {
		return res, fmt.Errorf("%w: %d events, %d bytes pending", events.ErrBacklogFull, count, bytes)
	}
	if _, err := tx.Exec(ctx, `UPDATE event_backlog SET pending_count = pending_count + 1, pending_bytes = pending_bytes + $2 WHERE tenant_id = $1`, tenant, size); err != nil {
		return res, fmt.Errorf("events: account backlog: %w", err)
	}

	if _, err := s.EnsureSubjectToken(ctx, tx, env.TenantID); err != nil {
		return res, err
	}

	// 5. Sequence from the locked scope counter.
	if _, err := tx.Exec(ctx, `INSERT INTO event_scopes (tenant_id, scope_key, project_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		tenant, scope.Key(), nullable(string(env.ProjectID))); err != nil {
		return res, fmt.Errorf("events: scope row: %w", err)
	}
	var seq int64
	if err := tx.QueryRow(ctx, `UPDATE event_scopes SET last_seq = last_seq + 1 WHERE tenant_id = $1 AND scope_key = $2 RETURNING last_seq`,
		tenant, scope.Key()).Scan(&seq); err != nil {
		return res, fmt.Errorf("events: allocate sequence: %w", err)
	}

	if _, err := tx.Exec(ctx, `INSERT INTO event_observations
		(tenant_id, scope_key, seq, event_id, event_type, project_id, resource_type, resource_id, resource_version, occurred_at, payload, payload_digest)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		tenant, scope.Key(), seq, env.ID, env.Type, nullable(string(env.ProjectID)), env.Resource.Type, env.Resource.ID, env.Resource.Version, env.OccurredAt, payload, digest); err != nil {
		return res, fmt.Errorf("events: record observation: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO outbox (tenant_id, event_id, project_id, event_type, payload, payload_digest, created_at, next_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`,
		tenant, env.ID, nullable(string(env.ProjectID)), env.Type, payload, digest, s.now()); err != nil {
		return res, fmt.Errorf("events: record outbox: %w", err)
	}
	res.Sequence = seq
	return res, nil
}

type existingEvent struct {
	digest string
	seq    int64
}

func (s *Store) lookupExisting(ctx context.Context, tx pgx.Tx, tenant, eventID string) (existingEvent, bool, error) {
	var e existingEvent
	err := tx.QueryRow(ctx, `SELECT payload_digest, seq FROM event_observations WHERE tenant_id = $1 AND event_id = $2`, tenant, eventID).Scan(&e.digest, &e.seq)
	if err == nil {
		return e, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return e, false, fmt.Errorf("events: look up event: %w", err)
	}
	err = tx.QueryRow(ctx, `SELECT payload_digest FROM outbox WHERE tenant_id = $1 AND event_id = $2`, tenant, eventID).Scan(&e.digest)
	if err == nil {
		return e, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return e, false, fmt.Errorf("events: look up event: %w", err)
	}
	// Deduplication evidence outlives the outbox row and the observation (their
	// retention is independent and usually shorter), and is the last record that
	// knows the event was ever appended. While any consumer's inbox entry exists
	// the ID is taken: recreating an outbox row and observation would give the
	// event a fresh origin (created_at, recorded_at) that postdates the entry's
	// completion, and once the entry is pruned those fresh records would make a
	// delivery or repair of the event look like unprocessed work.
	err = tx.QueryRow(ctx, `SELECT payload_digest FROM event_inbox WHERE tenant_id = $1 AND event_id = $2 LIMIT 1`, tenant, eventID).Scan(&e.digest)
	if err == nil {
		return e, true, nil // seq stays 0: the observation was pruned
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return e, false, fmt.Errorf("events: look up event: %w", err)
	}
	return e, false, nil
}

// checkAppendHorizon refuses an event whose origin lies before the tenant's
// durable deduplication boundary (event_dedup_boundary.inbox_before, advanced in
// the transaction that prunes completed inbox entries). An event pruned from
// every table can only be recognized by its own content: a retry of Append
// carries the identical envelope, hence the identical occurredAt, and a
// completion time is stamped no earlier than the occurrence (see processInTx),
// so such a retry always lies before the boundary its pruning recorded, for any
// producer clock skew. Events that were never consumed
// need no such protection: only inbox entries are forgotten by this boundary.
//
// Only the durable value is used, not the current policy: until a prune has
// forgotten something, the retained identity evidence (or the unpruned inbox)
// still protects the event, and raising a retention setting cannot lower the
// boundary.
func (s *Store) checkAppendHorizon(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
	var before time.Time
	switch err := tx.QueryRow(ctx, `SELECT inbox_before FROM event_dedup_boundary WHERE tenant_id = $1`, string(env.TenantID)).Scan(&before); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("events: read deduplication boundary: %w", err)
	}
	if env.OccurredAt.Before(before) {
		return fmt.Errorf("%w: event %s occurred at %s, before the deduplication boundary %s",
			events.ErrStaleAppend, env.ID, env.OccurredAt.UTC().Format(time.RFC3339), before.UTC().Format(time.RFC3339))
	}
	return nil
}
