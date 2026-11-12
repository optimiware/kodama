package readers_test

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	identitystore "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// commitEvent writes one valid committed event in scope through the neutral
// seam and returns its durable publication sequence.
func commitEvent(t *testing.T, backend *memory.Store, scope tenancy.Scope, eventID id.EventID) uint64 {
	t.Helper()
	envelope, err := event.NewEnvelope(eventID, event.Source{Kind: event.SourceShell, PrincipalID: testPrincipal}, scope, testNow, event.EnvelopeInput{
		Type:           event.TypeExecutionQueued,
		SubjectID:      "execution-subject",
		SubjectVersion: 1,
	})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if _, err := backend.Execute(context.Background(), scope, store.Unit{Events: []event.Envelope{envelope}}); err != nil {
		t.Fatalf("commit event: %v", err)
	}
	row, ok := backend.Snapshot(scope, "events", string(eventID))
	if !ok {
		t.Fatalf("committed event %q is not readable", eventID)
	}
	value, ok := row.Values["publish_sequence"].(int64)
	if !ok {
		t.Fatalf("event %q has no publication sequence", eventID)
	}
	return uint64(value)
}

// seedService writes one enabled service row with a current credential pair.
func seedService(t *testing.T, backend *memory.Store, scope tenancy.Scope, serviceID id.PrincipalID, version uint64, roles []tenancy.Role, expiry *time.Time) {
	t.Helper()
	encoded, err := jsonMarshal(roles)
	if err != nil {
		t.Fatalf("encode service roles: %v", err)
	}
	values := map[string]any{
		"name":             string(serviceID),
		"state":            "enabled",
		"roles":            encoded,
		"token_nonce":      nil,
		"token_expires_at": nil,
		"version":          int64(version),
		"created_at":       testNow,
	}
	if expiry != nil {
		nonce := make([]byte, 32)
		for index := range nonce {
			nonce[index] = byte(index + 1)
		}
		values["token_nonce"] = nonce
		values["token_expires_at"] = *expiry
	}
	if _, err := backend.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{Table: "services", ID: string(serviceID), Insert: true, Values: values}},
	}); err != nil {
		t.Fatalf("seed service: %v", err)
	}
}

// TestCursorReaderReadsTheScopedHighWaterSequence proves the cursor is the
// highest committed sequence in scope, zero for an empty stream, and stable
// across repeated reads.
func TestCursorReaderReadsTheScopedHighWaterSequence(t *testing.T) {
	backend := newStore()
	cursor, err := readers.NewCursorReader(backend)
	if err != nil {
		t.Fatalf("NewCursorReader: %v", err)
	}
	empty, err := cursor.Cursor(context.Background(), testScope())
	if err != nil {
		t.Fatalf("empty cursor: %v", err)
	}
	if empty != 0 {
		t.Fatalf("empty cursor = %d; want 0", empty)
	}
	first := commitEvent(t, backend, testScope(), "event-1")
	second := commitEvent(t, backend, testScope(), "event-2")
	other := commitEvent(t, backend, otherScope(), "event-3")
	tenant := commitEvent(t, backend, tenancy.Scope{TenantID: testTenant}, "event-4")
	if second <= first || other <= second || tenant <= other {
		t.Fatalf("sequences are not globally increasing: %d %d %d %d", first, second, other, tenant)
	}
	got, err := cursor.Cursor(context.Background(), testScope())
	if err != nil {
		t.Fatalf("Cursor: %v", err)
	}
	if got != second {
		t.Fatalf("project cursor = %d; want %d", got, second)
	}
	again, err := cursor.Cursor(context.Background(), testScope())
	if err != nil {
		t.Fatalf("second Cursor: %v", err)
	}
	if again != got {
		t.Fatalf("cursor is not deterministic: %d then %d", got, again)
	}
	foreign, err := cursor.Cursor(context.Background(), otherScope())
	if err != nil {
		t.Fatalf("foreign Cursor: %v", err)
	}
	if foreign != other {
		t.Fatalf("foreign cursor = %d; want its own %d", foreign, other)
	}
	tenantCursor, err := cursor.Cursor(context.Background(), tenancy.Scope{TenantID: testTenant})
	if err != nil {
		t.Fatalf("tenant Cursor: %v", err)
	}
	if tenantCursor != tenant {
		t.Fatalf("tenant cursor = %d; want the tenant-wide event %d", tenantCursor, tenant)
	}
}

// TestCredentialValidityReadsExecutionAndServiceExpiry proves the reader returns
// the stored expiry of the principal's current credential and reports a
// principal with no current pair as not_found.
func TestCredentialValidityReadsExecutionAndServiceExpiry(t *testing.T) {
	backend := newStore()
	validity, err := readers.NewCredentialValidity(backend)
	if err != nil {
		t.Fatalf("NewCredentialValidity: %v", err)
	}
	scope := testScope()
	expiry := testNow.Add(time.Hour)
	seedService(t, backend, scope, "service-1", 1, []tenancy.Role{tenancy.RoleEngineer}, &expiry)
	seedService(t, backend, scope, "service-empty", 1, []tenancy.Role{tenancy.RoleEngineer}, nil)

	servicePrincipal := identity.Principal{
		ID:      "service-1",
		Kind:    identity.KindService,
		Scope:   scope,
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	}
	got, err := validity.ValidUntil(context.Background(), servicePrincipal)
	if err != nil {
		t.Fatalf("service ValidUntil: %v", err)
	}
	if !got.Equal(expiry) {
		t.Fatalf("service expiry = %v; want %v", got, expiry)
	}
	emptyPrincipal := servicePrincipal
	emptyPrincipal.ID = "service-empty"
	if _, err := validity.ValidUntil(context.Background(), emptyPrincipal); !isClass(err, fault.NotFound) {
		t.Fatalf("absent service credential error = %v; want not_found", err)
	}
}

