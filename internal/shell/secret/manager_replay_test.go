package secret_test

// Receipt-replay regression proofs for the secret manager.
//
// The proofs drive the manager through the neutral store.Executor seam with a
// faithful replay-capable double. A shell capability package may not import a
// concrete store adapter (internal/archtest refuses it), so this double mirrors
// the memory adapter's commit order for the secrets table: it checks the
// receipt first, replays an equal receipt before applying any write, refuses
// changed content for the same caller key, and applies guarded inserts,
// updates and deletes with adapter-owned identity and scope columns. A fake
// that could not replay would hide exactly the defect these tests cover.

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// replayStore is a replay-capable store.Executor and secret.NameLookup for the
// secrets table. It keeps committed rows, receipts, audit rows and event
// publication intents so a retry can replay an equal receipt instead of
// re-applying its write, and it enforces the same optimistic version guard and
// commit-boundary lock/guard semantics the memory and PostgreSQL adapters
// enforce.
type replayStore struct {
	rows          map[string]map[string]any
	receipts      map[receipt.Key]receipt.Record
	audits        []event.Audit
	events        []event.Envelope
	units         []store.Unit
	executions    int
	replays       int
	scanErr       error
	beforeExecute func()
}

func newReplayStore() *replayStore {
	return &replayStore{
		rows:     make(map[string]map[string]any),
		receipts: make(map[receipt.Key]receipt.Record),
	}
}

func (executor *replayStore) Execute(ctx context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	if err := ctx.Err(); err != nil {
		return store.UnitResult{}, err
	}
	if err := scope.Validate(); err != nil {
		return store.UnitResult{}, err
	}
	executor.executions++
	executor.units = append(executor.units, unit)
	if unit.Receipt != nil {
		if existing, found := executor.receipts[unit.Receipt.Key]; found {
			if existing.ContentHash != unit.Receipt.ContentHash {
				return store.UnitResult{}, fault.New(fault.ChangedIdempotencyContent,
					"a receipt with the same caller key already holds different request content")
			}
			executor.replays++
			return store.UnitResult{Replayed: true, Receipt: cloneReceipt(existing)}, nil
		}
	}
	if executor.beforeExecute != nil {
		executor.beforeExecute()
	}

	staged := &replayStore{
		rows:     cloneRows(executor.rows),
		receipts: cloneReceipts(executor.receipts),
		audits:   append([]event.Audit(nil), executor.audits...),
		events:   append([]event.Envelope(nil), executor.events...),
	}
	if err := staged.applyLocks(scope, unit.Locks); err != nil {
		return store.UnitResult{}, err
	}
	for _, write := range unit.Writes {
		if err := staged.apply(scope, write); err != nil {
			return store.UnitResult{}, err
		}
	}
	if unit.Receipt != nil {
		staged.receipts[unit.Receipt.Key] = cloneReceipt(*unit.Receipt)
	}
	staged.audits = append(staged.audits, unit.Audit...)
	staged.events = append(staged.events, unit.Events...)
	executor.rows = staged.rows
	executor.receipts = staged.receipts
	executor.audits = staged.audits
	executor.events = staged.events
	result := store.UnitResult{}
	if unit.Receipt != nil {
		result.Receipt = cloneReceipt(*unit.Receipt)
	}
	return result, nil
}

