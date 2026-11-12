// Package store declares the adapter-independent persistence seams and the
// relational schema design shared by every persistence adapter.
//
// Executor is the neutral structured seam: a caller submits one complete Unit
// (locks, business writes and guarded deletes, receipt, audit rows and event
// publication intent) and the adapter applies it atomically. SQL text stays
// inside a concrete adapter, so a non-SQL adapter can implement the same
// commit-unit semantics without importing the PostgreSQL adapter.
//
// Scanner is the separate scoped read seam for multi-row queries and keyset
// pages. It is not a repository and it exposes no generic
// Save/Update/SetState/Persist/Store method: it only reads rows that are already
// inside the caller's scope. A caller names a table, equality and strict range
// filters, an order and an opaque keyset cursor; the adapter always applies the
// transaction-local scope, validates the request against the declared schema and
// returns one bounded deterministic page. The scope columns are applied by the
// adapter and can never be widened or omitted by the caller.
//
// The lower-level SQL seam remains for SQL adapters: a caller begins exactly one
// transaction with its typed ownership scope, performs the complete commit unit
// through that transaction and commits or rolls back. There is no generic
// Save/Update/SetState/Persist/Store/Repository method and no split business
// write, because a handoff, settlement, receipt, revocation or publication
// intent that can commit apart from its business change is not atomic. A row
// removal is the explicit, guarded Write.Delete, never an unguarded delete.
//
// Scope is established transaction-locally by the adapter from verified caller
// context; it is not passed on every statement and must reset when the
// transaction completes so a pooled connection cannot leak a prior scope.
//
// SchemaDesign is declaration data only. It declares the reviewed relational
// shape (tables, forced RLS, ownership foreign keys, uniqueness, state checks,
// the one-active-action index, lock order and migration naming) that the
// concrete adapters implement; this package writes no migration file.
package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// ScopedConnection begins one transaction whose ownership scope is established
// from verified caller context. A connection is not a scope itself: every
// Begin gets its own transaction and its own transaction-local scope.
type ScopedConnection interface {
	Begin(context.Context, tenancy.Scope) (Transaction, error)
}

// Transaction is one scoped atomic commit unit.
//
// Exec and Query are the only statement paths. Commit applies the whole unit;
// Rollback leaves no partial rows. There is intentionally no generic
// Save/Update/SetState/Persist/Store/Repository method.
type Transaction interface {
	Exec(context.Context, Statement) (Result, error)
	Query(context.Context, Query) (Rows, error)
	Commit(context.Context) error
	Rollback(context.Context) error
}

// Statement is one parameterized write statement. SQL text is fixed by the
// adapter; values travel in Args, never interpolated into the text.
type Statement struct {
	SQL  string
	Args []any
}

// Query is one parameterized read statement.
type Query struct {
	SQL  string
	Args []any
}

// Result is the adapter-reported outcome of one statement.
type Result struct {
	RowsAffected int64
}

// Rows iterates one bounded query result. Scan copies the current row into the
// supplied destinations; Err reports iteration failure and Close releases the
// result. Callers must Close every Rows value they obtain.
type Rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}

// MaxQueryLimit is the largest page a Scanner may return. Limit is a required
// positive bound: zero and any value above MaxQueryLimit are refused before a
// statement runs, so an unbounded scan is impossible.
const MaxQueryLimit = 500

// MaxQueryCursorLength bounds the opaque keyset cursor token. The token itself
// is adapter-defined; the bound and the printable-token shape are shared so
// every adapter refuses the same malformed cursor before decoding it.
const MaxQueryCursorLength = 4096

// Scanner is the separate scoped multi-row read seam. It is deliberately not a
// repository: there is no generic Save/Update/SetState/Persist/Store method and
// Query never writes, locks or publishes. A caller submits one complete read
// request and the adapter:
//
//   - opens exactly one read-only transaction with the same transaction-local
//     scope as Executor.Read and completes it so the scope resets;
//   - always applies the scope columns itself: a project scope reads the exact
//     project, a tenant-wide scope reads tenant-wide rows only (project_id IS
//     NULL), and a project-required table refuses a tenant-wide scope. A caller
//     equality on tenant_id/project_id can only narrow that predicate; it can
//     never widen or omit it;
//   - scopes the two root tables by their own identity because neither carries
//     the other's scope column: a tenants row is visible only where its id is
//     the scope tenant, and a projects row is visible only where its tenant_id
//     is the scope tenant and, for a project scope, only where its id is the
//     scope project. A tenant-wide scope therefore reads all of its own tenant's
//     projects and no other tenant's tenants or projects rows. A caller equality
//     on id can only narrow that predicate; it can never widen or omit it;
//   - validates the table and every column against the SchemaDesign
//     allow-list, plus Limit, duplicate columns and cursor shape, before any
//     statement runs. A nullable column is refused as a Range or OrderBy column
//     (invalid_input) because NULL placement and the NULL keyset comparison are
//     unspecified; Equals on a nullable column is allowed, but a nil equality is
//     still refused;
//   - binds equality and range values as parameters only, never as text;
//   - orders by the requested columns and always appends the row id as the
//     final tie-breaker, so equal order values cannot reorder rows;
//   - returns one deterministic page with HasMore and an opaque Next cursor
//     built from the order columns plus the row id. Keyset pagination is stable
//     under concurrent inserts: rows strictly after the cursor are returned and
//     rows before it are never repeated;
//   - refuses a cursor that was produced for another scope (forbidden) or for
//     another table or order (invalid_input).
//
// Errors use the existing fault classes: an unknown table/column, a bad limit,
// a malformed cursor or a mismatched table/order cursor is invalid_input; a
// cross-scope cursor is forbidden; a missing dependency or deadline keeps its
// existing dependency_unavailable/deadline_expired class.
type Scanner interface {
	Query(ctx context.Context, scope tenancy.Scope, query ScanQuery) (QueryPage, error)
}

// ScanQuery is one complete scoped read request. It is a description, never
// SQL: the adapter chooses the statement and binds every value as a parameter.
//
// Table and every Equals/Range/OrderBy column must be declared by
// SchemaDesign(). Equals is a set of column equality values. Range carries
// strict bounds: a row is included only when it is greater than After and less
// than Before (each bound is optional, but at least one is required). OrderBy
// names the requested ordering; the adapter appends the row id when the final
// order column is not id. After is the opaque cursor returned as QueryPage.Next
// by a previous call with the same scope, table and order. Limit is required
// and must be in 1..MaxQueryLimit. Range and OrderBy columns must be
// non-nullable: NULL placement and the NULL keyset comparison are unspecified,
// so a nullable column is refused instead of silently skipping or stalling a
// page.
type ScanQuery struct {
	Table   string
	Equals  map[string]any
	Range   []RangeFilter
	OrderBy []Order
	After   string
	Limit   int
}

// RangeFilter is one strict range bound on a single column. After is exclusive
// lower bound, Before is exclusive upper bound; nil means unbounded on that
// side. A filter with neither bound is refused because it would silently widen
// the scan.
type RangeFilter struct {
	Column string
	After  any
	Before any
}

// Order is one ordering term. Desc reverses only that column. The adapter
// appends the row id as a final ascending tie-breaker when the order does not
// already end with id, so the page order is total and deterministic.
type Order struct {
	Column string
	Desc   bool
}

// QueryPage is one bounded scanner page. Rows are deterministic and ordered by
// the requested order plus the row id. HasMore reports that at least one row
// exists beyond the page; Next is the opaque cursor for the next page and is
// non-empty exactly when HasMore is true.
type QueryPage struct {
	Rows    []Row
	Next    string
	HasMore bool
}

