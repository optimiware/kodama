// In-process SQL emulator for the shared seam vectors.
//
// It is not a PostgreSQL implementation. It understands exactly the
// parameterized statement shapes the adapter emits for the controller, outbox,
// credential and receipt/deadline seams, applies them to an in-memory row set
// and exposes the committed state for the vector's inspect steps. No test in
// this file opens a socket or starts a server.
package postgres

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/shell/store"
)

// emuRow is one committed row. values carries every column; tenant_id,
// project_id, id and version are always present.
type emuRow struct {
	values map[string]any
}

func (row *emuRow) clone() *emuRow {
	values := make(map[string]any, len(row.values))
	for column, value := range row.values {
		values[column] = cloneEmuValue(value)
	}
	return &emuRow{values: values}
}

func cloneEmuValue(value any) any {
	if bytes, ok := value.([]byte); ok {
		return append([]byte(nil), bytes...)
	}
	return value
}

// emuDB is the DB seam over the emulator. role models the pool's bound role.
type emuDB struct {
	mu   sync.Mutex
	role string
	now  time.Time
	rows map[string][]*emuRow

	// controllerRLS models the forced-RLS policies of the kodama_controller
	// role for the statement shapes the adapter emits. It is enabled by default
	// for that role, so a controller test cannot pass a statement real
	// PostgreSQL would filter. It is not a general RLS implementation.
	controllerRLS bool
	// onStatement is a deterministic test hook invoked after a select has been
	// evaluated. A test uses it to model a concurrent change between the
	// adapter's statements; production never sets it.
	onStatement func(conn *emuConn, sql string)

	// publishSequence models the durable events_publish_sequence_seq default.
	// Like a PostgreSQL sequence, a value is consumed even by a transaction
	// that later rolls back, so it is stored on the DB, not the transaction.
	publishSequence int64

	begins int
	conns  []*emuConn
}

func newEmuDB(role string, now time.Time) *emuDB {
	return &emuDB{
		role:          role,
		now:           now,
		rows:          make(map[string][]*emuRow),
		controllerRLS: role == store.ControllerRole,
	}
}

func (db *emuDB) Begin(ctx context.Context) (Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	working := make(map[string][]*emuRow, len(db.rows))
	for table, rows := range db.rows {
		copied := make([]*emuRow, len(rows))
		for index, row := range rows {
			copied[index] = row.clone()
		}
		working[table] = copied
	}
	conn := &emuConn{db: db, working: working}
	db.begins++
	db.conns = append(db.conns, conn)
	return conn, nil
}

func (db *emuDB) seed(table string, values map[string]any) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.rows[table] = append(db.rows[table], &emuRow{values: cloneEmuMap(values)})
}

func cloneEmuMap(values map[string]any) map[string]any {
	cloned := make(map[string]any, len(values))
	for column, value := range values {
		cloned[column] = cloneEmuValue(value)
	}
	return cloned
}

// emuConn is one transaction over a copy-on-write row set.
type emuConn struct {
	db      *emuDB
	working map[string][]*emuRow

	// openRows counts the iterators still being read on this transaction. A
	// commit or rollback while one is open is refused, as pgx does.
	openRows txOpenRows

	scopeSet   bool
	tenant     string
	project    string
	credential string

	closed bool
}

func (conn *emuConn) Exec(ctx context.Context, sql string, args ...any) (store.Result, error) {
	if err := ctx.Err(); err != nil {
		return store.Result{}, err
	}
	if conn.closed {
		return store.Result{}, faultInternal("emulator: transaction is closed")
	}
	return conn.exec(sql, args)
}

func (conn *emuConn) Query(ctx context.Context, sql string, args ...any) (store.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if conn.closed {
		return nil, faultInternal("emulator: transaction is closed")
	}
	rows, err := conn.query(sql, args)
	if err != nil {
		return nil, err
	}
	return conn.openRows.track(rows), nil
}

func (conn *emuConn) Commit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if conn.closed {
		return faultInternal("emulator: transaction is closed")
	}
	if conn.openRows.busy() {
		return errConnBusy
	}
	conn.closed = true
	conn.db.mu.Lock()
	defer conn.db.mu.Unlock()
	conn.db.rows = conn.working
	return nil
}

