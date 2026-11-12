package event_test

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// fakeCursor is the consumer-owned resume-cursor seam. It reports the latest
// committed event-table sequence for one scope.
type fakeCursor struct {
	mu     sync.Mutex
	cursor uint64
	err    error
	scopes []tenancy.Scope
	calls  int
}

func (cursor *fakeCursor) Cursor(_ context.Context, scope tenancy.Scope) (uint64, error) {
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	cursor.calls++
	cursor.scopes = append(cursor.scopes, scope)
	if cursor.err != nil {
		return 0, cursor.err
	}
	return cursor.cursor, nil
}

func (cursor *fakeCursor) callCount() int {
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	return cursor.calls
}

func (cursor *fakeCursor) lastScope() tenancy.Scope {
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	if len(cursor.scopes) == 0 {
		return tenancy.Scope{}
	}
	return cursor.scopes[len(cursor.scopes)-1]
}

// fakeValidity is the consumer-owned credential-validity seam. A subscription
// grant may never outlive the principal's current credential.
type fakeValidity struct {
	mu     sync.Mutex
	until  time.Time
	err    error
	calls  int
	actors []identity.Principal
}

func (validity *fakeValidity) ValidUntil(_ context.Context, principal identity.Principal) (time.Time, error) {
	validity.mu.Lock()
	defer validity.mu.Unlock()
	validity.calls++
	validity.actors = append(validity.actors, principal)
	if validity.err != nil {
		return time.Time{}, validity.err
	}
	return validity.until, nil
}

func (validity *fakeValidity) callCount() int {
	validity.mu.Lock()
	defer validity.mu.Unlock()
	return validity.calls
}

type subscribeHarness struct {
	authority  *fakeAuthority
	principals *fakePrincipals
	cursor     *fakeCursor
	validity   *fakeValidity
	clock      fakeClock
	subscriber *event.Subscriber
}

func newSubscribeHarness(t *testing.T, actor identity.Principal) *subscribeHarness {
	t.Helper()
	harness := &subscribeHarness{
		authority:  &fakeAuthority{denied: map[authorize.AuthorityCheck]bool{}},
		principals: &fakePrincipals{principal: actor},
		cursor:     &fakeCursor{cursor: 42},
		validity:   &fakeValidity{until: fixedNow.Add(time.Hour)},
		clock:      fakeClock{now: fixedNow},
	}
	subscriber, err := event.NewSubscriber(event.SubscribeConfig{
		Authorizer: authorize.NewMatrix(),
		Authority:  harness.authority,
		Principals: harness.principals,
		Cursor:     harness.cursor,
		Validity:   harness.validity,
		Clock:      harness.clock,
	})
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	harness.subscriber = subscriber
	return harness
}

func subscribeCommand(actor identity.Principal, scope tenancy.Scope) event.SubscribeCommand {
	return event.SubscribeCommand{
		Actor: actor,
		Scope: scope,
		Setup: event.SubscribeSetup{Scope: scope},
	}
}

// TestSubscriberAllowsEveryRegistryRole asserts event.subscribe admits exactly
// the declared human/service roles and refuses every other role.
func TestSubscriberAllowsEveryRegistryRole(t *testing.T) {
	kinds := []struct {
		name      string
		principal func(tenancy.Role) identity.Principal
	}{
		{"human", humanWithRole},
		{"service", serviceWithRole},
	}
	for _, kind := range kinds {
		for _, role := range []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator, tenancy.RoleViewer} {
			t.Run(kind.name+"/"+string(role)+"/allowed", func(t *testing.T) {
				actor := kind.principal(role)
				harness := newSubscribeHarness(t, actor)
				if _, err := harness.subscriber.Setup(context.Background(), subscribeCommand(actor, testScope())); err != nil {
					t.Fatalf("Setup: %v", err)
				}
				if harness.cursor.callCount() != 1 || harness.validity.callCount() != 1 {
					t.Errorf("cursor/validity calls = %d/%d, want 1/1", harness.cursor.callCount(), harness.validity.callCount())
				}
			})
		}
		for _, role := range deniedRoles(kind.name) {
			t.Run(kind.name+"/"+string(role)+"/refused", func(t *testing.T) {
				actor := kind.principal(role)
				harness := newSubscribeHarness(t, actor)
				_, err := harness.subscriber.Setup(context.Background(), subscribeCommand(actor, testScope()))
				assertFaultClass(t, err, fault.Forbidden)
				if harness.cursor.callCount() != 0 || harness.validity.callCount() != 0 {
					t.Errorf("cursor/validity calls = %d/%d, want 0/0 for an ungranted role", harness.cursor.callCount(), harness.validity.callCount())
				}
			})
		}
	}
}

