package domaincontract

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestServiceAdministrationCommitUnits pins the service administration seam.
// Create/update/status are complete commit units with an owner scope, acting
// principal, expected version, receipt/audit/event intent, current-authority
// reads and post-commit effects. Disablement must carry the RevokeAll
// credential-clearing obligation; the expected service version is the only
// stale-read guard and the obligation carries no observed credential value. An
// enablement must not carry a revocation or invent a credential.
func TestServiceAdministrationCommitUnits(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	scope := sampleScope()
	receiptRecord := sampleReceipt()
	audit := sampleAudit(now)
	events := sampleEvents(now)
	authority := sampleAuthority()
	postCommit := samplePostCommit()

	created := service.Service{
		ID:      service.ServiceID("service-1"),
		Scope:   scope,
		State:   service.StateEnabled,
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Version: 1,
	}
	createUnit := service.CreateUnit{
		Owner:      scope,
		Service:    created,
		Actor:      id.PrincipalID("human-1"),
		At:         now,
		Receipt:    receiptRecord,
		Audit:      audit,
		Events:     events,
		Authority:  authority,
		PostCommit: postCommit,
	}
	if err := createUnit.Validate(); err != nil {
		t.Fatalf("valid service create unit refused: %v", err)
	}
	if createUnit.Service.Version != 1 || createUnit.Owner != scope {
		t.Fatalf("service create unit lost its version or scope: %+v", createUnit)
	}
	assertInvalidInput(t, "service create without a service", service.CreateUnit{
		Owner: scope, Actor: id.PrincipalID("human-1"), At: now,
	}.Validate())
	assertInvalidInput(t, "service create with a non-initial version", func() error {
		unit := createUnit
		unit.Service.Version = 2
		return unit.Validate()
	}())
	assertInvalidInput(t, "service create with a mismatched scope", func() error {
		unit := createUnit
		unit.Service.Scope = tenancy.Scope{TenantID: id.TenantID("tenant-2"), ProjectID: id.ProjectID("project-2")}
		return unit.Validate()
	}())
	assertInvalidInput(t, "service create carrying a credential pair", func() error {
		unit := createUnit
		expiry := now.Add(time.Hour)
		unit.Service.TokenNonce = make([]byte, service.MinTokenNonceBytes)
		unit.Service.TokenExpiry = &expiry
		return unit.Validate()
	}())
	assertInvalidInput(t, "service create without an actor", func() error {
		unit := createUnit
		unit.Actor = ""
		return unit.Validate()
	}())
	assertInvalidInput(t, "service create without a commit time", func() error {
		unit := createUnit
		unit.At = time.Time{}
		return unit.Validate()
	}())

	updated := created
	updated.Roles = []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleViewer}
	updated.Version = 2
	updateUnit := service.UpdateUnit{
		Owner:           scope,
		ServiceID:       updated.ID,
		ExpectedVersion: 1,
		ExpectedState:   service.StateEnabled,
		Service:         updated,
		Actor:           id.PrincipalID("human-1"),
		At:              now,
		Receipt:         receiptRecord,
		Audit:           audit,
		Events:          events,
		Authority:       authority,
		PostCommit:      postCommit,
	}
	if err := updateUnit.Validate(); err != nil {
		t.Fatalf("valid service update unit refused: %v", err)
	}
	assertInvalidInput(t, "service update with a stale version", func() error {
		unit := updateUnit
		unit.ExpectedVersion = 2
		return unit.Validate()
	}())
	assertInvalidInput(t, "service update with a mismatched service id", func() error {
		unit := updateUnit
		unit.ServiceID = service.ServiceID("service-2")
		return unit.Validate()
	}())
	assertInvalidInput(t, "service update that changes enablement", func() error {
		unit := updateUnit
		unit.Service.State = service.StateDisabled
		return unit.Validate()
	}())
	assertInvalidInput(t, "service update carrying a credential pair", func() error {
		unit := updateUnit
		expiry := now.Add(time.Hour)
		unit.Service.TokenNonce = make([]byte, service.MinTokenNonceBytes)
		unit.Service.TokenExpiry = &expiry
		return unit.Validate()
	}())

	enableUnit := service.StatusUnit{
		Owner:           scope,
		ServiceID:       created.ID,
		ExpectedVersion: 2,
		State:           service.StateEnabled,
		Actor:           id.PrincipalID("human-1"),
		At:              now,
		Receipt:         receiptRecord,
		Audit:           audit,
		Events:          events,
		Authority:       authority,
		PostCommit:      postCommit,
	}
	if err := enableUnit.Validate(); err != nil {
		t.Fatalf("valid service enable unit refused: %v", err)
	}
	disableUnit := enableUnit
	disableUnit.State = service.StateDisabled
	disableUnit.RevokeAll = &service.RevokeAll{}
	if err := disableUnit.Validate(); err != nil {
		t.Fatalf("valid service disable unit refused: %v", err)
	}
	if disableUnit.RevokeAll == nil {
		t.Fatalf("service disable unit lost its revocation obligation: %+v", disableUnit)
	}
	// A disablement is valid for a service that has never held a credential:
	// RevokeAll is unconditional and the service row's expected version is the
	// only guard, so there is no credential generation to fabricate.
	if len(created.TokenNonce) != 0 || created.TokenExpiry != nil {
		t.Fatalf("fixture service unexpectedly holds a credential: %+v", created)
	}
	neverCredentialed := service.StatusUnit{
		Owner:           scope,
		ServiceID:       created.ID,
		ExpectedVersion: 1,
		State:           service.StateDisabled,
		RevokeAll:       &service.RevokeAll{},
		Actor:           id.PrincipalID("human-1"),
		At:              now,
		Receipt:         receiptRecord,
		Audit:           audit,
		Events:          events,
		Authority:       authority,
		PostCommit:      postCommit,
	}
	if err := neverCredentialed.Validate(); err != nil {
		t.Fatalf("disable unit for a never-credentialed service refused: %v", err)
	}
	assertInvalidInput(t, "service disable without a revocation obligation", func() error {
		unit := disableUnit
		unit.RevokeAll = nil
		return unit.Validate()
	}())
	assertInvalidInput(t, "service enable carrying a revocation obligation", func() error {
		unit := enableUnit
		unit.RevokeAll = &service.RevokeAll{}
		return unit.Validate()
	}())
	assertInvalidInput(t, "service status with a zero expected version", func() error {
		unit := enableUnit
		unit.ExpectedVersion = 0
		return unit.Validate()
	}())
	assertInvalidInput(t, "service status with an unknown state", func() error {
		unit := enableUnit
		unit.State = service.State("suspended")
		return unit.Validate()
	}())

	units := []struct {
		name   string
		value  any
		fields []string
	}{
		{
			name:  "service.CreateUnit",
			value: createUnit,
			fields: []string{
				"Owner", "Service", "Actor", "At", "Receipt", "Audit", "Events", "Authority", "PostCommit",
			},
		},
		{
			name:  "service.UpdateUnit",
			value: updateUnit,
			fields: []string{
				"Owner", "ServiceID", "ExpectedVersion", "ExpectedState", "Service", "Actor", "At",
				"Receipt", "Audit", "Events", "Authority", "PostCommit",
			},
		},
		{
			name:  "service.StatusUnit",
			value: disableUnit,
			fields: []string{
				"Owner", "ServiceID", "ExpectedVersion", "State", "RevokeAll", "Actor", "At",
				"Receipt", "Audit", "Events", "Authority", "PostCommit",
			},
		},
	}
	for _, unit := range units {
		typ := reflect.TypeOf(unit.value)
		for _, field := range unit.fields {
			if _, ok := typ.FieldByName(field); !ok {
				t.Errorf("%s is missing required field %q", unit.name, field)
			}
		}
	}

	revokeType := reflect.TypeOf(service.RevokeAll{})
	if revokeType.NumField() != 0 {
		t.Errorf("service.RevokeAll has %d fields; want the unconditional obligation with no observed credential value", revokeType.NumField())
	}
	statusType := reflect.TypeOf(disableUnit)
	revokeField, ok := statusType.FieldByName("RevokeAll")
	if !ok || revokeField.Type != reflect.TypeOf((*service.RevokeAll)(nil)) {
		t.Fatalf("service.StatusUnit.RevokeAll type = %v; want *service.RevokeAll", revokeField.Type)
	}

	var _ service.AdminPort = fakeServiceAdmin{}
	adminPort := reflect.TypeOf((*service.AdminPort)(nil)).Elem()
	wantMethods := map[string]bool{"Create": true, "Update": true, "SetStatus": true}
	if adminPort.NumMethod() != len(wantMethods) {
		t.Fatalf("service.AdminPort has %d methods; want %d", adminPort.NumMethod(), len(wantMethods))
	}
	contextType := reflect.TypeOf((*context.Context)(nil)).Elem()
	for index := 0; index < adminPort.NumMethod(); index++ {
		method := adminPort.Method(index)
		if !wantMethods[method.Name] {
			t.Errorf("service.AdminPort has unexpected method %q", method.Name)
		}
		if method.Type.NumIn() == 0 || method.Type.In(0) != contextType {
			t.Errorf("service.AdminPort.%s must take context.Context first", method.Name)
		}
	}

	createResult := service.CreateResult{ServiceID: created.ID, Version: 1, Replayed: false}
	updateResult := service.UpdateResult{Version: 2, Replayed: false}
	statusResult := service.StatusResult{State: service.StateDisabled, Version: 3, RevokedCredentials: 1, Replayed: false}
	if createResult.ServiceID != created.ID || updateResult.Version != 2 || statusResult.RevokedCredentials != 1 {
		t.Fatalf("service administration results lost their committed facts: %+v / %+v / %+v", createResult, updateResult, statusResult)
	}
}