func (conn *emuConn) Rollback(ctx context.Context) error {
	if conn.closed {
		return faultInternal("emulator: transaction is closed")
	}
	if conn.openRows.busy() {
		return errConnBusy
	}
	conn.closed = true
	return nil
}

func (conn *emuConn) exec(sql string, args []any) (store.Result, error) {
	lower := strings.ToLower(strings.TrimSpace(sql))
	switch {
	case strings.HasPrefix(lower, "set transaction"):
		return store.Result{RowsAffected: 1}, nil
	case strings.HasPrefix(lower, "select set_config"):
		if strings.Contains(lower, "kodama.tenant_id") {
			conn.scopeSet = true
			conn.tenant = emuString(args[0])
			conn.project = emuString(args[1])
		}
		if strings.Contains(lower, "kodama.credential_id") {
			conn.credential = emuString(args[0])
		}
		return store.Result{RowsAffected: 1}, nil
	case strings.HasPrefix(lower, "select "):
		rows, err := conn.selectRows(sql, args)
		if err != nil {
			return store.Result{}, err
		}
		conn.observe(sql)
		count := int64(0)
		for rows.Next() {
			count++
		}
		if err := rows.Err(); err != nil {
			return store.Result{}, err
		}
		_ = rows.Close()
		return store.Result{RowsAffected: count}, nil
	case strings.HasPrefix(lower, "insert into "):
		return conn.insert(sql, args)
	case strings.HasPrefix(lower, "update "):
		return conn.update(sql, args)
	case strings.HasPrefix(lower, "delete from "):
		return conn.delete(sql, args)
	default:
		return store.Result{}, faultInternal("emulator: unsupported exec statement: " + sql)
	}
}

func (conn *emuConn) query(sql string, args []any) (store.Rows, error) {
	lower := strings.ToLower(strings.TrimSpace(sql))
	switch {
	case strings.HasPrefix(lower, "select statement_timestamp()"):
		return &fakeRows{rows: [][]any{{conn.db.now}}, index: -1}, nil
	case strings.HasPrefix(lower, "select set_config"):
		conn.exec(sql, args)
		return &fakeRows{rows: [][]any{{"", ""}}, index: -1}, nil
	case strings.HasPrefix(lower, "select "):
		rows, err := conn.selectRows(sql, args)
		if err != nil {
			return nil, err
		}
		conn.observe(sql)
		return rows, nil
	default:
		return nil, faultInternal("emulator: unsupported query statement: " + sql)
	}
}

// observe invokes the test hook after a select has been evaluated. It is a
// deterministic concurrency seam for the adapter's multi-statement
// transactions, never part of the adapter's own behavior.
func (conn *emuConn) observe(sql string) {
	if hook := conn.db.onStatement; hook != nil {
		hook(conn, sql)
	}
}

var insertPattern = regexp.MustCompile(`(?is)^insert\s+into\s+([a-z0-9_"]+)\s*\(([^)]*)\)\s*values\s*\((.*)\)$`)

func (conn *emuConn) insert(sql string, args []any) (store.Result, error) {
	match := insertPattern.FindStringSubmatch(strings.TrimSpace(sql))
	if match == nil {
		return store.Result{}, faultInternal("emulator: unparsable insert: " + sql)
	}
	table := trimIdent(match[1])
	columns := splitTop(match[2], ',')
	values := splitTop(match[3], ',')
	if len(columns) != len(values) {
		return store.Result{}, faultInternal("emulator: insert column/value mismatch")
	}
	row := make(map[string]any, len(columns))
	for index, column := range columns {
		value, err := emuOperand(strings.TrimSpace(values[index]), args)
		if err != nil {
			return store.Result{}, err
		}
		row[trimIdent(column)] = value
	}
	if _, ok := row["version"]; !ok {
		row["version"] = int64(1)
	}
	if table == "events" {
		if _, ok := row["publish_sequence"]; !ok {
			conn.db.mu.Lock()
			conn.db.publishSequence++
			sequence := conn.db.publishSequence
			conn.db.mu.Unlock()
			row["publish_sequence"] = sequence
		}
	}
	conn.working[table] = append(conn.working[table], &emuRow{values: row})
	return store.Result{RowsAffected: 1}, nil
}