// Validate checks a ScanQuery against the declared schema allow-list and the
// shared bounds before an adapter runs a statement. It refuses an empty or
// unknown table, an empty/unknown/nil equality, an empty or unknown range or
// order column, a duplicate range/order column, a range filter with no bound,
// a nullable range or order column (NULL placement and the NULL keyset
// comparison are unspecified), a Limit outside 1..MaxQueryLimit and a malformed
// cursor token.
func (query ScanQuery) Validate() error {
	if query.Table == "" {
		return fault.New(fault.InvalidInput, "query table is empty")
	}
	table, ok := schemaTable(query.Table)
	if !ok {
		return fault.New(fault.InvalidInput, fmt.Sprintf("query table %q is not in the frozen schema", query.Table))
	}
	if query.Limit < 1 || query.Limit > MaxQueryLimit {
		return fault.New(fault.InvalidInput, fmt.Sprintf("query limit %d is outside 1..%d", query.Limit, MaxQueryLimit))
	}
	for column, value := range query.Equals {
		if column == "" {
			return fault.New(fault.InvalidInput, "query equality column is empty")
		}
		if !tableHasColumn(table, column) {
			return fault.New(fault.InvalidInput, fmt.Sprintf("query equality column %q is not declared by table %q", column, table.Name))
		}
		if value == nil {
			return fault.New(fault.InvalidInput, fmt.Sprintf("query equality column %q has a nil value", column))
		}
	}
	rangeColumns := make(map[string]bool, len(query.Range))
	for _, filter := range query.Range {
		if err := filter.Validate(); err != nil {
			return err
		}
		column, ok := tableColumn(table, filter.Column)
		if !ok {
			return fault.New(fault.InvalidInput, fmt.Sprintf("query range column %q is not declared by table %q", filter.Column, table.Name))
		}
		if column.Nullable {
			return fault.New(fault.InvalidInput, fmt.Sprintf("query range column %q is nullable; NULL keyset comparison is unspecified", filter.Column))
		}
		if rangeColumns[filter.Column] {
			return fault.New(fault.InvalidInput, fmt.Sprintf("query range column %q is repeated", filter.Column))
		}
		rangeColumns[filter.Column] = true
	}
	orderColumns := make(map[string]bool, len(query.OrderBy))
	for index, order := range query.OrderBy {
		if err := order.Validate(); err != nil {
			return err
		}
		column, ok := tableColumn(table, order.Column)
		if !ok {
			return fault.New(fault.InvalidInput, fmt.Sprintf("query order column %q is not declared by table %q", order.Column, table.Name))
		}
		if column.Nullable {
			return fault.New(fault.InvalidInput, fmt.Sprintf("query order column %q is nullable; NULL placement is unspecified", order.Column))
		}
		if orderColumns[order.Column] {
			return fault.New(fault.InvalidInput, fmt.Sprintf("query order column %q is repeated", order.Column))
		}
		if order.Column == "id" && index != len(query.OrderBy)-1 {
			return fault.New(fault.InvalidInput, "query order must end with the row id")
		}
		orderColumns[order.Column] = true
	}
	if err := validateQueryCursor(query.After); err != nil {
		return err
	}
	return nil
}

// Validate checks one range filter's shared shape. The owning ScanQuery.Validate
// additionally proves the column exists in the requested table.
func (filter RangeFilter) Validate() error {
	if filter.Column == "" {
		return fault.New(fault.InvalidInput, "range filter column is empty")
	}
	if filter.After == nil && filter.Before == nil {
		return fault.New(fault.InvalidInput, fmt.Sprintf("range filter on %q has no bound", filter.Column))
	}
	return nil
}

// Validate checks one order term's shared shape. The owning ScanQuery.Validate
// additionally proves the column exists in the requested table.
func (order Order) Validate() error {
	if order.Column == "" {
		return fault.New(fault.InvalidInput, "order column is empty")
	}
	return nil
}

func validateQueryCursor(cursor string) error {
	if cursor == "" {
		return nil
	}
	if len(cursor) > MaxQueryCursorLength {
		return fault.New(fault.InvalidInput, fmt.Sprintf("query cursor is longer than %d bytes", MaxQueryCursorLength))
	}
	for index := 0; index < len(cursor); index++ {
		character := cursor[index]
		if character <= ' ' || character >= 0x7f {
			return fault.New(fault.InvalidInput, "query cursor contains a control, whitespace or non-ASCII byte")
		}
	}
	return nil
}

func schemaTable(name string) (Table, bool) {
	for _, table := range SchemaDesign().Tables {
		if table.Name == name {
			return table, true
		}
	}
	return Table{}, false
}

func tableHasColumn(table Table, name string) bool {
	_, ok := tableColumn(table, name)
	return ok
}

func tableColumn(table Table, name string) (Column, bool) {
	for _, column := range table.Columns {
		if column.Name == name {
			return column, true
		}
	}
	return Column{}, false
}

// Column is one table column in the schema design.
//
// Default is the reviewed SQL default expression, empty when the column has
// none. It is declaration data only: an adapter that renders migrations must
// emit it, and a writer still supplies the column explicitly.
type Column struct {
	Name     string
	Type     string
	Nullable bool
	Default  string
}

// ForeignKey is one declared ownership or reference constraint. Columns lists
// the local columns; ReferencedColumns lists the target columns in the same
// order.
type ForeignKey struct {
	Name              string
	Columns           []string
	References        string
	ReferencedColumns []string
}

// UniqueConstraint is one declared uniqueness rule. NullsNotDistinct is set for
// nullable project scope so a tenant-wide row cannot be duplicated by NULL
// comparison semantics.
type UniqueConstraint struct {
	Name             string
	Columns          []string
	NullsNotDistinct bool
}

// Index is one declared queryable index. Predicate is the partial-index
// predicate, empty for a full index.
type Index struct {
	Name      string
	Columns   []string
	Unique    bool
	Predicate string
}

// Check is one declared row-shape constraint. Expression is the SQL predicate
// the migration owner must render; it is data, not an executed statement.
type Check struct {
	Name       string
	Expression string
}

// Table is one declared table with its primary key, columns, constraints,
// indexes and RLS posture. Scoped tables enable and force row-level security so
// the runtime role cannot bypass it.
//
// PrimaryKey is the declared primary-key column set. It is always present so a
// referenced table has an exact identity for its id column and every composite
// foreign-key target is backed by a primary key or unique constraint.
type Table struct {
	Name                  string
	PrimaryKey            []string
	Columns               []Column
	ForeignKeys           []ForeignKey
	Unique                []UniqueConstraint
	Indexes               []Index
	Checks                []Check
	RowLevelSecurity      bool
	ForceRowLevelSecurity bool
}

// Schema is the complete reviewed schema design. RuntimeRole is the non-owner
// role used by the product; OwnerRole owns migrations and is never used at
// runtime. ControllerRole, PublisherRole and AuthRole are the three additional
// non-owner, NOBYPASSRLS system roles used by the cross-scope controller seam,
// the event outbox and the pre-authentication credential lookup. LockOrder is
// the single order every multi-table commit acquires locks in.
// MigrationNamePattern and MigrationSequenceStart pin reviewed sequential
// migration file names.
//
// RoleAttributes pins each system role's security attributes as declaration
// data. Policies pins every system-role row-level-security policy as role,
// table, command and portable USING/WITH CHECK predicate data, so an adapter
// renders the reviewed policy instead of inventing a scope predicate.
type Schema struct {
	Name                   string
	RuntimeRole            string
	OwnerRole              string
	ControllerRole         string
	PublisherRole          string
	AuthRole               string
	RoleAttributes         []RoleAttributes
	Policies               []Policy
	Tables                 []Table
	LockOrder              []string
	MigrationNamePattern   string
	MigrationSequenceStart int
}

// PolicyCommand is one SQL command a row-level-security policy applies to.
type PolicyCommand string

const (
	// PolicySelect is a SELECT policy.
	PolicySelect PolicyCommand = "select"
	// PolicyInsert is an INSERT policy.
	PolicyInsert PolicyCommand = "insert"
	// PolicyUpdate is an UPDATE policy.
	PolicyUpdate PolicyCommand = "update"
	// PolicyDelete is a DELETE policy. No system role declares one.
	PolicyDelete PolicyCommand = "delete"
)

var policyCommands = [...]PolicyCommand{PolicySelect, PolicyInsert, PolicyUpdate, PolicyDelete}

// Valid reports whether the command is one of the declared policy commands.
func (command PolicyCommand) Valid() bool {
	for _, candidate := range policyCommands {
		if command == candidate {
			return true
		}
	}
	return false
}

// Portable row-level-security predicate vocabulary. The vocabulary is data, not
// SQL text: an adapter renders each name with its own quoted statement.
const (
	// PredicateAny admits every row of the table. It is used only where an
	// explicit table grant already limits the role, such as the controller's
	// cross-scope read of its five state/event tables and the publisher's events
	// read.
	PredicateAny = "any"
	// PredicateScopeMatchesTxnScope requires the row's tenant/project scope to
	// equal the transaction-local scope the adapter established. For the
	// controller it is the owning scope Controller.Execute derived from the
	// execution row; for the runtime role it is the verified caller scope.
	PredicateScopeMatchesTxnScope = "scope_matches_txn_scope"
	// PredicateColumnInSet requires the named column value to be one of Values.
	// It pins the publisher's publish_state vocabulary, for example.
	PredicateColumnInSet = "column_in_set"
	// PredicateExactRow requires the row's named identity column to equal the
	// exact id the operation binds, so a lookup cannot list or range-scan rows.
	PredicateExactRow = "exact_row"
)

