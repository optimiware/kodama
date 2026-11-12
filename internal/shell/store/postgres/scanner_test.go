// store.Scanner conformance for the PostgreSQL adapter.
//
// The contract test runner lives in a _test.go file and is not importable, so
// this suite adapts the same testdata/scanner.json vectors. It executes the
// adapter's generated statements against a small in-memory emulation of the
// declared schema and asserts the statement text and bound parameters as well:
// identifiers come from the schema allow-list, every caller value is a
// parameter, the scope predicate is always present, the keyset comparison is
// correct for mixed directions and a page never exceeds its bound.
//
// No test opens a socket or starts PostgreSQL. The existing in-process DB seam
// is the only database these tests touch.
package postgres

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func TestStoreImplementsScanner(t *testing.T) {
	var _ store.Scanner = (*Store)(nil)
}

// --- scanner.json vectors -------------------------------------------------

type scanVectorDocument struct {
	Version int          `json:"version"`
	Vectors []scanVector `json:"vectors"`
}

type scanVector struct {
	Name  string     `json:"name"`
	Kind  string     `json:"kind"`
	Seed  scanSeed   `json:"seed"`
	Steps []scanStep `json:"steps"`
}

type scanSeed struct {
	Rows []scanSeedRow `json:"rows"`
}

type scanSeedRow struct {
	Table     string         `json:"table"`
	TenantID  string         `json:"tenant_id"`
	ProjectID string         `json:"project_id"`
	ID        string         `json:"id"`
	Values    map[string]any `json:"values"`
}

type scanScopeJSON struct {
	TenantID  string `json:"tenant_id"`
	ProjectID string `json:"project_id"`
}

type scanRangeJSON struct {
	Column string `json:"column"`
	After  any    `json:"after"`
	Before any    `json:"before"`
}

type scanOrderJSON struct {
	Column string `json:"column"`
	Desc   bool   `json:"desc"`
}

type scanStep struct {
	Op          string          `json:"op"`
	Scope       *scanScopeJSON  `json:"scope"`
	Table       string          `json:"table"`
	Equals      map[string]any  `json:"equals"`
	Range       []scanRangeJSON `json:"range"`
	OrderBy     []scanOrderJSON `json:"order_by"`
	AfterStep   *int            `json:"after_step"`
	After       string          `json:"after"`
	Limit       int             `json:"limit"`
	Row         *scanSeedRow    `json:"row"`
	WantIDs     []string        `json:"want_ids"`
	WantNext    *bool           `json:"want_next"`
	WantHasMore *bool           `json:"want_has_more"`
	WantError   string          `json:"want_error"`
	WantCount   *int            `json:"want_count"`
}

var scanVectorKinds = map[string]bool{
	"scope": true, "pagination": true, "determinism": true, "cursor": true,
	"limit": true, "invalid_input": true, "readonly": true,
}

func loadScanVectors(t *testing.T) scanVectorDocument {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "test", "contracts", "adapters", "testdata", "scanner.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read scanner conformance vectors: %v", err)
	}
	var document scanVectorDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode scanner conformance vectors: %v", err)
	}
	if document.Version != 1 {
		t.Fatalf("scanner vector version = %d; want 1", document.Version)
	}
	if len(document.Vectors) == 0 {
		t.Fatal("scanner conformance vector set is empty")
	}
	seen := make(map[string]bool)
	for _, vector := range document.Vectors {
		if vector.Name == "" || !scanVectorKinds[vector.Kind] {
			t.Fatalf("scanner vector %+v is incomplete or has an unknown kind", vector)
		}
		if seen[vector.Name] {
			t.Fatalf("duplicate scanner vector %q", vector.Name)
		}
		seen[vector.Name] = true
		if len(vector.Steps) == 0 {
			t.Fatalf("scanner vector %q has no steps", vector.Name)
		}
	}
	return document
}

func scanScope(scope *scanScopeJSON) tenancy.Scope {
	if scope == nil {
		return tenancy.Scope{}
	}
	return tenancy.Scope{TenantID: id.TenantID(scope.TenantID), ProjectID: id.ProjectID(scope.ProjectID)}
}

func scanQueryFromStep(step scanStep) store.ScanQuery {
	query := store.ScanQuery{
		Table:   step.Table,
		Equals:  step.Equals,
		After:   step.After,
		Limit:   step.Limit,
		OrderBy: make([]store.Order, 0, len(step.OrderBy)),
	}
	for _, filter := range step.Range {
		query.Range = append(query.Range, store.RangeFilter{Column: filter.Column, After: filter.After, Before: filter.Before})
	}
	for _, order := range step.OrderBy {
		query.OrderBy = append(query.OrderBy, store.Order{Column: order.Column, Desc: order.Desc})
	}
	return query
}

// TestScannerConformanceVectors runs every adapter scanner vector against the
// PostgreSQL adapter over the fake DB seam, emulating the generated statement
// in memory.
func TestScannerConformanceVectors(t *testing.T) {
	document := loadScanVectors(t)
	for _, vector := range document.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			state := newScanVectorState(t, vector.Seed)
			db := newFakeDB()
			db.script.setQuery(state.query)
			scanner := New(db)
			next := make([]string, len(vector.Steps))
			for index, step := range vector.Steps {
				switch step.Op {
				case "query":
					runScanVectorQuery(t, scanner, db, state, vector, index, step, next)
				case "insert":
					if step.Row == nil {
						t.Fatalf("step %d inserts no row", index)
					}
					state.insert(*step.Row)
				case "inspect":
					if step.WantCount == nil {
						t.Fatalf("step %d inspects without a count", index)
					}
					if got := state.count(step.Table, step.Scope); got != *step.WantCount {
						t.Fatalf("step %d count(%s) = %d; want %d", index, step.Table, got, *step.WantCount)
					}
				default:
					t.Fatalf("step %d has unknown op %q", index, step.Op)
				}
			}
		})
	}
}

func runScanVectorQuery(t *testing.T, scanner *Store, db *fakeDB, state *scanVectorState, vector scanVector, index int, step scanStep, next []string) {
	t.Helper()
	if step.Scope == nil {
		t.Fatalf("step %d queries without a scope", index)
	}
	query := scanQueryFromStep(step)
	if step.AfterStep != nil {
		if *step.AfterStep < 0 || *step.AfterStep >= index {
			t.Fatalf("step %d after_step %d is not a prior step", index, *step.AfterStep)
		}
		query.After = next[*step.AfterStep]
	}
	scope := scanScope(step.Scope)
	before := db.beginCount()
	page, err := scanner.Query(context.Background(), scope, query)
	if step.WantError != "" {
		if err == nil {
			t.Fatalf("step %d succeeded; want error class %q", index, step.WantError)
		}
		class, ok := fault.ClassOf(err)
		if !ok || string(class) != step.WantError {
			t.Fatalf("step %d error = %v (class=%q); want %q", index, err, class, step.WantError)
		}
		if got := db.beginCount(); got != before {
			t.Fatalf("step %d refused query opened %d transactions", index, got-before)
		}
		return
	}
	if err != nil {
		t.Fatalf("step %d: %v", index, err)
	}
	if step.WantIDs != nil {
		got := make([]string, 0, len(page.Rows))
		for _, row := range page.Rows {
			text, _ := row.Values["id"].(string)
			got = append(got, text)
		}
		if !equalStrings(got, step.WantIDs) {
			t.Fatalf("step %d ids = %v; want %v", index, got, step.WantIDs)
		}
	}
	if step.WantHasMore != nil && page.HasMore != *step.WantHasMore {
		t.Fatalf("step %d HasMore = %v; want %v", index, page.HasMore, *step.WantHasMore)
	}
	if step.WantNext != nil && (page.Next != "") != *step.WantNext {
		t.Fatalf("step %d Next = %q; want non-empty=%v", index, page.Next, *step.WantNext)
	}
	if page.HasMore && page.Next == "" {
		t.Fatalf("step %d reports HasMore without a next cursor", index)
	}
	if !page.HasMore && page.Next != "" {
		t.Fatalf("step %d reports no HasMore but a next cursor", index)
	}
	next[index] = page.Next
	assertScanVectorStatement(t, step, scope, query, db.conn(t))
}

