package factory

// Test-owned row holds for the real-stack race schedules.
//
// A race that cannot be forced from outside is not a test. The harness holds
// the exact row a serialized commit must lock, on a second PostgreSQL
// connection, and observes through pg_stat_activity that the intended product
// statement is queued behind that hold before it releases it. No sleep decides
// an ordering: the test proves the waiter exists (pg_blocking_pids) and only
// then commits the hold, so the order of the two racing commits is explicit.
//
// The hold uses the runtime role because it is the non-owner role that owns
// every state table's SELECT/UPDATE authority and is confined by forced RLS to
// the harness scope. The observation session is the shard's administrator
// session, which is the documented pg_stat_activity observer in
// test/support/infrastructure/postgres.Shard.RawSession.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/shell/store"
)

// holdTables is the closed set of tables the race tests may hold. A hold on any
// other table is refused instead of interpolating an arbitrary identifier into
// SQL.
var holdTables = map[string]bool{
	"executions": true,
	"actions":    true,
	"tasks":      true,
}

// RowHold is one open transaction that owns the FOR UPDATE locks of the named
// rows. Release commits (or rolls back) it and closes the connection.
type RowHold struct {
	conn     *pgx.Conn
	tx       pgx.Tx
	pid      int32
	released bool
	mu       sync.Mutex
}

// PID is the backend process id of the holding transaction. It is the value
// pg_blocking_pids reports for a queued product statement.
func (hold *RowHold) PID() int32 { return hold.pid }

// Release commits the hold and closes its connection. It is idempotent so a
// test cleanup after an explicit release is harmless.
func (hold *RowHold) Release(ctx context.Context) error {
	if hold == nil {
		return nil
	}
	hold.mu.Lock()
	defer hold.mu.Unlock()
	if hold.released {
		return nil
	}
	hold.released = true
	var errs []error
	if hold.tx != nil {
		if err := hold.tx.Commit(ctx); err != nil {
			errs = append(errs, fmt.Errorf("commit row hold: %w", err))
		}
		hold.tx = nil
	}
	if hold.conn != nil {
		if err := hold.conn.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("close row hold connection: %w", err))
		}
		hold.conn = nil
	}
	return errors.Join(errs...)
}

// HoldExecutionRows locks one execution row and, when named, its action rows in
// one transaction. The order (execution then actions) matches the product's
// lock order, so a product commit blocks at its first lock statement
// instead of deadlocking against the hold.
func (h *Harness) HoldExecutionRows(ctx context.Context, executionID string, actionIDs ...string) (*RowHold, error) {
	targets := []holdTarget{{table: "executions", id: executionID}}
	for _, actionID := range actionIDs {
		targets = append(targets, holdTarget{table: "actions", id: actionID})
	}
	return h.holdRows(ctx, targets...)
}

// HoldTaskRow locks one task row.
func (h *Harness) HoldTaskRow(ctx context.Context, taskID string) (*RowHold, error) {
	return h.holdRows(ctx, holdTarget{table: "tasks", id: taskID})
}

type holdTarget struct {
	table string
	id    string
}

