package adaptercontract

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestMaxQueryLimit pins the required positive page bound.
func TestMaxQueryLimit(t *testing.T) {
	if store.MaxQueryLimit != 500 {
		t.Fatalf("MaxQueryLimit = %d; want 500", store.MaxQueryLimit)
	}
	if store.MaxQueryCursorLength <= 0 {
		t.Fatalf("MaxQueryCursorLength = %d; want a positive bound", store.MaxQueryCursorLength)
	}
}

// TestScanQueryValidate pins the shared declaration-level validation vectors:
// limit bounds, table/column allow-list, duplicate columns, range shape and
// cursor shape. Adapters call the same methods, so these cases are the contract.
func TestScanQueryValidate(t *testing.T) {
	longCursor := strings.Repeat("a", store.MaxQueryCursorLength+1)
	cases := []struct {
		name  string
		query store.ScanQuery
		want  fault.Class
	}{
		{
			name:  "minimal valid query",
			query: store.ScanQuery{Table: "executions", Limit: 1},
		},
		{
			name: "full valid query",
			query: store.ScanQuery{
				Table:   "executions",
				Equals:  map[string]any{"state": "queued", "tenant_id": "tenant-1"},
				Range:   []store.RangeFilter{{Column: "created_at", After: "2026-01-01T00:00:00Z"}},
				OrderBy: []store.Order{{Column: "created_at"}, {Column: "id", Desc: true}},
				After:   "cursor_ABC-123",
				Limit:   store.MaxQueryLimit,
			},
		},
		{
			name:  "empty order is id order",
			query: store.ScanQuery{Table: "executions", Limit: 10},
		},
		{
			name:  "limit zero is refused",
			query: store.ScanQuery{Table: "executions", Limit: 0},
			want:  fault.InvalidInput,
		},
		{
			name:  "limit above maximum is refused",
			query: store.ScanQuery{Table: "executions", Limit: store.MaxQueryLimit + 1},
			want:  fault.InvalidInput,
		},
		{
			name:  "negative limit is refused",
			query: store.ScanQuery{Table: "executions", Limit: -1},
			want:  fault.InvalidInput,
		},
		{
			name:  "empty table is refused",
			query: store.ScanQuery{Table: "", Limit: 10},
			want:  fault.InvalidInput,
		},
		{
			name:  "unknown table is refused",
			query: store.ScanQuery{Table: "unknown", Limit: 10},
			want:  fault.InvalidInput,
		},
		{
			name:  "unknown equality column is refused",
			query: store.ScanQuery{Table: "executions", Equals: map[string]any{"nope": "x"}, Limit: 10},
			want:  fault.InvalidInput,
		},
		{
			name:  "empty equality column is refused",
			query: store.ScanQuery{Table: "executions", Equals: map[string]any{"": "x"}, Limit: 10},
			want:  fault.InvalidInput,
		},
		{
			name:  "nil equality value is refused",
			query: store.ScanQuery{Table: "executions", Equals: map[string]any{"state": nil}, Limit: 10},
			want:  fault.InvalidInput,
		},
		{
			name:  "unknown range column is refused",
			query: store.ScanQuery{Table: "executions", Range: []store.RangeFilter{{Column: "nope", After: "a"}}, Limit: 10},
			want:  fault.InvalidInput,
		},
		{
			name:  "empty range column is refused",
			query: store.ScanQuery{Table: "executions", Range: []store.RangeFilter{{Column: "", After: "a"}}, Limit: 10},
			want:  fault.InvalidInput,
		},
		{
			name:  "range without a bound is refused",
			query: store.ScanQuery{Table: "executions", Range: []store.RangeFilter{{Column: "created_at"}}, Limit: 10},
			want:  fault.InvalidInput,
		},
		{
			name:  "duplicate range column is refused",
			query: store.ScanQuery{Table: "executions", Range: []store.RangeFilter{{Column: "created_at", After: "a"}, {Column: "created_at", Before: "z"}}, Limit: 10},
			want:  fault.InvalidInput,
		},
		{
			name:  "unknown order column is refused",
			query: store.ScanQuery{Table: "executions", OrderBy: []store.Order{{Column: "nope"}}, Limit: 10},
			want:  fault.InvalidInput,
		},
		{
			name:  "empty order column is refused",
			query: store.ScanQuery{Table: "executions", OrderBy: []store.Order{{Column: ""}}, Limit: 10},
			want:  fault.InvalidInput,
		},
		{
			name:  "duplicate order column is refused",
			query: store.ScanQuery{Table: "executions", OrderBy: []store.Order{{Column: "created_at"}, {Column: "created_at", Desc: true}}, Limit: 10},
			want:  fault.InvalidInput,
		},
		{
			name:  "id order must be final",
			query: store.ScanQuery{Table: "executions", OrderBy: []store.Order{{Column: "id"}, {Column: "created_at"}}, Limit: 10},
			want:  fault.InvalidInput,
		},
		{
			name:  "cursor with whitespace is refused",
			query: store.ScanQuery{Table: "executions", After: "not a cursor", Limit: 10},
			want:  fault.InvalidInput,
		},
		{
			name:  "cursor with a control byte is refused",
			query: store.ScanQuery{Table: "executions", After: "cursor\x00value", Limit: 10},
			want:  fault.InvalidInput,
		},
		{
			name:  "cursor with a non-ASCII byte is refused",
			query: store.ScanQuery{Table: "executions", After: "cursor\xffvalue", Limit: 10},
			want:  fault.InvalidInput,
		},
		{
			name:  "overlong cursor is refused",
			query: store.ScanQuery{Table: "executions", After: longCursor, Limit: 10},
			want:  fault.InvalidInput,
		},
		{
			name:  "nullable order column is refused",
			query: store.ScanQuery{Table: "events", OrderBy: []store.Order{{Column: "project_id"}}, Limit: 10},
			want:  fault.InvalidInput,
		},
		{
			name:  "nullable range column is refused",
			query: store.ScanQuery{Table: "events", Range: []store.RangeFilter{{Column: "project_id", After: "a"}}, Limit: 10},
			want:  fault.InvalidInput,
		},
		{
			name:  "nullable equality column is allowed",
			query: store.ScanQuery{Table: "events", Equals: map[string]any{"project_id": "project-1"}, Limit: 10},
		},
		{
			name:  "nil equality on a nullable column is refused",
			query: store.ScanQuery{Table: "events", Equals: map[string]any{"project_id": nil}, Limit: 10},
			want:  fault.InvalidInput,
		},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.query.Validate()
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("Validate() = %v; want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil; want fault class %q", testCase.want)
			}
			class, ok := fault.ClassOf(err)
			if !ok || class != testCase.want {
				t.Fatalf("Validate() class = %q (fault=%v); want %q", class, ok, testCase.want)
			}
		})
	}
}