// assertScanVectorStatement checks the generated SQL and bound parameters for
// one successful vector step.
func assertScanVectorStatement(t *testing.T, step scanStep, scope tenancy.Scope, query store.ScanQuery, conn *fakeConn) {
	t.Helper()
	calls := conn.callsCopy()
	if conn.commits != 1 || conn.rollbacks != 0 {
		t.Fatalf("scanner terminal calls: commits=%d rollbacks=%d; want one read-only commit", conn.commits, conn.rollbacks)
	}
	if conn.calls[len(conn.calls)-1].kind != "commit" {
		t.Fatalf("scanner ended with %q; want a commit", conn.calls[len(conn.calls)-1].kind)
	}
	queries := queryCalls(calls)
	if len(queries) != 1 {
		t.Fatalf("scanner issued %d queries; want 1", len(queries))
	}
	call := queries[0]
	lower := strings.ToLower(call.sql)
	if !strings.Contains(lower, `from "`+strings.ToLower(step.Table)+`"`) {
		t.Errorf("statement does not select the allow-listed table: %s", call.sql)
	}
	if !strings.Contains(lower, " order by ") || !strings.Contains(lower, `"id"`) {
		t.Errorf("statement is not ordered with an id tie-break: %s", call.sql)
	}
	if !strings.Contains(lower, " limit $") {
		t.Errorf("statement does not bound the page with a parameter: %s", call.sql)
	}
	if !argContainsNumber(call.args, int64(step.Limit+1)) {
		t.Errorf("statement args %v do not carry limit+1 %d", call.args, step.Limit+1)
	}
	if !argContains(call.args, string(scope.TenantID)) {
		t.Errorf("statement args %v do not carry the scope tenant %q", call.args, scope.TenantID)
	}
	meta, _ := lookupTable(step.Table)
	if meta != nil && meta.hasProject {
		if scope.ProjectScoped() && !argContains(call.args, string(scope.ProjectID)) {
			t.Errorf("statement args %v do not carry the scope project %q", call.args, scope.ProjectID)
		}
		if scope.TenantScoped() && !strings.Contains(lower, `"project_id" is null`) {
			t.Errorf("tenant-wide statement does not require project_id IS NULL: %s", call.sql)
		}
	}
	for column, value := range step.Equals {
		if !argContains(call.args, value) {
			t.Errorf("equality %q=%v is not a bound parameter in %v", column, value, call.args)
		}
		if text, ok := value.(string); ok && text != "" && strings.Contains(call.sql, text) {
			t.Errorf("equality value %q was interpolated into SQL: %s", text, call.sql)
		}
	}
	for _, filter := range step.Range {
		if filter.After != nil && !argContains(call.args, filter.After) {
			t.Errorf("range after %v is not a bound parameter in %v", filter.After, call.args)
		}
		if filter.Before != nil && !argContains(call.args, filter.Before) {
			t.Errorf("range before %v is not a bound parameter in %v", filter.Before, call.args)
		}
	}
	for _, forbidden := range []string{"for update", "insert into", "update ", "delete from"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("scanner statement contains a write or lock: %s", call.sql)
		}
	}
	for _, call := range calls {
		if call.kind != "exec" {
			continue
		}
		if strings.Contains(strings.ToLower(call.sql), "set_config") ||
			strings.Contains(strings.ToLower(call.sql), "set transaction read only") {
			continue
		}
		t.Errorf("scanner issued an unexpected exec statement: %s", call.sql)
	}
}

// --- in-memory emulation of the generated scanner statement -----------------

type scanVectorRow struct {
	table  string
	values map[string]any
}

type scanVectorState struct {
	t    *testing.T
	rows []scanVectorRow
}

func newScanVectorState(t *testing.T, seed scanSeed) *scanVectorState {
	t.Helper()
	state := &scanVectorState{t: t}
	for _, row := range seed.Rows {
		state.insert(row)
	}
	return state
}

func (state *scanVectorState) insert(row scanSeedRow) {
	raw := make(map[string]any, len(row.Values)+3)
	maps.Copy(raw, row.Values)
	raw["tenant_id"] = row.TenantID
	raw["project_id"] = row.ProjectID
	raw["id"] = row.ID
	state.rows = append(state.rows, scanVectorRow{
		table:  row.Table,
		values: normalizeScanRow(state.t, row.Table, raw),
	})
}

func (state *scanVectorState) count(table string, scope *scanScopeJSON) int {
	meta, ok := lookupTable(table)
	if !ok {
		return 0
	}
	count := 0
	for _, row := range state.rows {
		if row.table == table && scanRowVisible(meta, row.values, scope) {
			count++
		}
	}
	return count
}

func scanRowVisible(meta *tableMeta, values map[string]any, scope *scanScopeJSON) bool {
	if scope == nil {
		return true
	}
	if meta.hasTenant {
		tenant, _ := values["tenant_id"].(string)
		if tenant != scope.TenantID {
			return false
		}
	}
	if meta.hasProject {
		project, _ := values["project_id"].(string)
		if scope.ProjectID == "" {
			return project == ""
		}
		return project == scope.ProjectID
	}
	switch meta.table.Name {
	case "tenants":
		identity, _ := values["id"].(string)
		return identity == scope.TenantID
	case "projects":
		if scope.ProjectID == "" {
			return true
		}
		identity, _ := values["id"].(string)
		return identity == scope.ProjectID
	}
	return false
}

func normalizeScanRow(t *testing.T, table string, raw map[string]any) map[string]any {
	t.Helper()
	meta, ok := lookupTable(table)
	if !ok {
		t.Fatalf("emulator seed names unknown table %q", table)
	}
	values := make(map[string]any, len(meta.columns))
	for name, column := range meta.columns {
		value, present := raw[name]
		if !present || value == nil || (name == "project_id" && column.Nullable && value == "") {
			if column.Nullable {
				values[name] = nil
			} else {
				values[name] = scanZeroValue(column)
			}
			continue
		}
		values[name] = scanCoerceValue(t, column, value)
	}
	return values
}

func scanZeroValue(column store.Column) any {
	switch column.Type {
	case "text":
		return ""
	case "bigint", "integer":
		return int64(0)
	case "boolean":
		return false
	case "timestamptz":
		return time.Time{}
	case "bytea", "jsonb":
		return []byte{}
	default:
		return nil
	}
}

func scanCoerceValue(t *testing.T, column store.Column, value any) any {
	t.Helper()
	switch column.Type {
	case "text":
		if text, ok := value.(string); ok {
			return text
		}
		return fmt.Sprint(value)
	case "bigint", "integer":
		switch typed := value.(type) {
		case int64:
			return typed
		case int:
			return int64(typed)
		case float64:
			return int64(typed)
		case json.Number:
			number, err := typed.Int64()
			if err != nil {
				t.Fatalf("coerce %v into %s: %v", value, column.Type, err)
			}
			return number
		case string:
			number, err := strconv.ParseInt(typed, 10, 64)
			if err != nil {
				t.Fatalf("coerce %q into %s: %v", typed, column.Type, err)
			}
			return number
		}
	case "timestamptz":
		if typed, ok := value.(time.Time); ok {
			return typed
		}
		if text, ok := value.(string); ok {
			parsed, err := time.Parse(time.RFC3339Nano, text)
			if err != nil {
				t.Fatalf("coerce %q into timestamptz: %v", text, err)
			}
			return parsed
		}
	case "boolean":
		if typed, ok := value.(bool); ok {
			return typed
		}
	case "bytea", "jsonb":
		if typed, ok := value.([]byte); ok {
			return typed
		}
		if text, ok := value.(string); ok {
			return []byte(text)
		}
	}
	t.Fatalf("cannot coerce %T into %s", value, column.Type)
	return nil
}

