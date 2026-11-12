package secret_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func newManager(t *testing.T, clock *fakeClock, ids *fakeIDs, executor *fakeExecutor, cipher *fakeCipher, lookup *fakeLookup) *secret.Manager {
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
	if manager == nil {
		t.Fatal("NewManager returned nil")
	}
	return manager
}

func TestManagerPutInsertsEncryptedCurrentValue(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	clock := &fakeClock{now: testNow}
	ids := &fakeIDs{}
	executor := &fakeExecutor{}
	cipher := &fakeCipher{}
	lookup := &fakeLookup{rows: map[string]secret.Stored{}}
	manager := newManager(t, clock, ids, executor, cipher, lookup)

	command := testPutCommand(scope, id.SecretName("DB_PASSWORD"), "s3cr3t-value", 0)
	metadata, err := manager.Put(context.Background(), command)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if metadata.ID != "secret-1" || metadata.Scope != scope || metadata.Name != "DB_PASSWORD" {
		t.Fatalf("metadata identity = %+v; want secret-1 in project-a", metadata)
	}
	if metadata.Version != 1 {
		t.Fatalf("metadata version = %d; want 1", metadata.Version)
	}
	if !metadata.UpdatedAt.Equal(testNow) {
		t.Fatalf("metadata updated at = %v; want %v", metadata.UpdatedAt, testNow)
	}

	commits := executor.callSnapshot()
	if len(commits) != 1 {
		t.Fatalf("commit count = %d; want 1", len(commits))
	}
	if commits[0].scope != scope {
		t.Fatalf("commit scope = %+v; want %+v", commits[0].scope, scope)
	}
	unit := commits[0].unit
	if len(unit.Writes) != 1 {
		t.Fatalf("write count = %d; want 1", len(unit.Writes))
	}
	write := unit.Writes[0]
	if write.Table != "secrets" || write.ID != "secret-1" {
		t.Fatalf("write target = %q/%q; want secrets/secret-1", write.Table, write.ID)
	}
	if !write.Insert || write.Delete {
		t.Fatalf("write flags = insert:%v delete:%v; want insert only", write.Insert, write.Delete)
	}
	if write.ExpectVersion != 0 {
		t.Fatalf("write expect version = %d; want 0", write.ExpectVersion)
	}
	if _, present := write.Values["tenant_id"]; present {
		t.Fatal("insert values carry tenant_id; the adapter supplies it from the commit scope")
	}
	if _, present := write.Values["project_id"]; present {
		t.Fatal("insert values carry project_id; the adapter supplies it from the commit scope")
	}
	requireValueString(t, write.Values, "name", "DB_PASSWORD")
	requireValueUint64(t, write.Values, "version", 1)
	requireValueTime(t, write.Values, "created_at", testNow)
	requireValueTime(t, write.Values, "updated_at", testNow)
	requireValueString(t, write.Values, "key_ref", "test-key")
	requireValueUint32(t, write.Values, "key_version", 1)
	requireValueBytes(t, write.Values, "nonce", secret.ContentNonceBytes)
	requireValueBytes(t, write.Values, "wrapped_key", secret.WrappedKeyBytes)
	requireValueBytes(t, write.Values, "ciphertext", len("ciphertext"))

	if unit.Receipt == nil {
		t.Fatal("commit unit has no receipt")
	}
	wantFingerprint, err := receipt.Fingerprint(command.Content)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if unit.Receipt.ID != "receipt-1" {
		t.Fatalf("receipt id = %q; want receipt-1", unit.Receipt.ID)
	}
	if unit.Receipt.Key != (receipt.Key{
		TenantID:    id.TenantID("tenant-a"),
		ProjectID:   id.ProjectID("project-a"),
		PrincipalID: id.PrincipalID("principal-admin"),
		Operation:   "secret.write",
		CallerKey:   "caller-put",
	}) {
		t.Fatalf("receipt key = %+v; want the secret.write identity", unit.Receipt.Key)
	}
	if unit.Receipt.ContentHash != wantFingerprint {
		t.Fatalf("receipt content hash = %q; want %q", unit.Receipt.ContentHash, wantFingerprint)
	}
	if !unit.Receipt.At.Equal(testNow) {
		t.Fatalf("receipt time = %v; want %v", unit.Receipt.At, testNow)
	}

	if len(unit.Audit) != 1 {
		t.Fatalf("audit count = %d; want 1", len(unit.Audit))
	}
	audit := unit.Audit[0]
	if audit.Scope != scope || audit.Actor != "principal-admin" || audit.Operation != "secret.write" {
		t.Fatalf("audit = %+v; want scoped secret.write by principal-admin", audit)
	}
	if !audit.At.Equal(testNow) {
		t.Fatalf("audit time = %v; want %v", audit.At, testNow)
	}
	requireEvent(t, unit.Events, scope, "secret-1", 1, "principal-admin", testNow)

	seals := cipher.sealCalls()
	if len(seals) != 1 {
		t.Fatalf("seal count = %d; want 1", len(seals))
	}
	wantAssociated := secret.AssociatedData{
		InstallationID: "install-1",
		TenantID:       id.TenantID("tenant-a"),
		ProjectID:      id.ProjectID("project-a"),
		SecretID:       secret.SecretID("secret-1"),
		Name:           id.SecretName("DB_PASSWORD"),
		KeyVersion:     secret.KeyVersion(1),
	}
	if seals[0].associated != wantAssociated {
		t.Fatalf("associated data = %+v; want %+v", seals[0].associated, wantAssociated)
	}
	if string(seals[0].plaintext) != "s3cr3t-value" {
		t.Fatalf("sealed plaintext = %q; want the command value", seals[0].plaintext)
	}
}