// TestScannerConformanceVectors runs the executable scanner vectors against a
// trivial in-package reference interpreter. Adapters port the same
// testdata/scanner.json vectors; this test proves the vectors describe a real
// scoped keyset read rather than labels.
func TestScannerConformanceVectors(t *testing.T) {
	ScannerConformance(t, func(_ *testing.T, seed []store.Row) store.Scanner {
		return newReferenceScanner(seed)
	})
}

// TestScannerVectorsDetectBrokenAdapter proves the vectors are not vacuous: an
// implementation that ignores the page limit must fail at least one vector.
func TestScannerVectorsDetectBrokenAdapter(t *testing.T) {
	document := loadScannerVectors(t)
	validateScannerVectors(t, document)
	failures := 0
	for _, vector := range document.Vectors {
		scanner := brokenScanner{newReferenceScanner(scannerSeedRows(t, vector.Seed))}
		if err := runScannerVector(vector, scanner); err != nil {
			failures++
		}
	}
	if failures == 0 {
		t.Fatal("no scanner vector failed against the deliberately non-conforming fake")
	}
}

// ScannerConformance runs every scanner vector against newScanner. newScanner
// receives the vector's seeded committed rows. A conforming adapter passes
// every vector; the vectors are the shared semantic input the store adapters
// port.
func ScannerConformance(t *testing.T, newScanner func(t *testing.T, seed []store.Row) store.Scanner) {
	t.Helper()
	document := loadScannerVectors(t)
	validateScannerVectors(t, document)
	for _, vector := range document.Vectors {
		vector := vector
		t.Run(vector.Name, func(t *testing.T) {
			scanner := newScanner(t, scannerSeedRows(t, vector.Seed))
			if err := runScannerVector(vector, scanner); err != nil {
				t.Fatalf("vector %q: %v", vector.Name, err)
			}
		})
	}
}

