package adaptercontract

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/shell/store"
)

// schemaVectorDocument is the executable schema conformance input. It pins the
// table shape, the three non-owner system roles and their attributes, the
// row-level-security policies, the explicit complete grant sets, the single
// lock order and the migration naming rules. The store adapters port the same
// JSON document; it is semantic input, not PostgreSQL isolation proof.
type schemaVectorDocument struct {
	Version        int                         `json:"version"`
	Roles          schemaVectorRoles           `json:"roles"`
	Migration      schemaVectorMigrate         `json:"migration"`
	RoleAttributes []schemaVectorRoleAttribute `json:"role_attributes"`
	Policies       []schemaVectorPolicy        `json:"policies"`
	LockOrder      []string                    `json:"lock_order"`
	Tables         []schemaVectorTable         `json:"tables"`
	Grants         []schemaVectorGrant         `json:"grants"`
}

type schemaVectorRoles struct {
	Runtime    string `json:"runtime"`
	Owner      string `json:"owner"`
	Controller string `json:"controller"`
	Publisher  string `json:"publisher"`
	Auth       string `json:"auth"`
}

type schemaVectorMigrate struct {
	Pattern       string `json:"pattern"`
	SequenceStart int    `json:"sequence_start"`
}

type schemaVectorRoleAttribute struct {
	Name        string   `json:"name"`
	NoBypassRLS bool     `json:"nobypassrls"`
	NoInherit   bool     `json:"noinherit"`
	Memberships []string `json:"memberships"`
}

type schemaVectorPolicy struct {
	Role      string   `json:"role"`
	Table     string   `json:"table"`
	Command   string   `json:"command"`
	Using     string   `json:"using"`
	WithCheck string   `json:"with_check"`
	Column    string   `json:"column"`
	Values    []string `json:"values"`
}

type schemaVectorTable struct {
	Name             string               `json:"name"`
	RequiredColumns  []schemaVectorColumn `json:"required_columns"`
	ForbiddenColumns []string             `json:"forbidden_columns"`
	Indexes          []schemaVectorIndex  `json:"indexes"`
	Checks           []schemaVectorCheck  `json:"checks"`
	ProjectNullable  *bool                `json:"project_nullable"`
	ForcedRLS        *bool                `json:"forced_rls"`
}

type schemaVectorColumn struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Nullable bool    `json:"nullable"`
	Default  *string `json:"default"`
}

type schemaVectorIndex struct {
	Name      string   `json:"name"`
	Columns   []string `json:"columns"`
	Unique    bool     `json:"unique"`
	Predicate string   `json:"predicate"`
}

type schemaVectorCheck struct {
	Name     string   `json:"name"`
	Contains []string `json:"contains"`
	// Exact pins the complete normalized expression. It is how the
	// receipts_completion_pair rule (completed requires a result, incomplete may
	// carry an in-progress result) is distinguished from a strict equality pair.
	Exact string `json:"exact"`
}

type schemaVectorGrant struct {
	Role          string   `json:"role"`
	Table         string   `json:"table"`
	Operations    []string `json:"operations"`
	SelectColumns []string `json:"select_columns"`
	WriteColumns  []string `json:"write_columns"`
}

// schemaMutation is one deliberately broken copy of the declaration.
// It can mutate the schema design and the grant set, because grants come from
// provider functions rather than fields on Schema.
type schemaMutation struct {
	name   string
	mutate func(*store.Schema, []store.Grant) []store.Grant
}

