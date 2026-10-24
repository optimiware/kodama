package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
)

type auditRow struct {
	tenant       domain.TenantID
	id           string
	project      string
	operator     string
	action       string
	resourceType string
	resourceID   string
	reason       string
	metadata     map[string]any
}

// insertAudit writes one operator audit row to the merged audit table in the
// caller's transaction. Metadata must contain only values safe for every
// reader of the audit trail (identifiers, counts, ranges).
func insertAudit(ctx context.Context, tx pgx.Tx, a auditRow) error {
	meta, err := json.Marshal(a.metadata)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit (tenant_id, id, project_id, actor_type, actor_id, action, resource_type, resource_id, reason, metadata)
		VALUES ($1, $2, $3, 'operator', $4, $5, $6, $7, $8, $9)`,
		string(a.tenant), a.id, nullable(a.project), a.operator, a.action, a.resourceType, a.resourceID, nullable(a.reason), meta)
	if err != nil {
		return fmt.Errorf("events: write audit: %w", err)
	}
	return nil
}

// authorizeOperator checks an operator for a tenant-level consumer operation
// (fence, clear, discard) through the configured authorizer; no authorizer
// refuses everything.
func (s *Store) authorizeOperator(ctx context.Context, tenant domain.TenantID, consumer string, operator events.Operator, reason string) error {
	_, err := events.Authorize(ctx, s.opts.Authorizer, events.ReplayRequest{
		Scope: events.Scope{TenantID: tenant}, Consumer: consumer, FromSeq: 1, ToSeq: 1, Reason: reason, Operator: operator,
	})
	return err
}

// ReplayRecord is the durable state of one replay request.
type ReplayRecord struct {
	ID             string
	Request        events.ReplayRequest
	Classification events.ReplayClassification
	State          string
	NextSeq        int64
	Processed      int
	Skipped        int
	Failed         int
	CreatedAt      time.Time
	// EvidenceFloor is the deduplication horizon fixed at admission: events
	// recorded earlier that have no inbox evidence were beyond it.
	EvidenceFloor time.Time
	CompletedAt   time.Time
}

func (s *Store) binding(consumer string) (Binding, error) {
	b, ok := s.consumers[consumer]
	if !ok {
		return Binding{}, fmt.Errorf("%w: consumer %q is not registered", events.ErrInvalidConfiguration, consumer)
	}
	return b, nil
}

// classify is the dry-run: it describes, from delivery evidence only, what a
// replay of req would do for the consumer. It reads in the caller's transaction.
//
// It also returns the evidence floor the classification used: the later of the
// current inbox retention's cutoff and the durable deduplication boundary that
// pruning persisted, so a larger retention setting cannot readmit forgotten
// events as "undelivered".
func (s *Store) classify(ctx context.Context, tx pgx.Tx, req events.ReplayRequest, b Binding, now time.Time) (events.ReplayClassification, time.Time, error) {
	c := events.ReplayClassification{Effect: b.Spec.Effect}
	bound, err := s.loadDedupBoundary(ctx, tx, req.Scope.TenantID, now)
	if err != nil {
		return c, time.Time{}, err
	}
	floor := bound.inboxBefore
	wm, err := s.Watermark(ctx, tx, req.Scope)
	if err != nil {
		return c, floor, err
	}
	if req.ToSeq > wm.HighWater {
		return c, floor, fmt.Errorf("%w: range ends at %d but the scope's last event is %d", events.ErrReplayUnsafe, req.ToSeq, wm.HighWater)
	}
	if wm.Floor >= req.FromSeq {
		c.Expired = int(min(req.ToSeq, wm.Floor) - req.FromSeq + 1)
	}
	lo := max(req.FromSeq, wm.Floor+1)
	if lo <= req.ToSeq {
		types := b.Spec.Types
		if types == nil {
			types = []string{}
		}
		err := tx.QueryRow(ctx, `SELECT count(*),
				count(*) FILTER (WHERE i.state = 'completed'),
				count(*) FILTER (WHERE i.state = 'quarantined'),
				count(*) FILTER (WHERE i.state = 'pending'),
				count(*) FILTER (WHERE i.event_id IS NULL AND o.recorded_at >= $6),
				count(*) FILTER (WHERE i.event_id IS NULL AND o.recorded_at < $6)
			FROM event_observations o
			LEFT JOIN event_inbox i ON i.tenant_id = o.tenant_id AND i.consumer = $2 AND i.event_id = o.event_id
			WHERE o.tenant_id = $1 AND o.scope_key = $3 AND o.seq BETWEEN $4 AND $5
			  AND (cardinality($7::text[]) = 0 OR o.event_type = ANY($7))`,
			string(req.Scope.TenantID), b.Spec.Name, req.Scope.Key(), lo, req.ToSeq, floor, types).
			Scan(&c.Events, &c.AlreadyCompleted, &c.Quarantined, &c.Pending, &c.Undelivered, &c.BeyondHorizon)
		if err != nil {
			return c, floor, fmt.Errorf("events: classify replay: %w", err)
		}
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM event_recovery_fences WHERE tenant_id = $1 AND consumer = $2 AND cleared_at IS NULL)`,
		string(req.Scope.TenantID), b.Spec.Name).Scan(&c.Fenced); err != nil {
		return c, floor, fmt.Errorf("events: read fence: %w", err)
	}
	return c, floor, nil
}