var policyPredicates = [...]string{PredicateAny, PredicateScopeMatchesTxnScope, PredicateColumnInSet, PredicateExactRow}

// Valid reports whether the predicate is one of the portable vocabulary names.
func validPolicyPredicate(predicate string) bool {
	for _, candidate := range policyPredicates {
		if predicate == candidate {
			return true
		}
	}
	return false
}

// Policy is one system-role row-level-security policy. Role, Table and Command
// name the policy; Using and WithCheck are predicate names from the portable
// vocabulary; Column and Values parameterize column_in_set/exact_row.
// WithCheck is empty for a SELECT policy. It is declaration data only: an
// adapter renders the policy, never a blanket permissive rule.
type Policy struct {
	Role      string
	Table     string
	Command   PolicyCommand
	Using     string
	WithCheck string
	Column    string
	Values    []string
}

// Validate checks that a policy names a declared system role and table, a
// declared command, vocabulary predicates and the parameter columns/values
// those predicates need.
func (policy Policy) Validate() error {
	if !isSystemRole(policy.Role) {
		return fault.New(fault.InvalidInput, fmt.Sprintf("policy role %q is not a declared system role", policy.Role))
	}
	table, ok := schemaTable(policy.Table)
	if !ok {
		return fault.New(fault.InvalidInput, fmt.Sprintf("policy table %q is not in the frozen schema", policy.Table))
	}
	if !policy.Command.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("policy command %q is not frozen", policy.Command))
	}
	if !validPolicyPredicate(policy.Using) {
		return fault.New(fault.InvalidInput, fmt.Sprintf("policy using predicate %q is not in the portable vocabulary", policy.Using))
	}
	if policy.WithCheck != "" && !validPolicyPredicate(policy.WithCheck) {
		return fault.New(fault.InvalidInput, fmt.Sprintf("policy with-check predicate %q is not in the portable vocabulary", policy.WithCheck))
	}
	for _, predicate := range []string{policy.Using, policy.WithCheck} {
		switch predicate {
		case PredicateColumnInSet:
			if policy.Column == "" || !tableHasColumn(table, policy.Column) {
				return fault.New(fault.InvalidInput, fmt.Sprintf("policy column %q is not declared by table %q", policy.Column, policy.Table))
			}
			if len(policy.Values) == 0 {
				return fault.New(fault.InvalidInput, "a column_in_set policy has no values")
			}
		case PredicateExactRow:
			if policy.Column == "" || !tableHasColumn(table, policy.Column) {
				return fault.New(fault.InvalidInput, fmt.Sprintf("policy column %q is not declared by table %q", policy.Column, policy.Table))
			}
		}
	}
	return nil
}

// RoleAttributes pins one non-owner system role's security attributes. A system
// role must be NOBYPASSRLS and NOINHERIT and must not be a member of the
// runtime or owner role, so it cannot inherit a broader authority.
type RoleAttributes struct {
	Name        string
	NoBypassRLS bool
	NoInherit   bool
	Memberships []string
}

// Validate checks the required system-role attributes.
func (attributes RoleAttributes) Validate() error {
	if !isSystemRole(attributes.Name) {
		return fault.New(fault.InvalidInput, fmt.Sprintf("role attributes name %q is not a declared system role", attributes.Name))
	}
	if !attributes.NoBypassRLS {
		return fault.New(fault.InvalidInput, fmt.Sprintf("system role %q must be NOBYPASSRLS", attributes.Name))
	}
	if !attributes.NoInherit {
		return fault.New(fault.InvalidInput, fmt.Sprintf("system role %q must be NOINHERIT", attributes.Name))
	}
	for _, membership := range attributes.Memberships {
		if membership == DefaultRuntimeRole || membership == DefaultOwnerRole {
			return fault.New(fault.InvalidInput, fmt.Sprintf("system role %q must not be a member of %q", attributes.Name, membership))
		}
	}
	return nil
}

func isSystemRole(role string) bool {
	return role == ControllerRole || role == PublisherRole || role == AuthRole
}

// SystemRoleAttributes returns the reviewed attributes of the three system
// roles: NOBYPASSRLS, NOINHERIT and no membership in the runtime or owner role.
func SystemRoleAttributes() []RoleAttributes {
	return []RoleAttributes{
		{Name: ControllerRole, NoBypassRLS: true, NoInherit: true},
		{Name: PublisherRole, NoBypassRLS: true, NoInherit: true},
		{Name: AuthRole, NoBypassRLS: true, NoInherit: true},
	}
}

// SystemPolicies returns the reviewed row-level-security policies for the three
// system roles.
//
// The controller reads its five state/event tables without a row predicate
// because Controller.Read/Scan are cross-scope, but the table grants limit it
// to exactly those tables; its INSERT/UPDATE policies require the
// transaction-local scope Controller.Execute derived from the guarded execution
// row. The events and audit INSERT policies let the controller commit
// execution-bound lifecycle event and audit rows only inside the owning scope.
// The publisher reads all events and may update only a valid publish_state. The
// auth role performs exact-row SELECT only and has no write policy.
func SystemPolicies() []Policy {
	return []Policy{
		{Role: ControllerRole, Table: "executions", Command: PolicySelect, Using: PredicateAny},
		{Role: ControllerRole, Table: "executions", Command: PolicyUpdate, Using: PredicateScopeMatchesTxnScope, WithCheck: PredicateScopeMatchesTxnScope},
		{Role: ControllerRole, Table: "actions", Command: PolicySelect, Using: PredicateAny},
		{Role: ControllerRole, Table: "actions", Command: PolicyInsert, Using: PredicateScopeMatchesTxnScope, WithCheck: PredicateScopeMatchesTxnScope},
		{Role: ControllerRole, Table: "actions", Command: PolicyUpdate, Using: PredicateScopeMatchesTxnScope, WithCheck: PredicateScopeMatchesTxnScope},
		{Role: ControllerRole, Table: "cleanup", Command: PolicySelect, Using: PredicateAny},
		{Role: ControllerRole, Table: "cleanup", Command: PolicyInsert, Using: PredicateScopeMatchesTxnScope, WithCheck: PredicateScopeMatchesTxnScope},
		{Role: ControllerRole, Table: "cleanup", Command: PolicyUpdate, Using: PredicateScopeMatchesTxnScope, WithCheck: PredicateScopeMatchesTxnScope},
		{Role: ControllerRole, Table: "events", Command: PolicySelect, Using: PredicateAny},
		{Role: ControllerRole, Table: "events", Command: PolicyInsert, Using: PredicateScopeMatchesTxnScope, WithCheck: PredicateScopeMatchesTxnScope},
		{Role: ControllerRole, Table: "audit", Command: PolicyInsert, Using: PredicateScopeMatchesTxnScope, WithCheck: PredicateScopeMatchesTxnScope},
		{Role: PublisherRole, Table: "events", Command: PolicySelect, Using: PredicateAny},
		{Role: PublisherRole, Table: "events", Command: PolicyUpdate, Using: PredicateAny, WithCheck: PredicateColumnInSet, Column: "publish_state", Values: []string{"pending", "published"}},
		{Role: AuthRole, Table: "executions", Command: PolicySelect, Using: PredicateExactRow, Column: "id"},
		{Role: AuthRole, Table: "services", Command: PolicySelect, Using: PredicateExactRow, Column: "id"},
	}
}

// Default schema roles and migration naming.
const (
	// DefaultRuntimeRole is the non-owner runtime role. It has no RLS bypass.
	DefaultRuntimeRole = "kodama_runtime"
	// DefaultOwnerRole owns migrations and is never a runtime credential.
	DefaultOwnerRole = "kodama_owner"
	// ControllerRole is the non-owner, NOBYPASSRLS role used only by the
	// cross-scope kernel-state controller seam.
	ControllerRole = "kodama_controller"
	// PublisherRole is the non-owner, NOBYPASSRLS role used only by the event
	// outbox publisher.
	PublisherRole = "kodama_publisher"
	// AuthRole is the non-owner, NOBYPASSRLS role used only by the
	// pre-authentication credential lookup.
	AuthRole = "kodama_auth"
	// MigrationNamePattern is the reviewed sequential migration name shape.
	MigrationNamePattern = `^\d{4}_[a-z0-9_]+\.sql$`
	// MigrationSequenceStart is the first migration number.
	MigrationSequenceStart = 1
)