// TestSchemaDesignConformanceVectors runs the executable schema vectors against
// SchemaDesign and its grant providers. It proves the declared tables,
// columns, indexes, checks, roles, role attributes, policies, complete grant
// sets, lock order and migration naming, and proves the vectors are not vacuous
// by mutating a copy of the design and of the grant set.
func TestSchemaDesignConformanceVectors(t *testing.T) {
	document := loadSchemaVectors(t)
	validateSchemaVectorDocument(t, document)
	schema := store.SchemaDesign()
	grants := allSystemGrants()
	if failures := schemaVectorFailures(schema, grants, document); len(failures) > 0 {
		t.Fatalf("SchemaDesign does not satisfy the schema vectors:\n%s", strings.Join(failures, "\n"))
	}

	mutations := []schemaMutation{
		{"drop an action column", func(schema *store.Schema, grants []store.Grant) []store.Grant {
			table := schemaTablePointer(schema, "actions")
			table.Columns = removeSchemaColumn(table.Columns, "launch_state")
			return grants
		}},
		{"restore the dropped verification source event", func(schema *store.Schema, grants []store.Grant) []store.Grant {
			table := schemaTablePointer(schema, "verifications")
			table.Columns = append(table.Columns, store.Column{Name: "source_event_id", Type: "text"})
			return grants
		}},
		{"drop the receipts report dedupe index", func(schema *store.Schema, grants []store.Grant) []store.Grant {
			table := schemaTablePointer(schema, "receipts")
			table.Indexes = removeSchemaIndex(table.Indexes, "receipts_resource_report_dedupe")
			return grants
		}},
		{"drop the receipts completion marker", func(schema *store.Schema, grants []store.Grant) []store.Grant {
			table := schemaTablePointer(schema, "receipts")
			table.Columns = removeSchemaColumn(table.Columns, "completed_at")
			return grants
		}},
		{"drop the receipts completion check", func(schema *store.Schema, grants []store.Grant) []store.Grant {
			table := schemaTablePointer(schema, "receipts")
			table.Checks = removeSchemaCheck(table.Checks, "receipts_completion_pair")
			return grants
		}},
		{"restore the strict receipt completion equality pair", func(schema *store.Schema, grants []store.Grant) []store.Grant {
			table := schemaTablePointer(schema, "receipts")
			for index := range table.Checks {
				if table.Checks[index].Name == "receipts_completion_pair" {
					table.Checks[index].Expression = "(completed_at IS NULL) = (result IS NULL)"
				}
			}
			return grants
		}},
		{"drop the pipelines push projection default", func(schema *store.Schema, grants []store.Grant) []store.Grant {
			table := schemaTablePointer(schema, "pipelines")
			for index := range table.Columns {
				if table.Columns[index].Name == "push_triggers" {
					table.Columns[index].Default = ""
				}
			}
			return grants
		}},
		{"unforce row-level security", func(schema *store.Schema, grants []store.Grant) []store.Grant {
			table := schemaTablePointer(schema, "model_usage")
			table.ForceRowLevelSecurity = false
			return grants
		}},
		{"collapse the controller role into the owner role", func(schema *store.Schema, grants []store.Grant) []store.Grant {
			schema.ControllerRole = schema.OwnerRole
			return grants
		}},
		{"shorten the lock order", func(schema *store.Schema, grants []store.Grant) []store.Grant {
			schema.LockOrder = schema.LockOrder[:len(schema.LockOrder)-1]
			return grants
		}},
		{"make the controller write policy scope-free", func(schema *store.Schema, grants []store.Grant) []store.Grant {
			for index := range schema.Policies {
				policy := &schema.Policies[index]
				if policy.Role == store.ControllerRole && policy.Command == store.PolicyUpdate {
					policy.Using = store.PredicateAny
					policy.WithCheck = store.PredicateAny
				}
			}
			return grants
		}},
		{"drop every controller write policy", func(schema *store.Schema, grants []store.Grant) []store.Grant {
			kept := schema.Policies[:0]
			for _, policy := range schema.Policies {
				if policy.Role == store.ControllerRole && policy.Command != store.PolicySelect {
					continue
				}
				kept = append(kept, policy)
			}
			schema.Policies = kept
			return grants
		}},
		{"grant the controller BYPASSRLS", func(schema *store.Schema, grants []store.Grant) []store.Grant {
			for index := range schema.RoleAttributes {
				if schema.RoleAttributes[index].Name == store.ControllerRole {
					schema.RoleAttributes[index].NoBypassRLS = false
				}
			}
			return grants
		}},
		{"add controller membership in the runtime role", func(schema *store.Schema, grants []store.Grant) []store.Grant {
			for index := range schema.RoleAttributes {
				if schema.RoleAttributes[index].Name == store.ControllerRole {
					schema.RoleAttributes[index].Memberships = append(schema.RoleAttributes[index].Memberships, schema.RuntimeRole)
				}
			}
			return grants
		}},
		{"widen the publisher grant to every event column", func(schema *store.Schema, grants []store.Grant) []store.Grant {
			return mutateGrant(grants, store.PublisherRole, "events", func(grant *store.Grant) {
				grant.WriteColumns = nil
			})
		}},
		{"grant the publisher an insert operation", func(schema *store.Schema, grants []store.Grant) []store.Grant {
			return mutateGrant(grants, store.PublisherRole, "events", func(grant *store.Grant) {
				grant.Operations = append(grant.Operations, store.OperationInsert)
			})
		}},
		{"grant the publisher receipts access", func(schema *store.Schema, grants []store.Grant) []store.Grant {
			return append(grants, store.Grant{
				Role:       store.PublisherRole,
				Table:      "receipts",
				Operations: []store.Operation{store.OperationSelect},
			})
		}},
		{"grant the auth role secrets access", func(schema *store.Schema, grants []store.Grant) []store.Grant {
			return append(grants, store.Grant{
				Role:          store.AuthRole,
				Table:         "secrets",
				Operations:    []store.Operation{store.OperationSelect},
				SelectColumns: []string{"id"},
			})
		}},
		{"grant the controller DELETE", func(schema *store.Schema, grants []store.Grant) []store.Grant {
			return mutateGrant(grants, store.ControllerRole, "executions", func(grant *store.Grant) {
				grant.Operations = append(grant.Operations, store.OperationDelete)
			})
		}},
		{"drop the auth service grant", func(schema *store.Schema, grants []store.Grant) []store.Grant {
			kept := grants[:0]
			for _, grant := range grants {
				if grant.Role == store.AuthRole && grant.Table == "services" {
					continue
				}
				kept = append(kept, grant)
			}
			return kept
		}},
	}
	for _, mutation := range mutations {
		broken := store.SchemaDesign()
		brokenGrants := cloneGrants(allSystemGrants())
		brokenGrants = mutation.mutate(&broken, brokenGrants)
		if failures := schemaVectorFailures(broken, brokenGrants, document); len(failures) == 0 {
			t.Errorf("schema vectors did not detect mutation %q", mutation.name)
		}
	}
}

