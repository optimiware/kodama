// Scanner conformance and store-specific tests.
//
// The adapter scanner vectors live in test/contracts/adapters/testdata/
// scanner.json. That file is never edited. The reference interpreter and its
// runner are test-only code in another package, so this file loads the same
// JSON document through a relative path and executes every vector against the
// memory adapter: query steps call Store.Query, insert steps commit one seeded
// row, and inspect steps count committed rows visible in the given scope.
//
// Seeded vector values are translated into the memory adapter's value model
// (timestamptz as time.Time, integers as int64, bytea/jsonb as []byte) before
// they are committed, so ordering and range comparisons exercise the same typed
// comparison a production caller sees. Event rows are committed publication
// records, exactly as Execute publishes them.
package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

type scanVectorDocument struct {
	Version int          `json:"version"`
	Vectors []scanVector `json:"vectors"`
}

type scanVector struct {
	Name  string         `json:"name"`
	Kind  string         `json:"kind"`
	Seed  scanVectorSeed `json:"seed"`
	Steps []scanVectorStep
}

type scanVectorSeed struct {
	Rows []scanVectorRow `json:"rows"`
}

type scanVectorRow struct {
	Table     string         `json:"table"`
	TenantID  string         `json:"tenant_id"`
	ProjectID string         `json:"project_id"`
	ID        string         `json:"id"`
	Values    map[string]any `json:"values"`
}

type scanVectorScope struct {
	TenantID  string `json:"tenant_id"`
	ProjectID string `json:"project_id"`
}

type scanVectorRange struct {
	Column string `json:"column"`
	After  any    `json:"after"`
	Before any    `json:"before"`
}

type scanVectorOrder struct {
	Column string `json:"column"`
	Desc   bool   `json:"desc"`
}

type scanVectorStep struct {
	Op          string            `json:"op"`
	Scope       *scanVectorScope  `json:"scope"`
	Table       string            `json:"table"`
	Equals      map[string]any    `json:"equals"`
	Range       []scanVectorRange `json:"range"`
	OrderBy     []scanVectorOrder `json:"order_by"`
	AfterStep   *int              `json:"after_step"`
	After       string            `json:"after"`
	Limit       int               `json:"limit"`
	Row         *scanVectorRow    `json:"row"`
	WantIDs     []string          `json:"want_ids"`
	WantNext    *bool             `json:"want_next"`
	WantHasMore *bool             `json:"want_has_more"`
	WantError   string            `json:"want_error"`
	WantCount   *int              `json:"want_count"`
}

var scanVectorKinds = map[string]bool{
	"scope":         true,
	"pagination":    true,
	"determinism":   true,
	"cursor":        true,
	"limit":         true,
	"invalid_input": true,
	"readonly":      true,
}

var scanVectorErrors = map[string]bool{
	"invalid_input": true,
	"forbidden":     true,
}

// TestScannerRunsEveryAdapterScannerVector executes the adapter scanner
// vectors against the memory adapter.
func TestScannerRunsEveryAdapterScannerVector(t *testing.T) {
	document := loadScanVectors(t)
	validateScanVectors(t, document)
	for _, vector := range document.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			s := New()
			for _, row := range vector.Seed.Rows {
				seedScanVectorRow(t, s, row)
			}
			if err := runScanVector(t, s, vector); err != nil {
				t.Fatalf("vector %q: %v", vector.Name, err)
			}
		})
	}
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
	return document
}

func validateScanVectors(t *testing.T, document scanVectorDocument) {
	t.Helper()
	if document.Version != 1 {
		t.Fatalf("scanner vector document version = %d; want 1", document.Version)
	}
	if len(document.Vectors) == 0 {
		t.Fatal("scanner conformance vector set is empty")
	}
	names := make(map[string]bool, len(document.Vectors))
	kinds := make(map[string]int, len(scanVectorKinds))
	for _, vector := range document.Vectors {
		if vector.Name == "" || vector.Kind == "" {
			t.Errorf("incomplete scanner vector %+v", vector)
			continue
		}
		if names[vector.Name] {
			t.Errorf("duplicate scanner vector name %q", vector.Name)
		}
		names[vector.Name] = true
		if !scanVectorKinds[vector.Kind] {
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
			if step.WantError != "" && !scanVectorErrors[step.WantError] {
				t.Errorf("vector %q step %d expects unknown error class %q", vector.Name, index, step.WantError)
			}
		}
		if queries == 0 {
			t.Errorf("vector %q has no query step", vector.Name)
		}
	}
	for _, kind := range []string{"scope", "pagination", "determinism", "cursor", "limit", "invalid_input", "readonly"} {
		if kinds[kind] == 0 {
			t.Errorf("scanner conformance vectors are missing kind %q", kind)
		}
	}
}

