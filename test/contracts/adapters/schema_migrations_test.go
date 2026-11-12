package adaptercontract

// Static checks on the migration set against the schema declaration: constraint
// order, the foreign-key set, the evidence columns that deliberately carry no
// reference, and the system-role policies and controller privileges.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
)

var (
	uniqueConstraintPattern = regexp.MustCompile(`(?is)^alter\s+table\s+([a-z0-9_]+)\s+add\s+constraint\s+([a-z0-9_]+)\s+unique(?:\s+nulls\s+not\s+distinct)?\s*\(([^)]*)\)$`)
	foreignKeyPattern       = regexp.MustCompile(`(?is)^alter\s+table\s+([a-z0-9_]+)\s+add\s+constraint\s+([a-z0-9_]+)\s+foreign\s+key\s*\(([^)]*)\)\s+references\s+([a-z0-9_]+)\s*\(([^)]*)\)$`)
	checkConstraintPattern  = regexp.MustCompile(`(?is)^alter\s+table\s+[a-z0-9_]+\s+add\s+constraint\s+[a-z0-9_]+\s+check\s*\(.*\)$`)
	systemPolicyPattern     = regexp.MustCompile(`(?is)^create\s+policy\s+[a-z0-9_]+\s+on\s+([a-z0-9_]+)\s+as\s+permissive\s+for\s+([a-z]+)\s+to\s+([a-z0-9_]+)\b`)
)

type migrationUnique struct {
	index   int
	table   string
	name    string
	columns []string
}

type migrationForeignKey struct {
	index      int
	table      string
	name       string
	columns    []string
	refTable   string
	refColumns []string
}

// expectedForeignKeys is the complete foreign-key set of the schema.
var expectedForeignKeys = []string{
	"actions_execution",
	"actions_project",
	"actions_tenant",
	"audit_project",
	"audit_tenant",
	"cleanup_execution",
	"cleanup_project",
	"cleanup_tenant",
	"content_records_execution",
	"content_records_project",
	"content_records_tenant",
	"event_consumptions_project",
	"event_consumptions_tenant",
	"events_project",
	"events_tenant",
	"executions_project",
	"executions_task",
	"executions_tenant",
	"memberships_project",
	"memberships_tenant",
	"model_usage_execution",
	"model_usage_project",
	"model_usage_tenant",
	"pipelines_project",
	"pipelines_tenant",
	"projects_tenant",
	"receipts_project",
	"receipts_tenant",
	"resource_bindings_project",
	"resource_bindings_resource",
	"resource_bindings_service",
	"resource_bindings_tenant",
	"resources_project",
	"resources_tenant",
	"secrets_project",
	"secrets_tenant",
	"services_project",
	"services_tenant",
	"task_messages_project",
	"task_messages_task",
	"task_messages_tenant",
	"tasks_project",
	"tasks_tenant",
	"verifications_action",
	"verifications_execution",
	"verifications_project",
	"verifications_resource",
	"verifications_service",
	"verifications_tenant",
}

// migrationForeignKeys returns every foreign key the migration set creates.
func migrationForeignKeys(t *testing.T) map[string]migrationForeignKey {
	t.Helper()
	keys := make(map[string]migrationForeignKey)
	for _, migration := range storepg.Migrations() {
		for _, statement := range splitMigrationStatements(stripMigrationComments(migration.SQL)) {
			match := foreignKeyPattern.FindStringSubmatch(statement)
			if match == nil {
				continue
			}
			name := strings.ToLower(match[2])
			if _, duplicate := keys[name]; duplicate {
				t.Errorf("foreign key %q is created twice", name)
			}
			keys[name] = migrationForeignKey{
				table:      strings.ToLower(match[1]),
				name:       name,
				columns:    splitColumnList(match[3]),
				refTable:   strings.ToLower(match[4]),
				refColumns: splitColumnList(match[5]),
			}
		}
	}
	return keys
}

