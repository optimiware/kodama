// Service administration tests.
//
// The Admin service builds one complete commit unit per registry operation:
// service.create, service.update, service.enable and service.disable. Every
// command authorizes against the declared matrix, re-reads the current
// principal, carries the registry authority reads, builds the
// receipt/audit/event intent and applies exactly one AdminPort commit. A
// disablement additionally carries the mandatory RevokeAll obligation and
// invalidates established connections only after the commit succeeded.
package service_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

var adminNow = time.Date(2026, time.November, 4, 12, 0, 0, 0, time.UTC)

type adminClock struct{ now time.Time }

func (clock adminClock) Now() time.Time { return clock.now }

type adminIDs struct {
	mu   sync.Mutex
	next int
}

func (source *adminIDs) NewEventID() (id.EventID, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.next++
	return id.EventID(fmt.Sprintf("event-%d", source.next)), nil
}

func (source *adminIDs) NewReceiptID() (id.ReceiptID, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.next++
	return id.ReceiptID(fmt.Sprintf("receipt-%d", source.next)), nil
}

var (
	_ service.Clock    = adminClock{}
	_ service.IDSource = (*adminIDs)(nil)
)

type adminAuthorizer struct {
	mu         sync.Mutex
	decision   authorize.Decision
	err        error
	calls      int
	operations []authorize.OperationName
}

func (fake *adminAuthorizer) Authorize(_ context.Context, _ authorize.Principal, operation authorize.OperationName, _ tenancy.Scope) (authorize.Decision, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	fake.operations = append(fake.operations, operation)
	if fake.err != nil {
		return "", fake.err
	}
	if fake.decision == "" {
		return authorize.DecisionAllow, nil
	}
	return fake.decision, nil
}

type adminAuthority struct {
	mu    sync.Mutex
	err   error
	deny  authorize.AuthorityCheck
	calls int
	reads []authorize.AuthorityRead
}

func (fake *adminAuthority) Read(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	fake.reads = append(fake.reads, reads...)
	if fake.err != nil {
		return nil, fake.err
	}
	facts := make([]authorize.AuthorityFact, len(reads))
	for index, read := range reads {
		facts[index] = authorize.AuthorityFact{
			Check:       read.Check,
			Scope:       read.Scope,
			PrincipalID: read.PrincipalID,
			TargetID:    read.TargetID,
			Version:     read.ExpectedVersion,
			Allowed:     read.Check != fake.deny,
		}
	}
	return facts, nil
}

type adminPrincipals struct {
	mu        sync.Mutex
	principal identity.Principal
	err       error
	calls     int
}

func (fake *adminPrincipals) ReadPrincipal(_ context.Context, _ identity.PrincipalRequest) (identity.Principal, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	return fake.principal, fake.err
}

type adminServices struct {
	mu       sync.Mutex
	current  service.Service
	err      error
	calls    int
	requests []service.ServiceReadRequest
}

func (fake *adminServices) ReadService(_ context.Context, request service.ServiceReadRequest) (service.Service, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	fake.requests = append(fake.requests, request)
	return fake.current, fake.err
}

type adminPort struct {
	mu          sync.Mutex
	create      func(context.Context, service.CreateUnit) (service.CreateResult, error)
	update      func(context.Context, service.UpdateUnit) (service.UpdateResult, error)
	setStatus   func(context.Context, service.StatusUnit) (service.StatusResult, error)
	createCalls int
	updateCalls int
	statusCalls int
}

func (port *adminPort) Create(ctx context.Context, unit service.CreateUnit) (service.CreateResult, error) {
	port.mu.Lock()
	port.createCalls++
	callback := port.create
	port.mu.Unlock()
	if callback == nil {
		return service.CreateResult{}, fault.New(fault.InternalFailure, "unexpected Create call")
	}
	return callback(ctx, unit)
}

func (port *adminPort) Update(ctx context.Context, unit service.UpdateUnit) (service.UpdateResult, error) {
	port.mu.Lock()
	port.updateCalls++
	callback := port.update
	port.mu.Unlock()
	if callback == nil {
		return service.UpdateResult{}, fault.New(fault.InternalFailure, "unexpected Update call")
	}
	return callback(ctx, unit)
}

