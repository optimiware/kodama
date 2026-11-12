package secret_test

// Deterministic fakes for the secret management and resolution tests. Every
// fake is in-memory; no test uses a wall clock, a network, a database or a real
// key provider.

import (
	"bytes"
	"context"
	"fmt"
	"sync"
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

var testNow = time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

func projectScope(tenant, project string) tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)}
}

func tenantScope(tenant string) tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID(tenant)}
}

type fakeClock struct {
	now time.Time
}

func (clock *fakeClock) Now() time.Time { return clock.now }

type fakeIDs struct {
	mu       sync.Mutex
	secrets  int
	receipts int
	events   int
}

func (ids *fakeIDs) NewSecretID() (secret.SecretID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.secrets++
	return secret.SecretID(fmt.Sprintf("secret-%d", ids.secrets)), nil
}

func (ids *fakeIDs) NewReceiptID() (id.ReceiptID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.receipts++
	return id.ReceiptID(fmt.Sprintf("receipt-%d", ids.receipts)), nil
}

func (ids *fakeIDs) NewEventID() (id.EventID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.events++
	return id.EventID(fmt.Sprintf("event-%d", ids.events)), nil
}

func testEnvelope(keyVersion secret.KeyVersion) secret.Envelope {
	return secret.Envelope{
		Suite:      secret.SuiteAES256GCM,
		KeyRef:     secret.KeyRef("test-key"),
		KeyVersion: keyVersion,
		Nonce:      bytes.Repeat([]byte{0x01}, secret.ContentNonceBytes),
		Ciphertext: []byte("ciphertext"),
		WrappedKey: bytes.Repeat([]byte{0x02}, secret.WrappedKeyBytes),
		WrapNonce:  bytes.Repeat([]byte{0x03}, secret.WrapNonceBytes),
	}
}

func testStored(scope tenancy.Scope, name id.SecretName, secretID string, version uint64, keyVersion secret.KeyVersion) secret.Stored {
	return secret.Stored{
		Metadata: secret.Metadata{
			ID:        secret.SecretID(secretID),
			Scope:     scope,
			Name:      name,
			Version:   version,
			UpdatedAt: testNow.Add(-time.Hour),
		},
		CreatedAt: testNow.Add(-24 * time.Hour),
		Envelope:  testEnvelope(keyVersion),
	}
}

type callLog struct {
	mu      sync.Mutex
	entries []string
}

func (log *callLog) add(entry string) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.entries = append(log.entries, entry)
}

func (log *callLog) snapshot() []string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return append([]string(nil), log.entries...)
}

type sealCall struct {
	associated secret.AssociatedData
	plaintext  []byte
}

type openCall struct {
	associated secret.AssociatedData
	envelope   secret.Envelope
}

type fakeCipher struct {
	mu      sync.Mutex
	seals   []sealCall
	opens   []openCall
	values  map[id.SecretName]string
	sealErr error
	openErr error
	order   *callLog
}

func (cipher *fakeCipher) Seal(_ context.Context, associated secret.AssociatedData, plaintext []byte) (secret.Envelope, error) {
	cipher.mu.Lock()
	defer cipher.mu.Unlock()
	cipher.seals = append(cipher.seals, sealCall{
		associated: associated,
		plaintext:  append([]byte(nil), plaintext...),
	})
	if cipher.order != nil {
		cipher.order.add("seal:" + string(associated.Name))
	}
	if cipher.sealErr != nil {
		return secret.Envelope{}, cipher.sealErr
	}
	envelope := testEnvelope(associated.KeyVersion)
	envelope.KeyRef = secret.KeyRef("test-key")
	return envelope, nil
}

func (cipher *fakeCipher) Open(_ context.Context, associated secret.AssociatedData, envelope secret.Envelope) ([]byte, error) {
	cipher.mu.Lock()
	defer cipher.mu.Unlock()
	cipher.opens = append(cipher.opens, openCall{
		associated: associated,
		envelope:   envelope,
	})
	if cipher.order != nil {
		cipher.order.add("open:" + string(associated.Name))
	}
	if cipher.openErr != nil {
		return nil, cipher.openErr
	}
	if value, ok := cipher.values[associated.Name]; ok {
		return []byte(value), nil
	}
	return []byte("value-for-" + string(associated.Name)), nil
}

func (cipher *fakeCipher) sealCalls() []sealCall {
	cipher.mu.Lock()
	defer cipher.mu.Unlock()
	return append([]sealCall(nil), cipher.seals...)
}

func (cipher *fakeCipher) openCalls() []openCall {
	cipher.mu.Lock()
	defer cipher.mu.Unlock()
	return append([]openCall(nil), cipher.opens...)
}

type lookupCall struct {
	scope tenancy.Scope
	name  id.SecretName
}

type fakeLookup struct {
	mu       sync.Mutex
	rows     map[string]secret.Stored
	calls    []lookupCall
	err      error
	onLookup func(tenancy.Scope, id.SecretName) (secret.Stored, error)
}

