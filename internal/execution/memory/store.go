package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/memdb"
)

// Store implements execution.Store over memdb. One method call is one
// memdb.Update (or View): it commits all of its rows, or none.
type Store struct {
	db    *memdb.Store
	peers Peers
	opts  Options
}

var _ execution.Store = (*Store)(nil)

// NewStore requires the kernel and every peer join but the grant revoker.
func NewStore(db *memdb.Store, peers Peers, opts Options) (*Store, error) {
	if db == nil || peers.Authority == nil || peers.Work == nil || peers.Workspaces == nil || peers.Secrets == nil || peers.Events == nil {
		return nil, fmt.Errorf("%w: kernel, authority, work, workspace, secret and event bindings are required", execution.ErrNotConfigured)
	}
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Now().UTC() }
	}
	if opts.NewID == nil {
		opts.NewID = func(prefix string) string {
			b := make([]byte, 12)
			if _, err := rand.Read(b); err != nil {
				panic(err)
			}
			return prefix + "_" + hex.EncodeToString(b)
		}
	}
	return &Store{db: db, peers: peers, opts: opts}, nil
}

func (s *Store) now() time.Time { return s.opts.Clock().UTC().Truncate(time.Microsecond) }

// nextSeq allocates the next sort key of an append-only row from the kernel's
// transactional state: a rolled back update gives its numbers back, and a
// second Store over the same kernel never reuses a key.
func nextSeq(tx memdb.Tx) (string, error) {
	cur, _ := seqT.Get(tx, "rows")
	cur.Name = "rows"
	cur.N++
	if err := seqT.Put(tx, cur); err != nil {
		return "", err
	}
	return fmt.Sprintf("%016d", cur.N), nil
}

// inTenant runs fn in one update and classifies its failure. A failure after fn
// returned nil came from the commit: whether it applied is unknown for the
// caller, so it is unavailable and never an acknowledgment.
func (s *Store) inTenant(ctx context.Context, tenant domain.TenantID, fn func(tx memdb.Tx) error) error {
	completed := false
	err := s.db.Update(ctx, tenant, func(tx memdb.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		completed = true
		return nil
	})
	if err != nil && completed {
		return execution.Unavailablef(err, "the commit outcome is unknown")
	}
	return classify(err)
}

func (s *Store) inTenantRead(ctx context.Context, tenant domain.TenantID, fn func(tx memdb.Tx) error) error {
	return classify(s.db.View(ctx, tenant, fn))
}

// classify maps kernel failures to the module's classes. Business errors pass
// through.
func classify(err error) error {
	if err == nil || execution.IsBusinessError(err) {
		return err
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return execution.Unavailablef(err, "the request ended before the outcome was known")
	case errors.Is(err, domain.ErrConflict):
		return fmt.Errorf("%w: a unique key is already held", execution.ErrConflict)
	case errors.Is(err, domain.ErrInvalidValue):
		return fmt.Errorf("%w: %v", execution.ErrInvalid, err)
	}
	return err
}

// ---- reads ----

func (s *Store) loadRun(tx memdb.Tx, scope domain.Scope, id domain.RunID) (*execution.Run, error) {
	r, ok := runsT.Get(tx, runKey(scope, id))
	if !ok {
		return nil, execution.ErrNotFound
	}
	c := cloneRun(r)
	return &c, nil
}

// loadAggregate reads a run with its steps, attempts and bookkeeping, as copies.
func (s *Store) loadAggregate(tx memdb.Tx, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error) {
	run, err := s.loadRun(tx, scope, id)
	if err != nil {
		return nil, err
	}
	agg := &execution.Aggregate{Run: run, Controls: map[domain.AttemptID]*execution.Control{}}
	for _, row := range collect(tx, stepsT, func(r stepRow) bool { return r.Project == scope.ProjectID && r.Step.RunID == id }) {
		st, err := cloneStep(row.Step)
		if err != nil {
			return nil, err
		}
		agg.Steps = append(agg.Steps, &st)
	}
	sort.Slice(agg.Steps, func(i, j int) bool { return agg.Steps[i].Ordinal < agg.Steps[j].Ordinal })
	for _, a := range collect(tx, attemptsT, func(a execution.Attempt) bool { return a.Scope == scope && a.RunID == id }) {
		a := cloneAttempt(a)
		agg.Attempts = append(agg.Attempts, &a)
	}
	sort.Slice(agg.Attempts, func(i, j int) bool {
		if agg.Attempts[i].StepID != agg.Attempts[j].StepID {
			return agg.Attempts[i].StepID < agg.Attempts[j].StepID
		}
		return agg.Attempts[i].Ordinal < agg.Attempts[j].Ordinal
	})
	for _, c := range collect(tx, controlsT, func(c controlRow) bool { return c.Project == scope.ProjectID && c.Run == id }) {
		ctl := cloneControl(c.Control)
		agg.Controls[ctl.AttemptID] = &ctl
	}
	return agg, nil
}