// TestSchemaSystemRoles proves the three system roles are non-owner,
// NOBYPASSRLS, NOINHERIT roles distinct from the runtime and owner roles, that
// no system role is a member of the runtime or owner role, that every declared
// policy validates, and that every scoped table forces row-level security.
func TestSchemaSystemRoles(t *testing.T) {
	schema := store.SchemaDesign()
	roles := []string{schema.RuntimeRole, schema.ControllerRole, schema.PublisherRole, schema.AuthRole}
	seen := make(map[string]bool, len(roles))
	for _, role := range roles {
		if role == "" {
			t.Fatal("schema declares an empty system role")
		}
		if seen[role] {
			t.Fatalf("schema declares system role %q twice", role)
		}
		seen[role] = true
		if role == schema.OwnerRole {
			t.Fatalf("runtime system role %q must not be the owner role", role)
		}
	}
	if len(schema.RoleAttributes) != 3 {
		t.Fatalf("schema declares %d role attribute sets; want 3", len(schema.RoleAttributes))
	}
	for _, attributes := range schema.RoleAttributes {
		if err := attributes.Validate(); err != nil {
			t.Errorf("system role attributes are invalid: %v", err)
		}
		for _, membership := range attributes.Memberships {
			if membership == schema.RuntimeRole || membership == schema.OwnerRole {
				t.Errorf("system role %q is a member of %q", attributes.Name, membership)
			}
		}
	}
	if len(schema.Policies) == 0 {
		t.Fatal("schema declares no row-level-security policies")
	}
	for _, policy := range schema.Policies {
		if err := policy.Validate(); err != nil {
			t.Errorf("system policy is invalid: %v", err)
		}
		if policy.Role == schema.AuthRole && policy.Command != store.PolicySelect {
			t.Errorf("auth role declares a %q policy; it must be read-only", policy.Command)
		}
	}
	for _, table := range schema.Tables {
		if table.Name == "tenants" || hasColumn(table, "tenant_id") {
			if !table.RowLevelSecurity || !table.ForceRowLevelSecurity {
				t.Errorf("table %q does not force row-level security", table.Name)
			}
		}
	}
}