// TestCredentialValidityExecutionExpiryAndHumanDeferral proves an execution's
// expiry is read from its execution row and a human principal has no
// store-backed validity bound.
func TestCredentialValidityExecutionExpiryAndHumanDeferral(t *testing.T) {
	backend := newStore()
	validity, err := readers.NewCredentialValidity(backend)
	if err != nil {
		t.Fatalf("NewCredentialValidity: %v", err)
	}
	scope := testScope()
	expiry := testNow.Add(30 * time.Minute)
	if _, err := backend.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "executions",
			ID:     "execution-credential",
			Insert: true,
			Values: map[string]any{
				"pipeline_id":      nil,
				"task_id":          nil,
				"principal_id":     "execution-principal-9",
				"state":            string(kernel.ExecutionRunning),
				"version":          int64(1),
				"yaml_sha256":      []byte("sha256"),
				"snapshot":         []byte(`{}`),
				"token_nonce":      make([]byte, 32),
				"token_expires_at": expiry,
				"workspace_id":     "ws-credential",
				"created_at":       testNow,
			},
		}},
	}); err != nil {
		t.Fatalf("seed execution credential: %v", err)
	}
	executionPrincipal := identity.Principal{
		ID:          "execution-principal-9",
		Kind:        identity.KindExecution,
		Scope:       scope,
		Enabled:     true,
		Version:     1,
		ExecutionID: "execution-credential",
	}
	got, err := validity.ValidUntil(context.Background(), executionPrincipal)
	if err != nil {
		t.Fatalf("execution ValidUntil: %v", err)
	}
	if !got.Equal(expiry) {
		t.Fatalf("execution expiry = %v; want %v", got, expiry)
	}
	human := identity.Principal{ID: testPrincipal, Kind: identity.KindHuman, Scope: scope, Roles: []tenancy.Role{tenancy.RoleEngineer}, Enabled: true, Version: 1}
	if _, err := validity.ValidUntil(context.Background(), human); !isClass(err, fault.NotFound) {
		t.Fatalf("human validity error = %v; want not_found (OIDC boundary is not store-backed)", err)
	}
}

// TestRealSubscriberSetupReadsCursorAndCredentialValidity drives the real event
// subscriber onto the cursor and validity readers over one memory store.
func TestRealSubscriberSetupReadsCursorAndCredentialValidity(t *testing.T) {
	backend := newStore()
	scope := testScope()
	identityAdapter, err := identitystore.New(identitystore.Config{Executor: backend, Scanner: backend, Clock: serviceClock{}})
	if err != nil {
		t.Fatalf("identity.New: %v", err)
	}
	cursor, err := readers.NewCursorReader(backend)
	if err != nil {
		t.Fatalf("NewCursorReader: %v", err)
	}
	validity, err := readers.NewCredentialValidity(backend)
	if err != nil {
		t.Fatalf("NewCredentialValidity: %v", err)
	}
	subscriber, err := event.NewSubscriber(event.SubscribeConfig{
		Authorizer: allowAuthorizer{},
		Authority:  identityAdapter,
		Principals: identityAdapter,
		Cursor:     cursor,
		Validity:   validity,
		Clock:      serviceClock{},
	})
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	expiry := testNow.Add(time.Hour)
	seedService(t, backend, scope, "service-1", 1, []tenancy.Role{tenancy.RoleEngineer}, &expiry)
	highWater := commitEvent(t, backend, scope, "event-1")

	actor := authorize.Principal{ID: "service-1", Kind: identity.KindService, Scope: scope, Roles: []tenancy.Role{tenancy.RoleEngineer}}
	grant, err := subscriber.Setup(context.Background(), event.SubscribeCommand{
		Actor: actor,
		Scope: scope,
		Setup: event.SubscribeSetup{},
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if grant.Cursor != highWater {
		t.Fatalf("grant cursor = %d; want the committed high-water %d", grant.Cursor, highWater)
	}
	if !grant.ExpiresAt.Equal(expiry) {
		t.Fatalf("grant expiry = %v; want the credential expiry %v", grant.ExpiresAt, expiry)
	}
	if _, err := subscriber.Setup(context.Background(), event.SubscribeCommand{
		Actor: actor,
		Scope: scope,
		Setup: event.SubscribeSetup{Cursor: highWater + 1},
	}); !isClass(err, fault.InvalidInput) {
		t.Fatalf("ahead cursor error = %v; want invalid_input", err)
	}

	// A human membership has no store-backed credential expiry; the subscriber
	// reports that absence rather than inventing a bound.
	seedMembership(t, backend, scope, "membership-1", testPrincipal, 1, tenancy.RoleEngineer)
	humanActor := authorize.Principal{ID: testPrincipal, Kind: identity.KindHuman, Scope: scope, Roles: []tenancy.Role{tenancy.RoleEngineer}}
	if _, err := subscriber.Setup(context.Background(), event.SubscribeCommand{
		Actor: humanActor,
		Scope: scope,
		Setup: event.SubscribeSetup{},
	}); !isClass(err, fault.NotFound) {
		t.Fatalf("human setup error = %v; want not_found", err)
	}
}