var coalescePattern = regexp.MustCompile(`(?i)^coalesce\(\s*(\$\d+)\s*,\s*([a-z0-9_"]+)\s*\)$`)

// emuIsNull reports whether a bound argument is SQL NULL: an untyped nil or a
// nil byte slice, which the driver also sends as NULL.
func emuIsNull(value any) bool {
	if value == nil {
		return true
	}
	bytes, ok := value.([]byte)
	return ok && bytes == nil
}

var updatePattern = regexp.MustCompile(`(?is)^update\s+([a-z0-9_"]+)\s+set\s+(.*?)\s+where\s*(.*)$`)

func (conn *emuConn) update(sql string, args []any) (store.Result, error) {
	match := updatePattern.FindStringSubmatch(strings.TrimSpace(sql))
	if match == nil {
		return store.Result{}, faultInternal("emulator: unparsable update: " + sql)
	}
	table := trimIdent(match[1])
	assignments := splitTop(match[2], ',')
	predicate := match[3]
	if err := validateEmuPredicate(predicate); err != nil {
		return store.Result{}, err
	}
	affected := int64(0)
	for _, row := range conn.working[table] {
		if conn.controllerScopeUpdateTable(table) && !conn.scopePolicyVisible(row) {
			continue
		}
		if !conn.matches(table, row, predicate, args) {
			continue
		}
		for _, assignment := range assignments {
			parts := strings.SplitN(assignment, "=", 2)
			if len(parts) != 2 {
				return store.Result{}, faultInternal("emulator: unparsable assignment " + assignment)
			}
			column := trimIdent(strings.TrimSpace(parts[0]))
			value := strings.TrimSpace(parts[1])
			if strings.EqualFold(value, column+" + 1") {
				row.values[column] = emuInt(row.values[column]) + 1
				continue
			}
			// COALESCE(<operand>, <column>) keeps the stored value when the
			// operand is NULL; it is the only expression form an assignment
			// renders besides a bare operand and the version increment.
			if match := coalescePattern.FindStringSubmatch(value); match != nil {
				resolved, err := emuOperand(match[1], args)
				if err != nil {
					return store.Result{}, err
				}
				if !emuIsNull(resolved) {
					row.values[column] = resolved
				} else if trimIdent(match[2]) != column {
					row.values[column] = row.values[trimIdent(match[2])]
				}
				continue
			}
			resolved, err := emuOperand(value, args)
			if err != nil {
				return store.Result{}, err
			}
			row.values[column] = resolved
		}
		affected++
	}
	return store.Result{RowsAffected: affected}, nil
}

var deletePattern = regexp.MustCompile(`(?is)^delete\s+from\s+([a-z0-9_"]+)\s+where\s*(.*)$`)

func (conn *emuConn) delete(sql string, args []any) (store.Result, error) {
	match := deletePattern.FindStringSubmatch(strings.TrimSpace(sql))
	if match == nil {
		return store.Result{}, faultInternal("emulator: unparsable delete: " + sql)
	}
	table := trimIdent(match[1])
	predicate := match[2]
	if err := validateEmuPredicate(predicate); err != nil {
		return store.Result{}, err
	}
	kept := make([]*emuRow, 0, len(conn.working[table]))
	affected := int64(0)
	for _, row := range conn.working[table] {
		if conn.matches(table, row, predicate, args) {
			affected++
			continue
		}
		kept = append(kept, row)
	}
	conn.working[table] = kept
	return store.Result{RowsAffected: affected}, nil
}

