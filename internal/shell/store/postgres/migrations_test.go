// Static migration and schema-shape checks.
//
// These tests never execute SQL against a server. They prove the reviewed
// migration set is deterministic, sequential, complete for the declared
// SchemaDesign, and carries the required ownership keys, uniqueness, indexes,
// forced RLS, non-owner runtime role and event publication cursor. Actual SQL
// validity, RLS enforcement and locking are explicitly not proven here.
package postgres

import (
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/shell/store"
)

func normalizeSQL(sql string) string {
	blockComments := regexp.MustCompile(`(?s)/\*.*?\*/`)
	sql = blockComments.ReplaceAllString(sql, " ")
	lineComments := regexp.MustCompile(`--[^\n]*`)
	sql = lineComments.ReplaceAllString(sql, " ")
	sql = strings.ToLower(sql)
	return strings.Join(strings.Fields(sql), " ")
}

func allMigrationSQL() string {
	parts := make([]string, 0, len(Migrations()))
	for _, migration := range Migrations() {
		parts = append(parts, migration.SQL)
	}
	return normalizeSQL(strings.Join(parts, "\n;\n"))
}

func ident(name string) string {
	return `"?` + regexp.QuoteMeta(name) + `"?`
}

func qualified(name string) string {
	return `(?:[a-z_][a-z0-9_]*\.)?` + ident(name)
}

// createBlockRange returns the offsets of the inside of one CREATE TABLE body.
func createBlockRange(sql, table string) (int, int, bool) {
	pattern := regexp.MustCompile(`create\s+table\s+(?:if\s+not\s+exists\s+)?` + qualified(table) + `\s*\(`)
	location := pattern.FindStringIndex(sql)
	if location == nil {
		return 0, 0, false
	}
	start := location[1]
	depth := 1
	for index := start; index < len(sql); index++ {
		switch sql[index] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return start, index, true
			}
		}
	}
	return 0, 0, false
}

func createBlock(t *testing.T, sql, table string) string {
	t.Helper()
	start, end, ok := createBlockRange(sql, table)
	if !ok {
		t.Fatalf("migrations have no CREATE TABLE for %q", table)
	}
	return sql[start:end]
}

func statementPosition(sql, fragment string) int {
	return strings.Index(sql, normalizeSQL(fragment))
}

func regexPosition(sql string, pattern *regexp.Regexp) int {
	location := pattern.FindStringIndex(sql)
	if location == nil {
		return -1
	}
	return location[0]
}

// addedColumn reports whether a migration adds the column with an ALTER TABLE
// ADD COLUMN instead of declaring it in the CREATE TABLE block.
func addedColumn(sql, table, column string) bool {
	pattern := regexp.MustCompile(
		`alter\s+table\s+` + qualified(table) + `\s+add\s+column\s+` + ident(column) + `(?:[^a-z0-9_]|$)`,
	)
	return pattern.MatchString(sql)
}

func hasSchemaColumn(table store.Table, name string) bool {
	for _, column := range table.Columns {
		if column.Name == name {
			return true
		}
	}
	return false
}

func splitColumns(value string) []string {
	parts := strings.Split(value, ",")
	columns := make([]string, 0, len(parts))
	for _, part := range parts {
		column := strings.TrimSpace(part)
		column = strings.Trim(column, "\"`")
		column = strings.TrimSpace(column)
		if column != "" {
			columns = append(columns, column)
		}
	}
	return columns
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestMigrationsAreSequentialAndDeterministic(t *testing.T) {
	first := Migrations()
	second := Migrations()
	if len(first) == 0 {
		t.Fatal("the adapter declares no migrations")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Migrations() is not deterministic across calls")
	}

	pattern := regexp.MustCompile(store.MigrationNamePattern)
	seen := make(map[string]bool, len(first))
	for index, migration := range first {
		if !pattern.MatchString(migration.Name) {
			t.Errorf("migration name %q does not match the declared pattern %q", migration.Name, store.MigrationNamePattern)
		}
		if seen[migration.Name] {
			t.Errorf("migration name %q is declared twice", migration.Name)
		}
		seen[migration.Name] = true
		number, err := strconv.Atoi(strings.SplitN(migration.Name, "_", 2)[0])
		if err != nil {
			t.Errorf("migration name %q has no numeric prefix", migration.Name)
			continue
		}
		if want := store.MigrationSequenceStart + index; number != want {
			t.Errorf("migration %q is number %d; want sequential number %d", migration.Name, number, want)
		}
		if strings.TrimSpace(migration.SQL) == "" {
			t.Errorf("migration %q has no SQL", migration.Name)
		}
	}
}

func TestMigrationsCreateEverySchemaTable(t *testing.T) {
	sql := allMigrationSQL()
	schema := store.SchemaDesign()
	for _, table := range schema.Tables {
		block := createBlock(t, sql, table.Name)
		if count := strings.Count(sql, "create table"); count < len(schema.Tables) {
			t.Fatalf("migrations declare %d CREATE TABLE statements for %d schema tables", count, len(schema.Tables))
		}
		for _, column := range table.Columns {
			columnPattern := regexp.MustCompile(`(?:^|[^a-z0-9_])` + regexp.QuoteMeta(column.Name) + `(?:[^a-z0-9_]|$)`)
			if !columnPattern.MatchString(block) && !addedColumn(sql, table.Name, column.Name) {
				t.Errorf("table %q is missing column %q", table.Name, column.Name)
			}
		}
		if !regexp.MustCompile(`primary\s+key\s*\(\s*` + strings.Join(quoteAll(table.PrimaryKey), `\s*,\s*`) + `\s*\)`).MatchString(block) {
			t.Errorf("table %q is missing primary key %v", table.Name, table.PrimaryKey)
		}
	}
}

func quoteAll(values []string) []string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = ident(value)
	}
	return quoted
}