// TestEventReadContract pins the event.read query/page shape: a scope, an
// exclusive sequence cursor, an optional type filter, a bounded limit and a
// stable next cursor with a committed-only page.
func TestEventReadContract(t *testing.T) {
	if event.MaxReadLimit != 200 {
		t.Fatalf("event.MaxReadLimit = %d; want 200", event.MaxReadLimit)
	}
	query := event.ReadQuery{
		Scope:         sampleScope(),
		AfterSequence: 10,
		Types:         []event.Type{event.TypeTaskCreated, event.TypeExecutionQueued},
		Limit:         event.MaxReadLimit,
	}
	if err := query.Validate(); err != nil {
		t.Fatalf("valid event read query refused: %v", err)
	}
	assertInvalidInput(t, "event read with a zero limit", func() error {
		q := query
		q.Limit = 0
		return q.Validate()
	}())
	assertInvalidInput(t, "event read with an oversized limit", func() error {
		q := query
		q.Limit = event.MaxReadLimit + 1
		return q.Validate()
	}())
	assertInvalidInput(t, "event read with an unknown type filter", func() error {
		q := query
		q.Types = []event.Type{event.Type("bogus.event")}
		return q.Validate()
	}())
	assertInvalidInput(t, "event read with an empty scope", func() error {
		q := query
		q.Scope = tenancy.Scope{}
		return q.Validate()
	}())

	page := event.ReadPage{Events: sampleEvents(time.Unix(1_700_000_000, 0).UTC()), Next: 11, HasMore: true}
	if page.Next != 11 || !page.HasMore || len(page.Events) != 1 {
		t.Fatalf("event read page lost its cursor or committed events: %+v", page)
	}
	if page.Events[0].Scope != query.Scope {
		t.Fatalf("event read page returned a foreign scope: %+v", page.Events[0])
	}

	var _ event.ReadPort = fakeEventRead{}
	readPort := reflect.TypeOf((*event.ReadPort)(nil)).Elem()
	if readPort.NumMethod() != 1 || readPort.Method(0).Name != "Read" {
		t.Fatalf("event.ReadPort must declare exactly Read; got %v", readPort)
	}
	if readPort.Method(0).Type.NumIn() == 0 || readPort.Method(0).Type.In(0) != reflect.TypeOf((*context.Context)(nil)).Elem() {
		t.Fatal("event.ReadPort.Read must take context.Context first")
	}
}

