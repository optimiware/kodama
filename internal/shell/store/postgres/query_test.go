// Scoped read and retained-fact round-trip behavior.
//
// Reads are parameterized, scoped and defensive: a row that carries a
// different tenant/project than the transaction scope is refused even if a
// query predicate were wrong. Retained pipeline YAML, launch snapshots, output
// settlement, stop evidence and cleanup progress keep their exact bytes.
package postgres

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/verify"
)

func TestStoreImplementsVerifyBindingReader(t *testing.T) {
	var _ verify.BindingReader = New(newFakeDB())
}

func bindingFixture() map[string]any {
	return map[string]any{
		"id":                "binding-1",
		"tenant_id":         testTenantID,
		"project_id":        testProjectID,
		"resource_id":       testResourceID,
		"service_id":        testServiceID,
		"origins":           []byte(`["https://git.example.com"]`),
		"operations":        []byte(`["git.ref.update"]`),
		"reportable_events": []byte(`["git.ref.updated"]`),
		"metadata":          []byte(`{"repo":"acme/app"}`),
		"version":           int64(4),
	}
}

func TestStoreReadBindingUsesResourceAndServiceIdentity(t *testing.T) {
	db := newFakeDB()
	db.script.setQuery(queryByTable(map[string]map[string]any{"resource_bindings": bindingFixture()}))
	connector := New(db)
	binding, err := connector.ReadBinding(context.Background(), verify.BindingQuery{
		Scope:      testProjectScope(),
		ResourceID: testResourceID,
		ServiceID:  testServiceID,
	})
	if err != nil {
		t.Fatalf("ReadBinding: %v", err)
	}
	if string(binding.ID) != "binding-1" {
		t.Errorf("binding id = %q; want binding-1", binding.ID)
	}
	if string(binding.ResourceID) != testResourceID {
		t.Errorf("binding resource id = %q; want %q", binding.ResourceID, testResourceID)
	}
	if string(binding.EnforcingService) != testServiceID {
		t.Errorf("binding enforcing service = %q; want %q", binding.EnforcingService, testServiceID)
	}
	if binding.Scope != testProjectScope() {
		t.Errorf("binding scope = %+v; want %+v", binding.Scope, testProjectScope())
	}
	if binding.Version != 4 {
		t.Errorf("binding version = %d; want 4", binding.Version)
	}
	if len(binding.TrustedOrigins) != 1 || binding.TrustedOrigins[0] != "https://git.example.com" {
		t.Errorf("binding origins = %v", binding.TrustedOrigins)
	}
	if len(binding.Operations) != 1 || string(binding.Operations[0]) != "git.ref.update" {
		t.Errorf("binding operations = %v", binding.Operations)
	}
	if len(binding.ReportableEvents) != 1 || binding.ReportableEvents[0] != "git.ref.updated" {
		t.Errorf("binding reportable events = %v", binding.ReportableEvents)
	}
	if binding.Metadata["repo"] != "acme/app" {
		t.Errorf("binding metadata = %v", binding.Metadata)
	}

	conn := db.conn(t)
	queries := queryCalls(conn.callsCopy())
	if len(queries) != 1 {
		t.Fatalf("ReadBinding issued %d queries; want 1", len(queries))
	}
	text := strings.ToLower(queries[0].sql)
	for _, column := range []string{"resource_id", "service_id", "tenant_id", "project_id"} {
		if !strings.Contains(text, column) {
			t.Errorf("binding lookup does not filter on %q: %s", column, queries[0].sql)
		}
	}
	for _, value := range []any{testTenantID, testProjectID, testResourceID, testServiceID} {
		if !argContains(queries[0].args, value) {
			t.Errorf("binding lookup args %v do not carry %v", queries[0].args, value)
		}
	}
	if strings.Contains(text, testResourceID) || strings.Contains(text, testServiceID) {
		t.Errorf("binding lookup interpolated an identity: %s", queries[0].sql)
	}
	if conn.terminalCalls() != 1 {
		t.Fatalf("ReadBinding issued %d terminal calls; want exactly 1", conn.terminalCalls())
	}
}