func (port *adminPort) SetStatus(ctx context.Context, unit service.StatusUnit) (service.StatusResult, error) {
	port.mu.Lock()
	port.statusCalls++
	callback := port.setStatus
	port.mu.Unlock()
	if callback == nil {
		return service.StatusResult{}, fault.New(fault.InternalFailure, "unexpected SetStatus call")
	}
	return callback(ctx, unit)
}

type adminInvalidator struct {
	mu            sync.Mutex
	err           error
	hook          func()
	calls         int
	invalidations []service.ServiceInvalidation
}

func (fake *adminInvalidator) InvalidateService(_ context.Context, invalidation service.ServiceInvalidation) error {
	fake.mu.Lock()
	fake.calls++
	fake.invalidations = append(fake.invalidations, invalidation)
	hook := fake.hook
	err := fake.err
	fake.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

type adminFixture struct {
	ports       *adminPort
	authorizer  *adminAuthorizer
	authority   *adminAuthority
	principals  *adminPrincipals
	services    *adminServices
	invalidator *adminInvalidator
	admin       *service.Admin
}

func newAdminFixture(t *testing.T) *adminFixture {
	t.Helper()
	fixture := &adminFixture{
		ports:       &adminPort{},
		authorizer:  &adminAuthorizer{},
		authority:   &adminAuthority{},
		principals:  &adminPrincipals{principal: adminPrincipal(testScope(), tenancy.RoleAdmin)},
		services:    &adminServices{current: adminService(t, testScope(), service.StateEnabled, 1)},
		invalidator: &adminInvalidator{},
	}
	fixture.ports.create = func(_ context.Context, unit service.CreateUnit) (service.CreateResult, error) {
		return service.CreateResult{ServiceID: unit.Service.ID, Version: unit.Service.Version}, nil
	}
	fixture.ports.update = func(_ context.Context, unit service.UpdateUnit) (service.UpdateResult, error) {
		return service.UpdateResult{Version: unit.Service.Version}, nil
	}
	fixture.ports.setStatus = func(_ context.Context, unit service.StatusUnit) (service.StatusResult, error) {
		return service.StatusResult{State: unit.State, Version: unit.ExpectedVersion + 1}, nil
	}
	admin, err := service.NewAdmin(service.AdminConfig{
		Authorizer:    fixture.authorizer,
		Authority:     fixture.authority,
		Principals:    fixture.principals,
		Services:      fixture.services,
		Port:          fixture.ports,
		Invalidations: fixture.invalidator,
		Clock:         adminClock{now: adminNow},
		IDs:           &adminIDs{},
	})
	if err != nil {
		t.Fatalf("NewAdmin: %v", err)
	}
	fixture.admin = admin
	return fixture
}

func adminPrincipal(scope tenancy.Scope, roles ...tenancy.Role) authorize.Principal {
	return authorize.Principal{
		ID:      id.PrincipalID("principal-admin"),
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   roles,
		Enabled: true,
		Version: 1,
	}
}

func adminService(t *testing.T, scope tenancy.Scope, state service.State, version uint64) service.Service {
	t.Helper()
	created, err := service.NewService(service.CreateRequest{
		ID:    service.ServiceID("svc-1"),
		Scope: scope,
		State: state,
		Roles: []tenancy.Role{tenancy.RoleViewer},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	created.Version = version
	if state == service.StateDisabled {
		created.TokenNonce = nil
		created.TokenExpiry = nil
	}
	return created
}

func withCredential(t *testing.T, value service.Service) service.Service {
	t.Helper()
	value.TokenNonce = testNonce()
	expiry := testExpiry()
	value.TokenExpiry = &expiry
	if err := value.Validate(); err != nil {
		t.Fatalf("credential service is invalid: %v", err)
	}
	return value
}

func adminContent(body string) receipt.Content {
	return receipt.Content{MediaType: "application/json", Bytes: []byte(body)}
}

func createCommand() service.CreateCommand {
	return service.CreateCommand{
		Actor:     adminPrincipal(testScope(), tenancy.RoleAdmin),
		ID:        service.ServiceID("svc-1"),
		Scope:     testScope(),
		State:     service.StateEnabled,
		Roles:     []tenancy.Role{tenancy.RoleEngineer},
		CallerKey: "caller-1",
		Content:   adminContent(`{"roles":["engineer"]}`),
	}
}

func updateCommand() service.UpdateCommand {
	return service.UpdateCommand{
		Actor:           adminPrincipal(testScope(), tenancy.RoleAdmin),
		Scope:           testScope(),
		ServiceID:       service.ServiceID("svc-1"),
		ExpectedVersion: 1,
		Roles:           []tenancy.Role{tenancy.RoleEngineer},
		CallerKey:       "caller-1",
		Content:         adminContent(`{"roles":["engineer"]}`),
	}
}

func statusCommand() service.StatusCommand {
	return service.StatusCommand{
		Actor:           adminPrincipal(testScope(), tenancy.RoleAdmin),
		Scope:           testScope(),
		ServiceID:       service.ServiceID("svc-1"),
		ExpectedVersion: 1,
		CallerKey:       "caller-1",
		Content:         adminContent(`{}`),
	}
}

func TestAdminCreateCommitsCompleteUnit(t *testing.T) {
	fixture := newAdminFixture(t)
	var captured service.CreateUnit
	fixture.ports.create = func(_ context.Context, unit service.CreateUnit) (service.CreateResult, error) {
		captured = unit
		return service.CreateResult{ServiceID: unit.Service.ID, Version: unit.Service.Version}, nil
	}

	result, err := fixture.admin.Create(context.Background(), createCommand())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if result.ServiceID != service.ServiceID("svc-1") || result.Version != 1 {
		t.Fatalf("result = %+v; want svc-1 version 1", result)
	}
	if captured.Owner != testScope() || captured.Actor != id.PrincipalID("principal-admin") {
		t.Fatalf("unit owner/actor = %+v/%q", captured.Owner, captured.Actor)
	}
	if captured.Service.Version != 1 || len(captured.Service.TokenNonce) != 0 || captured.Service.TokenExpiry != nil {
		t.Fatalf("created service = %+v; want version 1 with no credential", captured.Service)
	}
	if captured.Receipt.Key.Operation != "service.create" || captured.Receipt.Key.CallerKey != "caller-1" {
		t.Fatalf("receipt key = %+v", captured.Receipt.Key)
	}
	if captured.Receipt.ContentHash == "" || captured.Receipt.At != adminNow {
		t.Fatalf("receipt content hash/time = %q/%v", captured.Receipt.ContentHash, captured.Receipt.At)
	}
	if len(captured.Audit) != 1 || captured.Audit[0].Operation != "service.create" || captured.Audit[0].Detail != "" {
		t.Fatalf("audit = %+v; want one secret-free service.create row", captured.Audit)
	}
	if len(captured.Events) != 1 {
		t.Fatalf("events = %+v; want one service.changed envelope", captured.Events)
	}
	changed := captured.Events[0]
	if changed.Type != event.TypeServiceChanged || changed.SubjectID != "svc-1" || changed.SubjectVersion != 1 {
		t.Fatalf("event = %+v; want service.changed svc-1 v1", changed)
	}
	if len(changed.Data) != 0 {
		t.Fatalf("event carries data %q", changed.Data)
	}
	read, ok := adminRead(captured.Authority, "service.create", authorize.CheckMembership)
	if !ok {
		t.Fatalf("authority reads = %+v; want membership", captured.Authority)
	}
	if len(read.Required.HumanRoles) != 1 || read.Required.HumanRoles[0] != tenancy.RoleAdmin {
		t.Fatalf("authority grant = %+v; want Admin", read.Required)
	}
	if len(captured.PostCommit) != 1 || captured.PostCommit[0].Kind != event.PostPublishEvent {
		t.Fatalf("post-commit = %+v; want publish_event", captured.PostCommit)
	}
}

func adminRead(reads []authorize.AuthorityRead, operation authorize.OperationName, check authorize.AuthorityCheck) (authorize.AuthorityRead, bool) {
	for _, read := range reads {
		if read.Operation == operation && read.Check == check {
			return read, true
		}
	}
	return authorize.AuthorityRead{}, false
}

func TestAdminRefusesWithoutCommit(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*adminFixture)
		call   func(*adminFixture) error
		want   fault.Class
	}{
		{
			name: "authorizer denies",
			mutate: func(fixture *adminFixture) {
				fixture.authorizer.decision = authorize.DecisionDeny
			},
			want: fault.Forbidden,
		},
		{
			name: "authority denies membership",
			mutate: func(fixture *adminFixture) {
				fixture.authority.deny = authorize.CheckMembership
			},
			want: fault.Forbidden,
		},
		{
			name: "service principal",
			mutate: func(fixture *adminFixture) {
			},
			call: func(fixture *adminFixture) error {
				command := createCommand()
				command.Actor = authorize.Principal{
					ID: id.PrincipalID("principal-svc"), Kind: identity.KindService,
					Scope: testScope(), Roles: []tenancy.Role{tenancy.RoleEngineer}, Enabled: true, Version: 1,
				}
				_, err := fixture.admin.Create(context.Background(), command)
				return err
			},
			want: fault.Forbidden,
		},
		{
			name: "execution principal",
			mutate: func(fixture *adminFixture) {
			},
			call: func(fixture *adminFixture) error {
				command := createCommand()
				command.Actor = authorize.Principal{
					ID: id.PrincipalID("principal-exec"), Kind: identity.KindExecution,
					Scope: testScope(), ExecutionID: id.ExecutionID("exec-1"), Enabled: true, Version: 1,
				}
				_, err := fixture.admin.Create(context.Background(), command)
				return err
			},
			want: fault.Forbidden,
		},
		{
			name: "human without admin",
			mutate: func(fixture *adminFixture) {
				fixture.principals.principal = adminPrincipal(testScope(), tenancy.RoleEngineer)
			},
			want: fault.Forbidden,
		},
		{
			name: "disabled principal",
			mutate: func(fixture *adminFixture) {
				principal := adminPrincipal(testScope(), tenancy.RoleAdmin)
				principal.Enabled = false
				fixture.principals.principal = principal
			},
			want: fault.Unauthenticated,
		},
		{
			name: "cross-scope actor",
			mutate: func(fixture *adminFixture) {
				other := tenancy.Scope{TenantID: id.TenantID("tenant-b"), ProjectID: id.ProjectID("project-2")}
				fixture.principals.principal = adminPrincipal(other, tenancy.RoleAdmin)
			},
			want: fault.Forbidden,
		},
		{
			name: "tenant-only command scope",
			mutate: func(fixture *adminFixture) {
			},
			want: fault.InvalidInput,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newAdminFixture(t)
			testCase.mutate(fixture)
			var err error
			if testCase.call != nil {
				err = testCase.call(fixture)
			} else {
				command := createCommand()
				if testCase.name == "cross-scope actor" {
					command.Actor.Scope = tenancy.Scope{TenantID: id.TenantID("tenant-b"), ProjectID: id.ProjectID("project-2")}
				}
				if testCase.name == "tenant-only command scope" {
					command.Scope = tenancy.Scope{TenantID: id.TenantID("tenant-a")}
					command.Actor.Scope = command.Scope
				}
				_, err = fixture.admin.Create(context.Background(), command)
			}
			requireClass(t, err, testCase.want)
			if fixture.ports.createCalls != 0 {
				t.Fatalf("Create port calls = %d; want 0", fixture.ports.createCalls)
			}
		})
	}
}