// applyLocks mirrors the adapter's lock phase: every present lock row must be
// visible in the unit scope at the expected version and column values, and
// every ExpectAbsent lock must match no existing row for its unique key. A
// missing, changed or unexpectedly present row refuses the whole unit.
func (executor *replayStore) applyLocks(scope tenancy.Scope, locks []store.Lock) error {
	for _, lock := range locks {
		if lock.ExpectAbsent {
			if len(lock.IDs) > 0 || lock.ExpectVersion != 0 || len(lock.Expect) > 0 || lock.TenantWide || lock.Deadline != nil || len(lock.Key) == 0 {
				return fault.New(fault.InvalidInput, "malformed absence lock")
			}
			prefix := lock.Table + "/"
			for key, values := range executor.rows {
				if !strings.HasPrefix(key, prefix) {
					continue
				}
				if replayAbsenceMatches(values, lock.Key) {
					return fault.New(fault.VersionConflict, "an expected-absent row exists")
				}
			}
			continue
		}
		for _, identity := range lock.IDs {
			values, ok := executor.rows[lock.Table+"/"+identity]
			if !ok || !replayLockVisible(scope, lock, values) {
				return fault.New(fault.VersionConflict, "a locked row is missing or out of scope")
			}
			if lock.ExpectVersion != 0 {
				current, ok := values["version"].(int64)
				if !ok || uint64(current) != lock.ExpectVersion {
					return fault.New(fault.VersionConflict, "a guarded row was not at the expected version")
				}
			}
			for name, expected := range lock.Expect {
				if !replayGuardEqual(values[name], expected) {
					return fault.New(fault.VersionConflict, "a guarded row no longer matches its expected column values")
				}
			}
		}
	}
	return nil
}

func replayAbsenceMatches(values map[string]any, key map[string]any) bool {
	for column, expected := range key {
		stored, present := values[column]
		if expected == nil {
			if !present || stored == nil {
				continue
			}
			if text, ok := stored.(string); ok && text == "" {
				continue
			}
			return false
		}
		if !replayGuardEqual(stored, expected) {
			return false
		}
	}
	return true
}

// replayLockVisible applies the adapter's lock-scope rule: a tenant-wide lock
// targets the NULL-project row from either scope, while an ordinary lock is
// visible only inside the unit's own scope.
func replayLockVisible(scope tenancy.Scope, lock store.Lock, values map[string]any) bool {
	if lock.TenantWide || scope.TenantScoped() {
		tenant, _ := values["tenant_id"].(string)
		project, _ := values["project_id"].(string)
		return tenant == string(scope.TenantID) && project == ""
	}
	return replayVisible(scope, values)
}

// replayGuardEqual compares one stored guard column with its expected value.
// The adapter normalizes integer columns to int64, so the comparison accepts
// the integer shapes a caller may use for the same value.
func replayGuardEqual(stored, expected any) bool {
	switch want := expected.(type) {
	case uint64:
		current, ok := stored.(int64)
		return ok && uint64(current) == want
	case int64:
		current, ok := stored.(int64)
		return ok && current == want
	default:
		return stored == expected
	}
}

// seedMembership installs one current membership row for the acting principal
// with the principal id as the row id. The identity adapter allocates a
// membership-<random> row id instead; seedIdentityMembership models that real
// shape, while this helper keeps the fixed-id proofs readable.
func (executor *replayStore) seedMembership(scope tenancy.Scope, actor id.PrincipalID, version uint64) {
	var project any
	if scope.ProjectScoped() {
		project = string(scope.ProjectID)
	}
	executor.rows["memberships/"+string(actor)] = map[string]any{
		"id":           string(actor),
		"tenant_id":    string(scope.TenantID),
		"project_id":   project,
		"principal_id": string(actor),
		"roles":        []byte(`["engineer"]`),
		"version":      int64(version),
		"created_at":   testNow,
	}
}

// seedIdentityMembership installs one membership row with the exact shape the
// identity adapter commits: a global membership-<hex> primary key that is not
// the principal id, a jsonb roles value, and a NULL project id for the
// tenant-wide grant.
func (executor *replayStore) seedIdentityMembership(scope tenancy.Scope, rowID string, principal id.PrincipalID, version uint64, roles ...string) {
	var project any
	if scope.ProjectScoped() {
		project = string(scope.ProjectID)
	}
	encoded := []byte(`[]`)
	if len(roles) > 0 {
		encoded = []byte(`["` + strings.Join(roles, `","`) + `"]`)
	}
	executor.rows["memberships/"+rowID] = map[string]any{
		"id":           rowID,
		"tenant_id":    string(scope.TenantID),
		"project_id":   project,
		"principal_id": string(principal),
		"roles":        encoded,
		"version":      int64(version),
		"created_at":   testNow,
	}
}

