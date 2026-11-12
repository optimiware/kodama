package app

// The pure bootstrap commit-unit shape. One unit carries the new tenant (with
// its encrypted derivation key), the initial ADMIN-only tenant-wide membership,
// one immutable audit row and one config.changed publication intent. The unit
// has no receipt and no locks: the tenant's unique insert is the one-shot guard.

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

func TestBootstrapAdminUnitShape(t *testing.T) {
	config := BootstrapAdminConfig{
		InstallationID: "installation-bootstrap",
		TenantID:       bootstrapTestTenant,
		TenantName:     "Bootstrap Tenant",
		TokenFile:      "/tmp/unused-token",
		KeyVersion:     secret.MinKeyVersion,
	}
	principal := id.PrincipalID("oidc-bootstrap-principal")
	key := token.TenantKey{
		TenantID:   config.TenantID,
		Version:    1,
		Ciphertext: []byte("ciphertext"),
		WrappedKey: []byte("wrapped-key"),
		WrapNonce:  []byte("wrap-nonce"),
		KeyRef:     "test:key",
		KeyVersion: uint64(secret.MinKeyVersion),
	}
	ids := &bootstrapTestIDs{
		rowID:    "row-bootstrap",
		eventIDs: []id.EventID{"event-audit", "event-config"},
	}
	at := bootstrapTestNow

	unit, result, err := buildBootstrapAdminUnit(config, principal, key, at, ids)
	if err != nil {
		t.Fatalf("buildBootstrapAdminUnit: %v", err)
	}
	if err := unit.Validate(); err != nil {
		t.Fatalf("the bootstrap unit is not a valid commit unit: %v", err)
	}
	if unit.Receipt != nil {
		t.Fatal("the bootstrap unit carries a receipt; the tenant insert is the one-shot guard")
	}
	if len(unit.Locks) != 0 {
		t.Fatalf("the bootstrap unit carries %d locks; want none", len(unit.Locks))
	}
	if len(unit.Writes) != 2 {
		t.Fatalf("the bootstrap unit writes %d rows; want the tenant and the membership", len(unit.Writes))
	}

	var tenantWrite, membershipWrite *store.Write
	for index := range unit.Writes {
		switch unit.Writes[index].Table {
		case "tenants":
			tenantWrite = &unit.Writes[index]
		case "memberships":
			membershipWrite = &unit.Writes[index]
		default:
			t.Fatalf("the bootstrap unit writes unexpected table %q", unit.Writes[index].Table)
		}
	}
	if tenantWrite == nil {
		t.Fatal("the bootstrap unit does not insert the tenant")
	}
	if membershipWrite == nil {
		t.Fatal("the bootstrap unit does not insert the membership")
	}

	if !tenantWrite.Insert || tenantWrite.ID != string(config.TenantID) {
		t.Fatalf("tenant write = %+v; want an insert of %q", tenantWrite, config.TenantID)
	}
	if tenantWrite.Values["name"] != config.TenantName {
		t.Fatalf("tenant name = %v; want %q", tenantWrite.Values["name"], config.TenantName)
	}
	if version := bootstrapUnitVersion(t, tenantWrite.Values["version"]); version != 1 {
		t.Fatalf("tenant version = %d; want 1", version)
	}
	created, ok := tenantWrite.Values["created_at"].(time.Time)
	if !ok || !created.Equal(at) {
		t.Fatalf("tenant created_at = %v; want %s", tenantWrite.Values["created_at"], at)
	}
	rawKey, ok := tenantWrite.Values["derivation_key"].([]byte)
	if !ok {
		t.Fatalf("tenant derivation_key = %T; want stored JSON bytes", tenantWrite.Values["derivation_key"])
	}
	var storedKey token.TenantKey
	if err := json.Unmarshal(rawKey, &storedKey); err != nil {
		t.Fatalf("decode tenant derivation_key: %v", err)
	}
	if !reflect.DeepEqual(storedKey, key) {
		t.Fatalf("stored derivation key = %+v; want the minted key %+v", storedKey, key)
	}

	if !membershipWrite.Insert || membershipWrite.ID != "row-bootstrap" {
		t.Fatalf("membership write = %+v; want an insert with the allocated row id", membershipWrite)
	}
	if membershipWrite.Values["principal_id"] != string(principal) {
		t.Fatalf("membership principal = %v; want %q", membershipWrite.Values["principal_id"], principal)
	}
	rawRoles, ok := membershipWrite.Values["roles"].([]byte)
	if !ok {
		t.Fatalf("membership roles = %T; want stored JSON bytes", membershipWrite.Values["roles"])
	}
	var roles []tenancy.Role
	if err := json.Unmarshal(rawRoles, &roles); err != nil {
		t.Fatalf("decode membership roles: %v", err)
	}
	if !reflect.DeepEqual(roles, []tenancy.Role{tenancy.RoleAdmin}) {
		t.Fatalf("membership roles = %v; want exactly [admin]", roles)
	}
	if version := bootstrapUnitVersion(t, membershipWrite.Values["version"]); version != 1 {
		t.Fatalf("membership version = %d; want 1", version)
	}
	created, ok = membershipWrite.Values["created_at"].(time.Time)
	if !ok || !created.Equal(at) {
		t.Fatalf("membership created_at = %v; want %s", membershipWrite.Values["created_at"], at)
	}

	if len(unit.Audit) != 1 {
		t.Fatalf("the bootstrap unit carries %d audit rows; want exactly 1", len(unit.Audit))
	}
	audit := unit.Audit[0]
	if audit.ID != "event-audit" || audit.Scope != (tenancy.Scope{TenantID: config.TenantID}) ||
		audit.Actor != principal || audit.Operation != BootstrapAdminOperation || !audit.At.Equal(at) {
		t.Fatalf("audit row = %+v; want the bootstrap audit shape", audit)
	}

	if len(unit.Events) != 1 {
		t.Fatalf("the bootstrap unit carries %d event rows; want exactly 1", len(unit.Events))
	}
	envelope := unit.Events[0]
	if envelope.ID != "event-config" || envelope.Type != event.TypeConfigChanged {
		t.Fatalf("event identity/type = %q/%q; want event-config/config.changed", envelope.ID, envelope.Type)
	}
	if envelope.Source != (event.Source{Kind: event.SourceShell, PrincipalID: principal}) {
		t.Fatalf("event source = %+v; want shell/%q", envelope.Source, principal)
	}
	if envelope.Scope != (tenancy.Scope{TenantID: config.TenantID}) ||
		envelope.SubjectID != string(config.TenantID) || envelope.SubjectVersion != 1 {
		t.Fatalf("event scope/subject = %+v/%q/%d; want the new tenant at version 1",
			envelope.Scope, envelope.SubjectID, envelope.SubjectVersion)
	}
	if !envelope.At.Equal(at) {
		t.Fatalf("event time = %s; want %s", envelope.At, at)
	}
	if len(envelope.Data) != 0 {
		t.Fatalf("event data = %q; want no payload", envelope.Data)
	}

	if result.TenantID != config.TenantID || result.PrincipalID != principal ||
		result.MembershipID != "row-bootstrap" || result.AuditID != "event-audit" ||
		result.EventID != "event-config" {
		t.Fatalf("bootstrap result = %+v; want the allocated identities", result)
	}
}