func (state *scanVectorState) query(sql string, args []any) (store.Rows, error) {
	parsed, err := parseScanSQL(sql)
	if err != nil {
		return nil, fmt.Errorf("emulator cannot parse scanner statement %q: %w", sql, err)
	}
	if parsed.limitParam < 1 || parsed.limitParam > len(args) {
		return nil, fmt.Errorf("emulator limit parameter %d is outside %d args", parsed.limitParam, len(args))
	}
	limit := scanInt(args[parsed.limitParam-1])
	if limit < 0 {
		return nil, fmt.Errorf("emulator limit %d is negative", limit)
	}
	matches := make([]map[string]any, 0, len(state.rows))
	for _, row := range state.rows {
		if row.table != parsed.table {
			continue
		}
		matched, err := parsed.predicate(row.values, args)
		if err != nil {
			return nil, err
		}
		if matched {
			matches = append(matches, row.values)
		}
	}
	sort.SliceStable(matches, func(left, right int) bool {
		return compareScanRows(matches[left], matches[right], parsed.order) < 0
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	columns := selectColumns(sql)
	if len(columns) == 0 {
		return nil, fmt.Errorf("emulator cannot read the select list from %q", sql)
	}
	rows := make([][]any, len(matches))
	for rowIndex, values := range matches {
		row := make([]any, len(columns))
		for columnIndex, column := range columns {
			row[columnIndex] = values[column]
		}
		rows[rowIndex] = row
	}
	return &fakeRows{rows: rows, index: -1}, nil
}

func scanInt(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		number, _ := typed.Int64()
		return int(number)
	default:
		return -1
	}
}

func compareScanRows(left, right map[string]any, terms []store.Order) int {
	for _, term := range terms {
		comparison := compareScanValues(left[term.Column], right[term.Column])
		if comparison == 0 {
			continue
		}
		if term.Desc {
			return -comparison
		}
		return comparison
	}
	return 0
}

func compareScanValues(left, right any) int {
	if left == nil && right == nil {
		return 0
	}
	if left == nil {
		return -1
	}
	if right == nil {
		return 1
	}
	leftTime, leftIsTime := left.(time.Time)
	rightTime, rightIsTime := right.(time.Time)
	if leftIsTime && !rightIsTime {
		if text, ok := right.(string); ok {
			if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
				rightTime, rightIsTime = parsed, true
			}
		}
	}
	if rightIsTime && !leftIsTime {
		if text, ok := left.(string); ok {
			if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
				leftTime, leftIsTime = parsed, true
			}
		}
	}
	if leftIsTime && rightIsTime {
		switch {
		case leftTime.Before(rightTime):
			return -1
		case leftTime.After(rightTime):
			return 1
		default:
			return 0
		}
	}
	leftNumber, leftIsNumber := scanNumber(left)
	rightNumber, rightIsNumber := scanNumber(right)
	if leftIsNumber && rightIsNumber {
		switch {
		case leftNumber < rightNumber:
			return -1
		case leftNumber > rightNumber:
			return 1
		default:
			return 0
		}
	}
	if leftString, ok := left.(string); ok {
		if rightString, ok := right.(string); ok {
			return strings.Compare(leftString, rightString)
		}
	}
	if leftBool, ok := left.(bool); ok {
		if rightBool, ok := right.(bool); ok {
			switch {
			case leftBool == rightBool:
				return 0
			case !leftBool:
				return -1
			default:
				return 1
			}
		}
	}
	if leftBytes, ok := left.([]byte); ok {
		if rightBytes, ok := right.([]byte); ok {
			return bytes.Compare(leftBytes, rightBytes)
		}
	}
	return strings.Compare(fmt.Sprint(left), fmt.Sprint(right))
}

func scanNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case float64:
		return typed, true
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	default:
		return 0, false
	}
}

// --- statement parser ------------------------------------------------------

type scanParsed struct {
	table      string
	predicate  func(map[string]any, []any) (bool, error)
	order      []store.Order
	limitParam int
}

func parseScanSQL(sql string) (scanParsed, error) {
	upper := strings.ToUpper(sql)
	fromIndex := strings.Index(upper, " FROM ")
	whereIndex := strings.Index(upper, " WHERE ")
	orderIndex := strings.Index(upper, " ORDER BY ")
	limitIndex := strings.Index(upper, " LIMIT ")
	if fromIndex < 0 || whereIndex < 0 || orderIndex < 0 || limitIndex < 0 ||
		!(fromIndex < whereIndex && whereIndex < orderIndex && orderIndex < limitIndex) {
		return scanParsed{}, fmt.Errorf("statement does not have the expected SELECT/FROM/WHERE/ORDER BY/LIMIT shape")
	}
	table, err := unquoteScanIdent(strings.TrimSpace(sql[fromIndex+len(" FROM ") : whereIndex]))
	if err != nil {
		return scanParsed{}, err
	}
	predicate, err := parseScanPredicate(strings.TrimSpace(sql[whereIndex+len(" WHERE ") : orderIndex]))
	if err != nil {
		return scanParsed{}, err
	}
	order, err := parseScanOrder(strings.TrimSpace(sql[orderIndex+len(" ORDER BY ") : limitIndex]))
	if err != nil {
		return scanParsed{}, err
	}
	limitParam, err := parseScanParam(strings.TrimSpace(sql[limitIndex+len(" LIMIT "):]))
	if err != nil {
		return scanParsed{}, err
	}
	return scanParsed{table: table, predicate: predicate, order: order, limitParam: limitParam}, nil
}

func unquoteScanIdent(value string) (string, error) {
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", fmt.Errorf("identifier %q is not quoted", value)
	}
	return strings.ReplaceAll(value[1:len(value)-1], `""`, `"`), nil
}

func parseScanOrder(value string) ([]store.Order, error) {
	parts := strings.Split(value, ",")
	order := make([]store.Order, 0, len(parts))
	for _, part := range parts {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) != 2 {
			return nil, fmt.Errorf("order term %q is not a quoted column and direction", part)
		}
		column, err := unquoteScanIdent(fields[0])
		if err != nil {
			return nil, err
		}
		switch strings.ToUpper(fields[1]) {
		case "ASC":
			order = append(order, store.Order{Column: column})
		case "DESC":
			order = append(order, store.Order{Column: column, Desc: true})
		default:
			return nil, fmt.Errorf("order term %q has an unknown direction", part)
		}
	}
	return order, nil
}

func parseScanParam(value string) (int, error) {
	if !strings.HasPrefix(value, "$") {
		return 0, fmt.Errorf("parameter %q is not positional", value)
	}
	index, err := strconv.Atoi(value[1:])
	if err != nil || index < 1 {
		return 0, fmt.Errorf("parameter %q is not a positive index", value)
	}
	return index, nil
}

const (
	scanTokenEOF = iota
	scanTokenIdent
	scanTokenParam
	scanTokenOp
	scanTokenLParen
	scanTokenRParen
	scanTokenComma
	scanTokenAnd
	scanTokenOr
	scanTokenIs
	scanTokenNull
)

type scanToken struct {
	kind   int
	text   string
	number int
}