type scannerVectorDocument struct {
	Version int             `json:"version"`
	Vectors []scannerVector `json:"vectors"`
}

type scannerVector struct {
	Name  string        `json:"name"`
	Kind  string        `json:"kind"`
	Seed  scannerSeed   `json:"seed"`
	Steps []scannerStep `json:"steps"`
}

type scannerSeed struct {
	Rows []scannerSeedRow `json:"rows"`
}

type scannerSeedRow struct {
	Table     string         `json:"table"`
	TenantID  string         `json:"tenant_id"`
	ProjectID string         `json:"project_id"`
	ID        string         `json:"id"`
	Values    map[string]any `json:"values"`
}

type scannerScope struct {
	TenantID  string `json:"tenant_id"`
	ProjectID string `json:"project_id"`
}

type scannerRange struct {
	Column string `json:"column"`
	After  any    `json:"after"`
	Before any    `json:"before"`
}

type scannerOrder struct {
	Column string `json:"column"`
	Desc   bool   `json:"desc"`
}

type scannerStep struct {
	Op          string          `json:"op"`
	Scope       *scannerScope   `json:"scope"`
	Table       string          `json:"table"`
	Equals      map[string]any  `json:"equals"`
	Range       []scannerRange  `json:"range"`
	OrderBy     []scannerOrder  `json:"order_by"`
	AfterStep   *int            `json:"after_step"`
	After       string          `json:"after"`
	Limit       int             `json:"limit"`
	Row         *scannerSeedRow `json:"row"`
	WantIDs     []string        `json:"want_ids"`
	WantNext    *bool           `json:"want_next"`
	WantHasMore *bool           `json:"want_has_more"`
	WantError   string          `json:"want_error"`
	WantCount   *int            `json:"want_count"`
}

var scannerVectorKinds = map[string]bool{
	"scope":         true,
	"pagination":    true,
	"determinism":   true,
	"cursor":        true,
	"limit":         true,
	"invalid_input": true,
	"readonly":      true,
}

var scannerVectorErrors = map[string]bool{
	"invalid_input": true,
	"forbidden":     true,
}

func loadScannerVectors(t *testing.T) scannerVectorDocument {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "scanner.json"))
	if err != nil {
		t.Fatalf("read scanner conformance vectors: %v", err)
	}
	var document scannerVectorDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode scanner conformance vectors: %v", err)
	}
	return document
}

func validateScannerVectors(t *testing.T, document scannerVectorDocument) {
	t.Helper()
	if document.Version != 1 {
		t.Fatalf("scanner vector document version = %d; want 1", document.Version)
	}
	if len(document.Vectors) == 0 {
		t.Fatal("scanner conformance vector set is empty")
	}
	names := make(map[string]bool, len(document.Vectors))
	kinds := make(map[string]int, len(scannerVectorKinds))
	for _, vector := range document.Vectors {
		if vector.Name == "" || vector.Kind == "" {
			t.Errorf("incomplete scanner vector %+v", vector)
			continue
		}
		if names[vector.Name] {
			t.Errorf("duplicate scanner vector name %q", vector.Name)
		}
		names[vector.Name] = true
		if !scannerVectorKinds[vector.Kind] {
			t.Errorf("vector %q has unknown kind %q", vector.Name, vector.Kind)
		}
		kinds[vector.Kind]++
		if len(vector.Steps) == 0 {
			t.Errorf("vector %q has no steps", vector.Name)
			continue
		}
		queries := 0
		for index, step := range vector.Steps {
			switch step.Op {
			case "query":
				queries++
				if step.Scope == nil {
					t.Errorf("vector %q step %d queries without a scope", vector.Name, index)
				}
				if step.Table == "" {
					t.Errorf("vector %q step %d queries without a table", vector.Name, index)
				}
				if step.AfterStep != nil {
					if *step.AfterStep < 0 || *step.AfterStep >= index {
						t.Errorf("vector %q step %d after_step %d is not a prior step", vector.Name, index, *step.AfterStep)
					} else if vector.Steps[*step.AfterStep].Op != "query" {
						t.Errorf("vector %q step %d after_step %d does not name a query", vector.Name, index, *step.AfterStep)
					}
				}
				if step.WantError == "" && step.WantIDs == nil && step.WantHasMore == nil && step.WantNext == nil {
					t.Errorf("vector %q step %d asserts nothing", vector.Name, index)
				}
			case "insert":
				if step.Row == nil {
					t.Errorf("vector %q step %d inserts no row", vector.Name, index)
				}
			case "inspect":
				if step.Table == "" || step.WantCount == nil {
					t.Errorf("vector %q step %d inspects without a table/count", vector.Name, index)
				}
			default:
				t.Errorf("vector %q step %d has unknown op %q", vector.Name, index, step.Op)
			}
			if step.WantError != "" && !scannerVectorErrors[step.WantError] {
				t.Errorf("vector %q step %d expects unknown error class %q", vector.Name, index, step.WantError)
			}
		}
		if queries == 0 {
			t.Errorf("vector %q has no query step", vector.Name)
		}
	}
	required := []string{"scope", "pagination", "determinism", "cursor", "limit", "invalid_input", "readonly"}
	for _, kind := range required {
		if kinds[kind] == 0 {
			t.Errorf("scanner conformance vectors are missing kind %q", kind)
		}
	}
	if !hasScannerStep(document, func(step scannerStep) bool { return step.AfterStep != nil }) {
		t.Error("scanner vectors never paginate from a keyset cursor")
	}
	if !hasScannerStep(document, func(step scannerStep) bool { return step.Op == "insert" }) {
		t.Error("scanner vectors never inject a concurrent insert")
	}
	if !hasScannerStep(document, func(step scannerStep) bool { return step.WantError == "forbidden" }) {
		t.Error("scanner vectors never refuse a cross-scope cursor")
	}
	if !hasScannerStep(document, func(step scannerStep) bool { return step.WantError == "invalid_input" }) {
		t.Error("scanner vectors never refuse an invalid query")
	}
}