// SchemaDesign returns the declared relational schema design. It is
// deterministic data with no I/O; the adapters translate it into queries and
// migrations.
func SchemaDesign() Schema {
	return Schema{
		Name:                   "kodama",
		RuntimeRole:            DefaultRuntimeRole,
		OwnerRole:              DefaultOwnerRole,
		ControllerRole:         ControllerRole,
		PublisherRole:          PublisherRole,
		AuthRole:               AuthRole,
		RoleAttributes:         SystemRoleAttributes(),
		Policies:               SystemPolicies(),
		LockOrder:              lockOrder(),
		MigrationNamePattern:   MigrationNamePattern,
		MigrationSequenceStart: MigrationSequenceStart,
		Tables: []Table{
			{
				Name:       "tenants",
				PrimaryKey: primaryKey("id"),
				Columns: []Column{
					column("id", "text", false),
					column("name", "text", false),
					column("version", "bigint", false),
					column("derivation_key", "jsonb", false),
					column("created_at", "timestamptz", false),
				},
				Checks: []Check{
					check("tenants_derivation_key_object", "jsonb_typeof(derivation_key) = 'object'"),
				},
				RowLevelSecurity:      true,
				ForceRowLevelSecurity: true,
			},
			{
				Name:       "projects",
				PrimaryKey: primaryKey("id"),
				Columns: []Column{
					column("id", "text", false),
					column("tenant_id", "text", false),
					column("name", "text", false),
					column("version", "bigint", false),
					column("created_at", "timestamptz", false),
				},
				ForeignKeys: []ForeignKey{
					foreignKey("projects_tenant", []string{"tenant_id"}, "tenants", []string{"id"}),
				},
				Unique: []UniqueConstraint{
					unique("projects_tenant_name", false, "tenant_id", "name"),
					unique("projects_tenant_id", false, "tenant_id", "id"),
				},
				RowLevelSecurity:      true,
				ForceRowLevelSecurity: true,
			},
			{
				Name:       "memberships",
				PrimaryKey: primaryKey("id"),
				Columns: []Column{
					column("id", "text", false),
					column("tenant_id", "text", false),
					column("project_id", "text", true),
					column("principal_id", "text", false),
					column("roles", "jsonb", false),
					column("version", "bigint", false),
					column("created_at", "timestamptz", false),
				},
				ForeignKeys: []ForeignKey{
					foreignKey("memberships_tenant", []string{"tenant_id"}, "tenants", []string{"id"}),
					foreignKey("memberships_project", []string{"tenant_id", "project_id"}, "projects", []string{"tenant_id", "id"}),
				},
				Unique: []UniqueConstraint{
					unique("memberships_scope_principal", true, "tenant_id", "project_id", "principal_id"),
				},
				RowLevelSecurity:      true,
				ForceRowLevelSecurity: true,
			},
			{
				Name:       "services",
				PrimaryKey: primaryKey("id"),
				Columns: []Column{
					column("id", "text", false),
					column("tenant_id", "text", false),
					column("project_id", "text", true),
					column("name", "text", false),
					column("state", "text", false),
					column("roles", "jsonb", false),
					column("token_nonce", "bytea", true),
					column("token_expires_at", "timestamptz", true),
					column("version", "bigint", false),
					column("created_at", "timestamptz", false),
				},
				ForeignKeys: []ForeignKey{
					foreignKey("services_tenant", []string{"tenant_id"}, "tenants", []string{"id"}),
					foreignKey("services_project", []string{"tenant_id", "project_id"}, "projects", []string{"tenant_id", "id"}),
				},
				Unique: []UniqueConstraint{
					unique("services_scope_name", true, "tenant_id", "project_id", "name"),
					unique("services_tenant_id", false, "tenant_id", "id"),
				},
				Checks: []Check{
					check("services_state", "state IN ('enabled', 'disabled')"),
					check("services_token_pair", "(token_nonce IS NULL) = (token_expires_at IS NULL)"),
					check("services_token_nonce_length", "token_nonce IS NULL OR octet_length(token_nonce) = 32"),
				},
				RowLevelSecurity:      true,
				ForceRowLevelSecurity: true,
			},
			{
				Name:       "resource_bindings",
				PrimaryKey: primaryKey("id"),
				Columns: []Column{
					column("id", "text", false),
					column("tenant_id", "text", false),
					column("project_id", "text", false),
					column("resource_id", "text", false),
					column("service_id", "text", false),
					column("origins", "jsonb", false),
					column("operations", "jsonb", false),
					column("reportable_events", "jsonb", false),
					column("metadata", "jsonb", false),
					column("version", "bigint", false),
					column("created_at", "timestamptz", false),
				},
				ForeignKeys: []ForeignKey{
					foreignKey("resource_bindings_tenant", []string{"tenant_id"}, "tenants", []string{"id"}),
					foreignKey("resource_bindings_project", []string{"tenant_id", "project_id"}, "projects", []string{"tenant_id", "id"}),
					foreignKey("resource_bindings_resource", []string{"tenant_id", "project_id", "resource_id"}, "resources", []string{"tenant_id", "project_id", "id"}),
					foreignKey("resource_bindings_service", []string{"tenant_id", "service_id"}, "services", []string{"tenant_id", "id"}),
				},
				Unique: []UniqueConstraint{
					unique("resource_bindings_scope_id", false, "tenant_id", "project_id", "id"),
					unique("resource_bindings_scope_resource_service", false, "tenant_id", "project_id", "resource_id", "service_id"),
				},
				Indexes: []Index{
					index("resource_bindings_scope_service", false, "", "tenant_id", "project_id", "service_id"),
				},
				RowLevelSecurity:      true,
				ForceRowLevelSecurity: true,
			},
			{
				Name:       "pipelines",
				PrimaryKey: primaryKey("id"),
				Columns: []Column{
					column("id", "text", false),
					column("tenant_id", "text", false),
					column("project_id", "text", false),
					column("name", "text", false),
					column("yaml", "bytea", false),
					column("yaml_sha256", "bytea", false),
					column("push_triggers", "jsonb", false, "[]"),
					column("version", "bigint", false),
					column("created_at", "timestamptz", false),
				},
				ForeignKeys: []ForeignKey{
					foreignKey("pipelines_tenant", []string{"tenant_id"}, "tenants", []string{"id"}),
					foreignKey("pipelines_project", []string{"tenant_id", "project_id"}, "projects", []string{"tenant_id", "id"}),
				},
				Unique: []UniqueConstraint{
					unique("pipelines_scope_name", false, "tenant_id", "project_id", "name"),
					unique("pipelines_scope_id", false, "tenant_id", "project_id", "id"),
				},
				Checks: []Check{
					check("pipelines_push_triggers_array", "jsonb_typeof(push_triggers) = 'array'"),
				},
				RowLevelSecurity:      true,
				ForceRowLevelSecurity: true,
			},
			{
				Name:       "executions",
				PrimaryKey: primaryKey("id"),
				Columns: []Column{
					column("id", "text", false),
					column("tenant_id", "text", false),
					column("project_id", "text", false),
					column("pipeline_id", "text", true),
					column("task_id", "text", true),
					column("principal_id", "text", false),
					column("state", "text", false),
					column("version", "bigint", false),
					column("yaml_sha256", "bytea", false),
					column("snapshot", "jsonb", false),
					column("token_nonce", "bytea", true),
					column("token_expires_at", "timestamptz", true),
					column("workspace_id", "text", true),
					column("failure", "jsonb", true),
					column("created_at", "timestamptz", false),
				},
				ForeignKeys: []ForeignKey{
					foreignKey("executions_tenant", []string{"tenant_id"}, "tenants", []string{"id"}),
					foreignKey("executions_project", []string{"tenant_id", "project_id"}, "projects", []string{"tenant_id", "id"}),
					foreignKey("executions_task", []string{"tenant_id", "project_id", "task_id"}, "tasks", []string{"tenant_id", "project_id", "id"}),
				},
				Unique: []UniqueConstraint{
					unique("executions_scope_id", false, "tenant_id", "project_id", "id"),
				},
				Checks: []Check{
					check("executions_state", "state IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')"),
					check("executions_token_pair", "(token_nonce IS NULL) = (token_expires_at IS NULL)"),
					check("executions_token_nonce_length", "token_nonce IS NULL OR octet_length(token_nonce) = 32"),
				},
				Indexes: []Index{
					index("executions_scope_state", false, "", "tenant_id", "project_id", "state"),
				},
				RowLevelSecurity:      true,
				ForceRowLevelSecurity: true,
			},
			{
				Name:       "actions",
				PrimaryKey: primaryKey("id"),
				Columns: []Column{
					column("id", "text", false),
					column("execution_id", "text", false),
					column("tenant_id", "text", false),
					column("project_id", "text", false),
					column("index", "integer", false),
					column("name", "text", false),
					column("kind", "text", false),
					column("state", "text", false),
					column("version", "bigint", false),
					column("timeout_ms", "bigint", false),
					column("deadline", "timestamptz", true),
					column("outputs", "jsonb", true),
					column("staged_outputs", "jsonb", true),
					column("staged_outputs_set", "boolean", false),
					column("stop", "jsonb", true),
					column("request_subject", "jsonb", true),
					column("request_assignment", "jsonb", true),
					column("request_decision", "text", true),
					column("request_actor", "text", true),
					column("request_resolved_at", "timestamptz", true),
					column("workload_id", "text", true),
					column("launch_state", "text", false),
					column("launch_key", "text", true),
					column("observation", "jsonb", true),
					column("activated_at", "timestamptz", true),
					column("answer_provenance", "bytea", true),
				},
				ForeignKeys: []ForeignKey{
					foreignKey("actions_tenant", []string{"tenant_id"}, "tenants", []string{"id"}),
					foreignKey("actions_project", []string{"tenant_id", "project_id"}, "projects", []string{"tenant_id", "id"}),
					foreignKey("actions_execution", []string{"tenant_id", "project_id", "execution_id"}, "executions", []string{"tenant_id", "project_id", "id"}),
				},
				Unique: []UniqueConstraint{
					unique("actions_execution_index", false, "execution_id", "index"),
					unique("actions_execution_name", false, "execution_id", "name"),
					unique("actions_scope_id", false, "tenant_id", "project_id", "id"),
				},
				Checks: []Check{
					check("actions_state", "state IN ('pending', 'running', 'succeeded', 'failed', 'cancelled')"),
					check("actions_kind", "kind IN ('container', 'request')"),
					check("actions_launch_state", "launch_state IN ('not_issued', 'issued', 'unknown', 'retiring')"),
					check("actions_request_decision", "request_decision IS NULL OR request_decision IN ('pass', 'fail')"),
					check("actions_request_resolution_pair", "(request_decision IS NULL) = (request_actor IS NULL) AND (request_decision IS NULL) = (request_resolved_at IS NULL)"),
				},
				Indexes: []Index{
					index("actions_execution_state", false, "", "execution_id", "state"),
					index("actions_one_running", true, "state = 'running'", "execution_id"),
				},
				RowLevelSecurity:      true,
				ForceRowLevelSecurity: true,
			},
			{
				Name:       "tasks",
				PrimaryKey: primaryKey("id"),
				Columns: []Column{
					column("id", "text", false),
					column("tenant_id", "text", false),
					column("project_id", "text", false),
					column("content_version", "bigint", false),
					column("lifecycle_version", "bigint", false),
					column("state", "text", false),
					column("pipeline_id", "text", true),
					column("current_execution_id", "text", true),
					column("initiator_id", "text", false),
					column("title", "text", false),
					column("description", "text", true),
					column("metadata", "jsonb", true),
					column("inputs", "jsonb", true),
					column("execution_history", "jsonb", true),
					column("question_id", "text", true),
					column("question_body", "text", true),
					column("question_assignment", "jsonb", true),
					column("question_references", "jsonb", true),
					column("question_version", "bigint", true),
					column("question_asked_by", "text", true),
					column("question_at", "timestamptz", true),
					column("created_at", "timestamptz", false),
				},
				ForeignKeys: []ForeignKey{
					foreignKey("tasks_tenant", []string{"tenant_id"}, "tenants", []string{"id"}),
					foreignKey("tasks_project", []string{"tenant_id", "project_id"}, "projects", []string{"tenant_id", "id"}),
				},
				Unique: []UniqueConstraint{
					unique("tasks_scope_id", false, "tenant_id", "project_id", "id"),
				},
				Checks: []Check{
					check("tasks_state", "state IN ('open', 'waiting', 'done', 'cancelled')"),
					check("tasks_question_pair", "(question_id IS NULL) = (question_version IS NULL)"),
				},
				Indexes: []Index{
					index("tasks_scope_state", false, "", "tenant_id", "project_id", "state"),
				},
				RowLevelSecurity:      true,
				ForceRowLevelSecurity: true,
			},
			{
				Name:       "task_messages",
				PrimaryKey: primaryKey("id"),
				Columns: []Column{
					column("id", "text", false),
					column("task_id", "text", false),
					column("tenant_id", "text", false),
					column("project_id", "text", false),
					column("author_id", "text", false),
					column("at", "timestamptz", false),
					column("body", "text", false),
					column("refs", "jsonb", true),
				},
				ForeignKeys: []ForeignKey{
					foreignKey("task_messages_tenant", []string{"tenant_id"}, "tenants", []string{"id"}),
					foreignKey("task_messages_project", []string{"tenant_id", "project_id"}, "projects", []string{"tenant_id", "id"}),
					foreignKey("task_messages_task", []string{"tenant_id", "project_id", "task_id"}, "tasks", []string{"tenant_id", "project_id", "id"}),
				},
				RowLevelSecurity:      true,
				ForceRowLevelSecurity: true,
			},
			{
				Name:       "events",
				PrimaryKey: primaryKey("id"),
				Columns: []Column{
					column("id", "text", false),
					column("tenant_id", "text", false),
					column("project_id", "text", true),
					column("type", "text", false),
					column("report_type", "text", true),
					column("subject_id", "text", false),
					column("subject_version", "bigint", true),
					column("source_kind", "text", false),
					column("source_principal", "text", false),
					column("correlation_id", "text", true),
					column("causation_id", "text", true),
					column("at", "timestamptz", false),
					column("data", "bytea", true),
					column("publish_state", "text", false),
					column("publish_sequence", "bigint", false),
				},
				ForeignKeys: []ForeignKey{
					foreignKey("events_tenant", []string{"tenant_id"}, "tenants", []string{"id"}),
					foreignKey("events_project", []string{"tenant_id", "project_id"}, "projects", []string{"tenant_id", "id"}),
				},
				Unique: []UniqueConstraint{
					unique("events_publish_sequence", false, "publish_sequence"),
				},
				Checks: []Check{
					check("events_publish_state", "publish_state IN ('pending', 'published')"),
					check("events_report_type", "(report_type IS NULL) = (type <> 'resource.verified')"),
				},
				Indexes: []Index{
					index("events_scope_at", false, "", "tenant_id", "project_id", "at"),
					index("events_publish_backlog", false, "publish_state = 'pending'", "publish_sequence"),
				},
				RowLevelSecurity:      true,
				ForceRowLevelSecurity: true,
			},
			{
				Name:       "audit",
				PrimaryKey: primaryKey("id"),
				Columns: []Column{
					column("id", "text", false),
					column("tenant_id", "text", false),
					column("project_id", "text", true),
					column("actor", "text", false),
					column("operation", "text", false),
					column("at", "timestamptz", false),
					column("detail", "text", true),
				},
				ForeignKeys: []ForeignKey{
					foreignKey("audit_tenant", []string{"tenant_id"}, "tenants", []string{"id"}),
					foreignKey("audit_project", []string{"tenant_id", "project_id"}, "projects", []string{"tenant_id", "id"}),
				},
				RowLevelSecurity:      true,
				ForceRowLevelSecurity: true,
			},
			{
				Name:       "receipts",
				PrimaryKey: primaryKey("id"),
				Columns: []Column{
					column("id", "text", false),
					column("tenant_id", "text", false),
					column("project_id", "text", true),
					column("principal_id", "text", false),
					column("operation", "text", false),
					column("caller_key", "text", false),
					column("content_sha256", "bytea", false),
					column("result", "bytea", true),
					column("completed_at", "timestamptz", true),
					column("execution_id", "text", true),
					column("snapshot_sha256", "bytea", true),
					column("created_at", "timestamptz", false),
				},
				ForeignKeys: []ForeignKey{
					foreignKey("receipts_tenant", []string{"tenant_id"}, "tenants", []string{"id"}),
					foreignKey("receipts_project", []string{"tenant_id", "project_id"}, "projects", []string{"tenant_id", "id"}),
				},
				Unique: []UniqueConstraint{
					unique("receipts_identity", true, "tenant_id", "project_id", "principal_id", "operation", "caller_key"),
				},
				Checks: []Check{
					// A receipt is either incomplete (a NULL completion marker)
					// or completed (marker and result). The marker is the
					// compare-and-set guard for a later completion unit. An
					// incomplete receipt MAY carry an in-progress result (the
					// trigger Select's persisted selection), which a later
					// completion overwrites; only a completed receipt must
					// carry a result.
					check("receipts_completion_pair", "completed_at IS NULL OR result IS NOT NULL"),
				},
				Indexes: []Index{
					// A verified resource report is deduplicated by its service
					// principal and source-event caller key, independently of any
					// other operation that reuses the same caller key.
					index("receipts_resource_report_dedupe", true, "operation = 'resource.report'", "tenant_id", "project_id", "principal_id", "caller_key"),
				},
				RowLevelSecurity:      true,
				ForceRowLevelSecurity: true,
			},
			{
				Name:       "verifications",
				PrimaryKey: primaryKey("id"),
				Columns: []Column{
					column("id", "text", false),
					column("tenant_id", "text", false),
					column("project_id", "text", false),
					column("service_id", "text", false),
					column("actor_id", "text", false),
					column("execution_id", "text", true),
					column("action_id", "text", true),
					column("binding_id", "text", false),
					column("resource_id", "text", false),
					column("operation", "text", false),
					column("subject", "jsonb", false),
					column("requested_approval", "jsonb", true),
					column("result", "text", false),
					column("approved", "boolean", false),
					column("created_at", "timestamptz", false),
				},
				ForeignKeys: []ForeignKey{
					foreignKey("verifications_tenant", []string{"tenant_id"}, "tenants", []string{"id"}),
					foreignKey("verifications_project", []string{"tenant_id", "project_id"}, "projects", []string{"tenant_id", "id"}),
					foreignKey("verifications_execution", []string{"tenant_id", "project_id", "execution_id"}, "executions", []string{"tenant_id", "project_id", "id"}),
					foreignKey("verifications_action", []string{"tenant_id", "project_id", "action_id"}, "actions", []string{"tenant_id", "project_id", "id"}),
					foreignKey("verifications_resource", []string{"tenant_id", "project_id", "resource_id"}, "resources", []string{"tenant_id", "project_id", "id"}),
					foreignKey("verifications_service", []string{"tenant_id", "service_id"}, "services", []string{"tenant_id", "id"}),
				},
				RowLevelSecurity:      true,
				ForceRowLevelSecurity: true,
			},
			{
				Name:       "secrets",
				PrimaryKey: primaryKey("id"),
				Columns: []Column{
					column("id", "text", false),
					column("tenant_id", "text", false),
					column("project_id", "text", true),
					column("name", "text", false),
					column("ciphertext", "bytea", false),
					column("wrapped_key", "bytea", false),
					column("wrap_nonce", "bytea", false),
					column("key_ref", "text", false),
					column("key_version", "integer", false),
					column("nonce", "bytea", false),
					column("version", "bigint", false),
					column("created_at", "timestamptz", false),
					column("updated_at", "timestamptz", false),
				},
				ForeignKeys: []ForeignKey{
					foreignKey("secrets_tenant", []string{"tenant_id"}, "tenants", []string{"id"}),
					foreignKey("secrets_project", []string{"tenant_id", "project_id"}, "projects", []string{"tenant_id", "id"}),
				},
				Unique: []UniqueConstraint{
					unique("secrets_scope_name", true, "tenant_id", "project_id", "name"),
				},
				Checks: []Check{
					check("secrets_key_version", "key_version > 0"),
					check("secrets_version", "version > 0"),
					check("secrets_nonce_length", "octet_length(nonce) = 12"),
				},
				RowLevelSecurity:      true,
				ForceRowLevelSecurity: true,
			},
			{
				Name:       "resources",
				PrimaryKey: primaryKey("id"),
				Columns: []Column{
					column("id", "text", false),
					column("tenant_id", "text", false),
					column("project_id", "text", false),
					column("kind", "text", false),
					column("version", "bigint", false),
					column("metadata", "jsonb", false),
					column("created_at", "timestamptz", false),
				},
				ForeignKeys: []ForeignKey{
					foreignKey("resources_tenant", []string{"tenant_id"}, "tenants", []string{"id"}),
					foreignKey("resources_project", []string{"tenant_id", "project_id"}, "projects", []string{"tenant_id", "id"}),
				},
				Unique: []UniqueConstraint{
					unique("resources_scope_id", false, "tenant_id", "project_id", "id"),
				},
				RowLevelSecurity:      true,
				ForceRowLevelSecurity: true,
			},
			{
				Name:       "cleanup",
				PrimaryKey: primaryKey("id"),
				Columns: []Column{
					column("id", "text", false),
					column("tenant_id", "text", false),
					column("project_id", "text", false),
					column("execution_id", "text", false),
					column("workload_id", "text", true),
					column("workspace_id", "text", true),
					column("state", "text", false),
					column("attempts", "integer", false),
					column("last_error", "text", true),
					column("next_retry_at", "timestamptz", true),
				},
				ForeignKeys: []ForeignKey{
					foreignKey("cleanup_tenant", []string{"tenant_id"}, "tenants", []string{"id"}),
					foreignKey("cleanup_project", []string{"tenant_id", "project_id"}, "projects", []string{"tenant_id", "id"}),
					foreignKey("cleanup_execution", []string{"tenant_id", "project_id", "execution_id"}, "executions", []string{"tenant_id", "project_id", "id"}),
				},
				Checks: []Check{
					check("cleanup_state", "state IN ('none', 'pending', 'complete')"),
				},
				RowLevelSecurity:      true,
				ForceRowLevelSecurity: true,
			},
			{
				Name:       "model_usage",
				PrimaryKey: primaryKey("id"),
				Columns: []Column{
					column("id", "text", false),
					column("tenant_id", "text", false),
					column("project_id", "text", false),
					column("principal_id", "text", false),
					column("execution_id", "text", true),
					column("model", "text", false),
					column("upstream_model", "text", false),
					column("outcome", "text", false),
					column("usage_known", "boolean", false),
					column("prompt_tokens", "bigint", false),
					column("completion_tokens", "bigint", false),
					column("total_tokens", "bigint", false),
					column("recorded_at", "timestamptz", false),
					column("version", "bigint", false),
				},
				ForeignKeys: []ForeignKey{
					foreignKey("model_usage_tenant", []string{"tenant_id"}, "tenants", []string{"id"}),
					foreignKey("model_usage_project", []string{"tenant_id", "project_id"}, "projects", []string{"tenant_id", "id"}),
					foreignKey("model_usage_execution", []string{"tenant_id", "project_id", "execution_id"}, "executions", []string{"tenant_id", "project_id", "id"}),
				},
				Unique: []UniqueConstraint{
					unique("model_usage_scope_id", false, "tenant_id", "project_id", "id"),
				},
				Checks: []Check{
					check("model_usage_outcome", "outcome IN ('succeeded', 'failed', 'uncertain')"),
					check("model_usage_counts", "prompt_tokens >= 0 AND completion_tokens >= 0 AND total_tokens >= 0 AND total_tokens = prompt_tokens + completion_tokens"),
					check("model_usage_known", "usage_known = (outcome = 'succeeded')"),
				},
				Indexes: []Index{
					index("model_usage_scope_recorded", false, "", "tenant_id", "project_id", "recorded_at", "id"),
				},
				RowLevelSecurity:      true,
				ForceRowLevelSecurity: true,
			},
			{
				Name:       "content_records",
				PrimaryKey: primaryKey("id"),
				Columns: []Column{
					column("id", "text", false),
					column("tenant_id", "text", false),
					column("project_id", "text", false),
					column("principal_id", "text", false),
					column("execution_id", "text", true),
					column("model", "text", false),
					column("request_body", "bytea", false),
					column("response_body", "bytea", false),
					column("recorded_at", "timestamptz", false),
					column("retain_until", "timestamptz", false),
					column("version", "bigint", false),
				},
				ForeignKeys: []ForeignKey{
					foreignKey("content_records_tenant", []string{"tenant_id"}, "tenants", []string{"id"}),
					foreignKey("content_records_project", []string{"tenant_id", "project_id"}, "projects", []string{"tenant_id", "id"}),
					foreignKey("content_records_execution", []string{"tenant_id", "project_id", "execution_id"}, "executions", []string{"tenant_id", "project_id", "id"}),
				},
				Unique: []UniqueConstraint{
					unique("content_records_scope_id", false, "tenant_id", "project_id", "id"),
				},
				Checks: []Check{
					check("content_records_retention", "retain_until > recorded_at"),
				},
				Indexes: []Index{
					index("content_records_scope_recorded", false, "", "tenant_id", "project_id", "recorded_at", "id"),
				},
				RowLevelSecurity:      true,
				ForceRowLevelSecurity: true,
			},
			{
				Name:       "event_consumptions",
				PrimaryKey: primaryKey("id"),
				Columns: []Column{
					column("id", "text", false),
					column("tenant_id", "text", false),
					column("project_id", "text", true),
					column("consumer", "text", false),
					column("completed_at", "timestamptz", false),
					column("version", "bigint", false),
				},
				ForeignKeys: []ForeignKey{
					foreignKey("event_consumptions_tenant", []string{"tenant_id"}, "tenants", []string{"id"}),
					foreignKey("event_consumptions_project", []string{"tenant_id", "project_id"}, "projects", []string{"tenant_id", "id"}),
				},
				Unique: []UniqueConstraint{
					unique("event_consumptions_scope_id", false, "tenant_id", "project_id", "id"),
				},
				Indexes: []Index{
					index("event_consumptions_scope_consumer", false, "", "tenant_id", "project_id", "consumer"),
				},
				RowLevelSecurity:      true,
				ForceRowLevelSecurity: true,
			},
		},
	}
}

