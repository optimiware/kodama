package event_test

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// deniedRoles lists valid principals of the supplied kind that hold no
// event.read/event.subscribe grant. A service can never hold Admin, so its
// denial case is Reviewer.
func deniedRoles(kind string) []tenancy.Role {
	if kind == "service" {
		return []tenancy.Role{tenancy.RoleReviewer}
	}
	return []tenancy.Role{tenancy.RoleAdmin, tenancy.RoleReviewer}
}

// humanWithRole is one current human principal holding exactly the supplied
// membership role in the test project.
func humanWithRole(role tenancy.Role) identity.Principal {
	return identity.Principal{
		ID:      id.PrincipalID("principal-human"),
		Kind:    identity.KindHuman,
		Scope:   testScope(),
		Roles:   []tenancy.Role{role},
		Enabled: true,
		Version: 1,
	}
}

// serviceWithRole is one current service principal holding exactly the supplied
// service-assignable role in the test project.
func serviceWithRole(role tenancy.Role) identity.Principal {
	return identity.Principal{
		ID:      id.PrincipalID("svc-git"),
		Kind:    identity.KindService,
		Scope:   testScope(),
		Roles:   []tenancy.Role{role},
		Enabled: true,
		Version: 1,
	}
}

// fakePrincipals answers the current-principal confirmation read. The service
// must re-read the principal after the matrix grant, so a stale or disabled
// snapshot can never authorize a read.
type fakePrincipals struct {
	mu        sync.Mutex
	principal identity.Principal
	err       error
	calls     int
}

func (principals *fakePrincipals) ReadPrincipal(_ context.Context, _ identity.PrincipalRequest) (identity.Principal, error) {
	principals.mu.Lock()
	defer principals.mu.Unlock()
	principals.calls++
	if principals.err != nil {
		return identity.Principal{}, principals.err
	}
	return principals.principal, nil
}

func (principals *fakePrincipals) callCount() int {
	principals.mu.Lock()
	defer principals.mu.Unlock()
	return principals.calls
}

// fakeReadPort is the scoped committed-event read seam. It records the exact
// query the service issued so scope/cursor/filter/limit propagation is visible.
type fakeReadPort struct {
	mu      sync.Mutex
	page    event.ReadPage
	err     error
	queries []event.ReadQuery
	calls   int
}

func (port *fakeReadPort) Read(_ context.Context, query event.ReadQuery) (event.ReadPage, error) {
	port.mu.Lock()
	defer port.mu.Unlock()
	port.calls++
	port.queries = append(port.queries, query)
	if port.err != nil {
		return event.ReadPage{}, port.err
	}
	return port.page, nil
}

func (port *fakeReadPort) callCount() int {
	port.mu.Lock()
	defer port.mu.Unlock()
	return port.calls
}

func (port *fakeReadPort) lastQuery() event.ReadQuery {
	port.mu.Lock()
	defer port.mu.Unlock()
	if len(port.queries) == 0 {
		return event.ReadQuery{}
	}
	return port.queries[len(port.queries)-1]
}

// readEnvelope builds one committed-looking event row for a page.
func readEnvelope(eventType event.Type, scope tenancy.Scope, eventID id.EventID) event.Envelope {
	return event.Envelope{
		ID:        eventID,
		Type:      eventType,
		Source:    event.Source{Kind: event.SourceShell, PrincipalID: "principal-human"},
		At:        fixedNow,
		Scope:     scope,
		SubjectID: "subject-1",
	}
}

type readHarness struct {
	authority  *fakeAuthority
	principals *fakePrincipals
	port       *fakeReadPort
	reader     *event.Reader
}

func newReadHarness(t *testing.T, actor identity.Principal) *readHarness {
	t.Helper()
	harness := &readHarness{
		authority:  &fakeAuthority{denied: map[authorize.AuthorityCheck]bool{}},
		principals: &fakePrincipals{principal: actor},
		port:       &fakeReadPort{},
	}
	reader, err := event.NewReader(event.ReadConfig{
		Authorizer: authorize.NewMatrix(),
		Authority:  harness.authority,
		Principals: harness.principals,
		Port:       harness.port,
	})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	harness.reader = reader
	return harness
}

func readCommand(actor identity.Principal, scope tenancy.Scope, limit int) event.ReadCommand {
	return event.ReadCommand{
		Actor: actor,
		Scope: scope,
		Query: event.ReadQuery{Limit: limit},
	}
}