// TestSubscriberRefusesExecutionPrincipal asserts event.subscribe declares no
// execution permission, so an execution credential is refused before any setup
// read.
func TestSubscriberRefusesExecutionPrincipal(t *testing.T) {
	actor := executionPrincipal()
	harness := newSubscribeHarness(t, actor)
	_, err := harness.subscriber.Setup(context.Background(), subscribeCommand(actor, testScope()))
	assertFaultClass(t, err, fault.Forbidden)
	if harness.cursor.callCount() != 0 || harness.validity.callCount() != 0 {
		t.Errorf("cursor/validity calls = %d/%d, want 0/0 for an execution principal", harness.cursor.callCount(), harness.validity.callCount())
	}
}

// TestSubscriberRefusesDisabledPrincipal asserts the current-principal
// confirmation refuses a principal disabled after the caller's snapshot.
func TestSubscriberRefusesDisabledPrincipal(t *testing.T) {
	actor := humanWithRole(tenancy.RoleEngineer)
	harness := newSubscribeHarness(t, actor)
	disabled := actor
	disabled.Enabled = false
	harness.principals.principal = disabled

	_, err := harness.subscriber.Setup(context.Background(), subscribeCommand(actor, testScope()))
	assertFaultClass(t, err, fault.Forbidden)
	if harness.cursor.callCount() != 0 || harness.validity.callCount() != 0 {
		t.Errorf("cursor/validity calls = %d/%d, want 0/0 for a disabled principal", harness.cursor.callCount(), harness.validity.callCount())
	}
}

// TestSubscriberRefusesCrossScope asserts a caller cannot subscribe to another
// tenant's or project's events.
func TestSubscriberRefusesCrossScope(t *testing.T) {
	cases := []struct {
		name  string
		actor identity.Principal
		scope tenancy.Scope
	}{
		{"other tenant", humanWithRole(tenancy.RoleEngineer), otherScope()},
		{"other project", humanWithRole(tenancy.RoleEngineer), tenancy.Scope{TenantID: testTenant, ProjectID: otherProject}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			actor := testCase.actor
			harness := newSubscribeHarness(t, actor)
			_, err := harness.subscriber.Setup(context.Background(), subscribeCommand(actor, testCase.scope))
			assertFaultClass(t, err, fault.Forbidden)
			if harness.cursor.callCount() != 0 || harness.validity.callCount() != 0 {
				t.Errorf("cursor/validity calls = %d/%d, want 0/0 for a cross-scope setup", harness.cursor.callCount(), harness.validity.callCount())
			}
		})
	}
}

// TestSubscriberRefusesForeignSetupScope asserts a body setup cannot widen the
// authorized scope.
func TestSubscriberRefusesForeignSetupScope(t *testing.T) {
	actor := humanWithRole(tenancy.RoleEngineer)
	harness := newSubscribeHarness(t, actor)
	command := subscribeCommand(actor, testScope())
	command.Setup.Scope = otherScope()

	_, err := harness.subscriber.Setup(context.Background(), command)
	assertFaultClass(t, err, fault.Forbidden)
	if harness.cursor.callCount() != 0 || harness.validity.callCount() != 0 {
		t.Errorf("cursor/validity calls = %d/%d, want 0/0 for a foreign setup scope", harness.cursor.callCount(), harness.validity.callCount())
	}
}

// TestSubscriberRefusesUnknownTypeFilter asserts the optional filter only
// accepts declared lifecycle types.
func TestSubscriberRefusesUnknownTypeFilter(t *testing.T) {
	actor := humanWithRole(tenancy.RoleEngineer)
	harness := newSubscribeHarness(t, actor)
	command := subscribeCommand(actor, testScope())
	command.Setup.Types = []event.Type{event.Type("bogus.event")}

	_, err := harness.subscriber.Setup(context.Background(), command)
	assertFaultClass(t, err, fault.InvalidInput)
	if harness.cursor.callCount() != 0 || harness.validity.callCount() != 0 {
		t.Errorf("cursor/validity calls = %d/%d, want 0/0 for an unknown type filter", harness.cursor.callCount(), harness.validity.callCount())
	}
}

// TestSubscriberBuildsScopeQualifiedSubjects asserts the grant names exactly
// the authorized project's committed-event subject: no wildcard, no foreign
// scope and a bounded list.
func TestSubscriberBuildsScopeQualifiedSubjects(t *testing.T) {
	actor := humanWithRole(tenancy.RoleEngineer)
	harness := newSubscribeHarness(t, actor)
	grant, err := harness.subscriber.Setup(context.Background(), subscribeCommand(actor, testScope()))
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	want := []string{"kodama.v1.tenant-a.project-a.events"}
	if !reflect.DeepEqual(grant.Subjects, want) {
		t.Fatalf("subjects = %v, want %v", grant.Subjects, want)
	}
	if len(grant.Subjects) > event.MaxSubscribeSubjects {
		t.Errorf("subject count = %d, want at most %d", len(grant.Subjects), event.MaxSubscribeSubjects)
	}
	for _, subject := range grant.Subjects {
		if strings.ContainsAny(subject, "*>") {
			t.Errorf("subject %q is a wildcard", subject)
		}
	}
	if err := grant.ValidateFor(testScope()); err != nil {
		t.Errorf("grant does not validate for its scope: %v", err)
	}
	if err := grant.Validate(); err != nil {
		t.Errorf("grant does not validate: %v", err)
	}
}