func lookupKey(scope tenancy.Scope, name id.SecretName) string {
	return string(scope.TenantID) + "/" + string(scope.ProjectID) + "/" + string(name)
}

func (lookup *fakeLookup) Lookup(_ context.Context, scope tenancy.Scope, name id.SecretName) (secret.Stored, error) {
	lookup.mu.Lock()
	lookup.calls = append(lookup.calls, lookupCall{scope: scope, name: name})
	onLookup := lookup.onLookup
	err := lookup.err
	row, ok := lookup.rows[lookupKey(scope, name)]
	lookup.mu.Unlock()

	if onLookup != nil {
		return onLookup(scope, name)
	}
	if err != nil {
		return secret.Stored{}, err
	}
	if ok {
		return row, nil
	}
	return secret.Stored{}, fault.New(fault.NotFound, "secret is not stored")
}

func (lookup *fakeLookup) callSnapshot() []lookupCall {
	lookup.mu.Lock()
	defer lookup.mu.Unlock()
	return append([]lookupCall(nil), lookup.calls...)
}

type policyCall struct {
	scope tenancy.Scope
	name  id.SecretName
}

type fakePolicy struct {
	mu      sync.Mutex
	deny    map[id.SecretName]error
	calls   []policyCall
	onCheck func(tenancy.Scope, id.SecretName) error
	order   *callLog
}

func (policy *fakePolicy) Check(_ context.Context, scope tenancy.Scope, name id.SecretName) error {
	policy.mu.Lock()
	policy.calls = append(policy.calls, policyCall{scope: scope, name: name})
	onCheck := policy.onCheck
	denied, isDenied := policy.deny[name]
	policy.mu.Unlock()

	if policy.order != nil {
		policy.order.add("check:" + string(name))
	}
	if onCheck != nil {
		return onCheck(scope, name)
	}
	if isDenied {
		return denied
	}
	return nil
}

func (policy *fakePolicy) callSnapshot() []policyCall {
	policy.mu.Lock()
	defer policy.mu.Unlock()
	return append([]policyCall(nil), policy.calls...)
}

type executeCall struct {
	scope tenancy.Scope
	unit  store.Unit
}

type fakeExecutor struct {
	mu            sync.Mutex
	calls         []executeCall
	err           error
	result        store.UnitResult
	versions      map[string]uint64
	rows          map[string]store.Row
	readErr       error
	membershipErr error
}

func (executor *fakeExecutor) Execute(_ context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.calls = append(executor.calls, executeCall{scope: scope, unit: unit})
	if executor.err != nil {
		return store.UnitResult{}, executor.err
	}
	if executor.versions == nil {
		executor.versions = map[string]uint64{}
	}
	for _, write := range unit.Writes {
		key := write.Table + "/" + write.ID
		current, exists := executor.versions[key]
		if write.Delete {
			if !exists || current != write.ExpectVersion {
				return store.UnitResult{}, fault.New(fault.VersionConflict, "stale delete")
			}
			delete(executor.versions, key)
			continue
		}
		if write.Insert {
			if exists {
				return store.UnitResult{}, fault.New(fault.VersionConflict, "row already exists")
			}
			version, _ := write.Values["version"].(uint64)
			executor.versions[key] = version
			continue
		}
		if !exists || current != write.ExpectVersion {
			return store.UnitResult{}, fault.New(fault.VersionConflict, "stale update")
		}
		version, _ := write.Values["version"].(uint64)
		executor.versions[key] = version
	}
	return executor.result, nil
}

// Query mirrors the neutral scanner's membership read for the basic manager
// tests: one synthetic current membership row per acting principal. The
// identity-shaped row proofs live in manager_membership_scan_test.go.
func (executor *fakeExecutor) Query(_ context.Context, _ tenancy.Scope, query store.ScanQuery) (store.QueryPage, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if executor.readErr != nil {
		return store.QueryPage{}, executor.readErr
	}
	if query.Table != "memberships" {
		return store.QueryPage{}, nil
	}
	if executor.membershipErr != nil {
		return store.QueryPage{}, executor.membershipErr
	}
	principal, _ := query.Equals["principal_id"].(string)
	if principal == "" {
		return store.QueryPage{}, nil
	}
	return store.QueryPage{Rows: []store.Row{{Table: query.Table, Values: map[string]any{
		"id":           "membership-" + principal,
		"principal_id": principal,
		"roles":        []byte(`["engineer"]`),
		"version":      int64(1),
	}}}}, nil
}

func (executor *fakeExecutor) Read(_ context.Context, _ tenancy.Scope, table, id string) (store.Row, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if executor.readErr != nil {
		return store.Row{}, executor.readErr
	}
	if table == "memberships" {
		if executor.membershipErr != nil {
			return store.Row{}, executor.membershipErr
		}
		return store.Row{Table: table, Values: map[string]any{
			"id":           id,
			"principal_id": id,
			"roles":        []byte(`["engineer"]`),
			"version":      int64(1),
		}}, nil
	}
	row, ok := executor.rows[table+"/"+id]
	if !ok {
		return store.Row{}, fault.New(fault.NotFound, "row is not stored")
	}
	return row, nil
}