func hasScannerStep(document scannerVectorDocument, match func(scannerStep) bool) bool {
	for _, vector := range document.Vectors {
		for _, step := range vector.Steps {
			if match(step) {
				return true
			}
		}
	}
	return false
}

func scannerSeedRows(t *testing.T, seed scannerSeed) []store.Row {
	t.Helper()
	rows := make([]store.Row, 0, len(seed.Rows))
	for _, row := range seed.Rows {
		rows = append(rows, scannerStoreRow(row))
	}
	return rows
}

func scannerStoreRow(row scannerSeedRow) store.Row {
	values := make(map[string]any, len(row.Values)+3)
	for column, value := range row.Values {
		values[column] = value
	}
	values["tenant_id"] = row.TenantID
	values["project_id"] = row.ProjectID
	values["id"] = row.ID
	return store.Row{Table: row.Table, Values: values}
}

func runScannerVector(vector scannerVector, scanner store.Scanner) error {
	next := make([]string, len(vector.Steps))
	for index, step := range vector.Steps {
		switch step.Op {
		case "query":
			if step.Scope == nil {
				return fmt.Errorf("step %d has no scope", index)
			}
			query := store.ScanQuery{
				Table:   step.Table,
				Equals:  step.Equals,
				Range:   scannerRangeFilters(step.Range),
				OrderBy: scannerOrderTerms(step.OrderBy),
				After:   step.After,
				Limit:   step.Limit,
			}
			if step.AfterStep != nil {
				query.After = next[*step.AfterStep]
			}
			scope := tenancy.Scope{
				TenantID:  id.TenantID(step.Scope.TenantID),
				ProjectID: id.ProjectID(step.Scope.ProjectID),
			}
			page, err := scanner.Query(context.Background(), scope, query)
			if step.WantError != "" {
				if err == nil {
					return fmt.Errorf("step %d succeeded; want error class %q", index, step.WantError)
				}
				class, ok := fault.ClassOf(err)
				if !ok || string(class) != step.WantError {
					return fmt.Errorf("step %d error = %v (class=%q); want class %q", index, err, class, step.WantError)
				}
				continue
			}
			if err != nil {
				return fmt.Errorf("step %d: %w", index, err)
			}
			if step.WantIDs != nil {
				got := make([]string, 0, len(page.Rows))
				for _, row := range page.Rows {
					got = append(got, scannerString(row.Values["id"]))
				}
				if !equalStrings(got, step.WantIDs) {
					return fmt.Errorf("step %d ids = %v; want %v", index, got, step.WantIDs)
				}
			}
			if step.WantHasMore != nil && page.HasMore != *step.WantHasMore {
				return fmt.Errorf("step %d HasMore = %v; want %v", index, page.HasMore, *step.WantHasMore)
			}
			if step.WantNext != nil && (page.Next != "") != *step.WantNext {
				return fmt.Errorf("step %d Next = %q; want non-empty=%v", index, page.Next, *step.WantNext)
			}
			if page.HasMore && page.Next == "" {
				return fmt.Errorf("step %d reports HasMore without a next cursor", index)
			}
			if !page.HasMore && page.Next != "" {
				return fmt.Errorf("step %d reports no HasMore but a next cursor", index)
			}
			next[index] = page.Next
		case "insert":
			inserter, ok := scanner.(interface{ Insert(store.Row) })
			if !ok {
				return fmt.Errorf("step %d: scanner does not accept seeded rows", index)
			}
			if step.Row == nil {
				return fmt.Errorf("step %d inserts no row", index)
			}
			inserter.Insert(scannerStoreRow(*step.Row))
		case "inspect":
			counter, ok := scanner.(interface {
				Count(string, *tenancy.Scope) int
			})
			if !ok {
				return fmt.Errorf("step %d: scanner cannot be inspected", index)
			}
			var scope *tenancy.Scope
			if step.Scope != nil {
				value := tenancy.Scope{
					TenantID:  id.TenantID(step.Scope.TenantID),
					ProjectID: id.ProjectID(step.Scope.ProjectID),
				}
				scope = &value
			}
			if step.WantCount == nil {
				return fmt.Errorf("step %d inspects without a count", index)
			}
			if got := counter.Count(step.Table, scope); got != *step.WantCount {
				return fmt.Errorf("step %d count(%s) = %d; want %d", index, step.Table, got, *step.WantCount)
			}
		default:
			return fmt.Errorf("step %d has unknown op %q", index, step.Op)
		}
	}
	return nil
}