// TestReaderAllowsEveryRegistryRole asserts the event.read handler admits
// exactly the declared human/service roles and refuses every other role.
func TestReaderAllowsEveryRegistryRole(t *testing.T) {
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
				harness := newReadHarness(t, actor)
				if _, err := harness.reader.Read(context.Background(), readCommand(actor, testScope(), 10)); err != nil {
					t.Fatalf("Read: %v", err)
				}
				if harness.port.callCount() != 1 {
					t.Errorf("read port calls = %d, want 1", harness.port.callCount())
				}
				if got := harness.port.lastQuery().Scope; got != testScope() {
					t.Errorf("read query scope = %#v, want the authorized %#v", got, testScope())
				}
			})
		}
		for _, role := range deniedRoles(kind.name) {
			t.Run(kind.name+"/"+string(role)+"/refused", func(t *testing.T) {
				actor := kind.principal(role)
				harness := newReadHarness(t, actor)
				_, err := harness.reader.Read(context.Background(), readCommand(actor, testScope(), 10))
				assertFaultClass(t, err, fault.Forbidden)
				if harness.port.callCount() != 0 {
					t.Errorf("read port calls = %d, want 0 for an ungranted role", harness.port.callCount())
				}
			})
		}
	}
}

// TestReaderRefusesExecutionPrincipal asserts event.read declares no execution
// permission, so an execution credential is refused before any read.
func TestReaderRefusesExecutionPrincipal(t *testing.T) {
	actor := executionPrincipal()
	harness := newReadHarness(t, actor)
	_, err := harness.reader.Read(context.Background(), readCommand(actor, testScope(), 10))
	assertFaultClass(t, err, fault.Forbidden)
	if harness.principals.callCount() != 0 {
		t.Errorf("principal reads = %d, want 0 for an execution principal", harness.principals.callCount())
	}
	if harness.authority.callCount() != 0 {
		t.Errorf("authority reads = %d, want 0 for an execution principal", harness.authority.callCount())
	}
	if harness.port.callCount() != 0 {
		t.Errorf("read port calls = %d, want 0 for an execution principal", harness.port.callCount())
	}
}

// TestReaderRefusesDisabledPrincipal asserts a principal disabled after the
// caller's snapshot is refused by the current-principal confirmation.
func TestReaderRefusesDisabledPrincipal(t *testing.T) {
	actor := humanWithRole(tenancy.RoleEngineer)
	harness := newReadHarness(t, actor)
	disabled := actor
	disabled.Enabled = false
	harness.principals.principal = disabled

	_, err := harness.reader.Read(context.Background(), readCommand(actor, testScope(), 10))
	assertFaultClass(t, err, fault.Forbidden)
	if harness.port.callCount() != 0 {
		t.Errorf("read port calls = %d, want 0 for a disabled principal", harness.port.callCount())
	}
}

// TestReaderRefusesCrossScope asserts a caller cannot read another tenant's or
// project's events.
func TestReaderRefusesCrossScope(t *testing.T) {
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
			harness := newReadHarness(t, actor)
			_, err := harness.reader.Read(context.Background(), readCommand(actor, testCase.scope, 10))
			assertFaultClass(t, err, fault.Forbidden)
			if harness.principals.callCount() != 0 {
				t.Errorf("principal reads = %d, want 0 for a cross-scope read", harness.principals.callCount())
			}
			if harness.port.callCount() != 0 {
				t.Errorf("read port calls = %d, want 0 for a cross-scope read", harness.port.callCount())
			}
		})
	}
}

// TestReaderAllowsContainedScope asserts a tenant-wide actor may read a project
// inside its tenant, while the query still carries the authorized project.
func TestReaderAllowsContainedScope(t *testing.T) {
	actor := humanWithRole(tenancy.RoleEngineer)
	actor.Scope = tenancy.Scope{TenantID: testTenant}
	harness := newReadHarness(t, actor)

	if _, err := harness.reader.Read(context.Background(), readCommand(actor, testScope(), 10)); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := harness.port.lastQuery().Scope; got != testScope() {
		t.Errorf("read query scope = %#v, want the authorized %#v", got, testScope())
	}
}

// TestReaderRefusesForeignQueryScope asserts a body query cannot widen the
// authorized scope.
func TestReaderRefusesForeignQueryScope(t *testing.T) {
	actor := humanWithRole(tenancy.RoleEngineer)
	harness := newReadHarness(t, actor)
	command := readCommand(actor, testScope(), 10)
	command.Query.Scope = otherScope()

	_, err := harness.reader.Read(context.Background(), command)
	assertFaultClass(t, err, fault.Forbidden)
	if harness.port.callCount() != 0 {
		t.Errorf("read port calls = %d, want 0 for a foreign query scope", harness.port.callCount())
	}
}