func lexScanPredicate(value string) ([]scanToken, error) {
	tokens := make([]scanToken, 0, 16)
	for index := 0; index < len(value); {
		character := value[index]
		switch {
		case character == ' ' || character == '\t' || character == '\n':
			index++
		case character == '(':
			tokens = append(tokens, scanToken{kind: scanTokenLParen})
			index++
		case character == ')':
			tokens = append(tokens, scanToken{kind: scanTokenRParen})
			index++
		case character == ',':
			tokens = append(tokens, scanToken{kind: scanTokenComma})
			index++
		case character == '"':
			end := index + 1
			var builder strings.Builder
			for end < len(value) {
				if value[end] == '"' {
					if end+1 < len(value) && value[end+1] == '"' {
						builder.WriteByte('"')
						end += 2
						continue
					}
					break
				}
				builder.WriteByte(value[end])
				end++
			}
			if end >= len(value) {
				return nil, fmt.Errorf("unterminated quoted identifier")
			}
			tokens = append(tokens, scanToken{kind: scanTokenIdent, text: builder.String()})
			index = end + 1
		case character == '$':
			end := index + 1
			for end < len(value) && value[end] >= '0' && value[end] <= '9' {
				end++
			}
			number, err := strconv.Atoi(value[index+1 : end])
			if err != nil {
				return nil, fmt.Errorf("bad parameter %q", value[index:end])
			}
			tokens = append(tokens, scanToken{kind: scanTokenParam, number: number})
			index = end
		case character == '=' || character == '>' || character == '<':
			tokens = append(tokens, scanToken{kind: scanTokenOp, text: string(character)})
			index++
		case (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z'):
			end := index
			for end < len(value) {
				next := value[end]
				if !((next >= 'a' && next <= 'z') || (next >= 'A' && next <= 'Z') || next == '_') {
					break
				}
				end++
			}
			word := strings.ToUpper(value[index:end])
			switch word {
			case "AND":
				tokens = append(tokens, scanToken{kind: scanTokenAnd})
			case "OR":
				tokens = append(tokens, scanToken{kind: scanTokenOr})
			case "IS":
				tokens = append(tokens, scanToken{kind: scanTokenIs})
			case "NULL":
				tokens = append(tokens, scanToken{kind: scanTokenNull})
			default:
				return nil, fmt.Errorf("unexpected keyword %q", word)
			}
			index = end
		default:
			return nil, fmt.Errorf("unexpected character %q", string(character))
		}
	}
	tokens = append(tokens, scanToken{kind: scanTokenEOF})
	return tokens, nil
}

type scanParser struct {
	tokens []scanToken
	pos    int
}

func parseScanPredicate(value string) (func(map[string]any, []any) (bool, error), error) {
	tokens, err := lexScanPredicate(value)
	if err != nil {
		return nil, err
	}
	parser := &scanParser{tokens: tokens}
	expression, err := parser.parseOr()
	if err != nil {
		return nil, err
	}
	if parser.peek().kind != scanTokenEOF {
		return nil, fmt.Errorf("unexpected trailing predicate token")
	}
	return expression, nil
}

func (parser *scanParser) peek() scanToken { return parser.tokens[parser.pos] }

func (parser *scanParser) peekAt(offset int) scanToken {
	position := parser.pos + offset
	if position >= len(parser.tokens) {
		return scanToken{kind: scanTokenEOF}
	}
	return parser.tokens[position]
}

func (parser *scanParser) next() scanToken {
	token := parser.tokens[parser.pos]
	if parser.pos < len(parser.tokens)-1 {
		parser.pos++
	}
	return token
}

func (parser *scanParser) parseOr() (func(map[string]any, []any) (bool, error), error) {
	left, err := parser.parseAnd()
	if err != nil {
		return nil, err
	}
	for parser.peek().kind == scanTokenOr {
		parser.next()
		right, err := parser.parseAnd()
		if err != nil {
			return nil, err
		}
		leftValue, rightValue := left, right
		left = func(values map[string]any, args []any) (bool, error) {
			matched, err := leftValue(values, args)
			if err != nil || matched {
				return matched, err
			}
			return rightValue(values, args)
		}
	}
	return left, nil
}

func (parser *scanParser) parseAnd() (func(map[string]any, []any) (bool, error), error) {
	left, err := parser.parsePrimary()
	if err != nil {
		return nil, err
	}
	for parser.peek().kind == scanTokenAnd {
		parser.next()
		right, err := parser.parsePrimary()
		if err != nil {
			return nil, err
		}
		leftValue, rightValue := left, right
		left = func(values map[string]any, args []any) (bool, error) {
			matched, err := leftValue(values, args)
			if err != nil || !matched {
				return matched, err
			}
			return rightValue(values, args)
		}
	}
	return left, nil
}

func (parser *scanParser) parsePrimary() (func(map[string]any, []any) (bool, error), error) {
	token := parser.peek()
	if token.kind == scanTokenLParen {
		if parser.peekAt(1).kind == scanTokenIdent &&
			(parser.peekAt(2).kind == scanTokenComma || parser.peekAt(2).kind == scanTokenRParen) {
			return parser.parseRowComparison()
		}
		parser.next()
		expression, err := parser.parseOr()
		if err != nil {
			return nil, err
		}
		if parser.next().kind != scanTokenRParen {
			return nil, fmt.Errorf("grouped predicate is not closed")
		}
		return expression, nil
	}
	if token.kind != scanTokenIdent {
		return nil, fmt.Errorf("predicate does not start with a column")
	}
	parser.next()
	column := token.text
	if parser.peek().kind == scanTokenIs {
		parser.next()
		if parser.next().kind != scanTokenNull {
			return nil, fmt.Errorf("IS is not followed by NULL")
		}
		return func(values map[string]any, _ []any) (bool, error) {
			return values[column] == nil, nil
		}, nil
	}
	opToken := parser.next()
	if opToken.kind != scanTokenOp {
		return nil, fmt.Errorf("column %q is not followed by a comparison", column)
	}
	paramToken := parser.next()
	if paramToken.kind != scanTokenParam {
		return nil, fmt.Errorf("comparison on %q is not bound to a parameter", column)
	}
	op, index := opToken.text, paramToken.number
	return func(values map[string]any, args []any) (bool, error) {
		if index < 1 || index > len(args) {
			return false, fmt.Errorf("parameter $%d is outside %d args", index, len(args))
		}
		return scanCompareOp(compareScanValues(values[column], args[index-1]), op), nil
	}, nil
}

func (parser *scanParser) parseRowComparison() (func(map[string]any, []any) (bool, error), error) {
	parser.next() // (
	columns := make([]string, 0, 2)
	for {
		token := parser.next()
		if token.kind != scanTokenIdent {
			return nil, fmt.Errorf("row comparison has a non-column term")
		}
		columns = append(columns, token.text)
		if parser.peek().kind == scanTokenComma {
			parser.next()
			continue
		}
		break
	}
	if parser.next().kind != scanTokenRParen {
		return nil, fmt.Errorf("row comparison column list is not closed")
	}
	opToken := parser.next()
	if opToken.kind != scanTokenOp {
		return nil, fmt.Errorf("row comparison has no operator")
	}
	if parser.next().kind != scanTokenLParen {
		return nil, fmt.Errorf("row comparison parameter list is not open")
	}
	params := make([]int, 0, len(columns))
	for {
		token := parser.next()
		if token.kind != scanTokenParam {
			return nil, fmt.Errorf("row comparison has a non-parameter term")
		}
		params = append(params, token.number)
		if parser.peek().kind == scanTokenComma {
			parser.next()
			continue
		}
		break
	}
	if parser.next().kind != scanTokenRParen {
		return nil, fmt.Errorf("row comparison parameter list is not closed")
	}
	if len(columns) != len(params) {
		return nil, fmt.Errorf("row comparison has %d columns and %d parameters", len(columns), len(params))
	}
	op := opToken.text
	return func(values map[string]any, args []any) (bool, error) {
		for index, column := range columns {
			param := params[index]
			if param < 1 || param > len(args) {
				return false, fmt.Errorf("parameter $%d is outside %d args", param, len(args))
			}
			comparison := compareScanValues(values[column], args[param-1])
			if comparison != 0 {
				return scanCompareOp(comparison, op), nil
			}
		}
		return false, nil
	}, nil
}

func scanCompareOp(comparison int, op string) bool {
	switch op {
	case "=":
		return comparison == 0
	case ">":
		return comparison > 0
	case "<":
		return comparison < 0
	default:
		return false
	}
}

// --- targeted behavior -----------------------------------------------------

func newEmptyScanner(t *testing.T) (*Store, *fakeDB) {
	t.Helper()
	db := newFakeDB()
	db.script.setQuery(func(string, []any) (store.Rows, error) {
		return &fakeRows{index: -1}, nil
	})
	return New(db), db
}

func TestScannerRefusesInvalidRequestsBeforeBegin(t *testing.T) {
	longCursor := strings.Repeat("a", store.MaxQueryCursorLength+1)
	cases := []struct {
		name  string
		query store.ScanQuery
	}{
		{"limit zero", store.ScanQuery{Table: "executions", Limit: 0}},
		{"limit negative", store.ScanQuery{Table: "executions", Limit: -1}},
		{"limit above maximum", store.ScanQuery{Table: "executions", Limit: store.MaxQueryLimit + 1}},
		{"empty table", store.ScanQuery{Table: "", Limit: 10}},
		{"unknown table", store.ScanQuery{Table: "unknown", Limit: 10}},
		{"unknown equality column", store.ScanQuery{Table: "executions", Equals: map[string]any{"nope": "x"}, Limit: 10}},
		{"nil equality", store.ScanQuery{Table: "executions", Equals: map[string]any{"state": nil}, Limit: 10}},
		{"unknown range column", store.ScanQuery{Table: "executions", Range: []store.RangeFilter{{Column: "nope", After: "a"}}, Limit: 10}},
		{"range without bound", store.ScanQuery{Table: "executions", Range: []store.RangeFilter{{Column: "created_at"}}, Limit: 10}},
		{"nullable range column", store.ScanQuery{Table: "events", Range: []store.RangeFilter{{Column: "project_id", After: "a"}}, Limit: 10}},
		{"nullable order column", store.ScanQuery{Table: "events", OrderBy: []store.Order{{Column: "project_id"}}, Limit: 10}},
		{"duplicate order column", store.ScanQuery{Table: "executions", OrderBy: []store.Order{{Column: "created_at"}, {Column: "created_at", Desc: true}}, Limit: 10}},
		{"id order not final", store.ScanQuery{Table: "executions", OrderBy: []store.Order{{Column: "id"}, {Column: "created_at"}}, Limit: 10}},
		{"cursor with whitespace", store.ScanQuery{Table: "executions", After: "not a cursor", Limit: 10}},
		{"overlong cursor", store.ScanQuery{Table: "executions", After: longCursor, Limit: 10}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			scanner, db := newEmptyScanner(t)
			_, err := scanner.Query(context.Background(), testProjectScope(), testCase.query)
			assertFaultClass(t, err, fault.InvalidInput)
			if got := db.beginCount(); got != 0 {
				t.Fatalf("refused query opened %d transactions", got)
			}
		})
	}
}

