// Deterministic in-process database seam for the PostgreSQL adapter tests.
//
// No test in this package opens a socket, starts PostgreSQL or runs a
// migration against a server. The fake is the only database these tests ever
// touch: it records every statement and argument, supports injected failures
// before and after an effect, and returns scripted rows through the declared
// store.Rows seam.
package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	testTenantID    = "tenant-1"
	testProjectID   = "project-1"
	testPrincipalID = "principal-1"
	testServiceID   = "service-1"
	testResourceID  = "resource-1"
	testExecutionID = "execution-1"
)

func testProjectScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenantID, ProjectID: testProjectID}
}

func testTenantScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenantID}
}

func testTime() time.Time {
	return time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
}

func testDigest(data []byte) []byte {
	sum := sha256.Sum256(data)
	out := make([]byte, len(sum))
	copy(out, sum[:])
	return out
}

// recordedCall is one statement the adapter issued through the seam.
type recordedCall struct {
	kind string
	sql  string
	args []any
}

// fakeScript is the shared script every physical connection in one test uses.
// Handlers may block on a barrier to create deterministic concurrent
// schedules.
type fakeScript struct {
	mu          sync.Mutex
	execFn      func(sql string, args []any) (store.Result, error)
	queryFn     func(sql string, args []any) (store.Rows, error)
	commitErr   error
	rollbackErr error
}

func (script *fakeScript) exec(sql string, args []any) (store.Result, error) {
	script.mu.Lock()
	handler := script.execFn
	script.mu.Unlock()
	if handler != nil {
		return handler(sql, args)
	}
	return store.Result{RowsAffected: 1}, nil
}

func (script *fakeScript) query(sql string, args []any) (store.Rows, error) {
	script.mu.Lock()
	handler := script.queryFn
	script.mu.Unlock()
	if handler != nil {
		return handler(sql, args)
	}
	return &fakeRows{}, nil
}

func (script *fakeScript) commit() error {
	script.mu.Lock()
	defer script.mu.Unlock()
	return script.commitErr
}

func (script *fakeScript) rollback() error {
	script.mu.Lock()
	defer script.mu.Unlock()
	return script.rollbackErr
}

func (script *fakeScript) setExec(handler func(sql string, args []any) (store.Result, error)) {
	script.mu.Lock()
	defer script.mu.Unlock()
	script.execFn = handler
}

func (script *fakeScript) setQuery(handler func(sql string, args []any) (store.Rows, error)) {
	script.mu.Lock()
	defer script.mu.Unlock()
	script.queryFn = handler
}

// errConnBusy models pgx's refusal to complete or reuse a connection while a
// rows iterator is still being read. Real PostgreSQL returns "conn busy" in
// this case, so the in-process seams must refuse it too instead of accepting an
// adapter that commits mid-iteration.
var errConnBusy = errors.New("conn busy: a rows iterator is still open on the transaction")

// fakeConn is one physical connection handed out by fakeDB.Begin.
type fakeConn struct {
	script *fakeScript

	// openRows counts the iterators still being read on this connection.
	openRows txOpenRows

	mu        sync.Mutex
	calls     []recordedCall
	commits   int
	rollbacks int
}

func (conn *fakeConn) Exec(ctx context.Context, sql string, args ...any) (store.Result, error) {
	conn.record("exec", sql, args)
	if err := ctx.Err(); err != nil {
		return store.Result{}, err
	}
	return conn.script.exec(sql, args)
}

func (conn *fakeConn) Query(ctx context.Context, sql string, args ...any) (store.Rows, error) {
	conn.record("query", sql, args)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := conn.script.query(sql, args)
	if err != nil {
		return nil, err
	}
	return conn.openRows.track(rows), nil
}

func (conn *fakeConn) Commit(ctx context.Context) error {
	if conn.openRows.busy() {
		return errConnBusy
	}
	conn.record("commit", "", nil)
	conn.mu.Lock()
	conn.commits++
	conn.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return conn.script.commit()
}

func (conn *fakeConn) Rollback(ctx context.Context) error {
	if conn.openRows.busy() {
		return errConnBusy
	}
	conn.record("rollback", "", nil)
	conn.mu.Lock()
	conn.rollbacks++
	conn.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return conn.script.rollback()
}