func TestManagerPutReplacesCurrentValueAndBumpsVersion(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	clock := &fakeClock{now: testNow}
	ids := &fakeIDs{}
	executor := &fakeExecutor{versions: map[string]uint64{"secrets/secret-9": 4}}
	cipher := &fakeCipher{}
	stored := testStored(scope, id.SecretName("DB_PASSWORD"), "secret-9", 4, 1)
	lookup := &fakeLookup{rows: map[string]secret.Stored{lookupKey(scope, stored.Metadata.Name): stored}}
	manager := newManager(t, clock, ids, executor, cipher, lookup)

	metadata, err := manager.Put(context.Background(), testPutCommand(scope, "DB_PASSWORD", "new-value", 4))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if metadata.ID != "secret-9" || metadata.Version != 5 {
		t.Fatalf("metadata = %+v; want secret-9 at version 5", metadata)
	}

	commits := executor.callSnapshot()
	if len(commits) != 1 {
		t.Fatalf("commit count = %d; want 1", len(commits))
	}
	write := commits[0].unit.Writes[0]
	if write.Insert || write.Delete {
		t.Fatalf("write flags = insert:%v delete:%v; want update only", write.Insert, write.Delete)
	}
	if write.ID != "secret-9" || write.ExpectVersion != 4 {
		t.Fatalf("write target = %q expect %d; want secret-9 expect 4", write.ID, write.ExpectVersion)
	}
	if _, present := write.Values["version"]; present {
		t.Fatal("update values carry a caller-supplied version; the adapter advances the version column")
	}
	requireValueTime(t, write.Values, "created_at", stored.CreatedAt)
	requireValueTime(t, write.Values, "updated_at", testNow)
	requireEvent(t, commits[0].unit.Events, scope, "secret-9", 5, "principal-admin", testNow)

	seals := cipher.sealCalls()
	if len(seals) != 1 {
		t.Fatalf("seal count = %d; want 1", len(seals))
	}
	if seals[0].associated.SecretID != "secret-9" {
		t.Fatalf("seal associated secret = %q; want secret-9", seals[0].associated.SecretID)
	}
}

func TestManagerPutRefusesStaleVersion(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	clock := &fakeClock{now: testNow}
	ids := &fakeIDs{}
	executor := &fakeExecutor{versions: map[string]uint64{"secrets/secret-9": 4}}
	cipher := &fakeCipher{}
	stored := testStored(scope, id.SecretName("DB_PASSWORD"), "secret-9", 4, 1)
	lookup := &fakeLookup{rows: map[string]secret.Stored{lookupKey(scope, stored.Metadata.Name): stored}}
	manager := newManager(t, clock, ids, executor, cipher, lookup)

	_, err := manager.Put(context.Background(), testPutCommand(scope, "DB_PASSWORD", "new-value", 3))
	requireClass(t, err, fault.VersionConflict)
	if version := executor.versionOf("secrets", "secret-9"); version != 0 && version != 4 {
		t.Fatalf("stale replacement advanced the stored version to %d", version)
	}
}

