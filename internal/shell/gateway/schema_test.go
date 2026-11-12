package gateway_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/gateway"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// schemaTable returns one declared table by name.
func schemaTable(t *testing.T, name string) store.Table {
	t.Helper()
	for _, table := range store.SchemaDesign().Tables {
		if table.Name == name {
			return table
		}
	}
	t.Fatalf("table %q is not in the declared SchemaDesign", name)
	return store.Table{}
}

// schemaColumn returns one declared column by name.
func schemaColumn(t *testing.T, table store.Table, name string) store.Column {
	t.Helper()
	for _, column := range table.Columns {
		if column.Name == name {
			return column
		}
	}
	t.Fatalf("table %q has no column %q", table.Name, name)
	return store.Column{}
}

// assertUnitConformsToSchema proves one gateway-built unit can be applied to the
// declared table: the table exists, the unit is a single plain insert with no
// receipt/lock/audit/event, every written column exists, every required column
// is present and every value has the column's plausible Go type. Adapter-owned
// identity/scope columns (id, tenant_id, project_id) must not be written by the
// caller.
func assertUnitConformsToSchema(t *testing.T, unit store.Unit, tableName string) {
	t.Helper()
	if len(unit.Locks) != 0 {
		t.Errorf("unit locks = %d; want 0", len(unit.Locks))
	}
	if unit.Receipt != nil {
		t.Errorf("unit carries a receipt; want none")
	}
	if unit.ReceiptMode != store.ReceiptInsert {
		t.Errorf("unit receipt mode = %q; want the insert default", unit.ReceiptMode)
	}
	if unit.ReceiptExpect != nil {
		t.Errorf("unit carries a receipt expectation; want none")
	}
	if len(unit.Audit) != 0 {
		t.Errorf("unit audit rows = %d; want 0", len(unit.Audit))
	}
	if len(unit.Events) != 0 {
		t.Errorf("unit events = %d; want 0", len(unit.Events))
	}
	if len(unit.Writes) != 1 {
		t.Fatalf("unit writes = %d; want exactly one", len(unit.Writes))
	}
	write := unit.Writes[0]
	if !write.Insert || write.Delete {
		t.Fatalf("unit write is not a plain insert: %#v", write)
	}
	if write.ExpectVersion != 0 || len(write.Expect) != 0 {
		t.Errorf("unit insert carries a guard: version=%d expect=%v", write.ExpectVersion, write.Expect)
	}
	if write.Table != tableName {
		t.Errorf("unit writes table %q; want %q", write.Table, tableName)
	}
	table := schemaTable(t, tableName)
	if write.ID == "" {
		t.Errorf("unit row id is empty")
	}
	for name, value := range write.Values {
		column := schemaColumn(t, table, name)
		if name == "id" || name == "tenant_id" || name == "project_id" {
			t.Errorf("unit writes adapter-owned column %q", name)
		}
		assertPlausibleColumnValue(t, column, value)
	}
	for _, column := range table.Columns {
		if column.Nullable || column.Name == "id" || column.Name == "tenant_id" || column.Name == "project_id" {
			continue
		}
		if _, present := write.Values[column.Name]; !present {
			t.Errorf("unit omits required column %q of table %q", column.Name, table.Name)
		}
	}
}

// assertPlausibleColumnValue checks that a written value matches the declared
// column type the memory/PostgreSQL adapters enforce, and that only a nullable
// column may be SQL NULL.
func assertPlausibleColumnValue(t *testing.T, column store.Column, value any) {
	t.Helper()
	if value == nil {
		if !column.Nullable {
			t.Errorf("column %q is not nullable but the unit writes NULL", column.Name)
		}
		return
	}
	switch column.Type {
	case "text":
		if _, ok := value.(string); !ok {
			t.Errorf("column %q expects text; got %T", column.Name, value)
		}
	case "bigint", "integer":
		switch value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		default:
			t.Errorf("column %q expects an integer; got %T", column.Name, value)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			t.Errorf("column %q expects bool; got %T", column.Name, value)
		}
	case "timestamptz":
		if _, ok := value.(time.Time); !ok {
			t.Errorf("column %q expects time.Time; got %T", column.Name, value)
		}
	case "bytea", "jsonb":
		if _, ok := value.([]byte); !ok {
			t.Errorf("column %q expects []byte; got %T", column.Name, value)
		}
	default:
		t.Errorf("column %q has unsupported declared type %q", column.Name, column.Type)
	}
}

