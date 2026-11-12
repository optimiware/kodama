// Secret management: encrypted current-value writes and metadata-only reads.
//
// Every mutation validates and seals before it asks the neutral store executor
// to commit one complete unit. The sealed envelope is the only value the row
// carries; the plaintext exists only in memory for the seal call and never
// enters the receipt result, audit detail, event data or an error message.
//
// A replacement and a delete re-read the current stored row but always commit
// the caller's expected version as the optimistic guard. A retry therefore
// reaches the executor's receipt replay instead of failing on a row that its
// own earlier attempt advanced, while a genuinely stale caller still fails the
// guard with version_conflict.
package secret

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	secretsTable          = "secrets"
	membershipsTable      = "memberships"
	servicesTable         = "services"
	secretWriteOperation  = "secret.write"
	secretDeleteOperation = "secret.delete"
	rowIDColumn           = "id"
	rowRolesColumn        = "roles"
	rowVersionColumn      = "version"
	membershipPrincipal   = "principal_id"
	serviceStateColumn    = "state"
	serviceEnabledState   = "enabled"
	secretTableVersionCol = "version"
	secretTableCreatedCol = "created_at"
	secretTableUpdatedCol = "updated_at"
	secretTableProjectCol = "project_id"
	secretTableTenantCol  = "tenant_id"
	secretTableNameCol    = "name"
	secretTableIDCol      = "id"
	secretTableKeyRefCol  = "key_ref"
	secretTableKeyVerCol  = "key_version"
	secretTableNonceCol   = "nonce"
	secretTableWrappedCol = "wrapped_key"
	secretTableWrapNonce  = "wrap_nonce"
	secretTableCipherCol  = "ciphertext"
)

// Manager writes, replaces and deletes one current secret value through one
// guarded store commit unit. Reads return Metadata only.
type Manager struct {
	installationID string
	keyVersion     KeyVersion
	clock          Clock
	ids            IDSource
	executor       store.Executor
	scanner        store.Scanner
	cipher         EnvelopeCipher
	lookup         NameLookup
}

// NewManager validates every injected boundary and returns the management
// service. A missing port, an empty installation id or a non-positive key
// version is a construction failure rather than a partially wired manager.
func NewManager(config ManagerConfig) (*Manager, error) {
	switch {
	case config.InstallationID == "":
		return nil, fault.New(fault.InvalidInput, "secret manager requires an installation id")
	case config.KeyVersion < MinKeyVersion:
		return nil, fault.New(fault.InvalidInput, "secret manager requires a positive key version")
	case config.Clock == nil:
		return nil, fault.New(fault.InvalidInput, "secret manager requires a clock")
	case config.IDs == nil:
		return nil, fault.New(fault.InvalidInput, "secret manager requires an identity source")
	case config.Executor == nil:
		return nil, fault.New(fault.InvalidInput, "secret manager requires a store executor")
	case config.Scanner == nil:
		return nil, fault.New(fault.InvalidInput, "secret manager requires a store scanner")
	case config.Cipher == nil:
		return nil, fault.New(fault.InvalidInput, "secret manager requires an envelope cipher")
	case config.Lookup == nil:
		return nil, fault.New(fault.InvalidInput, "secret manager requires a name lookup")
	}
	return &Manager{
		installationID: config.InstallationID,
		keyVersion:     config.KeyVersion,
		clock:          config.Clock,
		ids:            config.IDs,
		executor:       config.Executor,
		scanner:        config.Scanner,
		cipher:         config.Cipher,
		lookup:         config.Lookup,
	}, nil
}

