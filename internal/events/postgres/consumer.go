package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
)

// Quarantine reason codes stored with a quarantined delivery.
const (
	ReasonUnsupportedVersion = "unsupported_version"
	ReasonUnsupportedType    = "unsupported_type"
	ReasonMalformed          = "malformed"
	ReasonTooLarge           = "too_large"
	ReasonScopeMismatch      = "scope_mismatch"
	ReasonPermanentFailure   = "permanent_failure"
	ReasonRetriesExhausted   = "retries_exhausted"
	// ReasonBeyondHorizon marks a delivery of an event that has no inbox entry
	// and is old enough that its deduplication evidence may have been pruned.
	ReasonBeyondHorizon = "beyond_horizon"
	// ReasonConflictingContent marks a delivery whose event ID has an inbox
	// entry created for different content. Event content is immutable, so the
	// delivery is kept for an operator and the original entry is not touched.
	ReasonConflictingContent = "conflicting_content"
)

// maxQuarantinePayload is the most raw payload retained with a quarantine
// record; longer messages are stored truncated and flagged.
const maxQuarantinePayload = 65536

// replayContext marks processing driven by an audited replay request. It
// carries the runner's claim token and the facts the request was admitted on,
// so every event's transaction can revalidate them atomically.
type replayContext struct {
	grant events.ReplayGrant
	id    string
	// token is the runner's fencing token for the request.
	token int64
	// seq is the observation sequence being replayed and recordedAt its
	// recording time.
	seq        int64
	recordedAt time.Time
	// evidenceFloor is the deduplication horizon fixed when the request was
	// admitted, and effect the effect class it was classified under.
	evidenceFloor time.Time
	effect        events.EffectClass
	lease         time.Duration
}

// Process turns one received delivery into a durable local outcome under the
// delivery's trusted tenant. It is the events.Processor port.
//
// The tenant comes from the subscription, never from the payload. The payload
// is decoded against the event contract; an unsupported version or type,
// malformed or oversized input, or an envelope whose scope differs from the
// trusted scope is quarantined durably, and only then reported as quarantined
// so the transport may acknowledge. Valid events run through the inbox: the
// (tenant, consumer, event) entry is locked, the handler and the inbox result
// commit in one transaction, and failures are recorded durably with their class.
func (s *Store) Process(ctx context.Context, d events.Delivery) (events.Outcome, error) {
	binding, ok := s.consumers[d.Consumer]
	if !ok {
		return events.Outcome{}, fmt.Errorf("%w: consumer %q is not registered", events.ErrInvalidConfiguration, d.Consumer)
	}
	if err := d.Tenant.Validate(); err != nil {
		return events.Outcome{}, err
	}
	env, derr := eventsv1.Decode(d.Payload)
	if derr != nil {
		return s.quarantineRaw(ctx, d, reasonFor(derr), derr.Error())
	}
	if env.TenantID != d.Tenant {
		return s.quarantineRaw(ctx, d, ReasonScopeMismatch, "envelope tenant differs from the trusted subscription scope")
	}
	if !binding.Spec.Accepts(env.Type) {
		return events.Outcome{Kind: events.OutcomeCompleted, Reason: "filtered"}, nil
	}
	res, err := s.processEnvelope(ctx, d.Tenant, binding, env, d.Payload, d.Identity, nil)
	if errors.Is(err, errProjectScope) {
		// A syntactically valid envelope naming a project the trusted tenant
		// does not own is a permanent scope violation: quarantine it (the
		// project is display-only there, never a foreign key) instead of
		// failing every redelivery.
		return s.quarantineRaw(ctx, d, ReasonScopeMismatch, "envelope project is not owned by the trusted subscription tenant")
	}
	if errors.Is(err, errBeyondHorizon) {
		// A copy that outlived its own deduplication evidence (a message held in
		// a client buffer or in flight past retention): running it could repeat
		// an effect that was already committed and forgotten. Keep it for an
		// operator instead of admitting it as new or redelivering it forever.
		return s.quarantineRaw(ctx, d, ReasonBeyondHorizon, "event has no inbox entry and is older than the deduplication horizon")
	}
	if errors.Is(err, errContentConflict) {
		// Not a retry candidate (the content will never match) and not the
		// original event: quarantine this copy under its own key, so the
		// original entry, its quarantine record and its retry schedule stay as
		// they were.
		return s.quarantineDelivery(ctx, d, ReasonConflictingContent,
			"event content differs from the content already recorded for this event ID", true)
	}
	return res.outcome, err
}

