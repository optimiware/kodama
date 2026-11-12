//go:build component

package postgrescomponent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// schemaVector mirrors the adapter schema conformance input. The test reads
// the exact vector file rather than restating it, so a drift in either the
// database or the vector fails.
type schemaVector struct {
	Version        int                   `json:"version"`
	Roles          schemaRoles           `json:"roles"`
	Migration      schemaMigration       `json:"migration"`
	RoleAttributes []schemaRoleAttribute `json:"role_attributes"`
	Policies       []schemaPolicy        `json:"policies"`
	LockOrder      []string              `json:"lock_order"`
	Tables         []schemaTable         `json:"tables"`
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
	Nobypassrls bool     `json:"nobypassrls"`
	Noinherit   bool     `json:"noinherit"`
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

type schemaTable struct {
	Name             string         `json:"name"`
	RequiredColumns  []schemaColumn `json:"required_columns"`
	ForbiddenColumns []string       `json:"forbidden_columns"`
	Indexes          []schemaIndex  `json:"indexes"`
	Checks           []schemaCheck  `json:"checks"`
	ProjectNullable  *bool          `json:"project_nullable"`
	ForcedRLS        *bool          `json:"forced_rls"`
}

type schemaColumn struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Nullable bool    `json:"nullable"`
	Default  *string `json:"default"`
}

type schemaIndex struct {
	Name      string   `json:"name"`
	Columns   []string `json:"columns"`
	Unique    bool     `json:"unique"`
	Predicate string   `json:"predicate"`
}

type schemaCheck struct {
	Name     string   `json:"name"`
	Contains []string `json:"contains"`
	Exact    string   `json:"exact"`
}

type schemaGrant struct {
	Role          string   `json:"role"`
	Table         string   `json:"table"`
	Operations    []string `json:"operations"`
	SelectColumns []string `json:"select_columns"`
	WriteColumns  []string `json:"write_columns"`
}

func loadSchemaVector(t *testing.T) schemaVector {
	t.Helper()
	path := filepath.Join("..", "..", "contracts", "adapters", "testdata", "schema.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schema vector: %v", err)
	}
	var document schemaVector
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode schema vector: %v", err)
	}
	if document.Version != 1 {
		t.Fatalf("schema vector version = %d; want 1", document.Version)
	}
	return document
}

// TestFrozenSchemaFactsMatchPostgreSQL is the executable schema conformance
// vector against real PostgreSQL: columns, types, nullability, defaults,
// forbidden columns, indexes, checks, forced RLS, role attributes, grants and
// policies.
func TestFrozenSchemaFactsMatchPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := pgtest.Start(t)
	runner := pgtest.NewMigrationRunner(shard)
	if applied, err := runner.Apply(ctx, 0); err != nil {
		t.Fatalf("apply migrations: %v", err)
	} else if len(applied) != storepgMigrations() {
		t.Fatalf("applied %d migrations; want %d", len(applied), storepgMigrations())
	}
	document := loadSchemaVector(t)
	failures := schemaFailures(ctx, t, shard, document)
	if len(failures) > 0 {
		t.Fatalf("real PostgreSQL does not satisfy the schema vectors:\n%s", strings.Join(failures, "\n"))
	}
	writeSummary(t, shard, "schema", map[string]any{"tables": len(document.Tables), "policies": len(document.Policies), "grants": len(document.Grants)})
}

func storepgMigrations() int { return len(storepg.Migrations()) }