func TestStoreReadBindingRefusesInvalidQueryBeforeBegin(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	_, err := connector.ReadBinding(context.Background(), verify.BindingQuery{
		Scope:      testProjectScope(),
		ResourceID: "bad id",
		ServiceID:  testServiceID,
	})
	assertFaultClass(t, err, fault.InvalidInput)
	if got := db.beginCount(); got != 0 {
		t.Fatalf("invalid binding query opened %d transactions", got)
	}
}

func TestStoreReadBindingRefusesTenantScope(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	_, err := connector.ReadBinding(context.Background(), verify.BindingQuery{
		Scope:      testTenantScope(),
		ResourceID: testResourceID,
		ServiceID:  testServiceID,
	})
	// A binding is project-scoped. Refusing the request or finding no row are
	// both safe; returning a binding without project authority is not.
	assertFaultClassAny(t, err, fault.InvalidInput, fault.NotFound)
	if got := db.beginCount(); got > 1 {
		t.Fatalf("tenant-scoped binding query opened %d transactions", got)
	}
}

func TestStoreReadBindingNotFound(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	_, err := connector.ReadBinding(context.Background(), verify.BindingQuery{
		Scope:      testProjectScope(),
		ResourceID: testResourceID,
		ServiceID:  testServiceID,
	})
	assertFaultClass(t, err, fault.NotFound)
	if got := db.conn(t).terminalCalls(); got != 1 {
		t.Fatalf("not-found binding read issued %d terminal calls; want 1", got)
	}
}

func TestStoreReadBindingRefusesCrossScopeRow(t *testing.T) {
	fixture := bindingFixture()
	fixture["tenant_id"] = "tenant-2"
	db := newFakeDB()
	db.script.setQuery(queryByTable(map[string]map[string]any{"resource_bindings": fixture}))
	connector := New(db)
	_, err := connector.ReadBinding(context.Background(), verify.BindingQuery{
		Scope:      testProjectScope(),
		ResourceID: testResourceID,
		ServiceID:  testServiceID,
	})
	assertFaultClass(t, err, fault.Forbidden)
	if got := db.conn(t).terminalCalls(); got != 1 {
		t.Fatalf("cross-scope binding read issued %d terminal calls; want 1", got)
	}
}

func TestStoreReadBindingRollsBackOnQueryFailure(t *testing.T) {
	db := newFakeDB()
	injected := errors.New("query failed")
	db.script.setQuery(func(sql string, args []any) (store.Rows, error) {
		return nil, injected
	})
	connector := New(db)
	_, err := connector.ReadBinding(context.Background(), verify.BindingQuery{
		Scope:      testProjectScope(),
		ResourceID: testResourceID,
		ServiceID:  testServiceID,
	})
	if !errors.Is(err, injected) {
		t.Fatalf("ReadBinding error = %v; want the injected cause", err)
	}
	if got := db.conn(t).terminalCalls(); got != 1 {
		t.Fatalf("failed binding read issued %d terminal calls; want 1 rollback", got)
	}
}

func pipelineFixture(yaml []byte, hash []byte) map[string]any {
	return map[string]any{
		"id":          "pipeline-1",
		"tenant_id":   testTenantID,
		"project_id":  testProjectID,
		"name":        "build",
		"yaml":        yaml,
		"yaml_sha256": hash,
		"version":     int64(3),
		"created_at":  testTime(),
	}
}