func TestScannerRefusesTenantScopeForProjectRequiredTable(t *testing.T) {
	scanner, db := newEmptyScanner(t)
	_, err := scanner.Query(context.Background(), testTenantScope(), store.ScanQuery{Table: "executions", Limit: 10})
	assertFaultClass(t, err, fault.InvalidInput)
	if got := db.beginCount(); got != 0 {
		t.Fatalf("tenant-wide project-table query opened %d transactions", got)
	}
}

func TestScannerReadOnlyTransactionAndScopeReset(t *testing.T) {
	scanner, db := newEmptyScanner(t)
	page, err := scanner.Query(context.Background(), testProjectScope(), store.ScanQuery{Table: "executions", Limit: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Rows) != 0 || page.HasMore || page.Next != "" {
		t.Fatalf("empty read page = %+v; want empty", page)
	}
	if got := db.beginCount(); got != 1 {
		t.Fatalf("Query opened %d transactions; want 1", got)
	}
	conn := db.conn(t)
	if conn.commits != 1 || conn.rollbacks != 0 {
		t.Fatalf("terminal calls: commits=%d rollbacks=%d; want one commit", conn.commits, conn.rollbacks)
	}
	calls := conn.callsCopy()
	if len(calls) != 4 {
		t.Fatalf("Query issued %d calls; want read-only, scope, query and commit", len(calls))
	}
	if calls[0].kind != "exec" || !strings.EqualFold(strings.TrimSpace(calls[0].sql), beginReadOnlySQL) {
		t.Fatalf("first call = %+v; want %s", calls[0], beginReadOnlySQL)
	}
	if calls[1].kind != "exec" || indexOfSQL(calls, "set_config") != 1 {
		t.Fatalf("scope was not established second: %+v", calls[1])
	}
	if !strings.Contains(strings.ToLower(calls[1].sql), "true") {
		t.Errorf("scope statement is not transaction-local: %s", calls[1].sql)
	}
	if calls[2].kind != "query" {
		t.Fatalf("third call = %q; want the scoped query", calls[2].kind)
	}
	if calls[3].kind != "commit" {
		t.Fatalf("last call = %q; want a read-only commit", calls[3].kind)
	}
	for _, value := range []any{testTenantID, testProjectID} {
		if !argContains(calls[2].args, value) {
			t.Errorf("query args %v do not carry scope value %v", calls[2].args, value)
		}
	}
}

func TestScannerRollsBackOnFailure(t *testing.T) {
	t.Run("query failure", func(t *testing.T) {
		db := newFakeDB()
		db.script.setQuery(func(string, []any) (store.Rows, error) {
			return nil, errors.New("query failed")
		})
		_, err := New(db).Query(context.Background(), testProjectScope(), store.ScanQuery{Table: "executions", Limit: 10})
		if err == nil {
			t.Fatal("query failure was reported as success")
		}
		conn := db.conn(t)
		if conn.commits != 0 || conn.rollbacks != 1 {
			t.Fatalf("query failure terminal calls: commits=%d rollbacks=%d", conn.commits, conn.rollbacks)
		}
	})

	t.Run("scan failure", func(t *testing.T) {
		db := newFakeDB()
		db.script.setQuery(func(string, []any) (store.Rows, error) {
			return &fakeRows{rows: [][]any{{"only-one-column"}}, index: -1}, nil
		})
		_, err := New(db).Query(context.Background(), testProjectScope(), store.ScanQuery{Table: "executions", Limit: 10})
		assertFaultClass(t, err, fault.InternalFailure)
		conn := db.conn(t)
		if conn.commits != 0 || conn.rollbacks != 1 {
			t.Fatalf("scan failure terminal calls: commits=%d rollbacks=%d", conn.commits, conn.rollbacks)
		}
	})

	t.Run("iteration failure", func(t *testing.T) {
		db := newFakeDB()
		db.script.setQuery(func(string, []any) (store.Rows, error) {
			return &fakeRows{err: errors.New("iteration failed"), index: -1}, nil
		})
		_, err := New(db).Query(context.Background(), testProjectScope(), store.ScanQuery{Table: "executions", Limit: 10})
		if err == nil {
			t.Fatal("iteration failure was reported as success")
		}
		conn := db.conn(t)
		if conn.commits != 0 || conn.rollbacks != 1 {
			t.Fatalf("iteration failure terminal calls: commits=%d rollbacks=%d", conn.commits, conn.rollbacks)
		}
	})

	t.Run("commit failure", func(t *testing.T) {
		db := newFakeDB()
		db.script.commitErr = errors.New("commit failed")
		_, err := New(db).Query(context.Background(), testProjectScope(), store.ScanQuery{Table: "executions", Limit: 10})
		if err == nil {
			t.Fatal("commit failure was reported as success")
		}
		conn := db.conn(t)
		if conn.commits != 1 || conn.rollbacks != 1 {
			t.Fatalf("commit failure terminal calls: commits=%d rollbacks=%d; want cleanup rollback", conn.commits, conn.rollbacks)
		}
	})
}

func TestScannerScopePredicates(t *testing.T) {
	t.Run("tenants root row", func(t *testing.T) {
		scanner, db := newEmptyScanner(t)
		if _, err := scanner.Query(context.Background(), testProjectScope(), store.ScanQuery{Table: "tenants", Limit: 10}); err != nil {
			t.Fatalf("Query tenants: %v", err)
		}
		call := onlyScanQuery(t, db)
		if !strings.Contains(call.sql, `"id" = $1`) {
			t.Errorf("tenant root predicate = %s; want id bound to the scope tenant", call.sql)
		}
		if !argContains(call.args, testTenantID) {
			t.Errorf("tenant root args %v do not carry %q", call.args, testTenantID)
		}
	})

	t.Run("projects project scope", func(t *testing.T) {
		scanner, db := newEmptyScanner(t)
		if _, err := scanner.Query(context.Background(), testProjectScope(), store.ScanQuery{Table: "projects", Limit: 10}); err != nil {
			t.Fatalf("Query projects: %v", err)
		}
		call := onlyScanQuery(t, db)
		if !strings.Contains(call.sql, `"tenant_id" = $1`) || !strings.Contains(call.sql, `"id" = $2`) {
			t.Errorf("project root predicate = %s; want tenant_id and id", call.sql)
		}
		if !argContains(call.args, testTenantID) || !argContains(call.args, testProjectID) {
			t.Errorf("project root args %v do not carry the scope", call.args)
		}
	})

	t.Run("projects tenant scope", func(t *testing.T) {
		scanner, db := newEmptyScanner(t)
		if _, err := scanner.Query(context.Background(), testTenantScope(), store.ScanQuery{Table: "projects", Limit: 10}); err != nil {
			t.Fatalf("Query projects tenant-wide: %v", err)
		}
		call := onlyScanQuery(t, db)
		if !strings.Contains(call.sql, `"tenant_id" = $1`) {
			t.Errorf("tenant-wide project predicate = %s; want tenant_id", call.sql)
		}
		if strings.Contains(call.sql, `"id" =`) {
			t.Errorf("tenant-wide project predicate wrongly filters by project id: %s", call.sql)
		}
	})

	t.Run("events tenant-wide is project_id null", func(t *testing.T) {
		scanner, db := newEmptyScanner(t)
		if _, err := scanner.Query(context.Background(), testTenantScope(), store.ScanQuery{Table: "events", Limit: 10}); err != nil {
			t.Fatalf("Query events tenant-wide: %v", err)
		}
		call := onlyScanQuery(t, db)
		if !strings.Contains(strings.ToLower(call.sql), `"project_id" is null`) {
			t.Errorf("tenant-wide events predicate = %s; want project_id IS NULL", call.sql)
		}
	})

	t.Run("caller equality cannot widen", func(t *testing.T) {
		scanner, db := newEmptyScanner(t)
		query := store.ScanQuery{Table: "executions", Equals: map[string]any{"tenant_id": "tenant-2"}, Limit: 10}
		if _, err := scanner.Query(context.Background(), testProjectScope(), query); err != nil {
			t.Fatalf("Query: %v", err)
		}
		call := onlyScanQuery(t, db)
		if !argContains(call.args, testTenantID) || !argContains(call.args, "tenant-2") {
			t.Errorf("equality args %v do not carry both the scope and caller tenant", call.args)
		}
		if strings.Contains(call.sql, " OR ") {
			t.Errorf("caller equality widened the scope predicate: %s", call.sql)
		}
	})
}

func onlyScanQuery(t *testing.T, db *fakeDB) recordedCall {
	t.Helper()
	queries := queryCalls(db.conn(t).callsCopy())
	if len(queries) != 1 {
		t.Fatalf("scanner issued %d queries; want 1", len(queries))
	}
	return queries[0]
}

func TestScannerKeysetMixedDirections(t *testing.T) {
	seed := scanSeed{Rows: []scanSeedRow{
		{Table: "executions", TenantID: testTenantID, ProjectID: testProjectID, ID: "execution-1", Values: map[string]any{"created_at": "2026-01-01T00:00:01Z", "state": "a"}},
		{Table: "executions", TenantID: testTenantID, ProjectID: testProjectID, ID: "execution-2", Values: map[string]any{"created_at": "2026-01-01T00:00:01Z", "state": "b"}},
		{Table: "executions", TenantID: testTenantID, ProjectID: testProjectID, ID: "execution-3", Values: map[string]any{"created_at": "2026-01-01T00:00:02Z", "state": "a"}},
		// Rows outside the transaction scope that satisfy the cursor's later OR
		// clauses: the second clause (created_at = cursor AND state > cursor) and
		// the third clause (created_at = cursor AND state = cursor AND id >
		// cursor). If the scope predicates were ANDed with only the first OR
		// clause, page 2 would return these rows or fail the scope post-check.
		{Table: "executions", TenantID: "tenant-2", ProjectID: testProjectID, ID: "foreign-tenant-1", Values: map[string]any{"created_at": "2026-01-01T00:00:02Z", "state": "b"}},
		{Table: "executions", TenantID: testTenantID, ProjectID: "project-2", ID: "zzz-foreign-project", Values: map[string]any{"created_at": "2026-01-01T00:00:02Z", "state": "a"}},
	}}
	state := newScanVectorState(t, seed)
	db := newFakeDB()
	db.script.setQuery(state.query)
	scanner := New(db)

	first := store.ScanQuery{
		Table:   "executions",
		OrderBy: []store.Order{{Column: "created_at", Desc: true}, {Column: "state"}},
		Limit:   1,
	}
	page, err := scanner.Query(context.Background(), testProjectScope(), first)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if ids := scanPageIDs(page); !equalStrings(ids, []string{"execution-3"}) {
		t.Fatalf("first page ids = %v; want execution-3", ids)
	}
	if !page.HasMore || page.Next == "" {
		t.Fatalf("first page HasMore=%v Next=%q; want another page", page.HasMore, page.Next)
	}

	second := first
	second.After = page.Next
	second.Limit = 10
	page, err = scanner.Query(context.Background(), testProjectScope(), second)
	if err != nil {
		t.Fatalf("second page failed instead of filtering out-of-scope rows in SQL (scope post-check error): %v", err)
	}
	if ids := scanPageIDs(page); !equalStrings(ids, []string{"execution-1", "execution-2"}) {
		t.Fatalf("second page ids = %v; want execution-1, execution-2 only", ids)
	}
	assertScanRowsInScope(t, page, testProjectScope())
	if page.HasMore || page.Next != "" {
		t.Fatalf("second page HasMore=%v Next=%q; want the final page", page.HasMore, page.Next)
	}

	call := onlyScanQuery(t, db)
	lower := strings.ToLower(call.sql)
	if !strings.Contains(lower, " or ") {
		t.Errorf("mixed-direction keyset is not an OR chain: %s", call.sql)
	}
	if !strings.Contains(call.sql, `"created_at" < $`) || !strings.Contains(call.sql, `"state" > $`) {
		t.Errorf("mixed-direction keyset has the wrong comparison directions: %s", call.sql)
	}
	assertScopePredicatesOutsideOrGroup(t, call.sql)
	for _, key := range []string{"execution-3", "2026-01-01T00:00:02Z"} {
		if strings.Contains(call.sql, key) {
			t.Errorf("cursor key %q was interpolated into SQL: %s", key, call.sql)
		}
		if !argContains(call.args, key) && !argContains(call.args, mustScanTime(t, key)) {
			t.Errorf("cursor key %q is not a bound parameter in %v", key, call.args)
		}
	}
}

func mustScanTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed
}