func (h *Harness) holdRows(ctx context.Context, targets ...holdTarget) (*RowHold, error) {
	if len(targets) == 0 {
		return nil, errors.New("factory: a row hold needs at least one target")
	}
	for _, target := range targets {
		if !holdTables[target.table] {
			return nil, fmt.Errorf("factory: table %q is not a race-hold target", target.table)
		}
		if strings.TrimSpace(target.id) == "" {
			return nil, fmt.Errorf("factory: %s row hold needs an identity", target.table)
		}
	}
	conn, err := h.Postgres.RawSession(ctx, store.DefaultRuntimeRole)
	if err != nil {
		return nil, fmt.Errorf("factory: open row-hold session: %w", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		_ = conn.Close(ctx)
		return nil, fmt.Errorf("factory: begin row-hold transaction: %w", err)
	}
	rollback := func(cause error) (*RowHold, error) {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		_ = conn.Close(context.WithoutCancel(ctx))
		return nil, cause
	}
	if _, err := tx.Exec(ctx,
		`SELECT set_config('kodama.tenant_id', $1, true), set_config('kodama.project_id', $2, true)`,
		string(h.TenantID), string(h.ProjectID)); err != nil {
		return rollback(fmt.Errorf("factory: establish row-hold scope: %w", err))
	}
	for _, target := range targets {
		var locked string
		statement := "SELECT id FROM " + target.table + " WHERE id = $1 FOR UPDATE"
		if err := tx.QueryRow(ctx, statement, target.id).Scan(&locked); err != nil {
			return rollback(fmt.Errorf("factory: lock %s %q: %w", target.table, target.id, err))
		}
		if locked != target.id {
			return rollback(fmt.Errorf("factory: locked %s %q, want %q", target.table, locked, target.id))
		}
	}
	var pid int32
	if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		return rollback(fmt.Errorf("factory: read row-hold backend pid: %w", err))
	}
	hold := &RowHold{conn: conn, tx: tx, pid: pid}
	h.T.Cleanup(func() {
		_ = hold.Release(context.Background())
	})
	return hold, nil
}

// BlockedStatement is one product backend queued behind a test hold.
type BlockedStatement struct {
	PID           int32
	WaitEventType string
	WaitEvent     string
	Query         string
	// QueryStart is when the queued statement began. PostgreSQL grants a
	// released row lock to the longest-waiting compatible waiter, so a
	// non-decreasing QueryStart across two observed waiters is the FIFO order
	// the race depends on.
	QueryStart time.Time
	// Blockers is pg_blocking_pids(pid). PostgreSQL reports the waiter ahead in
	// a tuple-lock queue as the blocker of the waiter behind it, so the second
	// racer's direct blocker is the winner, not the hold. The barrier follows
	// this chain to the hold and the race asserts it explicitly.
	Blockers []int32
}

// WaitForBlockedStatement is the race barrier: it waits until a product backend
// in this shard is blocked on a lock held by hold and its current statement
// targets table. It returns as soon as that exact waiter is observed; it never
// releases the hold itself.
func (h *Harness) WaitForBlockedStatement(ctx context.Context, hold *RowHold, table string, timeout time.Duration) (BlockedStatement, error) {
	statements, err := h.WaitForBlockedStatements(ctx, hold, table, 1, timeout)
	if err != nil {
		return BlockedStatement{}, err
	}
	return statements[0], nil
}

// WaitForBlockedStatements is the two-party contention barrier: it waits until
// at least want distinct product backends are blocked behind the same hold and
// statement-target the same table, then returns every observed waiter ordered by
// query start (then backend pid). The caller can therefore assert the exact
// winner/loser queue order instead of relying on a sleep.
func (h *Harness) WaitForBlockedStatements(ctx context.Context, hold *RowHold, table string, want int, timeout time.Duration) ([]BlockedStatement, error) {
	if hold == nil {
		return nil, errors.New("factory: blocked-statement wait needs a hold")
	}
	if !holdTables[table] {
		return nil, fmt.Errorf("factory: table %q is not a race-hold target", table)
	}
	if want < 1 {
		return nil, fmt.Errorf("factory: blocked-statement wait needs a positive waiter count, got %d", want)
	}
	observer, err := h.Postgres.RawSession(ctx, "postgres")
	if err != nil {
		return nil, fmt.Errorf("factory: open pg_stat_activity observer: %w", err)
	}
	defer func() { _ = observer.Close(context.WithoutCancel(ctx)) }()

	deadline := time.Now().Add(timeout)
	var observed []BlockedStatement
	for {
		observed, err = h.blockedStatements(ctx, observer, hold, table)
		if err != nil {
			return nil, err
		}
		if len(observed) >= want {
			return observed, nil
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf(
				"factory: observed %d of %d blocked statements on %s behind hold pid %d within %s; observed=%+v; all lock waiters: %s",
				len(observed), want, table, hold.PID(), timeout, observed, h.lockWaiterSnapshot(ctx, observer))
		}
		// Barrier polling, not a timing decision: the loop exits on the
		// observed waiters or on the explicit timeout.
		time.Sleep(10 * time.Millisecond)
	}
}

