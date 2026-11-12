// Schema conformance: every store.Unit the proxy builds must be a valid commit
// unit against store.SchemaDesign, so a table/column drift is caught in the
// package instead of surfacing as an adapter refusal or a silently dropped
// usage row. The proxy's bounded usage detail is an audit.detail row; an
// authorized opt-in raw-content capture is the content_records row with the
// writer's seven-day default retention.
package mcpproxy_test

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/mcpproxy"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// schemaTable returns one declared table or fails the test.
func schemaTable(t *testing.T, name string) store.Table {
	t.Helper()
	for _, table := range store.SchemaDesign().Tables {
		if table.Name == name {
			return table
		}
	}
	t.Fatalf("table %q is not declared by store.SchemaDesign", name)
	return store.Table{}
}

// schemaColumn returns one declared column or fails the test.
func schemaColumn(t *testing.T, table store.Table, name string) store.Column {
	t.Helper()
	for _, column := range table.Columns {
		if column.Name == name {
			return column
		}
	}
	t.Fatalf("table %q has no column %q in store.SchemaDesign", table.Name, name)
	return store.Column{}
}

// adapterSupplied reports whether the store adapter fills the column from the
// commit-unit scope and the row identity rather than the caller.
func adapterSupplied(name string) bool {
	switch name {
	case "id", "tenant_id", "project_id":
		return true
	default:
		return false
	}
}

// assertRequiredColumnsPresent proves every non-nullable, default-free column
// the adapter does not supply is carried by the row.
func assertRequiredColumnsPresent(t *testing.T, table store.Table, row map[string]any) {
	t.Helper()
	for _, column := range table.Columns {
		if column.Nullable || column.Default != "" || adapterSupplied(column.Name) {
			continue
		}
		value, ok := row[column.Name]
		if !ok || value == nil {
			t.Fatalf("row for table %q omits required column %q", table.Name, column.Name)
		}
	}
}

// validateUnitAgainstSchemaDesign checks every audit row and business write the
// proxy built against the declared schema design. The unit scope must be the
// audit row scope the adapter derives the tenant/project columns from.
func validateUnitAgainstSchemaDesign(t *testing.T, scope tenancy.Scope, unit store.Unit) {
	t.Helper()
	if unit.Receipt != nil {
		t.Fatalf("tool.invoke has no receipt policy; the proxy unit carries one")
	}
	if len(unit.Events) != 0 || len(unit.Locks) != 0 {
		t.Fatalf("the proxy usage unit carries events/locks: %+v", unit)
	}
	auditTable := schemaTable(t, "audit")
	for _, audit := range unit.Audit {
		if audit.Scope != scope {
			t.Fatalf("audit row scope = %+v; want the unit scope %+v", audit.Scope, scope)
		}
		if audit.ID == "" || audit.Actor == "" || audit.Operation == "" || audit.At.IsZero() {
			t.Fatalf("audit row omits a required value: %+v", audit)
		}
		row := map[string]any{
			"id":         string(audit.ID),
			"tenant_id":  string(audit.Scope.TenantID),
			"project_id": nil,
			"actor":      string(audit.Actor),
			"operation":  audit.Operation,
			"at":         audit.At,
			"detail":     audit.Detail,
		}
		if audit.Scope.ProjectID != "" {
			row["project_id"] = string(audit.Scope.ProjectID)
		}
		for name, value := range row {
			column := schemaColumn(t, auditTable, name)
			if value == nil && !column.Nullable {
				t.Fatalf("audit row has a NULL in required column %q", name)
			}
		}
		assertRequiredColumnsPresent(t, auditTable, row)
	}
	for _, write := range unit.Writes {
		table := schemaTable(t, write.Table)
		if !write.Insert {
			t.Fatalf("the proxy wrote a non-insert row to %q", write.Table)
		}
		if write.ID == "" {
			t.Fatalf("the proxy write to %q has no row identity", write.Table)
		}
		for name := range write.Values {
			schemaColumn(t, table, name)
		}
		assertRequiredColumnsPresent(t, table, write.Values)
		assertColumnValueShape(t, table, write.Values)
	}
}

// assertColumnValueShape proves each written value has the Go shape the
// declared column type accepts, so a text/bigint/timestamptz/bytea drift is
// caught too.
func assertColumnValueShape(t *testing.T, table store.Table, values map[string]any) {
	t.Helper()
	for name, value := range values {
		column := schemaColumn(t, table, name)
		if value == nil {
			if !column.Nullable {
				t.Fatalf("write to %q has a NULL in required column %q", table.Name, name)
			}
			continue
		}
		switch column.Type {
		case "text":
			if _, ok := value.(string); !ok {
				t.Fatalf("write column %q expects text; got %T", name, value)
			}
		case "bigint", "integer":
			switch value.(type) {
			case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			default:
				t.Fatalf("write column %q expects %s; got %T", name, column.Type, value)
			}
		case "bytea", "jsonb":
			if _, ok := value.([]byte); !ok {
				t.Fatalf("write column %q expects %s; got %T", name, column.Type, value)
			}
		case "timestamptz":
			if _, ok := value.(time.Time); !ok {
				t.Fatalf("write column %q expects timestamptz; got %T", name, value)
			}
		case "boolean":
			if _, ok := value.(bool); !ok {
				t.Fatalf("write column %q expects boolean; got %T", name, value)
			}
		}
	}
}

