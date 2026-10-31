package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

const digestPrefix = "sha256:"

// Bounds of the durable columns the kernel's request validation does not state.
const (
	maxCallerChars = 512
	maxActorChars  = 256
)

func checkIdentityBounds(id runtime.SignalIdentity) error {
	if utf8.RuneCountInString(id.Caller) > maxCallerChars {
		return invalid("a signal caller is at most %d characters", maxCallerChars)
	}
	return nil
}

// LookupSignal returns the durable record or tombstone of a signal identity, or
// the zero lookup when there is none. It reads the identity's own row only: it
// does not depend on the wait, step or run still existing.
func (s *Store) LookupSignal(ctx context.Context, id runtime.SignalIdentity) (runtime.SignalLookup, error) {
	if err := id.Validate(); err != nil {
		return runtime.SignalLookup{}, err
	}
	if err := checkIdentityBounds(id); err != nil {
		return runtime.SignalLookup{}, err
	}
	var out runtime.SignalLookup
	err := s.view(ctx, id.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = lookupSignal(ctx, tx, id)
		return err
	})
	if err != nil {
		return runtime.SignalLookup{}, err
	}
	return out, nil
}

func lookupSignal(ctx context.Context, tx pgx.Tx, id runtime.SignalIdentity) (runtime.SignalLookup, error) {
	var (
		shape, digest                   string
		applied                         bool
		refusal, runID, stepKey, waitID *string
		kind, outcome, answer, actor    *string
		reasonCol                       *string
		version                         *int64
		decidedAt                       *time.Time
		recordedAt                      time.Time
	)
	err := tx.QueryRow(ctx, stmtGetSignal, id.Scope.TenantID, id.Scope.ProjectID, id.Caller, id.Key).
		Scan(&shape, &digest, &applied, &refusal, &runID, &stepKey, &waitID, &kind, &outcome, &answer, &actor, &version, &decidedAt, &reasonCol, &recordedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return runtime.SignalLookup{}, nil
	}
	if err != nil {
		return runtime.SignalLookup{}, fmt.Errorf("runstate/postgres: read signal: %w", err)
	}
	sid := id.ID()
	if shape == "tombstone" {
		return runtime.SignalLookup{Tombstone: &runtime.SignalTombstone{Identity: id, ID: sid, Digest: digestPrefix + digest,
			Refusal: str[runtime.RefusalReason](refusal), RecordedAt: utc(recordedAt)}}, nil
	}
	if runID == nil || stepKey == nil || waitID == nil {
		return runtime.SignalLookup{}, fmt.Errorf("runstate/postgres: signal %s has no target", sid)
	}
	rec := runtime.SignalRecord{Identity: id, ID: sid, Digest: digestPrefix + digest, Applied: applied,
		Refusal: str[runtime.RefusalReason](refusal), RecordedAt: utc(recordedAt),
		Target: runtime.SignalTarget{RunID: domain.RunID(*runID), StepKey: domain.StepID(*stepKey), WaitID: runtime.WaitID(*waitID)}}
	if kind != nil && version != nil && decidedAt != nil {
		d := runtime.Decision{Kind: runtime.DecisionKind(*kind), Outcome: str[string](outcome), Answer: str[string](answer),
			Actor: str[string](actor), Version: domain.Version(*version), At: utcp(decidedAt)}
		switch d.Kind {
		case runtime.DecisionResolved:
			// The deciding signal is the applied record itself; a refusal that lost
			// to a resolution names the signal that won.
			d.SignalID = sid
			if !applied {
				signal, err := winningSignal(ctx, tx, id.Scope, rec.Target)
				if err != nil {
					return runtime.SignalLookup{}, err
				}
				d.SignalID = signal
			}
		case runtime.DecisionCancelled:
			// The reason is part of the record, so a replay needs nothing else. A
			// cancelled record written before migration 007 has none: it is refused
			// (fail closed) rather than given a reason nobody recorded, and the
			// signal is not routed again.
			if reasonCol == nil {
				return runtime.SignalLookup{}, fmt.Errorf("%w: signal %s lost to a cancellation recorded before its reason was kept (migration 007); "+
					"the reason is not recorded and is not guessed", domain.ErrInvalidValue, sid)
			}
			d.Reason = runtime.Reason(*reasonCol)
		}
		rec.Decision = d
	}
	return runtime.SignalLookup{Record: &rec}, nil
}

