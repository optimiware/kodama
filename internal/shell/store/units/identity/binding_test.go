package identity_test

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func (h *harness) seedResource(scope tenancy.Scope, resourceID string) {
	h.t.Helper()
	h.seed(scope, "resources", resourceID, map[string]any{
		"kind":       "repository",
		"version":    int64(1),
		"metadata":   []byte(`{}`),
		"created_at": testNow,
	})
}

func bindingAuthority(scope tenancy.Scope, actor id.PrincipalID, resourceID string) []authorize.AuthorityRead {
	return []authorize.AuthorityRead{
		membershipRead(scope, actor),
		{Check: authorize.CheckResourceOwnership, Scope: scope, PrincipalID: actor, TargetID: resourceID},
	}
}

func (h *harness) registerUnit(scope tenancy.Scope, actor, bindingID, resourceID string) resource.BindingUnit {
	h.t.Helper()
	return resource.BindingUnit{
		Owner:           scope,
		BindingID:       resource.BindingID(bindingID),
		ExpectedVersion: 0,
		Binding: resource.Binding{
			ID:               resource.BindingID(bindingID),
			ResourceID:       resource.ResourceID(resourceID),
			Scope:            scope,
			EnforcingService: "service-a",
			TrustedOrigins:   []string{"https://git.example"},
			Operations:       []resource.Operation{resource.OperationGitRefUpdate},
			ReportableEvents: []string{"git.ref.updated"},
			Version:          1,
			Metadata:         map[string]string{"repo": "kodama", "owner": "team"},
		},
		Receipt:   h.receipt(scope, id.PrincipalID(actor), "resource.binding.register", "caller-"+bindingID, resource.BindingResult{Version: 1}),
		Audit:     []event.Audit{h.audit(scope, id.PrincipalID(actor), "resource.binding.register")},
		Events:    []event.Envelope{h.envelope(scope, id.PrincipalID(actor), bindingID, 1)},
		Authority: bindingAuthority(scope, id.PrincipalID(actor), resourceID),
	}
}

func TestRegisterBindingRoundTrip(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedResource(scope, "resource-a")
	unit := h.registerUnit(scope, "principal-admin", "binding-a", "resource-a")
	result, err := h.adapter.Apply(context.Background(), unit)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Version != 1 || result.Replayed {
		t.Fatalf("result = %+v; want version 1", result)
	}
	binding, err := h.adapter.ReadBinding(context.Background(), resource.BindingReadRequest{Scope: scope, BindingID: "binding-a"})
	if err != nil {
		t.Fatalf("ReadBinding: %v", err)
	}
	want := unit.Binding
	if !reflect.DeepEqual(binding, want) {
		t.Fatalf("read binding = %+v; want %+v", binding, want)
	}
	if got := len(h.store.Audit(scope)); got != 1 {
		t.Fatalf("audit rows = %d; want 1", got)
	}
	if got := len(h.store.Events(scope)); got != 1 {
		t.Fatalf("event rows = %d; want 1", got)
	}
	if _, found := h.store.Receipt(unit.Receipt.Key); !found {
		t.Fatal("the binding receipt was not committed")
	}
}

func TestRegisterBindingReplayReturnsOriginalResult(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedResource(scope, "resource-a")
	unit := h.registerUnit(scope, "principal-admin", "binding-a", "resource-a")
	first, err := h.adapter.Apply(context.Background(), unit)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	second, err := h.adapter.Apply(context.Background(), unit)
	if err != nil {
		t.Fatalf("replayed Apply: %v", err)
	}
	if !second.Replayed || second.Version != first.Version {
		t.Fatalf("replayed result = %+v; want version %d replayed", second, first.Version)
	}
	if got := len(h.store.Events(scope)); got != 1 {
		t.Fatalf("event rows after replay = %d; want 1", got)
	}
}