// Table names of the model-usage, content-record and event-consumption tables.
// They are declared here so an adapter never spells one as free text.
const (
	// ModelUsageTable is the attributable model-usage table.
	ModelUsageTable = "model_usage"
	// ContentRecordsTable is the authorized raw-content capture table.
	ContentRecordsTable = "content_records"
	// EventConsumptionsTable is the durable trigger event-completion marker.
	EventConsumptionsTable = "event_consumptions"
)

// Operation is one SQL privilege a non-owner role may hold.
type Operation string

const (
	// OperationSelect is a read privilege.
	OperationSelect Operation = "select"
	// OperationInsert is a row-creation privilege.
	OperationInsert Operation = "insert"
	// OperationUpdate is a row-mutation privilege.
	OperationUpdate Operation = "update"
	// OperationDelete is a row-removal privilege.
	OperationDelete Operation = "delete"
)

var operations = [...]Operation{OperationSelect, OperationInsert, OperationUpdate, OperationDelete}

// Valid reports whether the operation is one of the declared privileges.
func (operation Operation) Valid() bool {
	for _, candidate := range operations {
		if operation == candidate {
			return true
		}
	}
	return false
}

// Grant is one explicit table privilege set for one non-owner, NOBYPASSRLS
// role. SelectColumns restricts SELECT when non-empty; WriteColumns restricts
// INSERT/UPDATE when non-empty. Empty means every column. It is declaration
// data: an adapter renders the grant and policy, never a general unscoped query.
type Grant struct {
	Role          string
	Table         string
	Operations    []Operation
	SelectColumns []string
	WriteColumns  []string
}