func reasonFor(err error) string {
	switch {
	case errors.Is(err, eventsv1.ErrUnsupportedVersion):
		return ReasonUnsupportedVersion
	case errors.Is(err, eventsv1.ErrUnsupportedType):
		return ReasonUnsupportedType
	case errors.Is(err, eventsv1.ErrTooLarge):
		return ReasonTooLarge
	default:
		return ReasonMalformed
	}
}

type processResult struct {
	outcome events.Outcome
	// skipped is true when a completed duplicate caused no effect.
	skipped bool
}

// processEnvelope runs one decoded event through the inbox in one tenant
// transaction. replay is non-nil for audited replay, which first reopens a
// quarantined entry; a completed entry is never reopened.
func (s *Store) processEnvelope(ctx context.Context, tenant domain.TenantID, b Binding, env eventsv1.Envelope, raw []byte, id events.DeliveryIdentity, replay *replayContext) (processResult, error) {
	var res processResult
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		res = processResult{}
		now := s.now()
		if replay != nil {
			// Lock order of a replayed event, before any row lock: the tenant
			// admission lock (shared), then the replay request row, inbox rows,
			// and whatever the handler locks, including the backlog and scope
			// rows of an Append it performs. Fencing and replay admission take
			// the admission lock exclusively and then the request rows. A
			// handler's Append takes the admission lock shared as well: were it
			// first taken there, after guardReplayStep locked the request row,
			// the append could queue behind a fence that waits for that very
			// row, a deadlock. Holding the shared lock from the start makes the
			// append's own request immediate (a lock this session already holds
			// is granted even when an exclusive request is queued) and the
			// fence wait for the event instead. Live deliveries hold no request
			// row and so need no such ordering; they must not block pruning.
			if err := lockAdmissionShared(ctx, tx, tenant); err != nil {
				return err
			}
			// First statement: validates and renews the runner's claim, which
			// also serializes this effect with takeover and fencing.
			if err := s.guardReplayStep(ctx, tx, tenant, b, replay, now); err != nil {
				return err
			}
		}
		var err error
		if res, err = s.processInTx(ctx, tx, tenant, b, env, raw, id, replay, now); err != nil {
			return err
		}
		if replay != nil {
			return recordReplayStep(ctx, tx, tenant, replay, res)
		}
		return nil
	})
	if err != nil {
		return processResult{}, err
	}
	return res, nil
}

