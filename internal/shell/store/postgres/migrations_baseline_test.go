package postgres

import (
	"regexp"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/shell/store"
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