// winningSignal returns the identifier of the signal that resolved the target
// wait: the one applied record of that wait.
func winningSignal(ctx context.Context, tx pgx.Tx, scope domain.Scope, target runtime.SignalTarget) (runtime.SignalID, error) {
	var signal string
	err := tx.QueryRow(ctx, stmtWinningSignal, scope.TenantID, scope.ProjectID, target.WaitID).Scan(&signal)
	if err != nil {
		return "", fmt.Errorf("runstate/postgres: read the signal that resolved wait %s: %w", target.WaitID, err)
	}
	return runtime.SignalID(signal), nil
}

// DecideSignal routes and decides one signal in a single transaction by applying
// runtime.RouteSignal against the identity's durable lookup and the waits of the
// caller's project. The updated wait, the decision record or tombstone, and the
// outbox events commit together or not at all.
//
// Two locks make it safe against races: an advisory lock on the signal identity
// serializes duplicates, so a retry sees the record its twin committed; the row
// lock of each run the signal can reach serializes it against another signal,
// the engine's commits and a cancellation, all of which decide the wait through
// its row and its version. Whoever commits first wins; the others re-read.
func (s *Store) DecideSignal(ctx context.Context, req runtime.SignalRequest, now time.Time) (runtime.SignalResult, error) {
	if err := req.Validate(); err != nil {
		return runtime.SignalResult{}, err
	}
	if err := checkIdentityBounds(req.Identity); err != nil {
		return runtime.SignalResult{}, err
	}
	if utf8.RuneCountInString(req.Actor) > maxActorChars {
		return runtime.SignalResult{}, invalid("a signal actor is at most %d characters", maxActorChars)
	}
	now = micro(now)
	scope := req.Identity.Scope
	var out runtime.SignalResult
	err := s.update(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, stmtLockSignalIdentity, string(req.Identity.ID())); err != nil {
			return fmt.Errorf("runstate/postgres: lock the signal identity: %w", err)
		}
		prior, err := lookupSignal(ctx, tx, req.Identity)
		if err != nil {
			return err
		}
		recs := map[domain.RunID]runtime.RunRecord{}
		var waits []runtime.Wait
		if !prior.Found() {
			ids, err := candidateRuns(ctx, tx, req)
			if err != nil {
				return err
			}
			for _, id := range ids {
				rec, err := loadRecord(ctx, tx, scope, id, true)
				if err != nil {
					if hasCode(err, domain.CodeNotFound) {
						continue
					}
					return err
				}
				recs[id] = rec
				waits = append(waits, rec.Waits...)
			}
		}
		// The shell supplies authority before routing; the store passes none.
		o, err := runtime.RouteSignal(req, prior, waits, nil, now)
		if err != nil {
			return err
		}
		out = o.Result
		if o.Record == nil && o.Tombstone == nil && o.Wait == nil {
			return nil // a replay of a recorded identity writes nothing
		}
		var events []runtime.Event
		if o.Wait != nil {
			w := *o.Wait
			rec := recs[w.RunID]
			wi := slices.IndexFunc(rec.Waits, func(x runtime.Wait) bool { return x.ID == w.ID && x.StepKey == w.StepKey })
			si := slices.IndexFunc(rec.Run.Steps, func(x runtime.Step) bool { return x.Key == w.StepKey })
			if wi < 0 || si < 0 || rec.Waits[wi].Version+1 != w.Version {
				return versionConflict("wait %s changed during routing", w.ID)
			}
			if err := writeStep(ctx, tx, scope, w.RunID, rec.Run.Steps[si], &w, nil, now); err != nil {
				return err
			}
			rec.Waits[wi] = w
			recs[w.RunID] = rec
			events = append(events, decisionEvent(w, now))
		}
		switch {
		case o.Record != nil:
			if err := insertSignal(ctx, tx, runtime.SignalLookup{Record: o.Record}); err != nil {
				return err
			}
			if !o.Record.Applied {
				t := o.Record.Target
				version := o.Record.Decision.Version
				if version == 0 {
					if rec, ok := recs[t.RunID]; ok {
						if i := slices.IndexFunc(rec.Waits, func(x runtime.Wait) bool { return x.ID == t.WaitID }); i >= 0 {
							version = rec.Waits[i].Version
						}
					}
				}
				events = append(events, runtime.Event{ID: "signal.refused:" + string(o.Record.ID), Scope: scope,
					RunID: t.RunID, StepKey: t.StepKey, Kind: runtime.EventSignalRefused, Version: version, At: now})
			}
		case o.Tombstone != nil:
			// A targetless refusal has no run to attach an event to; its durable
			// tombstone is the record.
			if err := insertSignal(ctx, tx, runtime.SignalLookup{Tombstone: o.Tombstone}); err != nil {
				return err
			}
		}
		return emit(ctx, tx, events)
	})
	if err != nil {
		return runtime.SignalResult{}, err
	}
	return out, nil
}

