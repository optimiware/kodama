package kv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/shell/authorize"
	"github.com/optimiware/kodema/internal/shell/event"
	"github.com/optimiware/kodema/internal/shell/identity"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

const (
	kvPutOperation       = "kv.put"
	kvGetOperation       = "kv.get"
	membershipsTable     = "memberships"
	servicesTable        = "services"
	executionsTable      = "executions"
	actionsTable         = "actions"
	serviceEnabledState  = "enabled"
	executionRunning     = "running"
	actionContainerKind  = "container"
	actionRunningState   = "running"
	rowIDColumn          = "id"
	rowVersionColumn     = "version"
	rowRolesColumn       = "roles"
	rowStateColumn       = "state"
	rowPrincipalColumn   = "principal_id"
	rowExecutionIDColumn = "execution_id"
	rowDeadlineColumn    = "deadline"
)

// Config constructs the project-scoped KV capability service.
// Objects is optional: when nil, NewService wires the default StoreBackend
// over Executor so local relational blob storage works out of the box.
type Config struct {
	Clock    Clock
	IDs      IDSource
	Executor store.Executor
	Scanner  store.Scanner
	Objects  ObjectStore
}

// Service implements authorized, project-scoped, content-addressed immutable
// KV storage.
type Service struct {
	clock    Clock
	ids      IDSource
	executor store.Executor
	scanner  store.Scanner
	objects  ObjectStore
}

// NewService validates all required ports and returns the KV service.
func NewService(config Config) (*Service, error) {
	switch {
	case config.Clock == nil:
		return nil, fault.New(fault.InvalidInput, "kv service requires a clock")
	case config.IDs == nil:
		return nil, fault.New(fault.InvalidInput, "kv service requires an identity source")
	case config.Executor == nil:
		return nil, fault.New(fault.InvalidInput, "kv service requires a store executor")
	case config.Scanner == nil:
		return nil, fault.New(fault.InvalidInput, "kv service requires a store scanner")
	}
	objects := config.Objects
	if objects == nil {
		backend, err := NewStoreBackend(config.Executor)
		if err != nil {
			return nil, err
		}
		objects = backend
	}
	return &Service{
		clock:    config.Clock,
		ids:      config.IDs,
		executor: config.Executor,
		scanner:  config.Scanner,
		objects:  objects,
	}, nil
}

// Put stores bounded immutable bytes under command.Scope. The key is derived
// strictly server-side from SHA-256(bytes). Re-putting identical bytes in the
// same project scope returns Existed=true with the existing metadata without
// mutating the stored object.
func (service *Service) Put(ctx context.Context, command PutCommand) (PutResult, error) {
	if err := validateProjectScope(command.Scope); err != nil {
		return PutResult{}, err
	}
	if err := command.Actor.Validate(); err != nil {
		return PutResult{}, err
	}
	if !command.Actor.Enabled {
		return PutResult{}, fault.New(fault.Forbidden, "acting principal is disabled")
	}
	if !scopeContains(command.Actor.Scope, command.Scope) {
		return PutResult{}, fault.New(fault.Forbidden, "acting principal scope does not cover target project")
	}
	if len(command.Data) > MaxValueBytes {
		return PutResult{}, fault.New(fault.InvalidInput, fmt.Sprintf("kv value is %d bytes; at most %d are allowed", len(command.Data), MaxValueBytes))
	}
	contentType, err := NormalizeContentType(command.ContentType, DefaultContentType)
	if err != nil {
		return PutResult{}, err
	}
	now := service.clock.Now()
	if now.IsZero() {
		return PutResult{}, fault.New(fault.InvalidInput, "kv commit time is zero")
	}

	// Verify current authority BEFORE probing object existence so an
	// unauthorized caller can never distinguish existing vs missing hashes.
	locks, err := service.authorityLocks(ctx, command.Scope, command.Actor, kvPutOperation, now)
	if err != nil {
		return PutResult{}, err
	}

	key := KeyFor(command.Data)
	rowID, err := RowID(command.Scope, key)
	if err != nil {
		return PutResult{}, err
	}
	auditID, err := service.ids.NewEventID()
	if err != nil {
		return PutResult{}, err
	}
	record := PutRecord{
		RowID: rowID,
		Metadata: Metadata{
			Key:         key,
			Scope:       command.Scope,
			ContentType: contentType,
			SizeBytes:   int64(len(command.Data)),
			CreatedBy:   command.Actor.ID,
			CreatedAt:   now,
		},
		Data:  command.Data,
		Locks: locks,
		Audit: []event.Audit{{
			ID:        auditID,
			Scope:     command.Scope,
			Actor:     command.Actor.ID,
			Operation: kvPutOperation,
			At:        now,
			Detail:    string(key),
		}},
	}
	meta, existed, err := service.objects.Put(ctx, record)
	if err != nil {
		if isVersionConflict(err) {
			// A concurrent identical put in the same project may have inserted
			// the same content hash first. Re-verify current authority before
			// converging idempotently on the committed object.
			if obj, getErr := service.objects.Get(ctx, command.Scope, key); getErr == nil && bytes.Equal(obj.Data, command.Data) {
				if _, authErr := service.authorityLocks(ctx, command.Scope, command.Actor, kvPutOperation, service.clock.Now()); authErr != nil {
					return PutResult{}, authErr
				}
				return PutResult{Metadata: obj.Metadata, Existed: true}, nil
			}
		}
		return PutResult{}, err
	}
	return PutResult{Metadata: meta, Existed: existed}, nil
}