// Put inserts or replaces one current encrypted value and returns its readable
// metadata. The plaintext is sealed before the commit and never serialized.
func (manager *Manager) Put(ctx context.Context, command PutCommand) (Metadata, error) {
	if err := manager.validatePut(command); err != nil {
		return Metadata{}, err
	}
	now := manager.clock.Now()
	if now.IsZero() {
		return Metadata{}, fault.New(fault.InvalidInput, "secret commit time is zero")
	}

	var (
		secretID  SecretID
		createdAt time.Time
		version   uint64
		expected  uint64
		insert    bool
	)
	if command.ExpectedVersion == 0 {
		allocated, err := manager.ids.NewSecretID()
		if err != nil {
			return Metadata{}, err
		}
		secretID = allocated
		createdAt = now
		version = 1
		insert = true
	} else {
		stored, err := manager.lookup.Lookup(ctx, command.Scope, command.Name)
		if err != nil {
			return Metadata{}, err
		}
		if err := validateStoredIdentity(stored, command.Scope, command.Name); err != nil {
			return Metadata{}, err
		}
		// Keep the caller's expected version as the write guard even when the
		// stored row is already newer. A retry then reaches the executor's
		// receipt replay instead of failing here, while a genuinely stale caller
		// still fails the write guard with version_conflict.
		secretID = stored.Metadata.ID
		createdAt = stored.CreatedAt
		expected = command.ExpectedVersion
		version = command.ExpectedVersion + 1
	}

	associated := AssociatedData{
		InstallationID: manager.installationID,
		TenantID:       command.Scope.TenantID,
		ProjectID:      command.Scope.ProjectID,
		SecretID:       secretID,
		Name:           command.Name,
		KeyVersion:     manager.keyVersion,
	}
	if err := associated.Validate(); err != nil {
		return Metadata{}, err
	}
	envelope, err := manager.cipher.Seal(ctx, associated, []byte(command.Value))
	if err != nil {
		return Metadata{}, err
	}
	if err := envelope.ValidateFor(associated); err != nil {
		return Metadata{}, err
	}

	metadata := Metadata{
		ID:        secretID,
		Scope:     command.Scope,
		Name:      command.Name,
		Version:   version,
		UpdatedAt: now,
	}
	write := store.Write{
		Table:         secretsTable,
		ID:            string(secretID),
		Insert:        insert,
		ExpectVersion: expected,
		Values:        secretRowValues(command.Name, createdAt, now, version, envelope, insert),
	}
	unit, err := manager.commitUnit(ctx, command.Scope, command.Actor, command.ActorKind, secretWriteOperation, command.CallerKey, command.Content, string(secretID), version, metadata, now)
	if err != nil {
		return Metadata{}, err
	}
	unit.Writes = []store.Write{write}
	result, err := manager.executor.Execute(ctx, command.Scope, unit)
	if err != nil {
		return Metadata{}, err
	}
	if result.Replayed {
		// The executor replayed an equal receipt, so this attempt's freshly
		// allocated identity and metadata were never committed. Return the
		// original durable metadata instead of the speculative one.
		return replayedMetadata(result.Receipt)
	}
	return metadata, nil
}

// replayedMetadata decodes the durable metadata stored in the original receipt
// result of an equal secret write. A receipt without usable committed metadata
// is an internal inconsistency, never a silent zero value.
func replayedMetadata(record receipt.Record) (Metadata, error) {
	var committed Metadata
	if err := json.Unmarshal(record.Result, &committed); err != nil {
		return Metadata{}, fault.Wrap(fault.InternalFailure, "replayed secret result could not be decoded", err)
	}
	if committed.ID == "" || committed.Version == 0 {
		return Metadata{}, fault.New(fault.InternalFailure, "replayed secret result has no committed metadata")
	}
	return committed, nil
}