func TestAdminCreateRefusesAdminRole(t *testing.T) {
	fixture := newAdminFixture(t)
	command := createCommand()
	command.Roles = []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleAdmin}
	_, err := fixture.admin.Create(context.Background(), command)
	requireClass(t, err, fault.InvalidInput)
	if fixture.ports.createCalls != 0 {
		t.Fatalf("Create port calls = %d; want 0", fixture.ports.createCalls)
	}
}

type replayCreate struct {
	commits     int
	replays     int
	key         receipt.Key
	contentHash string
	stored      service.CreateResult
}

func (fake *replayCreate) create(_ context.Context, unit service.CreateUnit) (service.CreateResult, error) {
	if fake.commits == 0 {
		fake.commits++
		fake.key = unit.Receipt.Key
		fake.contentHash = unit.Receipt.ContentHash
		fake.stored = service.CreateResult{ServiceID: unit.Service.ID, Version: unit.Service.Version}
		return fake.stored, nil
	}
	if unit.Receipt.Key != fake.key || unit.Receipt.ContentHash != fake.contentHash {
		return service.CreateResult{}, fault.New(fault.ChangedIdempotencyContent, "changed idempotency content")
	}
	fake.replays++
	result := fake.stored
	result.Replayed = true
	return result, nil
}

func TestAdminCreateReplayAndChangedContent(t *testing.T) {
	fixture := newAdminFixture(t)
	replay := &replayCreate{}
	fixture.ports.create = replay.create
	command := createCommand()

	first, err := fixture.admin.Create(context.Background(), command)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if first.Replayed {
		t.Fatal("first commit reported a replay")
	}
	second, err := fixture.admin.Create(context.Background(), command)
	if err != nil {
		t.Fatalf("replayed Create: %v", err)
	}
	if !second.Replayed || second.ServiceID != first.ServiceID || second.Version != first.Version {
		t.Fatalf("replay result = %+v; want original %+v with Replayed", second, first)
	}
	if replay.commits != 1 || replay.replays != 1 {
		t.Fatalf("commits/replays = %d/%d; want 1/1", replay.commits, replay.replays)
	}

	changed := command
	changed.Content = adminContent(`{"roles":["viewer"]}`)
	_, err = fixture.admin.Create(context.Background(), changed)
	requireClass(t, err, fault.ChangedIdempotencyContent)
}