// Get retrieves the immutable bytes and metadata for one canonical KV key in
// command.Scope. Keys belonging to another project or tenant return not_found.
func (service *Service) Get(ctx context.Context, command GetCommand) (Object, error) {
	if err := validateProjectScope(command.Scope); err != nil {
		return Object{}, err
	}
	if err := command.Actor.Validate(); err != nil {
		return Object{}, err
	}
	if !command.Actor.Enabled {
		return Object{}, fault.New(fault.Forbidden, "acting principal is disabled")
	}
	if !scopeContains(command.Actor.Scope, command.Scope) {
		return Object{}, fault.New(fault.Forbidden, "acting principal scope does not cover target project")
	}
	key, err := ParseKey(string(command.Key))
	if err != nil {
		return Object{}, err
	}
	now := service.clock.Now()
	if now.IsZero() {
		return Object{}, fault.New(fault.InvalidInput, "kv read time is zero")
	}
	if _, err := service.authorityLocks(ctx, command.Scope, command.Actor, kvGetOperation, now); err != nil {
		return Object{}, err
	}
	return service.objects.Get(ctx, command.Scope, key)
}

func validateProjectScope(scope tenancy.Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if !scope.ProjectScoped() {
		return fault.New(fault.InvalidInput, "kv operations require a project scope")
	}
	return nil
}

func scopeContains(owner, target tenancy.Scope) bool {
	if owner.TenantID != target.TenantID {
		return false
	}
	if owner.ProjectID == "" {
		return true
	}
	return owner.ProjectID == target.ProjectID
}

// authorityLocks re-reads the acting principal's current durable authority
// rows for operation in scope and returns the commit-boundary locks that pin
// them. Both Put and Get use this so authorization never relies on a stale
// principal snapshot.
func (service *Service) authorityLocks(
	ctx context.Context,
	scope tenancy.Scope,
	actor identity.Principal,
	operation string,
	now time.Time,
) ([]store.Lock, error) {
	switch actor.Kind {
	case identity.KindHuman:
		return service.membershipGuard(ctx, scope, actor.ID, operation)
	case identity.KindService:
		guard, err := service.serviceGuard(ctx, scope, actor.ID, operation)
		if err != nil {
			return nil, err
		}
		return []store.Lock{guard}, nil
	case identity.KindExecution:
		return service.executionGuard(ctx, scope, actor, operation, now)
	default:
		return nil, fault.New(fault.Forbidden, "unsupported principal kind for kv operation")
	}
}