// toolErrorResponse is a valid MCP tool-error result.
func toolErrorResponse(context.Context, mcpproxy.UpstreamRequest) (io.ReadCloser, error) {
	return newTrackedBody([]byte(`{"content":{"message":"bad input"},"isError":true}`)), nil
}

// dependencyUnavailableResponse is a proven pre-effect transport failure.
func dependencyUnavailableResponse(context.Context, mcpproxy.UpstreamRequest) (io.ReadCloser, error) {
	return nil, &mcpproxy.UpstreamError{Class: fault.DependencyUnavailable, Dispatched: false}
}

// dispatchedFailureResponse is a transport failure after a possible dispatch.
func dispatchedFailureResponse(context.Context, mcpproxy.UpstreamRequest) (io.ReadCloser, error) {
	return nil, &mcpproxy.UpstreamError{Class: fault.DependencyUnavailable, Dispatched: true}
}

// malformedResponse is a body the declared response decoder refuses.
func malformedResponse(context.Context, mcpproxy.UpstreamRequest) (io.ReadCloser, error) {
	return newTrackedBody([]byte(`{"content":`)), nil
}

// TestEveryProxyUnitConformsToSchemaDesign drives every usage-recording outcome
// and validates each committed unit against the declared schema design.
func TestEveryProxyUnitConformsToSchemaDesign(t *testing.T) {
	cases := []struct {
		name        string
		rawContent  bool
		human       bool
		respond     func(context.Context, mcpproxy.UpstreamRequest) (io.ReadCloser, error)
		wantAudits  int
		wantContent int
	}{
		{name: "default-success", wantAudits: 1},
		{name: "raw-content-execution", rawContent: true, wantAudits: 1, wantContent: 1},
		{name: "raw-content-human", rawContent: true, human: true, wantAudits: 1, wantContent: 1},
		{name: "raw-content-tool-error", rawContent: true, respond: toolErrorResponse, wantAudits: 1, wantContent: 1},
		{name: "dependency-unavailable", respond: dependencyUnavailableResponse, wantAudits: 1},
		{name: "uncertain-dispatched", respond: dispatchedFailureResponse, wantAudits: 1},
		{name: "malformed-response", respond: malformedResponse, wantAudits: 1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newFakes()
			if testCase.human {
				fake.credentials.setPrincipal(humanEngineer())
			}
			if testCase.respond != nil {
				fake.transport.setResponse(testCase.respond)
			}
			config := fake.config()
			config.Retention = mcpproxy.Retention{RawContent: testCase.rawContent}
			proxy, err := mcpproxy.New(config)
			if err != nil {
				t.Fatalf("New returned error: %v", err)
			}
			// Some outcomes settle as an error after the effect; the usage unit
			// is still committed and is what this test validates.
			_, _ = proxy.Call(context.Background(), readCallRequest())

			units := fake.store.allUnits()
			if len(units) != 1 {
				t.Fatalf("usage units = %d; want exactly 1", len(units))
			}
			unit := units[0]
			_, scope, ok := fake.store.lastUnit()
			if !ok {
				t.Fatal("the usage unit was not recorded")
			}
			validateUnitAgainstSchemaDesign(t, scope, unit)
			if len(unit.Audit) != testCase.wantAudits {
				t.Fatalf("audit rows = %d; want %d", len(unit.Audit), testCase.wantAudits)
			}
			contentWrites := 0
			for _, write := range unit.Writes {
				if write.Table == store.ContentRecordsTable {
					contentWrites++
				}
			}
			if contentWrites != testCase.wantContent {
				t.Fatalf("content_records writes = %d; want %d", contentWrites, testCase.wantContent)
			}
		})
	}
}