func TestAdminUpdateCannotChangeEnablementOrCarryCredential(t *testing.T) {
	fixture := newAdminFixture(t)
	fixture.services.current = withCredential(t, adminService(t, testScope(), service.StateEnabled, 3))
	var captured service.UpdateUnit
	fixture.ports.update = func(_ context.Context, unit service.UpdateUnit) (service.UpdateResult, error) {
		captured = unit
		return service.UpdateResult{Version: unit.Service.Version}, nil
	}

	command := updateCommand()
	command.ExpectedVersion = 3
	result, err := fixture.admin.Update(context.Background(), command)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if result.Version != 4 {
		t.Fatalf("result version = %d; want 4", result.Version)
	}
	if captured.ExpectedState != service.StateEnabled || captured.Service.State != service.StateEnabled {
		t.Fatalf("expected/next state = %q/%q; update must not change enablement", captured.ExpectedState, captured.Service.State)
	}
	if len(captured.Service.TokenNonce) != 0 || captured.Service.TokenExpiry != nil {
		t.Fatalf("update unit carries a credential pair: %+v", captured.Service)
	}
	if captured.Service.Version != 4 || captured.ServiceID != service.ServiceID("svc-1") {
		t.Fatalf("next service = %+v; want svc-1 v4", captured.Service)
	}
	if len(captured.Service.Roles) != 1 || captured.Service.Roles[0] != tenancy.RoleEngineer {
		t.Fatalf("roles = %v; want [engineer]", captured.Service.Roles)
	}
	if _, ok := adminRead(captured.Authority, "service.update", authorize.CheckServiceEnabled); !ok {
		t.Fatalf("authority reads = %+v; want service_enabled", captured.Authority)
	}
	if len(captured.Events) != 1 || captured.Events[0].SubjectVersion != 4 || captured.Events[0].Type != event.TypeServiceChanged {
		t.Fatalf("event = %+v; want service.changed v4", captured.Events)
	}
}