func TestManagerPutRefusesMissingReplacement(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	manager := newManager(t, &fakeClock{now: testNow}, &fakeIDs{}, &fakeExecutor{}, &fakeCipher{}, &fakeLookup{rows: map[string]secret.Stored{}})

	_, err := manager.Put(context.Background(), testPutCommand(scope, "DB_PASSWORD", "new-value", 1))
	requireClass(t, err, fault.NotFound)
}

func TestManagerDeleteCommitsGuardedDelete(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	clock := &fakeClock{now: testNow}
	ids := &fakeIDs{}
	executor := &fakeExecutor{versions: map[string]uint64{"secrets/secret-9": 2}}
	cipher := &fakeCipher{}
	stored := testStored(scope, id.SecretName("DB_PASSWORD"), "secret-9", 2, 1)
	lookup := &fakeLookup{rows: map[string]secret.Stored{lookupKey(scope, stored.Metadata.Name): stored}}
	manager := newManager(t, clock, ids, executor, cipher, lookup)

	if err := manager.Delete(context.Background(), testDeleteCommand(scope, "DB_PASSWORD", 2)); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	commits := executor.callSnapshot()
	if len(commits) != 1 {
		t.Fatalf("commit count = %d; want 1", len(commits))
	}
	unit := commits[0].unit
	if len(unit.Writes) != 1 {
		t.Fatalf("write count = %d; want 1", len(unit.Writes))
	}
	write := unit.Writes[0]
	if !write.Delete || write.Insert {
		t.Fatalf("write flags = insert:%v delete:%v; want delete only", write.Insert, write.Delete)
	}
	if write.ID != "secret-9" || write.ExpectVersion != 2 {
		t.Fatalf("delete target = %q expect %d; want secret-9 expect 2", write.ID, write.ExpectVersion)
	}
	if len(write.Values) != 0 {
		t.Fatalf("delete values = %v; want none", write.Values)
	}
	if len(cipher.sealCalls()) != 0 {
		t.Fatal("delete sealed a value")
	}
	if unit.Receipt == nil || unit.Receipt.Key.Operation != "secret.delete" {
		t.Fatalf("delete receipt = %+v; want secret.delete", unit.Receipt)
	}
	if len(unit.Audit) != 1 || unit.Audit[0].Operation != "secret.delete" {
		t.Fatalf("delete audit = %+v; want secret.delete", unit.Audit)
	}
	requireEvent(t, unit.Events, scope, "secret-9", 2, "principal-admin", testNow)
}

func TestManagerDeleteRefusesStaleVersion(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	stored := testStored(scope, id.SecretName("DB_PASSWORD"), "secret-9", 2, 1)
	lookup := &fakeLookup{rows: map[string]secret.Stored{lookupKey(scope, stored.Metadata.Name): stored}}
	executor := &fakeExecutor{versions: map[string]uint64{"secrets/secret-9": 2}}
	manager := newManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, lookup)

	err := manager.Delete(context.Background(), testDeleteCommand(scope, "DB_PASSWORD", 1))
	requireClass(t, err, fault.VersionConflict)
	if version := executor.versionOf("secrets", "secret-9"); version != 0 && version != 2 {
		t.Fatalf("stale delete advanced the stored version to %d", version)
	}
}

func TestManagerGetReturnsMetadataOnly(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	stored := testStored(scope, id.SecretName("DB_PASSWORD"), "secret-9", 4, 1)
	executor := &fakeExecutor{rows: map[string]store.Row{
		"secrets/secret-9": {
			Table: "secrets",
			Values: map[string]any{
				"id":         "secret-9",
				"tenant_id":  "tenant-a",
				"project_id": "project-a",
				"name":       "DB_PASSWORD",
				"version":    uint64(4),
				"created_at": stored.CreatedAt,
				"updated_at": stored.Metadata.UpdatedAt,
				"ciphertext": []byte("ciphertext-that-must-not-be-returned"),
			},
		},
	}}
	lookup := &fakeLookup{rows: map[string]secret.Stored{}}
	manager := newManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, lookup)

	metadata, err := manager.Get(context.Background(), scope, "secret-9")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if metadata != stored.Metadata {
		t.Fatalf("metadata = %+v; want the stored metadata", metadata)
	}
	if len(lookup.callSnapshot()) != 0 {
		t.Fatal("a metadata read used the resolution lookup instead of the store executor")
	}
}

