package adaptercontract

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/optimiware/kodama/internal/shell/store"
)

// TestSchemaRequiredTables pins the relational record families S04 requires.
// It checks the schema design, not the migration files.
func TestSchemaRequiredTables(t *testing.T) {
	schema := store.SchemaDesign()
	if schema.RuntimeRole == "" || schema.OwnerRole == "" {
		t.Fatalf("schema roles are empty: runtime=%q owner=%q", schema.RuntimeRole, schema.OwnerRole)
	}
	if schema.RuntimeRole == schema.OwnerRole {
		t.Fatalf("runtime and owner roles must differ, both are %q", schema.RuntimeRole)
	}
	required := []string{
		"tenants",
		"projects",
		"memberships",
		"services",
		"resource_bindings",
		"pipelines",
		"executions",
		"actions",
		"tasks",
		"task_messages",
		"events",
		"audit",
		"receipts",
		"verifications",
		"secrets",
		"resources",
		"cleanup",
	}
	for _, name := range required {
		if _, ok := findTable(schema, name); !ok {
			t.Errorf("schema is missing required table %q", name)
		}
	}
}

// TestSchemaCoversAtomicCommitUnits proves every atomic commit unit has the
// durable record families its transaction needs. The store seam itself stays a
// single scoped transaction; the capability port supplies the complete unit.
func TestSchemaCoversAtomicCommitUnits(t *testing.T) {
	schema := store.SchemaDesign()
	for _, unit := range atomicCommitUnits() {
		for _, name := range unit.tables {
			if _, ok := findTable(schema, name); !ok {
				t.Errorf("commit unit %q needs table %q", unit.name, name)
			}
		}
	}
}

// TestSchemaCompositeOwnership proves a child row cannot attach an id from a
// different tenant/project: every table with tenant scope carries a foreign key
// that includes the tenant (and project when present) columns.
func TestSchemaCompositeOwnership(t *testing.T) {
	schema := store.SchemaDesign()
	for _, table := range schema.Tables {
		if !hasColumn(table, "tenant_id") {
			continue
		}
		if hasColumn(table, "project_id") {
			if !hasForeignKeyWithColumns(table, "tenant_id", "project_id") {
				t.Errorf("table %q has no composite tenant/project foreign key", table.Name)
			}
			continue
		}
		if !hasForeignKeyWithColumns(table, "tenant_id") {
			t.Errorf("table %q has no tenant foreign key", table.Name)
		}
	}
}

// TestSchemaUniqueness pins the identities that must be unique, including the
// nullable-project cases where NULLS DISTINCT would permit duplicates.
func TestSchemaUniqueness(t *testing.T) {
	schema := store.SchemaDesign()
	cases := []struct {
		table           string
		columns         []string
		nullableProject bool
	}{
		{"projects", []string{"tenant_id", "name"}, false},
		{"projects", []string{"tenant_id", "id"}, false},
		{"memberships", []string{"tenant_id", "project_id", "principal_id"}, true},
		{"services", []string{"tenant_id", "project_id", "name"}, true},
		{"services", []string{"tenant_id", "id"}, false},
		{"pipelines", []string{"tenant_id", "project_id", "name"}, false},
		{"actions", []string{"execution_id", "index"}, false},
		{"actions", []string{"execution_id", "name"}, false},
		{"receipts", []string{"tenant_id", "project_id", "principal_id", "operation", "caller_key"}, true},
		{"secrets", []string{"tenant_id", "project_id", "name"}, true},
		{"resource_bindings", []string{"tenant_id", "project_id", "resource_id", "service_id"}, false},
	}
	for _, tc := range cases {
		table, ok := findTable(schema, tc.table)
		if !ok {
			t.Errorf("table %q is missing", tc.table)
			continue
		}
		constraint, ok := uniqueWithColumns(table, tc.columns)
		if !ok {
			t.Errorf("table %q has no unique constraint on %v", tc.table, tc.columns)
			continue
		}
		if tc.nullableProject && !constraint.NullsNotDistinct {
			t.Errorf("table %q unique constraint %q must use NULLS NOT DISTINCT for nullable project scope", tc.table, constraint.Name)
		}
	}
}

