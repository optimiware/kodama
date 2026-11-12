// Scanner implementation: the scoped multi-row read seam.
//
// Query is deliberately read-only. It takes one consistent snapshot of the
// committed state under the store's lock, releases the lock, and then filters,
// orders and pages the copied rows. A concurrent commit therefore either lands
// entirely before the snapshot or entirely after it, and a returned page never
// aliases stored state.
//
// The adapter always applies the caller's scope. Business rows use the same
// rowVisible predicate as Read (including the tenants/projects root-row rule),
// while events, audit and receipts are scoped from their committed publication
// records. A caller equality can only narrow those predicates.
package memory

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

var _ store.Scanner = (*Store)(nil)

// scanTerm is one canonical ordering term with its resolved column metadata.
type scanTerm struct {
	column store.Column
	desc   bool
}

// preparedScanEquals is one normalized equality predicate.
type preparedScanEquals struct {
	column store.Column
	value  any
}

// preparedScanRange is one normalized strict range predicate.
type preparedScanRange struct {
	column store.Column
	after  any
	before any
}

// preparedScanQuery is a validated and normalized ScanQuery. Bound values are
// resolved to the column's contract type before any row is inspected, so a
// mismatched caller value is refused as invalid_input even when no row matches.
type preparedScanQuery struct {
	equals []preparedScanEquals
	ranges []preparedScanRange
	terms  []scanTerm
	cursor *scanCursor
}

// scanRow is one snapshotted committed row. Its values are already deep copies.
type scanRow struct {
	identity string
	values   map[string]any
}

// scanCursor is the opaque keyset token. It carries the scope, table and
// canonical order it was produced for, plus the order-column values of the last
// returned row.
type scanCursor struct {
	TenantID  string            `json:"tenant_id"`
	ProjectID string            `json:"project_id"`
	Table     string            `json:"table"`
	Order     []scanCursorOrder `json:"order"`
	Key       []any             `json:"key"`
}

type scanCursorOrder struct {
	Column string `json:"column"`
	Desc   bool   `json:"desc"`
}

// Query returns one deterministic scoped page. It validates the request, takes
// one consistent snapshot of committed rows, applies the adapter scope and the
// caller's filters, orders by the requested columns plus the row id, and applies
// the keyset cursor. It never writes, locks or publishes.
func (s *Store) Query(ctx context.Context, scope tenancy.Scope, query store.ScanQuery) (store.QueryPage, error) {
	if err := ctx.Err(); err != nil {
		return store.QueryPage{}, err
	}
	if err := query.Validate(); err != nil {
		return store.QueryPage{}, err
	}
	if err := scope.Validate(); err != nil {
		return store.QueryPage{}, err
	}
	meta, ok := lookupTable(query.Table)
	if !ok {
		return store.QueryPage{}, faultInvalid(fmt.Sprintf("unknown table %q", query.Table))
	}
	if meta.projectRequired && scope.TenantScoped() {
		return store.QueryPage{}, faultInvalid(fmt.Sprintf("table %q is project scoped; a tenant-scope query is not allowed", query.Table))
	}
	prepared, err := prepareScanQuery(meta, scope, query)
	if err != nil {
		return store.QueryPage{}, err
	}

	candidates := s.snapshotScanRows(meta, scope)
	if err := ctx.Err(); err != nil {
		return store.QueryPage{}, err
	}
	return buildScanPage(query, prepared, scope, candidates), nil
}

// buildScanPage filters, orders, pages and encodes one consistent candidate
// snapshot. The scope is carried only by the opaque cursor binding; the
// candidates were already scope-filtered when the snapshot was taken.
func buildScanPage(query store.ScanQuery, prepared *preparedScanQuery, scope tenancy.Scope, candidates []scanRow) store.QueryPage {
	filtered := make([]scanRow, 0, len(candidates))
	for _, row := range candidates {
		if prepared.matches(row.values) {
			filtered = append(filtered, row)
		}
	}
	sort.SliceStable(filtered, func(left, right int) bool {
		return compareScanRows(filtered[left], filtered[right], prepared.terms) < 0
	})
	if prepared.cursor != nil {
		after := make([]scanRow, 0, len(filtered))
		for _, row := range filtered {
			if scanRowAfterCursor(row, prepared.terms, prepared.cursor.Key) {
				after = append(after, row)
			}
		}
		filtered = after
	}

	page := store.QueryPage{}
	if len(filtered) > query.Limit {
		page.HasMore = true
		filtered = filtered[:query.Limit]
	}
	page.Rows = make([]store.Row, 0, len(filtered))
	for _, row := range filtered {
		page.Rows = append(page.Rows, store.Row{Table: query.Table, Values: copyValues(row.values)})
	}
	if page.HasMore {
		page.Next = encodeScanCursor(scope, query.Table, prepared.terms, filtered[len(filtered)-1])
	}
	return page
}

