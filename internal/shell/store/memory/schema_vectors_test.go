// Schema conformance: the adapter schema.json vectors executed against the
// memory adapter's derived schema index, plus behavioral proof that the
// declared columns and tables persist. The adapter never hand-maintains a table
// or column list: it builds its metadata from store.SchemaDesign, so these
// vectors catch any drift between the declaration and what the memory
// write/read paths accept.
package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
)

type schemaVectorDocument struct {
	Version   int                   `json:"version"`
	LockOrder []string              `json:"lock_order"`
	Tables    []schemaVectorTable   `json:"tables"`
	Roles     map[string]string     `json:"roles"`
	Migration schemaVectorMigration `json:"migration"`
}

type schemaVectorMigration struct {
	Pattern       string `json:"pattern"`
	SequenceStart int    `json:"sequence_start"`
}

type schemaVectorTable struct {
	Name             string               `json:"name"`
	RequiredColumns  []schemaVectorColumn `json:"required_columns"`
	ForbiddenColumns []string             `json:"forbidden_columns"`
	Indexes          []schemaVectorIndex  `json:"indexes"`
	Checks           []schemaVectorCheck  `json:"checks"`
	ProjectNullable  *bool                `json:"project_nullable"`
	ForcedRLS        *bool                `json:"forced_rls"`
}

type schemaVectorColumn struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Nullable bool    `json:"nullable"`
	Default  *string `json:"default"`
}

type schemaVectorIndex struct {
	Name      string   `json:"name"`
	Columns   []string `json:"columns"`
	Unique    bool     `json:"unique"`
	Predicate string   `json:"predicate"`
}

type schemaVectorCheck struct {
	Name     string   `json:"name"`
	Contains []string `json:"contains"`
	// Exact pins the complete normalized expression. It is how the
	// receipts_completion_pair rule (completed requires a result, incomplete may
	// carry an in-progress result) is distinguished from a strict equality pair.
	Exact string `json:"exact"`
}

// normalizeSchemaExpression canonicalizes a declared check expression so the
// exact comparison is insensitive to spacing and keyword case.
func normalizeSchemaExpression(expression string) string {
	return strings.Join(strings.Fields(strings.ToLower(expression)), " ")
}

func loadSchemaVectors(t *testing.T) schemaVectorDocument {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "test", "contracts", "adapters", "testdata", "schema.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schema conformance vectors: %v", err)
	}
	var document schemaVectorDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode schema conformance vectors: %v", err)
	}
	return document
}