// TestSchemaNullableTenantScope pins which tables may be tenant-wide and which
// are always project-scoped. Tenant-wide rows use an empty project id.
func TestSchemaNullableTenantScope(t *testing.T) {
	schema := store.SchemaDesign()
	nullable := []string{"memberships", "services", "secrets", "receipts", "events", "audit"}
	for _, name := range nullable {
		table, ok := findTable(schema, name)
		if !ok {
			t.Errorf("table %q is missing", name)
			continue
		}
		column, ok := findColumn(table, "project_id")
		if !ok {
			t.Errorf("table %q has no project_id column", name)
			continue
		}
		if !column.Nullable {
			t.Errorf("table %q project_id must be nullable for tenant-wide rows", name)
		}
	}
	required := []string{
		"resource_bindings",
		"pipelines",
		"executions",
		"actions",
		"tasks",
		"task_messages",
		"verifications",
		"resources",
		"cleanup",
	}
	for _, name := range required {
		table, ok := findTable(schema, name)
		if !ok {
			t.Errorf("table %q is missing", name)
			continue
		}
		column, ok := findColumn(table, "project_id")
		if !ok {
			t.Errorf("table %q has no project_id column", name)
			continue
		}
		if column.Nullable {
			t.Errorf("table %q project_id must be required", name)
		}
	}
}

// TestSchemaStateAndCredentialConstraints pins the state/kind vocabulary,
// one-active-action serialization and nonce/expiry pair consistency.
func TestSchemaStateAndCredentialConstraints(t *testing.T) {
	schema := store.SchemaDesign()
	checks := []struct {
		table  string
		tokens []string
	}{
		{"executions", []string{"queued", "running", "succeeded", "failed", "cancelled"}},
		{"actions", []string{"pending", "running", "succeeded", "failed", "cancelled"}},
		{"actions", []string{"container", "request"}},
		{"tasks", []string{"open", "waiting", "done", "cancelled"}},
		{"executions", []string{"token_nonce", "token_expires_at", "null"}},
		{"services", []string{"token_nonce", "token_expires_at", "null"}},
		{"executions", []string{"token_nonce", "32"}},
		{"services", []string{"token_nonce", "32"}},
	}
	for _, tc := range checks {
		table, ok := findTable(schema, tc.table)
		if !ok {
			t.Errorf("table %q is missing", tc.table)
			continue
		}
		if !hasCheckWithTokens(table, tc.tokens...) {
			t.Errorf("table %q has no check constraint containing %v", tc.table, tc.tokens)
		}
	}

	actions, ok := findTable(schema, "actions")
	if !ok {
		t.Fatal("actions table is missing")
	}
	foundActive := false
	for _, index := range actions.Indexes {
		if index.Unique && sameStringSet(index.Columns, []string{"execution_id"}) && containsAll(index.Predicate, []string{"running"}) {
			foundActive = true
		}
	}
	if !foundActive {
		t.Error("actions has no partial unique index enforcing at most one running action per execution")
	}
}

// TestSchemaForcedRLS proves every scoped table is under forced row-level
// security and that the runtime role is not the owner.
func TestSchemaForcedRLS(t *testing.T) {
	schema := store.SchemaDesign()
	if schema.RuntimeRole == schema.OwnerRole {
		t.Fatalf("runtime role %q must not be the owner role", schema.RuntimeRole)
	}
	for _, table := range schema.Tables {
		if table.Name != "tenants" && !hasColumn(table, "tenant_id") {
			continue
		}
		if !table.RowLevelSecurity {
			t.Errorf("table %q does not enable row-level security", table.Name)
		}
		if !table.ForceRowLevelSecurity {
			t.Errorf("table %q does not force row-level security", table.Name)
		}
	}
}

