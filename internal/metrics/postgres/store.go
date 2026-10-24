// Package postgres is the PostgreSQL adapter of the metrics module: the
// history-derived flow views (T042_001_flow_intervals.sql) and the batch summary
// query behind metrics.SummaryReader.
//
// Every read runs inside storage.WithTenantOptions under the non-owner runtime
// role, read-only and repeatable-read, as a single statement, with explicit
// tenant, project and item predicates in addition to forced row level security
// (the views are security invoker). The adapter writes nothing.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/metrics"
	"github.com/optimiware/kodama/internal/storage"
)

// Store implements metrics.SummaryReader over PostgreSQL.
type Store struct{ db storage.TxBeginner }

var _ metrics.SummaryReader = (*Store)(nil)

// NewStore requires the database: there is no in-memory fallback.
func NewStore(db storage.TxBeginner) (*Store, error) {
	if db == nil {
		return nil, errors.New("metrics/postgres: a database is required")
	}
	return &Store{db: db}, nil
}

// maxMicros keeps converted durations inside time.Duration.
const maxMicros = int64(math.MaxInt64 / int64(time.Microsecond))

// ReadSummaries implements metrics.SummaryReader. It returns the summaries of
// the items that exist in the scope (others are omitted without distinction) or
// an error and nothing else.
func (s *Store) ReadSummaries(ctx context.Context, scope domain.Scope, ids []domain.WorkItemID, w metrics.Window) ([]metrics.WorkSummary, error) {
	if s == nil || s.db == nil {
		return nil, metrics.Unavailablef(nil, "the database is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, metrics.Invalidf("%v", err)
	}
	if !w.Valid() {
		return nil, metrics.Invalidf("an observation window is required")
	}
	if len(ids) == 0 {
		return []metrics.WorkSummary{}, nil // never "all work"
	}
	if len(ids) > metrics.MaxBatch {
		return nil, metrics.Invalidf("at most %d work items can be summarized at once", metrics.MaxBatch)
	}
	text := make([]string, len(ids))
	for i, id := range ids {
		if err := id.Validate(); err != nil {
			return nil, metrics.Invalidf("work item id: %v", err)
		}
		text[i] = string(id)
	}

	var out []metrics.WorkSummary
	err := storage.WithTenantOptions(ctx, s.db, scope.TenantID,
		pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, summarySQL, string(scope.TenantID), string(scope.ProjectID), text, w.From, w.To)
			if err != nil {
				return err
			}
			defer rows.Close()
			var got []metrics.WorkSummary
			for rows.Next() {
				sum, err := scan(rows, scope, w)
				if err != nil {
					return err
				}
				got = append(got, sum)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			out = got
			return nil
		})
	if err != nil {
		return nil, classify(err)
	}
	if out == nil {
		out = []metrics.WorkSummary{}
	}
	return out, nil
}

func scan(rows pgx.Rows, scope domain.Scope, w metrics.Window) (metrics.WorkSummary, error) {
	var (
		id                                                         string
		anomaly, stateEnd                                          *string
		observed                                                   int64
		leadUS, activeUS, reviewUS, blockedUS, reopenedUS, complUS int64
		reopens, runs, completed, cancelled                        int64
		inFlight, openEnd                                          bool
	)
	if err := rows.Scan(&id, &anomaly, &observed, &leadUS, &activeUS, &reviewUS, &blockedUS, &reopenedUS,
		&reopens, &runs, &completed, &cancelled, &complUS, &inFlight, &openEnd, &stateEnd); err != nil {
		return metrics.WorkSummary{}, fmt.Errorf("metrics/postgres: scan summary: %w", err)
	}
	sum := metrics.WorkSummary{Scope: scope, ItemID: domain.WorkItemID(id), Window: w}
	if anomaly != nil {
		// A broken chain yields no figures: nothing negative, nothing invented.
		sum.Status, sum.Anomaly = metrics.StatusInvalidHistory, *anomaly
		return sum, nil
	}
	if observed == 0 {
		sum.Status = metrics.StatusNotStarted
		return sum, nil
	}
	for _, us := range []int64{leadUS, activeUS, reviewUS, blockedUS, reopenedUS, complUS} {
		if us < 0 || us > maxMicros {
			return metrics.WorkSummary{}, metrics.Unavailablef(nil, "a recorded interval is outside the representable range")
		}
	}
	if stateEnd == nil {
		return metrics.WorkSummary{}, metrics.Unavailablef(nil, "history has no state at the window end")
	}
	sum.Status = metrics.StatusOK
	sum.ObservedVersion = domain.Version(observed)
	sum.Lead = micros(leadUS)
	sum.Active = micros(activeUS)
	sum.Review = micros(reviewUS)
	sum.Blocked = micros(blockedUS)
	sum.Reopened = micros(reopenedUS)
	sum.CompletedLead = micros(complUS)
	sum.ReopenCount, sum.BlockedIntervals = int(reopens), int(runs)
	sum.CompletedEpisodes, sum.CancelledEpisodes = int(completed), int(cancelled)
	sum.InFlightAtStart, sum.OpenAtEnd = inFlight, openEnd
	sum.StateAtEnd = metrics.State(*stateEnd)
	return sum, nil
}

func micros(us int64) time.Duration { return time.Duration(us) * time.Microsecond }

// classify maps infrastructure failures to ErrUnavailable. The statement is a
// read-only snapshot, so every outage is safely retryable after fresh
// authorization.
func classify(err error) error {
	if err == nil || errors.Is(err, metrics.ErrUnavailable) || errors.Is(err, metrics.ErrInvalid) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return metrics.Unavailablef(err, "the request ended before flow summaries were read")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return metrics.Unavailablef(err, "the database could not complete the read")
	}
	return metrics.Unavailablef(err, "flow summaries could not be read")
}