func loadSchemaVectors(t *testing.T) schemaVectorDocument {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "schema.json"))
	if err != nil {
		t.Fatalf("read schema conformance vectors: %v", err)
	}
	var document schemaVectorDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode schema conformance vectors: %v", err)
	}
	return document
}

func validateSchemaVectorDocument(t *testing.T, document schemaVectorDocument) {
	t.Helper()
	if document.Version != 1 {
		t.Fatalf("schema vector document version = %d; want 1", document.Version)
	}
	if len(document.Tables) == 0 {
		t.Fatal("schema vector document declares no tables")
	}
	if len(document.Grants) == 0 {
		t.Fatal("schema vector document declares no grants")
	}
	if len(document.RoleAttributes) == 0 {
		t.Fatal("schema vector document declares no role attributes")
	}
	if len(document.Policies) == 0 {
		t.Fatal("schema vector document declares no policies")
	}
	if len(document.LockOrder) == 0 {
		t.Fatal("schema vector document declares no lock order")
	}
	names := make(map[string]bool, len(document.Tables))
	for _, table := range document.Tables {
		if table.Name == "" {
			t.Error("schema vector declares an unnamed table")
			continue
		}
		if names[table.Name] {
			t.Errorf("schema vector declares table %q twice", table.Name)
		}
		names[table.Name] = true
		if len(table.RequiredColumns) == 0 && len(table.ForbiddenColumns) == 0 && len(table.Indexes) == 0 && len(table.Checks) == 0 {
			t.Errorf("schema vector for table %q asserts nothing", table.Name)
		}
		for _, column := range table.RequiredColumns {
			if column.Name == "" || column.Type == "" {
				t.Errorf("schema vector for table %q has an incomplete column %+v", table.Name, column)
			}
		}
	}
	roleNames := make(map[string]bool, len(document.RoleAttributes))
	for _, attributes := range document.RoleAttributes {
		if attributes.Name == "" {
			t.Error("schema vector declares unnamed role attributes")
			continue
		}
		if roleNames[attributes.Name] {
			t.Errorf("schema vector declares role attributes for %q twice", attributes.Name)
		}
		roleNames[attributes.Name] = true
	}
	policyKeys := make(map[string]bool, len(document.Policies))
	for _, policy := range document.Policies {
		if policy.Role == "" || policy.Table == "" || policy.Command == "" || policy.Using == "" {
			t.Errorf("schema vector declares an incomplete policy %+v", policy)
			continue
		}
		key := policyKey(policy.Role, policy.Table, policy.Command)
		if policyKeys[key] {
			t.Errorf("schema vector declares policy %q twice", key)
		}
		policyKeys[key] = true
	}
	grantKeys := make(map[string]bool, len(document.Grants))
	for _, grant := range document.Grants {
		if grant.Role == "" || grant.Table == "" || len(grant.Operations) == 0 {
			t.Errorf("schema vector declares an incomplete grant %+v", grant)
			continue
		}
		key := grantKey(grant.Role, grant.Table)
		if grantKeys[key] {
			t.Errorf("schema vector declares grant %q twice", key)
		}
		grantKeys[key] = true
	}
}