// prepareScanQuery resolves every bound value against the declared schema
// column type and decodes a supplied cursor, refusing a malformed token or a
// token bound to another scope/table/order.
func prepareScanQuery(meta *tableMeta, scope tenancy.Scope, query store.ScanQuery) (*preparedScanQuery, error) {
	prepared := &preparedScanQuery{
		equals: make([]preparedScanEquals, 0, len(query.Equals)),
		ranges: make([]preparedScanRange, 0, len(query.Range)),
		terms:  canonicalScanTerms(meta, query.OrderBy),
	}
	for name, value := range query.Equals {
		column := meta.columns[name]
		normalized, err := normalizeScanValue(column, value)
		if err != nil {
			return nil, err
		}
		prepared.equals = append(prepared.equals, preparedScanEquals{column: column, value: normalized})
	}
	for _, filter := range query.Range {
		column := meta.columns[filter.Column]
		after, err := normalizeScanValue(column, filter.After)
		if err != nil {
			return nil, err
		}
		before, err := normalizeScanValue(column, filter.Before)
		if err != nil {
			return nil, err
		}
		prepared.ranges = append(prepared.ranges, preparedScanRange{column: column, after: after, before: before})
	}
	if query.After != "" {
		cursor, err := decodeScanCursor(query.After, scope, query.Table, prepared.terms)
		if err != nil {
			return nil, err
		}
		prepared.cursor = &cursor
	}
	return prepared, nil
}

// canonicalScanTerms appends the row id as the final ascending tie-breaker when
// the requested order does not already end with it.
func canonicalScanTerms(meta *tableMeta, orders []store.Order) []scanTerm {
	terms := make([]scanTerm, 0, len(orders)+1)
	for _, order := range orders {
		terms = append(terms, scanTerm{column: meta.columns[order.Column], desc: order.Desc})
	}
	if len(terms) == 0 || terms[len(terms)-1].column.Name != "id" {
		terms = append(terms, scanTerm{column: meta.columns["id"]})
	}
	return terms
}

// matches applies the caller's equality and strict range predicates. The
// adapter scope predicate was already applied when the snapshot was taken.
func (prepared *preparedScanQuery) matches(values map[string]any) bool {
	for _, equals := range prepared.equals {
		if compareScanValues(equals.column, values[equals.column.Name], equals.value) != 0 {
			return false
		}
	}
	for _, filter := range prepared.ranges {
		value := values[filter.column.Name]
		if filter.after != nil && compareScanValues(filter.column, value, filter.after) <= 0 {
			return false
		}
		if filter.before != nil && compareScanValues(filter.column, value, filter.before) >= 0 {
			return false
		}
	}
	return true
}

// snapshotScanRows copies every committed row of one table visible in scope
// while holding the store's lock, then releases it. Publication tables are read
// from their committed publication records, exactly like Read.
func (s *Store) snapshotScanRows(meta *tableMeta, scope tenancy.Scope) []scanRow {
	return s.snapshotScanRowsInScope(meta, &scope)
}

// snapshotScanRowsAll copies every committed row of one table across every
// scope. It is the controller seam's cross-scope read: the table authority is
// enforced by ControllerScan.Validate, never by a caller-supplied scope.
func (s *Store) snapshotScanRowsAll(meta *tableMeta) []scanRow {
	return s.snapshotScanRowsInScope(meta, nil)
}

// snapshotScanRowsInScope snapshots one table. A nil scope means the
// cross-scope controller read; a non-nil scope applies the same predicates as
// Read.
func (s *Store) snapshotScanRowsInScope(meta *tableMeta, scope *tenancy.Scope) []scanRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []scanRow
	switch meta.table.Name {
	case "events":
		for _, committed := range s.state.events {
			if scope != nil && !scopeVisible(*scope, committed.Envelope.Scope) {
				continue
			}
			rows = append(rows, scanRow{
				identity: string(committed.Envelope.ID),
				values:   eventRowValues(committed),
			})
		}
	case "audit":
		for _, audit := range s.state.audits {
			if scope != nil && !scopeVisible(*scope, audit.Scope) {
				continue
			}
			rows = append(rows, scanRow{
				identity: string(audit.ID),
				values:   auditRowValues(audit),
			})
		}
	case "receipts":
		for key, record := range s.state.receipts {
			rowScope := tenancy.Scope{TenantID: record.Key.TenantID, ProjectID: record.Key.ProjectID}
			if scope != nil && !scopeVisible(*scope, rowScope) {
				continue
			}
			rows = append(rows, scanRow{
				identity: string(record.ID),
				values:   receiptRowValues(record, s.state.receiptCompleted[key]),
			})
		}
	default:
		for identity, values := range s.state.rows[meta.table.Name] {
			if scope != nil && !rowVisible(meta, *scope, identity, values, false) {
				continue
			}
			rows = append(rows, scanRow{
				identity: identity,
				values:   materializeValues(meta, values),
			})
		}
	}
	return rows
}