// seedTenantMembership installs one tenant-wide (NULL project) membership row
// for the acting principal. A project-scoped read cannot see it; the manager
// must fall back to a tenant-scoped read and pin the lock with TenantWide.
func (executor *replayStore) seedTenantMembership(scope tenancy.Scope, actor id.PrincipalID, version uint64, roles string) {
	executor.rows["memberships/"+string(actor)] = map[string]any{
		"id":           string(actor),
		"tenant_id":    string(scope.TenantID),
		"project_id":   nil,
		"principal_id": string(actor),
		"roles":        []byte(roles),
		"version":      int64(version),
		"created_at":   testNow,
	}
}

// seedService installs one service-account row in the given scope. A service
// actor's principal id is its service id, so the row identity is the actor id.
func (executor *replayStore) seedService(scope tenancy.Scope, serviceID id.PrincipalID, version uint64, state, roles string) {
	var project any
	if scope.ProjectScoped() {
		project = string(scope.ProjectID)
	}
	executor.rows["services/"+string(serviceID)] = map[string]any{
		"id":         string(serviceID),
		"tenant_id":  string(scope.TenantID),
		"project_id": project,
		"name":       "service-" + string(serviceID),
		"state":      state,
		"roles":      []byte(roles),
		"version":    int64(version),
		"created_at": testNow,
	}
}

func (executor *replayStore) revokeMembership(actor id.PrincipalID) {
	delete(executor.rows, "memberships/"+string(actor))
}

// revokeMembershipRow deletes the observed membership row by its real row
// identity, mirroring an adapter revocation of an identity-allocated row.
func (executor *replayStore) revokeMembershipRow(rowID string) {
	delete(executor.rows, "memberships/"+rowID)
}

func (executor *replayStore) setMembershipVersion(actor id.PrincipalID, version uint64) {
	values, ok := executor.rows["memberships/"+string(actor)]
	if !ok {
		return
	}
	values["version"] = int64(version)
}

// setMembershipVersionRow advances the observed membership row's version by its
// real row identity.
func (executor *replayStore) setMembershipVersionRow(rowID string, version uint64) {
	values, ok := executor.rows["memberships/"+rowID]
	if !ok {
		return
	}
	values["version"] = int64(version)
}

// setServiceAuthority replaces a service row's enablement and roles and
// advances its version, mirroring service.Update's versioned value change.
func (executor *replayStore) setServiceAuthority(serviceID id.PrincipalID, state, roles string, version uint64) {
	values, ok := executor.rows["services/"+string(serviceID)]
	if !ok {
		return
	}
	values["state"] = state
	values["roles"] = []byte(roles)
	values["version"] = int64(version)
}

func (executor *replayStore) lastUnit(t *testing.T) store.Unit {
	t.Helper()
	if len(executor.units) == 0 {
		t.Fatal("no commit unit was executed")
	}
	return executor.units[len(executor.units)-1]
}

func (executor *replayStore) receiptCount() int { return len(executor.receipts) }
func (executor *replayStore) auditCount() int   { return len(executor.audits) }
func (executor *replayStore) eventCount() int   { return len(executor.events) }

func (executor *replayStore) hasRow(table, identity string) bool {
	_, ok := executor.rows[table+"/"+identity]
	return ok
}