// BlockedStatements returns the product backends currently queued behind hold's
// locks with a statement that targets table, ordered by query start.
func (h *Harness) BlockedStatements(ctx context.Context, hold *RowHold, table string) ([]BlockedStatement, error) {
	if hold == nil {
		return nil, errors.New("factory: blocked-statement read needs a hold")
	}
	if !holdTables[table] {
		return nil, fmt.Errorf("factory: table %q is not a race-hold target", table)
	}
	observer, err := h.Postgres.RawSession(ctx, "postgres")
	if err != nil {
		return nil, fmt.Errorf("factory: open pg_stat_activity observer: %w", err)
	}
	defer func() { _ = observer.Close(context.WithoutCancel(ctx)) }()
	return h.blockedStatements(ctx, observer, hold, table)
}

// blockedStatements reads and orders the waiters whose blocking chain reaches
// hold. PostgreSQL reports the waiter ahead of a queued tuple lock as the
// blocker, so the second racer is behind the first racer rather than directly
// behind the hold; the chain is what proves both are queued behind the hold and
// in the observed winner-first order.
func (h *Harness) blockedStatements(ctx context.Context, observer *pgx.Conn, hold *RowHold, table string) ([]BlockedStatement, error) {
	all, blockers, err := h.readLockWaiters(ctx, observer)
	if err != nil {
		return nil, err
	}
	observed := make([]BlockedStatement, 0, 2)
	for _, statement := range all {
		if !statementTargets(statement.Query, table) {
			continue
		}
		if !blockedBy(blockers, statement.PID, hold.PID()) {
			continue
		}
		observed = append(observed, statement)
	}
	sortBlockedStatements(observed)
	return observed, nil
}

// readLockWaiters reads every lock waiter in this database and its direct
// blocker list. The caller filters by the statement it expects and follows the
// blocker chain to the test-owned hold.
func (h *Harness) readLockWaiters(ctx context.Context, observer *pgx.Conn) ([]BlockedStatement, map[int32][]int32, error) {
	rows, err := observer.Query(ctx, `
		SELECT pid, coalesce(wait_event_type, ''), coalesce(wait_event, ''), coalesce(query, ''),
		       coalesce(query_start, 'epoch'::timestamptz), pg_blocking_pids(pid)
		  FROM pg_stat_activity
		 WHERE datname = $1
		   AND wait_event_type = 'Lock'`, h.Postgres.Database)
	if err != nil {
		return nil, nil, fmt.Errorf("factory: observe blocked statements: %w", err)
	}
	defer rows.Close()
	all := make([]BlockedStatement, 0, 4)
	blockers := make(map[int32][]int32, 4)
	for rows.Next() {
		var statement BlockedStatement
		if err := rows.Scan(&statement.PID, &statement.WaitEventType, &statement.WaitEvent, &statement.Query, &statement.QueryStart, &statement.Blockers); err != nil {
			return nil, nil, fmt.Errorf("factory: scan blocked statement: %w", err)
		}
		all = append(all, statement)
		blockers[statement.PID] = statement.Blockers
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("factory: iterate blocked statements: %w", err)
	}
	return all, blockers, nil
}

// sortBlockedStatements orders waiters by query start, then backend pid.
func sortBlockedStatements(observed []BlockedStatement) {
	sort.Slice(observed, func(left, right int) bool {
		if !observed[left].QueryStart.Equal(observed[right].QueryStart) {
			return observed[left].QueryStart.Before(observed[right].QueryStart)
		}
		return observed[left].PID < observed[right].PID
	})
}

