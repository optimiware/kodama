package identity_test

import (
	"context"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	tenancyadmin "github.com/optimiware/kodama/internal/shell/tenancy/admin"
	"github.com/optimiware/kodama/internal/shell/token"
)

// changedHash is a valid sha256 fingerprint that differs from every fixture
// receipt.
const changedHash = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// TestChangedReceiptContentRefused proves the idempotency identity rule on
// every mutation port group: equal content replays, changed content is refused
// with the changed_idempotency_content class and applies nothing.
func TestChangedReceiptContentRefused(t *testing.T) {
	t.Run("tenant create", func(t *testing.T) {
		h := newHarness(t)
		unit := h.tenantCreateUnit("tenant-a", "principal-admin")
		if _, err := h.adapter.CreateTenant(context.Background(), unit); err != nil {
			t.Fatalf("CreateTenant: %v", err)
		}
		changed := unit
		changed.Receipt.ContentHash = changedHash
		_, err := h.adapter.CreateTenant(context.Background(), changed)
		requireFaultClass(t, err, fault.ChangedIdempotencyContent)
		if got := len(h.store.Events(tenantScope("tenant-a"))); got != 1 {
			t.Fatalf("event rows = %d; want 1", got)
		}
	})
	t.Run("binding register", func(t *testing.T) {
		h := newHarness(t)
		scope := projectScope("tenant-a", "project-a")
		h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
		h.seedResource(scope, "resource-a")
		unit := h.registerUnit(scope, "principal-admin", "binding-a", "resource-a")
		if _, err := h.adapter.Apply(context.Background(), unit); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		changed := unit
		changed.Receipt.ContentHash = changedHash
		_, err := h.adapter.Apply(context.Background(), changed)
		requireFaultClass(t, err, fault.ChangedIdempotencyContent)
		if got := len(h.store.Events(scope)); got != 1 {
			t.Fatalf("event rows = %d; want 1", got)
		}
	})
	t.Run("credential mutate", func(t *testing.T) {
		h := newHarness(t)
		scope := projectScope("tenant-a", "project-a")
		h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
		h.seedExecution(scope, "execution-a", "principal-exec", 1, "running", nil, nil)
		unit := h.executionMutation(scope, "principal-admin", "execution-a", "principal-exec", token.MutationIssue, 1, testNonce(0x42), &testExpiry, token.MutationResult{State: token.StateCurrent, Version: 2})
		if _, err := h.adapter.Mutate(context.Background(), unit); err != nil {
			t.Fatalf("Mutate: %v", err)
		}
		changed := unit
		changed.Receipt.ContentHash = changedHash
		_, err := h.adapter.Mutate(context.Background(), changed)
		requireFaultClass(t, err, fault.ChangedIdempotencyContent)
		if got := len(h.store.Events(scope)); got != 1 {
			t.Fatalf("event rows = %d; want 1", got)
		}
	})
	t.Run("service create", func(t *testing.T) {
		h := newHarness(t)
		scope := projectScope("tenant-a", "project-a")
		h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
		unit := h.serviceCreateUnit(scope, "service-a")
		if _, err := h.adapter.Create(context.Background(), unit); err != nil {
			t.Fatalf("Create: %v", err)
		}
		changed := unit
		changed.Receipt.ContentHash = changedHash
		_, err := h.adapter.Create(context.Background(), changed)
		requireFaultClass(t, err, fault.ChangedIdempotencyContent)
		if got := len(h.store.Events(scope)); got != 1 {
			t.Fatalf("event rows = %d; want 1", got)
		}
	})
	t.Run("membership grant", func(t *testing.T) {
		h := newHarness(t)
		scope := projectScope("tenant-a", "project-a")
		h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
		unit := tenancyadmin.MembershipGrantUnit{
			Owner:           scope,
			Membership:      tenancy.Membership{Scope: scope, PrincipalID: "principal-b", Roles: []tenancy.Role{tenancy.RoleViewer}, Version: 1},
			ExpectedVersion: 0,
			At:              testNow,
			Receipt:         h.receipt(scope, "principal-admin", "membership.grant", "caller-grant", tenancyadmin.MembershipGrantResult{Version: 1}),
			Audit:           []event.Audit{h.audit(scope, "principal-admin", "membership.grant")},
			Events:          []event.Envelope{h.envelope(scope, "principal-admin", "principal-b", 1)},
			Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
		}
		if _, err := h.adapter.GrantMembership(context.Background(), unit); err != nil {
			t.Fatalf("GrantMembership: %v", err)
		}
		changed := unit
		changed.Receipt.ContentHash = changedHash
		_, err := h.adapter.GrantMembership(context.Background(), changed)
		requireFaultClass(t, err, fault.ChangedIdempotencyContent)
		if got := len(h.store.Events(scope)); got != 1 {
			t.Fatalf("event rows = %d; want 1", got)
		}
	})
}