// Delete removes one current value with a guarded delete. It never seals and
// never writes a value row.
func (manager *Manager) Delete(ctx context.Context, command DeleteCommand) error {
	if err := manager.validateDelete(command); err != nil {
		return err
	}
	now := manager.clock.Now()
	if now.IsZero() {
		return fault.New(fault.InvalidInput, "secret commit time is zero")
	}
	stored, err := manager.lookup.Lookup(ctx, command.Scope, command.Name)
	switch {
	case err == nil:
		if err := validateStoredIdentity(stored, command.Scope, command.Name); err != nil {
			return err
		}
	case isNotFound(err):
		// The current row is not visible. A retried delete must still reach the
		// executor's receipt replay, and a delete with no matching receipt must
		// fail its guard instead of silently succeeding. Guard a fresh identity
		// that cannot alias a committed row with the caller's expected version.
		placeholder, allocErr := manager.ids.NewSecretID()
		if allocErr != nil {
			return allocErr
		}
		stored = Stored{
			Metadata: Metadata{
				ID:      placeholder,
				Scope:   command.Scope,
				Name:    command.Name,
				Version: command.ExpectedVersion,
			},
		}
	default:
		return err
	}
	write := store.Write{
		Table:         secretsTable,
		ID:            string(stored.Metadata.ID),
		Delete:        true,
		ExpectVersion: command.ExpectedVersion,
	}
	unit, err := manager.commitUnit(ctx, command.Scope, command.Actor, command.ActorKind, secretDeleteOperation, command.CallerKey, command.Content, string(stored.Metadata.ID), command.ExpectedVersion, nil, now)
	if err != nil {
		return err
	}
	unit.Writes = []store.Write{write}
	_, err = manager.executor.Execute(ctx, command.Scope, unit)
	return err
}

// Get reads one stored row through the scoped store executor and returns
// metadata only. The ciphertext column is deliberately never decoded into the
// result.
func (manager *Manager) Get(ctx context.Context, scope tenancy.Scope, secretID SecretID) (Metadata, error) {
	if err := scope.Validate(); err != nil {
		return Metadata{}, err
	}
	if _, err := ParseSecretID(string(secretID)); err != nil {
		return Metadata{}, err
	}
	row, err := manager.executor.Read(ctx, scope, secretsTable, string(secretID))
	if err != nil {
		return Metadata{}, err
	}
	return metadataFromRow(row, scope)
}

// GetByName reads one stored secret by its scoped declared name through the
// name lookup seam and returns metadata only. The encrypted envelope is never
// returned or opened.
func (manager *Manager) GetByName(ctx context.Context, scope tenancy.Scope, name id.SecretName) (Metadata, error) {
	if err := scope.Validate(); err != nil {
		return Metadata{}, err
	}
	parsed, err := id.ParseSecretName(string(name))
	if err != nil {
		return Metadata{}, fault.Wrap(fault.InvalidInput, "secret name is invalid", err)
	}
	stored, err := manager.lookup.Lookup(ctx, scope, parsed)
	if err != nil {
		return Metadata{}, err
	}
	if err := validateStoredIdentity(stored, scope, parsed); err != nil {
		return Metadata{}, err
	}
	return stored.Metadata, nil
}

func (manager *Manager) validatePut(command PutCommand) error {
	if err := command.Scope.Validate(); err != nil {
		return err
	}
	if _, err := id.ParseSecretName(string(command.Name)); err != nil {
		return fault.Wrap(fault.InvalidInput, "secret name is invalid", err)
	}
	if err := DefaultBounds.CheckValue(len(command.Value)); err != nil {
		return err
	}
	if _, err := id.ParsePrincipalID(string(command.Actor)); err != nil {
		return fault.Wrap(fault.InvalidInput, "secret actor is invalid", err)
	}
	if err := validateSecretActorKind(command.ActorKind); err != nil {
		return err
	}
	if command.CallerKey == "" {
		return fault.New(fault.InvalidInput, "secret caller key is empty")
	}
	if _, err := receipt.Fingerprint(command.Content); err != nil {
		return err
	}
	return nil
}