func TestMigrationsCreateTablesInDependencyOrder(t *testing.T) {
	sql := allMigrationSQL()
	schema := store.SchemaDesign()
	positions := make(map[string]int, len(schema.Tables))
	for _, table := range schema.Tables {
		start, _, ok := createBlockRange(sql, table.Name)
		if !ok {
			t.Fatalf("migrations have no CREATE TABLE for %q", table.Name)
		}
		positions[table.Name] = start
	}

	for _, table := range schema.Tables {
		start, end, _ := createBlockRange(sql, table.Name)
		for _, key := range table.ForeignKeys {
			pattern := regexp.MustCompile(`constraint\s+` + ident(key.Name) + `\s+foreign\s+key`)
			location := pattern.FindStringIndex(sql)
			if location == nil {
				continue
			}
			if location[0] < start || location[0] > end {
				// The constraint is added after table creation, which is
				// allowed and avoids an ordering problem.
				continue
			}
			target, ok := positions[key.References]
			if !ok {
				t.Fatalf("foreign key %q references unknown table %q", key.Name, key.References)
			}
			if target >= start {
				t.Errorf("table %q is created before its referenced table %q; the inline foreign key %q cannot resolve", table.Name, key.References, key.Name)
			}
		}
	}

	for _, table := range schema.Tables {
		start, _, _ := createBlockRange(sql, table.Name)
		for _, index := range table.Indexes {
			pattern := regexp.MustCompile(`create\s+(?:unique\s+)?index\s+` + ident(index.Name))
			position := regexPosition(sql, pattern)
			if position < 0 {
				continue
			}
			if position < start {
				t.Errorf("index %q is created before table %q", index.Name, table.Name)
			}
		}
		if table.RowLevelSecurity {
			pattern := regexp.MustCompile(`alter\s+table\s+` + qualified(table.Name) + `\s+enable\s+row\s+level\s+security`)
			position := regexPosition(sql, pattern)
			if position >= 0 && position < start {
				t.Errorf("row-level security is enabled before table %q is created", table.Name)
			}
		}
	}
}