// TestSubscriberUsesCurrentCursorWhenUnset asserts an unset resume point starts
// at the event table's current committed sequence.
func TestSubscriberUsesCurrentCursorWhenUnset(t *testing.T) {
	actor := humanWithRole(tenancy.RoleEngineer)
	harness := newSubscribeHarness(t, actor)
	harness.cursor.cursor = 42
	grant, err := harness.subscriber.Setup(context.Background(), subscribeCommand(actor, testScope()))
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if grant.Cursor != 42 {
		t.Errorf("cursor = %d, want the current committed sequence 42", grant.Cursor)
	}
	if harness.cursor.lastScope() != testScope() {
		t.Errorf("cursor scope = %#v, want %#v", harness.cursor.lastScope(), testScope())
	}
}

// TestSubscriberAcceptsStaleCursor asserts an explicit older resume point is
// retained so a subscriber can replay committed events.
func TestSubscriberAcceptsStaleCursor(t *testing.T) {
	actor := humanWithRole(tenancy.RoleEngineer)
	harness := newSubscribeHarness(t, actor)
	harness.cursor.cursor = 42
	command := subscribeCommand(actor, testScope())
	command.Setup.Cursor = 5

	grant, err := harness.subscriber.Setup(context.Background(), command)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if grant.Cursor != 5 {
		t.Errorf("cursor = %d, want the requested stale cursor 5", grant.Cursor)
	}
}

// TestSubscriberRefusesUnknownCursor asserts a resume point ahead of the
// committed event table is refused rather than silently clamped.
func TestSubscriberRefusesUnknownCursor(t *testing.T) {
	actor := humanWithRole(tenancy.RoleEngineer)
	harness := newSubscribeHarness(t, actor)
	harness.cursor.cursor = 42
	command := subscribeCommand(actor, testScope())
	command.Setup.Cursor = 43

	_, err := harness.subscriber.Setup(context.Background(), command)
	assertFaultClass(t, err, fault.InvalidInput)
	if harness.cursor.callCount() != 1 {
		t.Errorf("cursor calls = %d, want 1 to compare the requested resume point", harness.cursor.callCount())
	}
}

// TestSubscriberExpiryIsCredentialValidity asserts the grant expiry is exactly
// the current credential's validity and never longer.
func TestSubscriberExpiryIsCredentialValidity(t *testing.T) {
	actor := humanWithRole(tenancy.RoleEngineer)
	harness := newSubscribeHarness(t, actor)
	validUntil := fixedNow.Add(2 * time.Hour)
	harness.validity.until = validUntil

	grant, err := harness.subscriber.Setup(context.Background(), subscribeCommand(actor, testScope()))
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if !grant.ExpiresAt.Equal(validUntil) {
		t.Fatalf("expiry = %v, want the credential validity %v", grant.ExpiresAt, validUntil)
	}
	if grant.ExpiresAt.After(validUntil) {
		t.Errorf("expiry = %v is longer than the credential validity %v", grant.ExpiresAt, validUntil)
	}
}

// TestSubscriberRefusesExpiredCredential asserts an expired credential cannot
// obtain a subscription grant.
func TestSubscriberRefusesExpiredCredential(t *testing.T) {
	for _, until := range []time.Time{fixedNow, fixedNow.Add(-time.Minute)} {
		actor := humanWithRole(tenancy.RoleEngineer)
		harness := newSubscribeHarness(t, actor)
		harness.validity.until = until

		_, err := harness.subscriber.Setup(context.Background(), subscribeCommand(actor, testScope()))
		assertFaultClass(t, err, fault.Forbidden)
		if harness.cursor.callCount() != 0 {
			t.Errorf("cursor calls = %d, want 0 for an expired credential", harness.cursor.callCount())
		}
	}
}

// TestSubscriberRefusesWhenCurrentAuthorityDoesNotHold asserts every
// event.subscribe authority read is confirmed before any setup effect.
func TestSubscriberRefusesWhenCurrentAuthorityDoesNotHold(t *testing.T) {
	for _, check := range []authorize.AuthorityCheck{authorize.CheckMembership, authorize.CheckPrincipalEnabled} {
		actor := humanWithRole(tenancy.RoleEngineer)
		harness := newSubscribeHarness(t, actor)
		harness.authority.deny(check)

		_, err := harness.subscriber.Setup(context.Background(), subscribeCommand(actor, testScope()))
		assertFaultClass(t, err, fault.Forbidden)
		if harness.cursor.callCount() != 0 || harness.validity.callCount() != 0 {
			t.Errorf("%s: cursor/validity calls = %d/%d, want 0/0", check, harness.cursor.callCount(), harness.validity.callCount())
		}
	}
}