func TestReadRowReturnsExactRetainedPipelineYAML(t *testing.T) {
	yaml := []byte("name: build\non:\n  push:\n    branches: [main]\n# exact trailing comment\n")
	hash := testDigest(yaml)
	db := newFakeDB()
	var captured *fakeRows
	db.script.setQuery(func(sql string, args []any) (store.Rows, error) {
		captured = singleRow("pipelines", sql, pipelineFixture(yaml, hash))
		return captured, nil
	})
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	row, err := ReadRow(context.Background(), tx, testProjectScope(), "pipelines", "pipeline-1")
	if err != nil {
		t.Fatalf("ReadRow: %v", err)
	}
	if row.Table != "pipelines" {
		t.Errorf("row table = %q", row.Table)
	}
	stored, ok := row.Values["yaml"].([]byte)
	if !ok {
		t.Fatalf("yaml column = %T; want []byte", row.Values["yaml"])
	}
	if !bytes.Equal(stored, yaml) {
		t.Errorf("retained YAML changed:\n got %q\nwant %q", stored, yaml)
	}
	storedHash, ok := row.Values["yaml_sha256"].([]byte)
	if !ok || !bytes.Equal(storedHash, hash) {
		t.Errorf("retained YAML hash = %v; want %v", row.Values["yaml_sha256"], hash)
	}
	if captured == nil || !captured.isClosed() {
		t.Error("ReadRow did not close its rows")
	}

	queries := queryCalls(db.conn(t).callsCopy())
	if len(queries) != 1 {
		t.Fatalf("ReadRow issued %d queries; want 1", len(queries))
	}
	text := strings.ToLower(queries[0].sql)
	if !strings.Contains(text, "tenant_id") || !strings.Contains(text, "project_id") {
		t.Errorf("scoped read does not filter on ownership: %s", queries[0].sql)
	}
	for _, value := range []any{testTenantID, testProjectID, "pipeline-1"} {
		if !argContains(queries[0].args, value) {
			t.Errorf("scoped read args %v do not carry %v", queries[0].args, value)
		}
	}
	if strings.Contains(text, "pipeline-1") {
		t.Errorf("scoped read interpolated the row identity: %s", queries[0].sql)
	}
}

func executionFixture(snapshot, nonce []byte) map[string]any {
	return map[string]any{
		"id":               testExecutionID,
		"tenant_id":        testTenantID,
		"project_id":       testProjectID,
		"pipeline_id":      "pipeline-1",
		"task_id":          "task-1",
		"principal_id":     testPrincipalID,
		"state":            "running",
		"version":          int64(2),
		"yaml_sha256":      testDigest([]byte("name: build\n")),
		"snapshot":         snapshot,
		"token_nonce":      nonce,
		"token_expires_at": testTime().Add(2 * time.Hour),
		"workspace_id":     "workspace-1",
		"created_at":       testTime(),
	}
}

func TestReadRowReturnsExactExecutionLaunchFacts(t *testing.T) {
	snapshot := []byte(`{"yaml":"name: build\n","inputs":{"a":"1"}}`)
	nonce := bytes.Repeat([]byte{0x7f}, 32)
	db := newFakeDB()
	db.script.setQuery(queryByTable(map[string]map[string]any{"executions": executionFixture(snapshot, nonce)}))
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	row, err := ReadRow(context.Background(), tx, testProjectScope(), "executions", testExecutionID)
	if err != nil {
		t.Fatalf("ReadRow: %v", err)
	}
	storedSnapshot, ok := row.Values["snapshot"].([]byte)
	if !ok || !bytes.Equal(storedSnapshot, snapshot) {
		t.Errorf("launch snapshot = %v; want exact %q", row.Values["snapshot"], snapshot)
	}
	storedNonce, ok := row.Values["token_nonce"].([]byte)
	if !ok || !bytes.Equal(storedNonce, nonce) {
		t.Errorf("token nonce = %v; want %v", row.Values["token_nonce"], nonce)
	}
	if row.Values["workspace_id"] != "workspace-1" {
		t.Errorf("workspace id = %v", row.Values["workspace_id"])
	}
}