// TestMigrationsCreateExactlyTheDeclaredForeignKeys proves the migration set,
// the declaration and the expected list name the same references with the same
// columns.
func TestMigrationsCreateExactlyTheDeclaredForeignKeys(t *testing.T) {
	created := migrationForeignKeys(t)
	got := make([]string, 0, len(created))
	for name := range created {
		got = append(got, name)
	}
	sort.Strings(got)
	want := append([]string(nil), expectedForeignKeys...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("foreign key set = %v; want exactly %v", got, want)
	}

	declared := 0
	for _, table := range store.SchemaDesign().Tables {
		for _, key := range table.ForeignKeys {
			declared++
			rendered, ok := created[key.Name]
			if !ok {
				t.Errorf("declared foreign key %q is missing from the migrations", key.Name)
				continue
			}
			if rendered.table != table.Name || strings.Join(rendered.columns, ",") != strings.Join(key.Columns, ",") ||
				rendered.refTable != key.References || strings.Join(rendered.refColumns, ",") != strings.Join(key.ReferencedColumns, ",") {
				t.Errorf("foreign key %q = %s%v -> %s%v; the declaration has %s%v -> %s%v", key.Name,
					rendered.table, rendered.columns, rendered.refTable, rendered.refColumns,
					table.Name, key.Columns, key.References, key.ReferencedColumns)
			}
		}
	}
	if declared != len(created) {
		t.Errorf("the declaration carries %d foreign keys; the migrations create %d", declared, len(created))
	}
}

// TestHistoryRowsKeepEvidenceWithoutAReference pins three deliberate absences.
// A verification keeps its binding identity and an execution or task keeps its
// pipeline identity as evidence, but none of them references the parent row:
// a binding or a pipeline with history can be deleted without destroying that
// history.
func TestHistoryRowsKeepEvidenceWithoutAReference(t *testing.T) {
	for name, key := range migrationForeignKeys(t) {
		if key.refTable == "pipelines" {
			t.Errorf("foreign key %q references pipelines; history rows must not hold a pipeline", name)
		}
		if key.table == "verifications" && key.refTable == "resource_bindings" {
			t.Errorf("foreign key %q references resource_bindings; a verification must not hold a binding", name)
		}
	}
	evidence := map[string]struct {
		column   string
		nullable bool
	}{
		"verifications": {column: "binding_id", nullable: false},
		"executions":    {column: "pipeline_id", nullable: true},
		"tasks":         {column: "pipeline_id", nullable: true},
	}
	found := 0
	for _, table := range store.SchemaDesign().Tables {
		want, tracked := evidence[table.Name]
		if !tracked {
			continue
		}
		for _, column := range table.Columns {
			if column.Name != want.column {
				continue
			}
			found++
			if column.Type != "text" || column.Nullable != want.nullable {
				t.Errorf("%s.%s = %s nullable=%t; want text nullable=%t", table.Name, column.Name, column.Type, column.Nullable, want.nullable)
			}
		}
	}
	if found != len(evidence) {
		t.Errorf("the declaration carries %d of the %d evidence columns", found, len(evidence))
	}
}