func (conn *emuConn) selectRows(sql string, args []any) (store.Rows, error) {
	lower := strings.ToLower(sql)
	fromIndex := strings.Index(lower, " from ")
	if fromIndex < 0 {
		return nil, faultInternal("emulator: select has no FROM: " + sql)
	}
	selectList := strings.TrimSpace(sql[len("select "):fromIndex])
	rest := strings.TrimSpace(sql[fromIndex+len(" from "):])
	tableToken := rest
	if index := strings.IndexAny(rest, " \t\n"); index >= 0 {
		tableToken = rest[:index]
	}
	table := trimIdent(tableToken)
	rest = strings.TrimSpace(rest[len(tableToken):])
	predicate := ""
	order := ""
	limit := -1
	hasWhere := false
	locking := false
	upper := strings.ToUpper(rest)
	if strings.HasPrefix(upper, "WHERE") {
		after := rest[len("WHERE"):]
		if after == "" || after[0] == ' ' || after[0] == '\t' || after[0] == '\n' {
			hasWhere = true
			predicate = after
		}
	}
	if index := strings.Index(strings.ToUpper(predicate), " FOR UPDATE"); index >= 0 {
		locking = true
		predicate = predicate[:index]
	}
	if index := strings.Index(strings.ToUpper(predicate), " LIMIT "); index >= 0 {
		limitText := strings.TrimSpace(predicate[index+len(" LIMIT "):])
		predicate = predicate[:index]
		if fields := strings.Fields(limitText); len(fields) > 0 {
			if value, err := emuOperand(fields[0], args); err == nil {
				limit = int(emuInt(value))
			}
		}
	}
	if index := strings.Index(strings.ToUpper(predicate), " ORDER BY "); index >= 0 {
		order = predicate[index+len(" ORDER BY "):]
		predicate = predicate[:index]
	}
	if hasWhere {
		if err := validateEmuPredicate(predicate); err != nil {
			return nil, err
		}
	}

	matched := make([]*emuRow, 0)
	for _, row := range conn.working[table] {
		if !conn.rowVisible(table, row) {
			continue
		}
		if locking && !conn.lockPolicyVisible(table, row) {
			continue
		}
		if strings.TrimSpace(predicate) != "" && !conn.matches(table, row, predicate, args) {
			continue
		}
		matched = append(matched, row)
	}
	if strings.TrimSpace(order) != "" {
		terms := splitTop(order, ',')
		sort.SliceStable(matched, func(left, right int) bool {
			for _, term := range terms {
				fields := strings.Fields(strings.TrimSpace(term))
				if len(fields) == 0 {
					continue
				}
				column := trimIdent(fields[0])
				descending := len(fields) > 1 && strings.EqualFold(fields[1], "DESC")
				comparison := emuCompare(matched[left].values[column], matched[right].values[column])
				if comparison == 0 {
					continue
				}
				if descending {
					return comparison > 0
				}
				return comparison < 0
			}
			return false
		})
	}
	if limit >= 0 && len(matched) > limit {
		matched = matched[:limit]
	}
	columns := splitTop(selectList, ',')
	result := make([][]any, len(matched))
	for index, row := range matched {
		values := make([]any, len(columns))
		for columnIndex, column := range columns {
			values[columnIndex] = row.values[trimIdent(strings.TrimSpace(column))]
		}
		result[index] = values
	}
	return &fakeRows{rows: result, index: -1}, nil
}

// rowVisible applies the emulator's light role simulation. The auth role's
// exact_row policy can only see the bound identity; every other role sees the
// rows its explicit table authority allows.
func (conn *emuConn) rowVisible(table string, row *emuRow) bool {
	if conn.db.role != "kodama_auth" {
		return true
	}
	switch table {
	case "executions", "services":
		return conn.credential != "" && emuString(row.values["id"]) == conn.credential
	default:
		return false
	}
}

func (conn *emuConn) matches(table string, row *emuRow, predicate string, args []any) bool {
	return emuMatch(conn, row, predicate, args)
}

// lockPolicyVisible models the controller UPDATE policy's USING expression for
// a SELECT ... FOR UPDATE. PostgreSQL applies the UPDATE policy to a locking
// read in addition to the SELECT policy, so a locking read on a table whose
// UPDATE policy is scope_matches_txn_scope sees only rows in the transaction-
// local scope. Before the scope is set the setting is NULL and no row matches.
func (conn *emuConn) lockPolicyVisible(table string, row *emuRow) bool {
	if !conn.controllerScopeUpdateTable(table) {
		return true
	}
	return conn.scopePolicyVisible(row)
}

