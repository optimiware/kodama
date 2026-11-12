package readers_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	identitystore "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// credentialIDs allocates the admin's durable audit, receipt and event
// identities.
type credentialIDs struct {
	mu     sync.Mutex
	events int
	recpt  int
}

func (ids *credentialIDs) NewEventID() (id.EventID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.events++
	return id.EventID("event-" + strconv.Itoa(ids.events)), nil
}

func (ids *credentialIDs) NewReceiptID() (id.ReceiptID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.recpt++
	return id.ReceiptID("receipt-" + strconv.Itoa(ids.recpt)), nil
}

// derivationResolver resolves one tenant key to deterministic bytes.
type derivationResolver struct{ key []byte }

func (resolver derivationResolver) ResolveDerivationKey(context.Context, token.TenantKey) ([]byte, error) {
	return append([]byte(nil), resolver.key...), nil
}

// credentialDeliverer accepts post-commit deliveries.
type credentialDeliverer struct{}

func (credentialDeliverer) Deliver(context.Context, token.Delivery) error { return nil }

// noInvalidations accepts post-commit invalidations.
type noInvalidations struct{}

func (noInvalidations) Invalidate(context.Context, token.Invalidation) error { return nil }

// deterministicReader is a fixed nonce source.
type deterministicReader struct{ fill byte }

func (reader deterministicReader) Read(buffer []byte) (int, error) {
	for index := range buffer {
		buffer[index] = reader.fill + byte(index)
	}
	return len(buffer), nil
}

// receiptFixture wires the real credential admin and manager onto the receipt
// lookup under test over one memory store.
type receiptFixture struct {
	store  *memory.Store
	lookup *readers.ReceiptLookup
	admin  *token.CredentialAdmin
	scope  tenancy.Scope
	actor  authorize.Principal
}

func newReceiptFixture(t *testing.T) *receiptFixture {
	t.Helper()
	backend := newStore()
	scope := testScope()
	identityAdapter, err := identitystore.New(identitystore.Config{Executor: backend, Scanner: backend, Clock: serviceClock{}})
	if err != nil {
		t.Fatalf("identity.New: %v", err)
	}
	lookup, err := readers.NewReceiptLookup(backend)
	if err != nil {
		t.Fatalf("NewReceiptLookup: %v", err)
	}
	seedTenantKey(t, backend)
	seedService(t, backend, scope, "service-1", 1, []tenancy.Role{tenancy.RoleEngineer}, nil)
	seedMembership(t, backend, scope, "membership-admin", testPrincipal, 1, tenancy.RoleAdmin)
	derivation := make([]byte, 32)
	for index := range derivation {
		derivation[index] = byte(index + 7)
	}
	manager, err := token.NewManager(token.ManagerConfig{
		Keys:          identityAdapter,
		Resolver:      derivationResolver{key: derivation},
		Credentials:   identityAdapter,
		Principals:    identityAdapter,
		Services:      identityAdapter,
		Mutations:     identityAdapter,
		Receipts:      lookup,
		Deliverer:     credentialDeliverer{},
		Invalidations: noInvalidations{},
		Clock:         serviceClock{},
		Random:        deterministicReader{fill: 1},
		Bounds:        token.DefaultBounds,
	})
	if err != nil {
		t.Fatalf("token.NewManager: %v", err)
	}
	admin, err := token.NewCredentialAdmin(token.CredentialAdminConfig{
		Authorizer:  allowAuthorizer{},
		Authority:   identityAdapter,
		Principals:  identityAdapter,
		Services:    identityAdapter,
		Credentials: identityAdapter,
		Manager:     manager,
		Receipts:    lookup,
		Clock:       serviceClock{},
		IDs:         &credentialIDs{},
	})
	if err != nil {
		t.Fatalf("token.NewCredentialAdmin: %v", err)
	}
	return &receiptFixture{
		store:  backend,
		lookup: lookup,
		admin:  admin,
		scope:  scope,
		actor:  authorize.Principal{ID: testPrincipal, Kind: identity.KindHuman, Scope: scope, Roles: []tenancy.Role{tenancy.RoleAdmin}},
	}
}

// seedTenantKey writes one tenant row carrying the structured derivation key.
func seedTenantKey(t *testing.T, backend *memory.Store) {
	t.Helper()
	key, err := token.NewTenantKey(testTenant, 1, token.TenantKeyMaterial{
		Ciphertext: []byte("ciphertext"),
		WrappedKey: []byte("wrapped-key"),
		WrapNonce:  []byte("wrap-nonce"),
		KeyRef:     "kms://factory-key",
		KeyVersion: 1,
	})
	if err != nil {
		t.Fatalf("NewTenantKey: %v", err)
	}
	encoded, err := json.Marshal(key)
	if err != nil {
		t.Fatalf("encode tenant key: %v", err)
	}
	if _, err := backend.Execute(context.Background(), tenancy.Scope{TenantID: testTenant}, store.Unit{
		Writes: []store.Write{{
			Table:  "tenants",
			ID:     testTenant,
			Insert: true,
			Values: map[string]any{
				"name":           "Tenant One",
				"version":        int64(1),
				"derivation_key": encoded,
				"created_at":     testNow,
			},
		}},
	}); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
}