// Validate checks that a grant names a role, a declared table, at least one
// declared operation and only declared columns.
func (grant Grant) Validate() error {
	if grant.Role == "" {
		return fault.New(fault.InvalidInput, "grant role is empty")
	}
	table, ok := schemaTable(grant.Table)
	if !ok {
		return fault.New(fault.InvalidInput, fmt.Sprintf("grant table %q is not in the frozen schema", grant.Table))
	}
	if len(grant.Operations) == 0 {
		return fault.New(fault.InvalidInput, "grant has no operations")
	}
	for _, operation := range grant.Operations {
		if !operation.Valid() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("grant operation %q is not frozen", operation))
		}
	}
	for _, column := range append(append([]string(nil), grant.SelectColumns...), grant.WriteColumns...) {
		if !tableHasColumn(table, column) {
			return fault.New(fault.InvalidInput, fmt.Sprintf("grant column %q is not declared by table %q", column, grant.Table))
		}
	}
	return nil
}

// ControllerReadTables returns the exact tables the cross-scope controller may
// read: the Kernel state rows plus the committed event rows the trigger
// EventReader scans. audit is deliberately absent: the audit trail is
// INSERT-only for the controller and a cross-scope controller read of it would
// return another tenant's audit rows. The controller has no general unscoped
// query.
func ControllerReadTables() []string {
	return []string{"executions", "actions", "cleanup", "events"}
}