func TestManagerGetPropagatesNotFound(t *testing.T) {
	manager := newManager(t, &fakeClock{now: testNow}, &fakeIDs{}, &fakeExecutor{rows: map[string]store.Row{}}, &fakeCipher{}, &fakeLookup{rows: map[string]secret.Stored{}})
	_, err := manager.Get(context.Background(), projectScope("tenant-a", "project-a"), "secret-9")
	requireClass(t, err, fault.NotFound)
}

func TestManagerGetByNameReturnsMetadataOnly(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	stored := testStored(scope, id.SecretName("DB_PASSWORD"), "secret-9", 4, 1)
	cipher := &fakeCipher{}
	lookup := &fakeLookup{rows: map[string]secret.Stored{
		lookupKey(scope, stored.Metadata.Name): stored,
	}}
	manager := newManager(t, &fakeClock{now: testNow}, &fakeIDs{}, &fakeExecutor{}, cipher, lookup)

	metadata, err := manager.GetByName(context.Background(), scope, "DB_PASSWORD")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if metadata != stored.Metadata {
		t.Fatalf("metadata = %+v; want %+v", metadata, stored.Metadata)
	}
	if len(cipher.openCalls()) != 0 {
		t.Fatal("GetByName opened the encrypted envelope")
	}

	_, err = manager.GetByName(context.Background(), scope, "MISSING_SECRET")
	requireClass(t, err, fault.NotFound)

	for _, invalid := range []struct {
		name       string
		queryScope tenancy.Scope
		secretName id.SecretName
	}{
		{name: "invalid scope", queryScope: tenancy.Scope{}, secretName: "DB_PASSWORD"},
		{name: "reserved token name", queryScope: scope, secretName: id.SecretName(id.SecretTokenName)},
		{name: "lowercase name", queryScope: scope, secretName: "db_password"},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			before := len(lookup.callSnapshot())
			_, err := manager.GetByName(context.Background(), invalid.queryScope, invalid.secretName)
			requireClass(t, err, fault.InvalidInput)
			if len(lookup.callSnapshot()) != before {
				t.Fatal("invalid GetByName reached lookup")
			}
		})
	}
}

func TestManagerRejectsInvalidAndOversizedValues(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	clock := &fakeClock{now: testNow}
	executor := &fakeExecutor{}
	cipher := &fakeCipher{}
	manager := newManager(t, clock, &fakeIDs{}, executor, cipher, &fakeLookup{rows: map[string]secret.Stored{}})

	cases := []struct {
		name    string
		command secret.PutCommand
		class   fault.Class
	}{
		{
			name:    "oversized value",
			command: testPutCommand(scope, "DB_PASSWORD", strings.Repeat("x", secret.DefaultBounds.MaxValueBytes+1), 0),
			class:   fault.InvalidInput,
		},
		{
			name:    "reserved credential name",
			command: testPutCommand(scope, id.SecretName(id.SecretTokenName), "value", 0),
			class:   fault.InvalidInput,
		},
		{
			name:    "lowercase name",
			command: testPutCommand(scope, "db_password", "value", 0),
			class:   fault.InvalidInput,
		},
		{
			name:    "invalid scope",
			command: testPutCommand(tenancy.Scope{}, "DB_PASSWORD", "value", 0),
			class:   fault.InvalidInput,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := manager.Put(context.Background(), testCase.command)
			requireClass(t, err, testCase.class)
		})
	}
	if len(cipher.sealCalls()) != 0 {
		t.Fatal("an invalid put reached the cipher")
	}
	if len(executor.callSnapshot()) != 0 {
		t.Fatal("an invalid put reached the executor")
	}
}

func TestManagerAcceptsValueAtExactBound(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	cipher := &fakeCipher{}
	manager := newManager(t, &fakeClock{now: testNow}, &fakeIDs{}, &fakeExecutor{}, cipher, &fakeLookup{rows: map[string]secret.Stored{}})

	value := strings.Repeat("x", secret.DefaultBounds.MaxValueBytes)
	if _, err := manager.Put(context.Background(), testPutCommand(scope, "DB_PASSWORD", value, 0)); err != nil {
		t.Fatalf("Put at the exact bound: %v", err)
	}
	seals := cipher.sealCalls()
	if len(seals) != 1 || len(seals[0].plaintext) != secret.DefaultBounds.MaxValueBytes {
		t.Fatalf("sealed value = %d bytes; want %d", len(seals[0].plaintext), secret.DefaultBounds.MaxValueBytes)
	}
}