func TestMigrationsDeclareForeignKeysAndUniqueness(t *testing.T) {
	sql := allMigrationSQL()
	schema := store.SchemaDesign()
	for _, table := range schema.Tables {
		for _, key := range table.ForeignKeys {
			pattern := regexp.MustCompile(`constraint\s+` + ident(key.Name) + `\s+foreign\s+key\s*\(([^)]*)\)\s+references\s+` + qualified(key.References) + `\s*\(([^)]*)\)`)
			match := pattern.FindStringSubmatch(sql)
			if match == nil {
				t.Errorf("table %q has no rendered foreign key %q referencing %q", table.Name, key.Name, key.References)
				continue
			}
			if got := splitColumns(match[1]); !equalStrings(got, key.Columns) {
				t.Errorf("foreign key %q local columns = %v; want %v", key.Name, got, key.Columns)
			}
			if got := splitColumns(match[2]); !equalStrings(got, key.ReferencedColumns) {
				t.Errorf("foreign key %q referenced columns = %v; want %v", key.Name, got, key.ReferencedColumns)
			}
		}

		for _, unique := range table.Unique {
			pattern := regexp.MustCompile(`constraint\s+` + ident(unique.Name) + `\s+unique(\s+nulls\s+not\s+distinct)?\s*\(([^)]*)\)`)
			match := pattern.FindStringSubmatch(sql)
			if match == nil {
				t.Errorf("table %q has no rendered unique constraint %q", table.Name, unique.Name)
				continue
			}
			hasNullsNotDistinct := strings.TrimSpace(match[1]) != ""
			if hasNullsNotDistinct != unique.NullsNotDistinct {
				t.Errorf("unique constraint %q NULLS NOT DISTINCT = %v; want %v", unique.Name, hasNullsNotDistinct, unique.NullsNotDistinct)
			}
			if got := splitColumns(match[2]); !equalStrings(got, unique.Columns) {
				t.Errorf("unique constraint %q columns = %v; want %v", unique.Name, got, unique.Columns)
			}
		}

		for _, check := range table.Checks {
			expression, ok := renderedCheck(sql, check.Name)
			if !ok {
				t.Errorf("table %q has no rendered check constraint %q", table.Name, check.Name)
				continue
			}
			if normalizeExpression(expression) != normalizeExpression(check.Expression) {
				t.Errorf("check constraint %q = %q; want %q", check.Name, expression, check.Expression)
			}
		}
	}
}

// renderedCheck returns the effective (last) rendered definition of one check.
// A later migration may drop and re-add a constraint, so the first rendering
// is not necessarily the final shape.
func renderedCheck(sql, name string) (string, bool) {
	pattern := regexp.MustCompile(`constraint\s+` + ident(name) + `\s+check\s*\(`)
	locations := pattern.FindAllStringIndex(sql, -1)
	if len(locations) == 0 {
		return "", false
	}
	start := locations[len(locations)-1][1]
	depth := 1
	for index := start; index < len(sql); index++ {
		switch sql[index] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return sql[start:index], true
			}
		}
	}
	return "", false
}

func normalizeExpression(value string) string {
	value = strings.ToLower(value)
	value = strings.ReplaceAll(value, "(", "")
	value = strings.ReplaceAll(value, ")", "")
	return strings.Join(strings.Fields(value), "")
}

func TestMigrationsDeclareIndexes(t *testing.T) {
	sql := allMigrationSQL()
	schema := store.SchemaDesign()
	for _, table := range schema.Tables {
		for _, index := range table.Indexes {
			pattern := regexp.MustCompile(`create\s+(unique\s+)?index\s+` + ident(index.Name) + `\s+on\s+` + qualified(table.Name) + `\s*(?:using\s+[a-z0-9_]+\s*)?\(([^)]*)\)(?:\s+where\s+([^;]+))?`)
			match := pattern.FindStringSubmatch(sql)
			if match == nil {
				t.Errorf("table %q has no rendered index %q", table.Name, index.Name)
				continue
			}
			if got := strings.TrimSpace(match[1]) != ""; got != index.Unique {
				t.Errorf("index %q unique = %v; want %v", index.Name, got, index.Unique)
			}
			if got := splitColumns(match[2]); !equalStrings(got, index.Columns) {
				t.Errorf("index %q columns = %v; want %v", index.Name, got, index.Columns)
			}
			if index.Predicate != "" {
				if normalizeExpression(match[3]) != normalizeExpression(index.Predicate) {
					t.Errorf("index %q predicate = %q; want %q", index.Name, match[3], index.Predicate)
				}
			}
		}
	}
}