// issueCommand is one service credential issue request.
func (fixture *receiptFixture) issueCommand(content []byte) token.ServiceCredentialCommand {
	return token.ServiceCredentialCommand{
		Actor:           fixture.actor,
		Scope:           fixture.scope,
		ServiceID:       service.ServiceID("service-1"),
		TTL:             5 * time.Minute,
		ExpectedVersion: 1,
		CallerKey:       "credential-issue",
		Content:         receipt.Content{MediaType: "application/json", Bytes: content},
	}
}

// serviceVersion reads the committed service version.
func (fixture *receiptFixture) serviceVersion(t *testing.T) uint64 {
	t.Helper()
	row, ok := fixture.store.Snapshot(fixture.scope, "services", "service-1")
	if !ok {
		t.Fatal("service row is missing")
	}
	value, ok := row.Values["version"].(int64)
	if !ok {
		t.Fatal("service row has no version")
	}
	return uint64(value)
}

// TestRealCredentialAdminLostResponseReplayThroughReceiptLookup proves an equal
// retry after a lost response replays the original secret-free result with no
// token and no second commit.
func TestRealCredentialAdminLostResponseReplayThroughReceiptLookup(t *testing.T) {
	fixture := newReceiptFixture(t)
	content := []byte(`{"service":"service-1","ttl":"5m"}`)
	first, err := fixture.admin.Issue(context.Background(), fixture.issueCommand(content))
	if err != nil {
		t.Fatalf("first issue: %v", err)
	}
	if first.Token == "" || first.Replayed {
		t.Fatalf("first issue = %+v; want a fresh token", first)
	}
	if version := fixture.serviceVersion(t); version != 2 {
		t.Fatalf("service version after issue = %d; want 2", version)
	}

	replayed, err := fixture.admin.Issue(context.Background(), fixture.issueCommand(content))
	if err != nil {
		t.Fatalf("replayed issue: %v", err)
	}
	if !replayed.Replayed {
		t.Fatalf("replayed = %+v; want Replayed", replayed)
	}
	if replayed.Token != "" {
		t.Fatalf("replayed token = %q; a replayed result must carry no token", replayed.Token)
	}
	if replayed.Delivery != nil || replayed.Delivered {
		t.Fatalf("replayed delivery = %+v delivered=%v; want none", replayed.Delivery, replayed.Delivered)
	}
	if replayed.State != first.State || replayed.Version != first.Version {
		t.Fatalf("replayed result = %+v; want the original %+v", replayed, first)
	}
	if version := fixture.serviceVersion(t); version != 2 {
		t.Fatalf("service version after replay = %d; want the original 2", version)
	}

	record, found, err := fixture.lookup.Lookup(context.Background(), receipt.Key{
		TenantID:    testTenant,
		ProjectID:   testProject,
		PrincipalID: testPrincipal,
		Operation:   "service.credential.issue",
		CallerKey:   "credential-issue",
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !found {
		t.Fatal("committed receipt was not found")
	}
	if strings.Contains(string(record.Result), first.Token) {
		t.Fatal("the durable receipt result contains the credential plaintext")
	}
	if strings.Contains(string(record.Result), `"token"`) || strings.Contains(string(record.Result), `"delivery"`) {
		t.Fatalf("the durable receipt result carries credential material: %s", record.Result)
	}
}

// TestReceiptLookupRefusesChangedContent proves an equal key with changed
// content is changed_idempotency_content and commits nothing.
func TestReceiptLookupRefusesChangedContent(t *testing.T) {
	fixture := newReceiptFixture(t)
	first, err := fixture.admin.Issue(context.Background(), fixture.issueCommand([]byte(`{"ttl":"5m"}`)))
	if err != nil {
		t.Fatalf("first issue: %v", err)
	}
	if first.Token == "" {
		t.Fatal("first issue returned no token")
	}
	if _, err := fixture.admin.Issue(context.Background(), fixture.issueCommand([]byte(`{"ttl":"6m"}`))); !isClass(err, fault.ChangedIdempotencyContent) {
		t.Fatalf("changed-content retry error = %v; want changed_idempotency_content", err)
	}
	if version := fixture.serviceVersion(t); version != 2 {
		t.Fatalf("service version = %d; a changed retry must not commit", version)
	}
}

// TestReceiptLookupScopeIsolation proves a receipt in another project is
// invisible to the scoped lookup.
func TestReceiptLookupScopeIsolation(t *testing.T) {
	fixture := newReceiptFixture(t)
	if _, err := fixture.admin.Issue(context.Background(), fixture.issueCommand([]byte(`{"ttl":"5m"}`))); err != nil {
		t.Fatalf("issue: %v", err)
	}
	foreign := receipt.Key{
		TenantID:    testTenant,
		ProjectID:   otherProject,
		PrincipalID: testPrincipal,
		Operation:   "service.credential.issue",
		CallerKey:   "credential-issue",
	}
	record, found, err := fixture.lookup.Lookup(context.Background(), foreign)
	if err != nil {
		t.Fatalf("foreign Lookup: %v", err)
	}
	if found {
		t.Fatalf("foreign lookup returned %+v; a cross-scope receipt must be invisible", record)
	}
	missing, found, err := fixture.lookup.Lookup(context.Background(), receipt.Key{
		TenantID:    testTenant,
		ProjectID:   testProject,
		PrincipalID: testPrincipal,
		Operation:   "service.credential.issue",
		CallerKey:   "never-used",
	})
	if err != nil {
		t.Fatalf("missing Lookup: %v", err)
	}
	if found || missing.ID != "" {
		t.Fatalf("missing lookup = %+v found=%v; want absent with no error", missing, found)
	}
}