func (manager *Manager) validateDelete(command DeleteCommand) error {
	if err := command.Scope.Validate(); err != nil {
		return err
	}
	if _, err := id.ParseSecretName(string(command.Name)); err != nil {
		return fault.Wrap(fault.InvalidInput, "secret name is invalid", err)
	}
	if command.ExpectedVersion == 0 {
		return fault.New(fault.InvalidInput, "secret delete expected version is zero")
	}
	if _, err := id.ParsePrincipalID(string(command.Actor)); err != nil {
		return fault.Wrap(fault.InvalidInput, "secret actor is invalid", err)
	}
	if err := validateSecretActorKind(command.ActorKind); err != nil {
		return err
	}
	if command.CallerKey == "" {
		return fault.New(fault.InvalidInput, "secret caller key is empty")
	}
	if _, err := receipt.Fingerprint(command.Content); err != nil {
		return err
	}
	return nil
}

// validateSecretActorKind accepts only the two principal kinds a secret
// mutation can carry. An execution principal never writes or deletes a stored
// secret, and an unknown kind fails closed rather than guessing a guard.
func validateSecretActorKind(kind identity.PrincipalKind) error {
	switch kind {
	case identity.KindHuman, identity.KindService:
		return nil
	default:
		return fault.New(fault.InvalidInput, "secret actor kind must be human or service")
	}
}

// commitUnit builds the receipt, audit row and config.changed publication
// intent that commit with the business write. It never carries the plaintext.
func (manager *Manager) commitUnit(
	ctx context.Context,
	scope tenancy.Scope,
	actor id.PrincipalID,
	actorKind identity.PrincipalKind,
	operation string,
	callerKey string,
	content receipt.Content,
	subject string,
	subjectVersion uint64,
	result any,
	at time.Time,
) (store.Unit, error) {
	guards, err := manager.authorityGuard(ctx, scope, actor, actorKind, operation)
	if err != nil {
		return store.Unit{}, err
	}
	key := receipt.Key{
		TenantID:    scope.TenantID,
		ProjectID:   scope.ProjectID,
		PrincipalID: actor,
		Operation:   operation,
		CallerKey:   callerKey,
	}
	if err := key.Validate(); err != nil {
		return store.Unit{}, err
	}
	fingerprint, err := receipt.Fingerprint(content)
	if err != nil {
		return store.Unit{}, err
	}
	receiptID, err := manager.ids.NewReceiptID()
	if err != nil {
		return store.Unit{}, err
	}
	resultBody, err := json.Marshal(result)
	if err != nil {
		return store.Unit{}, fault.Wrap(fault.InternalFailure, "secret receipt result could not be encoded", err)
	}
	auditID, err := manager.ids.NewEventID()
	if err != nil {
		return store.Unit{}, err
	}
	eventID, err := manager.ids.NewEventID()
	if err != nil {
		return store.Unit{}, err
	}
	audit := event.Audit{
		ID:        auditID,
		Scope:     scope,
		Actor:     actor,
		Operation: operation,
		At:        at,
	}
	envelope := event.Envelope{
		ID:             eventID,
		Type:           event.TypeConfigChanged,
		Source:         event.Source{Kind: event.SourceShell, PrincipalID: actor},
		At:             at,
		Scope:          scope,
		SubjectID:      subject,
		SubjectVersion: subjectVersion,
	}
	if err := envelope.Validate(); err != nil {
		return store.Unit{}, err
	}
	return store.Unit{
		Locks: guards,
		Receipt: &receipt.Record{
			ID:          receiptID,
			Key:         key,
			ContentHash: fingerprint,
			Result:      resultBody,
			At:          at,
		},
		Audit:  []event.Audit{audit},
		Events: []event.Envelope{envelope},
	}, nil
}