func schemaVectorFailures(schema store.Schema, grants []store.Grant, document schemaVectorDocument) []string {
	var failures []string
	expect := func(condition bool, format string, args ...any) {
		if !condition {
			failures = append(failures, strings.TrimSpace(fmt.Sprintf(format, args...)))
		}
	}
	expect(schema.RuntimeRole == document.Roles.Runtime, "runtime role = %q; want %q", schema.RuntimeRole, document.Roles.Runtime)
	expect(schema.OwnerRole == document.Roles.Owner, "owner role = %q; want %q", schema.OwnerRole, document.Roles.Owner)
	expect(schema.ControllerRole == document.Roles.Controller, "controller role = %q; want %q", schema.ControllerRole, document.Roles.Controller)
	expect(schema.PublisherRole == document.Roles.Publisher, "publisher role = %q; want %q", schema.PublisherRole, document.Roles.Publisher)
	expect(schema.AuthRole == document.Roles.Auth, "auth role = %q; want %q", schema.AuthRole, document.Roles.Auth)
	expect(schema.MigrationNamePattern == document.Migration.Pattern, "migration pattern = %q; want %q", schema.MigrationNamePattern, document.Migration.Pattern)
	expect(schema.MigrationSequenceStart == document.Migration.SequenceStart, "migration sequence start = %d; want %d", schema.MigrationSequenceStart, document.Migration.SequenceStart)
	expect(sameStringSet(schema.LockOrder, document.LockOrder), "lock order = %v; want %v", schema.LockOrder, document.LockOrder)

	// Role attributes are a complete set: no missing role, no extra role, and no
	// system role may inherit the runtime or owner authority.
	actualRoles := make(map[string]store.RoleAttributes, len(schema.RoleAttributes))
	for _, attributes := range schema.RoleAttributes {
		if err := attributes.Validate(); err != nil {
			failures = append(failures, fmt.Sprintf("role attributes %q are invalid: %v", attributes.Name, err))
		}
		if _, duplicate := actualRoles[attributes.Name]; duplicate {
			failures = append(failures, fmt.Sprintf("role attributes %q are declared twice", attributes.Name))
		}
		actualRoles[attributes.Name] = attributes
	}
	wantedRoles := make(map[string]bool, len(document.RoleAttributes))
	for _, want := range document.RoleAttributes {
		wantedRoles[want.Name] = true
		got, ok := actualRoles[want.Name]
		if !ok {
			failures = append(failures, fmt.Sprintf("role attributes for %q are missing", want.Name))
			continue
		}
		expect(got.NoBypassRLS == want.NoBypassRLS, "role %q nobypassrls = %v; want %v", want.Name, got.NoBypassRLS, want.NoBypassRLS)
		expect(got.NoInherit == want.NoInherit, "role %q noinherit = %v; want %v", want.Name, got.NoInherit, want.NoInherit)
		expect(sameStringSet(got.Memberships, want.Memberships), "role %q memberships = %v; want %v", want.Name, got.Memberships, want.Memberships)
		for _, membership := range got.Memberships {
			expect(membership != schema.RuntimeRole && membership != schema.OwnerRole, "role %q must not be a member of %q", want.Name, membership)
		}
	}
	for name := range actualRoles {
		expect(wantedRoles[name], "unexpected role attributes %q", name)
	}

	// Policies are a complete set pinned by role, table and command. A literal
	// scope predicate on the cross-scope controller SELECT or a blanket
	// permissive write policy is therefore a vector failure.
	actualPolicies := make(map[string]store.Policy, len(schema.Policies))
	for _, policy := range schema.Policies {
		if err := policy.Validate(); err != nil {
			failures = append(failures, fmt.Sprintf("policy %s/%s/%s is invalid: %v", policy.Role, policy.Table, policy.Command, err))
		}
		key := policyKey(policy.Role, policy.Table, string(policy.Command))
		if _, duplicate := actualPolicies[key]; duplicate {
			failures = append(failures, fmt.Sprintf("policy %q is declared twice", key))
		}
		actualPolicies[key] = policy
	}
	wantedPolicies := make(map[string]bool, len(document.Policies))
	for _, want := range document.Policies {
		key := policyKey(want.Role, want.Table, want.Command)
		wantedPolicies[key] = true
		got, ok := actualPolicies[key]
		if !ok {
			failures = append(failures, fmt.Sprintf("policy %q is missing", key))
			continue
		}
		expect(string(got.Command) == want.Command, "policy %q command = %q; want %q", key, got.Command, want.Command)
		expect(got.Using == want.Using, "policy %q using = %q; want %q", key, got.Using, want.Using)
		expect(got.WithCheck == want.WithCheck, "policy %q with check = %q; want %q", key, got.WithCheck, want.WithCheck)
		expect(got.Column == want.Column, "policy %q column = %q; want %q", key, got.Column, want.Column)
		expect(sameStringSet(got.Values, want.Values), "policy %q values = %v; want %v", key, got.Values, want.Values)
		expect(got.Command != store.PolicyDelete, "policy %q grants DELETE", key)
	}
	for key := range actualPolicies {
		expect(wantedPolicies[key], "unexpected policy %q", key)
	}
	for _, policy := range schema.Policies {
		switch policy.Role {
		case schema.ControllerRole:
			if policy.Command != store.PolicySelect {
				expect(stringSliceContains(store.ControllerWriteTables(), policy.Table), "controller write policy targets undeclared write table %q", policy.Table)
			}
		case schema.PublisherRole:
			expect(policy.Table == "events", "publisher policy targets %q; want events", policy.Table)
		case schema.AuthRole:
			expect(policy.Command == store.PolicySelect, "auth policy grants %q; it must be read-only", policy.Command)
		}
	}

	// Grants are a complete set per role: every declared grant must validate,
	// no extra grant/table/operation/column may exist, and no system role may
	// hold DELETE. Checking only that listed grants are present would miss an
	// added auth.secrets, publisher.receipts or controller DELETE grant.
	actualGrants := make(map[string]store.Grant, len(grants))
	for _, grant := range grants {
		if err := grant.Validate(); err != nil {
			failures = append(failures, fmt.Sprintf("grant %s/%s is invalid: %v", grant.Role, grant.Table, err))
		}
		key := grantKey(grant.Role, grant.Table)
		if _, duplicate := actualGrants[key]; duplicate {
			failures = append(failures, fmt.Sprintf("grant %q is declared twice", key))
		}
		actualGrants[key] = grant
		expect(grant.Role == schema.ControllerRole || grant.Role == schema.PublisherRole || grant.Role == schema.AuthRole,
			"grant %q names a non-system role", key)
		for _, operation := range grant.Operations {
			expect(operation != store.OperationDelete, "grant %q holds DELETE", key)
		}
	}
	wantedGrants := make(map[string]schemaVectorGrant, len(document.Grants))
	for _, want := range document.Grants {
		key := grantKey(want.Role, want.Table)
		if _, duplicate := wantedGrants[key]; duplicate {
			failures = append(failures, fmt.Sprintf("grant vector %q is declared twice", key))
		}
		wantedGrants[key] = want
	}
	for key, want := range wantedGrants {
		got, ok := actualGrants[key]
		if !ok {
			failures = append(failures, fmt.Sprintf("grant %q is missing", key))
			continue
		}
		expect(sameOperationSet(got.Operations, want.Operations), "grant %q operations = %v; want %v", key, got.Operations, want.Operations)
		expect(sameStringSet(got.SelectColumns, want.SelectColumns), "grant %q select columns = %v; want %v", key, got.SelectColumns, want.SelectColumns)
		expect(sameStringSet(got.WriteColumns, want.WriteColumns), "grant %q write columns = %v; want %v", key, got.WriteColumns, want.WriteColumns)
	}
	for key := range actualGrants {
		expect(wantedGrants[key].Role != "", "unexpected grant %q", key)
	}

	// The per-role authority is pinned directly as well, so a future grant
	// provider that drifts from the vector cannot pass by accident.
	publisher := grantsForRole(grants, schema.PublisherRole)
	expect(len(publisher) == 1, "publisher grants = %d; want exactly 1", len(publisher))
	if len(publisher) == 1 {
		expect(publisher[0].Table == "events", "publisher grant table = %q; want events", publisher[0].Table)
		expect(sameOperationSet(publisher[0].Operations, []string{"select", "update"}), "publisher operations = %v; want select,update", publisher[0].Operations)
		expect(sameStringSet(publisher[0].WriteColumns, []string{"publish_state"}), "publisher write columns = %v; want publish_state", publisher[0].WriteColumns)
	}
	auth := grantsForRole(grants, schema.AuthRole)
	expect(len(auth) == 2, "auth grants = %d; want exactly 2", len(auth))
	for _, grant := range auth {
		expect(sameOperationSet(grant.Operations, []string{"select"}), "auth grant %q operations = %v; want select", grant.Table, grant.Operations)
		expect(len(grant.WriteColumns) == 0, "auth grant %q has write columns %v", grant.Table, grant.WriteColumns)
		switch grant.Table {
		case "executions":
			expect(sameStringSet(grant.SelectColumns, []string{"id", "tenant_id", "project_id", "principal_id"}), "auth executions select columns = %v", grant.SelectColumns)
		case "services":
			expect(sameStringSet(grant.SelectColumns, []string{"id", "tenant_id", "project_id"}), "auth services select columns = %v", grant.SelectColumns)
		default:
			failures = append(failures, fmt.Sprintf("auth grant targets unexpected table %q", grant.Table))
		}
	}
	for _, grant := range grantsForRole(grants, schema.ControllerRole) {
		expect(stringSliceContains(store.ControllerReadTables(), grant.Table) || stringSliceContains(store.ControllerWriteTables(), grant.Table),
			"controller grant targets undeclared table %q", grant.Table)
		for _, operation := range grant.Operations {
			if operation == store.OperationSelect {
				expect(stringSliceContains(store.ControllerReadTables(), grant.Table), "controller grant %q holds SELECT on an undeclared read table", grant.Table)
				continue
			}
			expect(stringSliceContains(store.ControllerWriteTables(), grant.Table), "controller grant %q holds non-read operation on undeclared write table", grant.Table)
		}
	}

	for _, want := range document.Tables {
		table, ok := findTable(schema, want.Name)
		if !ok {
			failures = append(failures, fmt.Sprintf("table %q is missing", want.Name))
			continue
		}
		for _, column := range want.RequiredColumns {
			got, ok := findColumn(table, column.Name)
			if !ok {
				failures = append(failures, fmt.Sprintf("table %q is missing column %q", want.Name, column.Name))
				continue
			}
			expect(got.Type == column.Type, "table %q column %q type = %q; want %q", want.Name, column.Name, got.Type, column.Type)
			expect(got.Nullable == column.Nullable, "table %q column %q nullable = %v; want %v", want.Name, column.Name, got.Nullable, column.Nullable)
			if column.Default != nil {
				expect(got.Default == *column.Default, "table %q column %q default = %q; want %q", want.Name, column.Name, got.Default, *column.Default)
			}
		}
		for _, name := range want.ForbiddenColumns {
			expect(!hasColumn(table, name), "table %q must not declare column %q", want.Name, name)
		}
		for _, index := range want.Indexes {
			got, ok := findSchemaIndex(table, index.Name)
			if !ok {
				failures = append(failures, fmt.Sprintf("table %q is missing index %q", want.Name, index.Name))
				continue
			}
			expect(sameStringSet(got.Columns, index.Columns), "index %q columns = %v; want %v", index.Name, got.Columns, index.Columns)
			expect(got.Unique == index.Unique, "index %q unique = %v; want %v", index.Name, got.Unique, index.Unique)
			expect(got.Predicate == index.Predicate, "index %q predicate = %q; want %q", index.Name, got.Predicate, index.Predicate)
		}
		for _, check := range want.Checks {
			found := false
			for _, got := range table.Checks {
				if got.Name == check.Name && containsAll(got.Expression, check.Contains) {
					found = true
					if check.Exact != "" {
						expect(normalizeSchemaExpression(got.Expression) == normalizeSchemaExpression(check.Exact),
							"table %q check %q expression = %q; want %q", want.Name, check.Name, got.Expression, check.Exact)
					}
					break
				}
			}
			expect(found, "table %q has no check %q containing %v", want.Name, check.Name, check.Contains)
		}
		if want.ProjectNullable != nil {
			column, ok := findColumn(table, "project_id")
			if !ok {
				failures = append(failures, fmt.Sprintf("table %q has no project_id column", want.Name))
			} else {
				expect(column.Nullable == *want.ProjectNullable, "table %q project_id nullable = %v; want %v", want.Name, column.Nullable, *want.ProjectNullable)
			}
		}
		if want.ForcedRLS != nil {
			expect(table.ForceRowLevelSecurity == *want.ForcedRLS, "table %q force RLS = %v; want %v", want.Name, table.ForceRowLevelSecurity, *want.ForcedRLS)
		}
	}
	return failures
}