func actionFixture(outputs, staged, stop []byte) map[string]any {
	return map[string]any{
		"id":                 "action-1",
		"execution_id":       testExecutionID,
		"tenant_id":          testTenantID,
		"project_id":         testProjectID,
		"index":              int64(0),
		"name":               "action-1",
		"kind":               "container",
		"state":              "succeeded",
		"version":            int64(5),
		"timeout_ms":         int64(1000),
		"deadline":           testTime(),
		"outputs":            outputs,
		"staged_outputs":     staged,
		"staged_outputs_set": true,
		"stop":               stop,
	}
}

func TestReadRowReturnsActionOutputSettlementAndStopEvidence(t *testing.T) {
	outputs := []byte(`{"result":"ok"}`)
	staged := []byte(`{"result":"pending"}`)
	stop := []byte(`{"reason":"cancelled","confirmed":true}`)
	db := newFakeDB()
	db.script.setQuery(queryByTable(map[string]map[string]any{"actions": actionFixture(outputs, staged, stop)}))
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	row, err := ReadRow(context.Background(), tx, testProjectScope(), "actions", "action-1")
	if err != nil {
		t.Fatalf("ReadRow: %v", err)
	}
	for column, want := range map[string][]byte{"outputs": outputs, "staged_outputs": staged, "stop": stop} {
		got, ok := row.Values[column].([]byte)
		if !ok || !bytes.Equal(got, want) {
			t.Errorf("%s = %v; want exact %q", column, row.Values[column], want)
		}
	}
	if row.Values["staged_outputs_set"] != true {
		t.Errorf("staged_outputs_set = %v; want true", row.Values["staged_outputs_set"])
	}
}

func cleanupFixture() map[string]any {
	return map[string]any{
		"id":            "cleanup-1",
		"tenant_id":     testTenantID,
		"project_id":    testProjectID,
		"execution_id":  testExecutionID,
		"workload_id":   "workload-1",
		"workspace_id":  "workspace-1",
		"state":         "pending",
		"attempts":      int64(2),
		"last_error":    "runtime unavailable",
		"next_retry_at": testTime(),
	}
}

func TestReadRowReturnsCleanupProgress(t *testing.T) {
	db := newFakeDB()
	db.script.setQuery(queryByTable(map[string]map[string]any{"cleanup": cleanupFixture()}))
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	row, err := ReadRow(context.Background(), tx, testProjectScope(), "cleanup", "cleanup-1")
	if err != nil {
		t.Fatalf("ReadRow: %v", err)
	}
	if row.Values["state"] != "pending" {
		t.Errorf("cleanup state = %v", row.Values["state"])
	}
	if row.Values["last_error"] != "runtime unavailable" {
		t.Errorf("cleanup last_error = %v", row.Values["last_error"])
	}
}

func TestWriteRowPreservesRetainedPipelineYAML(t *testing.T) {
	yaml := []byte("name: build\non:\n  push:\n    branches: [main]\n# exact trailing comment\n")
	hash := testDigest(yaml)
	db := newFakeDB()
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	err = WriteRow(context.Background(), tx, testProjectScope(), store.Write{
		Table:  "pipelines",
		ID:     "pipeline-1",
		Insert: true,
		Values: map[string]any{
			"name":        "build",
			"yaml":        yaml,
			"yaml_sha256": hash,
			"version":     uint64(1),
			"created_at":  testTime(),
		},
	})
	if err != nil {
		t.Fatalf("WriteRow: %v", err)
	}
	calls := dataExecCalls(db.conn(t).callsCopy())
	if len(calls) != 1 {
		t.Fatalf("pipeline insert issued %d statements", len(calls))
	}
	if !argContains(calls[0].args, yaml) {
		t.Errorf("retained YAML did not travel byte-exactly: %v", calls[0].args)
	}
	if !argContains(calls[0].args, hash) {
		t.Errorf("retained YAML hash did not travel byte-exactly: %v", calls[0].args)
	}
	if strings.Contains(string(calls[0].sql), "name: build") {
		t.Errorf("YAML was interpolated into SQL: %s", calls[0].sql)
	}
}