// TestSchemaVectorsAgainstMemory proves the adapter's derived metadata carries
// every declared table, column, index and check, and no forbidden column.
func TestSchemaVectorsAgainstMemory(t *testing.T) {
	document := loadSchemaVectors(t)
	if document.Version != 1 {
		t.Fatalf("schema vector document version = %d; want 1", document.Version)
	}
	for _, vector := range document.Tables {
		t.Run(vector.Name, func(t *testing.T) {
			meta, ok := lookupTable(vector.Name)
			if !ok {
				t.Fatalf("table %q is not in the adapter's derived schema", vector.Name)
			}
			for _, required := range vector.RequiredColumns {
				column, ok := meta.columns[required.Name]
				if !ok {
					t.Fatalf("table %q has no required column %q", vector.Name, required.Name)
				}
				if column.Type != required.Type {
					t.Errorf("%s.%s type = %q; want %q", vector.Name, required.Name, column.Type, required.Type)
				}
				if column.Nullable != required.Nullable {
					t.Errorf("%s.%s nullable = %v; want %v", vector.Name, required.Name, column.Nullable, required.Nullable)
				}
				if required.Default != nil && column.Default != *required.Default {
					t.Errorf("%s.%s default = %q; want %q", vector.Name, required.Name, column.Default, *required.Default)
				}
			}
			for _, forbidden := range vector.ForbiddenColumns {
				if meta.hasColumn(forbidden) {
					t.Errorf("table %q declares forbidden column %q", vector.Name, forbidden)
				}
			}
			if vector.ProjectNullable != nil {
				column, ok := meta.columns["project_id"]
				if !ok {
					t.Fatalf("table %q has no project_id column", vector.Name)
				}
				if column.Nullable != *vector.ProjectNullable {
					t.Errorf("%s.project_id nullable = %v; want %v", vector.Name, column.Nullable, *vector.ProjectNullable)
				}
			}
			if vector.ForcedRLS != nil && meta.table.ForceRowLevelSecurity != *vector.ForcedRLS {
				t.Errorf("%s forced RLS = %v; want %v", vector.Name, meta.table.ForceRowLevelSecurity, *vector.ForcedRLS)
			}
			for _, required := range vector.Indexes {
				found := false
				for _, index := range meta.table.Indexes {
					if index.Name != required.Name {
						continue
					}
					found = true
					if !reflect.DeepEqual(index.Columns, required.Columns) {
						t.Errorf("%s index %q columns = %v; want %v", vector.Name, required.Name, index.Columns, required.Columns)
					}
					if index.Unique != required.Unique {
						t.Errorf("%s index %q unique = %v; want %v", vector.Name, required.Name, index.Unique, required.Unique)
					}
					if index.Predicate != required.Predicate {
						t.Errorf("%s index %q predicate = %q; want %q", vector.Name, required.Name, index.Predicate, required.Predicate)
					}
				}
				if !found {
					t.Errorf("table %q has no required index %q", vector.Name, required.Name)
				}
			}
			for _, required := range vector.Checks {
				found := false
				for _, check := range meta.table.Checks {
					if check.Name != required.Name {
						continue
					}
					found = true
					for _, fragment := range required.Contains {
						if !strings.Contains(check.Expression, fragment) {
							t.Errorf("%s check %q expression %q does not contain %q", vector.Name, required.Name, check.Expression, fragment)
						}
					}
					if required.Exact != "" && normalizeSchemaExpression(check.Expression) != normalizeSchemaExpression(required.Exact) {
						t.Errorf("%s check %q expression = %q; want %q", vector.Name, required.Name, check.Expression, required.Exact)
					}
				}
				if !found {
					t.Errorf("table %q has no required check %q", vector.Name, required.Name)
				}
			}
		})
	}

	if got := store.SchemaDesign().LockOrder; !reflect.DeepEqual(got, document.LockOrder) {
		t.Fatalf("adapter lock order = %v; want %v", got, document.LockOrder)
	}
	for position, table := range document.LockOrder {
		if got := schema.lockOrder[table]; got != position {
			t.Errorf("adapter lock position for %q = %d; want %d", table, got, position)
		}
	}
	if got := store.SchemaDesign().MigrationNamePattern; got != document.Migration.Pattern {
		t.Errorf("migration pattern = %q; want %q", got, document.Migration.Pattern)
	}
	if got := store.SchemaDesign().MigrationSequenceStart; got != document.Migration.SequenceStart {
		t.Errorf("migration sequence start = %d; want %d", got, document.Migration.SequenceStart)
	}
}

// TestSchemaVerificationHasNoSourceEvent proves source_event_id is not a
// writable column and a verification without it round-trips.
func TestSchemaVerificationHasNoSourceEvent(t *testing.T) {
	s := New()
	scope := projectScope()
	_, err := s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{{
		Table: "verifications", ID: "verification-1", Insert: true,
		Values: map[string]any{"source_event_id": "event-1"},
	}}})
	assertFault(t, err, fault.InvalidInput)
	assertAbsent(t, s, scope, "verifications", "verification-1")

	seedRow(t, s, scope, "verifications", "verification-1", map[string]any{
		"service_id": testServiceID, "actor_id": testPrincipalID,
		"binding_id": "binding-1", "resource_id": "resource-1",
		"operation": "push", "subject": []byte(`{}`), "result": "passed",
		"approved": true, "created_at": fixedTime(),
	})
	row := mustSnapshot(t, s, scope, "verifications", "verification-1")
	if _, present := row.Values["source_event_id"]; present {
		t.Fatal("verification row still carries source_event_id")
	}
}