// assertUsageChecks applies the declared model_usage CHECK expressions to one
// written row: the outcome vocabulary, the usage_known implication and the
// non-negative counts that sum to the total.
func assertUsageChecks(t *testing.T, values map[string]any) {
	t.Helper()
	outcome, ok := values["outcome"].(string)
	if !ok || (outcome != "succeeded" && outcome != "failed" && outcome != "uncertain") {
		t.Errorf("outcome = %v; want succeeded, failed or uncertain", values["outcome"])
	}
	known, ok := values["usage_known"].(bool)
	if !ok {
		t.Fatalf("usage_known = %v; want a boolean", values["usage_known"])
	}
	if known != (outcome == "succeeded") {
		t.Errorf("usage_known = %v with outcome %q; the CHECK requires usage_known = (outcome = 'succeeded')", known, outcome)
	}
	prompt := asInt64(t, values["prompt_tokens"])
	completion := asInt64(t, values["completion_tokens"])
	total := asInt64(t, values["total_tokens"])
	if prompt < 0 || completion < 0 || total < 0 {
		t.Errorf("counts = %d/%d/%d; the CHECK forbids negative counts", prompt, completion, total)
	}
	if prompt+completion != total {
		t.Errorf("counts = %d/%d/%d; the CHECK requires total = prompt + completion", prompt, completion, total)
	}
}

func asInt64(t *testing.T, value any) int64 {
	t.Helper()
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case uint64:
		return int64(typed)
	default:
		t.Fatalf("value %v (%T) is not an integer", value, value)
		return 0
	}
}

// TestGatewayUsageUnitConformsToSchema proves every usage unit the gateway
// actually commits validates against the declared model_usage table for the
// success, definitive-failure, uncertain and unknown-count cases.
func TestGatewayUsageUnitConformsToSchema(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*harness)
		invoke  func(*harness)
		outcome gateway.UsageOutcome
		known   bool
	}{
		{
			name: "succeeded with validated counts",
			invoke: func(h *harness) {
				_, err := h.gateway.Invoke(context.Background(), invocation())
				requireNoError(t, err)
			},
			outcome: gateway.UsageSucceeded,
			known:   true,
		},
		{
			name: "definitive provider failure with unknown counts",
			mutate: func(h *harness) {
				h.transport.handler = func(*http.Request) (*http.Response, error) {
					return h.transport.reply(http.StatusServiceUnavailable, "application/json", []byte(`{"error":"down"}`)), nil
				}
			},
			invoke: func(h *harness) {
				_, err := h.gateway.Invoke(context.Background(), invocation())
				requireClass(t, err, fault.DependencyUnavailable)
			},
			outcome: gateway.UsageFailed,
			known:   false,
		},
		{
			name: "lost response is uncertain with unknown counts",
			mutate: func(h *harness) {
				h.transport.handler = func(*http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     http.StatusText(http.StatusOK),
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       &trackingBody{data: []byte(`{"id":"cmpl-1"`), err: io.ErrUnexpectedEOF},
					}, nil
				}
			},
			invoke: func(h *harness) {
				_, err := h.gateway.Invoke(context.Background(), invocation())
				requireClass(t, err, fault.UncertainExternalEffect)
			},
			outcome: gateway.UsageUncertain,
			known:   false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t, nil)
			if testCase.mutate != nil {
				testCase.mutate(h)
			}
			testCase.invoke(h)
			calls := h.store.callList()
			if len(calls) != 1 {
				t.Fatalf("usage writes = %d; want exactly 1", len(calls))
			}
			assertUnitConformsToSchema(t, calls[0].unit, gateway.UsageTable)
			write := singleInsert(t, calls[0].unit)
			if write.Values["outcome"] != string(testCase.outcome) {
				t.Errorf("outcome = %v; want %q", write.Values["outcome"], testCase.outcome)
			}
			if write.Values["usage_known"] != testCase.known {
				t.Errorf("usage_known = %v; want %v", write.Values["usage_known"], testCase.known)
			}
			assertUsageChecks(t, write.Values)
		})
	}
}