func schemaFailures(ctx context.Context, t *testing.T, shard *pgtest.Shard, document schemaVector) []string {
	t.Helper()
	var failures []string
	expect := func(condition bool, format string, args ...any) {
		if !condition {
			failures = append(failures, fmt.Sprintf(format, args...))
		}
	}

	// Role attributes.
	for _, want := range document.RoleAttributes {
		attrs, ok := roleAttributes(ctx, t, shard, want.Name)
		if !ok {
			failures = append(failures, fmt.Sprintf("role %q is missing", want.Name))
			continue
		}
		expect(attrs.BypassRLS == !want.Nobypassrls, "role %q bypassrls = %v; want %v", want.Name, attrs.BypassRLS, !want.Nobypassrls)
		expect(attrs.Inherit == !want.Noinherit, "role %q inherit = %v; want %v", want.Name, attrs.Inherit, !want.Noinherit)
		expect(!attrs.Super && !attrs.CreateRole && !attrs.CreateDB, "role %q has elevated attributes: %+v", want.Name, attrs)
		memberships := roleMemberships(ctx, t, shard, want.Name)
		expect(sameSet(memberships, want.Memberships), "role %q memberships = %v; want %v", want.Name, memberships, want.Memberships)
	}
	expect(!roleAttributesValue(ctx, t, shard, document.Roles.Owner).CanLogin, "owner role is a login role")
	expect(roleAttributesValue(ctx, t, shard, document.Roles.Runtime).CanLogin, "runtime role is not a login role")

	// Tables, columns, indexes, checks and forced RLS.
	for _, want := range document.Tables {
		exists := tableExists(ctx, t, shard, want.Name)
		expect(exists, "table %q is missing", want.Name)
		if !exists {
			continue
		}
		for _, column := range want.RequiredColumns {
			fact, ok := columnFact(ctx, t, shard, want.Name, column.Name)
			if !ok {
				failures = append(failures, fmt.Sprintf("table %q is missing column %q", want.Name, column.Name))
				continue
			}
			expect(fact.Type == column.Type, "table %q column %q type = %q; want %q", want.Name, column.Name, fact.Type, column.Type)
			expect(fact.Nullable == column.Nullable, "table %q column %q nullable = %v; want %v", want.Name, column.Name, fact.Nullable, column.Nullable)
			if column.Default != nil {
				expect(normalizeDefault(fact.Default) == normalizeDefault(*column.Default),
					"table %q column %q default = %q; want %q", want.Name, column.Name, fact.Default, *column.Default)
			}
		}
		for _, forbidden := range want.ForbiddenColumns {
			_, ok := columnFact(ctx, t, shard, want.Name, forbidden)
			expect(!ok, "table %q must not declare column %q", want.Name, forbidden)
		}
		for _, index := range want.Indexes {
			fact, ok := indexFact(ctx, t, shard, want.Name, index.Name)
			if !ok {
				failures = append(failures, fmt.Sprintf("table %q is missing index %q", want.Name, index.Name))
				continue
			}
			expect(sameSet(fact.Columns, index.Columns), "index %q columns = %v; want %v", index.Name, fact.Columns, index.Columns)
			expect(fact.Unique == index.Unique, "index %q unique = %v; want %v", index.Name, fact.Unique, index.Unique)
			expect(normalizePredicate(fact.Predicate) == normalizePredicate(index.Predicate),
				"index %q predicate = %q; want %q", index.Name, fact.Predicate, index.Predicate)
		}
		for _, check := range want.Checks {
			def, ok := checkDefinition(ctx, t, shard, want.Name, check.Name)
			if !ok {
				failures = append(failures, fmt.Sprintf("table %q is missing check %q", want.Name, check.Name))
				continue
			}
			for _, contains := range check.Contains {
				expect(strings.Contains(strings.ToLower(def), strings.ToLower(contains)),
					"table %q check %q does not contain %q: %s", want.Name, check.Name, contains, def)
			}
			if check.Exact != "" {
				expect(normalizeExpression(def) == normalizeExpression(check.Exact),
					"table %q check %q expression = %q; want %q", want.Name, check.Name, def, check.Exact)
			}
		}
		if want.ProjectNullable != nil {
			column, ok := columnFact(ctx, t, shard, want.Name, "project_id")
			expect(ok && column.Nullable == *want.ProjectNullable,
				"table %q project_id nullable = %v; want %v", want.Name, ok && column.Nullable, *want.ProjectNullable)
		}
		if want.ForcedRLS != nil {
			enabled, forced := rlsFacts(ctx, t, shard, want.Name)
			expect(enabled == *want.ForcedRLS, "table %q RLS enabled = %v; want %v", want.Name, enabled, *want.ForcedRLS)
			expect(forced == *want.ForcedRLS, "table %q RLS forced = %v; want %v", want.Name, forced, *want.ForcedRLS)
		}
	}

	// Grants: complete set per role, including column restrictions.
	actualGrants := grantsByRoleTable(ctx, t, shard)
	for _, want := range document.Grants {
		key := want.Role + "|" + want.Table
		got, ok := actualGrants[key]
		if !ok {
			failures = append(failures, fmt.Sprintf("grant %q is missing", key))
			continue
		}
		expect(sameSet(got.Operations, want.Operations), "grant %q operations = %v; want %v", key, got.Operations, want.Operations)
		expect(sameSet(got.SelectColumns, want.SelectColumns), "grant %q select columns = %v; want %v", key, got.SelectColumns, want.SelectColumns)
		expect(sameSet(got.WriteColumns, want.WriteColumns), "grant %q write columns = %v; want %v", key, got.WriteColumns, want.WriteColumns)
		expect(!contains(got.Operations, "DELETE"), "grant %q holds DELETE", key)
	}
	expect(len(actualGrants) == len(document.Grants), "system grants = %d; want exactly %d", len(actualGrants), len(document.Grants))

	// Policies: every declared role, table, command, role list, permissive
	// posture and USING/WITH CHECK descriptor is mapped to the live catalog.
	// Migration 0011 excludes the two controller events/audit INSERT policies;
	// 0014 renders them from the declaration, so the live catalog must contain
	// every declared policy exactly and no widened or extra one.
	failures = append(failures, declaredPolicyFailures(ctx, t, shard, document.Policies)...)

	// The 0006 PUBLIC <table>_scope FOR ALL policy remains the general scope
	// admission for events/audit; it is not replaced by the controller-specific
	// INSERT policies.
	for _, table := range []string{"events", "audit"} {
		scopeName, scopeCmd, scopeRoles, scopeUsing := effectiveScopePolicy(ctx, t, shard, table)
		expect(scopeName == table+"_scope", "table %q scope policy = %q; want %q", table, scopeName, table+"_scope")
		expect(strings.EqualFold(scopeCmd, "ALL"), "table %q scope policy command = %q; want ALL", table, scopeCmd)
		expect(sameSet(scopeRoles, []string{"public"}), "table %q scope policy roles = %v; want public", table, scopeRoles)
		expect(strings.Contains(strings.ToLower(scopeUsing), "tenant_id"), "table %q scope policy has no tenant predicate: %q", table, scopeUsing)
	}
	return failures
}