func TestUpdateBindingStaleVersionLeavesNoPartialState(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedResource(scope, "resource-a")
	if _, err := h.adapter.Apply(context.Background(), h.registerUnit(scope, "principal-admin", "binding-a", "resource-a")); err != nil {
		t.Fatalf("register: %v", err)
	}
	update := resource.BindingUnit{
		Owner:           scope,
		BindingID:       "binding-a",
		ExpectedVersion: 3,
		Binding: resource.Binding{
			ID:               "binding-a",
			ResourceID:       "resource-a",
			Scope:            scope,
			EnforcingService: "service-a",
			Operations:       []resource.Operation{resource.OperationGitRefUpdate},
			Version:          4,
			Metadata:         map[string]string{"repo": "changed"},
		},
		Receipt: h.receipt(scope, "principal-admin", "resource.binding.update", "caller-update", resource.BindingResult{Version: 4}),
		Audit:   []event.Audit{h.audit(scope, "principal-admin", "resource.binding.update")},
		Events:  []event.Envelope{h.envelope(scope, "principal-admin", "binding-a", 4)},
		Authority: []authorize.AuthorityRead{
			membershipRead(scope, "principal-admin"),
			{Check: authorize.CheckBindingVersion, Scope: scope, PrincipalID: "principal-admin", TargetID: "binding-a", ExpectedVersion: 3},
		},
	}
	_, err := h.adapter.Apply(context.Background(), update)
	requireFaultClass(t, err, fault.VersionConflict)
	binding, err := h.adapter.ReadBinding(context.Background(), resource.BindingReadRequest{Scope: scope, BindingID: "binding-a"})
	if err != nil {
		t.Fatalf("ReadBinding: %v", err)
	}
	if binding.Version != 1 {
		t.Fatalf("binding version = %d; want untouched 1", binding.Version)
	}
	if got := binding.Metadata["repo"]; got != "kodama" {
		t.Fatalf("binding metadata repo = %q; want untouched kodama", got)
	}
	if got := len(h.store.Events(scope)); got != 1 {
		t.Fatalf("event rows = %d; want only the registration event", got)
	}
}

func TestDeleteBindingUnderVersion(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedResource(scope, "resource-a")
	if _, err := h.adapter.Apply(context.Background(), h.registerUnit(scope, "principal-admin", "binding-a", "resource-a")); err != nil {
		t.Fatalf("register: %v", err)
	}
	unit := resource.BindingDeleteUnit{
		Owner:           scope,
		BindingID:       "binding-a",
		ExpectedVersion: 1,
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "resource.binding.delete", "caller-delete", resource.BindingDeleteResult{Version: 2}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "resource.binding.delete")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "binding-a", 2)},
		Authority: []authorize.AuthorityRead{
			membershipRead(scope, "principal-admin"),
			{Check: authorize.CheckBindingVersion, Scope: scope, PrincipalID: "principal-admin", TargetID: "binding-a", ExpectedVersion: 1},
		},
	}
	result, err := h.adapter.DeleteBinding(context.Background(), unit)
	if err != nil {
		t.Fatalf("DeleteBinding: %v", err)
	}
	if result.Version != 2 || result.Replayed {
		t.Fatalf("result = %+v; want version 2", result)
	}
	if _, found := h.snapshot(scope, "resource_bindings", "binding-a"); found {
		t.Fatal("the binding row was not removed")
	}
	if got := len(h.store.Audit(scope)); got != 2 {
		t.Fatalf("audit rows = %d; want registration plus deletion", got)
	}
}

func TestDeleteBindingStaleVersionKeepsRow(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedResource(scope, "resource-a")
	if _, err := h.adapter.Apply(context.Background(), h.registerUnit(scope, "principal-admin", "binding-a", "resource-a")); err != nil {
		t.Fatalf("register: %v", err)
	}
	unit := resource.BindingDeleteUnit{
		Owner:           scope,
		BindingID:       "binding-a",
		ExpectedVersion: 2,
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "resource.binding.delete", "caller-stale", resource.BindingDeleteResult{Version: 3}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "resource.binding.delete")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "binding-a", 3)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
	_, err := h.adapter.DeleteBinding(context.Background(), unit)
	requireFaultClass(t, err, fault.VersionConflict)
	if _, found := h.snapshot(scope, "resource_bindings", "binding-a"); !found {
		t.Fatal("the stale deletion removed the row")
	}
	if got := len(h.store.Events(scope)); got != 1 {
		t.Fatalf("event rows = %d; want only the registration event", got)
	}
}