// TestGatewayUsageUnitUsesSQLNullExecutionID proves a non-execution caller's
// usage row stores SQL NULL in the nullable execution_id column, never an empty
// string that a text column would happily accept.
func TestGatewayUsageUnitUsesSQLNullExecutionID(t *testing.T) {
	h := newHarness(t, nil)
	h.principals.set(testCredential, humanEngineer(), nil)
	request := invocation()
	_, err := h.gateway.Invoke(context.Background(), request)
	requireNoError(t, err)
	calls := h.store.callList()
	if len(calls) != 1 {
		t.Fatalf("usage writes = %d; want exactly 1", len(calls))
	}
	assertUnitConformsToSchema(t, calls[0].unit, gateway.UsageTable)
	values := singleInsert(t, calls[0].unit).Values
	value, present := values["execution_id"]
	if !present {
		t.Fatal("usage unit omits execution_id; the nullable column must be present as NULL")
	}
	if value != nil {
		t.Errorf("execution_id = %v (%T); want SQL NULL for a non-execution caller", value, value)
	}
}

// TestContentRecordUnitConformsToSchema proves a content unit is exactly one
// content_records insert with the configured or default retention, the exact
// captured bytes and a nullable execution id.
func TestContentRecordUnitConformsToSchema(t *testing.T) {
	request := []byte(`{"model":"small-model","messages":[{"role":"user","content":"hi"}]}`)
	response := []byte(completionJSON())
	cases := []struct {
		name       string
		execution  bool
		retain     time.Time
		wantRetain time.Time
	}{
		{
			name:       "default seven-day retention",
			execution:  true,
			wantRetain: fixedNow.Add(gateway.DefaultContentRetention),
		},
		{
			name:       "configured retention",
			execution:  true,
			retain:     fixedNow.Add(48 * time.Hour),
			wantRetain: fixedNow.Add(48 * time.Hour),
		},
		{
			name:       "human caller has a NULL execution id",
			execution:  false,
			wantRetain: fixedNow.Add(gateway.DefaultContentRetention),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			record := gateway.ContentRecord{
				Scope:       testScope(),
				PrincipalID: testPrincipal,
				Model:       testModel,
				Request:     request,
				Response:    response,
				At:          fixedNow,
				RetainUntil: testCase.retain,
			}
			if testCase.execution {
				record.ExecutionID = testExecution
			}
			unit, err := record.Unit("content-1")
			requireNoError(t, err)
			assertUnitConformsToSchema(t, unit, gateway.ContentTable)
			write := unit.Writes[0]
			if write.ID == "" {
				t.Error("content unit row id is empty")
			}
			values := write.Values
			if values["principal_id"] != string(testPrincipal) {
				t.Errorf("principal_id = %v; want %q", values["principal_id"], testPrincipal)
			}
			if testCase.execution {
				if values["execution_id"] != string(testExecution) {
					t.Errorf("execution_id = %v; want %q", values["execution_id"], testExecution)
				}
			} else if value, present := values["execution_id"]; !present || value != nil {
				t.Errorf("execution_id = %v (present=%v); want SQL NULL", value, present)
			}
			if values["model"] != testModel {
				t.Errorf("model = %v; want %q", values["model"], testModel)
			}
			if body, ok := values["request_body"].([]byte); !ok || !bytes.Equal(body, request) {
				t.Errorf("request_body = %v; want the exact request bytes", values["request_body"])
			}
			if body, ok := values["response_body"].([]byte); !ok || !bytes.Equal(body, response) {
				t.Errorf("response_body = %v; want the exact response bytes", values["response_body"])
			}
			if values["recorded_at"] != fixedNow {
				t.Errorf("recorded_at = %v; want %v", values["recorded_at"], fixedNow)
			}
			retain, ok := values["retain_until"].(time.Time)
			if !ok || !retain.Equal(testCase.wantRetain) {
				t.Errorf("retain_until = %v; want %v", values["retain_until"], testCase.wantRetain)
			}
			if !retain.After(fixedNow) {
				t.Errorf("retain_until = %v; the CHECK requires it strictly after recorded_at", retain)
			}
			if asInt64(t, values["version"]) != 1 {
				t.Errorf("version = %v; want 1", values["version"])
			}
		})
	}
}