// receiptRowValues renders one committed receipt as its schema row. It is a
// defensive copy: the retained result body and snapshot digest are never
// aliased. completed_at is the durable completion marker that pairs with a
// non-NULL result.
func receiptRowValues(record receipt.Record, completed bool) map[string]any {
	values := map[string]any{
		"id":              string(record.ID),
		"tenant_id":       string(record.Key.TenantID),
		"project_id":      projectValue(tenancy.Scope{TenantID: record.Key.TenantID, ProjectID: record.Key.ProjectID}),
		"principal_id":    string(record.Key.PrincipalID),
		"operation":       record.Key.Operation,
		"caller_key":      record.Key.CallerKey,
		"content_sha256":  decodeReceiptHash(record.ContentHash),
		"result":          copyBytes(record.Result),
		"completed_at":    nil,
		"execution_id":    nullableString(string(record.ExecutionID)),
		"snapshot_sha256": nil,
		"created_at":      record.At,
	}
	if completed {
		values["completed_at"] = record.At
	}
	if record.SnapshotHash != nil {
		digest := *record.SnapshotHash
		values["snapshot_sha256"] = copyBytes(digest[:])
	}
	return values
}

// decodeReceiptHash turns the durable sha256 fingerprint into its bytea value.
func decodeReceiptHash(value string) []byte {
	const prefix = "sha256:"
	if strings.HasPrefix(value, prefix) {
		if decoded, err := hex.DecodeString(value[len(prefix):]); err == nil {
			return decoded
		}
	}
	return []byte(value)
}

// compareScanRows compares two snapshotted rows by the canonical terms.
func compareScanRows(left, right scanRow, terms []scanTerm) int {
	for _, term := range terms {
		comparison := compareScanValues(term.column, left.values[term.column.Name], right.values[term.column.Name])
		if comparison == 0 {
			continue
		}
		if term.desc {
			return -comparison
		}
		return comparison
	}
	return 0
}

// scanRowAfterCursor reports whether row sorts strictly after the keyset. A
// descending term reverses only its own comparison.
func scanRowAfterCursor(row scanRow, terms []scanTerm, key []any) bool {
	for index, term := range terms {
		comparison := compareScanValues(term.column, row.values[term.column.Name], key[index])
		if comparison == 0 {
			continue
		}
		if term.desc {
			return comparison < 0
		}
		return comparison > 0
	}
	return false
}

// compareScanValues compares two values of one column. Values are normalized to
// the column's contract type first; a nil sorts before any non-nil value.
func compareScanValues(column store.Column, left, right any) int {
	leftValue, leftErr := normalizeScanValue(column, left)
	rightValue, rightErr := normalizeScanValue(column, right)
	if leftErr != nil || rightErr != nil {
		return strings.Compare(fmt.Sprint(left), fmt.Sprint(right))
	}
	if leftValue == nil && rightValue == nil {
		return 0
	}
	if leftValue == nil {
		return -1
	}
	if rightValue == nil {
		return 1
	}
	switch typed := leftValue.(type) {
	case string:
		return strings.Compare(typed, rightValue.(string))
	case int64:
		other := rightValue.(int64)
		switch {
		case typed < other:
			return -1
		case typed > other:
			return 1
		default:
			return 0
		}
	case time.Time:
		other := rightValue.(time.Time)
		switch {
		case typed.Before(other):
			return -1
		case typed.After(other):
			return 1
		default:
			return 0
		}
	case []byte:
		return bytes.Compare(typed, rightValue.([]byte))
	case bool:
		other := rightValue.(bool)
		switch {
		case typed == other:
			return 0
		case !typed:
			return -1
		default:
			return 1
		}
	default:
		return strings.Compare(fmt.Sprint(leftValue), fmt.Sprint(rightValue))
	}
}