// DryRun authorizes the operator and classifies what replaying the range for
// the consumer would do, without changing anything. The classification is
// returned even when the replay would be refused; the error then explains why
// (unsafe consumer, beyond the horizon, expired range, restore fence).
func (s *Store) DryRun(ctx context.Context, req events.ReplayRequest) (events.ReplayClassification, error) {
	if _, err := events.Authorize(ctx, s.opts.Authorizer, req); err != nil {
		return events.ReplayClassification{}, err
	}
	b, err := s.binding(req.Consumer)
	if err != nil {
		return events.ReplayClassification{}, err
	}
	var c events.ReplayClassification
	err = s.inTenant(ctx, req.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		c, _, err = s.classify(ctx, tx, req, b, s.now())
		return err
	})
	if err != nil {
		return c, err
	}
	return c, c.Check(b.Spec, req.AllowBeyondHorizon)
}

// RequestReplay authorizes, classifies and, only if safe, persists the replay
// request together with its audit row in one transaction. Nothing runs yet:
// RunReplay (or ResumeReplays after a restart) performs the work.
func (s *Store) RequestReplay(ctx context.Context, req events.ReplayRequest) (ReplayRecord, error) {
	if _, err := events.Authorize(ctx, s.opts.Authorizer, req); err != nil {
		return ReplayRecord{}, err
	}
	b, err := s.binding(req.Consumer)
	if err != nil {
		return ReplayRecord{}, err
	}
	rec := ReplayRecord{ID: newID("rpl_"), Request: req, State: "requested", NextSeq: req.FromSeq}
	err = s.inTenant(ctx, req.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Serialize admission with observation pruning: pruning locks the scope
		// row FOR UPDATE, which conflicts with this lock, and reads the active
		// requests only after acquiring it. Holding the lock from before the
		// watermark and classification reads until the request row commits means
		// the classified range cannot be pruned before the request pins it, and
		// a prune that wins the lock is visible in the watermark read below.
		// KEY SHARE does not block appenders, which only update non-key columns.
		// Inbox pruning is serialized the same way, by the admission lock, so
		// the completed entries the classification counts cannot disappear
		// before the request pins them.
		if err := lockReplayAdmission(ctx, tx, req.Scope.TenantID); err != nil {
			return err
		}
		var one int
		switch err := tx.QueryRow(ctx, `SELECT 1 FROM event_scopes WHERE tenant_id = $1 AND scope_key = $2 FOR KEY SHARE`,
			string(req.Scope.TenantID), req.Scope.Key()).Scan(&one); {
		case errors.Is(err, pgx.ErrNoRows): // no events in this scope: classification rejects the range
		case err != nil:
			return fmt.Errorf("events: lock scope: %w", err)
		}
		now := s.now()
		c, floor, err := s.classify(ctx, tx, req, b, now)
		if err != nil {
			return err
		}
		if err := c.Check(b.Spec, req.AllowBeyondHorizon); err != nil {
			return err
		}
		var overlap int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM event_replay_requests WHERE tenant_id = $1 AND consumer = $2 AND scope_key = $3 AND state <> 'completed'
			AND from_seq <= $5 AND to_seq >= $4`, string(req.Scope.TenantID), req.Consumer, req.Scope.Key(), req.FromSeq, req.ToSeq).Scan(&overlap); err != nil {
			return fmt.Errorf("events: check active replays: %w", err)
		}
		if overlap > 0 {
			return fmt.Errorf("%w: an active replay already covers part of this range", events.ErrReplayUnsafe)
		}
		classification, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO event_replay_requests
			(tenant_id, id, consumer, scope_key, project_id, from_seq, to_seq, reason, operator_id, allow_beyond_horizon, effect_class, classification, next_seq, created_at, updated_at, evidence_floor)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14, $15)`,
			string(req.Scope.TenantID), rec.ID, req.Consumer, req.Scope.Key(), nullable(string(req.Scope.ProjectID)), req.FromSeq, req.ToSeq, req.Reason,
			req.Operator.ID, req.AllowBeyondHorizon, string(b.Spec.Effect), classification, req.FromSeq, now, floor); err != nil {
			return fmt.Errorf("events: persist replay request: %w", err)
		}
		rec.Classification, rec.CreatedAt, rec.EvidenceFloor = c, now, floor
		return insertAudit(ctx, tx, auditRow{tenant: req.Scope.TenantID, id: rec.ID + "_requested", project: string(req.Scope.ProjectID), operator: req.Operator.ID,
			action: "event.replay_requested", resourceType: "event_replay", resourceID: rec.ID, reason: req.Reason,
			metadata: map[string]any{"consumer": req.Consumer, "scope": req.Scope.String(), "fromSeq": req.FromSeq, "toSeq": req.ToSeq,
				"effectClass": string(b.Spec.Effect), "events": c.Events, "quarantined": c.Quarantined, "alreadyCompleted": c.AlreadyCompleted, "beyondHorizon": c.BeyondHorizon}})
	})
	if err != nil {
		return ReplayRecord{}, err
	}
	return rec, nil
}