// guardReplayStep runs inside the transaction of one replayed event, before
// anything else. It proves the runner still holds the request (its fencing
// token is current and the request is running), renews the lease, and checks
// the conditions the request was admitted under: no restore fence, and a
// consumer classification that is still safe and unchanged. Updating the
// request row takes its row lock, so a takeover or a fence committing first is
// observed here, and one arriving later waits until this event commits. A
// failure rolls the transaction back before any effect.
func (s *Store) guardReplayStep(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, b Binding, r *replayContext, now time.Time) error {
	tag, err := tx.Exec(ctx, `UPDATE event_replay_requests SET lease_expires_at = $4, updated_at = $5
		WHERE tenant_id = $1 AND id = $2 AND claim_token = $3 AND state = 'running'`, string(tenant), r.id, r.token, now.Add(r.lease), now)
	if err != nil {
		return fmt.Errorf("events: renew replay claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: replay %s is no longer held by this runner", events.ErrStaleClaim, r.id)
	}
	var fenced bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM event_recovery_fences WHERE tenant_id = $1 AND consumer = $2 AND cleared_at IS NULL)`,
		string(tenant), b.Spec.Name).Scan(&fenced); err != nil {
		return fmt.Errorf("events: read fence: %w", err)
	}
	c := events.ReplayClassification{Effect: b.Spec.Effect, Fenced: fenced}
	if err := c.Check(b.Spec, r.grant.Request().AllowBeyondHorizon); err != nil {
		return err
	}
	if b.Spec.Effect != r.effect {
		return fmt.Errorf("%w: consumer %s was classified %s when the replay was admitted and is %s now", events.ErrReplayUnsafe, b.Spec.Name, r.effect, b.Spec.Effect)
	}
	return nil
}

// recordReplayStep records, in the event's own transaction, the outcome counts
// and progress of the replay request.
func recordReplayStep(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, r *replayContext, res processResult) error {
	var processed, skipped, failed int
	switch {
	case res.skipped:
		skipped = 1
	case res.outcome.Kind == events.OutcomeCompleted:
		processed = 1
	default:
		failed = 1
	}
	tag, err := tx.Exec(ctx, `UPDATE event_replay_requests SET next_seq = GREATEST(next_seq, $4), processed = processed + $5, skipped = skipped + $6, failed = failed + $7
		WHERE tenant_id = $1 AND id = $2 AND claim_token = $3 AND state = 'running'`, string(tenant), r.id, r.token, r.seq+1, processed, skipped, failed)
	if err != nil {
		return fmt.Errorf("events: record replay step: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: replay %s is no longer held by this runner", events.ErrStaleClaim, r.id)
	}
	return nil
}

// errProjectScope reports an envelope whose project does not exist under the
// trusted tenant (absent, or owned by another tenant: the two are
// indistinguishable under row level security and must stay so).
var errProjectScope = errors.New("events: envelope project is not owned by the trusted tenant")

func (s *Store) processInTx(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, b Binding, env eventsv1.Envelope, raw []byte, id events.DeliveryIdentity, replay *replayContext, now time.Time) (processResult, error) {
	var res processResult
	consumer := b.Spec.Name
	digest, err := eventsv1.Digest(env)
	if err != nil {
		return res, fmt.Errorf("events: digest delivered event: %w", err)
	}
	// A delivery that contradicts an existing entry is a content conflict, whatever
	// else is wrong with it (for example a changed project): decided before the
	// scope checks so it cannot be mistaken for, or recorded over, the original.
	// This read takes no lock; the locked comparison after the insert is the
	// authoritative one.
	var existing string
	switch err := tx.QueryRow(ctx, `SELECT payload_digest FROM event_inbox WHERE tenant_id = $1 AND consumer = $2 AND event_id = $3`,
		string(tenant), consumer, env.ID).Scan(&existing); {
	case err == nil:
		if existing != digest {
			return res, errContentConflict
		}
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return res, fmt.Errorf("events: read inbox digest: %w", err)
	}
	if env.ProjectID != "" {
		// Ownership is checked under the trusted tenant before anything refers
		// to the project, so the composite foreign key can never be the thing
		// that rejects a delivery (which would be retried forever).
		var one int
		switch err := tx.QueryRow(ctx, `SELECT 1 FROM projects WHERE tenant_id = $1 AND id = $2`, string(tenant), string(env.ProjectID)).Scan(&one); {
		case errors.Is(err, pgx.ErrNoRows):
			return res, errProjectScope
		case err != nil:
			return res, fmt.Errorf("events: check project ownership: %w", err)
		}
	}
	tag, err := tx.Exec(ctx, `INSERT INTO event_inbox (tenant_id, consumer, event_id, project_id, event_type, payload, payload_digest)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING`,
		string(tenant), consumer, env.ID, nullable(string(env.ProjectID)), env.Type, raw, digest)
	if err != nil {
		return res, fmt.Errorf("events: inbox claim: %w", err)
	}
	// Every later decision and timestamp (evidence horizon, retry schedule,
	// completion time) uses a reading taken after the insert, not the one
	// captured when the transaction began, which a stalled worker may hold for
	// arbitrarily long.
	now = s.now()
	if replay == nil && tag.RowsAffected() == 1 {
		// The event has no inbox entry. That proves "never processed" only if
		// the entry cannot have been pruned: decided here, in the transaction
		// that would create the entry and run the effect, from durable facts,
		// never from when this delivery was fetched or selected. The admission
		// time is sampled only now: the uncommitted entry holds the event against
		// competing processing, so no prune can forget it between this reading
		// and the commit, and a reading taken before the insert (the transaction
		// may have been paused) could understate how much was forgotten since.
		// A refusal rolls the insert back before any effect.
		if err := s.checkEvidenceHorizon(ctx, tx, tenant, env.ID, now); err != nil {
			return res, err
		}
	}
	entry, stored, err := loadInbox(ctx, tx, tenant, consumer, env.ID)
	if err != nil {
		return res, err
	}
	// Event content is immutable across deliveries: the entry (locked above)
	// carries the digest of the content it was created for, and every decision
	// below (duplicate, backoff, retry, replay, handler) is about that content.
	// A different envelope under the same ID is refused before any of them, with
	// the original entry untouched (the transaction rolls back).
	if stored != digest {
		return res, errContentConflict
	}
	if replay != nil && tag.RowsAffected() == 1 {
		// No deduplication evidence exists for this event. That is proof of
		// "never processed" only inside the deduplication horizon fixed at the
		// request's admission (evidence of active requests is never pruned);
		// older events need the explicit, idempotent-consumer exception.
		if replay.recordedAt.Before(replay.evidenceFloor) {
			c := events.ReplayClassification{Effect: b.Spec.Effect, BeyondHorizon: 1}
			if err := c.Check(b.Spec, replay.grant.Request().AllowBeyondHorizon); err != nil {
				return res, err
			}
		}
	}
	if entry.State == events.InboxQuarantined && replay != nil {
		if err := entry.Reopen(replay.grant); err != nil {
			return res, err
		}
		if err := saveInbox(ctx, tx, entry); err != nil {
			return res, err
		}
		if err := recordInboxHistory(ctx, tx, entry, "replay_reopened", "", "", replay.id); err != nil {
			return res, err
		}
		// The quarantine record stays open until the effect completes, which a
		// transient failure may defer to a scheduled retry, repair or ordinary
		// delivery that carries no replay. Persist the authorizing replay with
		// the record, in the same transaction as the reopen.
		if _, err := tx.Exec(ctx, `UPDATE event_quarantine SET replay_id = $4, replay_by = $5
			WHERE tenant_id = $1 AND consumer = $2 AND delivery_key = $3 AND state = 'quarantined'`,
			string(tenant), consumer, env.ID, replay.id, replay.grant.Request().Operator.ID); err != nil {
			return res, fmt.Errorf("events: record quarantine replay: %w", err)
		}
	}
	if replay == nil && !entry.Due(now) {
		// The persisted schedule is authoritative: a duplicate delivery during
		// backoff neither runs the handler nor consumes an attempt.
		res.outcome = events.Outcome{Kind: events.OutcomeRetry, RetryAfter: entry.RetryIn(now), Reason: "retry not yet due"}
		return res, nil
	}
	switch err := entry.Begin(); {
	case errors.Is(err, events.ErrAlreadyCompleted):
		res.outcome, res.skipped = events.Outcome{Kind: events.OutcomeDuplicate}, true
		return res, nil
	case errors.Is(err, events.ErrQuarantined):
		res.outcome = events.Outcome{Kind: events.OutcomeQuarantined, Reason: "already quarantined"}
		return res, nil
	case err != nil:
		return res, err
	}

	sp, err := tx.Begin(ctx) // savepoint: the handler's effects roll back on failure
	if err != nil {
		return res, fmt.Errorf("events: savepoint: %w", err)
	}
	herr := runHandler(ctx, sp, b.Handle, env)
	if herr == nil {
		if err := sp.Commit(ctx); err != nil {
			return res, fmt.Errorf("events: release savepoint: %w", err)
		}
		// The completion time starts the retention clock of this evidence, and
		// pruning forgets an entry only once the completion lies before the
		// boundary. Every later decision about a missing entry compares a
		// retained origin of the event with that boundary (checkEvidenceHorizon,
		// Repair, replay classification, Append's horizon), so the stored
		// completion must never precede any origin those decisions can read:
		// the event's own occurrence time, the outbox row's creation (the
		// appender's clock) and the observation's recording time (the database
		// clock). These clocks may disagree with this process's by the skew the
		// deployment tolerates, in either direction, so the completion is the
		// latest of them and the current reading. A pruned entry therefore
		// implies every origin that survives it lies before the boundary.
		completedAt := now
		if env.OccurredAt.After(completedAt) {
			completedAt = env.OccurredAt.UTC()
		}
		origin, err := retainedOrigin(ctx, tx, tenant, env.ID)
		if err != nil {
			return res, err
		}
		if origin.After(completedAt) {
			completedAt = origin
		}
		if err := entry.Complete(completedAt); err != nil {
			return res, err
		}
		if err := saveInbox(ctx, tx, entry); err != nil {
			return res, err
		}
		if err := recordInboxHistory(ctx, tx, entry, "completed", "", "", replayID(replay)); err != nil {
			return res, err
		}
		// Completing the effect resolves the event's open quarantine record in
		// the same transaction, whichever path completed it: the replay itself,
		// or a later scheduled retry, repair or ordinary delivery of an event a
		// replay reopened. A completed duplicate returns earlier, so without
		// this the record could never be resolved. The operator is the replay
		// being run, or else the one recorded when the replay reopened the entry.
		var operator *string
		if replay != nil {
			op := replay.grant.Request().Operator.ID
			operator = &op
		}
		if _, err := tx.Exec(ctx, `UPDATE event_quarantine SET state = 'resolved', resolved_at = $4, resolution = 'replayed', resolved_by = COALESCE($5, replay_by)
			WHERE tenant_id = $1 AND consumer = $2 AND delivery_key = $3 AND state = 'quarantined' AND COALESCE($5, replay_by) IS NOT NULL`,
			string(tenant), consumer, env.ID, now, operator); err != nil {
			return res, fmt.Errorf("events: resolve quarantine: %w", err)
		}
		res.outcome = events.Outcome{Kind: events.OutcomeCompleted}
		return res, nil
	}
	if err := sp.Rollback(ctx); err != nil {
		return res, fmt.Errorf("events: roll back handler effects: %w", err)
	}
	class := events.ClassOrTransient(herr)
	quarantined, err := entry.Fail(class, herr.Error(), now, s.opts.Retry)
	if err != nil {
		return res, err
	}
	if err := saveInbox(ctx, tx, entry); err != nil {
		return res, err
	}
	outcome := "retry"
	if quarantined {
		outcome = "quarantined"
	}
	if err := recordInboxHistory(ctx, tx, entry, outcome, string(class), entry.LastError, replayID(replay)); err != nil {
		return res, err
	}
	if !quarantined {
		res.outcome = events.Outcome{Kind: events.OutcomeRetry, RetryAfter: s.opts.Retry.Delay(entry.Attempts), Reason: entry.LastError}
		return res, nil
	}
	reason := ReasonPermanentFailure
	if class == events.ClassTransient {
		reason = ReasonRetriesExhausted
	}
	qid, err := insertQuarantine(ctx, tx, quarantineRecord{
		tenant: tenant, consumer: consumer, deliveryKey: env.ID, eventID: env.ID, eventType: env.Type,
		reason: reason, diagnostic: entry.LastError, identity: id, payload: raw, reopen: true,
	})
	if err != nil {
		return res, err
	}
	res.outcome = events.Outcome{Kind: events.OutcomeQuarantined, QuarantineID: qid, Reason: reason}
	return res, nil
}

// errContentConflict reports a delivery whose content differs from the content
// the consumer's inbox entry for the same event ID was created for.
var errContentConflict = errors.New("events: event content differs from the content first delivered under the same ID")

// errBeyondHorizon reports that deduplication evidence for an event may have
// been pruned, so a missing inbox entry cannot be taken as "never processed".
var errBeyondHorizon = errors.New("events: deduplication evidence of the event may have been pruned")

// checkEvidenceHorizon decides whether a missing inbox entry can be trusted. It
// mirrors the pruning rule for completed entries, which are forgotten only when
// (1) completed_at is older than the inbox cutoff, (2) the outbox row is not
// unpublished, and (3) the last broker acceptance (published_at, or an outbox
// row pruned only after that) is older than the broker cutoff. The cutoffs are
// the later of the current policy's and the durable boundary pruning persisted
// (event_dedup_boundary): a larger retention setting, or a restart with other
// options, cannot restore evidence that an earlier prune deleted. Every fact it
// reads is monotone, so once the entry could have been pruned the answer stays
// "refuse":
//
//   - an unpublished outbox row, or one published within the broker horizon,
//     forbids pruning: absence of an entry means the event was never processed;
//   - otherwise the entry can only have been pruned if the event is older than
//     the inbox cutoff (completion is never earlier than origin), where origin is
//     the outbox row's creation time or, once that row is gone, the retained
//     observation's recording time. An event whose origin is unknown is refused.
//
// This makes any late copy harmless wherever it sat: a message buffered in a
// publisher's reconnect buffer, in flight on the network, held by a stalled
// consumer, or selected by a stalled repair worker.
func (s *Store) checkEvidenceHorizon(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, eventID string, now time.Time) error {
	// The boundary is read after the caller's insert (see processInTx): a prune
	// that forgot this event committed its boundary before the insert could
	// proceed, so it is visible here.
	bound, err := s.loadDedupBoundary(ctx, tx, tenant, now)
	if err != nil {
		return err
	}
	var (
		state     string
		created   time.Time
		published *time.Time
		origin    time.Time
		known     bool
	)
	switch err := tx.QueryRow(ctx, `SELECT state, created_at, published_at FROM outbox WHERE tenant_id = $1 AND event_id = $2`,
		string(tenant), eventID).Scan(&state, &created, &published); {
	case err == nil:
		if state != "published" || published == nil || !published.Before(bound.brokerBefore) {
			return nil
		}
		origin, known = created, true
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return fmt.Errorf("events: read outbox horizon: %w", err)
	}
	if !known {
		switch err := tx.QueryRow(ctx, `SELECT recorded_at FROM event_observations WHERE tenant_id = $1 AND event_id = $2`,
			string(tenant), eventID).Scan(&origin); {
		case err == nil:
			known = true
		case errors.Is(err, pgx.ErrNoRows):
		default:
			return fmt.Errorf("events: read observation horizon: %w", err)
		}
	}
	if known && !origin.Before(bound.inboxBefore) {
		return nil
	}
	return errBeyondHorizon
}

// retainedOrigin returns the latest origin timestamp still retained for the
// event: the outbox row's creation time and the observation's recording time.
// The zero time means neither exists (the event is unknown, or both were
// pruned, in which case no origin check can read them either).
func retainedOrigin(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, eventID string) (time.Time, error) {
	var origin *time.Time
	err := tx.QueryRow(ctx, `SELECT GREATEST(
			(SELECT max(created_at) FROM outbox WHERE tenant_id = $1 AND event_id = $2),
			(SELECT max(recorded_at) FROM event_observations WHERE tenant_id = $1 AND event_id = $2))`,
		string(tenant), eventID).Scan(&origin)
	if err != nil {
		return time.Time{}, fmt.Errorf("events: read event origin: %w", err)
	}
	if origin == nil {
		return time.Time{}, nil
	}
	return origin.UTC(), nil
}

func replayID(r *replayContext) string {
	if r == nil {
		return ""
	}
	return r.id
}

// runHandler calls the local effect, converting a panic into a transient
// failure so one bad event cannot take a worker down.
func runHandler(ctx context.Context, tx pgx.Tx, h TxHandler, env eventsv1.Envelope) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = events.Transient(fmt.Errorf("handler panic: %v", r))
		}
	}()
	return h(ctx, tx, env)
}

const inboxColumns = `state, attempts, next_attempt_at, completed_at, last_failure_class, last_error, replays`

func loadInbox(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, consumer, eventID string) (events.InboxEntry, string, error) {
	e := events.NewInboxEntry(string(tenant), consumer, eventID)
	var state, digest string
	var next, completed *time.Time
	var class, lastErr *string
	// FOR UPDATE serializes concurrent deliveries of the same (consumer, event).
	err := tx.QueryRow(ctx, `SELECT `+inboxColumns+`, payload_digest FROM event_inbox WHERE tenant_id = $1 AND consumer = $2 AND event_id = $3 FOR UPDATE`,
		string(tenant), consumer, eventID).Scan(&state, &e.Attempts, &next, &completed, &class, &lastErr, &e.Replays, &digest)
	if err != nil {
		return e, "", fmt.Errorf("events: load inbox entry: %w", err)
	}
	e.State = events.InboxState(state)
	if next != nil {
		e.NextAttemptAt = *next
	}
	if completed != nil {
		e.CompletedAt = *completed
	}
	if class != nil {
		e.LastClass = events.FailureClass(*class)
	}
	if lastErr != nil {
		e.LastError = *lastErr
	}
	return e, digest, nil
}

func saveInbox(ctx context.Context, tx pgx.Tx, e events.InboxEntry) error {
	var next, completed *time.Time
	if !e.NextAttemptAt.IsZero() {
		next = &e.NextAttemptAt
	}
	if !e.CompletedAt.IsZero() {
		completed = &e.CompletedAt
	}
	_, err := tx.Exec(ctx, `UPDATE event_inbox SET state = $4, attempts = $5, next_attempt_at = $6, completed_at = $7,
		last_failure_class = $8, last_error = $9, replays = $10,
		payload = CASE WHEN $4 = 'completed' THEN NULL ELSE payload END WHERE tenant_id = $1 AND consumer = $2 AND event_id = $3`,
		e.TenantID, e.Consumer, e.EventID, string(e.State), e.Attempts, next, completed, nullable(string(e.LastClass)), nullable(e.LastError), e.Replays)
	if err != nil {
		return fmt.Errorf("events: save inbox entry: %w", err)
	}
	return nil
}

func recordInboxHistory(ctx context.Context, tx pgx.Tx, e events.InboxEntry, outcome, class, message, replay string) error {
	_, err := tx.Exec(ctx, `INSERT INTO event_inbox_attempts (tenant_id, consumer, event_id, attempt, outcome, class, error, replay_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		e.TenantID, e.Consumer, e.EventID, e.Attempts, outcome, nullable(class), nullable(message), nullable(replay))
	if err != nil {
		return fmt.Errorf("events: record inbox history: %w", err)
	}
	return nil
}