// TestSchemaLockOrder pins the one lock order shared by task/execution/action/
// authority/receipt commits so two adapters cannot deadlock by acquiring the
// same rows in opposite order.
func TestSchemaLockOrder(t *testing.T) {
	schema := store.SchemaDesign()
	want := []string{
		"tenants",
		"projects",
		"memberships",
		"services",
		"resource_bindings",
		"pipelines",
		"tasks",
		"task_messages",
		"executions",
		"actions",
		"cleanup",
		"verifications",
		"receipts",
		"audit",
		"events",
	}
	if len(schema.LockOrder) != len(want) {
		t.Fatalf("lock order = %v; want %v", schema.LockOrder, want)
	}
	for index, name := range want {
		if schema.LockOrder[index] != name {
			t.Fatalf("lock order = %v; want %v", schema.LockOrder, want)
		}
		if _, ok := findTable(schema, name); !ok {
			t.Errorf("lock order names missing table %q", name)
		}
	}
}

// TestSchemaMigrationNaming pins sequential migration names: four
// zero-padded digits, an underscore and a lowercase slug.
func TestSchemaMigrationNaming(t *testing.T) {
	schema := store.SchemaDesign()
	const want = `^\d{4}_[a-z0-9_]+\.sql$`
	if schema.MigrationNamePattern != want {
		t.Fatalf("migration name pattern = %q; want %q", schema.MigrationNamePattern, want)
	}
	if schema.MigrationSequenceStart != 1 {
		t.Fatalf("migration sequence start = %d; want 1", schema.MigrationSequenceStart)
	}
	pattern, err := regexp.Compile(schema.MigrationNamePattern)
	if err != nil {
		t.Fatalf("migration pattern does not compile: %v", err)
	}
	valid := []string{"0001_initial.sql", "0002_add_receipts.sql", "0042_resource_bindings.sql"}
	for _, name := range valid {
		if !pattern.MatchString(name) {
			t.Errorf("migration pattern refused valid name %q", name)
		}
	}
	invalid := []string{"1_initial.sql", "0001_Initial.sql", "0001-initial.sql", "0001_initial.SQL", "0001_initial"}
	for _, name := range invalid {
		if pattern.MatchString(name) {
			t.Errorf("migration pattern accepted invalid name %q", name)
		}
	}
}

// TestSchemaIndexes pins the queryable ownership/state indexes required by the
// scoped reads and conflict checks.
func TestSchemaIndexes(t *testing.T) {
	schema := store.SchemaDesign()
	cases := []struct {
		table   string
		columns []string
	}{
		{"actions", []string{"execution_id", "state"}},
		{"receipts", []string{"tenant_id", "project_id", "principal_id", "operation", "caller_key"}},
		{"events", []string{"tenant_id", "project_id", "at"}},
		{"tasks", []string{"tenant_id", "project_id", "state"}},
		{"executions", []string{"tenant_id", "project_id", "state"}},
		{"secrets", []string{"tenant_id", "project_id", "name"}},
		{"resource_bindings", []string{"tenant_id", "project_id", "service_id"}},
		{"model_usage", []string{"tenant_id", "project_id", "recorded_at", "id"}},
		{"content_records", []string{"tenant_id", "project_id", "recorded_at", "id"}},
		{"event_consumptions", []string{"tenant_id", "project_id", "consumer"}},
	}
	for _, tc := range cases {
		table, ok := findTable(schema, tc.table)
		if !ok {
			t.Errorf("table %q is missing", tc.table)
			continue
		}
		if !hasIndexOrUnique(table, tc.columns) {
			t.Errorf("table %q has no index or unique constraint on %v", tc.table, tc.columns)
		}
	}
}

// TestSchemaBindingResourceLink proves a resource binding persists the stable
// resource identity Binding.ResourceID requires and cannot attach a
// resource from another project. The composite foreign key and the
// (resource_id, service_id) unique identity back verify.BindingQuery and
// binding lookups.
func TestSchemaBindingResourceLink(t *testing.T) {
	schema := store.SchemaDesign()
	bindings, ok := findTable(schema, "resource_bindings")
	if !ok {
		t.Fatal("resource_bindings table is missing")
	}
	resourceID, ok := findColumn(bindings, "resource_id")
	if !ok {
		t.Fatal("resource_bindings has no resource_id column")
	}
	if resourceID.Nullable {
		t.Error("resource_bindings.resource_id must be required")
	}

	wantColumns := []string{"tenant_id", "project_id", "resource_id"}
	wantReferenced := []string{"tenant_id", "project_id", "id"}
	found := false
	for _, key := range bindings.ForeignKeys {
		if key.References == "resources" &&
			exactColumnSet(key.Columns, wantColumns) &&
			exactColumnSet(key.ReferencedColumns, wantReferenced) {
			found = true
		}
	}
	if !found {
		t.Errorf("resource_bindings has no composite foreign key %v -> resources%v", wantColumns, wantReferenced)
	}

	identity := []string{"tenant_id", "project_id", "resource_id", "service_id"}
	if !hasExactUnique(bindings, identity) {
		t.Errorf("resource_bindings has no unique identity on %v for verify.BindingQuery", identity)
	}
}