func TestCrossScopeProjectUpdateRefused(t *testing.T) {
	h := newHarness(t)
	h.seedMembership(tenantScope("tenant-b"), "membership-admin-b", "principal-admin", 1, tenancy.RoleAdmin)
	h.seed(tenantScope("tenant-a"), "projects", "project-a", map[string]any{
		"name": "Project a", "version": int64(1), "created_at": testNow,
	})
	unit := tenancyadmin.ProjectUpdateUnit{
		Owner:           tenantScope("tenant-b"),
		ProjectID:       "project-a",
		ExpectedVersion: 1,
		Project:         tenancy.Project{ID: "project-a", TenantID: "tenant-b", Name: "Hijacked", Version: 2},
		At:              testNow,
		Receipt:         h.receipt(tenantScope("tenant-b"), "principal-admin", "project.update", "caller-cross", tenancyadmin.ProjectUpdateResult{Version: 2}),
		Audit:           []event.Audit{h.audit(tenantScope("tenant-b"), "principal-admin", "project.update")},
		Events:          []event.Envelope{h.envelope(tenantScope("tenant-b"), "principal-admin", "project-a", 2)},
		Authority:       []authorize.AuthorityRead{membershipRead(tenantScope("tenant-b"), "principal-admin")},
	}
	_, err := h.adapter.UpdateProject(context.Background(), unit)
	requireFaultClass(t, err, fault.VersionConflict)
	row, found := h.snapshot(tenantScope("tenant-a"), "projects", "project-a")
	if !found {
		t.Fatal("the project row disappeared")
	}
	if got := mustString(t, row, "name"); got != "Project a" {
		t.Fatalf("project name = %q; want the untouched name", got)
	}
}

func TestConcurrentProjectUpdateOneWinner(t *testing.T) {
	h := newHarness(t)
	scope := tenantScope("tenant-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seed(scope, "projects", "project-a", map[string]any{"name": "Project a", "version": int64(1), "created_at": testNow})
	base := tenancyadmin.ProjectUpdateUnit{
		Owner:           scope,
		ProjectID:       "project-a",
		ExpectedVersion: 1,
		Project:         tenancy.Project{ID: "project-a", TenantID: "tenant-a", Name: "First", Version: 2},
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "project.update", "caller-a", tenancyadmin.ProjectUpdateResult{Version: 2}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "project.update")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "project-a", 2)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
	other := base
	other.Project.Name = "Second"
	other.Receipt = h.receipt(scope, "principal-admin", "project.update", "caller-b", tenancyadmin.ProjectUpdateResult{Version: 2})
	other.Audit = []event.Audit{h.audit(scope, "principal-admin", "project.update")}
	other.Events = []event.Envelope{h.envelope(scope, "principal-admin", "project-a", 2)}
	results := runTwoProjectUpdates(h, base, other)
	requireOneWinner(t, results)
}