func runScanVector(t *testing.T, s *Store, vector scanVector) error {
	t.Helper()
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
				Range:   scanVectorRangeFilters(step.Range),
				OrderBy: scanVectorOrderTerms(step.OrderBy),
				After:   step.After,
				Limit:   step.Limit,
			}
			if step.AfterStep != nil {
				query.After = next[*step.AfterStep]
			}
			scope := scopeFor(step.Scope.TenantID, step.Scope.ProjectID)
			page, err := s.Query(context.Background(), scope, query)
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
					got = append(got, scanVectorString(row.Values["id"]))
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
			if step.Row == nil {
				return fmt.Errorf("step %d inserts no row", index)
			}
			seedScanVectorRow(t, s, *step.Row)
		case "inspect":
			if step.WantCount == nil {
				return fmt.Errorf("step %d inspects without a count", index)
			}
			var scope *tenancy.Scope
			if step.Scope != nil {
				value := scopeFor(step.Scope.TenantID, step.Scope.ProjectID)
				scope = &value
			}
			if got := scanVectorCount(s, step.Table, scope); got != *step.WantCount {
				return fmt.Errorf("step %d count(%s) = %d; want %d", index, step.Table, got, *step.WantCount)
			}
		default:
			return fmt.Errorf("step %d has unknown op %q", index, step.Op)
		}
	}
	return nil
}

func scanVectorRangeFilters(filters []scanVectorRange) []store.RangeFilter {
	if filters == nil {
		return nil
	}
	converted := make([]store.RangeFilter, 0, len(filters))
	for _, filter := range filters {
		converted = append(converted, store.RangeFilter{Column: filter.Column, After: filter.After, Before: filter.Before})
	}
	return converted
}

func scanVectorOrderTerms(orders []scanVectorOrder) []store.Order {
	if orders == nil {
		return nil
	}
	converted := make([]store.Order, 0, len(orders))
	for _, order := range orders {
		converted = append(converted, store.Order{Column: order.Column, Desc: order.Desc})
	}
	return converted
}

func scanVectorString(value any) string {
	text, _ := value.(string)
	return text
}

// seedScanVectorRow commits one vector seed row. A business row is committed
// through the adapter's real write path after its values are translated into
// the adapter value model. An event row is published as a committed event record
// because events are never generic business writes.
func seedScanVectorRow(t *testing.T, s *Store, row scanVectorRow) {
	t.Helper()
	scope := scopeFor(row.TenantID, row.ProjectID)
	switch row.Table {
	case "tenants":
		// A tenants row carries its own identity instead of a tenant_id column.
		scope = scopeFor(row.ID, "")
	case "projects":
		// A projects row is identified by its tenant plus its own id.
		scope = scopeFor(row.TenantID, row.ID)
	}
	switch row.Table {
	case "events":
		envelope := event.Envelope{
			ID:        id.EventID(row.ID),
			Type:      event.Type(scanVectorString(row.Values["type"])),
			Source:    event.Source{Kind: event.SourceShell, PrincipalID: id.PrincipalID(testPrincipalID)},
			At:        fixedTime(),
			Scope:     scope,
			SubjectID: row.ID,
		}
		s.mu.Lock()
		s.state.sequence++
		s.state.events = append(s.state.events, CommittedEvent{Sequence: s.state.sequence, Envelope: envelope})
		s.mu.Unlock()
	case "audit":
		audit := event.Audit{
			ID:        id.EventID(row.ID),
			Scope:     scope,
			Actor:     id.PrincipalID(testPrincipalID),
			Operation: "seeded",
			At:        fixedTime(),
		}
		s.mu.Lock()
		s.state.audits = append(s.state.audits, audit)
		s.mu.Unlock()
	default:
		seedRow(t, s, scope, row.Table, row.ID, scanSeedValues(t, row.Table, row.Values))
	}
}