// TestRawContentTargetsContentRecordsExactly proves an authorized opt-in
// records the redacted tool bytes in the content_records row with the seven-day
// default retention, and the audit detail stays metadata-only.
func TestRawContentTargetsContentRecordsExactly(t *testing.T) {
	fake := newFakes()
	fake.transport.setResponse(func(context.Context, mcpproxy.UpstreamRequest) (io.ReadCloser, error) {
		return newTrackedBody([]byte(`{"content":{"echo":"MCP-SECRET-123"},"isError":false}`)), nil
	})
	config := fake.config()
	config.Retention = mcpproxy.Retention{RawContent: true}
	proxy, err := mcpproxy.New(config)
	if err != nil {
		t.Fatalf("New with raw retention returned error: %v", err)
	}
	if _, err := proxy.Call(context.Background(), readCallRequest()); err != nil {
		t.Fatalf("Call returned error: %v", err)
	}

	units := fake.store.allUnits()
	if len(units) != 1 || len(units[0].Writes) != 1 {
		t.Fatalf("usage unit does not carry exactly one content write: %+v", units)
	}
	write := units[0].Writes[0]
	if write.Table != store.ContentRecordsTable {
		t.Fatalf("raw content target = %q; want %q", write.Table, store.ContentRecordsTable)
	}
	_, scope, ok := fake.store.lastUnit()
	if !ok {
		t.Fatal("the usage unit was not recorded")
	}
	validateUnitAgainstSchemaDesign(t, scope, units[0])

	if got := write.Values["principal_id"]; got != string(executionPrincipal().ID) {
		t.Fatalf("content principal_id = %v; want %q", got, executionPrincipal().ID)
	}
	if got := write.Values["execution_id"]; got != string(testExecution) {
		t.Fatalf("content execution_id = %v; want %q", got, testExecution)
	}
	if got := write.Values["model"]; got != "files/read_file" {
		t.Fatalf("content capability identity = %v; want files/read_file", got)
	}
	request, ok := write.Values["request_body"].([]byte)
	if !ok || len(request) == 0 {
		t.Fatalf("content request_body = %v; want the validated arguments", write.Values["request_body"])
	}
	response, ok := write.Values["response_body"].([]byte)
	if !ok || len(response) == 0 {
		t.Fatalf("content response_body = %v; want the redacted tool content", write.Values["response_body"])
	}
	if bytes.Contains(response, []byte(testSecret)) {
		t.Fatalf("content response_body leaked the credential: %q", response)
	}
	recorded, ok := write.Values["recorded_at"].(time.Time)
	if !ok || !recorded.Equal(fixedNow) {
		t.Fatalf("content recorded_at = %v; want %v", write.Values["recorded_at"], fixedNow)
	}
	retainUntil, ok := write.Values["retain_until"].(time.Time)
	if !ok || !retainUntil.Equal(fixedNow.Add(mcpproxy.DefaultRetention)) {
		t.Fatalf("content retain_until = %v; want %v", write.Values["retain_until"], fixedNow.Add(mcpproxy.DefaultRetention))
	}
	if !retainUntil.After(recorded) {
		t.Fatalf("content retain_until %v is not after recorded_at %v", retainUntil, recorded)
	}
	if got := write.Values["version"]; got != uint64(1) {
		t.Fatalf("content version = %v; want 1", got)
	}

	detail := decodeDetail(t, units[0].Audit[0].Detail)
	if retained, ok := detail["content_retained"].(bool); !ok || !retained {
		t.Fatalf("audit content_retained = %v; want true", detail["content_retained"])
	}
	if _, ok := detail["content"]; ok {
		t.Fatalf("audit detail still carries raw content: %v", detail["content"])
	}
	if _, ok := detail["retain_until"]; ok {
		t.Fatalf("audit detail still carries a retention deadline: %v", detail["retain_until"])
	}
	if bytes.Contains([]byte(units[0].Audit[0].Detail), []byte(testSecret)) {
		t.Fatalf("audit detail leaked the credential: %q", units[0].Audit[0].Detail)
	}
}

// TestDefaultUsageWritesNoContentRecord proves raw content is off by default:
// the unit carries the attributable audit row and no content capture.
func TestDefaultUsageWritesNoContentRecord(t *testing.T) {
	fake := newFakes()
	proxy := fake.newProxy(t)
	if _, err := proxy.Call(context.Background(), readCallRequest()); err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	units := fake.store.allUnits()
	if len(units) != 1 {
		t.Fatalf("usage units = %d; want 1", len(units))
	}
	if len(units[0].Writes) != 0 {
		t.Fatalf("default usage wrote %+v; want no content capture", units[0].Writes)
	}
}

// TestHumanContentRecordHasNoExecutionID proves a non-execution caller's
// content row leaves the nullable execution_id absent rather than inventing an
// execution identity.
func TestHumanContentRecordHasNoExecutionID(t *testing.T) {
	fake := newFakes()
	fake.credentials.setPrincipal(humanEngineer())
	config := fake.config()
	config.Retention = mcpproxy.Retention{RawContent: true}
	proxy, err := mcpproxy.New(config)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if _, err := proxy.Call(context.Background(), readCallRequest()); err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	units := fake.store.allUnits()
	if len(units) != 1 || len(units[0].Writes) != 1 {
		t.Fatalf("usage unit does not carry exactly one content write: %+v", units)
	}
	if value, ok := units[0].Writes[0].Values["execution_id"]; !ok || value != nil {
		t.Fatalf("human content row execution_id = %v; want an explicit SQL NULL", value)
	}
	_, scope, ok := fake.store.lastUnit()
	if !ok {
		t.Fatal("the usage unit was not recorded")
	}
	validateUnitAgainstSchemaDesign(t, scope, units[0])
}
