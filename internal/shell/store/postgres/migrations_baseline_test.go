package postgres

import (
	"regexp"
	"strings"
	"testing"

	"github.com/optimiware/kodema/internal/shell/store"
)

// migrationByName returns one migration by exact name.
func migrationByName(t *testing.T, name string) Migration {
	t.Helper()
	for _, migration := range Migrations() {
		if migration.Name == name {
			return migration
		}
	}
	t.Fatalf("migration %q is missing from the set", name)
	return Migration{}
}

// TestBaselineIsSchemaDefinitionOnly proves no baseline migration touches rows
// or removes an object: a fresh database has nothing to backfill or drop.
func TestBaselineIsSchemaDefinitionOnly(t *testing.T) {
	data := regexp.MustCompile(`(?im)^\s*(update|insert\s+into|delete\s+from|drop|truncate)\b`)
	for _, migration := range Migrations() {
		if match := data.FindString(migration.SQL); match != "" {
			t.Errorf("%s contains %q; the baseline only defines the schema", migration.Name, strings.TrimSpace(match))
		}
	}
}

// TestBaselineRendersTheDeclaredReceiptCompletionPair proves the constraints
// migration carries the declared completion rule: an incomplete receipt may
// hold an in-progress result, a completed receipt must hold one.
func TestBaselineRendersTheDeclaredReceiptCompletionPair(t *testing.T) {
	declared := ""
	for _, table := range store.SchemaDesign().Tables {
		if table.Name != "receipts" {
			continue
		}
		for _, check := range table.Checks {
			if check.Name == "receipts_completion_pair" {
				declared = check.Expression
			}
		}
	}
	if declared == "" {
		t.Fatal("the declaration has no receipts_completion_pair check")
	}
	want := "ALTER TABLE receipts ADD CONSTRAINT receipts_completion_pair CHECK (" + declared + ");"
	if !strings.Contains(migrationByName(t, "0003_constraints.sql").SQL, want) {
		t.Errorf("0003 does not render %q", want)
	}
	if normalizeExpression(declared) != normalizeExpression("completed_at IS NULL OR result IS NOT NULL") {
		t.Errorf("declared completion pair = %q; want the relaxed rule", declared)
	}
}

// TestBaselineRendersEveryDeclaredSystemPolicy proves the security migration
// creates each declared system-role policy exactly once, and that the
// controller may insert events and audit rows only inside the
// transaction-local scope.
func TestBaselineRendersEveryDeclaredSystemPolicy(t *testing.T) {
	sql := migrationByName(t, "0005_row_level_security.sql").SQL
	scope := renderPolicyPredicate(store.Policy{}, store.PredicateScopeMatchesTxnScope)
	inserts := map[string]bool{}
	for _, policy := range store.SystemPolicies() {
		statement := renderPolicyStatement(policy)
		if count := strings.Count(sql, statement); count != 1 {
			t.Errorf("0005 renders %q %d times; want once", statement, count)
		}
		if policy.Role == store.ControllerRole && policy.Command == store.PolicyInsert {
			inserts[policy.Table] = true
			if !strings.HasSuffix(statement, "WITH CHECK ("+scope+");") {
				t.Errorf("controller INSERT policy on %q is not confined to the transaction scope: %q", policy.Table, statement)
			}
		}
	}
	for _, table := range store.ControllerInsertOnlyTables() {
		if !inserts[table] {
			t.Errorf("the declaration has no controller INSERT policy on %q", table)
		}
	}
	system := regexp.MustCompile(`(?m)^CREATE POLICY \S+ ON \S+ AS PERMISSIVE`)
	if got, want := len(system.FindAllString(sql, -1)), len(store.SystemPolicies()); got != want {
		t.Errorf("0005 renders %d system-role policies; the declaration has %d", got, want)
	}
}

// TestIncrementalMigration0007PreservesBaseline proves model_usage.action_id
// and model_usage_action are emitted exclusively in incremental migration 0007
// so frozen baseline migrations 0001..0006 keep their exact SQL and checksums.
func TestIncrementalMigration0007PreservesBaseline(t *testing.T) {
	m0002 := migrationByName(t, "0002_tables.sql").SQL
	m0003 := migrationByName(t, "0003_constraints.sql").SQL
	m0007 := migrationByName(t, "0007_model_usage_action_id.sql").SQL

	// Baseline 0002 model_usage CREATE TABLE must not contain action_id.
	block := createBlock(t, normalizeSQL(m0002), "model_usage")
	if strings.Contains(block, "action_id") {
		t.Errorf("0002_tables.sql model_usage block was modified to include action_id: %s", block)
	}
	if strings.Contains(m0003, "model_usage_action") {
		t.Errorf("0003_constraints.sql was modified to include model_usage_action")
	}
	if !strings.Contains(m0007, "ALTER TABLE model_usage ADD COLUMN action_id TEXT;") {
		t.Errorf("0007_model_usage_action_id.sql missing ADD COLUMN action_id: %s", m0007)
	}
	if !strings.Contains(m0007, "ALTER TABLE model_usage ADD CONSTRAINT model_usage_action FOREIGN KEY (tenant_id, project_id, action_id) REFERENCES actions (tenant_id, project_id, id);") {
		t.Errorf("0007_model_usage_action_id.sql missing model_usage_action FK: %s", m0007)
	}
}