func bootstrapUnitVersion(t *testing.T, value any) uint64 {
	t.Helper()
	switch version := value.(type) {
	case int64:
		return uint64(version)
	case uint64:
		return version
	case int:
		return uint64(version)
	default:
		t.Fatalf("unit version has unexpected type %T (%v)", value, value)
		return 0
	}
}

// bootstrapOrderIDs records ONE shared call sequence across both allocation
// methods. The unit builder must allocate the membership row id first, then the
// audit event id, then the config.changed event id; separate per-method counters
// would not see a reordering between them.
type bootstrapOrderIDs struct {
	mu    sync.Mutex
	calls []string
}

func (ids *bootstrapOrderIDs) NewRowID() (string, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.calls = append(ids.calls, "row")
	return "row-ordered", nil
}

func (ids *bootstrapOrderIDs) NewEventID() (id.EventID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.calls = append(ids.calls, "event")
	return id.EventID(fmt.Sprintf("event-ordered-%d", len(ids.calls))), nil
}

func (ids *bootstrapOrderIDs) sequence() []string {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	return append([]string(nil), ids.calls...)
}

// TestBootstrapAdminAllocatesIdentitiesInPinnedOrder proves one id source sees
// the row id, then the audit event id, then the config.changed event id, both
// in the pure unit builder and through Run.
func TestBootstrapAdminAllocatesIdentitiesInPinnedOrder(t *testing.T) {
	config := BootstrapAdminConfig{
		InstallationID: "installation-bootstrap",
		TenantID:       bootstrapTestTenant,
		TenantName:     "Bootstrap Tenant",
		TokenFile:      "/tmp/unused-token",
		KeyVersion:     secret.MinKeyVersion,
	}
	principal := id.PrincipalID("oidc-bootstrap-principal")
	key := token.TenantKey{
		TenantID:   config.TenantID,
		Version:    1,
		Ciphertext: []byte("ciphertext"),
		WrappedKey: []byte("wrapped-key"),
		WrapNonce:  []byte("wrap-nonce"),
		KeyRef:     "test:key",
		KeyVersion: uint64(secret.MinKeyVersion),
	}
	wantOrder := []string{"row", "event", "event"}

	t.Run("unit builder", func(t *testing.T) {
		ids := &bootstrapOrderIDs{}
		_, result, err := buildBootstrapAdminUnit(config, principal, key, bootstrapTestNow, ids)
		if err != nil {
			t.Fatalf("buildBootstrapAdminUnit: %v", err)
		}
		if got := ids.sequence(); !reflect.DeepEqual(got, wantOrder) {
			t.Fatalf("identity allocation order = %v; want %v", got, wantOrder)
		}
		if result.MembershipID != "row-ordered" || result.AuditID != "event-ordered-2" ||
			result.EventID != "event-ordered-3" {
			t.Fatalf("allocated identities = %q/%q/%q; want row, audit, event in that order",
				result.MembershipID, result.AuditID, result.EventID)
		}
	})

	t.Run("run", func(t *testing.T) {
		clock := newFakeClock()
		memoryStore := memory.NewWithClock(clock.Now)
		cipher := newTestCipher(t)
		verifier := &bootstrapRecordingVerifier{
			principal: bootstrapHumanPrincipal(t, bootstrapTestTenant, bootstrapTestIssuer, bootstrapTestSubject),
		}
		ids := &bootstrapOrderIDs{}
		bootstrap := newBootstrapForTest(t, memoryStore, cipher, verifier, ids, clock,
			bootstrapTestConfig(bootstrapTestTenant, writeBootstrapToken(t, "ordered-token")))

		result, err := bootstrap.Run(context.Background())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if got := ids.sequence(); !reflect.DeepEqual(got, wantOrder) {
			t.Fatalf("identity allocation order = %v; want %v", got, wantOrder)
		}
		if result.MembershipID != "row-ordered" || result.AuditID != "event-ordered-2" ||
			result.EventID != "event-ordered-3" {
			t.Fatalf("allocated identities = %q/%q/%q; want row, audit, event in that order",
				result.MembershipID, result.AuditID, result.EventID)
		}
	})
}