func runTwoProjectUpdates(h *harness, first, second tenancyadmin.ProjectUpdateUnit) []error {
	var wait sync.WaitGroup
	results := make([]error, 2)
	wait.Add(2)
	for index, unit := range []tenancyadmin.ProjectUpdateUnit{first, second} {
		go func(index int, unit tenancyadmin.ProjectUpdateUnit) {
			defer wait.Done()
			_, results[index] = h.adapter.UpdateProject(context.Background(), unit)
		}(index, unit)
	}
	wait.Wait()
	return results
}

func TestConcurrentServiceStatusOneWinner(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedService(scope, "service-a", 1, string(service.StateEnabled), testNonce(0x07), &testExpiry)
	base := service.StatusUnit{
		Owner:           scope,
		ServiceID:       "service-a",
		ExpectedVersion: 1,
		State:           service.StateDisabled,
		RevokeAll:       &service.RevokeAll{},
		Actor:           "principal-admin",
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "service.status", "caller-a", service.StatusResult{State: service.StateDisabled, Version: 2, RevokedCredentials: 1}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "service.status")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "service-a", 2)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
	other := base
	other.Receipt = h.receipt(scope, "principal-admin", "service.status", "caller-b", service.StatusResult{State: service.StateDisabled, Version: 2, RevokedCredentials: 1})
	other.Audit = []event.Audit{h.audit(scope, "principal-admin", "service.status")}
	other.Events = []event.Envelope{h.envelope(scope, "principal-admin", "service-a", 2)}
	var wait sync.WaitGroup
	results := make([]error, 2)
	wait.Add(2)
	for index, unit := range []service.StatusUnit{base, other} {
		go func(index int, unit service.StatusUnit) {
			defer wait.Done()
			_, results[index] = h.adapter.SetStatus(context.Background(), unit)
		}(index, unit)
	}
	wait.Wait()
	requireOneWinner(t, results)
	read, err := h.adapter.ReadService(context.Background(), service.ServiceReadRequest{Scope: scope, ServiceID: "service-a"})
	if err != nil {
		t.Fatalf("ReadService: %v", err)
	}
	if read.State != service.StateDisabled || read.TokenNonce != nil {
		t.Fatalf("service = %+v; want disabled with a cleared pair", read)
	}
}

// requireOneWinner asserts exactly one concurrent commit succeeded and every
// other refusal is a version conflict.
func requireOneWinner(t *testing.T, results []error) {
	t.Helper()
	winners := 0
	for _, err := range results {
		if err == nil {
			winners++
			continue
		}
		requireFaultClass(t, err, fault.VersionConflict)
	}
	if winners != 1 {
		t.Fatalf("winners = %d; want exactly one", winners)
	}
}

// TestInsertModeReceiptIsCompleted proves the wave-3 receipts.completed_at
// completion pair stays valid for a plain insert-mode unit: the store inserts
// the receipt already completed, so completed_at is non-NULL exactly when the
// durable result is non-NULL.
func TestInsertModeReceiptIsCompleted(t *testing.T) {
	h := newHarness(t)
	unit := h.tenantCreateUnit("tenant-a", "principal-admin")
	if _, err := h.adapter.CreateTenant(context.Background(), unit); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	page, err := h.store.Query(context.Background(), tenantScope("tenant-a"), store.ScanQuery{
		Table:   "receipts",
		Equals:  map[string]any{"id": string(unit.Receipt.ID)},
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   1,
	})
	if err != nil {
		t.Fatalf("scan receipts: %v", err)
	}
	if len(page.Rows) != 1 {
		t.Fatalf("receipt rows = %d; want 1", len(page.Rows))
	}
	row := page.Rows[0]
	if row.Values["completed_at"] == nil {
		t.Fatal("insert-mode receipt completed_at = NULL; want a completion marker")
	}
	if row.Values["result"] == nil {
		t.Fatal("insert-mode receipt result = NULL; want the durable result")
	}
	if row.Values["content_sha256"] == nil {
		t.Fatal("insert-mode receipt content_sha256 = NULL; want the content fingerprint")
	}
}