func TestScannerKeysetUniformDirectionUsesRowValue(t *testing.T) {
	seed := scanSeed{Rows: []scanSeedRow{
		{Table: "executions", TenantID: testTenantID, ProjectID: testProjectID, ID: "execution-1", Values: map[string]any{"created_at": "2026-01-01T00:00:01Z"}},
		{Table: "executions", TenantID: testTenantID, ProjectID: testProjectID, ID: "execution-2", Values: map[string]any{"created_at": "2026-01-01T00:00:02Z"}},
		// Out-of-scope rows that satisfy the row-value keyset for the cursor
		// row execution-1: the page must still contain only in-scope rows.
		{Table: "executions", TenantID: "tenant-2", ProjectID: testProjectID, ID: "zzzz-foreign-tenant", Values: map[string]any{"created_at": "2026-01-01T00:00:01Z"}},
		{Table: "executions", TenantID: testTenantID, ProjectID: "project-2", ID: "foreign-project-1", Values: map[string]any{"created_at": "2026-01-01T00:00:03Z"}},
	}}
	state := newScanVectorState(t, seed)
	db := newFakeDB()
	db.script.setQuery(state.query)
	scanner := New(db)

	query := store.ScanQuery{Table: "executions", OrderBy: []store.Order{{Column: "created_at"}}, Limit: 1}
	page, err := scanner.Query(context.Background(), testProjectScope(), query)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	query.After = page.Next
	page, err = scanner.Query(context.Background(), testProjectScope(), query)
	if err != nil {
		t.Fatalf("second page failed instead of filtering out-of-scope rows in SQL (scope post-check error): %v", err)
	}
	if ids := scanPageIDs(page); !equalStrings(ids, []string{"execution-2"}) {
		t.Fatalf("second page ids = %v; want execution-2 only", ids)
	}
	assertScanRowsInScope(t, page, testProjectScope())
	call := onlyScanQuery(t, db)
	if !strings.Contains(call.sql, `("created_at", "id") > (`) {
		t.Errorf("uniform keyset is not a row-value comparison: %s", call.sql)
	}
	assertScopePredicatesOutsideOrGroup(t, call.sql)
}

// assertScanRowsInScope fails when a page carries a row from outside the
// transaction scope. The SQL predicate must already exclude such rows; the
// adapter's post-check turning them into a forbidden fault is not enough.
func assertScanRowsInScope(t *testing.T, page store.QueryPage, scope tenancy.Scope) {
	t.Helper()
	for _, row := range page.Rows {
		tenant, _ := row.Values["tenant_id"].(string)
		if tenant != string(scope.TenantID) {
			t.Errorf("page leaked row %v from tenant %q; want only tenant %q", row.Values["id"], tenant, scope.TenantID)
		}
		project, _ := row.Values["project_id"].(string)
		if project != string(scope.ProjectID) {
			t.Errorf("page leaked row %v from project %q; want only project %q", row.Values["id"], project, scope.ProjectID)
		}
	}
}

