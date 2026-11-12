//go:build component

package postgrescomponent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

type vectorDocument struct {
	Version int        `json:"version"`
	Vectors []storeVec `json:"vectors"`
}

type storeVec struct {
	Name  string       `json:"name"`
	Kind  string       `json:"kind"`
	Seed  vectorSeed   `json:"seed"`
	Steps []vectorStep `json:"steps"`
}

type vectorSeed struct {
	Rows     []vectorRow     `json:"rows"`
	Receipts []vectorReceipt `json:"receipts"`
}

type vectorRow struct {
	Table     string            `json:"table"`
	TenantID  string            `json:"tenant_id"`
	ProjectID string            `json:"project_id"`
	ID        string            `json:"id"`
	Version   uint64            `json:"version"`
	Values    map[string]string `json:"values"`
}

type vectorReceipt struct {
	TenantID    string `json:"tenant_id"`
	ProjectID   string `json:"project_id"`
	PrincipalID string `json:"principal_id"`
	Operation   string `json:"operation"`
	CallerKey   string `json:"caller_key"`
	Content     string `json:"content"`
	Result      string `json:"result"`
}

type vectorScope struct {
	TenantID  string `json:"tenant_id"`
	ProjectID string `json:"project_id"`
}

type vectorReference struct {
	Table     string `json:"table"`
	TenantID  string `json:"tenant_id"`
	ProjectID string `json:"project_id"`
	ID        string `json:"id"`
}

type vectorKey struct {
	TenantID    string `json:"tenant_id"`
	ProjectID   string `json:"project_id"`
	PrincipalID string `json:"principal_id"`
	Operation   string `json:"operation"`
	CallerKey   string `json:"caller_key"`
}

type vectorStep struct {
	Op            string            `json:"op"`
	Scope         *vectorScope      `json:"scope,omitempty"`
	Table         string            `json:"table,omitempty"`
	ID            string            `json:"id,omitempty"`
	Values        map[string]string `json:"values,omitempty"`
	ExpectVersion *uint64           `json:"expect_version,omitempty"`
	Reference     *vectorReference  `json:"reference,omitempty"`
	Key           *vectorKey        `json:"key,omitempty"`
	Content       string            `json:"content,omitempty"`
	Result        string            `json:"result,omitempty"`
	Point         string            `json:"point,omitempty"`
	WantError     string            `json:"want_error,omitempty"`
	WantResult    string            `json:"want_result,omitempty"`
	Want          map[string]string `json:"want,omitempty"`
	WantVersion   *uint64           `json:"want_version,omitempty"`
	WantAbsent    bool              `json:"want_absent,omitempty"`
	WantContent   string            `json:"want_content,omitempty"`
}

func loadStoreVectors(t *testing.T) vectorDocument {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "contracts", "adapters", "testdata", "store.json"))
	if err != nil {
		t.Fatalf("read store vectors: %v", err)
	}
	var document vectorDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode store vectors: %v", err)
	}
	if document.Version != 1 || len(document.Vectors) == 0 {
		t.Fatalf("store vector document version=%d vectors=%d; want 1 and non-empty", document.Version, len(document.Vectors))
	}
	return document
}

// TestFrozenStoreVectorsOnPostgreSQL runs the store vectors through the
// real Executor/Read seam over PostgreSQL. Each vector gets a clean database
// state; the vector's generic version maps to the table's actual optimistic
// column (tasks has no single version column, so lifecycle_version is its
// guard), and a cross-scope reference is resolved with a real scoped read.
func TestFrozenStoreVectorsOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := pgtest.Start(t)
	if _, err := pgtest.NewMigrationRunner(shard).Apply(ctx, 0); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	runtime := shard.Runtime(4)
	defer runtime.Close()
	executor := storepg.New(runtime)
	document := loadStoreVectors(t)
	tables := map[string]bool{}
	for _, table := range store.SchemaDesign().Tables {
		tables[table.Name] = true
	}
	for _, vector := range document.Vectors {
		shard.TruncateAll(ctx)
		shard.SeedScope(ctx, "tenant-1", "project-1")
		shard.SeedScope(ctx, "tenant-2", "project-2")
		seedVector(ctx, shard, vector.Seed)
		runner := &vectorRunner{ctx: ctx, t: t, shard: shard, runtime: runtime, executor: executor, tables: tables}
		for index, step := range vector.Steps {
			if err := runner.step(step); err != nil {
				t.Fatalf("vector %q step %d (%s): %v", vector.Name, index, step.Op, err)
			}
		}
	}
	writeSummary(t, shard, "store-vectors", map[string]any{"vectors": len(document.Vectors)})
}

