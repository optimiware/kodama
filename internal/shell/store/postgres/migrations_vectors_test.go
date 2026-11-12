// Static migration checks against the shared schema vectors: the migration set
// renders exactly the declared tables, system roles, grants and policies with
// nothing extra.
package postgres

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/shell/store"
)

func rawMigrationSQL() string {
	parts := make([]string, 0, len(Migrations()))
	for _, migration := range Migrations() {
		parts = append(parts, migration.SQL)
	}
	return strings.Join(parts, "\n")
}

// schemaVector mirrors the subset of testdata/schema.json this adapter checks.
type schemaVector struct {
	Version        int                   `json:"version"`
	Roles          schemaRoles           `json:"roles"`
	Migration      schemaMigration       `json:"migration"`
	RoleAttributes []schemaRoleAttribute `json:"role_attributes"`
	Policies       []schemaPolicy        `json:"policies"`
	LockOrder      []string              `json:"lock_order"`
	Tables         []schemaTableVector   `json:"tables"`
	Grants         []schemaGrant         `json:"grants"`
}

type schemaRoles struct {
	Runtime    string `json:"runtime"`
	Owner      string `json:"owner"`
	Controller string `json:"controller"`
	Publisher  string `json:"publisher"`
	Auth       string `json:"auth"`
}

type schemaMigration struct {
	Pattern       string `json:"pattern"`
	SequenceStart int    `json:"sequence_start"`
}

type schemaRoleAttribute struct {
	Name        string   `json:"name"`
	NoBypassRLS bool     `json:"nobypassrls"`
	NoInherit   bool     `json:"noinherit"`
	Memberships []string `json:"memberships"`
}

type schemaPolicy struct {
	Role      string   `json:"role"`
	Table     string   `json:"table"`
	Command   string   `json:"command"`
	Using     string   `json:"using"`
	WithCheck string   `json:"with_check"`
	Column    string   `json:"column"`
	Values    []string `json:"values"`
}

type schemaTableVector struct {
	Name             string               `json:"name"`
	RequiredColumns  []schemaColumnVector `json:"required_columns"`
	ForbiddenColumns []string             `json:"forbidden_columns"`
	Checks           []schemaCheckVector  `json:"checks"`
	Indexes          []schemaIndexVector  `json:"indexes"`
	ProjectNullable  *bool                `json:"project_nullable"`
	ForcedRLS        *bool                `json:"forced_rls"`
}

type schemaColumnVector struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	Default  string `json:"default"`
}

type schemaCheckVector struct {
	Name     string   `json:"name"`
	Contains []string `json:"contains"`
}

type schemaIndexVector struct {
	Name      string   `json:"name"`
	Columns   []string `json:"columns"`
	Unique    bool     `json:"unique"`
	Predicate string   `json:"predicate"`
}

type schemaGrant struct {
	Role          string   `json:"role"`
	Table         string   `json:"table"`
	Operations    []string `json:"operations"`
	SelectColumns []string `json:"select_columns"`
	WriteColumns  []string `json:"write_columns"`
}

func loadSchemaVectors(t *testing.T) schemaVector {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "test", "contracts", "adapters", "testdata", "schema.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schema conformance vectors: %v", err)
	}
	var document schemaVector
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode schema conformance vectors: %v", err)
	}
	return document
}