// blockedBy reports whether target is reachable from pid through the direct
// blocker graph, with a visited set so a corrupt cycle cannot hang the barrier.
func blockedBy(blockers map[int32][]int32, pid, target int32) bool {
	visited := map[int32]bool{pid: true}
	queue := []int32{pid}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, blocker := range blockers[current] {
			if blocker == target {
				return true
			}
			if !visited[blocker] {
				visited[blocker] = true
				queue = append(queue, blocker)
			}
		}
	}
	return false
}

// lockWaiterSnapshot renders every lock waiter in this database with its direct
// blockers. It is diagnostics for a barrier timeout, never an assertion.
func (h *Harness) lockWaiterSnapshot(ctx context.Context, observer *pgx.Conn) string {
	rows, err := observer.Query(ctx, `
		SELECT pid, pg_blocking_pids(pid), coalesce(query, '')
		  FROM pg_stat_activity
		 WHERE datname = $1 AND wait_event_type = 'Lock'`, h.Postgres.Database)
	if err != nil {
		return "snapshot error: " + err.Error()
	}
	defer rows.Close()
	parts := make([]string, 0, 4)
	for rows.Next() {
		var pid int32
		var blockers []int32
		var query string
		if err := rows.Scan(&pid, &blockers, &query); err != nil {
			return "snapshot scan error: " + err.Error()
		}
		parts = append(parts, fmt.Sprintf("pid=%d blockers=%v query=%q", pid, blockers, query))
	}
	if err := rows.Err(); err != nil {
		return "snapshot iteration error: " + err.Error()
	}
	return strings.Join(parts, "; ")
}

// statementTargets reports whether one pg_stat_activity query is a row lock or
// write against table.
func statementTargets(query, table string) bool {
	return strings.Contains(query, "FROM "+table) || strings.Contains(query, "UPDATE "+table)
}

// MembershipKey is the membership unique-key identity an admission pins: a
// project-scoped caller's absent tenant-wide row is (tenant, NULL, principal),
// and an absent exact-project row is (tenant, project, principal). The empty
// ProjectID renders the SQL NULL of a tenant-wide key; it is never the empty
// string, so a tenant-wide and a project key can never collide.
type MembershipKey struct {
	TenantID    string
	ProjectID   string
	PrincipalID string
}

// membershipKeyText reproduces the store's canonical absence-key text exactly:
// the length-prefixed table name, then tenant_id, project_id and principal_id,
// with a SQL NULL rendered as "n;". Both the absence guard and every insert into
// memberships hash this same text, so a session-level advisory lock on it
// contends with the product's transaction-scoped lock.
func membershipKeyText(key MembershipKey) (string, error) {
	if strings.TrimSpace(key.TenantID) == "" {
		return "", errors.New("factory: membership key needs a tenant")
	}
	if strings.TrimSpace(key.PrincipalID) == "" {
		return "", errors.New("factory: membership key needs a principal")
	}
	const table = "memberships"
	var builder strings.Builder
	fmt.Fprintf(&builder, "t%d:%s;", len(table), table)
	writeMembershipSegment(&builder, key.TenantID)
	if key.ProjectID == "" {
		builder.WriteString("n;")
	} else {
		writeMembershipSegment(&builder, key.ProjectID)
	}
	writeMembershipSegment(&builder, key.PrincipalID)
	return builder.String(), nil
}

// writeMembershipSegment renders one non-null key segment.
func writeMembershipSegment(builder *strings.Builder, value string) {
	fmt.Fprintf(builder, "s%d:%s;", len(value), value)
}

// AdvisoryHold is one open PostgreSQL session that owns a session-level
// advisory lock. Release unlocks it (closing the session also releases it).
type AdvisoryHold struct {
	conn     *pgx.Conn
	pid      int32
	key      string
	released bool
	mu       sync.Mutex
}

// PID is the backend process id of the holding session.
func (hold *AdvisoryHold) PID() int32 { return hold.pid }