func scannerRangeFilters(filters []scannerRange) []store.RangeFilter {
	if filters == nil {
		return nil
	}
	converted := make([]store.RangeFilter, 0, len(filters))
	for _, filter := range filters {
		converted = append(converted, store.RangeFilter{Column: filter.Column, After: filter.After, Before: filter.Before})
	}
	return converted
}

func scannerOrderTerms(orders []scannerOrder) []store.Order {
	if orders == nil {
		return nil
	}
	converted := make([]store.Order, 0, len(orders))
	for _, order := range orders {
		converted = append(converted, store.Order{Column: order.Column, Desc: order.Desc})
	}
	return converted
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func scannerString(value any) string {
	text, _ := value.(string)
	return text
}

// scannerRow is the reference interpreter's committed row. The tenant, project
// and id are duplicated out of Values so scope and tie-break checks never trust
// a caller-supplied map.
type scannerRow struct {
	table   string
	tenant  string
	project string
	id      string
	values  map[string]any
}

type referenceScanner struct {
	rows []scannerRow
}

func newReferenceScanner(seed []store.Row) *referenceScanner {
	scanner := &referenceScanner{rows: make([]scannerRow, 0, len(seed))}
	for _, row := range seed {
		scanner.Insert(row)
	}
	return scanner
}

// Insert adds one committed row. It is the reference interpreter's seeding
// hook; real adapters commit through store.Executor and only read through
// store.Scanner.
func (scanner *referenceScanner) Insert(row store.Row) {
	scanner.rows = append(scanner.rows, scannerRow{
		table:   row.Table,
		tenant:  scannerString(row.Values["tenant_id"]),
		project: scannerString(row.Values["project_id"]),
		id:      scannerString(row.Values["id"]),
		values:  cloneScannerValues(row.Values),
	})
}

// Count returns the number of committed rows in table visible in scope. A nil
// scope counts every row in the table.
func (scanner *referenceScanner) Count(table string, scope *tenancy.Scope) int {
	design, ok := scannerSchemaTable(table)
	if !ok {
		return 0
	}
	count := 0
	for _, row := range scanner.rows {
		if row.table != table {
			continue
		}
		if scope != nil && !scannerRowVisible(design, row, *scope) {
			continue
		}
		count++
	}
	return count
}

func (scanner *referenceScanner) Query(ctx context.Context, scope tenancy.Scope, query store.ScanQuery) (store.QueryPage, error) {
	if err := ctx.Err(); err != nil {
		return store.QueryPage{}, err
	}
	if err := scope.Validate(); err != nil {
		return store.QueryPage{}, err
	}
	if err := query.Validate(); err != nil {
		return store.QueryPage{}, err
	}
	design, _ := scannerSchemaTable(query.Table)
	if scannerProjectRequired(design) && scope.TenantScoped() {
		return store.QueryPage{}, fault.New(fault.InvalidInput, fmt.Sprintf("table %q is project scoped; a tenant-scope query is not allowed", query.Table))
	}
	terms := scannerCanonicalOrder(query.OrderBy)
	candidates := make([]scannerRow, 0, len(scanner.rows))
	for _, row := range scanner.rows {
		if row.table != query.Table {
			continue
		}
		if !scannerRowVisible(design, row, scope) {
			continue
		}
		if !scannerRowMatchesEquals(row, query.Equals) {
			continue
		}
		if !scannerRowMatchesRange(row, query.Range) {
			continue
		}
		candidates = append(candidates, row)
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		return scannerCompareRows(candidates[left], candidates[right], terms) < 0
	})
	if query.After != "" {
		cursor, err := decodeScannerCursor(query.After)
		if err != nil {
			return store.QueryPage{}, err
		}
		if cursor.TenantID != string(scope.TenantID) || cursor.ProjectID != string(scope.ProjectID) {
			return store.QueryPage{}, fault.New(fault.Forbidden, "query cursor belongs to another scope")
		}
		if cursor.Table != query.Table || !scannerSameOrder(cursor.Order, terms) {
			return store.QueryPage{}, fault.New(fault.InvalidInput, "query cursor belongs to another table or order")
		}
		filtered := make([]scannerRow, 0, len(candidates))
		for _, row := range candidates {
			if scannerRowAfterCursor(row, terms, cursor.Key) {
				filtered = append(filtered, row)
			}
		}
		candidates = filtered
	}
	page := store.QueryPage{}
	if len(candidates) > query.Limit {
		page.HasMore = true
		candidates = candidates[:query.Limit]
	}
	page.Rows = make([]store.Row, 0, len(candidates))
	for _, row := range candidates {
		page.Rows = append(page.Rows, store.Row{Table: query.Table, Values: cloneScannerValues(row.values)})
	}
	if page.HasMore {
		page.Next = encodeScannerCursor(scope, query.Table, terms, candidates[len(candidates)-1])
	}
	return page, nil
}