// GetReplay reads one replay request of the tenant.
func (s *Store) GetReplay(ctx context.Context, tenant domain.TenantID, id string) (ReplayRecord, error) {
	var rec ReplayRecord
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) (err error) {
		rec, _, err = loadReplay(ctx, tx, tenant, id, false)
		return err
	})
	return rec, err
}

type replayClaimState struct {
	token int64
	lease *time.Time
}

func loadReplay(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, id string, lock bool) (ReplayRecord, replayClaimState, error) {
	var rec ReplayRecord
	var project *string
	var classification []byte
	var cs replayClaimState
	var completed *time.Time
	q := `SELECT consumer, scope_key, project_id, from_seq, to_seq, reason, operator_id, allow_beyond_horizon, classification, state, next_seq,
			processed, skipped, failed, claim_token, lease_expires_at, created_at, evidence_floor, completed_at
		FROM event_replay_requests WHERE tenant_id = $1 AND id = $2`
	if lock {
		q += " FOR UPDATE"
	}
	var scopeKey string
	err := tx.QueryRow(ctx, q, string(tenant), id).Scan(&rec.Request.Consumer, &scopeKey, &project, &rec.Request.FromSeq, &rec.Request.ToSeq, &rec.Request.Reason,
		&rec.Request.Operator.ID, &rec.Request.AllowBeyondHorizon, &classification, &rec.State, &rec.NextSeq, &rec.Processed, &rec.Skipped, &rec.Failed, &cs.token, &cs.lease, &rec.CreatedAt, &rec.EvidenceFloor, &completed)
	if errors.Is(err, pgx.ErrNoRows) {
		return rec, cs, fmt.Errorf("%w: replay request %s", events.ErrNotFound, id)
	}
	if err != nil {
		return rec, cs, fmt.Errorf("events: load replay request: %w", err)
	}
	rec.ID = id
	rec.Request.Scope = events.Scope{TenantID: tenant}
	if project != nil {
		rec.Request.Scope.ProjectID = domain.ProjectID(*project)
	}
	if err := json.Unmarshal(classification, &rec.Classification); err != nil {
		return rec, cs, fmt.Errorf("events: replay classification: %w", err)
	}
	if completed != nil {
		rec.CompletedAt = *completed
	}
	return rec, cs, nil
}