// Release unlocks and closes the hold session. It is idempotent so a test
// cleanup after an explicit release is harmless.
func (hold *AdvisoryHold) Release(ctx context.Context) error {
	if hold == nil {
		return nil
	}
	hold.mu.Lock()
	defer hold.mu.Unlock()
	if hold.released {
		return nil
	}
	hold.released = true
	var errs []error
	if hold.conn != nil {
		var unlocked bool
		if err := hold.conn.QueryRow(ctx, "SELECT pg_advisory_unlock(hashtextextended($1, 0))", hold.key).Scan(&unlocked); err != nil {
			errs = append(errs, fmt.Errorf("factory: unlock membership advisory lock: %w", err))
		}
		if err := hold.conn.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("factory: close advisory hold session: %w", err))
		}
		hold.conn = nil
	}
	return errors.Join(errs...)
}

// HoldMembershipAdvisory takes the session-level advisory lock the
// membership absence guard and first grant both take. It uses the runtime role,
// the same role and scope the product transaction uses, and holds the key until
// Release or the test cleanup.
func (h *Harness) HoldMembershipAdvisory(ctx context.Context, key MembershipKey) (*AdvisoryHold, error) {
	text, err := membershipKeyText(key)
	if err != nil {
		return nil, err
	}
	conn, err := h.Postgres.RawSession(ctx, store.DefaultRuntimeRole)
	if err != nil {
		return nil, fmt.Errorf("factory: open advisory-hold session: %w", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1, 0))", text); err != nil {
		_ = conn.Close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("factory: take membership advisory lock: %w", err)
	}
	var pid int32
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		_ = conn.Close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("factory: read advisory-hold backend pid: %w", err)
	}
	hold := &AdvisoryHold{conn: conn, pid: pid, key: text}
	h.T.Cleanup(func() {
		_ = hold.Release(context.Background())
	})
	return hold, nil
}

// BlockedAdvisoryStatements returns the product backends currently queued on the
// membership transaction-scoped advisory lock behind hold, ordered by query
// start.
func (h *Harness) BlockedAdvisoryStatements(ctx context.Context, hold *AdvisoryHold) ([]BlockedStatement, error) {
	if hold == nil {
		return nil, errors.New("factory: blocked-advisory read needs a hold")
	}
	observer, err := h.Postgres.RawSession(ctx, "postgres")
	if err != nil {
		return nil, fmt.Errorf("factory: open pg_stat_activity observer: %w", err)
	}
	defer func() { _ = observer.Close(context.WithoutCancel(ctx)) }()
	return h.advisoryStatements(ctx, observer, hold)
}

// advisoryStatements reads and orders the waiters whose blocking chain reaches
// the advisory hold and whose statement is the product's transaction-scoped
// membership advisory lock.
func (h *Harness) advisoryStatements(ctx context.Context, observer *pgx.Conn, hold *AdvisoryHold) ([]BlockedStatement, error) {
	all, blockers, err := h.readLockWaiters(ctx, observer)
	if err != nil {
		return nil, err
	}
	observed := make([]BlockedStatement, 0, 2)
	for _, statement := range all {
		if !strings.Contains(statement.Query, "pg_advisory_xact_lock") {
			continue
		}
		if !blockedBy(blockers, statement.PID, hold.PID()) {
			continue
		}
		observed = append(observed, statement)
	}
	sortBlockedStatements(observed)
	return observed, nil
}

// WaitForBlockedAdvisoryStatement is the single-waiter barrier for an advisory
// hold.
func (h *Harness) WaitForBlockedAdvisoryStatement(ctx context.Context, hold *AdvisoryHold, timeout time.Duration) (BlockedStatement, error) {
	statements, err := h.WaitForBlockedAdvisoryStatements(ctx, hold, 1, timeout)
	if err != nil {
		return BlockedStatement{}, err
	}
	return statements[0], nil
}