func TestAdminUpdateRefusesAdminRoleAndMissingRoles(t *testing.T) {
	for _, roles := range [][]tenancy.Role{{tenancy.RoleAdmin}, nil} {
		fixture := newAdminFixture(t)
		command := updateCommand()
		command.Roles = roles
		_, err := fixture.admin.Update(context.Background(), command)
		requireClass(t, err, fault.InvalidInput)
		if fixture.ports.updateCalls != 0 {
			t.Fatalf("Update port calls = %d; want 0", fixture.ports.updateCalls)
		}
	}
}

func TestAdminUpdateStaleVersionPropagatesConflict(t *testing.T) {
	fixture := newAdminFixture(t)
	fixture.ports.update = func(_ context.Context, unit service.UpdateUnit) (service.UpdateResult, error) {
		if unit.ExpectedVersion != fixture.services.current.Version {
			return service.UpdateResult{}, fault.New(fault.VersionConflict, "stale service version")
		}
		return service.UpdateResult{Version: unit.Service.Version}, nil
	}
	command := updateCommand()
	command.ExpectedVersion = 9
	_, err := fixture.admin.Update(context.Background(), command)
	requireClass(t, err, fault.VersionConflict)
}

func TestAdminEnableNeverCarriesRevokeAll(t *testing.T) {
	fixture := newAdminFixture(t)
	fixture.services.current = adminService(t, testScope(), service.StateDisabled, 2)
	var captured service.StatusUnit
	fixture.ports.setStatus = func(_ context.Context, unit service.StatusUnit) (service.StatusResult, error) {
		captured = unit
		return service.StatusResult{State: unit.State, Version: unit.ExpectedVersion + 1}, nil
	}

	command := statusCommand()
	command.ExpectedVersion = 2
	result, err := fixture.admin.Enable(context.Background(), command)
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if result.State != service.StateEnabled || result.Version != 3 {
		t.Fatalf("result = %+v; want enabled v3", result)
	}
	if captured.State != service.StateEnabled || captured.RevokeAll != nil {
		t.Fatalf("enable unit = %+v; enablement must not carry a revocation", captured)
	}
	if fixture.invalidator.calls != 0 {
		t.Fatalf("invalidator calls = %d; enablement invalidates nothing", fixture.invalidator.calls)
	}
}

