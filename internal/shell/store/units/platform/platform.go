// Package platform holds the store-backed adapters for the event, verification,
// secret and platform ports.
//
// Every adapter here is deliberately thin: the owning capability decides and
// builds one complete domain commit unit, and the adapter maps that unit to
// exactly one store.Unit. The adapter never re-derives policy, never issues SQL
// and never imports a concrete persistence adapter in production code; it uses
// only the neutral store.Executor/store.Scanner seams and the capability
// packages that own the values it persists.
//
// Reads map a scoped request to store.Scanner.Query or store.Executor.Read and
// decode the declared schema columns back into the capability values. Writes
// map the unit's domain row, receipt, audit rows and event publication intent
// into one Execute call so they commit or roll back together. A replayed
// receipt returns the original durable result decoded from Receipt.Result;
// store faults are returned unchanged.
//
// Ports that are not adapters here, and why:
//
//   - event.Outbox is implemented directly by the memory and PostgreSQL stores
//     under the publisher role; it is not an adapter of this package.
//   - secret.NamePolicy is configuration-backed (secret.ConfiguredNamePolicy),
//     not a store table.
//   - gateway.ContentRecorder is gateway.StoreContentRecorder over
//     store.Executor.
//
// mcpproxy.ExecutionReader is implemented here; its permission comes from the
// injected consumer-owned PermissionSource because no declared column stores
// it.
package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Schema table and column names used by these adapters. The names come
// only from store.SchemaDesign(); a name that is not declared there is a bug.
const (
	eventsTable           = "events"
	membershipsTable      = "memberships"
	servicesTable         = "services"
	executionsTable       = "executions"
	actionsTable          = "actions"
	resourceBindingsTable = "resource_bindings"
	resourcesTable        = "resources"
	verificationsTable    = "verifications"
	secretsTable          = "secrets"
	tasksTable            = "tasks"
	pipelinesTable        = "pipelines"
	idColumn              = "id"
	principalColumn       = "principal_id"
	stateColumn           = "state"
	rolesColumn           = "roles"
	versionColumn         = "version"
	tokenNonceColumn      = "token_nonce"
	publishStateColumn    = "publish_state"
	publishSequenceColumn = "publish_sequence"
	pendingPublishState   = "pending"
	runningExecutionState = "running"
	enabledServiceState   = "enabled"
	requestActionKind     = "request"
	succeededActionState  = "succeeded"
	passRequestDecision   = "pass"
	nameColumn            = "name"
	subjectColumn         = "subject"
	assignmentColumn      = "request_assignment"
	requestSubjectColumn  = "request_subject"
	requestDecisionColumn = "request_decision"
	requestResolvedAtCol  = "request_resolved_at"
	executionIDColumn     = "execution_id"
	actionIDColumn        = "action_id"
	serviceIDColumn       = "service_id"
	bindingIDColumn       = "binding_id"
	resourceIDColumn      = "resource_id"
	operationColumn       = "operation"
	resultColumn          = "result"
	approvedColumn        = "approved"
	createdAtColumn       = "created_at"
	actorIDColumn         = "actor_id"
	subjectIDColumn       = "subject_id"
	subjectVersionColumn  = "subject_version"
	typeColumn            = "type"
	reportTypeColumn      = "report_type"
	sourceKindColumn      = "source_kind"
	sourcePrincipalColumn = "source_principal"
	correlationIDColumn   = "correlation_id"
	causationIDColumn     = "causation_id"
	atColumn              = "at"
	dataColumn            = "data"
	requestedApprovalCol  = "requested_approval"
	ciphertextColumn      = "ciphertext"
	wrappedKeyColumn      = "wrapped_key"
	wrapNonceColumn       = "wrap_nonce"
	keyRefColumn          = "key_ref"
	keyVersionColumn      = "key_version"
	nonceColumn           = "nonce"
	updatedAtColumn       = "updated_at"
	tenantIDColumn        = "tenant_id"
	projectIDColumn       = "project_id"
)

// missingColumn reports a durable row that does not carry a required declared
// column. It is an internal-failure class: the row violates the schema.
func missingColumn(table, column string) error {
	return fault.New(fault.InternalFailure, fmt.Sprintf("%s row has no usable %q column", table, column))
}