// apply mirrors the adapter's write shapes: an insert receives its identity and
// scope columns from the adapter, an update advances the version column itself,
// and an update or delete whose row is absent or not at ExpectVersion is a
// version conflict.
func (executor *replayStore) apply(scope tenancy.Scope, write store.Write) error {
	key := write.Table + "/" + write.ID
	existing, exists := executor.rows[key]
	switch {
	case write.Insert:
		if exists {
			return fault.New(fault.VersionConflict, "an inserted row already exists")
		}
		values := normalizeValues(write.Values)
		values["id"] = write.ID
		values["tenant_id"] = string(scope.TenantID)
		if scope.TenantScoped() {
			values["project_id"] = nil
		} else {
			values["project_id"] = string(scope.ProjectID)
		}
		executor.rows[key] = values
		return nil
	case write.Delete:
		if !exists {
			return fault.New(fault.VersionConflict, "a deleted row is missing or out of scope")
		}
		if err := checkVersionGuard(existing, write.ExpectVersion); err != nil {
			return err
		}
		delete(executor.rows, key)
		return nil
	default:
		if !exists {
			return fault.New(fault.VersionConflict, "an updated row is missing or out of scope")
		}
		if err := checkVersionGuard(existing, write.ExpectVersion); err != nil {
			return err
		}
		for name, value := range write.Values {
			existing[name] = normalizeValue(value)
		}
		current, ok := existing["version"].(int64)
		if !ok {
			return fault.New(fault.InternalFailure, "the stored row has no usable version")
		}
		existing["version"] = current + 1
		return nil
	}
}

func checkVersionGuard(values map[string]any, expect uint64) error {
	if expect == 0 {
		return nil
	}
	current, ok := values["version"].(int64)
	if !ok || uint64(current) != expect {
		return fault.New(fault.VersionConflict, "a written row was not at the expected version")
	}
	return nil
}

func (executor *replayStore) Read(_ context.Context, scope tenancy.Scope, table, identity string) (store.Row, error) {
	values, ok := executor.rows[table+"/"+identity]
	if !ok || !replayVisible(scope, values) {
		return store.Row{}, fault.New(fault.NotFound, "row is not stored")
	}
	return store.Row{Table: table, Values: cloneValues(values)}, nil
}

// Query mirrors the neutral scanner's contract for this double: it applies the
// transaction-local scope, the caller's equality predicates and the bounded
// deterministic id order. A project scope sees only the exact project rows and a
// tenant scope sees only the tenant-wide (NULL project) rows, exactly like the
// concrete adapters. It is the seam the commit-boundary membership guard scans.
func (executor *replayStore) Query(_ context.Context, scope tenancy.Scope, query store.ScanQuery) (store.QueryPage, error) {
	if err := scope.Validate(); err != nil {
		return store.QueryPage{}, err
	}
	if err := query.Validate(); err != nil {
		return store.QueryPage{}, err
	}
	if executor.scanErr != nil {
		return store.QueryPage{}, executor.scanErr
	}
	prefix := query.Table + "/"
	identities := make([]string, 0, len(executor.rows))
	for key, values := range executor.rows {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if !replayVisible(scope, values) {
			continue
		}
		if !replayMatches(values, query.Equals) {
			continue
		}
		identities = append(identities, strings.TrimPrefix(key, prefix))
	}
	sort.Strings(identities)
	page := store.QueryPage{}
	if len(identities) > query.Limit {
		page.HasMore = true
		identities = identities[:query.Limit]
	}
	page.Rows = make([]store.Row, 0, len(identities))
	for _, identity := range identities {
		page.Rows = append(page.Rows, store.Row{
			Table:  query.Table,
			Values: cloneValues(executor.rows[prefix+identity]),
		})
	}
	if page.HasMore {
		page.Next = "cursor:" + identities[len(identities)-1]
	}
	return page, nil
}

// replayMatches applies one caller equality set to a stored row. Equality can
// only narrow the scope predicate, never widen it.
func replayMatches(values map[string]any, equals map[string]any) bool {
	for column, expected := range equals {
		if !replayGuardEqual(values[column], expected) {
			return false
		}
	}
	return true
}

// Lookup adapts committed rows to the management NameLookup seam, so a version
// always comes from committed state rather than from a test map.
func (executor *replayStore) Lookup(_ context.Context, scope tenancy.Scope, name id.SecretName) (secret.Stored, error) {
	for _, values := range executor.rows {
		if !replayVisible(scope, values) {
			continue
		}
		if stored, ok := values["name"].(string); !ok || stored != string(name) {
			continue
		}
		return storedFromValues(values)
	}
	return secret.Stored{}, fault.New(fault.NotFound, "secret is not stored")
}