func allSystemGrants() []store.Grant {
	grants := make([]store.Grant, 0, len(store.ControllerGrants())+len(store.PublisherGrants())+len(store.AuthGrants()))
	grants = append(grants, store.ControllerGrants()...)
	grants = append(grants, store.PublisherGrants()...)
	grants = append(grants, store.AuthGrants()...)
	return grants
}

func cloneGrants(grants []store.Grant) []store.Grant {
	cloned := make([]store.Grant, len(grants))
	for index, grant := range grants {
		grant.Operations = append([]store.Operation(nil), grant.Operations...)
		grant.SelectColumns = append([]string(nil), grant.SelectColumns...)
		grant.WriteColumns = append([]string(nil), grant.WriteColumns...)
		cloned[index] = grant
	}
	return cloned
}

func mutateGrant(grants []store.Grant, role, table string, mutate func(*store.Grant)) []store.Grant {
	for index := range grants {
		if grants[index].Role == role && grants[index].Table == table {
			mutate(&grants[index])
			return grants
		}
	}
	return grants
}

func grantsForRole(grants []store.Grant, role string) []store.Grant {
	var matching []store.Grant
	for _, grant := range grants {
		if grant.Role == role {
			matching = append(matching, grant)
		}
	}
	return matching
}

func grantKey(role, table string) string {
	return role + "|" + table
}