func TestQueryBindingsPagesAreStableAndScoped(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	other := projectScope("tenant-a", "project-b")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	for _, binding := range []struct {
		id, resource string
	}{{"binding-1", "resource-1"}, {"binding-2", "resource-2"}, {"binding-3", "resource-3"}} {
		h.seedResource(scope, binding.resource)
		if _, err := h.adapter.Apply(context.Background(), h.registerUnit(scope, "principal-admin", binding.id, binding.resource)); err != nil {
			t.Fatalf("register %s: %v", binding.id, err)
		}
	}
	h.seedResource(other, "resource-x")
	foreign := h.registerUnit(other, "principal-admin", "binding-x", "resource-x")
	foreign.Authority = nil
	if _, err := h.adapter.Apply(context.Background(), foreign); err != nil {
		t.Fatalf("register foreign: %v", err)
	}

	first, err := h.adapter.QueryBindings(context.Background(), resource.BindingQuery{Scope: scope, Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Bindings) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %+v; want two rows and a cursor", first)
	}
	second, err := h.adapter.QueryBindings(context.Background(), resource.BindingQuery{Scope: scope, Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Bindings) != 1 {
		t.Fatalf("second page rows = %d; want 1", len(second.Bindings))
	}
	seen := map[resource.BindingID]bool{}
	for _, binding := range append(first.Bindings, second.Bindings...) {
		if seen[binding.ID] {
			t.Fatalf("binding %q was returned twice", binding.ID)
		}
		seen[binding.ID] = true
	}
	if seen["binding-x"] {
		t.Fatal("a cross-project binding leaked into the page")
	}
	if _, err := h.adapter.ReadBinding(context.Background(), resource.BindingReadRequest{Scope: other, BindingID: "binding-1"}); err == nil {
		t.Fatal("a cross-project read succeeded")
	} else {
		requireFaultClass(t, err, fault.NotFound)
	}
}

func TestConcurrentBindingUpdateOneWinner(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedResource(scope, "resource-a")
	if _, err := h.adapter.Apply(context.Background(), h.registerUnit(scope, "principal-admin", "binding-a", "resource-a")); err != nil {
		t.Fatalf("register: %v", err)
	}
	base := resource.BindingUnit{
		Owner:           scope,
		BindingID:       "binding-a",
		ExpectedVersion: 1,
		Binding: resource.Binding{
			ID:               "binding-a",
			ResourceID:       "resource-a",
			Scope:            scope,
			EnforcingService: "service-a",
			Operations:       []resource.Operation{resource.OperationGitRefUpdate},
			Version:          2,
			Metadata:         map[string]string{"repo": "a"},
		},
		Receipt: h.receipt(scope, "principal-admin", "resource.binding.update", "caller-a", resource.BindingResult{Version: 2}),
		Audit:   []event.Audit{h.audit(scope, "principal-admin", "resource.binding.update")},
		Events:  []event.Envelope{h.envelope(scope, "principal-admin", "binding-a", 2)},
		Authority: []authorize.AuthorityRead{
			membershipRead(scope, "principal-admin"),
			{Check: authorize.CheckBindingVersion, Scope: scope, PrincipalID: "principal-admin", TargetID: "binding-a", ExpectedVersion: 1},
		},
	}
	other := base
	other.Binding.Metadata = map[string]string{"repo": "b"}
	other.Receipt = h.receipt(scope, "principal-admin", "resource.binding.update", "caller-b", resource.BindingResult{Version: 2})
	other.Audit = []event.Audit{h.audit(scope, "principal-admin", "resource.binding.update")}
	other.Events = []event.Envelope{h.envelope(scope, "principal-admin", "binding-a", 2)}

	var wait sync.WaitGroup
	results := make([]error, 2)
	wait.Add(2)
	for index, unit := range []resource.BindingUnit{base, other} {
		go func(index int, unit resource.BindingUnit) {
			defer wait.Done()
			_, results[index] = h.adapter.Apply(context.Background(), unit)
		}(index, unit)
	}
	wait.Wait()
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
	binding, err := h.adapter.ReadBinding(context.Background(), resource.BindingReadRequest{Scope: scope, BindingID: "binding-a"})
	if err != nil {
		t.Fatalf("ReadBinding: %v", err)
	}
	if binding.Version != 2 {
		t.Fatalf("binding version = %d; want 2", binding.Version)
	}
}
