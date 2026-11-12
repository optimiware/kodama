package pgtest

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Null is the explicit SQL NULL sentinel in a golden row expectation.
type Null struct{}

// NullValue is the single explicit NULL value.
var NullValue = Null{}

// The normalized representation is a closed set of distinct Go types, so a NULL
// can never compare equal to an empty string, zero, false or a JSON object.
type (
	goldenNull  struct{}
	goldenText  string
	goldenInt   int64
	goldenBool  bool
	goldenJSON  string
	goldenBytes string
	goldenTime  time.Time
)

// goldenAsserted records which declared tables a component test whole-row
// asserts after a successful full comparison.
var goldenAsserted = map[string]string{}

// GoldenCoverageError reports every declared table with no successful
// whole-row assertion and no documented exemption.
func GoldenCoverageError(exemptions map[string]string) error {
	var missing []string
	for _, table := range store.SchemaDesign().Tables {
		if _, ok := goldenAsserted[table.Name]; ok {
			continue
		}
		if _, ok := exemptions[table.Name]; ok {
			continue
		}
		missing = append(missing, table.Name)
	}
	if len(missing) > 0 {
		return fmt.Errorf("golden coverage: tables with no whole-row assertion: %v", missing)
	}
	return nil
}

func isGoldenNull(value any) bool {
	_, ok := value.(goldenNull)
	return ok
}

// RequireGoldenRow reads one row through the application role and compares
// EVERY declared column against want. Coverage is recorded only after the whole
// row matches, so a failed or absent comparison never satisfies the registry.
func (s *Shard) RequireGoldenRow(t *testing.T, ctx context.Context, db *pgdriver.DB, scope tenancy.Scope,
	table string, key map[string]any, want map[string]any) {
	t.Helper()
	meta, ok := goldenTable(table)
	if !ok {
		t.Fatalf("golden row: table %q is not in the declared schema", table)
	}
	columns := make([]string, len(meta.Columns))
	columnType := make(map[string]string, len(meta.Columns))
	declared := make(map[string]bool, len(meta.Columns))
	for index, column := range meta.Columns {
		columns[index] = column.Name
		columnType[column.Name] = column.Type
		declared[column.Name] = true
	}
	_ = declared
	keyNames := make([]string, 0, len(key))
	for name := range key {
		keyNames = append(keyNames, name)
	}
	sort.Strings(keyNames)
	predicates := make([]string, 0, len(keyNames))
	args := make([]any, 0, len(keyNames))
	for _, name := range keyNames {
		args = append(args, key[name])
		predicates = append(predicates, fmt.Sprintf("%s = $%d", name, len(args)))
	}
	quoted := make([]string, len(columns))
	for index, name := range columns {
		quoted[index] = `"` + name + `"`
		if columnType[name] == "jsonb" || columnType[name] == "json" {
			// Read the document as text so no driver decode can round a number.
			quoted[index] += `::text`
		}
	}
	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s", strings.Join(quoted, ", "), table, strings.Join(predicates, " AND "))

	conn, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("golden row %s: begin: %v", table, err)
	}
	defer func() { _ = conn.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx,
		`SELECT set_config('kodama.tenant_id', $1, true), set_config('kodama.project_id', $2, true)`,
		string(scope.TenantID), string(scope.ProjectID)); err != nil {
		t.Fatalf("golden row %s: set scope: %v", table, err)
	}
	rows, err := conn.Query(ctx, query, args...)
	if err != nil {
		t.Fatalf("golden row %s: query: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("golden row %s: no row for %v (err=%v)", table, key, rows.Err())
	}
	raw := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for index := range raw {
		pointers[index] = &raw[index]
	}
	if err := rows.Scan(pointers...); err != nil {
		t.Fatalf("golden row %s: scan: %v", table, err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("golden row %s: read: %v", table, err)
	}
	if rows.Next() {
		t.Fatalf("golden row %s: more than one row for %v", table, key)
	}
	rows.Close()

	actualRow := make(map[string]any, len(columns))
	for index, name := range columns {
		actualRow[name] = normalizeGoldenValue(raw[index], columnType[name])
	}
	if err := applyGoldenRow(table, actualRow, want, columnType); err != nil {
		t.Fatalf("golden row %s: %v", table, err)
	}
	recordGoldenAssertion(t, table, fmt.Sprint(actualRow["id"]))
}

// validateGoldenColumns checks that the expectation states exactly the declared
// column set: no undeclared key, no omitted column and no duplicate declaration.
func validateGoldenColumns(declared []string, want map[string]any) error {
	seen := make(map[string]bool, len(declared))
	for _, name := range declared {
		if seen[name] {
			return fmt.Errorf("declared column %q is duplicated", name)
		}
		seen[name] = true
	}
	for name := range want {
		if !seen[name] {
			return fmt.Errorf("expectation names undeclared column %q", name)
		}
	}
	for _, name := range declared {
		if _, ok := want[name]; !ok {
			return fmt.Errorf("column %q is not stated in the expectation", name)
		}
	}
	return nil
}

// errGoldenNullMismatch marks a comparison where exactly one side is NULL.
var errGoldenNullMismatch = errors.New("NULL mismatch")

// applyGoldenRow compares one already-read row and records the table only after
// every column comparison succeeded. It reports every mismatching column at once
// so one run shows the whole divergence of a row.
func applyGoldenRow(table string, actual, want map[string]any, columnType map[string]string) error {
	declared := make([]string, 0, len(columnType))
	for name := range columnType {
		declared = append(declared, name)
	}
	sort.Strings(declared)
	if err := validateGoldenColumns(declared, want); err != nil {
		return err
	}
	var failures []string
	for _, name := range declared {
		expected, err := normalizeGoldenExpected(want[name], columnType[name])
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: expectation: %v", name, err))
			continue
		}
		if matcher, loose := expected.(GoldenMatcher); loose {
			if err := matcherAllowed(name, columnType[name], matcher); err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", name, err))
				continue
			}
		}
		if err := goldenCompare(actual[name], expected); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", name, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d column(s) differ: %s", len(failures), strings.Join(failures, "; "))
	}
	goldenAsserted[table] = "RequireGoldenRow"
	return nil
}