// TestMigrationsMatchSchemaVectors adapts the shared schema vectors: every
// declared column, forbidden column, check, index, forced-RLS posture, role,
// attribute, policy, grant and lock order must be present in the migration set,
// and nothing extra may be rendered for the system roles.
func TestMigrationsMatchSchemaVectors(t *testing.T) {
	document := loadSchemaVectors(t)
	sql := allMigrationSQL()
	raw := rawMigrationSQL()
	schema := store.SchemaDesign()

	if document.Version != 1 {
		t.Fatalf("schema vector version = %d; want 1", document.Version)
	}
	if schema.RuntimeRole != document.Roles.Runtime || schema.OwnerRole != document.Roles.Owner ||
		schema.ControllerRole != document.Roles.Controller || schema.PublisherRole != document.Roles.Publisher ||
		schema.AuthRole != document.Roles.Auth {
		t.Fatalf("schema roles = %+v; want vector %+v", schema, document.Roles)
	}
	if schema.MigrationNamePattern != document.Migration.Pattern || schema.MigrationSequenceStart != document.Migration.SequenceStart {
		t.Fatalf("migration naming = %q/%d; want vector %q/%d",
			schema.MigrationNamePattern, schema.MigrationSequenceStart, document.Migration.Pattern, document.Migration.SequenceStart)
	}
	if strings.Join(schema.LockOrder, ",") != strings.Join(document.LockOrder, ",") {
		t.Fatalf("lock order = %v; want vector %v", schema.LockOrder, document.LockOrder)
	}

	for _, table := range document.Tables {
		block := createBlock(t, sql, table.Name)
		for _, column := range table.RequiredColumns {
			definition := renderedColumnDefinition(t, sql, block, table.Name, column.Name)
			if definition == "" {
				t.Errorf("table %q is missing declared column %q", table.Name, column.Name)
				continue
			}
			if !strings.Contains(definition, strings.ToLower(column.Type)) {
				t.Errorf("column %q.%q type = %q; want %q", table.Name, column.Name, definition, column.Type)
			}
			if column.Nullable && strings.Contains(definition, "not null") {
				t.Errorf("column %q.%q is NOT NULL; want nullable", table.Name, column.Name)
			}
			if !column.Nullable && !strings.Contains(definition, "not null") {
				t.Errorf("column %q.%q is nullable; want NOT NULL", table.Name, column.Name)
			}
			if column.Default != "" {
				if !strings.Contains(definition, "default '"+strings.ToLower(column.Default)+"'") {
					t.Errorf("column %q.%q definition %q has no default %q", table.Name, column.Name, definition, column.Default)
				}
			}
		}
		for _, forbidden := range table.ForbiddenColumns {
			added := regexp.MustCompile(`add\s+column\s+` + ident(forbidden)).MatchString(sql)
			if added {
				t.Errorf("table %q re-adds forbidden column %q", table.Name, forbidden)
				continue
			}
			// A forbidden column inside a CREATE TABLE block must be dropped.
			dropped := regexp.MustCompile(`alter\s+table\s+` + qualified(table.Name) + `\s+drop\s+column\s+` + ident(forbidden)).MatchString(sql)
			inCreate := regexp.MustCompile(`(?:^|[^a-z0-9_])` + regexp.QuoteMeta(forbidden) + `(?:[^a-z0-9_]|$)`).MatchString(block)
			if inCreate && !dropped {
				t.Errorf("table %q declares forbidden column %q and never drops it", table.Name, forbidden)
			}
		}
		for _, check := range table.Checks {
			expression, ok := renderedCheck(sql, check.Name)
			if !ok {
				t.Errorf("table %q has no declared check %q", table.Name, check.Name)
				continue
			}
			for _, token := range check.Contains {
				if !strings.Contains(expression, strings.ToLower(token)) {
					t.Errorf("check %q = %q; missing token %q", check.Name, expression, token)
				}
			}
		}
		for _, index := range table.Indexes {
			pattern := regexp.MustCompile(`create\s+(unique\s+)?index\s+` + ident(index.Name) + `\s+on\s+` + qualified(table.Name) + `\s*\(([^)]*)\)(?:\s+where\s+([^;]+))?`)
			match := pattern.FindStringSubmatch(sql)
			if match == nil {
				t.Errorf("table %q has no declared index %q", table.Name, index.Name)
				continue
			}
			if got := strings.TrimSpace(match[1]) != ""; got != index.Unique {
				t.Errorf("index %q unique = %v; want %v", index.Name, got, index.Unique)
			}
			if got := splitColumns(match[2]); !equalStrings(got, index.Columns) {
				t.Errorf("index %q columns = %v; want %v", index.Name, got, index.Columns)
			}
			if index.Predicate != "" && normalizeExpression(match[3]) != normalizeExpression(index.Predicate) {
				t.Errorf("index %q predicate = %q; want %q", index.Name, match[3], index.Predicate)
			}
		}
		if table.ForcedRLS != nil && *table.ForcedRLS {
			if !regexp.MustCompile(`alter\s+table\s+` + qualified(table.Name) + `\s+force\s+row\s+level\s+security`).MatchString(sql) {
				t.Errorf("table %q does not force row-level security", table.Name)
			}
		}
		if table.ProjectNullable != nil {
			definition := renderedColumnDefinition(t, sql, block, table.Name, "project_id")
			if definition == "" {
				t.Errorf("table %q has no project_id column", table.Name)
			} else if nullable := !strings.Contains(definition, "not null"); nullable != *table.ProjectNullable {
				t.Errorf("table %q project_id nullable = %v; want %v", table.Name, nullable, *table.ProjectNullable)
			}
		}
	}

	for _, attributes := range document.RoleAttributes {
		pattern := regexp.MustCompile(`create\s+role\s+` + ident(attributes.Name) + `\s+([^;]+);`)
		match := pattern.FindStringSubmatch(sql)
		if match == nil {
			t.Errorf("migrations do not create declared role %q", attributes.Name)
			continue
		}
		flags := strings.ToUpper(match[1])
		if attributes.NoBypassRLS != strings.Contains(flags, "NOBYPASSRLS") {
			t.Errorf("role %q NOBYPASSRLS = %v; want %v", attributes.Name, !attributes.NoBypassRLS, attributes.NoBypassRLS)
		}
		if attributes.NoInherit != strings.Contains(flags, "NOINHERIT") {
			t.Errorf("role %q NOINHERIT = %v; want %v", attributes.Name, !attributes.NoInherit, attributes.NoInherit)
		}
		for _, membership := range attributes.Memberships {
			if regexp.MustCompile(`grant\s+` + ident(membership) + `\s+to\s+` + ident(attributes.Name)).MatchString(sql) {
				t.Errorf("role %q has declared membership %q", attributes.Name, membership)
			}
		}
	}

	// The policies are rendered from the declaration data, including the two
	// controller events/audit INSERT policies. The full rendered migration set
	// must contain every declared policy exactly once.
	expectedPolicies := make(map[string]bool, len(document.Policies))
	for _, policy := range document.Policies {
		key := policy.Role + "|" + policy.Table + "|" + policy.Command
		expectedPolicies[key] = true
		pattern := regexp.MustCompile(`create\s+policy\s+[a-z0-9_]+\s+on\s+` + qualified(policy.Table) +
			`[^;]*\s+for\s+` + policy.Command + `\s+to\s+` + ident(policy.Role))
		if !pattern.MatchString(sql) {
			t.Errorf("missing declared policy %s", key)
			continue
		}
		segment := statementAt(sql, regexPosition(sql, pattern))
		for _, predicate := range []string{policy.Using, policy.WithCheck} {
			if predicate == "" {
				continue
			}
			switch predicate {
			case store.PredicateAny:
				if !strings.Contains(segment, "true") {
					t.Errorf("policy %s does not render the any predicate: %s", key, segment)
				}
			case store.PredicateScopeMatchesTxnScope:
				if !strings.Contains(segment, "kodama.tenant_id") || !strings.Contains(segment, "kodama.project_id") {
					t.Errorf("policy %s does not render the transaction scope predicate: %s", key, segment)
				}
			case store.PredicateColumnInSet:
				for _, value := range policy.Values {
					if !strings.Contains(segment, "'"+strings.ToLower(value)+"'") {
						t.Errorf("policy %s does not render value %q: %s", key, value, segment)
					}
				}
			case store.PredicateExactRow:
				if !strings.Contains(segment, "kodama.credential_id") {
					t.Errorf("policy %s does not render the exact-row predicate: %s", key, segment)
				}
			}
		}
	}
	renderedPolicies := regexp.MustCompile(`create\s+policy\s+[a-z0-9_]+\s+on\s+[a-z0-9_]+\s+[^;]*\s+to\s+(kodama_controller|kodama_publisher|kodama_auth)`).FindAllString(sql, -1)
	if len(renderedPolicies) != len(expectedPolicies) {
		t.Errorf("rendered %d system-role policies; want exactly the %d declared", len(renderedPolicies), len(expectedPolicies))
	}

	expectedGrants := 0
	for _, grant := range document.Grants {
		for _, operation := range grant.Operations {
			expectedGrants++
			pattern := regexp.MustCompile(`grant\s+` + operation + `(?:\s*\(([^)]*)\))?\s+on\s+` + qualified(grant.Table) + `\s+to\s+` + ident(grant.Role))
			match := pattern.FindStringSubmatch(sql)
			if match == nil {
				t.Errorf("missing declared grant %s %s on %s", operation, grant.Role, grant.Table)
				continue
			}
			wantColumns := grant.SelectColumns
			if operation == "insert" || operation == "update" {
				wantColumns = grant.WriteColumns
			}
			if len(wantColumns) > 0 {
				if got := splitColumns(match[1]); !equalStrings(got, wantColumns) {
					t.Errorf("grant %s %s on %s columns = %v; want %v", operation, grant.Role, grant.Table, got, wantColumns)
				}
			}
		}
	}
	renderedGrants := regexp.MustCompile(`(?i)grant\s+(?:select|insert|update|delete)\b[^;]*\s+to\s+(kodama_controller|kodama_publisher|kodama_auth);`).FindAllString(raw, -1)
	if len(renderedGrants) != expectedGrants {
		t.Errorf("rendered %d system-role grants; want exactly the %d declared", len(renderedGrants), expectedGrants)
	}
}