// scanSeedValues translates JSON-shaped vector values into the typed shapes the
// memory adapter stores.
func scanSeedValues(t *testing.T, table string, raw map[string]any) map[string]any {
	t.Helper()
	meta, ok := lookupTable(table)
	if !ok {
		t.Fatalf("seed row names unknown table %q", table)
	}
	values := make(map[string]any, len(raw))
	for name, value := range raw {
		column, ok := meta.columns[name]
		if !ok {
			t.Fatalf("seed row %q has unknown column %q", table, name)
		}
		normalized, err := scanSeedValue(column, value)
		if err != nil {
			t.Fatalf("seed %s.%s: %v", table, name, err)
		}
		values[name] = normalized
	}
	return values
}

func scanSeedValue(column store.Column, value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	switch column.Type {
	case "text":
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("column %q expects text; got %T", column.Name, value)
		}
		return text, nil
	case "bigint", "integer":
		number, ok := value.(float64)
		if !ok {
			return nil, fmt.Errorf("column %q expects a number; got %T", column.Name, value)
		}
		return int64(number), nil
	case "timestamptz":
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("column %q expects a timestamp; got %T", column.Name, value)
		}
		moment, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			return nil, fmt.Errorf("column %q timestamp: %w", column.Name, err)
		}
		return moment, nil
	case "bytea", "jsonb":
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("column %q expects bytes; got %T", column.Name, value)
		}
		return []byte(text), nil
	case "boolean":
		boolean, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("column %q expects a boolean; got %T", column.Name, value)
		}
		return boolean, nil
	default:
		return nil, fmt.Errorf("column %q has unsupported type %q", column.Name, column.Type)
	}
}