// brokenScanner is deliberately wrong: it widens every page limit to the
// maximum. The conformance vectors must detect it.
type brokenScanner struct {
	*referenceScanner
}

func (scanner brokenScanner) Query(ctx context.Context, scope tenancy.Scope, query store.ScanQuery) (store.QueryPage, error) {
	query.Limit = store.MaxQueryLimit
	return scanner.referenceScanner.Query(ctx, scope, query)
}

func scannerSchemaTable(name string) (store.Table, bool) {
	for _, table := range store.SchemaDesign().Tables {
		if table.Name == name {
			return table, true
		}
	}
	return store.Table{}, false
}

func scannerTableHasColumn(table store.Table, name string) bool {
	for _, column := range table.Columns {
		if column.Name == name {
			return true
		}
	}
	return false
}

func scannerProjectRequired(table store.Table) bool {
	for _, column := range table.Columns {
		if column.Name == "project_id" {
			return !column.Nullable
		}
	}
	return false
}

func scannerRowVisible(table store.Table, row scannerRow, scope tenancy.Scope) bool {
	if scannerTableHasColumn(table, "tenant_id") && row.tenant != string(scope.TenantID) {
		return false
	}
	if scannerTableHasColumn(table, "project_id") {
		if scope.TenantScoped() {
			return row.project == ""
		}
		return row.project == string(scope.ProjectID)
	}
	switch table.Name {
	case "tenants":
		// The tenant root row carries its identity instead of a tenant_id
		// column, so only the row whose id is the scope tenant is visible.
		return row.id == string(scope.TenantID)
	case "projects":
		// The tenant_id check above already bound the row to the scope tenant.
		// A project scope additionally reads only its own project row; a
		// tenant-wide scope reads every project of its tenant, matching the
		// Read root-row rule.
		return scope.TenantScoped() || row.id == string(scope.ProjectID)
	default:
		return false
	}
}

func scannerRowValue(row scannerRow, column string) any {
	switch column {
	case "id":
		return row.id
	case "tenant_id":
		return row.tenant
	case "project_id":
		return row.project
	default:
		return row.values[column]
	}
}