// authorityGuard builds the commit-boundary authority locks for a secret
// mutation. A human actor is guarded by its membership union (both present-row
// identity+version pins and absent-row unique-key pins) and a service actor by
// its current service-account row, because the registry grants secret writes to
// human membership roles and to service roles separately.
func (manager *Manager) authorityGuard(ctx context.Context, scope tenancy.Scope, actor id.PrincipalID, kind identity.PrincipalKind, operation string) ([]store.Lock, error) {
	switch kind {
	case identity.KindHuman:
		return manager.membershipGuard(ctx, scope, actor, operation)
	case identity.KindService:
		guard, err := manager.serviceGuard(ctx, scope, actor, operation)
		if err != nil {
			return nil, err
		}
		return []store.Lock{guard}, nil
	default:
		return nil, fault.New(fault.Forbidden, "secret mutation requires a human or service actor")
	}
}

// membershipGuard evaluates the membership union for a human actor and emits
// commit-boundary locks for every applicable target. A project-scoped mutation
// scans both the tenant-wide row and the exact project row so a narrower
// project grant never shadows a tenant-wide grant, and pins absent rows with
// ExpectAbsent so any concurrent grant, revoke or role change refuses the whole
// unit with version_conflict.
func (manager *Manager) membershipGuard(ctx context.Context, scope tenancy.Scope, actor id.PrincipalID, operation string) ([]store.Lock, error) {
	registered, ok := authorize.LookupOperation(authorize.OperationName(operation))
	if !ok {
		return nil, fault.New(fault.InternalFailure, "secret operation is not registered")
	}
	tenantScope := tenancy.Scope{TenantID: scope.TenantID}
	tenantRow, err := manager.readObservedMembership(ctx, tenantScope, actor)
	if err != nil {
		return nil, err
	}
	var projectRow authorize.MembershipRow
	if scope.ProjectScoped() {
		projectRow, err = manager.readObservedMembership(ctx, scope, actor)
		if err != nil {
			return nil, err
		}
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
					secretTableTenantCol:  string(scope.TenantID),
					secretTableProjectCol: projectOrNil,
					membershipPrincipal:   string(actor),
				},
			})
			continue
		}
		locks = append(locks, store.Lock{
			Table:         membershipsTable,
			IDs:           []string{target.RowID},
			ExpectVersion: target.ExpectedVersion,
			TenantWide:    scope.ProjectScoped() && target.Scope.TenantScoped(),
			Expect:        map[string]any{membershipPrincipal: string(actor)},
		})
	}
	return locks, nil
}

