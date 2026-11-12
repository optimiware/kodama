package postgres

import (
	"fmt"
	"strings"

	"github.com/optimiware/kodama/internal/shell/store"
)

// Migration is one sequentially numbered schema change. SQL is applied exactly
// once by the migration ledger; an applied migration is never rewritten.
type Migration struct {
	Name string
	SQL  string
}

// eventPublishSequence is the durable ordering cursor the retrying event
// publisher reads. events.publish_sequence defaults to its next value so a
// committed publication intent always has a total order. The name is suffixed
// because the schema already names the publish_sequence uniqueness constraint
// events_publish_sequence, and that constraint's backing index would collide
// with a sequence of the same name in the schema namespace.
const eventPublishSequence = "events_publish_sequence_seq"

// migrationSet is rendered once from the schema declaration. Rendering is pure
// and deterministic, so Migrations() is stable across calls.
var migrationSet = buildMigrations()

// Migrations returns the migration set in application order. Each call returns
// a copy so a consumer cannot mutate the set.
func Migrations() []Migration {
	migrations := make([]Migration, len(migrationSet))
	copy(migrations, migrationSet)
	return migrations
}

// buildMigrations renders the baseline schema from the declaration data. A
// later schema change is appended as a new migration; these six are not
// regenerated once a release has applied them.
func buildMigrations() []Migration {
	design := store.SchemaDesign()
	tables := migrationTableOrder(design.Tables)
	return []Migration{
		{Name: "0001_roles.sql", SQL: renderRolesMigration(design)},
		{Name: "0002_tables.sql", SQL: renderTablesMigration(design, tables)},
		{Name: "0003_constraints.sql", SQL: renderConstraintsMigration(tables)},
		{Name: "0004_indexes.sql", SQL: renderIndexesMigration(tables)},
		{Name: "0005_row_level_security.sql", SQL: renderSecurityMigration(design)},
		{Name: "0006_privileges.sql", SQL: renderPrivilegesMigration(design)},
	}
}