// TestSchemaForeignKeyIntegrity proves the declared relational shape is
// structurally usable: every primary key and constraint names existing
// columns, every foreign key's referenced table/columns exist with matching
// arity, and a primary key or unique constraint covers exactly the referenced
// column set. A foreign key to an unkeyed column set could attach a duplicate
// or cross-scope identity, so the declaration itself is refused.
func TestSchemaForeignKeyIntegrity(t *testing.T) {
	schema := store.SchemaDesign()
	tables := make(map[string]store.Table, len(schema.Tables))
	names := make(map[string]bool, len(schema.Tables))
	for _, table := range schema.Tables {
		if table.Name == "" {
			t.Error("schema declares an unnamed table")
			continue
		}
		if names[table.Name] {
			t.Errorf("schema declares table %q twice", table.Name)
		}
		names[table.Name] = true
		tables[table.Name] = table
		if len(table.PrimaryKey) == 0 {
			t.Errorf("table %q has no primary key", table.Name)
		}
		if err := checkColumnSet(table, table.PrimaryKey); err != nil {
			t.Errorf("table %q primary key: %v", table.Name, err)
		}
		for _, unique := range table.Unique {
			if err := checkColumnSet(table, unique.Columns); err != nil {
				t.Errorf("table %q unique %q: %v", table.Name, unique.Name, err)
			}
		}
		for _, index := range table.Indexes {
			if err := checkColumnSet(table, index.Columns); err != nil {
				t.Errorf("table %q index %q: %v", table.Name, index.Name, err)
			}
		}
	}
	for _, table := range schema.Tables {
		for _, key := range table.ForeignKeys {
			target, ok := tables[key.References]
			if !ok {
				t.Errorf("table %q foreign key %q references missing table %q", table.Name, key.Name, key.References)
				continue
			}
			if len(key.Columns) != len(key.ReferencedColumns) {
				t.Errorf("table %q foreign key %q has %d local columns and %d referenced columns", table.Name, key.Name, len(key.Columns), len(key.ReferencedColumns))
				continue
			}
			if err := checkColumnSet(table, key.Columns); err != nil {
				t.Errorf("table %q foreign key %q local columns: %v", table.Name, key.Name, err)
			}
			if err := checkColumnSet(target, key.ReferencedColumns); err != nil {
				t.Errorf("table %q foreign key %q referenced columns: %v", table.Name, key.Name, err)
			}
			if !exactColumnSet(target.PrimaryKey, key.ReferencedColumns) && !hasExactUnique(target, key.ReferencedColumns) {
				t.Errorf("table %q foreign key %q references %s%v with no primary key or unique constraint covering exactly that set", table.Name, key.Name, key.References, key.ReferencedColumns)
			}
		}
	}
}

// TestSchemaCommitUnitTablesAreLocked proves the single lock order names every
// table an atomic commit unit writes, so two adapters cannot deadlock by
// taking a commit-unit row outside the frozen order.
func TestSchemaCommitUnitTablesAreLocked(t *testing.T) {
	schema := store.SchemaDesign()
	locked := make(map[string]bool, len(schema.LockOrder))
	seen := make(map[string]bool, len(schema.LockOrder))
	for _, name := range schema.LockOrder {
		if seen[name] {
			t.Errorf("lock order names table %q twice", name)
		}
		seen[name] = true
		if _, ok := findTable(schema, name); !ok {
			t.Errorf("lock order names missing table %q", name)
		}
		locked[name] = true
	}
	for _, unit := range atomicCommitUnits() {
		for _, table := range unit.tables {
			if !locked[table] {
				t.Errorf("commit unit %q writes table %q outside the single lock order", unit.name, table)
			}
		}
	}
}