func (service *Service) membershipGuard(
	ctx context.Context,
	scope tenancy.Scope,
	actor id.PrincipalID,
	operation string,
) ([]store.Lock, error) {
	registered, ok := authorize.LookupOperation(authorize.OperationName(operation))
	if !ok {
		return nil, fault.New(fault.InternalFailure, "kv operation is not registered")
	}
	tenantScope := tenancy.Scope{TenantID: scope.TenantID}
	tenantRow, err := service.readObservedMembership(ctx, tenantScope, actor)
	if err != nil {
		return nil, err
	}
	projectRow, err := service.readObservedMembership(ctx, scope, actor)
	if err != nil {
		return nil, err
	}
	fact, err := authorize.EvaluateMembership(authorize.AuthorityRead{
		Operation:   registered.Name,
		Required:    registered.Requires,
		Check:       authorize.CheckMembership,
		Scope:       scope,
		PrincipalID: actor,
	}, false, authorize.MembershipRows{
		TenantWide: tenantRow,
		Project:    projectRow,
	})
	if err != nil {
		return nil, err
	}
	locks := make([]store.Lock, 0, len(fact.Targets))
	for _, target := range fact.Targets {
		if target.Absent {
			var projectOrNil any
			if target.Scope.ProjectScoped() {
				projectOrNil = string(target.Scope.ProjectID)
			}
			locks = append(locks, store.Lock{
				Table:        membershipsTable,
				ExpectAbsent: true,
				Key: map[string]any{
					"tenant_id":        string(scope.TenantID),
					"project_id":       projectOrNil,
					rowPrincipalColumn: string(actor),
				},
			})
			continue
		}
		locks = append(locks, store.Lock{
			Table:         membershipsTable,
			IDs:           []string{target.RowID},
			ExpectVersion: target.ExpectedVersion,
			TenantWide:    target.Scope.TenantScoped(),
			Expect:        map[string]any{rowPrincipalColumn: string(actor)},
		})
	}
	return locks, nil
}

func (service *Service) readObservedMembership(ctx context.Context, scope tenancy.Scope, actor id.PrincipalID) (authorize.MembershipRow, error) {
	page, err := service.scanner.Query(ctx, scope, store.ScanQuery{
		Table:   membershipsTable,
		Equals:  map[string]any{rowPrincipalColumn: string(actor)},
		OrderBy: []store.Order{{Column: rowIDColumn}},
		Limit:   2,
	})
	if err != nil {
		return authorize.MembershipRow{}, err
	}
	if len(page.Rows) > 1 || (len(page.Rows) == 1 && page.HasMore) {
		return authorize.MembershipRow{}, fault.New(fault.InternalFailure, "acting principal has more than one membership row in one scope")
	}
	if len(page.Rows) == 0 {
		return authorize.MembershipRow{}, nil
	}
	row := page.Rows[0]
	principal, ok := row.Values[rowPrincipalColumn].(string)
	if !ok || principal != string(actor) {
		return authorize.MembershipRow{}, fault.New(fault.Forbidden, "acting principal's current membership does not match the actor")
	}
	roles, err := decodeRowRoles(row.Values[rowRolesColumn])
	if err != nil {
		return authorize.MembershipRow{}, err
	}
	version, ok := rowUint64(row.Values[rowVersionColumn])
	if !ok {
		return authorize.MembershipRow{}, fault.New(fault.InternalFailure, "acting principal's membership has no usable version")
	}
	observedID, ok := row.Values[rowIDColumn].(string)
	if !ok || observedID == "" {
		return authorize.MembershipRow{}, fault.New(fault.InternalFailure, "acting principal's membership has no usable identity")
	}
	return authorize.MembershipRow{
		RowID: observedID,
		Membership: &tenancy.Membership{
			Scope:       scope,
			PrincipalID: actor,
			Roles:       roles,
			Version:     version,
		},
	}, nil
}