// assertScopePredicatesOutsideOrGroup pins the boolean structure of the keyset
// WHERE clause: the tenant and project predicates must be ANDed at the top
// level, outside the parenthesized OR chain, so every alternative stays inside
// the transaction scope. Dropping the outer keyset parentheses would make the
// scope conjunction bind only the first alternative.
func assertScopePredicatesOutsideOrGroup(t *testing.T, sql string) {
	t.Helper()
	where := scanWhereClause(t, sql)
	depths := predicateDepths(t, where)
	for index := 0; index+len(" OR ") <= len(where); index++ {
		if where[index:index+len(" OR ")] != " OR " {
			continue
		}
		if depths[index] == 0 {
			t.Errorf("top-level OR escapes the scope conjunction: %s", sql)
		}
	}
	for _, comparison := range []string{`"tenant_id" =`, `"project_id" =`} {
		index := strings.Index(where, comparison)
		if index < 0 {
			t.Fatalf("statement is missing the scope comparison %q: %s", comparison, sql)
		}
		if depths[index] != 0 {
			t.Errorf("scope comparison %q is nested inside the keyset group: %s", comparison, sql)
		}
	}
}

// scanWhereClause extracts the predicate between WHERE and ORDER BY so a test
// can inspect the boolean structure the database evaluates.
func scanWhereClause(t *testing.T, sql string) string {
	t.Helper()
	lower := strings.ToLower(sql)
	start := strings.Index(lower, " where ")
	end := strings.LastIndex(lower, " order by ")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("statement has no WHERE ... ORDER BY shape: %s", sql)
	}
	return sql[start+len(" where ") : end]
}

// predicateDepths reports the parenthesis depth in front of every byte of a
// predicate. A '(' is recorded at the depth outside the group it opens and a
// ')' at the depth outside the group it closes.
func predicateDepths(t *testing.T, predicate string) []int {
	t.Helper()
	depths := make([]int, len(predicate))
	depth := 0
	for index := 0; index < len(predicate); index++ {
		character := predicate[index]
		if character == ')' {
			depth--
			if depth < 0 {
				t.Fatalf("predicate closes a group it never opened: %s", predicate)
			}
		}
		depths[index] = depth
		if character == '(' {
			depth++
		}
	}
	if depth != 0 {
		t.Fatalf("predicate leaves %d groups unclosed: %s", depth, predicate)
	}
	return depths
}

func scanPageIDs(page store.QueryPage) []string {
	ids := make([]string, 0, len(page.Rows))
	for _, row := range page.Rows {
		text, _ := row.Values["id"].(string)
		ids = append(ids, text)
	}
	return ids
}

func TestScannerCursorBinding(t *testing.T) {
	seed := scanSeed{Rows: []scanSeedRow{
		{Table: "executions", TenantID: testTenantID, ProjectID: testProjectID, ID: "execution-1", Values: map[string]any{"created_at": "2026-01-01T00:00:01Z"}},
		{Table: "executions", TenantID: testTenantID, ProjectID: testProjectID, ID: "execution-2", Values: map[string]any{"created_at": "2026-01-01T00:00:02Z"}},
		{Table: "tasks", TenantID: testTenantID, ProjectID: testProjectID, ID: "task-1", Values: map[string]any{"created_at": "2026-01-01T00:00:01Z"}},
	}}
	state := newScanVectorState(t, seed)
	db := newFakeDB()
	db.script.setQuery(state.query)
	scanner := New(db)

	base := store.ScanQuery{Table: "executions", OrderBy: []store.Order{{Column: "created_at"}}, Limit: 1}
	page, err := scanner.Query(context.Background(), testProjectScope(), base)
	if err != nil {
		t.Fatalf("cursor source: %v", err)
	}
	cursor := page.Next
	if cursor == "" {
		t.Fatal("cursor source produced no cursor")
	}

	cases := []struct {
		name  string
		scope tenancy.Scope
		query store.ScanQuery
		want  fault.Class
	}{
		{
			name:  "another scope",
			scope: tenancy.Scope{TenantID: testTenantID, ProjectID: "project-2"},
			query: store.ScanQuery{Table: "executions", OrderBy: []store.Order{{Column: "created_at"}}, After: cursor, Limit: 1},
			want:  fault.Forbidden,
		},
		{
			name:  "another tenant",
			scope: tenancy.Scope{TenantID: "tenant-2", ProjectID: testProjectID},
			query: store.ScanQuery{Table: "executions", OrderBy: []store.Order{{Column: "created_at"}}, After: cursor, Limit: 1},
			want:  fault.Forbidden,
		},
		{
			name:  "another table",
			scope: testProjectScope(),
			query: store.ScanQuery{Table: "tasks", OrderBy: []store.Order{{Column: "created_at"}}, After: cursor, Limit: 1},
			want:  fault.InvalidInput,
		},
		{
			name:  "another order",
			scope: testProjectScope(),
			query: store.ScanQuery{Table: "executions", OrderBy: []store.Order{{Column: "created_at", Desc: true}}, After: cursor, Limit: 1},
			want:  fault.InvalidInput,
		},
		{
			name:  "malformed token",
			scope: testProjectScope(),
			query: store.ScanQuery{Table: "executions", OrderBy: []store.Order{{Column: "created_at"}}, After: "!!!!", Limit: 1},
			want:  fault.InvalidInput,
		},
		{
			name:  "cursor injection",
			scope: testProjectScope(),
			query: store.ScanQuery{Table: "executions", OrderBy: []store.Order{{Column: "created_at"}}, After: "x'OR'1'='1", Limit: 1},
			want:  fault.InvalidInput,
		},
		{
			name:  "empty keyset",
			scope: testProjectScope(),
			query: store.ScanQuery{Table: "executions", OrderBy: []store.Order{{Column: "created_at"}}, After: encodeForgedCursor(t, queryCursor{
				TenantID: testTenantID, ProjectID: testProjectID, Table: "executions",
				Order: []cursorOrder{{Column: "created_at"}}, Key: []any{},
			}), Limit: 1},
			want: fault.InvalidInput,
		},
		{
			name:  "forged order column",
			scope: testProjectScope(),
			query: store.ScanQuery{Table: "executions", OrderBy: []store.Order{{Column: "created_at"}}, After: encodeForgedCursor(t, queryCursor{
				TenantID: testTenantID, ProjectID: testProjectID, Table: "executions",
				Order: []cursorOrder{{Column: "nope"}}, Key: []any{"x"},
			}), Limit: 1},
			want: fault.InvalidInput,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			before := db.beginCount()
			_, err := scanner.Query(context.Background(), testCase.scope, testCase.query)
			assertFaultClass(t, err, testCase.want)
			if got := db.beginCount(); got != before {
				t.Fatalf("refused cursor opened %d transactions", got-before)
			}
		})
	}
}

func encodeForgedCursor(t *testing.T, cursor queryCursor) string {
	t.Helper()
	data, err := json.Marshal(cursor)
	if err != nil {
		t.Fatalf("marshal forged cursor: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

func TestScannerLimitBounds(t *testing.T) {
	for _, limit := range []int{1, store.MaxQueryLimit} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			scanner, db := newEmptyScanner(t)
			if _, err := scanner.Query(context.Background(), testProjectScope(), store.ScanQuery{Table: "executions", Limit: limit}); err != nil {
				t.Fatalf("Query limit %d: %v", limit, err)
			}
			call := onlyScanQuery(t, db)
			if !argContainsNumber(call.args, int64(limit+1)) {
				t.Errorf("limit %d bound = %v; want limit+1 %d", limit, call.args, limit+1)
			}
		})
	}
}

func TestScannerRefusesIdentifierInjection(t *testing.T) {
	cases := []store.ScanQuery{
		{Table: `executions"; DROP TABLE executions; --`, Limit: 10},
		{Table: "Executions", Limit: 10},
		{Table: "executions", Equals: map[string]any{`state" OR "1"="1`: "x"}, Limit: 10},
		{Table: "executions", Range: []store.RangeFilter{{Column: `created_at" OR "1"="1`, After: "x"}}, Limit: 10},
		{Table: "executions", OrderBy: []store.Order{{Column: `id"; DROP TABLE executions; --`}}, Limit: 10},
	}
	for _, query := range cases {
		t.Run(query.Table, func(t *testing.T) {
			scanner, db := newEmptyScanner(t)
			_, err := scanner.Query(context.Background(), testProjectScope(), query)
			assertFaultClass(t, err, fault.InvalidInput)
			if got := db.beginCount(); got != 0 {
				t.Fatalf("identifier injection opened %d transactions", got)
			}
		})
	}
}