func TestAdminDisableCarriesRevokeAllAndInvalidates(t *testing.T) {
	fixture := newAdminFixture(t)
	fixture.services.current = withCredential(t, adminService(t, testScope(), service.StateEnabled, 2))
	var order []string
	var captured service.StatusUnit
	fixture.ports.setStatus = func(_ context.Context, unit service.StatusUnit) (service.StatusResult, error) {
		order = append(order, "commit")
		captured = unit
		return service.StatusResult{State: unit.State, Version: unit.ExpectedVersion + 1, RevokedCredentials: 1}, nil
	}
	fixture.invalidator.hook = func() { order = append(order, "invalidate") }

	command := statusCommand()
	command.ExpectedVersion = 2
	result, err := fixture.admin.Disable(context.Background(), command)
	if err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if result.State != service.StateDisabled || result.Version != 3 || result.RevokedCredentials != 1 {
		t.Fatalf("result = %+v; want disabled v3 revoking one credential", result)
	}
	if captured.State != service.StateDisabled || captured.RevokeAll == nil {
		t.Fatalf("disable unit = %+v; want the RevokeAll obligation", captured)
	}
	if len(captured.Events) != 1 || captured.Events[0].Type != event.TypeServiceChanged || captured.Events[0].SubjectVersion != 3 {
		t.Fatalf("event = %+v; want service.changed v3", captured.Events)
	}
	if len(captured.PostCommit) != 1 || captured.PostCommit[0].Kind != event.PostPublishEvent {
		t.Fatalf("post-commit = %+v; want publish_event", captured.PostCommit)
	}
	if fixture.invalidator.calls != 1 {
		t.Fatalf("invalidator calls = %d; want 1", fixture.invalidator.calls)
	}
	invalidation := fixture.invalidator.invalidations[0]
	if invalidation.Scope != testScope() || invalidation.ServiceID != service.ServiceID("svc-1") || invalidation.At != adminNow {
		t.Fatalf("invalidation = %+v", invalidation)
	}
	if len(order) != 2 || order[0] != "commit" || order[1] != "invalidate" {
		t.Fatalf("commit order = %v; want commit before invalidation", order)
	}
}

func TestAdminDisablePortFailureDoesNotInvalidate(t *testing.T) {
	fixture := newAdminFixture(t)
	fixture.services.current = adminService(t, testScope(), service.StateEnabled, 2)
	fixture.ports.setStatus = func(context.Context, service.StatusUnit) (service.StatusResult, error) {
		return service.StatusResult{}, fault.New(fault.DependencyUnavailable, "store unavailable")
	}
	command := statusCommand()
	command.ExpectedVersion = 2
	result, err := fixture.admin.Disable(context.Background(), command)
	requireClass(t, err, fault.DependencyUnavailable)
	if result != (service.StatusResult{}) {
		t.Fatalf("result = %+v; want zero on refusal", result)
	}
	if fixture.invalidator.calls != 0 {
		t.Fatalf("invalidator calls = %d; a failed commit must not invalidate", fixture.invalidator.calls)
	}
}

type versionedStatusPort struct {
	mu      sync.Mutex
	version uint64
	state   service.State
	commits int
}

func (port *versionedStatusPort) setStatus(_ context.Context, unit service.StatusUnit) (service.StatusResult, error) {
	port.mu.Lock()
	defer port.mu.Unlock()
	if unit.ExpectedVersion != port.version {
		return service.StatusResult{}, fault.New(fault.VersionConflict, "stale service version")
	}
	port.version++
	port.state = unit.State
	port.commits++
	return service.StatusResult{State: unit.State, Version: port.version}, nil
}