// Get implements execution.Store (a consistent read).
func (s *Store) Get(ctx context.Context, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error) {
	if scope.Validate() != nil || id.Validate() != nil {
		return nil, execution.ErrNotFound
	}
	var out *execution.Aggregate
	err := s.inTenantRead(ctx, scope.TenantID, func(tx memdb.Tx) (err error) {
		out, err = s.loadAggregate(tx, scope, id)
		return err
	})
	return out, err
}

// List implements execution.Store, ordered by (created_at, id).
func (s *Store) List(ctx context.Context, scope domain.Scope, q execution.ListQuery) ([]*execution.Run, error) {
	if scope.Validate() != nil {
		return nil, execution.ErrNotFound
	}
	var out []*execution.Run
	err := s.inTenantRead(ctx, scope.TenantID, func(tx memdb.Tx) error {
		statuses := map[execution.RunStatus]bool{}
		for _, st := range q.Statuses {
			statuses[st] = true
		}
		rows := collect(tx, runsT, func(r execution.Run) bool {
			switch {
			case r.Scope != scope:
				return false
			case len(statuses) > 0 && !statuses[r.Status]:
				return false
			case q.WorkItemID != "" && r.WorkItemID != q.WorkItemID:
				return false
			case q.After != nil:
				if !r.CreatedAt.Equal(q.After.CreatedAt) {
					return r.CreatedAt.After(q.After.CreatedAt)
				}
				return r.ID > q.After.ID
			}
			return true
		})
		sort.Slice(rows, func(i, j int) bool {
			if !rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
				return rows[i].CreatedAt.Before(rows[j].CreatedAt)
			}
			return rows[i].ID < rows[j].ID
		})
		if q.Limit > 0 && len(rows) > q.Limit {
			rows = rows[:q.Limit]
		}
		for i := range rows {
			r := cloneRun(rows[i])
			out = append(out, &r)
		}
		return nil
	})
	return out, err
}

// RunOf implements execution.Store.
func (s *Store) RunOf(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (domain.RunID, error) {
	if scope.Validate() != nil || attempt.Validate() != nil {
		return "", execution.ErrNotFound
	}
	var id domain.RunID
	err := s.inTenantRead(ctx, scope.TenantID, func(tx memdb.Tx) error {
		a, ok := attemptsT.Get(tx, key(string(scope.ProjectID), string(attempt)))
		if !ok {
			return execution.ErrNotFound
		}
		id = a.RunID
		return nil
	})
	return id, err
}

// scanRuns lists the tenant's runs that keep accepts, oldest first by
// (created_at, project, id), strictly after the cursor run.
func (s *Store) scanRuns(tx memdb.Tx, after execution.RunRef, limit int, keep func(execution.Run) bool) ([]execution.RunRef, error) {
	if limit < 1 || limit > 1000 {
		return nil, execution.Invalidf("limit must be between 1 and 1000")
	}
	var cursor *execution.Run
	if after.ID != "" {
		r, ok := runsT.Get(tx, runKey(after.Scope, after.ID))
		if !ok {
			return nil, execution.ErrNotFound
		}
		cursor = &r
	}
	rows := collect(tx, runsT, func(r execution.Run) bool {
		if !keep(r) {
			return false
		}
		return cursor == nil || runAfter(r, *cursor)
	})
	sort.Slice(rows, func(i, j int) bool { return runAfter(rows[j], rows[i]) })
	if len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]execution.RunRef, len(rows))
	for i, r := range rows {
		out[i] = execution.RunRef{Scope: r.Scope, ID: r.ID}
	}
	return out, nil
}

// runAfter orders runs by (created_at, project, id).
func runAfter(r, than execution.Run) bool {
	switch {
	case !r.CreatedAt.Equal(than.CreatedAt):
		return r.CreatedAt.After(than.CreatedAt)
	case r.Scope.ProjectID != than.Scope.ProjectID:
		return r.Scope.ProjectID > than.Scope.ProjectID
	}
	return r.ID > than.ID
}