func TestScannerBindsInjectedValues(t *testing.T) {
	scanner, db := newEmptyScanner(t)
	value := `x'; DROP TABLE executions; --`
	query := store.ScanQuery{Table: "executions", Equals: map[string]any{"state": value}, Limit: 10}
	if _, err := scanner.Query(context.Background(), testProjectScope(), query); err != nil {
		t.Fatalf("Query: %v", err)
	}
	call := onlyScanQuery(t, db)
	if strings.Contains(call.sql, "DROP TABLE") || strings.Contains(call.sql, value) {
		t.Fatalf("caller value was interpolated into SQL: %s", call.sql)
	}
	if !argContains(call.args, value) {
		t.Fatalf("caller value %q is not a bound parameter in %v", value, call.args)
	}
}

func TestScannerNullableEqualityAllowed(t *testing.T) {
	seed := scanSeed{Rows: []scanSeedRow{
		{Table: "events", TenantID: testTenantID, ProjectID: testProjectID, ID: "event-1", Values: map[string]any{"type": "execution.succeeded"}},
	}}
	state := newScanVectorState(t, seed)
	db := newFakeDB()
	db.script.setQuery(state.query)
	page, err := New(db).Query(context.Background(), testProjectScope(), store.ScanQuery{
		Table: "events", Equals: map[string]any{"project_id": testProjectID}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("nullable equality: %v", err)
	}
	if ids := scanPageIDs(page); !equalStrings(ids, []string{"event-1"}) {
		t.Fatalf("nullable equality ids = %v; want event-1", ids)
	}
}

func TestScannerDecodesTypedRows(t *testing.T) {
	seed := scanSeed{Rows: []scanSeedRow{
		{
			Table: "events", TenantID: testTenantID, ProjectID: "", ID: "event-1",
			Values: map[string]any{
				"type":             "execution.succeeded",
				"subject_version":  3,
				"data":             []byte(`{"ok":true}`),
				"publish_state":    "pending",
				"at":               "2026-01-01T00:00:01Z",
				"publish_sequence": 7,
			},
		},
	}}
	state := newScanVectorState(t, seed)
	db := newFakeDB()
	db.script.setQuery(state.query)
	page, err := New(db).Query(context.Background(), testTenantScope(), store.ScanQuery{Table: "events", Limit: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Rows) != 1 {
		t.Fatalf("rows = %d; want 1", len(page.Rows))
	}
	values := page.Rows[0].Values
	if values["id"] != "event-1" || values["type"] != "execution.succeeded" {
		t.Errorf("text values = %v", values)
	}
	if values["project_id"] != nil {
		t.Errorf("nullable project_id = %v; want nil", values["project_id"])
	}
	if version, ok := values["subject_version"].(int64); !ok || version != 3 {
		t.Errorf("subject_version = %T %v; want int64 3", values["subject_version"], values["subject_version"])
	}
	if data, ok := values["data"].([]byte); !ok || !bytes.Equal(data, []byte(`{"ok":true}`)) {
		t.Errorf("data = %T %v; want exact bytes", values["data"], values["data"])
	}
	if at, ok := values["at"].(time.Time); !ok || !at.Equal(time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)) {
		t.Errorf("at = %T %v; want a time.Time", values["at"], values["at"])
	}
}

func TestScannerDecodesBooleanRows(t *testing.T) {
	seed := scanSeed{Rows: []scanSeedRow{
		{
			Table: "verifications", TenantID: testTenantID, ProjectID: testProjectID, ID: "verification-1",
			Values: map[string]any{
				"service_id": "service-1", "actor_id": "principal-1",
				"binding_id": "binding-1", "resource_id": "resource-1", "operation": "git.ref.update",
				"subject": []byte(`{}`), "result": "pass", "approved": true,
			},
		},
	}}
	state := newScanVectorState(t, seed)
	db := newFakeDB()
	db.script.setQuery(state.query)
	page, err := New(db).Query(context.Background(), testProjectScope(), store.ScanQuery{Table: "verifications", Limit: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Rows) != 1 {
		t.Fatalf("rows = %d; want 1", len(page.Rows))
	}
	if approved, ok := page.Rows[0].Values["approved"].(bool); !ok || !approved {
		t.Errorf("approved = %T %v; want bool true", page.Rows[0].Values["approved"], page.Rows[0].Values["approved"])
	}
}

func TestScannerOrderTieBreak(t *testing.T) {
	cases := []struct {
		name  string
		order []store.Order
		want  string
	}{
		{"empty order is id", nil, `ORDER BY "id" ASC`},
		{"appends id", []store.Order{{Column: "created_at"}}, `ORDER BY "created_at" ASC, "id" ASC`},
		{"explicit id is final", []store.Order{{Column: "id", Desc: true}}, `ORDER BY "id" DESC`},
		{"mixed terms keep directions", []store.Order{{Column: "created_at", Desc: true}, {Column: "state"}}, `ORDER BY "created_at" DESC, "state" ASC, "id" ASC`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			scanner, db := newEmptyScanner(t)
			query := store.ScanQuery{Table: "executions", OrderBy: testCase.order, Limit: 10}
			if _, err := scanner.Query(context.Background(), testProjectScope(), query); err != nil {
				t.Fatalf("Query: %v", err)
			}
			call := onlyScanQuery(t, db)
			if !strings.Contains(call.sql, testCase.want) {
				t.Errorf("order clause = %s; want %q", call.sql, testCase.want)
			}
		})
	}
}

func TestScannerPaginationConsistency(t *testing.T) {
	seed := scanSeed{Rows: []scanSeedRow{
		{Table: "executions", TenantID: testTenantID, ProjectID: testProjectID, ID: "execution-1"},
		{Table: "executions", TenantID: testTenantID, ProjectID: testProjectID, ID: "execution-2"},
		{Table: "executions", TenantID: testTenantID, ProjectID: testProjectID, ID: "execution-3"},
	}}
	state := newScanVectorState(t, seed)
	db := newFakeDB()
	db.script.setQuery(state.query)
	scanner := New(db)

	page, err := scanner.Query(context.Background(), testProjectScope(), store.ScanQuery{Table: "executions", Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(page.Rows) != 2 || !page.HasMore || page.Next == "" {
		t.Fatalf("first page = %d rows, HasMore=%v Next=%q", len(page.Rows), page.HasMore, page.Next)
	}
	page, err = scanner.Query(context.Background(), testProjectScope(), store.ScanQuery{Table: "executions", Limit: 2, After: page.Next})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if ids := scanPageIDs(page); !equalStrings(ids, []string{"execution-3"}) || page.HasMore || page.Next != "" {
		t.Fatalf("second page ids = %v HasMore=%v Next=%q", ids, page.HasMore, page.Next)
	}
}

func TestScannerIntegerKeysetCursor(t *testing.T) {
	seed := scanSeed{Rows: []scanSeedRow{
		{Table: "actions", TenantID: testTenantID, ProjectID: testProjectID, ID: "action-1", Values: map[string]any{"execution_id": "execution-1", "index": 1, "name": "build", "kind": "container", "state": "pending"}},
		{Table: "actions", TenantID: testTenantID, ProjectID: testProjectID, ID: "action-2", Values: map[string]any{"execution_id": "execution-1", "index": 2, "name": "test", "kind": "container", "state": "pending"}},
	}}
	state := newScanVectorState(t, seed)
	db := newFakeDB()
	db.script.setQuery(state.query)
	scanner := New(db)

	query := store.ScanQuery{Table: "actions", OrderBy: []store.Order{{Column: "index"}}, Limit: 1}
	page, err := scanner.Query(context.Background(), testProjectScope(), query)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if ids := scanPageIDs(page); !equalStrings(ids, []string{"action-1"}) || page.Next == "" {
		t.Fatalf("first page ids = %v Next=%q", ids, page.Next)
	}
	query.After = page.Next
	page, err = scanner.Query(context.Background(), testProjectScope(), query)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if ids := scanPageIDs(page); !equalStrings(ids, []string{"action-2"}) {
		t.Fatalf("second page ids = %v; want action-2", ids)
	}
	if index, ok := page.Rows[0].Values["index"].(int64); !ok || index != 2 {
		t.Errorf("index = %T %v; want int64 2", page.Rows[0].Values["index"], page.Rows[0].Values["index"])
	}
}