func (executor *replayStore) snapshot(scope tenancy.Scope, identity secret.SecretID) (map[string]any, bool) {
	values, ok := executor.rows["secrets/"+string(identity)]
	if !ok || !replayVisible(scope, values) {
		return nil, false
	}
	return cloneValues(values), true
}

func (executor *replayStore) version(t *testing.T, scope tenancy.Scope, identity secret.SecretID) uint64 {
	t.Helper()
	values, ok := executor.snapshot(scope, identity)
	if !ok {
		t.Fatalf("secret %q is not stored", identity)
	}
	version, ok := values["version"].(int64)
	if !ok {
		t.Fatalf("stored secret version = %T; want int64", values["version"])
	}
	return uint64(version)
}

// replayVisible applies the adapter's scope predicates to one stored row.
func replayVisible(scope tenancy.Scope, values map[string]any) bool {
	tenant, _ := values["tenant_id"].(string)
	if tenant != string(scope.TenantID) {
		return false
	}
	project, _ := values["project_id"].(string)
	if scope.TenantScoped() {
		return project == ""
	}
	return project == string(scope.ProjectID)
}

func storedFromValues(values map[string]any) (secret.Stored, error) {
	identity, ok := values["id"].(string)
	if !ok {
		return secret.Stored{}, fault.New(fault.InternalFailure, "stored secret row has no usable id")
	}
	tenant, ok := values["tenant_id"].(string)
	if !ok {
		return secret.Stored{}, fault.New(fault.InternalFailure, "stored secret row has no usable tenant id")
	}
	project := ""
	if value, present := values["project_id"]; present && value != nil {
		text, ok := value.(string)
		if !ok {
			return secret.Stored{}, fault.New(fault.InternalFailure, "stored secret row has no usable project id")
		}
		project = text
	}
	nameText, ok := values["name"].(string)
	if !ok {
		return secret.Stored{}, fault.New(fault.InternalFailure, "stored secret row has no usable name")
	}
	version, ok := values["version"].(int64)
	if !ok {
		return secret.Stored{}, fault.New(fault.InternalFailure, "stored secret row has no usable version")
	}
	createdAt, ok := values["created_at"].(time.Time)
	if !ok {
		return secret.Stored{}, fault.New(fault.InternalFailure, "stored secret row has no usable creation time")
	}
	updatedAt, ok := values["updated_at"].(time.Time)
	if !ok {
		return secret.Stored{}, fault.New(fault.InternalFailure, "stored secret row has no usable update time")
	}
	keyRef, ok := values["key_ref"].(string)
	if !ok {
		return secret.Stored{}, fault.New(fault.InternalFailure, "stored secret row has no usable key reference")
	}
	keyVersion, ok := values["key_version"].(int64)
	if !ok {
		return secret.Stored{}, fault.New(fault.InternalFailure, "stored secret row has no usable key version")
	}
	nonce, _ := values["nonce"].([]byte)
	ciphertext, _ := values["ciphertext"].([]byte)
	wrappedKey, _ := values["wrapped_key"].([]byte)
	wrapNonce, _ := values["wrap_nonce"].([]byte)
	return secret.Stored{
		Metadata: secret.Metadata{
			ID:        secret.SecretID(identity),
			Scope:     tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)},
			Name:      id.SecretName(nameText),
			Version:   uint64(version),
			UpdatedAt: updatedAt,
		},
		CreatedAt: createdAt,
		Envelope: secret.Envelope{
			Suite:      secret.SuiteAES256GCM,
			KeyRef:     secret.KeyRef(keyRef),
			KeyVersion: secret.KeyVersion(uint32(keyVersion)),
			Nonce:      append([]byte(nil), nonce...),
			Ciphertext: append([]byte(nil), ciphertext...),
			WrappedKey: append([]byte(nil), wrappedKey...),
			WrapNonce:  append([]byte(nil), wrapNonce...),
		},
	}, nil
}