// controllerScopeUpdateTable reports whether the controller's UPDATE policy for
// table is scope_matches_txn_scope, so both a locking read and a state UPDATE
// are filtered by the transaction-local scope.
func (conn *emuConn) controllerScopeUpdateTable(table string) bool {
	if !conn.db.controllerRLS {
		return false
	}
	switch table {
	case "executions", "actions", "cleanup":
		return true
	default:
		return false
	}
}

// scopePolicyVisible renders scope_matches_txn_scope: the row's tenant and
// project must equal the transaction-local settings. A NULL project_id never
// equals the setting, matching SQL's three-valued comparison.
func (conn *emuConn) scopePolicyVisible(row *emuRow) bool {
	if !conn.scopeSet {
		return false
	}
	if emuString(row.values["tenant_id"]) != conn.tenant {
		return false
	}
	project, ok := row.values["project_id"]
	if !ok || project == nil {
		return false
	}
	return emuString(project) == conn.project
}

// validateEmuPredicate rejects the malformed predicate shapes PostgreSQL would
// refuse but a lenient parser could silently accept: an empty WHERE, a trailing
// or leading AND/OR, and a WHERE immediately followed by another clause. The
// emulator is only as strict as its parser, so it must not turn a syntax error
// into a passing vector.
func validateEmuPredicate(predicate string) error {
	trimmed := strings.TrimSpace(predicate)
	if trimmed == "" {
		return faultInternal("emulator: malformed predicate: empty WHERE clause")
	}
	upper := strings.ToUpper(trimmed)
	for _, suffix := range []string{" AND", " OR"} {
		if strings.HasSuffix(upper, suffix) {
			return faultInternal("emulator: malformed predicate: trailing " + strings.TrimSpace(suffix))
		}
	}
	for _, prefix := range []string{"AND ", "OR "} {
		if strings.HasPrefix(upper, prefix) {
			return faultInternal("emulator: malformed predicate: leading " + strings.TrimSpace(prefix))
		}
	}
	return nil
}

func emuMatch(conn *emuConn, row *emuRow, predicate string, args []any) bool {
	predicate = strings.TrimSpace(predicate)
	if predicate == "" {
		return true
	}
	if parts := splitTopKeyword(predicate, " OR "); len(parts) > 1 {
		for _, part := range parts {
			if emuMatch(conn, row, part, args) {
				return true
			}
		}
		return false
	}
	for _, part := range splitTopKeyword(predicate, " AND ") {
		if !emuTerm(conn, row, part, args) {
			return false
		}
	}
	return true
}

func emuTerm(conn *emuConn, row *emuRow, term string, args []any) bool {
	term = strings.TrimSpace(term)
	for strings.HasPrefix(term, "(") && strings.HasSuffix(term, ")") && balanced(term[1:len(term)-1]) {
		term = strings.TrimSpace(term[1 : len(term)-1])
	}
	upper := strings.ToUpper(term)
	if strings.HasPrefix(upper, "NOT ") {
		inner := strings.TrimSpace(term[len("NOT "):])
		for strings.HasPrefix(inner, "(") && strings.HasSuffix(inner, ")") && balanced(inner[1:len(inner)-1]) {
			inner = strings.TrimSpace(inner[1 : len(inner)-1])
		}
		return !emuMatch(conn, row, inner, args)
	}
	switch {
	case upper == "TRUE":
		return true
	case strings.HasSuffix(upper, " IS NULL"):
		column := trimIdent(strings.TrimSpace(term[:len(term)-len(" IS NULL")]))
		return row.values[column] == nil
	case strings.HasSuffix(upper, " IS NOT NULL"):
		column := trimIdent(strings.TrimSpace(term[:len(term)-len(" IS NOT NULL")]))
		return row.values[column] != nil
	}
	for _, operator := range []string{" IS NOT DISTINCT FROM ", " = ", " > ", " < "} {
		index := strings.Index(upper, operator)
		if index < 0 {
			continue
		}
		column := trimIdent(strings.TrimSpace(term[:index]))
		right := strings.TrimSpace(term[index+len(operator):])
		value, err := emuOperand(right, args)
		if err != nil {
			return false
		}
		left := row.values[column]
		switch operator {
		case " IS NOT DISTINCT FROM ":
			return emuEqual(left, value)
		case " = ":
			if left == nil || value == nil {
				return false
			}
			return emuEqual(left, value)
		case " > ":
			return emuCompare(left, value) > 0
		case " < ":
			return emuCompare(left, value) < 0
		}
	}
	return false
}