// TestSchemaGatewayKernelPipelineTriggerState proves the gateway, kernel,
// pipeline and trigger state persists through the ordinary scoped
// write/read/scan path.
func TestSchemaGatewayKernelPipelineTriggerState(t *testing.T) {
	s := New()
	scope := projectScope()

	// executions.failure and the actions launch/observation columns.
	execution := executionValues(1, "running")
	execution["failure"] = []byte(`{"code":"boom"}`)
	seedRow(t, s, scope, "executions", testExecutionID, execution)
	row := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := string(row.Values["failure"].([]byte)); got != `{"code":"boom"}` {
		t.Fatalf("executions.failure = %q; want the persisted evidence", got)
	}
	action := actionValues(1, "pending")
	action["workload_id"] = "workload-1"
	action["launch_state"] = "issued"
	action["launch_key"] = "launch-1"
	action["observation"] = []byte(`{"running":true}`)
	action["activated_at"] = fixedTime()
	action["answer_provenance"] = []byte("proof")
	seedRow(t, s, scope, "actions", testActionID, action)
	row = mustSnapshot(t, s, scope, "actions", testActionID)
	for column, want := range map[string]any{
		"workload_id": "workload-1", "launch_state": "issued", "launch_key": "launch-1",
	} {
		if got := rowString(t, row, column); got != want {
			t.Errorf("actions.%s = %q; want %q", column, got, want)
		}
	}
	if got := string(row.Values["observation"].([]byte)); got != `{"running":true}` {
		t.Errorf("actions.observation = %q", got)
	}

	// pipelines.push_triggers is a writable jsonb projection.
	pipeline := insertValues("pipelines")
	pipeline["push_triggers"] = []byte(`[{"branches":["main"]}]`)
	seedRow(t, s, scope, "pipelines", "pipeline-1", pipeline)
	row = mustSnapshot(t, s, scope, "pipelines", "pipeline-1")
	if got := string(row.Values["push_triggers"].([]byte)); got != `[{"branches":["main"]}]` {
		t.Fatalf("pipelines.push_triggers = %q", got)
	}

	// model_usage and content_records, execution-linked and nullable.
	seedRow(t, s, scope, store.ModelUsageTable, "usage-1", map[string]any{
		"principal_id": testPrincipalID, "execution_id": testExecutionID,
		"model": "gpt", "upstream_model": "gpt-2026", "outcome": "succeeded",
		"usage_known": true, "prompt_tokens": int64(1), "completion_tokens": int64(2),
		"total_tokens": int64(3), "recorded_at": fixedTime(), "version": int64(1),
	})
	seedRow(t, s, scope, store.ContentRecordsTable, "content-1", map[string]any{
		"principal_id": testPrincipalID, "model": "gpt",
		"request_body": []byte("question"), "response_body": []byte("answer"),
		"recorded_at": fixedTime(), "retain_until": fixedTime().Add(7 * 24 * time.Hour),
		"version": int64(1),
	})
	for _, table := range []string{store.ModelUsageTable, store.ContentRecordsTable} {
		page := queryScanner(t, s, scope, store.ScanQuery{Table: table, Limit: 10})
		if len(page.Rows) != 1 {
			t.Fatalf("table %q returned %d rows; want 1", table, len(page.Rows))
		}
	}

	// event_consumptions is the trigger completion marker.
	seedRow(t, s, scope, store.EventConsumptionsTable, "event-1", map[string]any{
		"consumer": "trigger", "completed_at": fixedTime(), "version": int64(1),
	})
	row = mustSnapshot(t, s, scope, store.EventConsumptionsTable, "event-1")
	if got := rowString(t, row, "consumer"); got != "trigger" {
		t.Fatalf("event_consumptions.consumer = %q; want trigger", got)
	}
}

// TestSchemaReceiptCompletionPair proves the receipts.completed_at marker is
// NULL exactly for an incomplete receipt and set once completion applies.
func TestSchemaReceiptCompletionPair(t *testing.T) {
	s := New()
	scope := projectScope()
	key := keyFor(testTenantID, testProjectID, testServiceID, "pipeline.push", "event-1")
	contentHash := fingerprintOf(t, `{"event":"event-1"}`)
	record := receipt.Record{
		ID:          seamReceiptIDOf(key),
		Key:         key,
		ContentHash: contentHash,
		At:          fixedTime(),
	}
	mustExecute(t, s, scope, store.Unit{Receipt: &record, ReceiptMode: store.ReceiptInsertIncomplete})

	receiptRow := func() map[string]any {
		page := queryScanner(t, s, scope, store.ScanQuery{
			Table: "receipts", Equals: map[string]any{"id": string(record.ID)}, Limit: 10,
		})
		if len(page.Rows) != 1 {
			t.Fatalf("receipt rows = %d; want 1", len(page.Rows))
		}
		return page.Rows[0].Values
	}
	if got := receiptRow()["completed_at"]; got != nil {
		t.Fatalf("incomplete receipt completed_at = %v; want NULL", got)
	}

	completed := record
	completed.Result = []byte("started")
	completed.ExecutionID = id.ExecutionID(testExecutionID)
	mustExecute(t, s, scope, store.Unit{
		Receipt:       &completed,
		ReceiptMode:   store.ReceiptComplete,
		ReceiptExpect: &store.ReceiptExpect{ContentHash: contentHash},
	})
	row := receiptRow()
	if row["completed_at"] == nil {
		t.Fatal("completed receipt completed_at = NULL; want a completion marker")
	}
	if got := string(row["result"].([]byte)); got != "started" {
		t.Fatalf("completed receipt result = %q; want started", got)
	}
}