// goldenCompare compares one normalized actual value with one expectation.
func goldenCompare(actual, expected any) error {
	if matcher, ok := expected.(GoldenMatcher); ok {
		if isGoldenNull(actual) {
			return fmt.Errorf("actual NULL; matcher rejected NULL")
		}
		if err := matcher.match(actual); err != nil {
			return fmt.Errorf("actual %#v; matcher failed: %w", actual, err)
		}
		return nil
	}
	if isGoldenNull(actual) != isGoldenNull(expected) {
		return fmt.Errorf("actual %#v; want %#v: %w", actual, expected, errGoldenNullMismatch)
	}
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("actual %#v; want %#v", actual, expected)
	}
	return nil
}

func goldenTable(name string) (store.Table, bool) {
	for _, table := range store.SchemaDesign().Tables {
		if table.Name == name {
			return table, true
		}
	}
	return store.Table{}, false
}

func normalizeGoldenValue(value any, columnType string) any {
	if value == nil {
		return goldenNull{}
	}
	switch columnType {
	case "jsonb", "json":
		var document []byte
		switch typed := value.(type) {
		case []byte:
			document = typed
		case string:
			document = []byte(typed)
		default:
			encoded, err := json.Marshal(value)
			if err != nil {
				return goldenJSON(fmt.Sprint(value))
			}
			document = encoded
		}
		canonical, err := canonicalJSON(document)
		if err != nil {
			return goldenJSON(string(document))
		}
		return goldenJSON(canonical)
	case "bytea":
		if data, ok := value.([]byte); ok {
			return goldenBytes(hex.EncodeToString(data))
		}
		return goldenBytes(fmt.Sprint(value))
	case "timestamptz", "timestamp":
		if stamp, ok := value.(time.Time); ok {
			return goldenTime(stamp.UTC())
		}
		return goldenText(fmt.Sprint(value))
	case "boolean":
		if flag, ok := value.(bool); ok {
			return goldenBool(flag)
		}
		return goldenText(fmt.Sprint(value))
	default:
		switch typed := value.(type) {
		case string:
			return goldenText(typed)
		case int64:
			return goldenInt(typed)
		case int32:
			return goldenInt(int64(typed))
		case int:
			return goldenInt(int64(typed))
		case bool:
			return goldenBool(typed)
		default:
			return goldenText(fmt.Sprint(value))
		}
	}
}

func normalizeGoldenExpected(value any, columnType string) (any, error) {
	switch typed := value.(type) {
	case Null, goldenNull, nil:
		return goldenNull{}, nil
	case GoldenMatcher:
		return typed, nil
	case time.Time:
		return goldenTime(typed.UTC()), nil
	case []byte:
		return goldenBytes(hex.EncodeToString(typed)), nil
	case string:
		if columnType == "jsonb" || columnType == "json" {
			canonical, err := canonicalJSON([]byte(typed))
			if err != nil {
				return nil, fmt.Errorf("invalid JSON expectation %q: %w", typed, err)
			}
			return goldenJSON(canonical), nil
		}
		return goldenText(typed), nil
	case int:
		return goldenInt(int64(typed)), nil
	case int64:
		return goldenInt(typed), nil
	case bool:
		return goldenBool(typed), nil
	default:
		return goldenText(fmt.Sprint(value)), nil
	}
}