var idLike = regexp.MustCompile(domain.IDPattern)

// quarantineRaw durably quarantines a delivery that cannot be processed at all.
// The tenant is the trusted subscription scope; nothing from the payload
// selects it. The event ID in the record is best effort and display only.
func (s *Store) quarantineRaw(ctx context.Context, d events.Delivery, reason, diagnostic string) (events.Outcome, error) {
	return s.quarantineDelivery(ctx, d, reason, diagnostic, false)
}

// quarantineDelivery is quarantineRaw. With contentKeyed the record is keyed by
// the payload's hash even when an event ID can be read from it: the ID already
// names another (original) record that this delivery must not overwrite.
func (s *Store) quarantineDelivery(ctx context.Context, d events.Delivery, reason, diagnostic string, contentKeyed bool) (events.Outcome, error) {
	sum := sha256.Sum256(d.Payload)
	rec := quarantineRecord{
		tenant: d.Tenant, consumer: d.Consumer, deliveryKey: "raw:" + hex.EncodeToString(sum[:]),
		reason: reason, diagnostic: events.BoundedMessage(diagnostic), identity: d.Identity, payload: d.Payload,
	}
	var loose struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if len(d.Payload) <= eventsv1.MaxEnvelopeBytes && json.Unmarshal(d.Payload, &loose) == nil {
		if idLike.MatchString(loose.ID) {
			rec.eventID = loose.ID
			if !contentKeyed {
				rec.deliveryKey = loose.ID
			}
		}
		// Display-only and untrusted: NUL and invalid UTF-8 cannot be stored in
		// a text column, so such a type is omitted (the payload keeps the bytes).
		if len(loose.Type) <= 128 && events.IsSafeText(loose.Type) {
			rec.eventType = loose.Type
		}
	}
	var id string
	err := s.inTenant(ctx, d.Tenant, func(ctx context.Context, tx pgx.Tx) (err error) {
		id, err = insertQuarantine(ctx, tx, rec)
		return err
	})
	if err != nil {
		return events.Outcome{}, err
	}
	return events.Outcome{Kind: events.OutcomeQuarantined, QuarantineID: id, Reason: reason}, nil
}