// TestReaderRefusesOutOfBoundsLimit asserts the 1..MaxReadLimit bound.
func TestReaderRefusesOutOfBoundsLimit(t *testing.T) {
	for _, limit := range []int{0, -1, event.MaxReadLimit + 1} {
		actor := humanWithRole(tenancy.RoleEngineer)
		harness := newReadHarness(t, actor)
		_, err := harness.reader.Read(context.Background(), readCommand(actor, testScope(), limit))
		assertFaultClass(t, err, fault.InvalidInput)
		if harness.port.callCount() != 0 {
			t.Errorf("limit %d: read port calls = %d, want 0", limit, harness.port.callCount())
		}
	}
}

// TestReaderRefusesUnknownTypeFilter asserts the optional filter only accepts
// declared lifecycle types.
func TestReaderRefusesUnknownTypeFilter(t *testing.T) {
	actor := humanWithRole(tenancy.RoleEngineer)
	harness := newReadHarness(t, actor)
	command := readCommand(actor, testScope(), 10)
	command.Query.Types = []event.Type{event.Type("bogus.event")}

	_, err := harness.reader.Read(context.Background(), command)
	assertFaultClass(t, err, fault.InvalidInput)
	if harness.port.callCount() != 0 {
		t.Errorf("read port calls = %d, want 0 for an unknown type filter", harness.port.callCount())
	}
}

// TestReaderPassesCursorAndTypeFilter asserts the authorized query reaches the
// port with its exclusive cursor, filter and limit intact.
func TestReaderPassesCursorAndTypeFilter(t *testing.T) {
	actor := humanWithRole(tenancy.RoleEngineer)
	harness := newReadHarness(t, actor)
	harness.port.page = event.ReadPage{
		Events:  []event.Envelope{readEnvelope(event.TypeTaskCreated, testScope(), id.EventID("event-1"))},
		Next:    11,
		HasMore: true,
	}
	command := readCommand(actor, testScope(), 5)
	command.Query.AfterSequence = 10
	command.Query.Types = []event.Type{event.TypeTaskCreated}

	page, err := harness.reader.Read(context.Background(), command)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if page.Next != 11 || !page.HasMore || len(page.Events) != 1 {
		t.Fatalf("page = %+v, want the port page", page)
	}
	query := harness.port.lastQuery()
	if query.Scope != testScope() || query.AfterSequence != 10 || query.Limit != 5 {
		t.Errorf("query = %+v, want scope/after/limit propagated", query)
	}
	if !reflect.DeepEqual(query.Types, []event.Type{event.TypeTaskCreated}) {
		t.Errorf("query types = %v, want the requested filter", query.Types)
	}
}

// TestReaderRefusesWhenCurrentAuthorityDoesNotHold asserts the membership read
// is confirmed before the port.
func TestReaderRefusesWhenCurrentAuthorityDoesNotHold(t *testing.T) {
	actor := humanWithRole(tenancy.RoleEngineer)
	harness := newReadHarness(t, actor)
	harness.authority.deny(authorize.CheckMembership)

	_, err := harness.reader.Read(context.Background(), readCommand(actor, testScope(), 10))
	assertFaultClass(t, err, fault.Forbidden)
	if harness.port.callCount() != 0 {
		t.Errorf("read port calls = %d, want 0 when membership is revoked", harness.port.callCount())
	}
}

// TestReaderRefusesWhenAuthorityReadFails asserts a dependency failure reading
// current authority is propagated and no read reaches the port.
func TestReaderRefusesWhenAuthorityReadFails(t *testing.T) {
	actor := humanWithRole(tenancy.RoleEngineer)
	harness := newReadHarness(t, actor)
	harness.authority.setError(fault.New(fault.DependencyUnavailable, "authority store unavailable"))

	_, err := harness.reader.Read(context.Background(), readCommand(actor, testScope(), 10))
	assertFaultClass(t, err, fault.DependencyUnavailable)
	if harness.port.callCount() != 0 {
		t.Errorf("read port calls = %d, want 0 when authority cannot be read", harness.port.callCount())
	}
}

