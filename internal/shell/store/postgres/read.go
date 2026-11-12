package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// ReadRow reads one row by identity inside the caller's scope. The query filters
// on tenant/project ownership in addition to the row id, and the decoded row is
// checked again after the read so a wrong predicate cannot return a cross-scope
// row.
func ReadRow(ctx context.Context, tx store.Transaction, scope tenancy.Scope, table, id string) (store.Row, error) {
	meta, ok := lookupTable(table)
	if !ok {
		return store.Row{}, faultInvalid(fmt.Sprintf("unknown table %q", table))
	}
	if err := scope.Validate(); err != nil {
		return store.Row{}, err
	}
	if !validIdentity(id) {
		return store.Row{}, faultInvalid(fmt.Sprintf("row id %q is not a valid identity", id))
	}
	if err := checkActiveScope(tx, scope); err != nil {
		return store.Row{}, err
	}
	query, err := scopedReadQuery(meta, scope, id)
	if err != nil {
		return store.Row{}, err
	}
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return store.Row{}, err
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return store.Row{}, err
		}
		return store.Row{}, faultNotFound(fmt.Sprintf("%s %q was not found in scope", table, id))
	}
	values, err := scanColumns(meta, meta.columnOrder, rows)
	if err != nil {
		return store.Row{}, err
	}
	if err := rows.Err(); err != nil {
		return store.Row{}, err
	}
	if err := checkRowScope(meta, scope, values); err != nil {
		return store.Row{}, err
	}
	return store.Row{Table: table, Values: values}, nil
}

// scopedReadQuery renders the parameterized scoped read. The tenant (and, when
// the table is project-scoped, the project) is always part of the predicate;
// project authority is never inferred from the row id alone.
//
// A project-scoped transaction reads its own project's rows: a table that only
// allows a nullable project column is read with project_id IS NULL in a
// tenant-scoped transaction and with the exact project otherwise. Tenant-wide
// facts are read through a tenant-scoped transaction, never by widening a
// project scope.
func scopedReadQuery(meta *tableMeta, scope tenancy.Scope, id string) (store.Query, error) {
	if meta.hasProject && meta.projectRequired && scope.TenantScoped() {
		return store.Query{}, faultInvalid(fmt.Sprintf("table %q is project scoped; a tenant-scope read is not allowed", meta.table.Name))
	}
	args := make([]any, 0, 3)
	predicates := make([]string, 0, 3)
	add := func(column string, value any) {
		args = append(args, value)
		predicates = append(predicates, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	if meta.hasTenant {
		add("tenant_id", string(scope.TenantID))
	}
	if meta.hasProject {
		if scope.TenantScoped() {
			predicates = append(predicates, "project_id IS NULL")
		} else {
			add("project_id", string(scope.ProjectID))
		}
	}
	add("id", id)
	return store.Query{
		SQL: fmt.Sprintf(
			"SELECT %s FROM %s WHERE %s",
			strings.Join(meta.columnOrder, ", "), meta.table.Name, strings.Join(predicates, " AND "),
		),
		Args: args,
	}, nil
}

// checkRowScope refuses a decoded row that carries a different owner than the
// transaction scope. It is the adapter's second line of defence behind the SQL
// predicate and the database's RLS policy.
func checkRowScope(meta *tableMeta, scope tenancy.Scope, values map[string]any) error {
	if meta.hasTenant {
		tenant, _ := values["tenant_id"].(string)
		if tenant != string(scope.TenantID) {
			return faultForbidden(fmt.Sprintf("row in %q belongs to another tenant", meta.table.Name))
		}
	}
	switch {
	case meta.hasProject && scope.TenantScoped():
		project, _ := values["project_id"].(string)
		if project != "" {
			return faultForbidden(fmt.Sprintf("row in %q belongs to a project but the transaction is tenant scoped", meta.table.Name))
		}
	case meta.hasProject:
		project, _ := values["project_id"].(string)
		if project != string(scope.ProjectID) {
			return faultForbidden(fmt.Sprintf("row in %q belongs to another project", meta.table.Name))
		}
	default:
		// The tenant and project root rows carry their own identity, so the row
		// identity itself must be inside the transaction scope.
		identity, _ := values["id"].(string)
		if err := checkRowIdentityScope(meta, scope, identity); err != nil {
			return err
		}
	}
	return nil
}

// checkRowIdentityScope ties the tenant and project root rows to the transaction
// scope. Their row identity is the tenant or project identity, so a row for
// another tenant (or, for a project-scoped transaction, another project) is out
// of scope even when the tenant matches.
func checkRowIdentityScope(meta *tableMeta, scope tenancy.Scope, identity string) error {
	switch meta.table.Name {
	case "tenants":
		if identity != string(scope.TenantID) {
			return faultForbidden("tenant row belongs to another tenant")
		}
	case "projects":
		if scope.ProjectScoped() && identity != string(scope.ProjectID) {
			return faultForbidden("project row belongs to another project")
		}
	}
	return nil
}

// scanColumns scans the current row into the supplied column list and returns
// the decoded values. Column order follows the list, which is also the SELECT
// list the adapter issued.
func scanColumns(meta *tableMeta, columns []string, rows store.Rows) (map[string]any, error) {
	destinations := make([]any, len(columns))
	resolvers := make([]func() any, len(columns))
	for position, name := range columns {
		column, ok := meta.columns[name]
		if !ok {
			return nil, faultInternal(fmt.Sprintf("table %q has no column %q", meta.table.Name, name))
		}
		destinations[position], resolvers[position] = scanTarget(column)
	}
	if err := rows.Scan(destinations...); err != nil {
		return nil, fault.Wrap(fault.InternalFailure, fmt.Sprintf("scan row from %q", meta.table.Name), err)
	}
	values := make(map[string]any, len(columns))
	for position, name := range columns {
		values[name] = resolvers[position]()
	}
	return values, nil
}

// scanTarget returns the scan destination and the value accessor for one column
// type. Nullable columns keep NULL distinct from the zero value.
func scanTarget(column store.Column) (any, func() any) {
	switch column.Type {
	case "text":
		if column.Nullable {
			target := new(sql.NullString)
			return target, func() any {
				if !target.Valid {
					return nil
				}
				return target.String
			}
		}
		target := new(string)
		return target, func() any { return *target }
	case "bigint", "integer":
		if column.Nullable {
			target := new(sql.NullInt64)
			return target, func() any {
				if !target.Valid {
					return nil
				}
				return target.Int64
			}
		}
		target := new(int64)
		return target, func() any { return *target }
	case "bytea", "jsonb":
		target := new([]byte)
		return target, func() any {
			if *target == nil {
				return nil
			}
			return append([]byte(nil), (*target)...)
		}
	case "boolean":
		if column.Nullable {
			target := new(sql.NullBool)
			return target, func() any {
				if !target.Valid {
					return nil
				}
				return target.Bool
			}
		}
		target := new(bool)
		return target, func() any { return *target }
	case "timestamptz":
		if column.Nullable {
			target := new(sql.NullTime)
			return target, func() any {
				if !target.Valid {
					return nil
				}
				return target.Time
			}
		}
		target := new(time.Time)
		return target, func() any { return *target }
	default:
		target := new(any)
		return target, func() any { return *target }
	}
}