// Dispatchable implements execution.Store: nonterminal runs that accept
// dispatch and have a pending, due step, oldest first. The claim rechecks
// everything again under its own transaction.
func (s *Store) Dispatchable(ctx context.Context, tenant domain.TenantID, after execution.RunRef, limit int) ([]execution.RunRef, error) {
	var out []execution.RunRef
	err := s.inTenantRead(ctx, tenant, func(tx memdb.Tx) (err error) {
		now := s.now()
		due := map[string]bool{}
		for _, row := range collect(tx, stepsT, func(r stepRow) bool {
			return r.Step.Status == execution.StepPending && (r.Step.NextAttemptAt == nil || !r.Step.NextAttemptAt.After(now))
		}) {
			due[key(string(row.Project), string(row.Step.RunID))] = true
		}
		out, err = s.scanRuns(tx, after, limit, func(r execution.Run) bool {
			return (r.Status == execution.RunQueued || r.Status == execution.RunRunning) &&
				r.CancelRequestedAt == nil && r.FailureIntentAt == nil &&
				(r.SourceRetention == execution.RetentionConfirmed || r.SourceRetention == execution.RetentionNotRequired) &&
				due[runKey(r.Scope, r.ID)]
		})
		return err
	})
	return out, err
}

// Active implements execution.Store.
func (s *Store) Active(ctx context.Context, tenant domain.TenantID, after execution.RunRef, limit int) ([]execution.RunRef, error) {
	var out []execution.RunRef
	err := s.inTenantRead(ctx, tenant, func(tx memdb.Tx) (err error) {
		out, err = s.scanRuns(tx, after, limit, func(r execution.Run) bool {
			return r.Status == execution.RunQueued || r.Status == execution.RunRunning
		})
		return err
	})
	return out, err
}

// ---- removal ----

// Removable implements execution.Store: settled attempts whose workload removal
// is not recorded and whose run is pinned to backend, oldest settlement first.
func (s *Store) Removable(ctx context.Context, tenant domain.TenantID, backend string, after execution.RemovalCursor, limit int) ([]execution.Removal, error) {
	if limit < 1 || limit > 1000 {
		return nil, execution.Invalidf("limit must be between 1 and 1000")
	}
	var out []execution.Removal
	err := s.inTenantRead(ctx, tenant, func(tx memdb.Tx) error {
		removed := map[string]bool{}
		for _, r := range collect(tx, removalsT, nil) {
			removed[key(string(r.Project), string(r.Attempt))] = true
		}
		pinned := map[string]bool{}
		for _, r := range collect(tx, runsT, func(r execution.Run) bool { return r.Snapshot.Runtime != nil && r.Snapshot.Runtime.Backend == backend }) {
			pinned[runKey(r.Scope, r.ID)] = true
		}
		var rows []execution.Removal
		for _, row := range collect(tx, controlsT, func(c controlRow) bool { return c.Control.SettledAt != nil }) {
			if removed[key(string(row.Project), string(row.Control.AttemptID))] || !pinned[key(string(row.Project), string(row.Run))] {
				continue
			}
			a, ok := attemptsT.Get(tx, key(string(row.Project), string(row.Control.AttemptID)))
			if !ok {
				continue
			}
			rem := execution.Removal{
				Workload:  execution.Workload{Scope: a.Scope, AttemptID: a.ID, Name: a.WorkloadName, UID: a.WorkloadUID},
				Backend:   backend,
				SettledAt: row.Control.SettledAt.UTC(),
			}
			if after.AttemptID != "" && !removalAfter(rem.Cursor(), after) {
				continue
			}
			rows = append(rows, rem)
		}
		sort.Slice(rows, func(i, j int) bool { return removalAfter(rows[j].Cursor(), rows[i].Cursor()) })
		if len(rows) > limit {
			rows = rows[:limit]
		}
		out = rows
		return nil
	})
	return out, err
}

func removalAfter(c, than execution.RemovalCursor) bool {
	switch {
	case !c.SettledAt.Equal(than.SettledAt):
		return c.SettledAt.After(than.SettledAt)
	case c.ProjectID != than.ProjectID:
		return c.ProjectID > than.ProjectID
	}
	return c.AttemptID > than.AttemptID
}

// RecordRemoval implements execution.Store. Recording an already recorded
// removal changes nothing; an unsettled attempt has no removable workload.
func (s *Store) RecordRemoval(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) error {
	if scope.Validate() != nil || attempt.Validate() != nil {
		return execution.ErrNotFound
	}
	return s.inTenant(ctx, scope.TenantID, func(tx memdb.Tx) error {
		c, ok := controlsT.Get(tx, key(string(scope.ProjectID), string(attempt)))
		switch {
		case !ok:
			return execution.ErrNotFound
		}
		if _, done := removalsT.Get(tx, key(string(scope.ProjectID), string(attempt))); done {
			return nil
		}
		if c.Control.SettledAt == nil {
			return fmt.Errorf("%w: the attempt is not settled", execution.ErrInvalidTransition)
		}
		return removalsT.Put(tx, removalRow{Project: scope.ProjectID, Attempt: attempt, At: s.now()})
	})
}
