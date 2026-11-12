// Scoped multi-row reads for the PostgreSQL adapter.
//
// Scanner is a separate read seam, not a repository. Query validates the
// structured request against the declared schema, opens exactly one read-only
// transaction with the same transaction-local scope as Read, renders one plain
// parameterized SELECT from allow-listed identifiers and binds every caller
// value, and returns one deterministic keyset page. It never writes, locks or
// publishes, and the transaction completes so a pooled connection cannot leak
// the scope.
package postgres

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// beginReadOnlySQL is the first statement of a scanner transaction. It runs
// before the scope setting because PostgreSQL requires SET TRANSACTION before
// any query in the transaction.
const beginReadOnlySQL = "SET TRANSACTION READ ONLY"

// Query implements store.Scanner over the scoped PostgreSQL store.
func (s *Store) Query(ctx context.Context, scope tenancy.Scope, query store.ScanQuery) (store.QueryPage, error) {
	if err := ctx.Err(); err != nil {
		return store.QueryPage{}, err
	}
	if err := scope.Validate(); err != nil {
		return store.QueryPage{}, err
	}
	if err := query.Validate(); err != nil {
		return store.QueryPage{}, err
	}
	meta, ok := lookupTable(query.Table)
	if !ok {
		return store.QueryPage{}, faultInvalid(fmt.Sprintf("query table %q is not in the frozen schema", query.Table))
	}
	statement, err := buildScanStatement(meta, scope, query)
	if err != nil {
		return store.QueryPage{}, err
	}
	tx, err := s.beginRead(ctx, scope)
	if err != nil {
		return store.QueryPage{}, err
	}
	page, err := readScanPage(ctx, tx, meta, scope, query.Table, statement, query.Limit)
	if err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return store.QueryPage{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.QueryPage{}, err
	}
	return page, nil
}

// scanStatement is one rendered scanner read: the parameterized statement and
// the canonical order (the requested terms plus the id tie-breaker) the cursor
// is bound to.
type scanStatement struct {
	query store.Query
	order []store.Order
}

func buildScanStatement(meta *tableMeta, scope tenancy.Scope, query store.ScanQuery) (scanStatement, error) {
	if meta.projectRequired && scope.TenantScoped() {
		return scanStatement{}, faultInvalid(fmt.Sprintf("table %q is project scoped; a tenant-scope query is not allowed", meta.table.Name))
	}
	order := canonicalScanOrder(query.OrderBy)
	var cursor *queryCursor
	if query.After != "" {
		decoded, err := decodeQueryCursor(query.After, meta, scope, order, query.Table)
		if err != nil {
			return scanStatement{}, err
		}
		cursor = decoded
	}

	args := make([]any, 0, 8)
	predicates := make([]string, 0, 8)
	addArgument := func(value any) int {
		args = append(args, value)
		return len(args)
	}
	addPredicate := func(predicate string) {
		predicates = append(predicates, predicate)
	}
	addComparison := func(column, operator string, value any) {
		index := addArgument(value)
		addPredicate(fmt.Sprintf("%s %s $%d", quoteIdentifier(column), operator, index))
	}

	// The adapter always applies the scope predicate. A caller equality on the
	// same columns is ANDed with it and can only narrow the read.
	if meta.hasTenant {
		addComparison("tenant_id", "=", string(scope.TenantID))
	}
	if meta.hasProject {
		if scope.TenantScoped() {
			addPredicate(fmt.Sprintf("%s IS NULL", quoteIdentifier("project_id")))
		} else {
			addComparison("project_id", "=", string(scope.ProjectID))
		}
	} else {
		switch meta.table.Name {
		case "tenants":
			// The tenant root row carries its identity instead of tenant_id.
			addComparison("id", "=", string(scope.TenantID))
		case "projects":
			// tenant_id is applied above; a project scope also reads only its
			// own project row.
			if scope.ProjectScoped() {
				addComparison("id", "=", string(scope.ProjectID))
			}
		}
	}

	for _, column := range sortedScanColumns(query.Equals) {
		addComparison(column, "=", query.Equals[column])
	}
	for _, filter := range query.Range {
		if filter.After != nil {
			addComparison(filter.Column, ">", filter.After)
		}
		if filter.Before != nil {
			addComparison(filter.Column, "<", filter.Before)
		}
	}
	if cursor != nil {
		addPredicate(keysetPredicate(order, cursor.Key, addArgument))
	}

	limitIndex := addArgument(query.Limit + 1)
	selectList := make([]string, 0, len(meta.columnOrder))
	for _, column := range meta.columnOrder {
		selectList = append(selectList, quoteIdentifier(column))
	}
	orderList := make([]string, 0, len(order))
	for _, term := range order {
		direction := "ASC"
		if term.Desc {
			direction = "DESC"
		}
		orderList = append(orderList, fmt.Sprintf("%s %s", quoteIdentifier(term.Column), direction))
	}
	statement := fmt.Sprintf(
		"SELECT %s FROM %s",
		strings.Join(selectList, ", "),
		quoteIdentifier(meta.table.Name),
	)
	if len(predicates) > 0 {
		statement += " WHERE " + strings.Join(predicates, " AND ")
	}
	statement += fmt.Sprintf(
		" ORDER BY %s LIMIT $%d",
		strings.Join(orderList, ", "),
		limitIndex,
	)
	return scanStatement{query: store.Query{SQL: statement, Args: args}, order: order}, nil
}