// atomicCommitUnits mirrors the atomic commit unit table. It is kept here as
// test data so the lock-order proof fails if a unit's written table is dropped.
func atomicCommitUnits() []struct {
	name   string
	tables []string
} {
	return []struct {
		name   string
		tables []string
	}{
		{"execution transition", []string{"executions", "actions", "receipts", "audit", "events"}},
		{"request activation", []string{"executions", "actions", "receipts", "audit", "events"}},
		{"request resolve", []string{"executions", "actions", "receipts", "audit", "events"}},
		{"task start/admission", []string{"tasks", "executions", "actions", "receipts", "audit", "events"}},
		{"task ask", []string{"tasks", "task_messages", "executions", "actions", "receipts", "audit", "events"}},
		{"task reply", []string{"tasks", "task_messages", "executions", "actions", "receipts", "audit", "events"}},
		{"task cancel", []string{"tasks", "executions", "actions", "receipts", "audit", "events"}},
		{"token mutation", []string{"services", "executions", "receipts", "audit", "events"}},
		{"binding/config change", []string{"resource_bindings", "receipts", "audit", "events"}},
		{"verified report", []string{"verifications", "receipts", "audit", "events"}},
		{"push selection/result", []string{"pipelines", "resource_bindings", "receipts", "audit", "events"}},
	}
}

func checkColumnSet(table store.Table, columns []string) error {
	if len(columns) == 0 {
		return fmt.Errorf("empty column set")
	}
	seen := make(map[string]bool, len(columns))
	for _, name := range columns {
		if seen[name] {
			return fmt.Errorf("column %q is repeated", name)
		}
		seen[name] = true
		if !hasColumn(table, name) {
			return fmt.Errorf("column %q does not exist", name)
		}
	}
	return nil
}

func hasExactUnique(table store.Table, columns []string) bool {
	for _, constraint := range table.Unique {
		if exactColumnSet(constraint.Columns, columns) {
			return true
		}
	}
	return false
}

func exactColumnSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	want := make(map[string]bool, len(right))
	for _, name := range right {
		want[name] = true
	}
	for _, name := range left {
		if !want[name] {
			return false
		}
	}
	return true
}

func findTable(schema store.Schema, name string) (store.Table, bool) {
	for _, table := range schema.Tables {
		if table.Name == name {
			return table, true
		}
	}
	return store.Table{}, false
}

func findColumn(table store.Table, name string) (store.Column, bool) {
	for _, column := range table.Columns {
		if column.Name == name {
			return column, true
		}
	}
	return store.Column{}, false
}

func hasColumn(table store.Table, name string) bool {
	_, ok := findColumn(table, name)
	return ok
}

func hasForeignKeyWithColumns(table store.Table, columns ...string) bool {
	for _, key := range table.ForeignKeys {
		if containsAllStrings(key.Columns, columns) {
			return true
		}
	}
	return false
}

func uniqueWithColumns(table store.Table, columns []string) (store.UniqueConstraint, bool) {
	for _, constraint := range table.Unique {
		if sameStringSet(constraint.Columns, columns) {
			return constraint, true
		}
	}
	return store.UniqueConstraint{}, false
}

func hasIndexOrUnique(table store.Table, columns []string) bool {
	if _, ok := uniqueWithColumns(table, columns); ok {
		return true
	}
	for _, index := range table.Indexes {
		if sameStringSet(index.Columns, columns) {
			return true
		}
	}
	return false
}

func hasCheckWithTokens(table store.Table, tokens ...string) bool {
	for _, check := range table.Checks {
		if containsAll(check.Expression, tokens) {
			return true
		}
	}
	return false
}

func containsAllStrings(values, wanted []string) bool {
	for _, want := range wanted {
		if !hasString(values, want) {
			return false
		}
	}
	return true
}