// corruptRow reports a durable row whose column has the wrong decoded shape.
func corruptRow(table, column string, value any) error {
	return fault.New(fault.InternalFailure, fmt.Sprintf("%s row column %q has unexpected type %T", table, column, value))
}

func textValue(row store.Row, column string) (string, error) {
	value, ok := row.Values[column].(string)
	if !ok {
		if row.Values[column] == nil {
			return "", missingColumn(row.Table, column)
		}
		return "", corruptRow(row.Table, column, row.Values[column])
	}
	return value, nil
}

func nullableTextValue(row store.Row, column string) (string, error) {
	value, present := row.Values[column]
	if !present || value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", corruptRow(row.Table, column, value)
	}
	return text, nil
}

func intValue(row store.Row, column string) (int64, error) {
	value, ok := row.Values[column].(int64)
	if !ok {
		if row.Values[column] == nil {
			return 0, missingColumn(row.Table, column)
		}
		return 0, corruptRow(row.Table, column, row.Values[column])
	}
	return value, nil
}

func nullableUintValue(row store.Row, column string) (uint64, error) {
	value, present := row.Values[column]
	if !present || value == nil {
		return 0, nil
	}
	number, ok := value.(int64)
	if !ok {
		return 0, corruptRow(row.Table, column, value)
	}
	if number < 0 {
		return 0, fault.New(fault.InternalFailure, fmt.Sprintf("%s row column %q is negative", row.Table, column))
	}
	return uint64(number), nil
}

func bytesValue(row store.Row, column string) ([]byte, error) {
	value, present := row.Values[column]
	if !present || value == nil {
		return nil, nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return nil, corruptRow(row.Table, column, value)
	}
	return append([]byte(nil), bytes...), nil
}

func timeValue(row store.Row, column string) (time.Time, error) {
	value, ok := row.Values[column].(time.Time)
	if !ok {
		if row.Values[column] == nil {
			return time.Time{}, missingColumn(row.Table, column)
		}
		return time.Time{}, corruptRow(row.Table, column, row.Values[column])
	}
	return value, nil
}

func nullableTimeValue(row store.Row, column string) (time.Time, error) {
	value, present := row.Values[column]
	if !present || value == nil {
		return time.Time{}, nil
	}
	moment, ok := value.(time.Time)
	if !ok {
		return time.Time{}, corruptRow(row.Table, column, value)
	}
	return moment, nil
}

// jsonValue encodes one retained value exactly. A JSONB column round-trips
// through the encoding/json representation the capability supplied.
func jsonValue(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "value could not be encoded for a jsonb column", err)
	}
	return encoded, nil
}

func decodeJSON(data []byte, target any, column string) error {
	if len(data) == 0 {
		return missingColumn("row", column)
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fault.Wrap(fault.InternalFailure, fmt.Sprintf("column %q does not decode", column), err)
	}
	return nil
}