func seedVector(ctx context.Context, shard *pgtest.Shard, seed vectorSeed) {
	for _, row := range seed.Rows {
		values := vectorDefaults(row.Table, row.Values)
		scope := vectorSeedScope(row)
		switch row.Table {
		case "tasks":
			shard.SeedTask(ctx, scope, row.ID, values["state"], int64(row.Version), "")
		case "executions":
			shard.SeedExecution(ctx, scope, row.ID, values["state"], int64(row.Version), "", "")
		case "events":
			shard.SeedEvent(ctx, scope, row.ID, values["type"], values["subject_id"], 1)
		default:
			panic("unsupported vector seed table " + row.Table)
		}
	}
	for _, record := range seed.Receipts {
		shard.SeedReceipt(ctx, pgtest.Scope(record.TenantID, record.ProjectID), "receipt-"+record.CallerKey,
			record.PrincipalID, record.Operation, record.CallerKey, record.Content, record.Result)
	}
}

// vectorSeedScope returns the seed row's own scope. The vector set
// seeds genuinely cross-scope rows whose project belongs to the same tenant, so
// the composite ownership foreign keys accept them unchanged.
func vectorSeedScope(row vectorRow) tenancy.Scope {
	return pgtest.Scope(row.TenantID, row.ProjectID)
}