// TestContentRecordUnitRejectsInvalidRetention proves a content unit cannot
// carry an already-expired capture or an unattributable row.
func TestContentRecordUnitRejectsInvalidRetention(t *testing.T) {
	valid := gateway.ContentRecord{
		Scope:       testScope(),
		PrincipalID: testPrincipal,
		ExecutionID: testExecution,
		Model:       testModel,
		Request:     []byte(`{"messages":[]}`),
		Response:    []byte(completionJSON()),
		At:          fixedNow,
	}
	cases := []struct {
		name   string
		mutate func(*gateway.ContentRecord)
	}{
		{"retention equal to the recorded time", func(r *gateway.ContentRecord) { r.RetainUntil = fixedNow }},
		{"retention before the recorded time", func(r *gateway.ContentRecord) { r.RetainUntil = fixedNow.Add(-time.Hour) }},
		{"zero recorded time", func(r *gateway.ContentRecord) { r.At = time.Time{} }},
		{"tenant-wide scope", func(r *gateway.ContentRecord) { r.Scope = tenancy.Scope{TenantID: testTenant} }},
		{"empty principal", func(r *gateway.ContentRecord) { r.PrincipalID = "" }},
		{"invalid execution id", func(r *gateway.ContentRecord) { r.ExecutionID = "not a valid id!" }},
		{"empty model", func(r *gateway.ContentRecord) { r.Model = "" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			record := valid
			testCase.mutate(&record)
			if _, err := record.Unit("content-1"); err == nil {
				t.Fatal("ContentRecord.Unit() = nil; want a refusal")
			} else {
				requireClass(t, err, fault.InvalidInput)
			}
		})
	}
}

// TestContentRecordUnitKeepsEmptyBodiesNonNull proves an empty capture is
// stored as a zero-length bytea, never SQL NULL, because request_body and
// response_body are NOT NULL columns.
func TestContentRecordUnitKeepsEmptyBodiesNonNull(t *testing.T) {
	record := gateway.ContentRecord{
		Scope:       testScope(),
		PrincipalID: testPrincipal,
		Model:       testModel,
		At:          fixedNow,
	}
	unit, err := record.Unit("content-empty")
	requireNoError(t, err)
	assertUnitConformsToSchema(t, unit, gateway.ContentTable)
	values := unit.Writes[0].Values
	for _, name := range []string{"request_body", "response_body"} {
		body, ok := values[name].([]byte)
		if !ok || body == nil {
			t.Errorf("%s = %#v; want a non-nil zero-length bytea", name, values[name])
		}
	}
}

// schemaExecutor is the memory-style double: it validates every unit against
// the declared schema before recording it, so a non-conforming unit fails the
// test instead of silently persisting.
type schemaExecutor struct {
	t     *testing.T
	mu    sync.Mutex
	calls []storeCall
	err   error
}