// normalizeValues decodes integer columns to the int64 shape store.Row
// promises, exactly as a concrete adapter does before storing them.
func normalizeValues(values map[string]any) map[string]any {
	normalized := make(map[string]any, len(values))
	for name, value := range values {
		normalized[name] = normalizeValue(value)
	}
	return normalized
}

func normalizeValue(value any) any {
	switch number := value.(type) {
	case uint64:
		return int64(number)
	case uint32:
		return int64(number)
	case int:
		return int64(number)
	case []byte:
		return append([]byte(nil), number...)
	default:
		return value
	}
}

func cloneRows(rows map[string]map[string]any) map[string]map[string]any {
	cloned := make(map[string]map[string]any, len(rows))
	for key, values := range rows {
		cloned[key] = cloneValues(values)
	}
	return cloned
}

func cloneReceipts(receipts map[receipt.Key]receipt.Record) map[receipt.Key]receipt.Record {
	cloned := make(map[receipt.Key]receipt.Record, len(receipts))
	for key, record := range receipts {
		cloned[key] = cloneReceipt(record)
	}
	return cloned
}

func cloneReceipt(record receipt.Record) receipt.Record {
	cloned := record
	cloned.Result = append([]byte(nil), record.Result...)
	return cloned
}

func cloneValues(values map[string]any) map[string]any {
	cloned := make(map[string]any, len(values))
	for name, value := range values {
		cloned[name] = cloneValue(value)
	}
	return cloned
}

func cloneValue(value any) any {
	if bytes, ok := value.([]byte); ok {
		return append([]byte(nil), bytes...)
	}
	return value
}

// replayManagerPorts is the pair of neutral seams the manager consumes; the
// replay double implements both.
type replayManagerPorts interface {
	store.Executor
	store.Scanner
}

func newReplayManager(t *testing.T, clock *fakeClock, ids *fakeIDs, executor replayManagerPorts, cipher *fakeCipher, lookup secret.NameLookup) *secret.Manager {
	t.Helper()
	manager, err := secret.NewManager(secret.ManagerConfig{
		InstallationID: "install-1",
		KeyVersion:     secret.KeyVersion(1),
		Clock:          clock,
		IDs:            ids,
		Executor:       executor,
		Scanner:        executor,
		Cipher:         cipher,
		Lookup:         lookup,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return manager
}

func replayPutCommand(scope tenancy.Scope, name id.SecretName, value string, expected uint64, callerKey string) secret.PutCommand {
	return secret.PutCommand{
		Scope:           scope,
		Name:            name,
		Value:           value,
		ExpectedVersion: expected,
		Actor:           id.PrincipalID("principal-admin"),
		ActorKind:       identity.KindHuman,
		CallerKey:       callerKey,
		Content: receipt.Content{
			MediaType: "application/json",
			Bytes:     []byte(`{"name":"` + string(name) + `","request":"` + callerKey + `"}`),
		},
	}
}

func replayDeleteCommand(scope tenancy.Scope, name id.SecretName, expected uint64, callerKey string) secret.DeleteCommand {
	return secret.DeleteCommand{
		Scope:           scope,
		Name:            name,
		ExpectedVersion: expected,
		Actor:           id.PrincipalID("principal-admin"),
		ActorKind:       identity.KindHuman,
		CallerKey:       callerKey,
		Content: receipt.Content{
			MediaType: "application/json",
			Bytes:     []byte(`{"name":"` + string(name) + `","request":"` + callerKey + `"}`),
		},
	}
}

func requireMetadataEqual(t *testing.T, got, want secret.Metadata) {
	t.Helper()
	if got.ID != want.ID || got.Scope != want.Scope || got.Name != want.Name || got.Version != want.Version || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Fatalf("metadata = %+v; want %+v", got, want)
	}
}

func TestManagerPutInsertRetryReplaysCommittedMetadata(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.seedMembership(scope, "principal-admin", 1)
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)
	command := replayPutCommand(scope, "DB_PASSWORD", "s3cr3t-value", 0, "insert-1")

	first, err := manager.Put(context.Background(), command)
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	retry, err := manager.Put(context.Background(), command)
	if err != nil {
		t.Fatalf("retried Put: %v", err)
	}
	requireMetadataEqual(t, retry, first)
	if executor.replays != 1 {
		t.Fatalf("receipt replays = %d; want 1", executor.replays)
	}
	if version := executor.version(t, scope, first.ID); version != 1 {
		t.Fatalf("stored version = %d; want 1", version)
	}
	if _, ok := executor.snapshot(scope, "secret-2"); ok {
		t.Fatal("retried insert committed a second secret row")
	}
}