// ControllerWriteTables returns the exact tables the cross-scope controller may
// write: the Kernel state rows plus the execution-bound event and audit rows an
// engine commit unit carries. The controller never advances an event's
// publication state; that is the separate publisher role. The event and audit
// tables are INSERT-only for the controller, and audit is write-only: it is in
// no controller read set and has no controller SELECT grant or policy.
func ControllerWriteTables() []string {
	return []string{"executions", "actions", "cleanup", "events", "audit"}
}

// ControllerInsertOnlyTables returns the controller write tables that admit
// INSERT but not UPDATE or DELETE. They are the event publication-intent rows
// and the audit trail: a committed row is immutable to this role. The adapters
// derive the insert-only refusal from this declaration and from
// ControllerGrants.
func ControllerInsertOnlyTables() []string {
	return []string{"events", "audit"}
}

// AbsenceGuardImplemented reports whether the concrete store adapters implement
// the absence guard. The memory and PostgreSQL adapters implement the absence
// plans, so this is true. Both adapters consult it and each keeps an
// overridable unexported package gate for tests; while the gate is off a unit
// carrying an absence guard is refused with invalid_input before any statement,
// so a declared-but-unimplemented guard fails closed instead of being silently
// ignored.
const AbsenceGuardImplemented = true

// AbsenceKeyColumns returns the registered UNIQUE-KEY columns an absence guard
// names for a table. Only memberships is registered: its unique identity
// is (tenant_id, project_id, principal_id) with a NULL project_id for a
// tenant-wide row, so the tenant-wide and project keys are distinct identities.
// An unregistered table returns nil, false and an adapter must refuse an
// absence guard on it (invalid_input).
func AbsenceKeyColumns(table string) ([]string, bool) {
	switch table {
	case "memberships":
		return []string{"tenant_id", "project_id", "principal_id"}, true
	}
	return nil, false
}

// ControllerEventCommitImplemented reports whether the concrete controller
// adapters implement the execution-bound event/audit write path. The memory
// and PostgreSQL controllers implement the atomic event/audit commit, so this
// is true. It is the single declaration both adapters consult; each keeps an
// overridable unexported package gate for tests.
const ControllerEventCommitImplemented = true

// ControllerGrants returns the explicit controller-role grants. The role is a
// non-owner, NOBYPASSRLS role; it may read the state tables and the committed
// event rows the trigger EventReader scans, write the state tables, and INSERT
// execution-bound event and audit rows. It has no UPDATE/DELETE authority on
// events or audit, no SELECT on audit (the audit trail is write-only to this
// role), no receipt grant and no unscoped query.
func ControllerGrants() []Grant {
	return []Grant{
		{Role: ControllerRole, Table: "executions", Operations: []Operation{OperationSelect, OperationUpdate}},
		{Role: ControllerRole, Table: "actions", Operations: []Operation{OperationSelect, OperationInsert, OperationUpdate}},
		{Role: ControllerRole, Table: "cleanup", Operations: []Operation{OperationSelect, OperationInsert, OperationUpdate}},
		{Role: ControllerRole, Table: "events", Operations: []Operation{OperationSelect, OperationInsert}},
		{Role: ControllerRole, Table: "audit", Operations: []Operation{OperationInsert}},
	}
}

// PublisherGrants returns the explicit outbox-publisher grants: events-only
// table-wide SELECT and a publish_state-column UPDATE. The role cannot read
// domain tables and cannot write any other event column.
func PublisherGrants() []Grant {
	return []Grant{
		{
			Role:         PublisherRole,
			Table:        "events",
			Operations:   []Operation{OperationSelect, OperationUpdate},
			WriteColumns: []string{"publish_state"},
		},
	}
}

// AuthGrants returns the explicit pre-authentication grants: exact-column
// SELECT on the two credential-bearing tables only. The role cannot list rows,
// read token material beyond the lookup columns, or write anything.
func AuthGrants() []Grant {
	return []Grant{
		{
			Role:          AuthRole,
			Table:         "executions",
			Operations:    []Operation{OperationSelect},
			SelectColumns: []string{"id", "tenant_id", "project_id", "principal_id"},
		},
		{
			Role:          AuthRole,
			Table:         "services",
			Operations:    []Operation{OperationSelect},
			SelectColumns: []string{"id", "tenant_id", "project_id"},
		},
	}
}

// ControllerScan is one complete cross-scope controller read request. It is the
// same shape as ScanQuery but restricted to ControllerReadTables; the adapter
// still applies the scanner bounds, cursor and deterministic ordering. There is
// deliberately no caller-supplied scope: the controller is the one seam that
// reads across scopes for the scope-free Kernel engine and the trigger
// EventReader.
type ControllerScan struct {
	ScanQuery
}