func vectorDefaults(table string, values map[string]string) map[string]string {
	out := map[string]string{}
	maps.Copy(out, values)
	switch table {
	case "tasks":
		out["state"] = defaultString(out["state"], "open")
	case "executions":
		out["state"] = defaultString(out["state"], "queued")
	case "events":
		out["type"] = defaultString(out["type"], "execution.queued")
		out["subject_id"] = defaultString(out["subject_id"], "execution-1")
	}
	return out
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

type vectorRunner struct {
	ctx      context.Context
	t        *testing.T
	shard    *pgtest.Shard
	runtime  *pgdriver.DB
	executor *storepg.Store
	tables   map[string]bool

	open     bool
	executed bool
	scope    tenancy.Scope
	writes   []store.Write
	events   []event.Envelope
	receipt  *receipt.Record
}

// step runs one vector step and applies its declared error expectation.
func (runner *vectorRunner) step(step vectorStep) error {
	err := runner.dispatch(step)
	if step.WantError != "" {
		if class := vectorErrorClass(err); class != step.WantError {
			return fmt.Errorf("error = %q (%v); want %q", class, err, step.WantError)
		}
		return nil
	}
	return err
}

func (runner *vectorRunner) dispatch(step vectorStep) error {
	switch step.Op {
	case "begin":
		if step.Scope == nil || step.Scope.TenantID == "" {
			return vectorError{"invalid_scope"}
		}
		runner.open = true
		runner.executed = false
		runner.scope = scopeFromVector(step.Scope)
		runner.writes = nil
		runner.events = nil
		runner.receipt = nil
		return nil
	case "write":
		return runner.write(step)
	case "receipt":
		return runner.receiptStep(step)
	case "commit":
		if runner.executed {
			runner.executed = false
			runner.open = false
			return nil
		}
		if !runner.open {
			return vectorError{"no_transaction"}
		}
		err := runner.executePending()
		runner.open = false
		runner.writes = nil
		runner.events = nil
		runner.receipt = nil
		return err
	case "rollback":
		runner.open = false
		runner.writes = nil
		runner.events = nil
		runner.receipt = nil
		return nil
	case "fail":
		runner.open = false
		runner.writes = nil
		runner.events = nil
		runner.receipt = nil
		return vectorError{"injected_failure"}
	case "read":
		if !runner.open {
			return vectorError{"no_transaction"}
		}
		row, err := runner.executor.Read(runner.ctx, runner.scope, step.Table, step.ID)
		if err != nil {
			if step.WantAbsent {
				return nil
			}
			return err
		}
		return checkVectorRow(step, row)
	case "inspect":
		return runner.inspect(step)
	case "inspect_receipt":
		return runner.inspectReceipt(step)
	default:
		return fmt.Errorf("unknown vector op %q", step.Op)
	}
}

func (runner *vectorRunner) write(step vectorStep) error {
	if !runner.open {
		return vectorError{"no_transaction"}
	}
	if !runner.tables[step.Table] {
		runner.open = false
		return vectorError{"invalid_table"}
	}
	if step.Reference != nil {
		referenceScope := tenancy.Scope{TenantID: id.TenantID(step.Reference.TenantID), ProjectID: id.ProjectID(step.Reference.ProjectID)}
		_, err := runner.executor.Read(runner.ctx, referenceScope, step.Reference.Table, step.Reference.ID)
		if err != nil || referenceScope != runner.scope {
			runner.open = false
			return vectorError{"cross_scope"}
		}
	}
	if step.Table == "events" {
		// Event rows are committed publication intent: they travel in
		// Unit.Events, never as a business write (store.ValidateWrite).
		runner.events = append(runner.events, event.Envelope{
			ID:             id.EventID(step.ID),
			Type:           event.Type(step.Values["type"]),
			Source:         event.Source{Kind: event.SourceShell, PrincipalID: "principal-1"},
			At:             pgtest.FixtureTime,
			Scope:          runner.scope,
			SubjectID:      step.Values["subject_id"],
			SubjectVersion: 1,
		})
		return nil
	}
	write := store.Write{Table: step.Table, ID: step.ID, Values: vectorValues(step.Values)}
	if step.Table == "tasks" {
		if step.ExpectVersion != nil {
			write.Expect = map[string]any{"lifecycle_version": int64(*step.ExpectVersion)}
		}
	} else if step.Table == "executions" {
		if step.ExpectVersion != nil {
			write.ExpectVersion = *step.ExpectVersion
		}
	}
	if step.ExpectVersion == nil {
		write.Insert = true
		write.Values = vectorInsertValues(step.Table, step.Values)
	}
	runner.writes = append(runner.writes, write)
	if step.WantError != "" {
		err := runner.executePending()
		runner.open = false
		runner.writes = nil
		runner.events = nil
		return err
	}
	return nil
}

func vectorValues(values map[string]string) map[string]any {
	out := map[string]any{}
	for key, value := range values {
		out[key] = value
	}
	return out
}

func vectorInsertValues(table string, values map[string]string) map[string]any {
	out := map[string]any{}
	for key, value := range values {
		out[key] = value
	}
	switch table {
	case "tasks":
		out["content_version"] = int64(1)
		out["lifecycle_version"] = int64(1)
		out["initiator_id"] = "principal-1"
		out["title"] = "vector task"
		out["created_at"] = pgtest.FixtureTime
	case "executions":
		out["principal_id"] = "principal-1"
		out["version"] = int64(1)
		out["yaml_sha256"] = []byte{0}
		out["snapshot"] = []byte(`{}`)
		out["created_at"] = pgtest.FixtureTime
	case "events":
		out["source_kind"] = "shell"
		out["source_principal"] = "principal-1"
		out["at"] = pgtest.FixtureTime
		out["publish_state"] = "pending"
		out["subject_version"] = int64(1)
	}
	return out
}

func (runner *vectorRunner) receiptStep(step vectorStep) error {
	if !runner.open {
		return vectorError{"no_transaction"}
	}
	if step.Key == nil {
		return vectorError{"invalid_scope"}
	}
	record := receipt.Record{
		ID: id.ReceiptID("receipt-" + step.Key.CallerKey),
		Key: receipt.Key{
			TenantID: id.TenantID(step.Key.TenantID), ProjectID: id.ProjectID(step.Key.ProjectID),
			PrincipalID: id.PrincipalID(step.Key.PrincipalID), Operation: step.Key.Operation, CallerKey: step.Key.CallerKey,
		},
		ContentHash: pgtest.Fingerprint(step.Content),
		Result:      []byte(step.Result),
		At:          pgtest.FixtureTime,
	}
	runner.receipt = &record
	if step.WantError == "" && step.WantResult == "" {
		return nil
	}
	result, err := runner.executor.Execute(runner.ctx, runner.scope, store.Unit{Receipt: runner.receipt})
	runner.receipt = nil
	runner.executed = err == nil
	if err != nil {
		runner.open = false
		return err
	}
	if step.WantResult != "" && string(result.Receipt.Result) != step.WantResult {
		return fmt.Errorf("receipt result = %q; want %q", result.Receipt.Result, step.WantResult)
	}
	return nil
}

func (runner *vectorRunner) executePending() error {
	unit := store.Unit{Writes: runner.writes, Events: runner.events, Receipt: runner.receipt}
	if len(unit.Writes) == 0 && len(unit.Events) == 0 && unit.Receipt == nil {
		return nil
	}
	_, err := runner.executor.Execute(runner.ctx, runner.scope, unit)
	return err
}

func (runner *vectorRunner) inspect(step vectorStep) error {
	scope := runner.scope
	if step.Scope != nil {
		scope = scopeFromVector(step.Scope)
	}
	row, err := runner.executor.Read(runner.ctx, scope, step.Table, step.ID)
	if err != nil {
		if step.WantAbsent {
			return nil
		}
		return err
	}
	if step.WantAbsent {
		return fmt.Errorf("row %s/%s is present; want absent", step.Table, step.ID)
	}
	return checkVectorRow(step, row)
}

func checkVectorRow(step vectorStep, row store.Row) error {
	if step.WantVersion != nil {
		version := rowVersion(row)
		if version != *step.WantVersion {
			return fmt.Errorf("row %s/%s version = %d; want %d", step.Table, step.ID, version, *step.WantVersion)
		}
	}
	for key, want := range step.Want {
		got := rowStringValue(row, key)
		if got != want {
			return fmt.Errorf("row %s/%s value %q = %q; want %q", step.Table, step.ID, key, got, want)
		}
	}
	return nil
}

func rowVersion(row store.Row) uint64 {
	if value, ok := row.Values["version"].(int64); ok {
		return uint64(value)
	}
	if value, ok := row.Values["lifecycle_version"].(int64); ok {
		return uint64(value)
	}
	return 0
}

func rowStringValue(row store.Row, key string) string {
	switch value := row.Values[key].(type) {
	case string:
		return value
	case []byte:
		return string(value)
	default:
		return fmt.Sprint(value)
	}
}

func (runner *vectorRunner) inspectReceipt(step vectorStep) error {
	if step.Key == nil {
		return vectorError{"invalid_scope"}
	}
	// Read through the product receipt-lookup adapter, not a test-owned SELECT:
	// the vector must exercise the same scoped read path production uses.
	lookup, err := readers.NewReceiptLookup(runner.executor)
	if err != nil {
		return err
	}
	key := receipt.Key{
		TenantID: id.TenantID(step.Key.TenantID), ProjectID: id.ProjectID(step.Key.ProjectID),
		PrincipalID: id.PrincipalID(step.Key.PrincipalID), Operation: step.Key.Operation, CallerKey: step.Key.CallerKey,
	}
	record, found, err := lookup.Lookup(runner.ctx, key)
	if err != nil {
		return err
	}
	if !found {
		if step.WantAbsent {
			return nil
		}
		return fmt.Errorf("receipt is absent; want present")
	}
	if step.WantContent != "" && record.ContentHash != pgtest.Fingerprint(step.WantContent) {
		return fmt.Errorf("receipt content hash = %q; want %q", record.ContentHash, pgtest.Fingerprint(step.WantContent))
	}
	if step.WantResult != "" && string(record.Result) != step.WantResult {
		return fmt.Errorf("receipt result = %q; want %q", record.Result, step.WantResult)
	}
	return nil
}

func scopeFromVector(scope *vectorScope) tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID(scope.TenantID), ProjectID: id.ProjectID(scope.ProjectID)}
}

type vectorError struct{ class string }

func (err vectorError) Error() string { return err.class }

func vectorErrorClass(err error) string {
	if err == nil {
		return "none"
	}
	if classed, ok := errors.AsType[vectorError](err); ok {
		return classed.class
	}
	if class, ok := fault.ClassOf(err); ok {
		switch class {
		case fault.VersionConflict:
			return "version_conflict"
		case fault.ChangedIdempotencyContent:
			return "changed_idempotency_content"
		case fault.Forbidden:
			return "cross_scope"
		case fault.NotFound:
			return "not_found"
		}
	}
	if strings.Contains(err.Error(), "not active") {
		return "no_transaction"
	}
	return "unclassified"
}