// migrationTableOrder returns the schema tables so every referenced table is
// created before the table that references it. The order is deterministic: the
// first not-yet-placed table in schema declaration order whose foreign-key
// targets are already placed wins.
func migrationTableOrder(tables []store.Table) []store.Table {
	placed := make(map[string]bool, len(tables))
	order := make([]store.Table, 0, len(tables))
	for len(order) < len(tables) {
		progressed := false
		for _, table := range tables {
			if placed[table.Name] {
				continue
			}
			ready := true
			for _, key := range table.ForeignKeys {
				if !placed[key.References] {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			order = append(order, table)
			placed[table.Name] = true
			progressed = true
			break
		}
		if !progressed {
			// A dependency cycle cannot resolve. Keep the remaining tables in
			// declaration order so the generated set stays deterministic and the
			// static dependency check reports the cycle instead of hanging.
			for _, table := range tables {
				if !placed[table.Name] {
					order = append(order, table)
					placed[table.Name] = true
				}
			}
		}
	}
	return order
}

// renderRolesMigration renders the migration owner, the runtime role and the
// system roles. The owner role owns every schema object and is never a runtime
// credential. Every other role is a non-owner, non-superuser login; none can
// bypass row-level security.
func renderRolesMigration(design store.Schema) string {
	var builder strings.Builder
	builder.WriteString("-- Migration 0001: migration-owner, runtime and system roles.\n")
	builder.WriteString("-- The owner role owns every schema object and is never a runtime\n")
	builder.WriteString("-- credential; migrations are applied by a database administrator that\n")
	builder.WriteString("-- assumes it. Every other role is a non-owner, non-superuser login that\n")
	builder.WriteString("-- cannot bypass row-level security.\n")
	builder.WriteString(fmt.Sprintf("CREATE ROLE %s NOLOGIN NOBYPASSRLS NOSUPERUSER NOCREATEDB NOCREATEROLE;\n", design.OwnerRole))
	builder.WriteString(fmt.Sprintf("CREATE ROLE %s LOGIN NOINHERIT NOBYPASSRLS NOSUPERUSER NOCREATEDB NOCREATEROLE;\n", design.RuntimeRole))
	builder.WriteString(fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s;\n", design.RuntimeRole))
	for _, attributes := range design.RoleAttributes {
		if err := attributes.Validate(); err != nil {
			panic("postgres: invalid system role attributes: " + err.Error())
		}
		flags := "LOGIN"
		if attributes.NoInherit {
			flags += " NOINHERIT"
		} else {
			flags += " INHERIT"
		}
		if attributes.NoBypassRLS {
			flags += " NOBYPASSRLS"
		} else {
			flags += " BYPASSRLS"
		}
		flags += " NOSUPERUSER NOCREATEDB NOCREATEROLE"
		builder.WriteString(fmt.Sprintf("CREATE ROLE %s %s;\n", attributes.Name, flags))
		builder.WriteString(fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s;\n", attributes.Name))
		for _, membership := range attributes.Memberships {
			builder.WriteString(fmt.Sprintf("GRANT %s TO %s;\n", membership, attributes.Name))
		}
	}
	return builder.String()
}

// renderTablesMigration renders the event publication sequence and every table
// in dependency order.
func renderTablesMigration(design store.Schema, tables []store.Table) string {
	var builder strings.Builder
	builder.WriteString("-- Migration 0002: the event publication sequence and the relational record\n")
	builder.WriteString("-- families in dependency order.\n")
	builder.WriteString(fmt.Sprintf("CREATE SEQUENCE %s;\n", eventPublishSequence))
	builder.WriteString(fmt.Sprintf("ALTER SEQUENCE %s OWNER TO %s;\n", eventPublishSequence, design.OwnerRole))
	for _, table := range tables {
		builder.WriteString(renderCreateTable(table))
	}
	return builder.String()
}

func renderCreateTable(table store.Table) string {
	lines := make([]string, 0, len(table.Columns)+1)
	for _, column := range table.Columns {
		lines = append(lines, "    "+renderColumnDefinition(table, column))
	}
	lines = append(lines, "    PRIMARY KEY ("+strings.Join(table.PrimaryKey, ", ")+")")
	return fmt.Sprintf("CREATE TABLE %s (\n%s\n);\n", table.Name, strings.Join(lines, ",\n"))
}

// renderColumnDefinition renders one column. Identifiers are emitted unquoted
// because every declared name is a lowercase ASCII identifier that PostgreSQL
// accepts as a column name; the publish-sequence default is the one durable
// column default the schema design requires.
func renderColumnDefinition(table store.Table, column store.Column) string {
	definition := column.Name + " " + strings.ToUpper(column.Type)
	if !column.Nullable {
		definition += " NOT NULL"
	}
	if table.Name == "events" && column.Name == "publish_sequence" {
		definition += fmt.Sprintf(" DEFAULT nextval('%s')", eventPublishSequence)
	} else if column.Default != "" {
		definition += " DEFAULT " + renderColumnDefault(column)
	}
	return definition
}

// renderConstraintsMigration renders the composite ownership foreign keys,
// uniqueness rules and row-shape checks as separate ALTER TABLE statements after
// every table exists, so declaration order can never leave a reference
// unresolved. It emits all UNIQUE/PRIMARY-KEY-referenced constraints first,
// then every foreign key, then every check: PostgreSQL refuses a foreign key
// whose referenced unique constraint does not exist yet (SQLSTATE 42830), and
// declaration order does not guarantee that.
func renderConstraintsMigration(tables []store.Table) string {
	var builder strings.Builder
	builder.WriteString("-- Migration 0003: uniqueness, ownership foreign keys and row-shape checks.\n")
	for _, table := range tables {
		for _, unique := range table.Unique {
			nulls := ""
			if unique.NullsNotDistinct {
				nulls = " NULLS NOT DISTINCT"
			}
			builder.WriteString(fmt.Sprintf(
				"ALTER TABLE %s ADD CONSTRAINT %s UNIQUE%s (%s);\n",
				table.Name, unique.Name, nulls, strings.Join(unique.Columns, ", "),
			))
		}
	}
	for _, table := range tables {
		for _, key := range table.ForeignKeys {
			builder.WriteString(fmt.Sprintf(
				"ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s);\n",
				table.Name, key.Name,
				strings.Join(key.Columns, ", "), key.References,
				strings.Join(key.ReferencedColumns, ", "),
			))
		}
	}
	for _, table := range tables {
		for _, check := range table.Checks {
			builder.WriteString(fmt.Sprintf(
				"ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s);\n",
				table.Name, check.Name, check.Expression,
			))
		}
	}
	return builder.String()
}

func renderIndexesMigration(tables []store.Table) string {
	var builder strings.Builder
	builder.WriteString("-- Migration 0004: queryable and uniqueness indexes.\n")
	for _, table := range tables {
		for _, index := range table.Indexes {
			unique := ""
			if index.Unique {
				unique = "UNIQUE "
			}
			builder.WriteString(fmt.Sprintf(
				"CREATE %sINDEX %s ON %s (%s)",
				unique, index.Name, table.Name, strings.Join(index.Columns, ", "),
			))
			if index.Predicate != "" {
				builder.WriteString(" WHERE " + index.Predicate)
			}
			builder.WriteString(";\n")
		}
	}
	return builder.String()
}

// renderSecurityMigration enables and forces row-level security on every scoped
// table, installs the scope policy that reads the transaction-local settings,
// hands every table to the migration owner and installs the declared
// system-role policies.
func renderSecurityMigration(design store.Schema) string {
	var builder strings.Builder
	builder.WriteString("-- Migration 0005: forced row-level security, scope policies, ownership and\n")
	builder.WriteString("-- the system-role policies.\n")
	for _, table := range design.Tables {
		if table.RowLevelSecurity {
			builder.WriteString(fmt.Sprintf("ALTER TABLE %s ENABLE ROW LEVEL SECURITY;\n", table.Name))
			if table.ForceRowLevelSecurity {
				builder.WriteString(fmt.Sprintf("ALTER TABLE %s FORCE ROW LEVEL SECURITY;\n", table.Name))
			}
			builder.WriteString(renderPolicy(table) + "\n")
		}
		builder.WriteString(fmt.Sprintf("ALTER TABLE %s OWNER TO %s;\n", table.Name, design.OwnerRole))
	}
	for _, policy := range design.Policies {
		if err := policy.Validate(); err != nil {
			panic("postgres: invalid system policy: " + err.Error())
		}
		builder.WriteString(renderPolicyStatement(policy) + "\n")
	}
	return builder.String()
}

// renderPolicy renders the one scope policy for a table. The policy reads the
// transaction-local tenant and project settings, so a runtime role without RLS
// bypass can only see rows in the caller's tenant. Within the tenant a
// project-scoped transaction additionally sees only its own project's rows plus
// the tenant-wide rows (NULL project), because tenant-wide memberships, service
// accounts and secrets are current facts a project-scoped transaction must still
// read. A tenant-scoped transaction sees every row of its tenant.
func renderPolicy(table store.Table) string {
	tenant := fmt.Sprintf("current_setting('%s', true)", scopeTenantSetting)
	project := fmt.Sprintf("current_setting('%s', true)", scopeProjectSetting)
	var expression string
	switch {
	case table.Name == "tenants":
		expression = "id = " + tenant
	case !tableHasColumn(table, "project_id"):
		expression = "tenant_id = " + tenant
	default:
		expression = fmt.Sprintf(
			"tenant_id = %s AND (project_id IS NULL OR COALESCE(%s, '') = '' OR project_id = %s)",
			tenant, project, project,
		)
	}
	return fmt.Sprintf("CREATE POLICY %s_scope ON %s FOR ALL USING (%s);", table.Name, table.Name, expression)
}

func tableHasColumn(table store.Table, name string) bool {
	for _, column := range table.Columns {
		if column.Name == name {
			return true
		}
	}
	return false
}

// renderPrivilegesMigration renders the table and sequence privileges. The
// runtime role is confined to its scope by row-level security, not by a table
// grant. Each system role receives only its declared grants. The controller
// also needs USAGE on the publication sequence, because events.publish_sequence
// defaults to its next value and the controller inserts event rows.
func renderPrivilegesMigration(design store.Schema) string {
	var builder strings.Builder
	builder.WriteString("-- Migration 0006: runtime and system-role privileges. Row-level security,\n")
	builder.WriteString("-- not a table grant, is what confines the runtime role to its scope.\n")
	builder.WriteString(fmt.Sprintf("GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %s;\n", design.RuntimeRole))
	builder.WriteString(fmt.Sprintf("GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %s;\n", design.RuntimeRole))
	grants := make([]store.Grant, 0)
	grants = append(grants, store.ControllerGrants()...)
	grants = append(grants, store.PublisherGrants()...)
	grants = append(grants, store.AuthGrants()...)
	for _, grant := range grants {
		if err := grant.Validate(); err != nil {
			panic("postgres: invalid system grant: " + err.Error())
		}
		for _, operation := range grant.Operations {
			columns := grantColumns(grant, operation)
			statement := fmt.Sprintf("GRANT %s", strings.ToUpper(string(operation)))
			if len(columns) > 0 {
				statement += " (" + strings.Join(columns, ", ") + ")"
			}
			statement += fmt.Sprintf(" ON %s TO %s;\n", grant.Table, grant.Role)
			builder.WriteString(statement)
		}
	}
	builder.WriteString(fmt.Sprintf("GRANT USAGE ON SEQUENCE %s TO %s;\n", eventPublishSequence, store.ControllerRole))
	return builder.String()
}

// renderColumnDefault renders a declared column default as a SQL literal. Text
// and jsonb defaults are quoted so the declaration's "[]" becomes the valid SQL
// literal '[]'.
func renderColumnDefault(column store.Column) string {
	switch column.Type {
	case "text", "jsonb":
		return sqlLiteral(column.Default)
	default:
		return column.Default
	}
}

// grantColumns returns the column restriction of one operation. An empty list
// means the grant covers the whole table.
func grantColumns(grant store.Grant, operation store.Operation) []string {
	switch operation {
	case store.OperationSelect:
		return grant.SelectColumns
	case store.OperationInsert, store.OperationUpdate:
		return grant.WriteColumns
	default:
		return nil
	}
}

// renderPolicyStatement renders one declared system-role policy. The portable
// predicate vocabulary is translated to plain SQL here; no policy is invented
// or widened. INSERT policies are admitted by WITH CHECK only, matching how
// PostgreSQL evaluates them.
func renderPolicyStatement(policy store.Policy) string {
	name := fmt.Sprintf("%s_%s_%s", policy.Role, policy.Table, policy.Command)
	statement := fmt.Sprintf(
		"CREATE POLICY %s ON %s AS PERMISSIVE FOR %s TO %s",
		name, policy.Table, strings.ToUpper(string(policy.Command)), policy.Role,
	)
	if policy.Command != store.PolicyInsert {
		statement += " USING (" + renderPolicyPredicate(policy, policy.Using) + ")"
	}
	if policy.WithCheck != "" {
		statement += " WITH CHECK (" + renderPolicyPredicate(policy, policy.WithCheck) + ")"
	}
	return statement + ";"
}

// renderPolicyPredicate translates one portable predicate name to SQL.
func renderPolicyPredicate(policy store.Policy, predicate string) string {
	switch predicate {
	case store.PredicateAny:
		return "true"
	case store.PredicateScopeMatchesTxnScope:
		return fmt.Sprintf(
			"tenant_id = current_setting('%s', true) AND project_id = current_setting('%s', true)",
			scopeTenantSetting, scopeProjectSetting,
		)
	case store.PredicateColumnInSet:
		values := make([]string, len(policy.Values))
		for index, value := range policy.Values {
			values[index] = sqlLiteral(value)
		}
		return fmt.Sprintf("%s IN (%s)", policy.Column, strings.Join(values, ", "))
	case store.PredicateExactRow:
		return fmt.Sprintf("%s = current_setting('%s', true)", policy.Column, scopeCredentialSetting)
	default:
		panic("postgres: unknown frozen policy predicate " + predicate)
	}
}

// sqlLiteral quotes one declaration-data string as a SQL literal. Values are
// declaration data, never caller input.
func sqlLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