// policyDefinition reads one policy's command, roles, USING and WITH CHECK from
// the live catalog.
func policyDefinition(ctx context.Context, t *testing.T, shard *pgtest.Shard, name string) (string, []string, string, string, bool) {
	t.Helper()
	rows, err := pgtest.QueryStringsN(ctx, shard.Admin(), 4,
		`SELECT cmd, roles::text, COALESCE(qual, ''), COALESCE(with_check, '')
		 FROM pg_policies WHERE schemaname = 'public' AND policyname = $1`, name)
	if err != nil {
		t.Fatalf("policy definition %s: %v", name, err)
	}
	if len(rows) != 1 {
		return "", nil, "", "", false
	}
	roles := strings.Trim(strings.TrimSpace(rows[0][1]), "{}")
	return rows[0][0], strings.Split(roles, ","), rows[0][2], rows[0][3], true
}

func effectiveScopePolicy(ctx context.Context, t *testing.T, shard *pgtest.Shard, table string) (string, string, []string, string) {
	t.Helper()
	rows, err := pgtest.QueryStringsN(ctx, shard.Admin(), 4,
		`SELECT policyname, cmd, roles::text, COALESCE(qual, '') FROM pg_policies
		 WHERE schemaname = 'public' AND tablename = $1 AND policyname = $2`, table, table+"_scope")
	if err != nil {
		t.Fatalf("effective policy %s: %v", table, err)
	}
	if len(rows) != 1 {
		return "", "", nil, ""
	}
	roles := strings.Trim(strings.TrimSpace(rows[0][2]), "{}")
	return rows[0][0], rows[0][1], strings.Split(roles, ","), rows[0][3]
}

type roleFacts struct {
	BypassRLS  bool
	Inherit    bool
	CanLogin   bool
	Super      bool
	CreateRole bool
	CreateDB   bool
}

func roleAttributes(ctx context.Context, t *testing.T, shard *pgtest.Shard, role string) (roleFacts, bool) {
	t.Helper()
	rows, err := pgtest.QueryStringsN(ctx, shard.Admin(), 6,
		`SELECT rolbypassrls::text, rolinherit::text, rolcanlogin::text, rolsuper::text, rolcreaterole::text, rolcreatedb::text FROM pg_roles WHERE rolname = $1`, role)
	if err != nil {
		t.Fatalf("read role %s: %v", role, err)
	}
	if len(rows) == 0 {
		return roleFacts{}, false
	}
	return roleFacts{
		BypassRLS: rows[0][0] == "true", Inherit: rows[0][1] == "true", CanLogin: rows[0][2] == "true",
		Super: rows[0][3] == "true", CreateRole: rows[0][4] == "true", CreateDB: rows[0][5] == "true",
	}, true
}