// TestConstraintsCreateReferencedUniquesBeforeForeignKeys pins the constraint
// order: all uniques, then all foreign keys, then all checks. PostgreSQL
// refuses a foreign key whose referenced unique constraint does not exist yet
// (SQLSTATE 42830).
func TestConstraintsCreateReferencedUniquesBeforeForeignKeys(t *testing.T) {
	statements := splitMigrationStatements(stripMigrationComments(migrationNamed(t, "0003_constraints.sql").SQL))
	if len(statements) == 0 {
		t.Fatal("0003_constraints.sql is empty")
	}
	var uniques []migrationUnique
	var foreignKeys []migrationForeignKey
	var checkIndexes []int
	for index, statement := range statements {
		switch {
		case uniqueConstraintPattern.MatchString(statement):
			match := uniqueConstraintPattern.FindStringSubmatch(statement)
			uniques = append(uniques, migrationUnique{index: index, table: strings.ToLower(match[1]), name: strings.ToLower(match[2]), columns: splitColumnList(match[3])})
		case foreignKeyPattern.MatchString(statement):
			match := foreignKeyPattern.FindStringSubmatch(statement)
			foreignKeys = append(foreignKeys, migrationForeignKey{
				index: index, table: strings.ToLower(match[1]), name: strings.ToLower(match[2]),
				columns: splitColumnList(match[3]), refTable: strings.ToLower(match[4]), refColumns: splitColumnList(match[5]),
			})
		case checkConstraintPattern.MatchString(statement):
			checkIndexes = append(checkIndexes, index)
		default:
			t.Fatalf("0003 statement %d is not a recognized constraint: %q", index, statement)
		}
	}
	if len(uniques) == 0 || len(foreignKeys) == 0 {
		t.Fatalf("0003 declares %d UNIQUE constraints and %d foreign keys; want both", len(uniques), len(foreignKeys))
	}
	if lastUnique, firstForeignKey := uniques[len(uniques)-1].index, foreignKeys[0].index; lastUnique > firstForeignKey {
		t.Errorf("0003 adds a foreign key at statement %d before the last UNIQUE at %d", firstForeignKey, lastUnique)
	}
	if len(checkIndexes) > 0 {
		if lastForeignKey, firstCheck := foreignKeys[len(foreignKeys)-1].index, checkIndexes[0]; lastForeignKey > firstCheck {
			t.Errorf("0003 adds a CHECK at statement %d before the last foreign key at %d", firstCheck, lastForeignKey)
		}
	}
	for _, key := range foreignKeys {
		for _, unique := range uniques {
			if unique.table != key.refTable || !sameStringSet(unique.columns, key.refColumns) {
				continue
			}
			if unique.index > key.index {
				t.Errorf("0003 adds foreign key %q at statement %d before the referenced UNIQUE %q at %d", key.name, key.index, unique.name, unique.index)
			}
		}
	}
}

// TestDeclaredSystemPoliciesRenderedExactlyOnce proves every policy in
// SystemPolicies() is created exactly once and the migrations create no
// undeclared system-role policy.
func TestDeclaredSystemPoliciesRenderedExactlyOnce(t *testing.T) {
	rendered := make(map[string]int)
	for _, migration := range storepg.Migrations() {
		for _, statement := range splitMigrationStatements(stripMigrationComments(migration.SQL)) {
			match := systemPolicyPattern.FindStringSubmatch(statement)
			if match == nil {
				continue
			}
			rendered[strings.ToLower(match[3])+"|"+strings.ToLower(match[1])+"|"+strings.ToLower(match[2])]++
		}
	}
	declared := make(map[string]bool, len(store.SystemPolicies()))
	for _, policy := range store.SystemPolicies() {
		key := policy.Role + "|" + policy.Table + "|" + string(policy.Command)
		declared[key] = true
		if got := rendered[key]; got != 1 {
			t.Errorf("declared policy %s is rendered %d times; want exactly once", key, got)
		}
	}
	for key, count := range rendered {
		if !declared[key] {
			t.Errorf("migrations render undeclared system-role policy %s (%d times)", key, count)
		}
	}
}