func policyKey(role, table, command string) string {
	return role + "|" + table + "|" + command
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sameOperationSet(left []store.Operation, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	want := make(map[string]bool, len(right))
	for _, operation := range right {
		want[operation] = true
	}
	for _, operation := range left {
		if !want[string(operation)] {
			return false
		}
	}
	return true
}

func findSchemaIndex(table store.Table, name string) (store.Index, bool) {
	for _, index := range table.Indexes {
		if index.Name == name {
			return index, true
		}
	}
	return store.Index{}, false
}

func schemaTablePointer(schema *store.Schema, name string) *store.Table {
	for index := range schema.Tables {
		if schema.Tables[index].Name == name {
			return &schema.Tables[index]
		}
	}
	return nil
}

func removeSchemaColumn(columns []store.Column, name string) []store.Column {
	kept := columns[:0]
	for _, column := range columns {
		if column.Name != name {
			kept = append(kept, column)
		}
	}
	return kept
}

func removeSchemaIndex(indexes []store.Index, name string) []store.Index {
	kept := indexes[:0]
	for _, index := range indexes {
		if index.Name != name {
			kept = append(kept, index)
		}
	}
	return kept
}

func removeSchemaCheck(checks []store.Check, name string) []store.Check {
	kept := checks[:0]
	for _, check := range checks {
		if check.Name != name {
			kept = append(kept, check)
		}
	}
	return kept
}

// normalizeSchemaExpression compares a check expression by its normalized
// tokens: case-insensitive with runs of whitespace collapsed. It lets a vector
// pin the complete expression without depending on incidental spacing.
func normalizeSchemaExpression(expression string) string {
	return strings.Join(strings.Fields(strings.ToLower(expression)), " ")
}