func (executor *fakeExecutor) callSnapshot() []executeCall {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return append([]executeCall(nil), executor.calls...)
}

func (executor *fakeExecutor) versionOf(table, id string) uint64 {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.versions[table+"/"+id]
}

func testPutCommand(scope tenancy.Scope, name id.SecretName, value string, expected uint64) secret.PutCommand {
	return secret.PutCommand{
		Scope:           scope,
		Name:            name,
		Value:           value,
		ExpectedVersion: expected,
		Actor:           id.PrincipalID("principal-admin"),
		ActorKind:       identity.KindHuman,
		CallerKey:       "caller-put",
		Content: receipt.Content{
			MediaType: "application/json",
			Bytes:     []byte(`{"name":"` + string(name) + `"}`),
		},
	}
}

func testDeleteCommand(scope tenancy.Scope, name id.SecretName, expected uint64) secret.DeleteCommand {
	return secret.DeleteCommand{
		Scope:           scope,
		Name:            name,
		ExpectedVersion: expected,
		Actor:           id.PrincipalID("principal-admin"),
		ActorKind:       identity.KindHuman,
		CallerKey:       "caller-delete",
		Content: receipt.Content{
			MediaType: "application/json",
			Bytes:     []byte(`{"name":"` + string(name) + `"}`),
		},
	}
}

func requireClass(t *testing.T, err error, class fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want fault class %q", class)
	}
	got, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("error %v has no fault class; want %q", err, class)
	}
	if got != class {
		t.Fatalf("error class = %q (%v); want %q", got, err, class)
	}
}

func requireValueString(t *testing.T, values map[string]any, key, want string) {
	t.Helper()
	got, ok := values[key]
	if !ok {
		t.Fatalf("row value %q is absent", key)
	}
	text, ok := got.(string)
	if !ok {
		t.Fatalf("row value %q = %T; want string", key, got)
	}
	if text != want {
		t.Fatalf("row value %q = %q; want %q", key, text, want)
	}
}

func requireValueUint64(t *testing.T, values map[string]any, key string, want uint64) {
	t.Helper()
	got, ok := values[key]
	if !ok {
		t.Fatalf("row value %q is absent", key)
	}
	number, ok := got.(uint64)
	if !ok {
		t.Fatalf("row value %q = %T; want uint64", key, got)
	}
	if number != want {
		t.Fatalf("row value %q = %d; want %d", key, number, want)
	}
}

func requireValueUint32(t *testing.T, values map[string]any, key string, want uint32) {
	t.Helper()
	got, ok := values[key]
	if !ok {
		t.Fatalf("row value %q is absent", key)
	}
	number, ok := got.(uint32)
	if !ok {
		t.Fatalf("row value %q = %T; want uint32", key, got)
	}
	if number != want {
		t.Fatalf("row value %q = %d; want %d", key, number, want)
	}
}

func requireValueBytes(t *testing.T, values map[string]any, key string, size int) {
	t.Helper()
	got, ok := values[key]
	if !ok {
		t.Fatalf("row value %q is absent", key)
	}
	data, ok := got.([]byte)
	if !ok {
		t.Fatalf("row value %q = %T; want []byte", key, got)
	}
	if len(data) != size {
		t.Fatalf("row value %q is %d bytes; want %d", key, len(data), size)
	}
}

func requireValueTime(t *testing.T, values map[string]any, key string, want time.Time) {
	t.Helper()
	got, ok := values[key]
	if !ok {
		t.Fatalf("row value %q is absent", key)
	}
	at, ok := got.(time.Time)
	if !ok {
		t.Fatalf("row value %q = %T; want time.Time", key, got)
	}
	if !at.Equal(want) {
		t.Fatalf("row value %q = %v; want %v", key, at, want)
	}
}

func requireEvent(t *testing.T, events []event.Envelope, scope tenancy.Scope, subject string, version uint64, actor id.PrincipalID, at time.Time) {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("event count = %d; want 1", len(events))
	}
	envelope := events[0]
	if envelope.Type != event.TypeConfigChanged {
		t.Fatalf("event type = %q; want %q", envelope.Type, event.TypeConfigChanged)
	}
	if envelope.Scope != scope {
		t.Fatalf("event scope = %+v; want %+v", envelope.Scope, scope)
	}
	if envelope.SubjectID != subject {
		t.Fatalf("event subject = %q; want %q", envelope.SubjectID, subject)
	}
	if envelope.SubjectVersion != version {
		t.Fatalf("event subject version = %d; want %d", envelope.SubjectVersion, version)
	}
	if envelope.Source.Kind != event.SourceShell {
		t.Fatalf("event source kind = %q; want %q", envelope.Source.Kind, event.SourceShell)
	}
	if envelope.Source.PrincipalID != actor {
		t.Fatalf("event source principal = %q; want %q", envelope.Source.PrincipalID, actor)
	}
	if !envelope.At.Equal(at) {
		t.Fatalf("event time = %v; want %v", envelope.At, at)
	}
}
