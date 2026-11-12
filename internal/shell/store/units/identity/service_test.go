package identity_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func (h *harness) serviceCreateUnit(scope tenancy.Scope, serviceID string) service.CreateUnit {
	return service.CreateUnit{
		Owner:     scope,
		Service:   service.Service{ID: service.ServiceID(serviceID), Scope: scope, State: service.StateEnabled, Roles: []tenancy.Role{tenancy.RoleEngineer}, Version: 1},
		Actor:     "principal-admin",
		At:        testNow,
		Receipt:   h.receipt(scope, "principal-admin", "service.create", "caller-"+serviceID, service.CreateResult{ServiceID: service.ServiceID(serviceID), Version: 1}),
		Audit:     []event.Audit{h.audit(scope, "principal-admin", "service.create")},
		Events:    []event.Envelope{h.envelope(scope, "principal-admin", serviceID, 1)},
		Authority: []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
}

func TestCreateServiceCommitsRow(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	result, err := h.adapter.Create(context.Background(), h.serviceCreateUnit(scope, "service-a"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if result.Version != 1 || result.Replayed || result.ServiceID != "service-a" {
		t.Fatalf("result = %+v; want service-a version 1", result)
	}
	read, err := h.adapter.ReadService(context.Background(), service.ServiceReadRequest{Scope: scope, ServiceID: "service-a"})
	if err != nil {
		t.Fatalf("ReadService: %v", err)
	}
	if read.State != service.StateEnabled || read.Version != 1 {
		t.Fatalf("service = %+v; want enabled version 1", read)
	}
	if len(read.Roles) != 1 || read.Roles[0] != tenancy.RoleEngineer {
		t.Fatalf("roles = %v; want engineer", read.Roles)
	}
	if read.TokenNonce != nil || read.TokenExpiry != nil {
		t.Fatalf("created service = %+v; want no credential pair", read)
	}
	if got := len(h.store.Events(scope)); got != 1 {
		t.Fatalf("event rows = %d; want 1", got)
	}
}

func TestUpdateServiceRolesUnderExpectedState(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedService(scope, "service-a", 1, string(service.StateEnabled), nil, nil)
	update := service.UpdateUnit{
		Owner:           scope,
		ServiceID:       "service-a",
		ExpectedVersion: 1,
		ExpectedState:   service.StateEnabled,
		Service:         service.Service{ID: "service-a", Scope: scope, State: service.StateEnabled, Roles: []tenancy.Role{tenancy.RoleReviewer}, Version: 2},
		Actor:           "principal-admin",
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "service.update", "caller-update", service.UpdateResult{Version: 2}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "service.update")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "service-a", 2)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
	result, err := h.adapter.Update(context.Background(), update)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if result.Version != 2 || result.Replayed {
		t.Fatalf("result = %+v; want version 2", result)
	}
	read, err := h.adapter.ReadService(context.Background(), service.ServiceReadRequest{Scope: scope, ServiceID: "service-a"})
	if err != nil {
		t.Fatalf("ReadService: %v", err)
	}
	if len(read.Roles) != 1 || read.Roles[0] != tenancy.RoleReviewer {
		t.Fatalf("roles = %v; want reviewer", read.Roles)
	}

	stale := update
	stale.Receipt = h.receipt(scope, "principal-admin", "service.update", "caller-stale", service.UpdateResult{Version: 2})
	stale.Service.Version = 2
	_, err = h.adapter.Update(context.Background(), stale)
	requireFaultClass(t, err, fault.VersionConflict)

	wrongState := update
	wrongState.Receipt = h.receipt(scope, "principal-admin", "service.update", "caller-state", service.UpdateResult{Version: 2})
	wrongState.ExpectedVersion = 2
	wrongState.Service.Version = 3
	wrongState.ExpectedState = service.StateDisabled
	wrongState.Service.State = service.StateDisabled
	_, err = h.adapter.Update(context.Background(), wrongState)
	requireFaultClass(t, err, fault.VersionConflict)
}

func TestSetStatusDisableClearsCredential(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedService(scope, "service-a", 2, string(service.StateEnabled), testNonce(0x09), &testExpiry)
	unit := service.StatusUnit{
		Owner:           scope,
		ServiceID:       "service-a",
		ExpectedVersion: 2,
		State:           service.StateDisabled,
		RevokeAll:       &service.RevokeAll{},
		Actor:           "principal-admin",
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "service.status", "caller-disable", service.StatusResult{State: service.StateDisabled, Version: 3, RevokedCredentials: 1}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "service.status")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "service-a", 3)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
	result, err := h.adapter.SetStatus(context.Background(), unit)
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if result.State != service.StateDisabled || result.Version != 3 || result.RevokedCredentials != 1 || result.Replayed {
		t.Fatalf("result = %+v; want disabled version 3 with one revoked credential", result)
	}
	read, err := h.adapter.ReadService(context.Background(), service.ServiceReadRequest{Scope: scope, ServiceID: "service-a"})
	if err != nil {
		t.Fatalf("ReadService: %v", err)
	}
	if read.State != service.StateDisabled {
		t.Fatalf("state = %q; want disabled", read.State)
	}
	if read.TokenNonce != nil || read.TokenExpiry != nil {
		t.Fatalf("service = %+v; want the pair cleared in the same commit", read)
	}
}

func TestSetStatusDisableWithoutCredential(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedService(scope, "service-a", 1, string(service.StateEnabled), nil, nil)
	unit := service.StatusUnit{
		Owner:           scope,
		ServiceID:       "service-a",
		ExpectedVersion: 1,
		State:           service.StateDisabled,
		RevokeAll:       &service.RevokeAll{},
		Actor:           "principal-admin",
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "service.status", "caller-disable", service.StatusResult{State: service.StateDisabled, Version: 2}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "service.status")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "service-a", 2)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
	result, err := h.adapter.SetStatus(context.Background(), unit)
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if result.RevokedCredentials != 0 {
		t.Fatalf("revoked credentials = %d; want 0 for a service that never held one", result.RevokedCredentials)
	}
}