func roleAttributesValue(ctx context.Context, t *testing.T, shard *pgtest.Shard, role string) roleFacts {
	t.Helper()
	attrs, ok := roleAttributes(ctx, t, shard, role)
	if !ok {
		t.Fatalf("role %q is missing", role)
	}
	return attrs
}

func roleMemberships(ctx context.Context, t *testing.T, shard *pgtest.Shard, role string) []string {
	t.Helper()
	rows, err := pgtest.QueryStringsN(ctx, shard.Admin(), 1,
		`SELECT parent.rolname FROM pg_auth_members m JOIN pg_roles parent ON parent.oid = m.roleid JOIN pg_roles member ON member.oid = m.member WHERE member.rolname = $1 ORDER BY 1`, role)
	if err != nil {
		t.Fatalf("read role memberships %s: %v", role, err)
	}
	out := make([]string, len(rows))
	for index, row := range rows {
		out[index] = row[0]
	}
	return out
}

func tableExists(ctx context.Context, t *testing.T, shard *pgtest.Shard, table string) bool {
	t.Helper()
	value, err := pgtest.ScalarInt(ctx, shard.Admin(),
		`SELECT count(*)::text FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1`, table)
	if err != nil {
		t.Fatalf("table exists %s: %v", table, err)
	}
	return value == 1
}

type columnInfo struct {
	Type     string
	Nullable bool
	Default  string
}

func columnFact(ctx context.Context, t *testing.T, shard *pgtest.Shard, table, column string) (columnInfo, bool) {
	t.Helper()
	rows, err := pgtest.QueryStringsN(ctx, shard.Admin(), 3,
		`SELECT data_type, is_nullable, COALESCE(column_default, '') FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`, table, column)
	if err != nil {
		t.Fatalf("column fact %s.%s: %v", table, column, err)
	}
	if len(rows) == 0 {
		return columnInfo{}, false
	}
	return columnInfo{Type: normalizeColumnType(rows[0][0]), Nullable: rows[0][1] == "YES", Default: rows[0][2]}, true
}

func normalizeColumnType(value string) string {
	switch value {
	case "timestamp with time zone":
		return "timestamptz"
	default:
		return value
	}
}

type indexInfo struct {
	Columns   []string
	Unique    bool
	Predicate string
}

func indexFact(ctx context.Context, t *testing.T, shard *pgtest.Shard, table, name string) (indexInfo, bool) {
	t.Helper()
	rows, err := pgtest.QueryStringsN(ctx, shard.Admin(), 3,
		`SELECT ix.indisunique::text, COALESCE(pg_get_expr(ix.indpred, ix.indrelid), ''),
		        COALESCE(array_to_string(ARRAY(
		            SELECT a.attname FROM unnest(ix.indkey) WITH ORDINALITY AS k(attnum, ord)
		            JOIN pg_attribute a ON a.attrelid = ix.indrelid AND a.attnum = k.attnum
		            ORDER BY k.ord), ','), '')
		 FROM pg_index ix JOIN pg_class i ON i.oid = ix.indexrelid JOIN pg_class t ON t.oid = ix.indrelid
		 WHERE t.relname = $1 AND i.relname = $2`, table, name)
	if err != nil {
		t.Fatalf("index fact %s.%s: %v", table, name, err)
	}
	if len(rows) == 0 {
		return indexInfo{}, false
	}
	var columns []string
	for column := range strings.SplitSeq(rows[0][2], ",") {
		if column != "" {
			columns = append(columns, column)
		}
	}
	return indexInfo{Unique: rows[0][0] == "true", Predicate: rows[0][1], Columns: columns}, true
}

func checkDefinition(ctx context.Context, t *testing.T, shard *pgtest.Shard, table, name string) (string, bool) {
	t.Helper()
	rows, err := pgtest.QueryStringsN(ctx, shard.Admin(), 1,
		`SELECT pg_get_constraintdef(c.oid) FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid
		 WHERE t.relname = $1 AND c.conname = $2`, table, name)
	if err != nil {
		t.Fatalf("check definition %s.%s: %v", table, name, err)
	}
	if len(rows) == 0 {
		return "", false
	}
	return rows[0][0], true
}