func (executor *schemaExecutor) Execute(_ context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	assertUnitConformsToSchema(executor.t, unit, gateway.ContentTable)
	executor.calls = append(executor.calls, storeCall{scope: scope, unit: unit})
	if executor.err != nil {
		return store.UnitResult{}, executor.err
	}
	return store.UnitResult{}, nil
}

func (executor *schemaExecutor) Read(context.Context, tenancy.Scope, string, string) (store.Row, error) {
	return store.Row{}, nil
}

func (executor *schemaExecutor) callList() []storeCall {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return append([]storeCall(nil), executor.calls...)
}

// TestStoreContentRecorderWritesThroughNeutralExecutor proves the store-backed
// ContentRecorder commits exactly one schema-conforming content_records unit
// through store.Executor under the record's project scope.
func TestStoreContentRecorderWritesThroughNeutralExecutor(t *testing.T) {
	executor := &schemaExecutor{t: t}
	recorder := gateway.StoreContentRecorder{Executor: executor, NewID: func() (string, error) { return "content-1", nil }}
	record := gateway.ContentRecord{
		Scope:       testScope(),
		PrincipalID: testPrincipal,
		ExecutionID: testExecution,
		Model:       testModel,
		Request:     []byte(`{"messages":[]}`),
		Response:    []byte(completionJSON()),
		At:          fixedNow,
	}
	requireNoError(t, recorder.Record(context.Background(), record))
	calls := executor.callList()
	if len(calls) != 1 {
		t.Fatalf("executor calls = %d; want exactly 1", len(calls))
	}
	if calls[0].scope != testScope() {
		t.Errorf("content write scope = %#v; want %#v", calls[0].scope, testScope())
	}
	values := singleInsert(t, calls[0].unit).Values
	if body, ok := values["request_body"].([]byte); !ok || !bytes.Equal(body, record.Request) {
		t.Errorf("request_body = %v; want the exact request bytes", values["request_body"])
	}
	if body, ok := values["response_body"].([]byte); !ok || !bytes.Equal(body, record.Response) {
		t.Errorf("response_body = %v; want the exact response bytes", values["response_body"])
	}
}

// TestStoreContentRecorderRequiresAnExecutor proves the store-backed recorder
// refuses to claim success without a persistence seam.
func TestStoreContentRecorderRequiresAnExecutor(t *testing.T) {
	recorder := gateway.StoreContentRecorder{}
	err := recorder.Record(context.Background(), gateway.ContentRecord{
		Scope:       testScope(),
		PrincipalID: testPrincipal,
		Model:       testModel,
		At:          fixedNow,
	})
	requireClass(t, err, fault.InternalFailure)
}

// TestContentRecordUnitCarriesNoCredential proves an opt-in capture built from a
// real invocation never places the provider credential or the caller credential
// in the persisted bytes or any other column.
func TestContentRecordUnitCarriesNoCredential(t *testing.T) {
	h := newHarness(t, func(config *gateway.Config) {
		config.Models[0].RecordContent = true
	})
	_, err := h.gateway.Invoke(context.Background(), invocation())
	requireNoError(t, err)
	records := h.content.recordList()
	if len(records) != 1 {
		t.Fatalf("content records = %d; want exactly 1", len(records))
	}
	unit, err := records[0].Unit("content-1")
	requireNoError(t, err)
	assertUnitConformsToSchema(t, unit, gateway.ContentTable)
	for name, value := range unit.Writes[0].Values {
		if text, ok := value.(string); ok && (strings.Contains(text, testSecret) || strings.Contains(text, testCredential)) {
			t.Errorf("content unit column %q carries a credential", name)
		}
		if data, ok := value.([]byte); ok && (bytes.Contains(data, []byte(testSecret)) || bytes.Contains(data, []byte(testCredential))) {
			t.Errorf("content unit column %q carries a credential", name)
		}
	}
}