func (conn *fakeConn) record(kind, sql string, args []any) {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	conn.calls = append(conn.calls, recordedCall{kind: kind, sql: sql, args: append([]any(nil), args...)})
}

func (conn *fakeConn) callsCopy() []recordedCall {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	return append([]recordedCall(nil), conn.calls...)
}

func (conn *fakeConn) terminalCalls() int {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	return conn.commits + conn.rollbacks
}

// fakeDB is the DB seam implementation. Each Begin returns a fresh physical
// connection so a pooled-scope test can observe per-transaction state.
type fakeDB struct {
	mu       sync.Mutex
	begins   int
	beginErr error
	script   *fakeScript
	conns    []*fakeConn
}

func newFakeDB() *fakeDB {
	return &fakeDB{script: &fakeScript{}}
}

func (db *fakeDB) Begin(ctx context.Context) (Conn, error) {
	db.mu.Lock()
	db.begins++
	beginErr := db.beginErr
	db.mu.Unlock()
	if beginErr != nil {
		return nil, beginErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn := &fakeConn{script: db.script}
	db.mu.Lock()
	db.conns = append(db.conns, conn)
	db.mu.Unlock()
	return conn, nil
}

func (db *fakeDB) beginCount() int {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.begins
}

func (db *fakeDB) conn(t *testing.T) *fakeConn {
	t.Helper()
	db.mu.Lock()
	defer db.mu.Unlock()
	if len(db.conns) == 0 {
		t.Fatal("the adapter did not open a database connection")
	}
	return db.conns[len(db.conns)-1]
}

func (db *fakeDB) connsCopy() []*fakeConn {
	db.mu.Lock()
	defer db.mu.Unlock()
	return append([]*fakeConn(nil), db.conns...)
}

// fakeRows implements store.Rows over scripted values.
type fakeRows struct {
	rows   [][]any
	index  int
	err    error
	closed bool
}

func (rows *fakeRows) Next() bool {
	if rows.closed {
		return false
	}
	if rows.index+1 >= len(rows.rows) {
		// pgx closes a Rows automatically once Next reports exhaustion, which
		// releases the connection without an explicit Close.
		rows.closed = true
		return false
	}
	rows.index++
	return true
}

func (rows *fakeRows) Scan(dest ...any) error {
	if rows.closed {
		return fmt.Errorf("scan on closed rows")
	}
	if rows.index < 0 || rows.index >= len(rows.rows) {
		return fmt.Errorf("scan without a current row")
	}
	row := rows.rows[rows.index]
	if len(dest) != len(row) {
		return fmt.Errorf("scan %d destinations for %d columns", len(dest), len(row))
	}
	for index := range dest {
		if err := assignValue(dest[index], row[index]); err != nil {
			return err
		}
	}
	return nil
}

func (rows *fakeRows) Err() error { return rows.err }

func (rows *fakeRows) Close() error {
	rows.closed = true
	return nil
}

func (rows *fakeRows) isClosed() bool { return rows.closed }

// txOpenRows counts the row iterators still being read on one transaction. A
// transaction may not commit or roll back while the count is non-zero: pgx
// reports "conn busy" in that case, so the in-process seams must refuse it too.
type txOpenRows struct {
	mu   sync.Mutex
	open int
}

// track wraps one iterator so the transaction sees it as open until it is
// exhausted or closed.
func (tracker *txOpenRows) track(rows store.Rows) store.Rows {
	tracker.mu.Lock()
	tracker.open++
	tracker.mu.Unlock()
	return &trackedRows{inner: rows, release: func() {
		tracker.mu.Lock()
		tracker.open--
		tracker.mu.Unlock()
	}}
}

func (tracker *txOpenRows) busy() bool {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.open > 0
}

// trackedRows releases its transaction slot exactly once, whether the adapter
// closes it or the iterator is exhausted.
type trackedRows struct {
	inner   store.Rows
	release func()
	once    sync.Once
}

func (rows *trackedRows) Next() bool {
	if rows.inner.Next() {
		return true
	}
	// pgx closes Rows when Next reports exhaustion; model that so a drained
	// iterator no longer occupies the connection.
	_ = rows.Close()
	return false
}

func (rows *trackedRows) Scan(dest ...any) error { return rows.inner.Scan(dest...) }

func (rows *trackedRows) Err() error { return rows.inner.Err() }

func (rows *trackedRows) Close() error {
	var err error
	rows.once.Do(func() {
		err = rows.inner.Close()
		rows.release()
	})
	return err
}

// assignValue copies one scripted value into the destination the adapter
// supplied. It accepts the destination types a plain database/sql or pgx
// implementation uses, including nullable wrappers.
func assignValue(dest, value any) error {
	switch target := dest.(type) {
	case *any:
		*target = value
		return nil
	case *string:
		switch typed := value.(type) {
		case nil:
			*target = ""
		case string:
			*target = typed
		case []byte:
			*target = string(typed)
		default:
			return fmt.Errorf("cannot scan %T into string", value)
		}
		return nil
	case *[]byte:
		switch typed := value.(type) {
		case nil:
			*target = nil
		case []byte:
			*target = append([]byte(nil), typed...)
		case [32]byte:
			*target = append([]byte(nil), typed[:]...)
		case string:
			*target = []byte(typed)
		default:
			return fmt.Errorf("cannot scan %T into []byte", value)
		}
		return nil
	case *bool:
		switch typed := value.(type) {
		case bool:
			*target = typed
		case nil:
			*target = false
		default:
			return fmt.Errorf("cannot scan %T into bool", value)
		}
		return nil
	case *int:
		number, err := intValue(value)
		if err != nil {
			return err
		}
		*target = int(number)
		return nil
	case *int64:
		number, err := intValue(value)
		if err != nil {
			return err
		}
		*target = number
		return nil
	case *uint64:
		number, err := intValue(value)
		if err != nil {
			return err
		}
		if number < 0 {
			return fmt.Errorf("cannot scan negative %d into uint64", number)
		}
		*target = uint64(number)
		return nil
	case *time.Time:
		typed, ok := value.(time.Time)
		if !ok {
			return fmt.Errorf("cannot scan %T into time.Time", value)
		}
		*target = typed
		return nil
	case **string:
		if value == nil {
			*target = nil
			return nil
		}
		typed, ok := value.(string)
		if !ok {
			return fmt.Errorf("cannot scan %T into *string", value)
		}
		copied := typed
		*target = &copied
		return nil
	case *[32]byte:
		typed, ok := value.([]byte)
		if !ok || len(typed) != len(target) {
			return fmt.Errorf("cannot scan %T into [32]byte", value)
		}
		copy(target[:], typed)
		return nil
	case *sql.NullString:
		switch typed := value.(type) {
		case nil:
			*target = sql.NullString{}
		case string:
			*target = sql.NullString{String: typed, Valid: true}
		case []byte:
			*target = sql.NullString{String: string(typed), Valid: true}
		default:
			return fmt.Errorf("cannot scan %T into sql.NullString", value)
		}
		return nil
	case *sql.NullTime:
		switch typed := value.(type) {
		case nil:
			*target = sql.NullTime{}
		case time.Time:
			*target = sql.NullTime{Time: typed, Valid: true}
		default:
			return fmt.Errorf("cannot scan %T into sql.NullTime", value)
		}
		return nil
	case *sql.NullInt64:
		switch typed := value.(type) {
		case nil:
			*target = sql.NullInt64{}
		case int64:
			*target = sql.NullInt64{Int64: typed, Valid: true}
		case uint64:
			*target = sql.NullInt64{Int64: int64(typed), Valid: true}
		case int:
			*target = sql.NullInt64{Int64: int64(typed), Valid: true}
		default:
			return fmt.Errorf("cannot scan %T into sql.NullInt64", value)
		}
		return nil
	case *sql.NullBool:
		switch typed := value.(type) {
		case nil:
			*target = sql.NullBool{}
		case bool:
			*target = sql.NullBool{Bool: typed, Valid: true}
		default:
			return fmt.Errorf("cannot scan %T into sql.NullBool", value)
		}
		return nil
	default:
		return fmt.Errorf("unsupported scan destination %T", dest)
	}
}

func intValue(value any) (int64, error) {
	switch typed := value.(type) {
	case int:
		return int64(typed), nil
	case int64:
		return typed, nil
	case uint64:
		if typed > uint64(^uint64(0)>>1) {
			return 0, fmt.Errorf("uint64 %d overflows int64", typed)
		}
		return int64(typed), nil
	case nil:
		return 0, nil
	default:
		return 0, fmt.Errorf("cannot scan %T into integer", value)
	}
}

// singleRow builds one scripted result row whose column order follows the
// adapter's SELECT list. Using the SELECT list keeps the tests independent of
// an incidental column order, while still proving the adapter asked for the
// required columns.
func singleRow(table, sql string, values map[string]any) *fakeRows {
	columns := selectColumns(sql)
	if len(columns) == 0 {
		columns = schemaColumns(table)
	}
	row := make([]any, len(columns))
	for index, column := range columns {
		row[index] = values[column]
	}
	return &fakeRows{rows: [][]any{row}, index: -1}
}

// selectColumns extracts the selected column names from a plain SELECT. An
// adapter that selects an expression or "*" falls back to the schema order.
func selectColumns(sql string) []string {
	lower := strings.ToLower(strings.TrimSpace(sql))
	if !strings.HasPrefix(lower, "select ") {
		return nil
	}
	rest := strings.TrimSpace(sql[len("select "):])
	fromIndex := strings.Index(strings.ToLower(rest), " from ")
	if fromIndex < 0 {
		return nil
	}
	list := strings.TrimSpace(rest[:fromIndex])
	if strings.HasPrefix(strings.ToLower(list), "distinct ") {
		list = strings.TrimSpace(list[len("distinct "):])
	}
	if list == "*" || strings.Contains(list, "*") {
		return nil
	}
	parts := strings.Split(list, ",")
	columns := make([]string, 0, len(parts))
	for _, part := range parts {
		column := strings.TrimSpace(part)
		if column == "" {
			return nil
		}
		if index := strings.IndexAny(column, " \t"); index >= 0 {
			column = column[:index]
		}
		if index := strings.LastIndex(column, "."); index >= 0 {
			column = column[index+1:]
		}
		column = strings.Trim(column, "\"`")
		if column == "" {
			return nil
		}
		columns = append(columns, strings.ToLower(column))
	}
	return columns
}

func schemaColumns(table string) []string {
	for _, candidate := range store.SchemaDesign().Tables {
		if candidate.Name != table {
			continue
		}
		columns := make([]string, len(candidate.Columns))
		for index, column := range candidate.Columns {
			columns[index] = column.Name
		}
		return columns
	}
	return nil
}

// queryByTable routes a scripted SELECT to the named table fixture.
func queryByTable(fixtures map[string]map[string]any) func(sql string, args []any) (store.Rows, error) {
	return func(sql string, args []any) (store.Rows, error) {
		lower := strings.ToLower(sql)
		for table, values := range fixtures {
			if strings.Contains(lower, " from "+table) || strings.Contains(lower, " from public."+table) ||
				strings.Contains(lower, " from \""+table+"\"") {
				return singleRow(table, sql, values), nil
			}
		}
		return &fakeRows{index: -1}, nil
	}
}

func testFingerprint(t *testing.T, body []byte) string {
	t.Helper()
	fingerprint, err := receipt.Fingerprint(receipt.Content{MediaType: "application/json", Bytes: body})
	if err != nil {
		t.Fatalf("fingerprint test content: %v", err)
	}
	return fingerprint
}

func testReceipt(t *testing.T) receipt.Record {
	t.Helper()
	return receipt.Record{
		ID:          "receipt-1",
		Key:         receipt.Key{TenantID: testTenantID, ProjectID: testProjectID, PrincipalID: testPrincipalID, Operation: "task.create", CallerKey: "caller-1"},
		ContentHash: testFingerprint(t, []byte(`{"a":1}`)),
		Result:      []byte(`{"task_id":"task-1"}`),
		ExecutionID: testExecutionID,
		At:          testTime(),
	}
}

func testTenantReceipt(t *testing.T) receipt.Record {
	t.Helper()
	record := testReceipt(t)
	record.Key.ProjectID = ""
	return record
}

func testEvent(scope tenancy.Scope) event.Envelope {
	return event.Envelope{
		ID:             "event-1",
		Type:           event.TypeTaskCreated,
		Source:         event.Source{Kind: event.SourceShell, PrincipalID: testPrincipalID},
		At:             testTime(),
		Scope:          scope,
		SubjectID:      "task-1",
		SubjectVersion: 1,
		Data:           []byte(`{"task_id":"task-1"}`),
	}
}

func testAudit(scope tenancy.Scope) event.Audit {
	return event.Audit{
		ID:        "audit-1",
		Scope:     scope,
		Actor:     testPrincipalID,
		Operation: "task.create",
		At:        testTime(),
		Detail:    "created task",
	}
}

// receiptFixture maps a durable receipt record to its row columns. The content
// hash is decoded to the bytea form the schema stores.
func receiptFixture(t *testing.T, record receipt.Record) map[string]any {
	t.Helper()
	hash := strings.TrimPrefix(record.ContentHash, "sha256:")
	decoded, err := hex.DecodeString(hash)
	if err != nil {
		t.Fatalf("decode receipt content hash: %v", err)
	}
	var project any
	if record.Key.ProjectID != "" {
		project = string(record.Key.ProjectID)
	}
	var execution any
	if record.ExecutionID != "" {
		execution = string(record.ExecutionID)
	}
	var snapshot any
	if record.SnapshotHash != nil {
		snapshot = record.SnapshotHash[:]
	}
	return map[string]any{
		"id":              string(record.ID),
		"tenant_id":       string(record.Key.TenantID),
		"project_id":      project,
		"principal_id":    string(record.Key.PrincipalID),
		"operation":       record.Key.Operation,
		"caller_key":      record.Key.CallerKey,
		"content_sha256":  decoded,
		"result":          record.Result,
		"execution_id":    execution,
		"snapshot_sha256": snapshot,
		"created_at":      record.At,
	}
}

func assertFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %s fault, got nil", want)
	}
	got, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("expected a %s fault, got %T: %v", want, err, err)
	}
	if got != want {
		t.Fatalf("fault class = %s; want %s (%v)", got, want, err)
	}
}