// nullableJSON returns nil for an absent JSONB value and the raw bytes
// otherwise, so an optional column can be persisted as SQL NULL.
func nullableJSON(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := jsonValue(value)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// authorityFacts carries the unit-local facts a generic authority read needs
// when its AuthorityRead does not name a row identity itself.
type authorityFacts struct {
	ExecutionID id.ExecutionID
	ActionID    id.ActionID
	// ActorKind is the principal kind the capability verified. The
	// actor.verify registry lists execution_running and action_deadline for
	// every actor, but only a human membership or a service row carries actor
	// authority without an execution: for those two kinds an empty target is
	// not applicable and adds no lock. Every other kind, including an
	// execution actor, must name the execution and action it is read against.
	ActorKind identity.PrincipalKind
}

// principalKind is the durable row family that represents one principal.
type principalKind int

const (
	principalMissing principalKind = iota
	principalExecution
	principalService
	principalHuman
)

// notApplicableActorAuthority reports whether a verified principal kind may
// legitimately carry no execution or action target. Only a human membership and
// a service row hold actor authority without an execution; an execution actor
// must name its running action, and an empty or unknown kind fails closed.
func notApplicableActorAuthority(kind identity.PrincipalKind) bool {
	return kind == identity.KindHuman || kind == identity.KindService
}

// authorityLocks maps the current-authority reads a capability attached to its
// unit into read-only store locks, so the adapter re-observes those rows inside
// the same commit. A versioned check becomes an ExpectVersion guard; a
// state/ownership check becomes an equality guard; a principal check resolves
// the row family that actually represents the principal.
//
// A membership or service-role read observes BOTH applicable rows, re-evaluates
// the effective union on them and pins every row by its observed identity and
// version, or by the absence guard when the row is absent. A role downgrade,
// grant, revoke or re-create that lands between the capability's read and this
// commit therefore cannot be locked in at the observed version. The adapter
// still makes no authorization decision beyond the facts the read names: policy
// stays in the capability that built the unit.
//
// An authority read whose target cannot exist for the verified actor kind is
// not applicable rather than malformed, but that exemption is limited to the two
// kinds that hold actor authority without an execution: a human membership and a
// service row. An execution actor's authority is its running execution, its
// running container action and that action's deadline together (KODAMA.md), so
// an execution actor that names no action has no deadline the commit could
// guard, and the read is refused. An empty or unknown actor kind names no
// applicable actor authority either, so it fails closed too. A supplied action
// target is still deadline-guarded for every actor kind.
func authorityLocks(
	ctx context.Context,
	executor store.Executor,
	scanner store.Scanner,
	scope tenancy.Scope,
	reads []authorize.AuthorityRead,
	facts authorityFacts,
) ([]store.Lock, error) {
	locks := make([]store.Lock, 0, 2*len(reads))
	add := func(lock store.Lock) {
		locks = append(locks, lock)
	}

	for _, read := range reads {
		if read.Scope != scope {
			return nil, fault.New(fault.Forbidden, "authority read names a foreign scope")
		}
		switch read.Check {
		case authorize.CheckMembership, authorize.CheckPushServiceAdmin:
			union, err := membershipUnionLocks(ctx, executor, scanner, scope, read)
			if err != nil {
				return nil, err
			}
			for _, lock := range union {
				add(lock)
			}
		case authorize.CheckPrincipalEnabled:
			if len(read.Targets) > 0 {
				// A principal-enablement read carrying membership pins is the
				// human actor's scoped union: re-read both membership rows,
				// confirm the exact pins and re-evaluate the observed role set
				// in the same commit.
				union, err := membershipUnionLocks(ctx, executor, scanner, scope, read)
				if err != nil {
					return nil, err
				}
				for _, lock := range union {
					add(lock)
				}
				break
			}
			lock, err := principalEnabledLock(ctx, executor, scanner, scope, read)
			if err != nil {
				return nil, err
			}
			add(lock)
		case authorize.CheckServiceEnabled:
			union, err := serviceEnabledUnionLocks(ctx, executor, scope, read)
			if err != nil {
				return nil, err
			}
			for _, lock := range union {
				add(lock)
			}
		case authorize.CheckExecutionRunning:
			target := read.TargetID
			if target == "" {
				target = string(facts.ExecutionID)
			}
			if target == "" {
				if notApplicableActorAuthority(facts.ActorKind) {
					// A human membership or a service row holds no execution,
					// so the execution_running read is not applicable to it. It
					// adds no lock; the actor's membership/service row is the
					// applicable guard.
					continue
				}
				return nil, fault.New(fault.InvalidInput, "execution_running authority read has no execution target")
			}
			lock, err := executionRunningLock(ctx, executor, scope, target, read.ExpectedVersion)
			if err != nil {
				return nil, err
			}
			add(lock)
		case authorize.CheckActionDeadline:
			target := read.TargetID
			if target == "" {
				target = string(facts.ActionID)
			}
			if target == "" {
				if notApplicableActorAuthority(facts.ActorKind) {
					// A human or service actor names no running action, so
					// there is no deadline to guard and the read adds no lock.
					// Its execution_running read is likewise not applicable,
					// and a supplied action stays deadline-guarded below.
					continue
				}
				// An execution actor's credential is revoked while it waits for
				// a request, so its authority is the running container action
				// and that action's deadline together: an execution attempt
				// that names no action has no deadline to guard and is refused.
				// An empty or unknown actor kind fails closed the same way.
				return nil, fault.New(fault.InvalidInput, "action_deadline authority read has no action target")
			}
			lock, err := actionDeadlineLock(ctx, executor, scope, target, read.ExpectedVersion)
			if err != nil {
				return nil, err
			}
			add(lock)
		case authorize.CheckBindingVersion:
			add(store.Lock{
				Table:         resourceBindingsTable,
				IDs:           []string{read.TargetID},
				ExpectVersion: read.ExpectedVersion,
			})
		case authorize.CheckResourceOwnership:
			add(store.Lock{Table: resourcesTable, IDs: []string{read.TargetID}})
		case authorize.CheckTaskVersion:
			add(store.Lock{
				Table:         tasksTable,
				IDs:           []string{read.TargetID},
				ExpectVersion: read.ExpectedVersion,
			})
		case authorize.CheckExecutionVersion:
			add(store.Lock{
				Table:         executionsTable,
				IDs:           []string{read.TargetID},
				ExpectVersion: read.ExpectedVersion,
			})
		case authorize.CheckPipelineVersion:
			add(store.Lock{
				Table:         pipelinesTable,
				IDs:           []string{read.TargetID},
				ExpectVersion: read.ExpectedVersion,
			})
		default:
			return nil, fault.New(fault.InternalFailure, fmt.Sprintf("unknown authority check %q", read.Check))
		}
	}
	return locks, nil
}

// principalEnabledLock maps a principal-enablement read onto the durable row
// that carries that principal. An execution must still be running with its
// current credential nonce, a service must still be enabled and hold a required
// service role, and a human must still hold a scoped membership with a required
// human role. The role re-evaluation happens before any observed version is
// pinned, so a downgrade that lands after the capability's read refuses the
// commit. An execution row is version-pinned only when the caller supplied a
// version: the engine's observation increments are not authority changes.
func principalEnabledLock(
	ctx context.Context,
	executor store.Executor,
	scanner store.Scanner,
	scope tenancy.Scope,
	read authorize.AuthorityRead,
) (store.Lock, error) {
	if read.PrincipalID == "" {
		return store.Lock{}, fault.New(fault.InvalidInput, "principal_enabled authority read has no principal")
	}
	row, kind, tenantWide, err := resolvePrincipalRow(ctx, executor, scanner, scope, read.PrincipalID)
	if err != nil {
		return store.Lock{}, err
	}
	switch kind {
	case principalExecution:
		if err := requireRowState(row, runningExecutionState); err != nil {
			return store.Lock{}, err
		}
		return executionLockFromRow(rowID(row), row, read.ExpectedVersion), nil
	case principalService:
		if err := requireRequiredRoles(row, read.Required.ServiceRoles); err != nil {
			return store.Lock{}, err
		}
		if err := requireRowState(row, enabledServiceState); err != nil {
			return store.Lock{}, err
		}
		version, err := pinnedVersion(row, read.ExpectedVersion)
		if err != nil {
			return store.Lock{}, err
		}
		return store.Lock{
			Table:         servicesTable,
			IDs:           []string{rowID(row)},
			ExpectVersion: version,
			Expect:        map[string]any{stateColumn: enabledServiceState},
			TenantWide:    tenantWide,
		}, nil
	case principalHuman:
		if err := requireRequiredRoles(row, read.Required.HumanRoles); err != nil {
			return store.Lock{}, err
		}
		version, err := pinnedVersion(row, read.ExpectedVersion)
		if err != nil {
			return store.Lock{}, err
		}
		return store.Lock{
			Table:         membershipsTable,
			IDs:           []string{rowID(row)},
			ExpectVersion: version,
			TenantWide:    tenantWide,
		}, nil
	default:
		return store.Lock{}, fault.New(fault.Forbidden, "current principal is missing or out of scope")
	}
}

// executionLockFromRow builds the execution row guard used by both the
// execution-running and principal-enablement checks. It always asserts the
// running state and the current credential nonce, so a stopped execution or a
// cleared/rotated execution credential refuses the commit. The row version is
// pinned only when the caller supplied one: an unguarded version is the engine's
// per-observation increment, and pinning a value read moments earlier turned
// that increment into a spurious version_conflict even though the execution was
// still running and the credential current. The lock still serializes the row
// inside the commit transaction.
func executionLockFromRow(identity string, row store.Row, expectedVersion uint64) store.Lock {
	return store.Lock{
		Table:         executionsTable,
		IDs:           []string{identity},
		ExpectVersion: expectedVersion,
		Expect: map[string]any{
			stateColumn:      runningExecutionState,
			tokenNonceColumn: row.Values[tokenNonceColumn],
		},
	}
}

// executionRunningLock re-reads one execution row and guards its running state
// and current credential nonce inside the commit. An explicit caller-observed
// version is still pinned.
func executionRunningLock(
	ctx context.Context,
	executor store.Executor,
	scope tenancy.Scope,
	target string,
	expectedVersion uint64,
) (store.Lock, error) {
	row, err := readRow(ctx, executor, scope, executionsTable, target)
	if err != nil {
		return store.Lock{}, err
	}
	return executionLockFromRow(target, row, expectedVersion), nil
}

// actionDeadlineLock re-reads one action row and asserts that its deadline is
// still strictly after the transaction clock. A deadline that passes between
// the capability's check and this commit is deadline_expired, never a persisted
// verification. The row version is pinned only when the caller supplied one; an
// unguarded version is the engine's per-observation increment, not an authority
// change, and the deadline guard already serializes the row.
func actionDeadlineLock(
	ctx context.Context,
	executor store.Executor,
	scope tenancy.Scope,
	target string,
	expectedVersion uint64,
) (store.Lock, error) {
	if _, err := readRow(ctx, executor, scope, actionsTable, target); err != nil {
		return store.Lock{}, err
	}
	return store.Lock{
		Table:         actionsTable,
		IDs:           []string{target},
		ExpectVersion: expectedVersion,
		Deadline:      &store.DeadlineGuard{Column: "deadline"},
	}, nil
}

// requireRequiredRoles refuses forbidden when the read records required roles
// and the row holds none of them. An empty required set is not a role
// constraint. The any-of semantics and the roles JSON source match the identity
// AuthorityReader.
func requireRequiredRoles(row store.Row, required []tenancy.Role) error {
	if len(required) == 0 {
		return nil
	}
	roles, err := decodeRowRoles(row)
	if err != nil {
		return err
	}
	if !hasAnyRole(roles, required) {
		return fault.New(fault.Forbidden, fmt.Sprintf("%s row does not currently hold a required role", row.Table))
	}
	return nil
}

// requireRowState refuses forbidden when the row's state is not the expected
// one, before any version is pinned.
func requireRowState(row store.Row, want string) error {
	state, err := textValue(row, stateColumn)
	if err != nil {
		return err
	}
	if state != want {
		return fault.New(fault.Forbidden, fmt.Sprintf("%s row is not currently %s", row.Table, want))
	}
	return nil
}

// decodeRowRoles decodes the roles JSON column of a membership or service row.
func decodeRowRoles(row store.Row) ([]tenancy.Role, error) {
	data, err := bytesValue(row, rolesColumn)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var roles []tenancy.Role
	if err := json.Unmarshal(data, &roles); err != nil {
		return nil, fault.Wrap(fault.InternalFailure,
			fmt.Sprintf("%s row column %q does not decode as roles", row.Table, rolesColumn), err)
	}
	return roles, nil
}

// hasAnyRole reports whether roles contains at least one required role.
func hasAnyRole(roles, required []tenancy.Role) bool {
	for _, wanted := range required {
		if slices.Contains(roles, wanted) {
			return true
		}
	}
	return false
}

// resolvePrincipalRow finds the durable row that represents one principal for
// the enablement checks: its execution row, its service row or its human
// membership row, in that order. A project-scoped lookup prefers the exact
// project row and falls back to the tenant-wide row, reporting which predicate
// matched. Membership AUTHORITY does not use this resolution: a membership
// authority read observes and pins both applicable rows (membership.go).
func resolvePrincipalRow(
	ctx context.Context,
	executor store.Executor,
	scanner store.Scanner,
	scope tenancy.Scope,
	principal id.PrincipalID,
) (store.Row, principalKind, bool, error) {
	if scope.ProjectScoped() {
		row, found, err := findExecutionByPrincipal(ctx, scanner, scope, principal)
		if err != nil {
			return store.Row{}, principalMissing, false, err
		}
		if found {
			return row, principalExecution, false, nil
		}
	}
	row, found, tenantWide, err := readNullableScope(ctx, executor, scope, servicesTable, string(principal))
	if err != nil {
		return store.Row{}, principalMissing, false, err
	}
	if found {
		return row, principalService, tenantWide, nil
	}
	row, found, tenantWide, err = findMembership(ctx, scanner, scope, principal)
	if err != nil {
		return store.Row{}, principalMissing, false, err
	}
	if found {
		return row, principalHuman, tenantWide, nil
	}
	return store.Row{}, principalMissing, false, nil
}

// findExecutionByPrincipal returns the project-scoped execution row of one
// execution principal. Executions are project-scoped, so a tenant-scoped read
// resolves nothing.
func findExecutionByPrincipal(
	ctx context.Context,
	scanner store.Scanner,
	scope tenancy.Scope,
	principal id.PrincipalID,
) (store.Row, bool, error) {
	if scanner == nil {
		return store.Row{}, false, fault.New(fault.InternalFailure, "principal resolution requires a scanner")
	}
	if scope.TenantScoped() {
		return store.Row{}, false, nil
	}
	page, err := scanner.Query(ctx, scope, store.ScanQuery{
		Table:   executionsTable,
		Equals:  map[string]any{principalColumn: string(principal)},
		OrderBy: []store.Order{{Column: idColumn}},
		Limit:   1,
	})
	if err != nil {
		return store.Row{}, false, err
	}
	if len(page.Rows) == 0 {
		return store.Row{}, false, nil
	}
	return page.Rows[0], true, nil
}

// findMembership returns the scoped human membership row of one principal. A
// project-scoped lookup prefers the exact project grant and falls back to the
// tenant-wide grant that covers the project; a tenant-scoped lookup reads only
// tenant-wide grants.
func findMembership(
	ctx context.Context,
	scanner store.Scanner,
	scope tenancy.Scope,
	principal id.PrincipalID,
) (store.Row, bool, bool, error) {
	if scanner == nil {
		return store.Row{}, false, false, fault.New(fault.InternalFailure, "principal resolution requires a scanner")
	}
	scopes := []struct {
		scope      tenancy.Scope
		tenantWide bool
	}{{scope: scope}}
	if scope.ProjectScoped() {
		scopes = append(scopes, struct {
			scope      tenancy.Scope
			tenantWide bool
		}{scope: tenancy.Scope{TenantID: scope.TenantID}, tenantWide: true})
	}
	for _, candidate := range scopes {
		page, err := scanner.Query(ctx, candidate.scope, store.ScanQuery{
			Table:   membershipsTable,
			Equals:  map[string]any{principalColumn: string(principal)},
			OrderBy: []store.Order{{Column: idColumn}},
			Limit:   1,
		})
		if err != nil {
			return store.Row{}, false, false, err
		}
		if len(page.Rows) > 0 {
			return page.Rows[0], true, candidate.tenantWide, nil
		}
	}
	return store.Row{}, false, false, nil
}

// readNullableScope reads one row from a table whose project column is
// nullable. A project-scoped caller first reads the exact project row and then
// the tenant-wide row, reporting which predicate matched so the lock can carry
// the same TenantWide target.
func readNullableScope(
	ctx context.Context,
	executor store.Executor,
	scope tenancy.Scope,
	table string,
	identity string,
) (store.Row, bool, bool, error) {
	row, err := readRow(ctx, executor, scope, table, identity)
	if err == nil {
		return row, true, false, nil
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		return store.Row{}, false, false, err
	}
	if !scope.ProjectScoped() {
		return store.Row{}, false, false, nil
	}
	row, err = readRow(ctx, executor, tenancy.Scope{TenantID: scope.TenantID}, table, identity)
	if err == nil {
		return row, true, true, nil
	}
	if class, ok := fault.ClassOf(err); ok && class == fault.NotFound {
		return store.Row{}, false, false, nil
	}
	return store.Row{}, false, false, err
}

// readRow reads one scoped row through the executor seam.
func readRow(
	ctx context.Context,
	executor store.Executor,
	scope tenancy.Scope,
	table string,
	identity string,
) (store.Row, error) {
	if executor == nil {
		return store.Row{}, fault.New(fault.InternalFailure, "authority read requires an executor")
	}
	if identity == "" {
		return store.Row{}, fault.New(fault.InvalidInput, "authority read has no row identity")
	}
	return executor.Read(ctx, scope, table, identity)
}

// rowID returns the stored identity of one row.
func rowID(row store.Row) string {
	identity, err := textValue(row, idColumn)
	if err != nil {
		return ""
	}
	return identity
}

// pinnedVersion returns the caller-supplied observed version when it has one,
// otherwise the version the adapter re-read from the durable row.
func pinnedVersion(row store.Row, expected uint64) (uint64, error) {
	if expected != 0 {
		return expected, nil
	}
	return confirmedVersion(row)
}