func emuOperand(value string, args []any) (any, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "$") {
		index, err := strconv.Atoi(value[1:])
		if err != nil || index < 1 || index > len(args) {
			return nil, faultInternal("emulator: bad placeholder " + value)
		}
		return args[index-1], nil
	}
	if strings.EqualFold(value, "NULL") {
		return nil, nil
	}
	if strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") {
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'"), nil
	}
	if number, err := strconv.ParseInt(value, 10, 64); err == nil {
		return number, nil
	}
	return value, nil
}

func emuString(value any) string {
	if value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	default:
		return fmt.Sprintf("%v", value)
	}
}

func emuInt(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case uint64:
		return int64(typed)
	case float64:
		return int64(typed)
	default:
		return 0
	}
}

func emuEqual(left, right any) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return emuCompare(left, right) == 0
}

func emuCompare(left, right any) int {
	if left == nil || right == nil {
		switch {
		case left == nil && right == nil:
			return 0
		case left == nil:
			return -1
		default:
			return 1
		}
	}
	switch leftValue := left.(type) {
	case string:
		return strings.Compare(leftValue, emuString(right))
	case int64:
		rightValue := emuInt(right)
		switch {
		case leftValue < rightValue:
			return -1
		case leftValue > rightValue:
			return 1
		default:
			return 0
		}
	case bool:
		rightValue, ok := right.(bool)
		if !ok {
			return -1
		}
		if leftValue == rightValue {
			return 0
		}
		if !leftValue {
			return -1
		}
		return 1
	case time.Time:
		rightValue, ok := right.(time.Time)
		if !ok {
			return -1
		}
		switch {
		case leftValue.Before(rightValue):
			return -1
		case leftValue.After(rightValue):
			return 1
		default:
			return 0
		}
	case []byte:
		rightValue, ok := right.([]byte)
		if !ok {
			return -1
		}
		return strings.Compare(string(leftValue), string(rightValue))
	default:
		return strings.Compare(fmt.Sprintf("%v", left), fmt.Sprintf("%v", right))
	}
}

func trimIdent(value string) string {
	return strings.Trim(strings.TrimSpace(value), "\"`")
}

// splitTop splits on a separator that is not inside parentheses or quotes.
func splitTop(value string, separator rune) []string {
	parts := make([]string, 0, 4)
	depth := 0
	quoted := false
	start := 0
	for index, character := range value {
		switch character {
		case '\'':
			quoted = !quoted
		case '(':
			if !quoted {
				depth++
			}
		case ')':
			if !quoted {
				depth--
			}
		default:
			if character == separator && depth == 0 && !quoted {
				parts = append(parts, strings.TrimSpace(value[start:index]))
				start = index + 1
			}
		}
	}
	parts = append(parts, strings.TrimSpace(value[start:]))
	return parts
}

// splitTopKeyword splits on a keyword outside parentheses and quotes.
func splitTopKeyword(value, keyword string) []string {
	upper := strings.ToUpper(value)
	parts := make([]string, 0, 4)
	depth := 0
	quoted := false
	start := 0
	for index := 0; index < len(value); index++ {
		switch value[index] {
		case '\'':
			quoted = !quoted
		case '(':
			if !quoted {
				depth++
			}
		case ')':
			if !quoted {
				depth--
			}
		}
		if depth != 0 || quoted {
			continue
		}
		if strings.HasPrefix(upper[index:], keyword) {
			parts = append(parts, strings.TrimSpace(value[start:index]))
			start = index + len(keyword)
			index += len(keyword) - 1
		}
	}
	parts = append(parts, strings.TrimSpace(value[start:]))
	return parts
}

func balanced(value string) bool {
	depth := 0
	quoted := false
	for _, character := range value {
		switch character {
		case '\'':
			quoted = !quoted
		case '(':
			if !quoted {
				depth++
			}
		case ')':
			if !quoted {
				depth--
			}
		}
	}
	return depth == 0 && !quoted
}