func assertFaultClassAny(t *testing.T, err error, wants ...fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected one of %v faults, got nil", wants)
	}
	got, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("expected one of %v faults, got %T: %v", wants, err, err)
	}
	if slices.Contains(wants, got) {
		return
	}
	t.Fatalf("fault class = %s; want one of %v (%v)", got, wants, err)
}

// argContains reports whether one statement argument carries the wanted value.
func argContains(args []any, want any) bool {
	for _, arg := range args {
		switch expected := want.(type) {
		case []byte:
			if actual, ok := arg.([]byte); ok && bytes.Equal(actual, expected) {
				return true
			}
		default:
			if reflect.DeepEqual(arg, want) {
				return true
			}
		}
	}
	return false
}

// argContainsNumber reports whether one argument carries the numeric value,
// independent of the concrete integer width the adapter chooses.
func argContainsNumber(args []any, want int64) bool {
	for _, arg := range args {
		switch typed := arg.(type) {
		case int:
			if int64(typed) == want {
				return true
			}
		case int64:
			if typed == want {
				return true
			}
		case uint64:
			if typed == uint64(want) {
				return true
			}
		}
	}
	return false
}

func execCalls(calls []recordedCall) []recordedCall {
	filtered := make([]recordedCall, 0, len(calls))
	for _, call := range calls {
		if call.kind == "exec" {
			filtered = append(filtered, call)
		}
	}
	return filtered
}

// dataExecCalls filters out the transaction-local scope statements so a test
// can count only the caller's writes and locks.
func dataExecCalls(calls []recordedCall) []recordedCall {
	filtered := make([]recordedCall, 0, len(calls))
	for _, call := range calls {
		if call.kind != "exec" {
			continue
		}
		if strings.Contains(strings.ToLower(call.sql), "set_config") {
			continue
		}
		filtered = append(filtered, call)
	}
	return filtered
}

func queryCalls(calls []recordedCall) []recordedCall {
	filtered := make([]recordedCall, 0, len(calls))
	for _, call := range calls {
		if call.kind == "query" {
			filtered = append(filtered, call)
		}
	}
	return filtered
}

func containsSQL(calls []recordedCall, fragment string) bool {
	for _, call := range calls {
		if strings.Contains(strings.ToLower(call.sql), strings.ToLower(fragment)) {
			return true
		}
	}
	return false
}

func indexOfSQL(calls []recordedCall, fragment string) int {
	for index, call := range calls {
		if strings.Contains(strings.ToLower(call.sql), strings.ToLower(fragment)) {
			return index
		}
	}
	return -1
}
