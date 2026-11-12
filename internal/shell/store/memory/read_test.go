// Scoped reads and inspection helpers: a read never widens the caller's scope,
// a missing row is not_found, retained bytes round-trip exactly, and every
// returned value is a defensive copy of committed state.
package memory

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/store"
)

func TestReadReturnsScopedDefensiveCopies(t *testing.T) {
	s := New()
	scope := projectScope()
	yaml := []byte("name: build\non:\n  push:\n    branches: [main]\n")
	seedRow(t, s, scope, "pipelines", "pipeline-1", map[string]any{
		"name": "build", "yaml": yaml, "yaml_sha256": sha256Sum(yaml), "version": uint64(1),
	})

	row, err := s.Read(context.Background(), scope, "pipelines", "pipeline-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if row.Table != "pipelines" {
		t.Errorf("row table = %q; want pipelines", row.Table)
	}
	stored, ok := row.Values["yaml"].([]byte)
	if !ok || !bytes.Equal(stored, yaml) {
		t.Fatalf("retained YAML = %v; want exact %q", row.Values["yaml"], yaml)
	}

	// Mutating the returned row must not alias stored state.
	stored[0] = 'X'
	row.Values["name"] = "mutated"
	delete(row.Values, "version")

	again, err := s.Read(context.Background(), scope, "pipelines", "pipeline-1")
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	againYAML, _ := again.Values["yaml"].([]byte)
	if !bytes.Equal(againYAML, yaml) {
		t.Errorf("mutating a Read result changed stored YAML to %q", againYAML)
	}
	if got := again.Values["name"]; got != "build" {
		t.Errorf("mutating a Read result changed the stored name to %v", got)
	}
	if got := rowVersion(t, again, "version"); got != 1 {
		t.Errorf("mutating a Read result changed the stored version to %d", got)
	}

	snapshot := mustSnapshot(t, s, scope, "pipelines", "pipeline-1")
	snapshotYAML, _ := snapshot.Values["yaml"].([]byte)
	snapshotYAML[0] = 'Y'
	snapshot.Values["name"] = "snapshot-mutated"
	snapshotAgain := mustSnapshot(t, s, scope, "pipelines", "pipeline-1")
	if got, _ := snapshotAgain.Values["yaml"].([]byte); !bytes.Equal(got, yaml) {
		t.Errorf("mutating a Snapshot result changed stored YAML to %q", got)
	}
	if got := snapshotAgain.Values["name"]; got != "build" {
		t.Errorf("mutating a Snapshot result changed the stored name to %v", got)
	}
}

func TestReadScopeIsolationAndRefusals(t *testing.T) {
	s := New()
	yaml := []byte("name: build\n")
	seedRow(t, s, projectScope(), "pipelines", "pipeline-1", map[string]any{
		"name": "build", "yaml": yaml, "yaml_sha256": sha256Sum(yaml), "version": uint64(1),
	})
	seedRow(t, s, tenantScope(), "services", testServiceID, insertValues("services"))
	seedRow(t, s, projectScope(), "tenants", testTenantID, insertValues("tenants"))

	_, err := s.Read(context.Background(), otherProjectScope(), "pipelines", "pipeline-1")
	assertFault(t, err, fault.NotFound)

	_, err = s.Read(context.Background(), tenantScope(), "pipelines", "pipeline-1")
	assertFault(t, err, fault.InvalidInput)

	_, err = s.Read(context.Background(), projectScope(), "not_a_table", "id-1")
	assertFault(t, err, fault.InvalidInput)

	_, err = s.Read(context.Background(), projectScope(), "pipelines", "bad id")
	assertFault(t, err, fault.InvalidInput)

	_, err = s.Read(context.Background(), projectScope(), "pipelines", "pipeline-2")
	assertFault(t, err, fault.NotFound)

	if _, err := s.Read(context.Background(), tenantScope(), "services", testServiceID); err != nil {
		t.Errorf("tenant-scoped read of a tenant-wide row: %v", err)
	}
	_, err = s.Read(context.Background(), projectScope(), "services", testServiceID)
	assertFault(t, err, fault.NotFound)

	if _, err := s.Read(context.Background(), projectScope(), "tenants", testTenantID); err != nil {
		t.Errorf("project-scoped read of its own tenant row: %v", err)
	}
	_, err = s.Read(context.Background(), projectScope(), "tenants", otherTenantID)
	assertFault(t, err, fault.NotFound)
}

func TestReadSeesOnlyCommittedState(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))
	unit := store.Unit{Writes: []store.Write{
		{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"state": "running"}},
	}}

	s.FailNextCommit(errAborted)
	if _, err := s.Execute(context.Background(), scope, unit); err == nil {
		t.Fatal("the armed failure did not fail the unit")
	}
	row, err := s.Read(context.Background(), scope, "executions", testExecutionID)
	if err != nil {
		t.Fatalf("Read after failed unit: %v", err)
	}
	if got := rowString(t, row, "state"); got != "queued" {
		t.Errorf("state after failed unit = %q; want queued", got)
	}
	if got := rowVersion(t, row, "version"); got != 1 {
		t.Errorf("version after failed unit = %d; want 1", got)
	}

	mustExecute(t, s, scope, unit)
	row, err = s.Read(context.Background(), scope, "executions", testExecutionID)
	if err != nil {
		t.Fatalf("Read after commit: %v", err)
	}
	if got := rowString(t, row, "state"); got != "running" {
		t.Errorf("state after commit = %q; want running", got)
	}
	if got := rowVersion(t, row, "version"); got != 2 {
		t.Errorf("version after commit = %d; want 2", got)
	}
}

func TestReadCanceledContext(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Read(ctx, scope, "executions", testExecutionID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Read with a canceled context = %v; want context.Canceled", err)
	}
}