// scanVectorCount counts committed rows in table visible in scope. A nil scope
// counts every committed row in the table.
func scanVectorCount(s *Store, table string, scope *tenancy.Scope) int {
	meta, ok := lookupTable(table)
	if !ok {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	switch table {
	case "events":
		for _, committed := range s.state.events {
			if scope != nil && !scopeVisible(*scope, committed.Envelope.Scope) {
				continue
			}
			count++
		}
	case "audit":
		for _, audit := range s.state.audits {
			if scope != nil && !scopeVisible(*scope, audit.Scope) {
				continue
			}
			count++
		}
	case "receipts":
		for _, record := range s.state.receipts {
			rowScope := tenancy.Scope{TenantID: record.Key.TenantID, ProjectID: record.Key.ProjectID}
			if scope != nil && !scopeVisible(*scope, rowScope) {
				continue
			}
			count++
		}
	default:
		for identity, values := range s.state.rows[table] {
			if scope != nil && !rowVisible(meta, *scope, identity, values, false) {
				continue
			}
			count++
		}
	}
	return count
}

// TestScannerReturnsDeepCopies proves a returned page cannot alias committed
// state: mutating a row map or a retained byte slice is invisible to the store
// and to a later page.
func TestScannerReturnsDeepCopies(t *testing.T) {
	s := New()
	seedRow(t, s, projectScope(), "executions", "execution-1", map[string]any{
		"version":  int64(1),
		"state":    "queued",
		"snapshot": []byte(`{"snapshot":"original"}`),
	})
	page := queryScanner(t, s, projectScope(), store.ScanQuery{
		Table:  "executions",
		Equals: map[string]any{"id": "execution-1"},
		Limit:  10,
	})
	if len(page.Rows) != 1 {
		t.Fatalf("rows = %d; want 1", len(page.Rows))
	}
	row := page.Rows[0]
	row.Values["state"] = "mutated"
	row.Values["id"] = "mutated"
	row.Values["snapshot"].([]byte)[0] = 'X'
	delete(row.Values, "version")

	again := queryScanner(t, s, projectScope(), store.ScanQuery{
		Table:  "executions",
		Equals: map[string]any{"id": "execution-1"},
		Limit:  10,
	})
	if len(again.Rows) != 1 {
		t.Fatalf("second rows = %d; want 1", len(again.Rows))
	}
	if got := again.Rows[0].Values["state"]; got != "queued" {
		t.Fatalf("stored state = %v; want queued", got)
	}
	if got := again.Rows[0].Values["id"]; got != "execution-1" {
		t.Fatalf("stored id = %v; want execution-1", got)
	}
	if _, ok := again.Rows[0].Values["version"]; !ok {
		t.Fatal("stored version column disappeared")
	}
	if got := again.Rows[0].Values["snapshot"].([]byte); !bytes.Equal(got, []byte(`{"snapshot":"original"}`)) {
		t.Fatalf("stored snapshot = %q; want original bytes", got)
	}
}

// TestScannerConcurrentWithExecute runs scoped pages while commit units insert
// new rows. It is meaningful under -race and pins the keyset invariant: a page
// traversal never repeats a row and stays ordered.
func TestScannerConcurrentWithExecute(t *testing.T) {
	s := New()
	for index := range 40 {
		seedRow(t, s, projectScope(), "executions", fmt.Sprintf("execution-%03d", index), map[string]any{
			"version":    int64(1),
			"state":      "queued",
			"created_at": fixedTime().Add(time.Duration(index) * time.Second),
		})
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	wg.Go(func() {
		for index := range 80 {
			unit := store.Unit{Writes: []store.Write{{
				Table:  "executions",
				ID:     fmt.Sprintf("inserted-%03d", index),
				Insert: true,
				Values: map[string]any{
					"version":    int64(1),
					"state":      "queued",
					"created_at": fixedTime().Add(time.Duration(index) * time.Millisecond),
				},
			}}}
			if _, err := s.Execute(context.Background(), projectScope(), unit); err != nil {
				errCh <- fmt.Errorf("insert %d: %w", index, err)
				return
			}
		}
	})
	for range 4 {
		wg.Go(func() {
			for round := range 25 {
				cursor := ""
				seen := make(map[string]bool)
				for {
					page, err := s.Query(context.Background(), projectScope(), store.ScanQuery{
						Table:   "executions",
						OrderBy: []store.Order{{Column: "created_at"}, {Column: "id"}},
						After:   cursor,
						Limit:   7,
					})
					if err != nil {
						errCh <- fmt.Errorf("query round %d: %w", round, err)
						return
					}
					var previousTime time.Time
					previousID := ""
					for _, row := range page.Rows {
						identity, _ := row.Values["id"].(string)
						if identity == "" {
							errCh <- errors.New("page row has no id")
							return
						}
						if seen[identity] {
							errCh <- fmt.Errorf("page traversal repeated row %q", identity)
							return
						}
						seen[identity] = true
						moment, _ := row.Values["created_at"].(time.Time)
						if !previousTime.IsZero() {
							if moment.Before(previousTime) || (moment.Equal(previousTime) && identity < previousID) {
								errCh <- fmt.Errorf("page order regressed from (%v,%q) to (%v,%q)", previousTime, previousID, moment, identity)
								return
							}
						}
						previousTime = moment
						previousID = identity
					}
					if !page.HasMore {
						break
					}
					if page.Next == "" {
						errCh <- errors.New("HasMore page has no cursor")
						return
					}
					cursor = page.Next
				}
			}
		})
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// TestScannerLimitBoundaries pins the required bounded page: limit 1 and the
// maximum are accepted, zero/negative/above-maximum are refused.
func TestScannerLimitBoundaries(t *testing.T) {
	s := New()
	const seeded = store.MaxQueryLimit + 1
	for index := range seeded {
		seedRow(t, s, projectScope(), "executions", fmt.Sprintf("execution-%03d", index), map[string]any{
			"version": int64(1),
			"state":   "queued",
		})
	}

	first := queryScanner(t, s, projectScope(), store.ScanQuery{Table: "executions", Limit: 1})
	if len(first.Rows) != 1 || !first.HasMore || first.Next == "" {
		t.Fatalf("limit 1 page = %d rows hasMore=%v next=%q; want 1 row and a cursor", len(first.Rows), first.HasMore, first.Next)
	}

	maximum := queryScanner(t, s, projectScope(), store.ScanQuery{Table: "executions", Limit: store.MaxQueryLimit})
	if len(maximum.Rows) != store.MaxQueryLimit || !maximum.HasMore || maximum.Next == "" {
		t.Fatalf("limit %d page = %d rows hasMore=%v; want %d rows and a cursor", store.MaxQueryLimit, len(maximum.Rows), maximum.HasMore, store.MaxQueryLimit)
	}
	rest := queryScanner(t, s, projectScope(), store.ScanQuery{Table: "executions", After: maximum.Next, Limit: store.MaxQueryLimit})
	if len(rest.Rows) != 1 || rest.HasMore || rest.Next != "" {
		t.Fatalf("rest page = %d rows hasMore=%v next=%q; want the final row", len(rest.Rows), rest.HasMore, rest.Next)
	}

	for _, limit := range []int{0, -1, store.MaxQueryLimit + 1} {
		_, err := s.Query(context.Background(), projectScope(), store.ScanQuery{Table: "executions", Limit: limit})
		assertScanFault(t, err, fault.InvalidInput)
	}
}

// TestScannerEverySchemaTableQueryable proves the scanner covers every declared
// table, including the tenants/projects root rows and the events/audit/receipts
// publication tables, and applies the caller scope exactly.
func TestScannerEverySchemaTableQueryable(t *testing.T) {
	s := New()
	scope := projectScope()
	for _, table := range store.SchemaDesign().Tables {
		switch table.Name {
		case "events":
			mustExecute(t, s, scope, store.Unit{Events: []event.Envelope{
				defaultEvent(scope, "event-1", event.TypeTaskCreated, "task-1"),
			}})
			continue
		case "audit":
			mustExecute(t, s, scope, store.Unit{Audit: []event.Audit{
				defaultAudit(scope, "audit-1", "task.create"),
			}})
			continue
		case "receipts":
			record := defaultReceipt(t)
			mustExecute(t, s, scope, store.Unit{Receipt: &record})
			continue
		}
		identity := "row-1"
		switch table.Name {
		case "tenants":
			identity = testTenantID
		case "projects":
			identity = testProjectID
		}
		seedRow(t, s, scope, table.Name, identity, insertValues(table.Name))
	}

	for _, table := range store.SchemaDesign().Tables {
		query := store.ScanQuery{Table: table.Name, Limit: 10}
		switch table.Name {
		case "tenants":
			query.Equals = map[string]any{"id": testTenantID}
		case "projects":
			query.Equals = map[string]any{"tenant_id": testTenantID}
		default:
			query.Equals = map[string]any{"tenant_id": testTenantID}
		}
		page := queryScanner(t, s, scope, query)
		if len(page.Rows) != 1 {
			t.Fatalf("table %q returned %d rows; want 1", table.Name, len(page.Rows))
		}
		if page.HasMore || page.Next != "" {
			t.Fatalf("table %q reported more rows than exist", table.Name)
		}
		if got, _ := page.Rows[0].Values["tenant_id"].(string); table.Name != "tenants" && got != testTenantID {
			t.Fatalf("table %q row tenant = %q; want %q", table.Name, got, testTenantID)
		}
	}
}

// TestScannerEmptyPageAndHasMore pins empty results and HasMore/Next coupling.
func TestScannerEmptyPageAndHasMore(t *testing.T) {
	s := New()
	for index := range 2 {
		seedRow(t, s, projectScope(), "executions", fmt.Sprintf("execution-%d", index+1), map[string]any{
			"version": int64(1),
			"state":   "queued",
		})
	}

	empty := queryScanner(t, s, projectScope(), store.ScanQuery{
		Table:  "executions",
		Equals: map[string]any{"id": "missing"},
		Limit:  10,
	})
	if len(empty.Rows) != 0 || empty.HasMore || empty.Next != "" {
		t.Fatalf("missing equality page = %+v; want empty", empty)
	}

	exact := queryScanner(t, s, projectScope(), store.ScanQuery{Table: "executions", Limit: 2})
	if len(exact.Rows) != 2 || exact.HasMore || exact.Next != "" {
		t.Fatalf("exact page = %d rows hasMore=%v next=%q; want no extra page", len(exact.Rows), exact.HasMore, exact.Next)
	}

	short := queryScanner(t, s, projectScope(), store.ScanQuery{Table: "executions", Limit: 1})
	if len(short.Rows) != 1 || !short.HasMore || short.Next == "" {
		t.Fatalf("short page = %d rows hasMore=%v next=%q; want a cursor", len(short.Rows), short.HasMore, short.Next)
	}
}

// TestScannerHonorsContext pins that a canceled context never returns a page.
func TestScannerHonorsContext(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Query(ctx, projectScope(), store.ScanQuery{Table: "executions", Limit: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Query error = %v; want context.Canceled", err)
	}
}

// TestScannerTenantScopeRefusesProjectTable pins the project-required table
// refusal shared with Read.
func TestScannerTenantScopeRefusesProjectTable(t *testing.T) {
	s := New()
	_, err := s.Query(context.Background(), tenantScope(), store.ScanQuery{Table: "executions", Limit: 1})
	assertScanFault(t, err, fault.InvalidInput)
}

func queryScanner(t *testing.T, s *Store, scope tenancy.Scope, query store.ScanQuery) store.QueryPage {
	t.Helper()
	page, err := s.Query(context.Background(), scope, query)
	if err != nil {
		t.Fatalf("Query(%s): %v", query.Table, err)
	}
	return page
}

func assertScanFault(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want fault class %q", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("error = %v (class=%q); want class %q", err, class, want)
	}
}