func scannerRowMatchesEquals(row scannerRow, equals map[string]any) bool {
	for column, want := range equals {
		if scannerCompareValues(scannerRowValue(row, column), want) != 0 {
			return false
		}
	}
	return true
}

func scannerRowMatchesRange(row scannerRow, filters []store.RangeFilter) bool {
	for _, filter := range filters {
		value := scannerRowValue(row, filter.Column)
		if filter.After != nil && scannerCompareValues(value, filter.After) <= 0 {
			return false
		}
		if filter.Before != nil && scannerCompareValues(value, filter.Before) >= 0 {
			return false
		}
	}
	return true
}

func scannerCanonicalOrder(orders []store.Order) []store.Order {
	terms := append([]store.Order(nil), orders...)
	if len(terms) == 0 || terms[len(terms)-1].Column != "id" {
		terms = append(terms, store.Order{Column: "id"})
	}
	return terms
}

func scannerCompareRows(left, right scannerRow, terms []store.Order) int {
	for _, term := range terms {
		comparison := scannerCompareValues(scannerRowValue(left, term.Column), scannerRowValue(right, term.Column))
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

func scannerCompareValues(left, right any) int {
	if left == nil && right == nil {
		return 0
	}
	if left == nil {
		return -1
	}
	if right == nil {
		return 1
	}
	switch leftValue := left.(type) {
	case string:
		if rightValue, ok := right.(string); ok {
			return strings.Compare(leftValue, rightValue)
		}
	case float64:
		if rightValue, ok := right.(float64); ok {
			switch {
			case leftValue < rightValue:
				return -1
			case leftValue > rightValue:
				return 1
			default:
				return 0
			}
		}
	case int64:
		if rightValue, ok := right.(int64); ok {
			switch {
			case leftValue < rightValue:
				return -1
			case leftValue > rightValue:
				return 1
			default:
				return 0
			}
		}
	case bool:
		if rightValue, ok := right.(bool); ok {
			switch {
			case leftValue == rightValue:
				return 0
			case !leftValue:
				return -1
			default:
				return 1
			}
		}
	}
	return strings.Compare(fmt.Sprint(left), fmt.Sprint(right))
}

type scannerCursor struct {
	TenantID  string               `json:"tenant_id"`
	ProjectID string               `json:"project_id"`
	Table     string               `json:"table"`
	Order     []scannerCursorOrder `json:"order"`
	Key       []any                `json:"key"`
}

type scannerCursorOrder struct {
	Column string `json:"column"`
	Desc   bool   `json:"desc"`
}

func encodeScannerCursor(scope tenancy.Scope, table string, terms []store.Order, row scannerRow) string {
	cursor := scannerCursor{
		TenantID:  string(scope.TenantID),
		ProjectID: string(scope.ProjectID),
		Table:     table,
		Order:     make([]scannerCursorOrder, 0, len(terms)),
		Key:       make([]any, 0, len(terms)),
	}
	for _, term := range terms {
		cursor.Order = append(cursor.Order, scannerCursorOrder{Column: term.Column, Desc: term.Desc})
		cursor.Key = append(cursor.Key, scannerRowValue(row, term.Column))
	}
	data, err := json.Marshal(cursor)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeScannerCursor(value string) (scannerCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return scannerCursor{}, fault.New(fault.InvalidInput, "query cursor is not a valid opaque token")
	}
	var cursor scannerCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return scannerCursor{}, fault.New(fault.InvalidInput, "query cursor is not a valid opaque token")
	}
	if len(cursor.Order) == 0 || len(cursor.Key) != len(cursor.Order) {
		return scannerCursor{}, fault.New(fault.InvalidInput, "query cursor has no keyset")
	}
	return cursor, nil
}

func scannerSameOrder(left []scannerCursorOrder, right []store.Order) bool {
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

func scannerRowAfterCursor(row scannerRow, terms []store.Order, key []any) bool {
	for index, term := range terms {
		comparison := scannerCompareValues(scannerRowValue(row, term.Column), key[index])
		if comparison == 0 {
			continue
		}
		if term.Desc {
			return comparison < 0
		}
		return comparison > 0
	}
	return false
}

func cloneScannerValues(values map[string]any) map[string]any {
	cloned := make(map[string]any, len(values))
	for column, value := range values {
		cloned[column] = value
	}
	return cloned
}