// RunReplay performs (or resumes) one replay request: authorization is checked
// again, the request is claimed under a lease and fencing token, and its range
// is processed in bounded batches through the same inbox transaction as a live
// delivery, so completed events are skipped and never repeated, quarantined
// ones are reopened, and the same event IDs are used. Progress is durable after
// every batch: after a crash another runner resumes from the next sequence and
// the inbox absorbs any overlap. A restore fence stops the run before the next event's effect, and so does a
// stale claim token: both are validated in each event's own transaction.
func (s *Store) RunReplay(ctx context.Context, tenant domain.TenantID, id string) (ReplayRecord, error) {
	var rec ReplayRecord
	var token int64
	var b Binding
	if err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		r, cs, err := loadReplay(ctx, tx, tenant, id, true)
		if err != nil {
			return err
		}
		if r.State == "completed" {
			rec = r
			return nil
		}
		now := s.now()
		if cs.lease != nil && cs.lease.After(now) {
			return fmt.Errorf("%w: replay %s is already running", events.ErrInvalidTransition, id)
		}
		if b, err = s.binding(r.Request.Consumer); err != nil {
			return err
		}
		var fenced bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM event_recovery_fences WHERE tenant_id = $1 AND consumer = $2 AND cleared_at IS NULL)`,
			string(tenant), r.Request.Consumer).Scan(&fenced); err != nil {
			return err
		}
		// A request with nothing left to process only needs its terminal
		// bookkeeping, which has no effect for the fence to guard.
		if fenced && r.NextSeq <= r.Request.ToSeq {
			return fmt.Errorf("%w: consumer %s", events.ErrReplayFenced, r.Request.Consumer)
		}
		if err := tx.QueryRow(ctx, `UPDATE event_replay_requests SET state = 'running', claim_token = claim_token + 1, lease_expires_at = $3, updated_at = $4
			WHERE tenant_id = $1 AND id = $2 RETURNING claim_token`, string(tenant), id, now.Add(s.opts.ReplayLease), now).Scan(&token); err != nil {
			return fmt.Errorf("events: claim replay: %w", err)
		}
		r.State = "running"
		rec = r
		return nil
	}); err != nil {
		return rec, err
	}
	if rec.State == "completed" {
		return rec, nil
	}
	grant, err := events.Authorize(ctx, s.opts.Authorizer, rec.Request)
	if err != nil {
		_ = s.releaseReplay(ctx, tenant, id, token)
		return rec, err
	}
	scope := rec.Request.Scope
	types := b.Spec.Types
	if types == nil {
		types = []string{}
	}
	for rec.NextSeq <= rec.Request.ToSeq {
		type item struct {
			seq        int64
			payload    []byte
			recordedAt time.Time
		}
		var batch []item
		if err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			batch = batch[:0]
			// Active requests pin their unprocessed observations against
			// pruning, so a floor at or past the next position means the range
			// was lost: stop rather than treat the gap as a short page.
			var floor int64
			if err := tx.QueryRow(ctx, `SELECT floor_seq FROM event_scopes WHERE tenant_id = $1 AND scope_key = $2`, string(tenant), scope.Key()).Scan(&floor); err != nil {
				return fmt.Errorf("events: read replay floor: %w", err)
			}
			if floor >= rec.NextSeq {
				return fmt.Errorf("%w: observations up to %d were pruned before replay %s processed %d", events.ErrReplayRangeExpired, floor, id, rec.NextSeq)
			}
			rows, err := tx.Query(ctx, `SELECT seq, payload, recorded_at FROM event_observations WHERE tenant_id = $1 AND scope_key = $2 AND seq BETWEEN $3 AND $4
				AND (cardinality($6::text[]) = 0 OR event_type = ANY($6)) ORDER BY seq LIMIT $5`,
				string(tenant), scope.Key(), rec.NextSeq, rec.Request.ToSeq, s.opts.ReplayBatch, types)
			if err != nil {
				return fmt.Errorf("events: replay batch: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var it item
				if err := rows.Scan(&it.seq, &it.payload, &it.recordedAt); err != nil {
					return err
				}
				batch = append(batch, it)
			}
			return rows.Err()
		}); err != nil {
			if refused(err) {
				_ = s.releaseReplay(ctx, tenant, id, token)
			}
			return rec, err
		}
		next := rec.NextSeq
		var processed, skipped, failed, undecodable int
		for _, it := range batch {
			env, derr := eventsv1.Decode(it.payload)
			if derr != nil {
				failed++
				undecodable++
				next = it.seq + 1
				continue
			}
			// The event's own transaction validates the claim token, the fence
			// and the admission-time safety, and records its progress.
			pr, err := s.processEnvelope(ctx, tenant, b, env, it.payload, events.DeliveryIdentity{}, &replayContext{
				grant: grant, id: id, token: token, seq: it.seq, recordedAt: it.recordedAt,
				evidenceFloor: rec.EvidenceFloor, effect: rec.Classification.Effect, lease: s.opts.ReplayLease,
			})
			if errors.Is(err, errContentConflict) {
				// The consumer's inbox holds this event ID for different
				// content, so replaying this observation would act on content
				// the entry does not describe. Keep the observation for an
				// operator, leave the entry as it is, and count the failure
				// durably with the step's progress.
				if _, qerr := s.quarantineDelivery(ctx, events.Delivery{Tenant: tenant, Consumer: b.Spec.Name, Payload: it.payload}, ReasonConflictingContent,
					"retained event content differs from the content already recorded for this event ID", true); qerr != nil {
					return rec, qerr
				}
				failed++
				undecodable++
				next = it.seq + 1
				continue
			}
			if err != nil {
				if refused(err) {
					_ = s.releaseReplay(ctx, tenant, id, token)
				}
				return rec, fmt.Errorf("events: replay event %s: %w", env.ID, err)
			}
			switch {
			case pr.skipped:
				skipped++
			case pr.outcome.Kind == events.OutcomeCompleted:
				processed++
			default:
				failed++
			}
			next = it.seq + 1
		}
		if len(batch) < s.opts.ReplayBatch {
			next = rec.Request.ToSeq + 1 // a short page, with the floor checked above, means nothing else in range qualifies
		}
		if err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			// Counts of processed events were recorded in their own transactions;
			// only undecodable events and the position are recorded here.
			// Completion is a separate, independently resumable step: a crash
			// after the last event leaves next_seq past the range and the next
			// runner finalizes without replaying anything.
			tag, err := tx.Exec(ctx, `UPDATE event_replay_requests SET next_seq = GREATEST(next_seq, $4), failed = failed + $5,
				lease_expires_at = $6::timestamptz, updated_at = $7
				WHERE tenant_id = $1 AND id = $2 AND claim_token = $3 AND state = 'running'`,
				string(tenant), id, token, next, undecodable, s.now().Add(s.opts.ReplayLease), s.now())
			if err != nil {
				return fmt.Errorf("events: record replay progress: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return fmt.Errorf("%w: replay %s was claimed by another runner", events.ErrStaleClaim, id)
			}
			return nil
		}); err != nil {
			return rec, err
		}
		rec.NextSeq, rec.Processed, rec.Skipped, rec.Failed = next, rec.Processed+processed, rec.Skipped+skipped, rec.Failed+failed
	}
	return s.finalizeReplay(ctx, tenant, id, token, rec)
}

// finalizeReplay completes a request whose range has been fully processed
// (next_seq beyond to_seq). It is fenced by the claim token and atomically
// persists the terminal state with its audit row, whose counters are read from
// the request row's durable counters in the same statement, never from runner
// memory: a runner that resumes after a crash reports the whole request. A
// second finalization finds the request completed and changes nothing, so the
// audit row exists exactly once.
func (s *Store) finalizeReplay(ctx context.Context, tenant domain.TenantID, id string, token int64, rec ReplayRecord) (ReplayRecord, error) {
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		now := s.now()
		var processed, skipped, failed int
		err := tx.QueryRow(ctx, `UPDATE event_replay_requests SET state = 'completed', lease_expires_at = NULL, completed_at = $4, updated_at = $4
			WHERE tenant_id = $1 AND id = $2 AND claim_token = $3 AND state = 'running' AND next_seq > to_seq
			RETURNING processed, skipped, failed`, string(tenant), id, token, now).Scan(&processed, &skipped, &failed)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: replay %s cannot be finalized by this runner", events.ErrStaleClaim, id)
		}
		if err != nil {
			return fmt.Errorf("events: finalize replay: %w", err)
		}
		rec.Processed, rec.Skipped, rec.Failed = processed, skipped, failed
		return insertAudit(ctx, tx, auditRow{tenant: tenant, id: id + "_completed", project: string(rec.Request.Scope.ProjectID), operator: rec.Request.Operator.ID,
			action: "event.replay_completed", resourceType: "event_replay", resourceID: id, reason: rec.Request.Reason,
			metadata: map[string]any{"consumer": rec.Request.Consumer, "processed": processed, "skipped": skipped, "failed": failed}})
	})
	if err != nil {
		return rec, err
	}
	rec.State, rec.CompletedAt = "completed", s.now()
	return rec, nil
}

// refused reports a deterministic refusal (restore fence, unsafe or changed
// classification, lost range) after which the runner knowingly stops and gives
// up its lease. Stale claims are not ours to release, and other errors keep the
// lease until it lapses.
func refused(err error) bool {
	return errors.Is(err, events.ErrReplayFenced) || errors.Is(err, events.ErrReplayUnsafe) ||
		errors.Is(err, events.ErrReplayBeyondHorizon) || errors.Is(err, events.ErrReplayRangeExpired)
}

func (s *Store) releaseReplay(ctx context.Context, tenant domain.TenantID, id string, token int64) error {
	return s.inTenant(context.WithoutCancel(ctx), tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE event_replay_requests SET lease_expires_at = NULL WHERE tenant_id = $1 AND id = $2 AND claim_token = $3 AND state = 'running'`, string(tenant), id, token)
		return err
	})
}