// TestSubscriberRefusesWhenAuthorityReadFails asserts a dependency failure
// reading current authority is propagated and no grant is produced.
func TestSubscriberRefusesWhenAuthorityReadFails(t *testing.T) {
	actor := humanWithRole(tenancy.RoleEngineer)
	harness := newSubscribeHarness(t, actor)
	harness.authority.setError(fault.New(fault.DependencyUnavailable, "authority store unavailable"))

	_, err := harness.subscriber.Setup(context.Background(), subscribeCommand(actor, testScope()))
	assertFaultClass(t, err, fault.DependencyUnavailable)
	if harness.cursor.callCount() != 0 || harness.validity.callCount() != 0 {
		t.Errorf("cursor/validity calls = %d/%d, want 0/0 when authority cannot be read", harness.cursor.callCount(), harness.validity.callCount())
	}
}

// TestSubscriberRefusesPartialComposition asserts the subscriber cannot be
// composed with a missing dependency.
func TestSubscriberRefusesPartialComposition(t *testing.T) {
	base := func() event.SubscribeConfig {
		return event.SubscribeConfig{
			Authorizer: authorize.NewMatrix(),
			Authority:  &fakeAuthority{denied: map[authorize.AuthorityCheck]bool{}},
			Principals: &fakePrincipals{principal: humanWithRole(tenancy.RoleEngineer)},
			Cursor:     &fakeCursor{cursor: 1},
			Validity:   &fakeValidity{until: fixedNow.Add(time.Hour)},
			Clock:      fakeClock{now: fixedNow},
		}
	}
	cases := []struct {
		name   string
		mutate func(*event.SubscribeConfig)
	}{
		{"missing authorizer", func(config *event.SubscribeConfig) { config.Authorizer = nil }},
		{"missing authority", func(config *event.SubscribeConfig) { config.Authority = nil }},
		{"missing principals", func(config *event.SubscribeConfig) { config.Principals = nil }},
		{"missing cursor", func(config *event.SubscribeConfig) { config.Cursor = nil }},
		{"missing validity", func(config *event.SubscribeConfig) { config.Validity = nil }},
		{"missing clock", func(config *event.SubscribeConfig) { config.Clock = nil }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := base()
			testCase.mutate(&config)
			if _, err := event.NewSubscriber(config); err == nil {
				t.Fatal("NewSubscriber accepted a partial composition")
			} else {
				assertFaultClass(t, err, fault.InvalidInput)
			}
		})
	}
}

// TestEventSubjectIsInjectiveAndScopeQualified asserts the subject builder
// follows the declared subject shape: a project is never encoded as the
// tenant-wide placeholder and a wildcard is never produced.
func TestEventSubjectIsInjectiveAndScopeQualified(t *testing.T) {
	project, err := event.EventSubject(testScope())
	if err != nil {
		t.Fatalf("EventSubject: %v", err)
	}
	if project != "kodama.v1.tenant-a.project-a.events" {
		t.Errorf("project subject = %q", project)
	}
	tenant, err := event.EventSubject(tenancy.Scope{TenantID: testTenant})
	if err != nil {
		t.Fatalf("EventSubject tenant-wide: %v", err)
	}
	if tenant != "kodama.v1.tenant-a._.events" {
		t.Errorf("tenant-wide subject = %q", tenant)
	}
	if project == tenant {
		t.Error("project and tenant-wide subjects are not injective")
	}
	if _, err := event.EventSubject(tenancy.Scope{TenantID: testTenant, ProjectID: id.ProjectID("_")}); err == nil {
		t.Error("EventSubject accepted a project id \"_\" and would collide with the tenant-wide subject")
	} else {
		assertFaultClass(t, err, fault.InvalidInput)
	}
	if _, err := event.EventSubject(tenancy.Scope{}); err == nil {
		t.Error("EventSubject accepted an empty scope")
	}
}

// TestSubscribeSurfaceCarriesNoCredential asserts the setup command and grant
// expose no credential, token or internal field.
func TestSubscribeSurfaceCarriesNoCredential(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[event.SubscribeCommand](),
		reflect.TypeFor[event.SubscribeGrant](),
		reflect.TypeFor[event.CursorPort](),
		reflect.TypeFor[event.CredentialValidity](),
	} {
		assertNoSecretFields(t, typ, typ.Name(), 0, map[reflect.Type]bool{})
	}
}