// candidateRuns returns, in a fixed order, the runs a signal can reach: the run
// of the wait it names, or the runs with an open wait whose complete correlation
// equals the request's. The caller locks them in this order.
func candidateRuns(ctx context.Context, tx pgx.Tx, req runtime.SignalRequest) ([]domain.RunID, error) {
	scope := req.Identity.Scope
	var rows pgx.Rows
	var err error
	if req.WaitID != "" {
		rows, err = tx.Query(ctx, stmtSignalRunsByWait, scope.TenantID, scope.ProjectID, req.WaitID)
	} else {
		rows, err = tx.Query(ctx, stmtSignalRunsByCorrelation, scope.TenantID, scope.ProjectID, jsonOf(req.Correlation))
	}
	if err != nil {
		return nil, fmt.Errorf("runstate/postgres: find the runs of a signal: %w", err)
	}
	defer rows.Close()
	var ids []domain.RunID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("runstate/postgres: scan run: %w", err)
		}
		ids = append(ids, domain.RunID(id))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.Sort(ids)
	return ids, nil
}

func decisionEvent(w runtime.Wait, now time.Time) runtime.Event {
	kind := runtime.EventWaitResolved
	if w.State == runtime.WaitExpired {
		kind = runtime.EventWaitExpired
	}
	return runtime.Event{ID: fmt.Sprintf("%s:%s:%d", kind, w.ID, w.Version), Scope: w.Scope, RunID: w.RunID,
		StepKey: w.StepKey, Kind: kind, Version: w.Version, At: now}
}

// insertSignal writes the durable record or tombstone of one signal identity.
func insertSignal(ctx context.Context, tx pgx.Tx, l runtime.SignalLookup) error {
	if r := l.Record; r != nil {
		var refusal, kind, outcome, answer, actor, version, decided, reason any
		if r.Refusal != "" {
			refusal = string(r.Refusal)
		}
		if d := r.Decision; d != (runtime.Decision{}) {
			kind, outcome, answer, actor = string(d.Kind), nz(d.Outcome), nz(d.Answer), nz(d.Actor)
			version, decided, reason = int64(d.Version), d.At, nz(string(d.Reason))
		}
		_, err := tx.Exec(ctx, stmtInsertSignal, r.Identity.Scope.TenantID, r.Identity.Scope.ProjectID, r.Identity.Caller, r.Identity.Key,
			string(r.ID), "decision", strings.TrimPrefix(r.Digest, digestPrefix), r.Applied, refusal, r.Target.RunID, r.Target.StepKey,
			r.Target.WaitID, kind, outcome, answer, actor, version, decided, reason, r.RecordedAt)
		if err != nil {
			return fmt.Errorf("runstate/postgres: record signal %s: %w", r.ID, err)
		}
		return nil
	}
	t := l.Tombstone
	_, err := tx.Exec(ctx, stmtInsertSignal, t.Identity.Scope.TenantID, t.Identity.Scope.ProjectID, t.Identity.Caller, t.Identity.Key,
		string(t.ID), "tombstone", strings.TrimPrefix(t.Digest, digestPrefix), false, string(t.Refusal), nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, t.RecordedAt)
	if err != nil {
		return fmt.Errorf("runstate/postgres: record signal %s: %w", t.ID, err)
	}
	return nil
}

func hasCode(err error, code domain.ErrorCode) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == code
}