func rlsFacts(ctx context.Context, t *testing.T, shard *pgtest.Shard, table string) (bool, bool) {
	t.Helper()
	row, err := pgtest.QueryOne(ctx, shard.Admin(), 2,
		`SELECT relrowsecurity::text, relforcerowsecurity::text FROM pg_class WHERE relname = $1 AND relkind = 'r'`, table)
	if err != nil {
		t.Fatalf("rls facts %s: %v", table, err)
	}
	return row[0] == "true", row[1] == "true"
}

type grantInfo struct {
	Operations    []string
	SelectColumns []string
	WriteColumns  []string
}

func grantsByRoleTable(ctx context.Context, t *testing.T, shard *pgtest.Shard) map[string]grantInfo {
	t.Helper()
	result := map[string]grantInfo{}
	tableRows, err := pgtest.QueryStringsN(ctx, shard.Admin(), 3,
		`SELECT grantee.rolname, c.relname, p.privilege_type
		 FROM pg_class c CROSS JOIN LATERAL aclexplode(c.relacl) p
		 JOIN pg_roles grantee ON grantee.oid = p.grantee
		 WHERE c.relkind = 'r' AND grantee.rolname IN ('kodama_controller','kodama_publisher','kodama_auth')
		 ORDER BY 1,2,3`)
	if err != nil {
		t.Fatalf("table privileges: %v", err)
	}
	for _, row := range tableRows {
		key := row[0] + "|" + row[1]
		grant := result[key]
		grant.Operations = append(grant.Operations, row[2])
		result[key] = grant
	}
	columnRows, err := pgtest.QueryStringsN(ctx, shard.Admin(), 4,
		`SELECT grantee.rolname, c.relname, p.privilege_type, a.attname
		 FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
		 CROSS JOIN LATERAL aclexplode(a.attacl) p
		 JOIN pg_roles grantee ON grantee.oid = p.grantee
		 WHERE c.relkind = 'r' AND a.attacl IS NOT NULL
		   AND grantee.rolname IN ('kodama_controller','kodama_publisher','kodama_auth')
		 ORDER BY 1,2,3,4`)
	if err != nil {
		t.Fatalf("column privileges: %v", err)
	}
	for _, row := range columnRows {
		key := row[0] + "|" + row[1]
		grant := result[key]
		switch row[2] {
		case "SELECT":
			grant.SelectColumns = append(grant.SelectColumns, row[3])
			grant.Operations = appendUnique(grant.Operations, "SELECT")
		case "INSERT", "UPDATE":
			grant.WriteColumns = append(grant.WriteColumns, row[3])
			grant.Operations = appendUnique(grant.Operations, row[2])
		}
		result[key] = grant
	}
	return result
}

func normalizeDefault(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "::jsonb", "")
	value = strings.ReplaceAll(value, "'", "")
	value = strings.ReplaceAll(value, " ", "")
	return value
}

func normalizePredicate(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "::text", "")
	value = strings.ReplaceAll(value, "(", "")
	value = strings.ReplaceAll(value, ")", "")
	return strings.Join(strings.Fields(value), " ")
}

func normalizeExpression(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "check")
	value = strings.ReplaceAll(value, "::text", "")
	value = strings.ReplaceAll(value, "(", "")
	value = strings.ReplaceAll(value, ")", "")
	return strings.Join(strings.Fields(value), " ")
}

func appendUnique(values []string, value string) []string {
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

func sameSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := append([]string(nil), left...)
	rightCopy := append([]string(nil), right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	for index := range leftCopy {
		if !strings.EqualFold(leftCopy[index], rightCopy[index]) {
			return false
		}
	}
	return true
}

func writeSummary(t *testing.T, shard *pgtest.Shard, name string, details map[string]any) {
	t.Helper()
	record := map[string]any{
		"suite":          name,
		"image":          shard.ImageTag,
		"digest":         shard.ImageDigest,
		"server_version": shard.ServerVersion,
		"database":       shard.Database,
	}
	maps.Copy(record, details)
	if err := pgtest.WriteEvidence(pgtest.EvidenceDir(), name+".json", record); err != nil {
		t.Logf("evidence %s: %v", name, err)
	}
}