func TestManagerPutReplacementRetryReplaysWithoutConflict(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.seedMembership(scope, "principal-admin", 1)
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	inserted, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "first", 0, "insert-1"))
	if err != nil {
		t.Fatalf("insert Put: %v", err)
	}
	replacement := replayPutCommand(scope, "DB_PASSWORD", "second", 1, "replace-1")
	replaced, err := manager.Put(context.Background(), replacement)
	if err != nil {
		t.Fatalf("replacement Put: %v", err)
	}
	if replaced.ID != inserted.ID || replaced.Version != 2 {
		t.Fatalf("replacement metadata = %+v; want %q at version 2", replaced, inserted.ID)
	}

	retry, err := manager.Put(context.Background(), replacement)
	if err != nil {
		t.Fatalf("retried replacement Put: %v", err)
	}
	requireMetadataEqual(t, retry, replaced)
	if executor.replays != 1 {
		t.Fatalf("receipt replays = %d; want 1", executor.replays)
	}
	if version := executor.version(t, scope, inserted.ID); version != 2 {
		t.Fatalf("stored version = %d; want 2", version)
	}
}

func TestManagerPutReplayRefusesChangedContent(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.seedMembership(scope, "principal-admin", 1)
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	command := replayPutCommand(scope, "DB_PASSWORD", "first", 0, "insert-1")
	inserted, err := manager.Put(context.Background(), command)
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	changed := replayPutCommand(scope, "DB_PASSWORD", "first", 0, "insert-1")
	changed.Content = receipt.Content{MediaType: "application/json", Bytes: []byte(`{"name":"DB_PASSWORD","request":"other"}`)}
	if _, err := manager.Put(context.Background(), changed); err == nil {
		t.Fatal("a changed-content retry was accepted")
	} else {
		requireClass(t, err, fault.ChangedIdempotencyContent)
	}
	if _, ok := executor.snapshot(scope, "secret-2"); ok {
		t.Fatal("a changed-content retry committed a second secret row")
	}
	if version := executor.version(t, scope, inserted.ID); version != 1 {
		t.Fatalf("stored version = %d; want 1", version)
	}
}

func TestManagerPutStaleVersionWithoutReceiptConflicts(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.seedMembership(scope, "principal-admin", 1)
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	inserted, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "first", 0, "insert-1"))
	if err != nil {
		t.Fatalf("insert Put: %v", err)
	}
	if _, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "second", 1, "replace-1")); err != nil {
		t.Fatalf("replacement Put: %v", err)
	}

	if _, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "third", 1, "replace-stale")); err == nil {
		t.Fatal("a stale replacement without a receipt was accepted")
	} else {
		requireClass(t, err, fault.VersionConflict)
	}
	if version := executor.version(t, scope, inserted.ID); version != 2 {
		t.Fatalf("stored version = %d; want the newer version 2", version)
	}
}