func (service *Service) serviceGuard(
	ctx context.Context,
	scope tenancy.Scope,
	actor id.PrincipalID,
	operation string,
) (store.Lock, error) {
	registered, ok := authorize.LookupOperation(authorize.OperationName(operation))
	if !ok {
		return store.Lock{}, fault.New(fault.InternalFailure, "kv operation is not registered")
	}
	row, tenantWide, err := service.readServiceRow(ctx, scope, actor)
	if err != nil {
		if isNotFound(err) {
			return store.Lock{}, fault.New(fault.Forbidden, "acting service has no current service account in scope")
		}
		return store.Lock{}, err
	}
	state, ok := row.Values[rowStateColumn].(string)
	if !ok {
		return store.Lock{}, fault.New(fault.InternalFailure, "acting service row has no usable state")
	}
	if state != serviceEnabledState {
		return store.Lock{}, fault.New(fault.Forbidden, "acting service is not enabled")
	}
	roles, err := decodeRowRoles(row.Values[rowRolesColumn])
	if err != nil {
		return store.Lock{}, err
	}
	for _, role := range roles {
		if role == tenancy.RoleAdmin {
			return store.Lock{}, fault.New(fault.Forbidden, "service accounts may not hold Admin")
		}
	}
	if !registered.Requires.AllowsRoles(true, roles) {
		return store.Lock{}, fault.New(fault.Forbidden, "acting service is not authorized for operation "+operation)
	}
	version, ok := rowUint64(row.Values[rowVersionColumn])
	if !ok {
		return store.Lock{}, fault.New(fault.InternalFailure, "acting service row has no usable version")
	}
	observedID, ok := row.Values[rowIDColumn].(string)
	if !ok || observedID != string(actor) {
		return store.Lock{}, fault.New(fault.Forbidden, "acting service row does not match the actor")
	}
	return store.Lock{
		Table:         servicesTable,
		IDs:           []string{observedID},
		TenantWide:    tenantWide,
		ExpectVersion: version,
		Expect:        map[string]any{rowStateColumn: serviceEnabledState},
	}, nil
}

func (service *Service) readServiceRow(ctx context.Context, scope tenancy.Scope, actor id.PrincipalID) (store.Row, bool, error) {
	row, err := service.executor.Read(ctx, scope, servicesTable, string(actor))
	if err == nil {
		return row, rowTenantWide(row), nil
	}
	if !isNotFound(err) {
		return store.Row{}, false, err
	}
	tenantScope := tenancy.Scope{TenantID: scope.TenantID}
	row, err = service.executor.Read(ctx, tenantScope, servicesTable, string(actor))
	if err != nil {
		return store.Row{}, false, err
	}
	return row, true, nil
}