// ResumeReplays continues every active replay request of the tenant whose
// runner is gone (lease expired or never started). It is safe to call
// repeatedly and from several processes: claims are fenced.
func (s *Store) ResumeReplays(ctx context.Context, tenant domain.TenantID) (int, error) {
	var ids []string
	if err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		ids = ids[:0]
		rows, err := tx.Query(ctx, `SELECT id FROM event_replay_requests WHERE tenant_id = $1 AND state <> 'completed'
			AND (lease_expires_at IS NULL OR lease_expires_at <= $2) ORDER BY created_at, id LIMIT 100`, string(tenant), s.now())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	}); err != nil {
		return 0, err
	}
	var errs []error
	resumed := 0
	for _, id := range ids {
		if _, err := s.RunReplay(ctx, tenant, id); err != nil {
			if errors.Is(err, events.ErrInvalidTransition) || errors.Is(err, events.ErrStaleClaim) {
				continue // another runner owns it
			}
			errs = append(errs, err)
			continue
		}
		resumed++
	}
	return resumed, errors.Join(errs...)
}

// ReplayReconciliation adapts ResumeReplays to the Reconciler.
func (s *Store) ReplayReconciliation() events.Reconciliation {
	return events.Reconciliation{Name: "replay-resume", Run: func(ctx context.Context, tenant domain.TenantID) error {
		_, err := s.ResumeReplays(ctx, tenant)
		return err
	}}
}