// canonicalScanOrder appends the row id as the final ascending tie-breaker when
// the requested order does not already end with it.
func canonicalScanOrder(requested []store.Order) []store.Order {
	order := append([]store.Order(nil), requested...)
	if len(order) == 0 || order[len(order)-1].Column != "id" {
		order = append(order, store.Order{Column: "id"})
	}
	return order
}

// keysetPredicate renders the strict comparison that follows the cursor. A
// uniform direction uses a row-value comparison; mixed directions use the
// equivalent expanded OR chain so every column keeps its own direction.
func keysetPredicate(order []store.Order, key []any, addArgument func(any) int) string {
	indexes := make([]int, len(order))
	for position := range order {
		indexes[position] = addArgument(key[position])
	}
	if len(order) == 1 {
		operator := ">"
		if order[0].Desc {
			operator = "<"
		}
		return fmt.Sprintf("%s %s $%d", quoteIdentifier(order[0].Column), operator, indexes[0])
	}
	if uniformScanDirection(order) {
		operator := ">"
		if order[0].Desc {
			operator = "<"
		}
		columns := make([]string, len(order))
		parameters := make([]string, len(order))
		for position, term := range order {
			columns[position] = quoteIdentifier(term.Column)
			parameters[position] = fmt.Sprintf("$%d", indexes[position])
		}
		return fmt.Sprintf("(%s) %s (%s)", strings.Join(columns, ", "), operator, strings.Join(parameters, ", "))
	}
	clauses := make([]string, len(order))
	for position, term := range order {
		parts := make([]string, 0, position+1)
		for earlier := range position {
			parts = append(parts, fmt.Sprintf("%s = $%d", quoteIdentifier(order[earlier].Column), indexes[earlier]))
		}
		operator := ">"
		if term.Desc {
			operator = "<"
		}
		parts = append(parts, fmt.Sprintf("%s %s $%d", quoteIdentifier(term.Column), operator, indexes[position]))
		clauses[position] = "(" + strings.Join(parts, " AND ") + ")"
	}
	return "(" + strings.Join(clauses, " OR ") + ")"
}

func uniformScanDirection(order []store.Order) bool {
	for _, term := range order[1:] {
		if term.Desc != order[0].Desc {
			return false
		}
	}
	return true
}

func sortedScanColumns(equals map[string]any) []string {
	columns := make([]string, 0, len(equals))
	for column := range equals {
		columns = append(columns, column)
	}
	sort.Strings(columns)
	return columns
}

// quoteIdentifier quotes one schema identifier. The name always comes from the
// declared schema allow-list; quoting keeps the statement independent of
// keyword or case accidents and never interpolates caller text.
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// readScanPage reads at most limit+1 rows, computes HasMore and builds the
// opaque next cursor from the last returned row. Every decoded row is checked
// against the transaction scope as a second line of defence behind the SQL
// predicate.
func readScanPage(ctx context.Context, tx store.Transaction, meta *tableMeta, scope tenancy.Scope, table string, statement scanStatement, limit int) (store.QueryPage, error) {
	rows, err := tx.Query(ctx, statement.query)
	if err != nil {
		return store.QueryPage{}, err
	}
	defer func() { _ = rows.Close() }()

	decoded := make([]map[string]any, 0, limit+1)
	for rows.Next() {
		values, err := scanColumns(meta, meta.columnOrder, rows)
		if err != nil {
			return store.QueryPage{}, err
		}
		if err := checkRowScope(meta, scope, values); err != nil {
			return store.QueryPage{}, err
		}
		decoded = append(decoded, values)
	}
	if err := rows.Err(); err != nil {
		return store.QueryPage{}, err
	}

	page := store.QueryPage{}
	if len(decoded) > limit {
		page.HasMore = true
		decoded = decoded[:limit]
	}
	page.Rows = make([]store.Row, len(decoded))
	for index, values := range decoded {
		page.Rows[index] = store.Row{Table: table, Values: values}
	}
	if page.HasMore {
		cursor := encodeQueryCursor(scope, table, statement.order, decoded[len(decoded)-1])
		if cursor == "" {
			return store.QueryPage{}, faultInternal("scanner could not encode the next cursor")
		}
		page.Next = cursor
	}
	return page, nil
}