// RepairResult counts one repair pass for a consumer.
type RepairResult struct {
	Retried    int
	Backfilled int
	Failed     int
	// Refused counts selected events whose deduplication evidence could no
	// longer be trusted when their transaction ran (see checkEvidenceHorizon).
	// They are not processed; an operator replay with the explicit
	// idempotent-consumer exception is the only way to run them.
	Refused int
}

// Repair re-drives work the broker may have lost, independently of any broker
// message: scheduled inbox retries that are due (their payload lives on the
// inbox entry, so they stay recoverable after the broker copy and the
// observation history expired) and retained observations the
// consumer has no inbox entry for, older than grace and still inside the inbox
// retention and the durable deduplication boundary (so pruned deduplication evidence cannot cause a repeat). The grace
// age is measured on the database clock that stamped recorded_at, so clock skew
// between this process and PostgreSQL cannot hide an event that was committed
// before the scan; the retention bound uses the injected clock. Every
// event goes through the same inbox transaction as a live delivery, so repair
// can never duplicate an effect. Quarantined entries are untouched.
func (s *Store) Repair(ctx context.Context, tenant domain.TenantID, consumer string, grace time.Duration, limit int) (RepairResult, error) {
	var res RepairResult
	b, ok := s.consumers[consumer]
	if !ok {
		return res, fmt.Errorf("%w: consumer %q is not registered", events.ErrInvalidConfiguration, consumer)
	}
	if limit < 1 || grace < 0 {
		return res, fmt.Errorf("%w: repair needs a positive limit and non-negative grace", events.ErrInvalidConfiguration)
	}
	now := s.now()
	type pending struct {
		payload []byte
		retry   bool
	}
	var work []pending
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		work = work[:0]
		bound, err := s.loadDedupBoundary(ctx, tx, tenant, now)
		if err != nil {
			return err
		}
		types := b.Spec.Types
		if types == nil {
			types = []string{}
		}
		rows, err := tx.Query(ctx, `SELECT i.payload, true FROM event_inbox i
				WHERE i.tenant_id = $1 AND i.consumer = $2 AND i.state = 'pending' AND i.next_attempt_at <= $3 AND i.payload IS NOT NULL
				  AND (cardinality($5::text[]) = 0 OR i.event_type = ANY($5))
				ORDER BY i.next_attempt_at LIMIT $4`, string(tenant), consumer, now, limit, types)
		if err != nil {
			return fmt.Errorf("events: repair retries: %w", err)
		}
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.payload, &p.retry); err != nil {
				rows.Close()
				return err
			}
			work = append(work, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if remaining := limit - len(work); remaining > 0 {
			rows, err := tx.Query(ctx, `SELECT o.payload, false FROM event_observations o
				LEFT JOIN event_inbox i ON i.tenant_id = o.tenant_id AND i.consumer = $2 AND i.event_id = o.event_id
				WHERE o.tenant_id = $1 AND i.event_id IS NULL AND o.recorded_at <= now() - make_interval(secs => $3::float8) AND o.recorded_at > $4
				  AND (cardinality($6::text[]) = 0 OR o.event_type = ANY($6))
				ORDER BY o.recorded_at, o.scope_key, o.seq LIMIT $5`,
				string(tenant), consumer, grace.Seconds(), bound.inboxBefore, remaining, types)
			if err != nil {
				return fmt.Errorf("events: repair backfill: %w", err)
			}
			for rows.Next() {
				var p pending
				if err := rows.Scan(&p.payload, &p.retry); err != nil {
					rows.Close()
					return err
				}
				work = append(work, p)
			}
			rows.Close()
			return rows.Err()
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	for _, w := range work {
		env, err := eventsv1.Decode(w.payload)
		if err != nil {
			res.Failed++
			continue
		}
		pr, err := s.processEnvelope(ctx, tenant, b, env, w.payload, events.DeliveryIdentity{}, nil)
		if errors.Is(err, errBeyondHorizon) {
			res.Refused++
			continue
		}
		if err != nil {
			res.Failed++
			continue
		}
		if pr.outcome.Kind == events.OutcomeRetry || pr.outcome.Kind == events.OutcomeQuarantined {
			res.Failed++
		}
		if w.retry {
			res.Retried++
		} else {
			res.Backfilled++
		}
	}
	return res, nil
}

// RepairReconciliation adapts Repair to the Reconciler's per-tenant callback.
func (s *Store) RepairReconciliation(consumer string, grace time.Duration, limit int) events.Reconciliation {
	return events.Reconciliation{
		Name: "inbox-repair-" + consumer,
		Run: func(ctx context.Context, tenant domain.TenantID) error {
			_, err := s.Repair(ctx, tenant, consumer, grace, limit)
			return err
		},
	}
}

func newID(prefix string) string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return prefix + hex.EncodeToString(b[:])
}