func TestManagerDeleteRetryReplaysWithoutNotFound(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.seedMembership(scope, "principal-admin", 1)
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	inserted, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "first", 0, "insert-1"))
	if err != nil {
		t.Fatalf("insert Put: %v", err)
	}
	command := replayDeleteCommand(scope, "DB_PASSWORD", 1, "delete-1")
	if err := manager.Delete(context.Background(), command); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := executor.snapshot(scope, inserted.ID); ok {
		t.Fatal("Delete left the secret row committed")
	}
	if err := manager.Delete(context.Background(), command); err != nil {
		t.Fatalf("retried Delete: %v", err)
	}
	if executor.replays != 1 {
		t.Fatalf("receipt replays = %d; want 1", executor.replays)
	}
	if _, ok := executor.snapshot(scope, inserted.ID); ok {
		t.Fatal("retried Delete recreated or kept the secret row")
	}
}

func TestManagerDeleteStaleVersionWithoutReceiptConflicts(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.seedMembership(scope, "principal-admin", 1)
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	inserted, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "first", 0, "insert-1"))
	if err != nil {
		t.Fatalf("insert Put: %v", err)
	}
	if _, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "second", 1, "replace-1")); err != nil {
		t.Fatalf("replacement Put: %v", err)
	}

	if err := manager.Delete(context.Background(), replayDeleteCommand(scope, "DB_PASSWORD", 1, "delete-stale")); err == nil {
		t.Fatal("a stale delete without a receipt was accepted")
	} else {
		requireClass(t, err, fault.VersionConflict)
	}
	if version := executor.version(t, scope, inserted.ID); version != 2 {
		t.Fatalf("stored version = %d; want the newer version 2", version)
	}
}

func TestManagerDeleteMissingRowWithoutReceiptConflicts(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.seedMembership(scope, "principal-admin", 1)
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	err := manager.Delete(context.Background(), replayDeleteCommand(scope, "DB_PASSWORD", 1, "delete-1"))
	requireClass(t, err, fault.VersionConflict)
}

// TestManagerStoresTenantWideAndProjectRowsDistinctly proves that the manager
// never supplies adapter-owned scope columns itself: the adapter renders the
// tenant-wide row with a SQL NULL project id and the project row with its
// project id, so two same-named secrets stay distinct rows that cannot
// overwrite each other.
func TestManagerStoresTenantWideAndProjectRowsDistinctly(t *testing.T) {
	tenant := tenantScope("tenant-a")
	project := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.seedMembership(tenant, "principal-admin", 1)
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	tenantValue, err := manager.Put(context.Background(), replayPutCommand(tenant, "SHARED", "tenant-value", 0, "tenant-insert"))
	if err != nil {
		t.Fatalf("tenant-wide Put: %v", err)
	}
	// The scoped membership identity is the principal, so the double holds one
	// visible membership row at a time. Re-seed it for the project scope before
	// the project-scoped write.
	executor.seedMembership(project, "principal-admin", 1)
	projectValue, err := manager.Put(context.Background(), replayPutCommand(project, "SHARED", "project-value", 0, "project-insert"))
	if err != nil {
		t.Fatalf("project Put: %v", err)
	}
	if tenantValue.ID == projectValue.ID {
		t.Fatalf("tenant-wide and project rows share id %q; want distinct rows", tenantValue.ID)
	}

	tenantRow, ok := executor.snapshot(tenant, tenantValue.ID)
	if !ok {
		t.Fatalf("tenant-wide row %q is not stored", tenantValue.ID)
	}
	if value, present := tenantRow["project_id"]; !present || value != nil {
		t.Fatalf("tenant-wide stored project_id = %#v (present %v); want SQL NULL", value, present)
	}
	projectRow, ok := executor.snapshot(project, projectValue.ID)
	if !ok {
		t.Fatalf("project row %q is not stored", projectValue.ID)
	}
	if value := projectRow["project_id"]; value != "project-a" {
		t.Fatalf("project stored project_id = %#v; want project-a", value)
	}
	if _, ok := executor.snapshot(project, tenantValue.ID); ok {
		t.Fatal("a project-scoped read saw the tenant-wide row")
	}
}