func (service *Service) executionGuard(
	ctx context.Context,
	scope tenancy.Scope,
	actor identity.Principal,
	operation string,
	now time.Time,
) ([]store.Lock, error) {
	registered, ok := authorize.LookupOperation(authorize.OperationName(operation))
	if !ok {
		return nil, fault.New(fault.InternalFailure, "kv operation is not registered")
	}
	if registered.Requires.Execution == authorize.ExecNone {
		return nil, fault.New(fault.Forbidden, "execution is not authorized for operation "+operation)
	}
	if actor.ExecutionID == "" {
		return nil, fault.New(fault.Forbidden, "execution principal has no execution id")
	}
	execRow, err := service.executor.Read(ctx, scope, executionsTable, string(actor.ExecutionID))
	if err != nil {
		if isNotFound(err) {
			return nil, fault.New(fault.Forbidden, "execution is not visible in scope")
		}
		return nil, err
	}
	principalID, ok := execRow.Values[rowPrincipalColumn].(string)
	if !ok || principalID != string(actor.ID) {
		return nil, fault.New(fault.Forbidden, "execution principal does not match stored execution")
	}
	state, ok := execRow.Values[rowStateColumn].(string)
	if !ok || state != executionRunning {
		return nil, fault.New(fault.Forbidden, "execution is not running")
	}
	execVersion, ok := rowUint64(execRow.Values[rowVersionColumn])
	if !ok {
		return nil, fault.New(fault.InternalFailure, "execution row has no usable version")
	}

	// Verify the execution currently has an active, unexpired container action.
	page, err := service.scanner.Query(ctx, scope, store.ScanQuery{
		Table: actionsTable,
		Equals: map[string]any{
			rowExecutionIDColumn: string(actor.ExecutionID),
			"kind":               actionContainerKind,
			rowStateColumn:       actionRunningState,
		},
		OrderBy: []store.Order{{Column: rowIDColumn}},
		Limit:   2,
	})
	if err != nil {
		return nil, err
	}
	if len(page.Rows) == 0 {
		return nil, fault.New(fault.Forbidden, "execution has no running container action")
	}
	if len(page.Rows) > 1 {
		return nil, fault.New(fault.InternalFailure, "execution has more than one running container action")
	}
	actionRow := page.Rows[0]
	actionID, ok := actionRow.Values[rowIDColumn].(string)
	if !ok || actionID == "" {
		return nil, fault.New(fault.InternalFailure, "running action has no usable id")
	}
	actionVersion, ok := rowUint64(actionRow.Values[rowVersionColumn])
	if !ok {
		return nil, fault.New(fault.InternalFailure, "running action has no usable version")
	}
	deadline, ok := actionRow.Values[rowDeadlineColumn].(time.Time)
	if !ok || !deadline.After(now) {
		return nil, fault.New(fault.DeadlineExpired, "running container action deadline has expired")
	}

	return []store.Lock{
		{
			Table:         executionsTable,
			IDs:           []string{string(actor.ExecutionID)},
			ExpectVersion: execVersion,
			Expect:        map[string]any{rowStateColumn: executionRunning},
		},
		{
			Table:         actionsTable,
			IDs:           []string{actionID},
			ExpectVersion: actionVersion,
			Expect: map[string]any{
				rowStateColumn: actionRunningState,
				"kind":         actionContainerKind,
			},
			Deadline: &store.DeadlineGuard{Column: rowDeadlineColumn},
		},
	}, nil
}

func rowTenantWide(row store.Row) bool {
	value, present := row.Values["project_id"]
	if !present || value == nil {
		return present
	}
	text, ok := value.(string)
	return ok && text == ""
}

func decodeRowRoles(value any) ([]tenancy.Role, error) {
	switch stored := value.(type) {
	case []byte:
		var names []string
		if err := json.Unmarshal(stored, &names); err != nil {
			return nil, fault.Wrap(fault.InternalFailure, "roles could not be decoded", err)
		}
		return parseRoles(names)
	case []string:
		return parseRoles(stored)
	case []any:
		names := make([]string, 0, len(stored))
		for _, item := range stored {
			name, ok := item.(string)
			if !ok {
				return nil, fault.New(fault.InternalFailure, "role entry is not a string")
			}
			names = append(names, name)
		}
		return parseRoles(names)
	default:
		return nil, fault.New(fault.InternalFailure, "authority row has no usable roles")
	}
}

func parseRoles(names []string) ([]tenancy.Role, error) {
	roles := make([]tenancy.Role, 0, len(names))
	for _, name := range names {
		role := tenancy.Role(name)
		if !role.Valid() {
			return nil, fault.New(fault.InternalFailure, "authority row carries an unknown role")
		}
		roles = append(roles, role)
	}
	return roles, nil
}

func rowUint64(value any) (uint64, bool) {
	switch number := value.(type) {
	case int64:
		if number <= 0 {
			return 0, false
		}
		return uint64(number), true
	case uint64:
		return number, number > 0
	case int:
		if number <= 0 {
			return 0, false
		}
		return uint64(number), true
	case uint32:
		return uint64(number), number > 0
	case float64:
		if number <= 0 || number != float64(uint64(number)) {
			return 0, false
		}
		return uint64(number), true
	default:
		return 0, false
	}
}