// readObservedMembership scans one scope for the acting principal's membership
// row and decodes its observed identity, roles and version. A missing row
// returns the zero MembershipRow so EvaluateMembership pins its absence.
func (manager *Manager) readObservedMembership(ctx context.Context, scope tenancy.Scope, actor id.PrincipalID) (authorize.MembershipRow, error) {
	row, found, err := manager.scanMembership(ctx, scope, actor)
	if err != nil {
		return authorize.MembershipRow{}, err
	}
	if !found {
		return authorize.MembershipRow{}, nil
	}
	principal, ok := row.Values[membershipPrincipal].(string)
	if !ok || principal != string(actor) {
		return authorize.MembershipRow{}, fault.New(fault.Forbidden, "acting principal's current membership does not match the actor")
	}
	roles, err := rowRoles(row.Values[rowRolesColumn])
	if err != nil {
		return authorize.MembershipRow{}, err
	}
	version, ok := rowVersion(row.Values)
	if !ok {
		return authorize.MembershipRow{}, fault.New(fault.InternalFailure, "acting principal's membership has no usable version")
	}
	observedID, err := rowIdentity(row)
	if err != nil {
		return authorize.MembershipRow{}, err
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

// scanMembership reads at most two membership rows for one principal in one
// scope. The declared schema allows exactly one row per (tenant, project,
// principal), so a second row is an internal inconsistency and fails closed
// instead of silently choosing one. A scan error fails closed as well.
func (manager *Manager) scanMembership(ctx context.Context, scope tenancy.Scope, actor id.PrincipalID) (store.Row, bool, error) {
	page, err := manager.scanner.Query(ctx, scope, store.ScanQuery{
		Table:   membershipsTable,
		Equals:  map[string]any{membershipPrincipal: string(actor)},
		OrderBy: []store.Order{{Column: rowIDColumn}},
		Limit:   2,
	})
	if err != nil {
		return store.Row{}, false, err
	}
	if len(page.Rows) > 1 || (len(page.Rows) == 1 && page.HasMore) {
		return store.Row{}, false, fault.New(fault.InternalFailure, "acting principal has more than one membership row in one scope")
	}
	if len(page.Rows) == 0 {
		return store.Row{}, false, nil
	}
	return page.Rows[0], true, nil
}

// serviceGuard locks the service-account row that currently grants a service
// actor. The row must still be enabled and hold the registry's exact service
// role for the operation; the observed version and enabled state are pinned so
// a disable or role change between this read and the commit fails closed.
func (manager *Manager) serviceGuard(ctx context.Context, scope tenancy.Scope, actor id.PrincipalID, operation string) (store.Lock, error) {
	row, tenantWide, err := manager.readAuthorityRow(ctx, scope, servicesTable, actor)
	if err != nil {
		if isNotFound(err) {
			return store.Lock{}, fault.New(fault.Forbidden, "acting service has no current service account granting the secret operation")
		}
		return store.Lock{}, err
	}
	state, ok := row.Values[serviceStateColumn].(string)
	if !ok {
		return store.Lock{}, fault.New(fault.InternalFailure, "acting service row has no usable state")
	}
	if state != serviceEnabledState {
		return store.Lock{}, fault.New(fault.Forbidden, "acting service is not enabled")
	}
	if err := rowAllowsOperation(row.Values, operation, identity.KindService); err != nil {
		return store.Lock{}, err
	}
	version, ok := rowVersion(row.Values)
	if !ok {
		return store.Lock{}, fault.New(fault.InternalFailure, "acting service row has no usable version")
	}
	observedID, err := rowIdentity(row)
	if err != nil {
		return store.Lock{}, err
	}
	// A service principal's identity is its service id, so the observed row
	// must be that same service account rather than another enabled service.
	if observedID != string(actor) {
		return store.Lock{}, fault.New(fault.Forbidden, "acting service row does not match the actor")
	}
	return store.Lock{
		Table:         servicesTable,
		IDs:           []string{observedID},
		TenantWide:    tenantWide,
		ExpectVersion: version,
		Expect:        map[string]any{serviceStateColumn: serviceEnabledState},
	}, nil
}

// readAuthorityRow reads the service-account row that currently grants a
// service actor. A service principal's identity is its service id, so the row
// is read by that id rather than scanned. A project-scoped mutation reads its
// own project row first and reads the tenant-wide row only when the project row
// is absent, so a tenant-wide-only service is resolved without widening the
// project scope. The boolean reports that the observed row is tenant-wide
// (project_id IS NULL).
func (manager *Manager) readAuthorityRow(ctx context.Context, scope tenancy.Scope, table string, actor id.PrincipalID) (store.Row, bool, error) {
	row, err := manager.executor.Read(ctx, scope, table, string(actor))
	if err == nil {
		return row, rowTenantWide(row), nil
	}
	if !isNotFound(err) || !scope.ProjectScoped() {
		return store.Row{}, false, err
	}
	tenant := tenancy.Scope{TenantID: scope.TenantID}
	row, err = manager.executor.Read(ctx, tenant, table, string(actor))
	if err != nil {
		return store.Row{}, false, err
	}
	return row, true, nil
}

// rowTenantWide reports whether the observed row is the NULL-project row. The
// fallback read is always tenant-wide, and a tenant-scoped direct read is too,
// so the lock is pinned with TenantWide in both cases.
func rowTenantWide(row store.Row) bool {
	value, present := row.Values["project_id"]
	if !present || value == nil {
		return present
	}
	text, ok := value.(string)
	return ok && text == ""
}

// rowAllowsOperation re-evaluates the registry's declared role set for the
// actor's exact kind against the row read at the commit boundary. Human and
// service grants are never unioned: a service cannot use a human-only role and
// a human cannot use a service-only grant. Authorization remains the authority
// decision; this read only refuses a role removed before the commit, and the
// version guard then refuses any later change.
func rowAllowsOperation(values map[string]any, operation string, kind identity.PrincipalKind) error {
	registered, ok := authorize.LookupOperation(authorize.OperationName(operation))
	if !ok {
		return fault.New(fault.InternalFailure, "secret operation is not registered")
	}
	roles, err := rowRoles(values[rowRolesColumn])
	if err != nil {
		return err
	}
	var declared []tenancy.Role
	switch kind {
	case identity.KindHuman:
		declared = registered.Requires.HumanRoles
	case identity.KindService:
		// A service row can never grant Admin or an unknown role, so the
		// registry's service roles are matched against the filtered set.
		roles = tenancy.AssignableServiceRoles(roles)
		declared = registered.Requires.ServiceRoles
	default:
		return fault.New(fault.InternalFailure, "secret operation actor kind is not frozen")
	}
	for _, role := range roles {
		if slices.Contains(declared, role) {
			return nil
		}
	}
	return fault.New(fault.Forbidden, "acting principal's current roles do not grant the secret operation")
}

// rowIdentity returns the real identity of the authority row that was
// observed. The lock targets this value, so the guard never guesses that a
// membership or service row id equals the actor id.
func rowIdentity(row store.Row) (string, error) {
	value, ok := row.Values[rowIDColumn].(string)
	if !ok || value == "" {
		return "", fault.New(fault.InternalFailure, "authority row has no usable identity")
	}
	return value, nil
}

// rowVersion normalizes the integer shapes an adapter may return for an
// authority row's optimistic version column.
func rowVersion(values map[string]any) (uint64, bool) {
	return membershipRowVersion(values[rowVersionColumn])
}

// rowRoles decodes the adapter's jsonb roles column into the declared role
// vocabulary. A malformed or unknown role fails closed.
func rowRoles(value any) ([]tenancy.Role, error) {
	switch stored := value.(type) {
	case []byte:
		var names []string
		if err := json.Unmarshal(stored, &names); err != nil {
			return nil, fault.Wrap(fault.InternalFailure, "membership roles could not be decoded", err)
		}
		return parseMembershipRoles(names)
	case []string:
		return parseMembershipRoles(stored)
	case []any:
		names := make([]string, 0, len(stored))
		for _, item := range stored {
			name, ok := item.(string)
			if !ok {
				return nil, fault.New(fault.InternalFailure, "membership role is not a string")
			}
			names = append(names, name)
		}
		return parseMembershipRoles(names)
	default:
		return nil, fault.New(fault.InternalFailure, "membership row has no usable roles")
	}
}

func parseMembershipRoles(names []string) ([]tenancy.Role, error) {
	roles := make([]tenancy.Role, 0, len(names))
	for _, name := range names {
		role := tenancy.Role(name)
		if !role.Valid() {
			return nil, fault.New(fault.InternalFailure, "membership row carries an unknown role")
		}
		roles = append(roles, role)
	}
	return roles, nil
}

// membershipRowVersion normalizes the integer shapes an adapter may return for
// a membership version column.
func membershipRowVersion(value any) (uint64, bool) {
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

// secretRowValues renders the caller-owned durable row columns for one sealed
// value. The adapter supplies id, tenant_id and project_id from the commit
// scope and the row identity, so they are deliberately absent here; a
// tenant-wide row is therefore stored with a SQL NULL project id by the
// adapter, never by an empty-string guess from the caller.
func secretRowValues(
	name id.SecretName,
	createdAt time.Time,
	updatedAt time.Time,
	version uint64,
	envelope Envelope,
	insert bool,
) map[string]any {
	values := map[string]any{
		secretTableNameCol:    string(name),
		secretTableKeyRefCol:  string(envelope.KeyRef),
		secretTableKeyVerCol:  uint32(envelope.KeyVersion),
		secretTableNonceCol:   append([]byte(nil), envelope.Nonce...),
		secretTableWrappedCol: append([]byte(nil), envelope.WrappedKey...),
		secretTableWrapNonce:  append([]byte(nil), envelope.WrapNonce...),
		secretTableCipherCol:  append([]byte(nil), envelope.Ciphertext...),
		secretTableCreatedCol: createdAt,
		secretTableUpdatedCol: updatedAt,
	}
	// Only an insert supplies the initial version. The adapter advances the
	// version column of an update itself, so an update value set that also
	// carried a version would be refused by both adapters.
	if insert {
		values[secretTableVersionCol] = version
	}
	return values
}

// validateStoredIdentity refuses a lookup result that does not exactly name the
// requested scope and name. A cross-scope or cross-name row must never be
// treated as the caller's current value.
func validateStoredIdentity(stored Stored, scope tenancy.Scope, name id.SecretName) error {
	if _, err := ParseSecretID(string(stored.Metadata.ID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "stored secret id is invalid", err)
	}
	if stored.Metadata.Scope != scope {
		return fault.New(fault.InvalidInput, "stored secret is outside the requested scope")
	}
	if stored.Metadata.Name != name {
		return fault.New(fault.InvalidInput, "stored secret name does not match the request")
	}
	return nil
}

func metadataFromRow(row store.Row, scope tenancy.Scope) (Metadata, error) {
	idValue, ok := row.Values[secretTableIDCol].(string)
	if !ok {
		return Metadata{}, fault.New(fault.InvalidInput, "stored secret row has no usable id")
	}
	secretID, err := ParseSecretID(idValue)
	if err != nil {
		return Metadata{}, err
	}
	tenantID, ok := row.Values[secretTableTenantCol].(string)
	if !ok {
		return Metadata{}, fault.New(fault.InvalidInput, "stored secret row has no usable tenant id")
	}
	projectID := ""
	if value, present := row.Values[secretTableProjectCol]; present && value != nil {
		text, ok := value.(string)
		if !ok {
			return Metadata{}, fault.New(fault.InvalidInput, "stored secret row has no usable project id")
		}
		projectID = text
	}
	nameValue, ok := row.Values[secretTableNameCol].(string)
	if !ok {
		return Metadata{}, fault.New(fault.InvalidInput, "stored secret row has no usable name")
	}
	name, err := id.ParseSecretName(nameValue)
	if err != nil {
		return Metadata{}, fault.Wrap(fault.InvalidInput, "stored secret name is invalid", err)
	}
	version, err := rowUint64(row.Values[secretTableVersionCol])
	if err != nil {
		return Metadata{}, err
	}
	updatedAt, ok := row.Values[secretTableUpdatedCol].(time.Time)
	if !ok {
		return Metadata{}, fault.New(fault.InvalidInput, "stored secret row has no usable update time")
	}
	metadata := Metadata{
		ID:        secretID,
		Scope:     tenancy.Scope{TenantID: id.TenantID(tenantID), ProjectID: id.ProjectID(projectID)},
		Name:      name,
		Version:   version,
		UpdatedAt: updatedAt,
	}
	if metadata.Scope != scope {
		return Metadata{}, fault.New(fault.InvalidInput, "stored secret row is outside the requested scope")
	}
	return metadata, nil
}

func rowUint64(value any) (uint64, error) {
	switch number := value.(type) {
	case uint64:
		return number, nil
	case int64:
		if number < 0 {
			return 0, fault.New(fault.InvalidInput, "stored secret version is negative")
		}
		return uint64(number), nil
	case int:
		if number < 0 {
			return 0, fault.New(fault.InvalidInput, "stored secret version is negative")
		}
		return uint64(number), nil
	default:
		return 0, fault.New(fault.InvalidInput, "stored secret row has no usable version")
	}
}