// renderedColumnDefinition returns the normalized definition of one declared
// column, either inside the CREATE TABLE block or in an ADD COLUMN statement.
func renderedColumnDefinition(t *testing.T, sql, block, table, column string) string {
	t.Helper()
	columnPattern := regexp.MustCompile(`(?:^|[,\n(])\s*` + ident(column) + `\s+([a-z0-9_]+(?:\s+not\s+null)?(?:\s+default\s+'[^']*')?)`)
	if match := columnPattern.FindStringSubmatch(block); match != nil {
		return strings.Join(strings.Fields(match[1]), " ")
	}
	addedPattern := regexp.MustCompile(`alter\s+table\s+` + qualified(table) + `\s+add\s+column\s+` + ident(column) + `\s+([a-z0-9_]+(?:\s+not\s+null)?(?:\s+default\s+'[^']*')?)`)
	if match := addedPattern.FindStringSubmatch(sql); match != nil {
		return strings.Join(strings.Fields(match[1]), " ")
	}
	return ""
}

// TestMigrationsSystemRolesHaveNoBroaderMembership proves no system role is a
// member of the runtime or owner role in either direction.
func TestMigrationsSystemRolesHaveNoBroaderMembership(t *testing.T) {
	sql := allMigrationSQL()
	for _, role := range []string{store.ControllerRole, store.PublisherRole, store.AuthRole} {
		for _, broader := range []string{store.DefaultRuntimeRole, store.DefaultOwnerRole} {
			for _, pattern := range []string{
				`grant\s+` + ident(broader) + `\s+to\s+` + ident(role),
				`grant\s+` + ident(role) + `\s+to\s+` + ident(broader),
			} {
				if regexp.MustCompile(pattern).MatchString(sql) {
					t.Errorf("system role %q has a membership involving %q", role, broader)
				}
			}
		}
	}
}