func TestMigrationsForceRLSWithNonOwnerRuntimeRole(t *testing.T) {
	sql := allMigrationSQL()
	schema := store.SchemaDesign()
	if schema.RuntimeRole == schema.OwnerRole {
		t.Fatalf("runtime role %q must not be the owner role", schema.RuntimeRole)
	}

	runtimeRole := regexp.MustCompile(`create\s+(?:role|user)\s+` + ident(schema.RuntimeRole))
	if !runtimeRole.MatchString(sql) {
		t.Errorf("migrations do not create the runtime role %q", schema.RuntimeRole)
	}
	ownerRole := regexp.MustCompile(`create\s+(?:role|user)\s+` + ident(schema.OwnerRole))
	if !ownerRole.MatchString(sql) {
		t.Errorf("migrations do not create the owner role %q", schema.OwnerRole)
	}

	for _, table := range schema.Tables {
		if table.RowLevelSecurity {
			enable := regexp.MustCompile(`alter\s+table\s+` + qualified(table.Name) + `\s+enable\s+row\s+level\s+security`)
			if !enable.MatchString(sql) {
				t.Errorf("table %q does not enable row-level security", table.Name)
			}
			policy := regexp.MustCompile(`create\s+policy\s+[a-z0-9_]+\s+on\s+` + qualified(table.Name))
			location := policy.FindStringIndex(sql)
			if location == nil {
				t.Errorf("table %q has no row-level security policy", table.Name)
			} else {
				segment := statementAt(sql, location[0])
				if !strings.Contains(segment, "kodama.tenant_id") {
					t.Errorf("policy on table %q does not read the transaction-local tenant setting", table.Name)
				}
				if hasSchemaColumn(table, "project_id") {
					if !strings.Contains(segment, "kodama.project_id") {
						t.Errorf("policy on table %q does not read the transaction-local project setting", table.Name)
					}
					if !strings.Contains(segment, "project_id") {
						t.Errorf("policy on table %q does not constrain project_id", table.Name)
					}
				}
			}
		}
		if table.ForceRowLevelSecurity {
			force := regexp.MustCompile(`alter\s+table\s+` + qualified(table.Name) + `\s+force\s+row\s+level\s+security`)
			if !force.MatchString(sql) {
				t.Errorf("table %q does not force row-level security", table.Name)
			}
		}
		owner := regexp.MustCompile(`alter\s+table\s+` + qualified(table.Name) + `\s+owner\s+to\s+` + ident(schema.OwnerRole))
		if !owner.MatchString(sql) {
			t.Errorf("table %q is not owned by the migration owner role %q", table.Name, schema.OwnerRole)
		}
	}

	grants := grantStatements(sql, schema.RuntimeRole)
	if len(grants) == 0 {
		t.Fatalf("migrations grant no privileges to the runtime role %q", schema.RuntimeRole)
	}
	allTables := false
	for _, grant := range grants {
		if strings.Contains(grant, "on all tables in schema") {
			allTables = true
		}
	}
	for _, table := range schema.Tables {
		if allTables {
			break
		}
		covered := false
		tablePattern := regexp.MustCompile(`(?:^|[^a-z0-9_])` + regexp.QuoteMeta(table.Name) + `(?:[^a-z0-9_]|$)`)
		if slices.ContainsFunc(grants, tablePattern.MatchString) {
			covered = true
		}
		if !covered {
			t.Errorf("runtime role %q has no grant on table %q", schema.RuntimeRole, table.Name)
		}
	}
	privileges := strings.Join(grants, " ")
	for _, privilege := range []string{"select", "insert", "update", "delete"} {
		if !strings.Contains(privileges, privilege) && !strings.Contains(privileges, "grant all") {
			t.Errorf("runtime role %q is not granted %s", schema.RuntimeRole, privilege)
		}
	}

	withoutDenials := strings.ReplaceAll(strings.ReplaceAll(sql, "nobypassrls", ""), "no bypassrls", "")
	if strings.Contains(withoutDenials, "bypassrls") {
		t.Error("migrations grant the runtime role RLS bypass")
	}
	for segment := range strings.SplitSeq(sql, ";") {
		if !strings.Contains(segment, schema.RuntimeRole) {
			continue
		}
		withoutSuperuserDenial := strings.ReplaceAll(strings.ReplaceAll(segment, "nosuperuser", ""), "no superuser", "")
		if strings.Contains(withoutSuperuserDenial, "superuser") {
			t.Errorf("runtime role is granted superuser: %s", strings.TrimSpace(segment))
		}
	}
}

func statementAt(sql string, position int) string {
	start := strings.LastIndex(sql[:position], ";") + 1
	end := strings.Index(sql[position:], ";")
	if end < 0 {
		return sql[start:]
	}
	return sql[start : position+end]
}

func grantStatements(sql, role string) []string {
	var grants []string
	for segment := range strings.SplitSeq(sql, ";") {
		if !strings.Contains(segment, "grant") || !strings.Contains(segment, role) {
			continue
		}
		grants = append(grants, strings.TrimSpace(segment))
	}
	return grants
}

func TestMigrationsDefaultEventPublishSequence(t *testing.T) {
	sql := allMigrationSQL()
	column := regexp.MustCompile(`publish_sequence\s+bigint\s+not\s+null\s+default\s+nextval\s*\(`)
	if !column.MatchString(sql) {
		t.Error("events.publish_sequence must default to a sequence value so the publisher has a durable ordering cursor")
	}
	if !regexp.MustCompile(`create\s+sequence`).MatchString(sql) {
		t.Error("migrations do not create the event publication sequence")
	}
}