// queryCursor is the opaque keyset token. It carries the scope, table and
// canonical order it was produced for, plus the typed key values of the last
// returned row.
type queryCursor struct {
	TenantID  string        `json:"tenant_id"`
	ProjectID string        `json:"project_id"`
	Table     string        `json:"table"`
	Order     []cursorOrder `json:"order"`
	Key       []any         `json:"key"`
}

type cursorOrder struct {
	Column string `json:"column"`
	Desc   bool   `json:"desc"`
}

func encodeQueryCursor(scope tenancy.Scope, table string, order []store.Order, values map[string]any) string {
	cursor := queryCursor{
		TenantID:  string(scope.TenantID),
		ProjectID: string(scope.ProjectID),
		Table:     table,
		Order:     make([]cursorOrder, 0, len(order)),
		Key:       make([]any, 0, len(order)),
	}
	for _, term := range order {
		cursor.Order = append(cursor.Order, cursorOrder{Column: term.Column, Desc: term.Desc})
		cursor.Key = append(cursor.Key, values[term.Column])
	}
	data, err := json.Marshal(cursor)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeQueryCursor(token string, meta *tableMeta, scope tenancy.Scope, order []store.Order, table string) (*queryCursor, error) {
	if len(token) > store.MaxQueryCursorLength {
		return nil, faultInvalid(fmt.Sprintf("query cursor is longer than %d bytes", store.MaxQueryCursorLength))
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, faultInvalid("query cursor is not a valid opaque token")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var cursor queryCursor
	if err := decoder.Decode(&cursor); err != nil {
		return nil, faultInvalid("query cursor is not a valid opaque token")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, faultInvalid("query cursor has trailing content")
	}
	if cursor.Table == "" || len(cursor.Order) == 0 || len(cursor.Key) != len(cursor.Order) {
		return nil, faultInvalid("query cursor has no complete keyset")
	}
	if cursor.TenantID != string(scope.TenantID) || cursor.ProjectID != string(scope.ProjectID) {
		return nil, faultForbidden("query cursor belongs to another scope")
	}
	if cursor.Table != table || !sameCursorOrder(cursor.Order, order) {
		return nil, faultInvalid("query cursor belongs to another table or order")
	}
	typed := make([]any, len(cursor.Key))
	for position, term := range order {
		column, ok := meta.columns[term.Column]
		if !ok {
			return nil, faultInvalid(fmt.Sprintf("query cursor names unknown column %q", term.Column))
		}
		value, err := decodeCursorValue(column, cursor.Key[position])
		if err != nil {
			return nil, err
		}
		typed[position] = value
	}
	cursor.Key = typed
	return &cursor, nil
}

func sameCursorOrder(left []cursorOrder, right []store.Order) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Column != right[index].Column || left[index].Desc != right[index].Desc {
			return false
		}
	}
	return true
}

// decodeCursorValue restores the schema type of one cursor key so the bound
// parameter keeps the column's type instead of a JSON number or string.
func decodeCursorValue(column store.Column, value any) (any, error) {
	switch column.Type {
	case "text":
		text, ok := value.(string)
		if !ok {
			return nil, faultInvalid(fmt.Sprintf("query cursor text key %q is not a string", column.Name))
		}
		return text, nil
	case "bigint", "integer":
		number, ok := value.(json.Number)
		if !ok {
			return nil, faultInvalid(fmt.Sprintf("query cursor integer key %q is not a number", column.Name))
		}
		parsed, err := number.Int64()
		if err != nil {
			return nil, faultInvalid(fmt.Sprintf("query cursor integer key %q is not an int64", column.Name))
		}
		return parsed, nil
	case "boolean":
		flag, ok := value.(bool)
		if !ok {
			return nil, faultInvalid(fmt.Sprintf("query cursor boolean key %q is not a bool", column.Name))
		}
		return flag, nil
	case "timestamptz":
		text, ok := value.(string)
		if !ok {
			return nil, faultInvalid(fmt.Sprintf("query cursor timestamp key %q is not a string", column.Name))
		}
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			return nil, faultInvalid(fmt.Sprintf("query cursor timestamp key %q is not RFC3339", column.Name))
		}
		return parsed, nil
	case "bytea", "jsonb":
		text, ok := value.(string)
		if !ok {
			return nil, faultInvalid(fmt.Sprintf("query cursor byte key %q is not a string", column.Name))
		}
		decoded, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return nil, faultInvalid(fmt.Sprintf("query cursor byte key %q is not base64", column.Name))
		}
		return decoded, nil
	default:
		return value, nil
	}
}