// normalizeScanValue converts a caller value or a stored value into the typed
// shape the memory adapter's value model uses: text as string, integers as
// int64, timestamptz as time.Time and bytea/jsonb as []byte. A value that cannot
// be represented is invalid_input rather than an untyped comparison.
func normalizeScanValue(column store.Column, value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	switch column.Type {
	case "text":
		text, ok := value.(string)
		if !ok {
			return nil, faultInvalid(fmt.Sprintf("column %q expects text; got %T", column.Name, value))
		}
		return text, nil
	case "bigint", "integer":
		switch typed := value.(type) {
		case int:
			return int64(typed), nil
		case int8:
			return int64(typed), nil
		case int16:
			return int64(typed), nil
		case int32:
			return int64(typed), nil
		case int64:
			return typed, nil
		case uint:
			if uint64(typed) > math.MaxInt64 {
				return nil, faultInvalid(fmt.Sprintf("column %q value overflows int64", column.Name))
			}
			return int64(typed), nil
		case uint8:
			return int64(typed), nil
		case uint16:
			return int64(typed), nil
		case uint32:
			return int64(typed), nil
		case uint64:
			if typed > math.MaxInt64 {
				return nil, faultInvalid(fmt.Sprintf("column %q value overflows int64", column.Name))
			}
			return int64(typed), nil
		case float64:
			if math.Trunc(typed) != typed || typed < math.MinInt64 || typed > math.MaxInt64 {
				return nil, faultInvalid(fmt.Sprintf("column %q value %v is not an integer", column.Name, typed))
			}
			return int64(typed), nil
		default:
			return nil, faultInvalid(fmt.Sprintf("column %q expects an integer; got %T", column.Name, value))
		}
	case "timestamptz":
		switch typed := value.(type) {
		case time.Time:
			return typed.UTC(), nil
		case string:
			moment, err := time.Parse(time.RFC3339Nano, typed)
			if err != nil {
				return nil, faultInvalid(fmt.Sprintf("column %q timestamp %q is not RFC3339", column.Name, typed))
			}
			return moment.UTC(), nil
		default:
			return nil, faultInvalid(fmt.Sprintf("column %q expects a timestamp; got %T", column.Name, value))
		}
	case "bytea", "jsonb":
		switch typed := value.(type) {
		case []byte:
			return typed, nil
		case string:
			if decoded, ok := decodeScanBytes(typed); ok {
				return decoded, nil
			}
			return nil, faultInvalid(fmt.Sprintf("column %q bytes are not a valid token", column.Name))
		default:
			return nil, faultInvalid(fmt.Sprintf("column %q expects bytes; got %T", column.Name, value))
		}
	case "boolean":
		boolean, ok := value.(bool)
		if !ok {
			return nil, faultInvalid(fmt.Sprintf("column %q expects a boolean; got %T", column.Name, value))
		}
		return boolean, nil
	default:
		return nil, faultInternal(fmt.Sprintf("column %q has unsupported type %q", column.Name, column.Type))
	}
}

// decodeScanBytes reconstructs a byte value from the base64 text a cursor JSON
// encoding produces.
func decodeScanBytes(text string) ([]byte, bool) {
	if decoded, err := base64.StdEncoding.DecodeString(text); err == nil {
		return decoded, true
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(text); err == nil {
		return decoded, true
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(text); err == nil {
		return decoded, true
	}
	return nil, false
}

// encodeScanCursor builds the opaque keyset token for the last row of a page.
func encodeScanCursor(scope tenancy.Scope, table string, terms []scanTerm, row scanRow) string {
	cursor := scanCursor{
		TenantID:  string(scope.TenantID),
		ProjectID: string(scope.ProjectID),
		Table:     table,
		Order:     make([]scanCursorOrder, 0, len(terms)),
		Key:       make([]any, 0, len(terms)),
	}
	for _, term := range terms {
		cursor.Order = append(cursor.Order, scanCursorOrder{Column: term.column.Name, Desc: term.desc})
		cursor.Key = append(cursor.Key, row.values[term.column.Name])
	}
	data, err := json.Marshal(cursor)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

// decodeScanCursor parses a keyset token and binds it to the caller's scope,
// table and canonical order. A cross-scope token is forbidden; a token for
// another table/order, or a malformed token, is invalid_input.
func decodeScanCursor(token string, scope tenancy.Scope, table string, terms []scanTerm) (scanCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return scanCursor{}, faultInvalid("query cursor is not a valid opaque token")
	}
	var cursor scanCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return scanCursor{}, faultInvalid("query cursor is not a valid opaque token")
	}
	if cursor.TenantID != string(scope.TenantID) || cursor.ProjectID != string(scope.ProjectID) {
		return scanCursor{}, faultForbidden("query cursor belongs to another scope")
	}
	if cursor.Table != table || !sameScanOrder(cursor.Order, terms) {
		return scanCursor{}, faultInvalid("query cursor belongs to another table or order")
	}
	if len(cursor.Order) == 0 || len(cursor.Key) != len(cursor.Order) {
		return scanCursor{}, faultInvalid("query cursor has no keyset")
	}
	for index := range cursor.Key {
		normalized, err := normalizeScanValue(terms[index].column, cursor.Key[index])
		if err != nil {
			return scanCursor{}, faultInvalid("query cursor key is not usable")
		}
		cursor.Key[index] = normalized
	}
	return cursor, nil
}

// sameScanOrder compares a cursor's canonical order with the request's.
func sameScanOrder(left []scanCursorOrder, right []scanTerm) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Column != right[index].column.Name || left[index].Desc != right[index].desc {
			return false
		}
	}
	return true
}