func TestSetStatusEnableDoesNotResurrectCredential(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedService(scope, "service-a", 2, string(service.StateDisabled), nil, nil)
	unit := service.StatusUnit{
		Owner:           scope,
		ServiceID:       "service-a",
		ExpectedVersion: 2,
		State:           service.StateEnabled,
		Actor:           "principal-admin",
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "service.status", "caller-enable", service.StatusResult{State: service.StateEnabled, Version: 3}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "service.status")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "service-a", 3)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
	result, err := h.adapter.SetStatus(context.Background(), unit)
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if result.State != service.StateEnabled || result.RevokedCredentials != 0 {
		t.Fatalf("result = %+v; want enabled with no revocation", result)
	}
	read, err := h.adapter.ReadService(context.Background(), service.ServiceReadRequest{Scope: scope, ServiceID: "service-a"})
	if err != nil {
		t.Fatalf("ReadService: %v", err)
	}
	if read.TokenNonce != nil || read.TokenExpiry != nil {
		t.Fatalf("service = %+v; enablement resurrected a credential", read)
	}
}

func TestSetStatusStaleVersionLeavesNoPartialState(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedService(scope, "service-a", 2, string(service.StateEnabled), testNonce(0x09), &testExpiry)
	unit := service.StatusUnit{
		Owner:           scope,
		ServiceID:       "service-a",
		ExpectedVersion: 1,
		State:           service.StateDisabled,
		RevokeAll:       &service.RevokeAll{},
		Actor:           "principal-admin",
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "service.status", "caller-stale", service.StatusResult{State: service.StateDisabled, Version: 2}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "service.status")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "service-a", 2)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
	_, err := h.adapter.SetStatus(context.Background(), unit)
	requireFaultClass(t, err, fault.VersionConflict)
	read, err := h.adapter.ReadService(context.Background(), service.ServiceReadRequest{Scope: scope, ServiceID: "service-a"})
	if err != nil {
		t.Fatalf("ReadService: %v", err)
	}
	if read.State != service.StateEnabled || read.Version != 2 || read.TokenNonce == nil {
		t.Fatalf("service = %+v; want the untouched enabled version 2 with its pair", read)
	}
	if got := len(h.store.Events(scope)); got != 0 {
		t.Fatalf("event rows = %d; want 0", got)
	}
}

func TestReadServiceScopeIsolation(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	other := projectScope("tenant-a", "project-b")
	h.seedService(scope, "service-a", 1, string(service.StateEnabled), nil, nil)
	_, err := h.adapter.ReadService(context.Background(), service.ServiceReadRequest{Scope: other, ServiceID: "service-a"})
	requireFaultClass(t, err, fault.NotFound)
}