func TestWriteRowPreservesLaunchStopAndCleanupFacts(t *testing.T) {
	snapshot := []byte(`{"yaml":"name: build\n"}`)
	nonce := bytes.Repeat([]byte{0x2a}, 32)
	outputs := []byte(`{"result":"ok"}`)
	staged := []byte(`{"result":"pending"}`)
	stop := []byte(`{"reason":"cancelled","confirmed":true}`)

	db := newFakeDB()
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := WriteRow(context.Background(), tx, testProjectScope(), store.Write{
		Table:         "executions",
		ID:            testExecutionID,
		ExpectVersion: 2,
		Values: map[string]any{
			"state":            "running",
			"snapshot":         snapshot,
			"token_nonce":      nonce,
			"token_expires_at": testTime(),
			"workspace_id":     "workspace-1",
		},
	}); err != nil {
		t.Fatalf("execution launch write: %v", err)
	}
	if err := WriteRow(context.Background(), tx, testProjectScope(), store.Write{
		Table:         "actions",
		ID:            "action-1",
		ExpectVersion: 5,
		Values: map[string]any{
			"state":              "succeeded",
			"outputs":            outputs,
			"staged_outputs":     staged,
			"staged_outputs_set": true,
			"stop":               stop,
		},
	}); err != nil {
		t.Fatalf("action settlement write: %v", err)
	}
	if err := WriteRow(context.Background(), tx, testProjectScope(), store.Write{
		Table:  "cleanup",
		ID:     "cleanup-1",
		Insert: true,
		Values: map[string]any{
			"execution_id":  testExecutionID,
			"state":         "pending",
			"attempts":      int64(0),
			"next_retry_at": testTime(),
		},
	}); err != nil {
		t.Fatalf("cleanup write: %v", err)
	}

	calls := dataExecCalls(db.conn(t).callsCopy())
	if len(calls) != 3 {
		t.Fatalf("launch/stop/cleanup writes issued %d statements; want 3", len(calls))
	}
	for _, value := range []any{snapshot, nonce, outputs, staged, stop} {
		if !argContains(calls[0].args, value) && !argContains(calls[1].args, value) {
			t.Errorf("retained value %q did not travel as an argument", value)
		}
	}
	if !argContains(calls[1].args, true) {
		t.Errorf("staged_outputs_set did not travel as an argument: %v", calls[1].args)
	}
}

func TestReadRowRefusesCrossScopeRow(t *testing.T) {
	yaml := []byte("name: build\n")
	fixture := pipelineFixture(yaml, testDigest(yaml))
	fixture["project_id"] = "project-2"
	db := newFakeDB()
	db.script.setQuery(queryByTable(map[string]map[string]any{"pipelines": fixture}))
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = ReadRow(context.Background(), tx, testProjectScope(), "pipelines", "pipeline-1")
	assertFaultClass(t, err, fault.Forbidden)
}

func TestReadRowNotFound(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = ReadRow(context.Background(), tx, testProjectScope(), "pipelines", "pipeline-1")
	assertFaultClass(t, err, fault.NotFound)
}

func TestReadRowRequiresProjectScopeForProjectTables(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testTenantScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = ReadRow(context.Background(), tx, testTenantScope(), "pipelines", "pipeline-1")
	// A project-required table cannot be read with tenant scope only. Refusing
	// or finding no row is safe; returning a project row is not.
	assertFaultClassAny(t, err, fault.InvalidInput, fault.NotFound)
}

func TestReadRowRefusesUnknownTable(t *testing.T) {
	db := newFakeDB()
	connector := New(db)
	tx, err := connector.Begin(context.Background(), testProjectScope())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = ReadRow(context.Background(), tx, testProjectScope(), "not_a_table", "id-1")
	assertFaultClass(t, err, fault.InvalidInput)
}