// FenceConsumer records a restore fence for a consumer: replay for it is
// refused until ClearFence. Operators fence consumers after a database restore,
// before side-effecting replay, so external effects can be reconciled first.
func (s *Store) FenceConsumer(ctx context.Context, tenant domain.TenantID, consumer string, operator events.Operator, reason string) error {
	if err := s.authorizeOperator(ctx, tenant, consumer, operator, reason); err != nil {
		return err
	}
	if _, err := s.binding(consumer); err != nil {
		return err
	}
	return s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		// Serialize with replay admission before enumerating the active
		// requests: RequestReplay holds this lock from its classification (which
		// reads the fence) to the commit of its request row. A request committed
		// before we get the lock is visible to the statement below and is
		// stopped by it; one that waits for us classifies after our fence
		// committed and is refused. Without it a request could be admitted
		// between our UPDATE and our commit, start its first effect without
		// seeing the uncommitted fence, and outlive our return.
		// Lock order: admission lock, then request rows (as RequestReplay does
		// before it inserts); replay steps take only their own request row.
		if err := lockReplayAdmission(ctx, tx, tenant); err != nil {
			return err
		}
		now := s.now()
		// Touch the consumer's active replay requests next. A running replay
		// holds its request row for the duration of each event, so this waits
		// for the event in flight and, once the fence commits, the runner's next
		// event observes it before any effect (see guardReplayStep).
		if _, err := tx.Exec(ctx, `UPDATE event_replay_requests SET updated_at = $3 WHERE tenant_id = $1 AND consumer = $2 AND state <> 'completed'`,
			string(tenant), consumer, now); err != nil {
			return fmt.Errorf("events: stop active replays: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO event_recovery_fences (tenant_id, consumer, fenced_at, fenced_by, reason) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id, consumer) DO UPDATE SET fenced_at = EXCLUDED.fenced_at, fenced_by = EXCLUDED.fenced_by, reason = EXCLUDED.reason,
				cleared_at = NULL, cleared_by = NULL, clear_reason = NULL`, string(tenant), consumer, now, operator.ID, reason); err != nil {
			return fmt.Errorf("events: fence consumer: %w", err)
		}
		return insertAudit(ctx, tx, auditRow{tenant: tenant, id: newID("fence_"), operator: operator.ID, action: "event.consumer_fenced",
			resourceType: "event_consumer", resourceID: consumer, reason: reason, metadata: map[string]any{"consumer": consumer}})
	})
}

// ClearFence records that the consumer's owner confirmed reconciliation of
// external effects and fencing of old controllers, re-enabling replay.
func (s *Store) ClearFence(ctx context.Context, tenant domain.TenantID, consumer string, operator events.Operator, reason string) error {
	if err := s.authorizeOperator(ctx, tenant, consumer, operator, reason); err != nil {
		return err
	}
	return s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE event_recovery_fences SET cleared_at = $3, cleared_by = $4, clear_reason = $5
			WHERE tenant_id = $1 AND consumer = $2 AND cleared_at IS NULL`, string(tenant), consumer, s.now(), operator.ID, reason)
		if err != nil {
			return fmt.Errorf("events: clear fence: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: no active fence for consumer %s", events.ErrNotFound, consumer)
		}
		return insertAudit(ctx, tx, auditRow{tenant: tenant, id: newID("unfence_"), operator: operator.ID, action: "event.consumer_fence_cleared",
			resourceType: "event_consumer", resourceID: consumer, reason: reason, metadata: map[string]any{"consumer": consumer}})
	})
}