// WaitForBlockedAdvisoryStatements is the two-party advisory barrier: it waits
// until at least want distinct product backends are queued on the same
// membership advisory key, then returns every observed waiter ordered by query
// start.
func (h *Harness) WaitForBlockedAdvisoryStatements(ctx context.Context, hold *AdvisoryHold, want int, timeout time.Duration) ([]BlockedStatement, error) {
	if hold == nil {
		return nil, errors.New("factory: blocked-advisory wait needs a hold")
	}
	if want < 1 {
		return nil, fmt.Errorf("factory: blocked-advisory wait needs a positive waiter count, got %d", want)
	}
	observer, err := h.Postgres.RawSession(ctx, "postgres")
	if err != nil {
		return nil, fmt.Errorf("factory: open pg_stat_activity observer: %w", err)
	}
	defer func() { _ = observer.Close(context.WithoutCancel(ctx)) }()

	deadline := time.Now().Add(timeout)
	var observed []BlockedStatement
	for {
		observed, err = h.advisoryStatements(ctx, observer, hold)
		if err != nil {
			return nil, err
		}
		if len(observed) >= want {
			return observed, nil
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf(
				"factory: observed %d of %d advisory waiters behind hold pid %d within %s; observed=%+v; all lock waiters: %s",
				len(observed), want, hold.PID(), timeout, observed, h.lockWaiterSnapshot(ctx, observer))
		}
		// Barrier polling, not a timing decision: the loop exits on the
		// observed waiters or on the explicit timeout.
		time.Sleep(10 * time.Millisecond)
	}
}

// BlockedStatementsWithoutHold returns every product backend currently queued
// on a row lock or write against table, regardless of which session blocks it.
// It is the loser-quiescence probe: an empty result after a released hold means
// the queued transaction has run its guard and left the queue.
func (h *Harness) BlockedStatementsWithoutHold(ctx context.Context, table string) ([]BlockedStatement, error) {
	if !holdTables[table] {
		return nil, fmt.Errorf("factory: table %q is not a race-hold target", table)
	}
	observer, err := h.Postgres.RawSession(ctx, "postgres")
	if err != nil {
		return nil, fmt.Errorf("factory: open pg_stat_activity observer: %w", err)
	}
	defer func() { _ = observer.Close(context.WithoutCancel(ctx)) }()
	all, _, err := h.readLockWaiters(ctx, observer)
	if err != nil {
		return nil, err
	}
	observed := make([]BlockedStatement, 0, 2)
	for _, statement := range all {
		if statementTargets(statement.Query, table) {
			observed = append(observed, statement)
		}
	}
	sortBlockedStatements(observed)
	return observed, nil
}

// DatabaseTime reads the PostgreSQL statement clock through the administrator
// session. It is the clock the store's deadline guard uses; the harness
// TestClock cannot move it.
func (h *Harness) DatabaseTime(ctx context.Context) (time.Time, error) {
	conn, err := h.Postgres.RawSession(ctx, "postgres")
	if err != nil {
		return time.Time{}, fmt.Errorf("factory: open database-clock session: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	var now time.Time
	if err := conn.QueryRow(ctx, "SELECT statement_timestamp()").Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("factory: read database clock: %w", err)
	}
	return now, nil
}

// WaitForDatabaseTimeAfter waits until the PostgreSQL statement clock is
// strictly after target. The deadline race needs a real wait because the store
// compares the persisted deadline with statement_timestamp(), not with the
// adjustable harness clock.
func (h *Harness) WaitForDatabaseTimeAfter(ctx context.Context, target time.Time, timeout time.Duration) (time.Time, error) {
	conn, err := h.Postgres.RawSession(ctx, "postgres")
	if err != nil {
		return time.Time{}, fmt.Errorf("factory: open database-clock session: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	deadline := time.Now().Add(timeout)
	for {
		var now time.Time
		if err := conn.QueryRow(ctx, "SELECT statement_timestamp()").Scan(&now); err != nil {
			return time.Time{}, fmt.Errorf("factory: read database clock: %w", err)
		}
		if now.After(target) {
			return now, nil
		}
		if !time.Now().Before(deadline) {
			return now, fmt.Errorf("factory: database clock %s did not pass %s within %s", now.UTC(), target.UTC(), timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