// TestEventSubscribeContract pins the event.subscribe setup/grant shape: the
// grant names only scope-qualified event subjects (never a wildcard or a
// foreign/mixed scope), a resume cursor and the credential expiry; it carries
// no credential or token material.
func TestEventSubscribeContract(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	scope := sampleScope()

	setup := event.SubscribeSetup{
		Scope:  scope,
		Types:  []event.Type{event.TypeTaskCreated, event.TypeTaskReply},
		Cursor: 5,
	}
	if err := setup.Validate(); err != nil {
		t.Fatalf("valid event subscribe setup refused: %v", err)
	}
	assertInvalidInput(t, "event subscribe setup with an empty scope", func() error {
		s := setup
		s.Scope = tenancy.Scope{}
		return s.Validate()
	}())
	assertInvalidInput(t, "event subscribe setup with an unknown type filter", func() error {
		s := setup
		s.Types = []event.Type{event.Type("bogus.event")}
		return s.Validate()
	}())

	if event.MaxSubscribeSubjects <= 0 {
		t.Fatalf("event.MaxSubscribeSubjects = %d; want a positive bound", event.MaxSubscribeSubjects)
	}
	grant := event.SubscribeGrant{
		Subjects:  []string{"kodama.v1.tenant-1.project-1.events"},
		Cursor:    5,
		ExpiresAt: now.Add(time.Hour),
	}
	if err := grant.Validate(); err != nil {
		t.Fatalf("valid event subscribe grant refused: %v", err)
	}
	if err := grant.ValidateFor(scope); err != nil {
		t.Fatalf("valid event subscribe grant refused for its scope: %v", err)
	}
	if err := (event.SubscribeGrant{
		Subjects:  []string{"kodama.v1.tenant-1._.events"},
		Cursor:    5,
		ExpiresAt: now.Add(time.Hour),
	}).ValidateFor(tenancy.Scope{TenantID: id.TenantID("tenant-1")}); err != nil {
		t.Fatalf("valid tenant-wide event subject refused: %v", err)
	}

	assertInvalidInput(t, "event subscribe grant with no subjects", func() error {
		g := grant
		g.Subjects = nil
		return g.Validate()
	}())
	assertInvalidInput(t, "event subscribe grant with an oversized subject count", func() error {
		g := grant
		g.Subjects = make([]string, event.MaxSubscribeSubjects+1)
		for index := range g.Subjects {
			g.Subjects[index] = "kodama.v1.tenant-1.project-1.events"
		}
		return g.Validate()
	}())
	assertInvalidInput(t, "event subscribe grant with a wildcard subject", func() error {
		g := grant
		g.Subjects = []string{"kodama.v1.tenant-1.project-1.*"}
		return g.Validate()
	}())
	assertInvalidInput(t, "event subscribe grant with a malformed subject", func() error {
		g := grant
		g.Subjects = []string{"not-a-subject"}
		return g.Validate()
	}())
	assertInvalidInput(t, "event subscribe grant with mixed scopes", func() error {
		g := grant
		g.Subjects = []string{"kodama.v1.tenant-1.project-1.events", "kodama.v1.tenant-1.project-2.events"}
		return g.Validate()
	}())
	assertInvalidInput(t, "event subscribe grant for a foreign scope", func() error {
		return grant.ValidateFor(tenancy.Scope{TenantID: id.TenantID("tenant-2"), ProjectID: id.ProjectID("project-1")})
	}())
	assertInvalidInput(t, "event subscribe grant without an expiry", func() error {
		g := grant
		g.ExpiresAt = time.Time{}
		return g.Validate()
	}())

	grantType := reflect.TypeOf(grant)
	for _, field := range []string{"Subjects", "Cursor", "ExpiresAt"} {
		if _, ok := grantType.FieldByName(field); !ok {
			t.Errorf("event.SubscribeGrant is missing required field %q", field)
		}
	}
	for index := 0; index < grantType.NumField(); index++ {
		name := strings.ToLower(grantType.Field(index).Name)
		if strings.Contains(name, "credential") || strings.Contains(name, "token") {
			t.Errorf("event.SubscribeGrant field %q carries credential material", grantType.Field(index).Name)
		}
	}
	setupType := reflect.TypeOf(setup)
	for _, field := range []string{"Scope", "Types", "Cursor"} {
		if _, ok := setupType.FieldByName(field); !ok {
			t.Errorf("event.SubscribeSetup is missing required field %q", field)
		}
	}
}

func assertInvalidInput(t *testing.T, name string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s was accepted", name)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != fault.InvalidInput {
		t.Fatalf("%s class = %v; want invalid_input", name, class)
	}
}

type fakeServiceAdmin struct{}

func (fakeServiceAdmin) Create(context.Context, service.CreateUnit) (service.CreateResult, error) {
	return service.CreateResult{}, nil
}

func (fakeServiceAdmin) Update(context.Context, service.UpdateUnit) (service.UpdateResult, error) {
	return service.UpdateResult{}, nil
}

func (fakeServiceAdmin) SetStatus(context.Context, service.StatusUnit) (service.StatusResult, error) {
	return service.StatusResult{}, nil
}

type fakeEventRead struct{}

func (fakeEventRead) Read(context.Context, event.ReadQuery) (event.ReadPage, error) {
	return event.ReadPage{}, nil
}