// TestReaderAcceptsCursorBeyondTheTable asserts an unknown (future) cursor is
// passed through and an empty committed page is returned rather than treated as
// a failure or silently reset.
func TestReaderAcceptsCursorBeyondTheTable(t *testing.T) {
	actor := humanWithRole(tenancy.RoleEngineer)
	harness := newReadHarness(t, actor)
	harness.port.page = event.ReadPage{Next: 100, HasMore: false}
	command := readCommand(actor, testScope(), 10)
	command.Query.AfterSequence = 100

	page, err := harness.reader.Read(context.Background(), command)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(page.Events) != 0 || page.Next != 100 || page.HasMore {
		t.Fatalf("page = %+v, want an empty page retaining the cursor", page)
	}
	if got := harness.port.lastQuery().AfterSequence; got != 100 {
		t.Errorf("query after sequence = %d, want 100", got)
	}
}

// TestReaderRefusesForeignScopePage asserts a port that returns another scope's
// event is a dependency-contract failure, never a successful read.
func TestReaderRefusesForeignScopePage(t *testing.T) {
	actor := humanWithRole(tenancy.RoleEngineer)
	harness := newReadHarness(t, actor)
	harness.port.page = event.ReadPage{
		Events: []event.Envelope{readEnvelope(event.TypeTaskCreated, otherScope(), id.EventID("event-1"))},
		Next:   1,
	}

	page, err := harness.reader.Read(context.Background(), readCommand(actor, testScope(), 10))
	assertFaultClass(t, err, fault.InternalFailure)
	if len(page.Events) != 0 || page.Next != 0 {
		t.Errorf("refused read returned a page: %+v", page)
	}
}

// TestReaderRefusesUnrequestedTypeInPage asserts a port cannot bypass the
// authorized type filter.
func TestReaderRefusesUnrequestedTypeInPage(t *testing.T) {
	actor := humanWithRole(tenancy.RoleEngineer)
	harness := newReadHarness(t, actor)
	harness.port.page = event.ReadPage{
		Events: []event.Envelope{readEnvelope(event.TypeTaskCreated, testScope(), id.EventID("event-1"))},
		Next:   1,
	}
	command := readCommand(actor, testScope(), 10)
	command.Query.Types = []event.Type{event.TypeExecutionQueued}

	_, err := harness.reader.Read(context.Background(), command)
	assertFaultClass(t, err, fault.InternalFailure)
}

// TestReaderRefusesRegressedPageCursor asserts the returned cursor never moves
// behind the requested exclusive cursor.
func TestReaderRefusesRegressedPageCursor(t *testing.T) {
	actor := humanWithRole(tenancy.RoleEngineer)
	harness := newReadHarness(t, actor)
	harness.port.page = event.ReadPage{Next: 9}
	command := readCommand(actor, testScope(), 10)
	command.Query.AfterSequence = 10

	_, err := harness.reader.Read(context.Background(), command)
	assertFaultClass(t, err, fault.InternalFailure)
}

// TestReaderRefusesPartialComposition asserts the reader cannot be composed
// with a missing dependency.
func TestReaderRefusesPartialComposition(t *testing.T) {
	base := func() event.ReadConfig {
		return event.ReadConfig{
			Authorizer: authorize.NewMatrix(),
			Authority:  &fakeAuthority{denied: map[authorize.AuthorityCheck]bool{}},
			Principals: &fakePrincipals{principal: humanWithRole(tenancy.RoleEngineer)},
			Port:       &fakeReadPort{},
		}
	}
	cases := []struct {
		name   string
		mutate func(*event.ReadConfig)
	}{
		{"missing authorizer", func(config *event.ReadConfig) { config.Authorizer = nil }},
		{"missing authority", func(config *event.ReadConfig) { config.Authority = nil }},
		{"missing principals", func(config *event.ReadConfig) { config.Principals = nil }},
		{"missing port", func(config *event.ReadConfig) { config.Port = nil }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := base()
			testCase.mutate(&config)
			if _, err := event.NewReader(config); err == nil {
				t.Fatal("NewReader accepted a partial composition")
			} else {
				assertFaultClass(t, err, fault.InvalidInput)
			}
		})
	}
}

// TestReadSurfaceCarriesNoCredential asserts the authorized read command/page
// expose no credential, token or internal field.
func TestReadSurfaceCarriesNoCredential(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[event.ReadCommand](),
		reflect.TypeFor[event.ReadPage](),
		reflect.TypeFor[event.ReadPort](),
		reflect.TypeFor[event.CursorPort](),
	} {
		assertNoSecretFields(t, typ, typ.Name(), 0, map[reflect.Type]bool{})
	}
}