// TestControllerPrivilegesAreTheDeclaredGrants proves the controller role
// receives exactly its declared table grants plus USAGE on the event
// publication sequence: it may insert events and audit rows, it never updates
// or deletes them, and it has no receipt access.
func TestControllerPrivilegesAreTheDeclaredGrants(t *testing.T) {
	declared := make(map[string]bool)
	for _, grant := range store.ControllerGrants() {
		for _, operation := range grant.Operations {
			declared[grant.Table+"|"+string(operation)] = true
		}
	}
	for _, table := range store.ControllerInsertOnlyTables() {
		if !declared[table+"|insert"] {
			t.Errorf("the declaration has no controller INSERT on %q", table)
		}
		for _, operation := range []string{"update", "delete"} {
			if declared[table+"|"+operation] {
				t.Errorf("the declaration lets the controller %s %q", operation, table)
			}
		}
	}

	rendered := make(map[string]bool)
	var sequences []string
	for _, statement := range splitMigrationStatements(stripMigrationComments(migrationNamed(t, "0006_privileges.sql").SQL)) {
		grant, err := parseMigrationGrant(strings.ToLower(statement))
		if err != nil {
			t.Fatalf("0006 statement cannot be parsed: %v", err)
		}
		if grant.role != store.ControllerRole {
			continue
		}
		for _, privilege := range grant.privileges {
			if grant.objectType == "sequence" {
				if privilege != "usage" {
					t.Errorf("0006 grants the controller %q on a sequence; want USAGE only", privilege)
				}
				sequences = append(sequences, grant.objects...)
				continue
			}
			for _, object := range grant.objects {
				rendered[object+"|"+privilege] = true
			}
		}
	}
	for key := range declared {
		if !rendered[key] {
			t.Errorf("0006 does not grant the declared controller privilege %s", key)
		}
	}
	for key := range rendered {
		if !declared[key] {
			t.Errorf("0006 grants the controller the undeclared privilege %s", key)
		}
		if strings.HasPrefix(key, "receipts|") {
			t.Errorf("0006 gives the controller receipt access: %s", key)
		}
	}
	if len(sequences) != 1 || sequences[0] != "events_publish_sequence_seq" {
		t.Errorf("controller sequence grants = %v; want only events_publish_sequence_seq", sequences)
	}
}

type migrationGrant struct {
	privileges []string
	objectType string
	objects    []string
	role       string
}

// parseMigrationGrant parses one lowercase GRANT statement. A column list
// after a privilege is dropped: the checks here compare privileges per table.
func parseMigrationGrant(statement string) (migrationGrant, error) {
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(statement), "grant "))
	onIndex := strings.Index(rest, " on ")
	toIndex := strings.LastIndex(rest, " to ")
	if onIndex < 0 || toIndex < 0 || toIndex < onIndex {
		return migrationGrant{}, fmt.Errorf("grant statement has no ON/TO clauses: %q", statement)
	}
	privileges := splitGrantList(regexp.MustCompile(`\([^)]*\)`).ReplaceAllString(rest[:onIndex], ""))
	objectPart := strings.TrimSpace(rest[onIndex+4 : toIndex])
	role := strings.TrimSpace(rest[toIndex+4:])
	objectType := "table"
	switch {
	case strings.HasPrefix(objectPart, "table "):
		objectPart = strings.TrimSpace(strings.TrimPrefix(objectPart, "table "))
	case strings.HasPrefix(objectPart, "sequence "):
		objectType = "sequence"
		objectPart = strings.TrimSpace(strings.TrimPrefix(objectPart, "sequence "))
	case strings.HasPrefix(objectPart, "all sequences "):
		objectType = "sequence"
	}
	return migrationGrant{privileges: privileges, objectType: objectType, objects: splitGrantList(objectPart), role: role}, nil
}

func splitGrantList(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func splitColumnList(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		out = append(out, strings.ToLower(strings.TrimSpace(part)))
	}
	return out
}

func migrationNamed(t *testing.T, name string) storepg.Migration {
	t.Helper()
	for _, migration := range storepg.Migrations() {
		if migration.Name == name {
			return migration
		}
	}
	t.Fatalf("migration %q is missing from the set", name)
	return storepg.Migration{}
}

func stripMigrationComments(sql string) string {
	lines := strings.Split(sql, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func splitMigrationStatements(sql string) []string {
	parts := strings.Split(sql, ";")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