func TestAdminConcurrentDisablesOneWinner(t *testing.T) {
	fixture := newAdminFixture(t)
	fixture.services.current = adminService(t, testScope(), service.StateEnabled, 1)
	port := &versionedStatusPort{version: 1, state: service.StateEnabled}
	fixture.ports.setStatus = port.setStatus

	var wait sync.WaitGroup
	errors := make([]error, 2)
	for index := range 2 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, errors[index] = fixture.admin.Disable(context.Background(), statusCommand())
		}(index)
	}
	wait.Wait()

	winners, conflicts := 0, 0
	for _, err := range errors {
		if err == nil {
			winners++
			continue
		}
		class, ok := fault.ClassOf(err)
		if !ok || class != fault.VersionConflict {
			t.Fatalf("loser error = %v; want version_conflict", err)
		}
		conflicts++
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("winners/conflicts = %d/%d; want 1/1", winners, conflicts)
	}
	if port.commits != 1 {
		t.Fatalf("commits = %d; want 1", port.commits)
	}
	if fixture.invalidator.calls != 1 {
		t.Fatalf("invalidator calls = %d; want 1", fixture.invalidator.calls)
	}
}

func TestAdminDisableAfterEnableDoesNotRestoreCredential(t *testing.T) {
	fixture := newAdminFixture(t)
	current := withCredential(t, adminService(t, testScope(), service.StateEnabled, 2))
	fixture.services.current = current
	port := &credentialClearingStatusPort{current: current}
	fixture.ports.setStatus = port.setStatus

	disable := statusCommand()
	disable.ExpectedVersion = 2
	if _, err := fixture.admin.Disable(context.Background(), disable); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	enable := statusCommand()
	enable.ExpectedVersion = 3
	if _, err := fixture.admin.Enable(context.Background(), enable); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if port.current.State != service.StateEnabled || port.current.Version != 4 {
		t.Fatalf("service after enable = %+v; want enabled v4", port.current)
	}
	if len(port.current.TokenNonce) != 0 || port.current.TokenExpiry != nil {
		t.Fatalf("enablement resurrected a credential: %+v", port.current)
	}
}

type credentialClearingStatusPort struct {
	mu      sync.Mutex
	current service.Service
}

func (port *credentialClearingStatusPort) setStatus(_ context.Context, unit service.StatusUnit) (service.StatusResult, error) {
	port.mu.Lock()
	defer port.mu.Unlock()
	if unit.ExpectedVersion != port.current.Version {
		return service.StatusResult{}, fault.New(fault.VersionConflict, "stale service version")
	}
	next := port.current
	next.Version++
	next.State = unit.State
	if unit.State == service.StateDisabled {
		next.TokenNonce = nil
		next.TokenExpiry = nil
	}
	port.current = next
	return service.StatusResult{State: next.State, Version: next.Version}, nil
}

func TestAdminDisableInvalidationFailureReportsUncertain(t *testing.T) {
	fixture := newAdminFixture(t)
	fixture.services.current = adminService(t, testScope(), service.StateEnabled, 2)
	fixture.invalidator.err = fault.New(fault.DependencyUnavailable, "broker unavailable")
	command := statusCommand()
	command.ExpectedVersion = 2
	result, err := fixture.admin.Disable(context.Background(), command)
	requireClass(t, err, fault.UncertainExternalEffect)
	if result.State != service.StateDisabled || result.Version != 3 {
		t.Fatalf("result = %+v; want the committed disablement reported with the failed effect", result)
	}
	if fixture.ports.statusCalls != 1 || fixture.invalidator.calls != 1 {
		t.Fatalf("status/invalidation calls = %d/%d; want 1/1", fixture.ports.statusCalls, fixture.invalidator.calls)
	}
}

func TestAdminErrorsCarryNoSecret(t *testing.T) {
	fixture := newAdminFixture(t)
	fixture.services.current = withCredential(t, adminService(t, testScope(), service.StateEnabled, 2))
	fixture.ports.setStatus = func(context.Context, service.StatusUnit) (service.StatusResult, error) {
		return service.StatusResult{}, fault.New(fault.DependencyUnavailable, "store unavailable")
	}
	command := statusCommand()
	command.ExpectedVersion = 2
	_, err := fixture.admin.Disable(context.Background(), command)
	if err == nil {
		t.Fatal("Disable unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), "0123456789abcdef") {
		t.Fatalf("error leaked credential material: %v", err)
	}
}