func TestManagerTenantWideAndProjectRowsStayDistinct(t *testing.T) {
	tenant := tenantScope("tenant-a")
	project := projectScope("tenant-a", "project-a")
	executor := &fakeExecutor{}
	manager := newManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, &fakeLookup{rows: map[string]secret.Stored{}})

	if _, err := manager.Put(context.Background(), testPutCommand(tenant, "SHARED", "tenant-value", 0)); err != nil {
		t.Fatalf("tenant-wide Put: %v", err)
	}
	if _, err := manager.Put(context.Background(), testPutCommand(project, "SHARED", "project-value", 0)); err != nil {
		t.Fatalf("project Put: %v", err)
	}
	commits := executor.callSnapshot()
	if len(commits) != 2 {
		t.Fatalf("commit count = %d; want 2", len(commits))
	}
	tenantWrite := commits[0].unit.Writes[0]
	projectWrite := commits[1].unit.Writes[0]
	if tenantWrite.ID == projectWrite.ID {
		t.Fatalf("tenant-wide and project rows share id %q; want distinct rows", tenantWrite.ID)
	}
	// The adapter owns the scope columns: a caller-supplied tenant_id or
	// project_id is refused before any effect, and the adapter renders a
	// tenant-wide row with a NULL project id. The committed row shape is
	// proven against the real executor in manager_replay_test.go.
	for _, write := range []store.Write{tenantWrite, projectWrite} {
		for _, reserved := range []string{"id", "tenant_id", "project_id"} {
			if value, ok := write.Values[reserved]; ok {
				t.Fatalf("secret row values carry adapter-supplied %q = %#v", reserved, value)
			}
		}
	}
}

func TestManagerPropagatesCipherAndCommitFailures(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")

	t.Run("cipher failure", func(t *testing.T) {
		cipher := &fakeCipher{sealErr: fault.New(fault.DependencyUnavailable, "key provider unavailable")}
		executor := &fakeExecutor{}
		manager := newManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, cipher, &fakeLookup{rows: map[string]secret.Stored{}})
		_, err := manager.Put(context.Background(), testPutCommand(scope, "DB_PASSWORD", "value", 0))
		requireClass(t, err, fault.DependencyUnavailable)
		if len(executor.callSnapshot()) != 0 {
			t.Fatal("a failed seal committed a store unit")
		}
	})

	t.Run("commit failure", func(t *testing.T) {
		executor := &fakeExecutor{err: fault.New(fault.DependencyUnavailable, "store unavailable")}
		manager := newManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, &fakeLookup{rows: map[string]secret.Stored{}})
		_, err := manager.Put(context.Background(), testPutCommand(scope, "DB_PASSWORD", "value", 0))
		requireClass(t, err, fault.DependencyUnavailable)
	})
}

func TestManagerCommitIntentNeverCarriesPlaintext(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	value := "plaintext-that-must-not-escape"
	executor := &fakeExecutor{}
	manager := newManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, &fakeLookup{rows: map[string]secret.Stored{}})

	if _, err := manager.Put(context.Background(), testPutCommand(scope, "DB_PASSWORD", value, 0)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	unit := executor.callSnapshot()[0].unit
	if unit.Receipt == nil {
		t.Fatal("commit unit has no receipt")
	}
	if bytes.Contains(unit.Receipt.Result, []byte(value)) {
		t.Fatal("receipt result contains the plaintext value")
	}
	for _, audit := range unit.Audit {
		if strings.Contains(audit.Detail, value) {
			t.Fatal("audit detail contains the plaintext value")
		}
	}
	for _, envelope := range unit.Events {
		if bytes.Contains(envelope.Data, []byte(value)) {
			t.Fatal("event data contains the plaintext value")
		}
	}
	encoded, err := json.Marshal(unit)
	if err != nil {
		t.Fatalf("marshal unit: %v", err)
	}
	if bytes.Contains(encoded, []byte(value)) {
		t.Fatal("serialized commit unit contains the plaintext value")
	}
	if !bytes.Contains(encoded, []byte("secrets")) {
		t.Fatal("serialized commit unit does not name the secrets table")
	}
	if !bytes.Contains(encoded, []byte(event.TypeConfigChanged)) {
		t.Fatal("serialized commit unit does not carry the config change event")
	}
}