// Validate checks the shared scan shape and then restricts the table to the
// controller read authority. A table outside that set is invalid_input.
func (scan ControllerScan) Validate() error {
	if err := scan.ScanQuery.Validate(); err != nil {
		return err
	}
	for _, table := range ControllerReadTables() {
		if table == scan.Table {
			return nil
		}
	}
	return fault.New(fault.InvalidInput, fmt.Sprintf("controller scan table %q is not in the controller authority", scan.Table))
}

// Controller is the separate cross-scope Kernel-state and trigger-event seam.
// It is not a general unscoped repository: it may read only the controller
// read tables and may write only the controller write tables. audit is
// write-only for this role and is in no read table.
//
// Read returns one row by identity from the controller read set. Scan returns
// one bounded deterministic page from the controller read set. Execute resolves
// the owning scope from the guarded execution row inside the same transaction
// and then applies the unit exactly like Executor.Execute under that derived
// scope: every lock and write must stay inside the owning scope, and a unit
// whose rows are outside it is refused.
//
// An engine commit unit may additionally carry execution-bound lifecycle
// event rows (ids from event.ProjectLifecycle) and audit rows, committed
// atomically with the state writes. The scope of every event/audit row must
// equal the scope derived from the locked/guarded execution row; a row with
// another tenant or project refuses the whole unit. An event or audit unit
// without a guarded execution row is refused, events/audit are INSERT-only, and
// a receipt is still refused because receipts belong to a scoped capability.
// The event row itself is the publication intent; the controller never updates
// publication state.
type Controller interface {
	Read(ctx context.Context, table, id string) (Row, error)
	Scan(ctx context.Context, scan ControllerScan) (QueryPage, error)
	Execute(ctx context.Context, executionID string, unit Unit) (UnitResult, error)
}

// CredentialKind is the closed set of derived credential kinds a
// pre-authentication lookup may name.
type CredentialKind string

const (
	// CredentialExecution is a kodex execution credential.
	CredentialExecution CredentialKind = "execution"
	// CredentialService is a kodsvc service credential.
	CredentialService CredentialKind = "service"
)

var credentialKinds = [...]CredentialKind{CredentialExecution, CredentialService}

// Valid reports whether the kind is one of the declared credential kinds.
func (kind CredentialKind) Valid() bool {
	for _, candidate := range credentialKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// CandidateScope is the bootstrap scope hint for one presented credential. It
// is never authority: the caller still verifies the MAC, current nonce/expiry
// and current principal state inside this candidate scope before any grant is
// evaluated. ProjectID is empty for a tenant-wide service.
type CandidateScope struct {
	TenantID    id.TenantID
	ProjectID   id.ProjectID
	PrincipalID id.PrincipalID
}

// Validate checks that the candidate names a tenant and principal and, when
// present, a project.
func (scope CandidateScope) Validate() error {
	if _, err := id.ParseTenantID(string(scope.TenantID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "candidate tenant id is invalid", err)
	}
	if scope.ProjectID != "" {
		if _, err := id.ParseProjectID(string(scope.ProjectID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "candidate project id is invalid", err)
		}
	}
	if _, err := id.ParsePrincipalID(string(scope.PrincipalID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "candidate principal id is invalid", err)
	}
	return nil
}

// CredentialLookup is the read-only pre-authentication lookup seam. It answers
// exactly one exact-kind/exact-id question and returns a candidate scope. It
// exposes no listing, scan, write or update method.
//
// A missing row and a row of a different kind are indistinguishable: both
// return fault.Unauthenticated with the same message, so the lookup cannot be
// used to enumerate tenants, projects, executions or services. The lookup never
// returns token material and never evaluates a grant.
type CredentialLookup interface {
	LookupCredential(ctx context.Context, kind CredentialKind, credentialID string) (CandidateScope, error)
}

// ReceiptMode selects what a unit does with its receipt.
type ReceiptMode string

const (
	// ReceiptInsert is the default zero mode: insert a new receipt already
	// completed (result present, completed_at set) or replay an equal one. The
	// zero value preserves every existing unit.
	ReceiptInsert ReceiptMode = ""
	// ReceiptInsertIncomplete inserts a new receipt with a NULL completed_at
	// marker, so a later ReceiptComplete unit can complete it under the same
	// key. It MAY carry an in-progress result (for example the trigger Select's
	// persisted selection) without completing the receipt. An equal-key replay
	// applies nothing, returns that stored in-progress result and does not
	// complete the receipt; changed content is changed_idempotency_content. A
	// completion-mode unit cannot be combined with this mode.
	ReceiptInsertIncomplete ReceiptMode = "insert_incomplete"
	// ReceiptComplete completes a previously inserted incomplete receipt under
	// the same key. Completion is a compare-and-set on the receipt key, a NULL
	// completed_at and the expected content hash. It overwrites any in-progress
	// result with the unit's final result and sets completed_at exactly once. A
	// completed equal-key replay applies nothing and returns the original
	// result; changed content is changed_idempotency_content; a missing receipt,
	// a stale expectation or a concurrent completion that no longer matches is
	// version_conflict.
	ReceiptComplete ReceiptMode = "complete"
)

var receiptModes = [...]ReceiptMode{ReceiptInsert, ReceiptInsertIncomplete, ReceiptComplete}

// Valid reports whether the mode is one of the declared modes.
func (mode ReceiptMode) Valid() bool {
	for _, candidate := range receiptModes {
		if mode == candidate {
			return true
		}
	}
	return false
}

// ReceiptExpect is the required prior state of a receipt a completion-mode unit
// advances. It is the completion guard: the stored receipt must still be
// incomplete (completed_at IS NULL) at this content hash, so a concurrent
// completion or a different selection is reclassified rather than overwriting
// the first result.
type ReceiptExpect struct {
	// Completed is the required prior completion marker. A completion-mode unit
	// compare-and-sets on completed_at IS NULL, so the only valid expectation is
	// false; true is version_conflict. It remains explicit data so an adapter
	// cannot silently complete an already-completed receipt.
	Completed bool
	// ContentHash is the required stored content fingerprint.
	ContentHash string
}

// Validate checks that the expected content hash is a sha256 fingerprint.
func (expect ReceiptExpect) Validate() error {
	if expect.ContentHash == "" {
		return fault.New(fault.InvalidInput, "receipt expectation has no content hash")
	}
	if !strings.HasPrefix(expect.ContentHash, "sha256:") || len(expect.ContentHash) != len("sha256:")+64 {
		return fault.New(fault.InvalidInput, "receipt expectation content hash is not a sha256 fingerprint")
	}
	return nil
}

// DeadlineGuard asserts that a timestamp column of the row a lock or write
// targets is strictly after the transaction clock. It is how request
// settlement refuses to answer at or past the persisted deadline even when the
// caller's cached decision was made earlier.
//
// The adapter reads the clock once inside the same transaction (the injected
// clock for the memory adapter, the database/transaction clock for SQL) and
// compares the stored value before any write. A NULL value or a value not
// strictly after the clock is deadline_expired.
type DeadlineGuard struct {
	Column string
}

// Validate checks that the guard names a column.
func (guard DeadlineGuard) Validate() error {
	if guard.Column == "" {
		return fault.New(fault.InvalidInput, "deadline guard column is empty")
	}
	return nil
}

// lockOrder is the single order every multi-table commit acquires locks in. It
// names every table written by an atomic commit unit plus the ownership
// parents those units lock first: task_messages (task ask/reply) and
// verifications (verified report) are included so no commit unit can acquire a
// row outside the declared order. cleanup is the durable execution-cleanup
// child and is locked after its execution parent.
func lockOrder() []string {
	return []string{
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
}

func primaryKey(columns ...string) []string {
	return append([]string(nil), columns...)
}

func column(name, dataType string, nullable bool, defaults ...string) Column {
	column := Column{Name: name, Type: dataType, Nullable: nullable}
	if len(defaults) > 0 {
		column.Default = defaults[0]
	}
	return column
}

func foreignKey(name string, columns []string, references string, referenced []string) ForeignKey {
	return ForeignKey{
		Name:              name,
		Columns:           append([]string(nil), columns...),
		References:        references,
		ReferencedColumns: append([]string(nil), referenced...),
	}
}

func unique(name string, nullsNotDistinct bool, columns ...string) UniqueConstraint {
	return UniqueConstraint{
		Name:             name,
		Columns:          append([]string(nil), columns...),
		NullsNotDistinct: nullsNotDistinct,
	}
}

func index(name string, isUnique bool, predicate string, columns ...string) Index {
	return Index{
		Name:      name,
		Columns:   append([]string(nil), columns...),
		Unique:    isUnique,
		Predicate: predicate,
	}
}

func check(name, expression string) Check {
	return Check{Name: name, Expression: expression}
}
