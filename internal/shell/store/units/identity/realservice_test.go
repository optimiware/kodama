package identity_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	shellidentity "github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	tenancyadmin "github.com/optimiware/kodama/internal/shell/tenancy/admin"
	"github.com/optimiware/kodama/internal/shell/token"
)

// The real-service tests drive the actual capability services (their real
// constructors and commit-unit builders) onto the store-backed adapters over
// the memory store. Only the boundaries this package does not own are faked:
// the authorizer, the injected clock/identities, the post-commit invalidator
// and deliverer, the derivation-key resolver, and the receipt lookup. The
// authority reader, principal reader, service reader and every mutation port
// are the real adapter under test.

// allowAuthorizer answers every grant question with allow. Authorization is a
// separate capability; these tests exercise the commit boundary, where the
// adapter re-evaluates the registry reads itself.
type allowAuthorizer struct{}

func (allowAuthorizer) Authorize(context.Context, authorize.Principal, authorize.OperationName, tenancy.Scope) (authorize.Decision, error) {
	return authorize.DecisionAllow, nil
}

// commitIDs is a deterministic, concurrency-safe event/receipt identity source.
type commitIDs struct {
	mu   sync.Mutex
	next int
}

func (source *commitIDs) allocate() int {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.next++
	return source.next
}

func (source *commitIDs) NewEventID() (id.EventID, error) {
	return id.EventID(fmt.Sprintf("event-%d", source.allocate())), nil
}

func (source *commitIDs) NewReceiptID() (id.ReceiptID, error) {
	return id.ReceiptID(fmt.Sprintf("receipt-%d", source.allocate())), nil
}

// recordingServiceInvalidator records post-commit service invalidations.
type recordingServiceInvalidator struct{ calls []service.ServiceInvalidation }

func (invalidator *recordingServiceInvalidator) InvalidateService(_ context.Context, invalidation service.ServiceInvalidation) error {
	invalidator.calls = append(invalidator.calls, invalidation)
	return nil
}

// recordingTokenInvalidator records post-commit credential invalidations.
type recordingTokenInvalidator struct{ calls []token.Invalidation }

func (invalidator *recordingTokenInvalidator) Invalidate(_ context.Context, invalidation token.Invalidation) error {
	invalidator.calls = append(invalidator.calls, invalidation)
	return nil
}

// recordingDeliverer records post-commit credential deliveries.
type recordingDeliverer struct{ calls []token.Delivery }

func (deliverer *recordingDeliverer) Deliver(_ context.Context, delivery token.Delivery) error {
	deliverer.calls = append(deliverer.calls, delivery)
	return nil
}

// staticResolver unwraps every tenant key to deterministic plaintext material.
// It stands in for the KMS boundary; the adapter never returns plaintext.
type staticResolver struct{}

func (staticResolver) ResolveDerivationKey(context.Context, token.TenantKey) ([]byte, error) {
	return bytes.Repeat([]byte{0x2a}, 32), nil
}

// memoryReceipts is the token.ReceiptLookup seam backed by the test
// memory store. It is a fake only because the readers package owns the
// production implementation.
type memoryReceipts struct{ store memoryStore }

func (lookup memoryReceipts) Lookup(_ context.Context, key receipt.Key) (receipt.Record, bool, error) {
	record, found := lookup.store.Receipt(key)
	return record, found, nil
}

// realServices binds the real capability services to one adapter over one
// memory store.
type realServices struct {
	t                    *testing.T
	h                    *harness
	tenancy              *tenancyadmin.Service
	bindings             *resource.Service
	services             *service.Admin
	credentials          *token.CredentialAdmin
	manager              *token.Manager
	serviceInvalidations *recordingServiceInvalidator
	deliveries           *recordingDeliverer
}

func newRealServices(t *testing.T) *realServices {
	t.Helper()
	return newRealServicesOn(t, newHarness(t))
}

// newRealServicesOn binds the real capability services to the adapter of an
// already-built harness, so a test can install an observing executor and
// interpose a concurrent change before a commit unit applies.
func newRealServicesOn(t *testing.T, h *harness) *realServices {
	t.Helper()
	clock := fixedClock{now: testNow}
	ids := &commitIDs{}
	receipts := memoryReceipts{store: h.store}
	invalidations := &recordingServiceInvalidator{}
	tokenInvalidations := &recordingTokenInvalidator{}
	deliverer := &recordingDeliverer{}

	tenancyService, err := tenancyadmin.NewService(tenancyadmin.Config{
		Clock:             clock,
		IDs:               ids,
		TenantCreate:      h.adapter,
		TenantKeyRotation: h.adapter,
		ProjectCreate:     h.adapter,
		ProjectUpdate:     h.adapter,
		MembershipGrant:   h.adapter,
		MembershipRevoke:  h.adapter,
		MembershipQuery:   h.adapter,
	})
	if err != nil {
		t.Fatalf("tenancyadmin.NewService: %v", err)
	}
	bindingService, err := resource.NewService(resource.Config{
		Clock:    clock,
		IDs:      ids,
		Bindings: h.adapter,
		Deletes:  h.adapter,
		Reads:    h.adapter,
		Queries:  h.adapter,
	})
	if err != nil {
		t.Fatalf("resource.NewService: %v", err)
	}
	serviceAdmin, err := service.NewAdmin(service.AdminConfig{
		Authorizer:    allowAuthorizer{},
		Authority:     h.adapter,
		Principals:    h.adapter,
		Services:      h.adapter,
		Port:          h.adapter,
		Invalidations: invalidations,
		Clock:         clock,
		IDs:           ids,
	})
	if err != nil {
		t.Fatalf("service.NewAdmin: %v", err)
	}
	manager, err := token.NewManager(token.ManagerConfig{
		Keys:          h.adapter,
		Resolver:      staticResolver{},
		Credentials:   h.adapter,
		Principals:    h.adapter,
		Services:      h.adapter,
		Mutations:     h.adapter,
		Receipts:      receipts,
		Deliverer:     deliverer,
		Invalidations: tokenInvalidations,
		Clock:         clock,
		Random:        bytes.NewReader(make([]byte, 1024)),
		Bounds:        token.DefaultBounds,
	})
	if err != nil {
		t.Fatalf("token.NewManager: %v", err)
	}
	credentialAdmin, err := token.NewCredentialAdmin(token.CredentialAdminConfig{
		Authorizer:  allowAuthorizer{},
		Authority:   h.adapter,
		Principals:  h.adapter,
		Services:    h.adapter,
		Credentials: h.adapter,
		Manager:     manager,
		Receipts:    receipts,
		Clock:       clock,
		IDs:         ids,
	})
	if err != nil {
		t.Fatalf("token.NewCredentialAdmin: %v", err)
	}
	return &realServices{
		t:                    t,
		h:                    h,
		tenancy:              tenancyService,
		bindings:             bindingService,
		services:             serviceAdmin,
		credentials:          credentialAdmin,
		manager:              manager,
		serviceInvalidations: invalidations,
		deliveries:           deliverer,
	}
}

// memoryStore is the narrow receipt surface the receipt-lookup fake needs.
type memoryStore interface {
	Receipt(receipt.Key) (receipt.Record, bool)
}

func requestContent(label string) receipt.Content {
	return receipt.Content{MediaType: "application/json", Bytes: []byte(`{"request":"` + label + `"}`)}
}

func humanPrincipal(scope tenancy.Scope, principal id.PrincipalID, roles ...tenancy.Role) authorize.Principal {
	return authorize.Principal{
		ID:      principal,
		Kind:    shellidentity.KindHuman,
		Scope:   scope,
		Roles:   roles,
		Enabled: true,
	}
}

// seedTenantRow commits a tenant row with its encrypted derivation key so a
// credential test can resolve a key without first creating the tenant.
func (h *harness) seedTenantRow(tenant string) {
	h.t.Helper()
	encoded, err := json.Marshal(testTenantKey(tenant, 1))
	if err != nil {
		h.t.Fatalf("marshal tenant key: %v", err)
	}
	h.seed(tenancy.Scope{TenantID: id.TenantID(tenant)}, "tenants", tenant, map[string]any{
		"name":           "Tenant " + tenant,
		"version":        int64(1),
		"derivation_key": encoded,
		"created_at":     testNow,
	})
}

// membershipFor returns one committed membership row for a principal, if any.
func (h *harness) membershipFor(scope tenancy.Scope, principal id.PrincipalID) (tenancyadmin.MembershipPage, error) {
	return h.adapter.ListMemberships(context.Background(), tenancyadmin.MembershipQuery{
		Scope:       scope,
		PrincipalID: principal,
		Limit:       10,
	})
}

// requireReceiptAbsent asserts no receipt was committed for one caller key.
func (h *harness) requireReceiptAbsent(scope tenancy.Scope, principal id.PrincipalID, operation, callerKey string) {
	h.t.Helper()
	key := receipt.Key{
		TenantID:    scope.TenantID,
		ProjectID:   scope.ProjectID,
		PrincipalID: principal,
		Operation:   operation,
		CallerKey:   callerKey,
	}
	if _, found := h.store.Receipt(key); found {
		h.t.Fatalf("receipt for %s/%s was committed; want nothing persisted", operation, callerKey)
	}
}

// TestRealTenancyAdminLifecycle drives the real tenancyadmin.Service through
// tenant creation, project creation/update, membership grant/revoke and the
// revoke replay, asserting each commit advances the row version, a stale
// command is a version_conflict that persists nothing, and a demoted actor is
// refused at the commit boundary.
func TestRealTenancyAdminLifecycle(t *testing.T) {
	ctx := context.Background()
	r := newRealServices(t)
	tenant := tenantScope("tenant-a")
	actor := humanPrincipal(tenant, "principal-admin", tenancy.RoleAdmin)
	// tenant.create's principal_enabled read names the NEW tenant's scope, the
	// only scope an AuthorityRead carries; the actor's grant must resolve
	// there. The adapter cannot infer the actor's own tenant from the read, so
	// this test seeds the actor grant in that named scope.
	r.h.seedMembership(tenant, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)

	created, err := r.tenancy.CreateTenant(ctx, tenancyadmin.TenantCreateCommand{
		Actor:     actor,
		TenantID:  "tenant-a",
		Name:      "Tenant a",
		Key:       testTenantKey("tenant-a", 1),
		CallerKey: "caller-tenant-create",
		Content:   requestContent("tenant-create"),
	})
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if created.Version != 1 || created.Replayed || created.TenantID != "tenant-a" {
		t.Fatalf("tenant create result = %+v; want committed version 1", created)
	}
	tenantRow, found := r.h.snapshot(tenant, "tenants", "tenant-a")
	if !found || mustInt(t, tenantRow, "version") != 1 {
		t.Fatalf("tenant row = %+v found=%t; want committed version 1", tenantRow, found)
	}

	project, err := r.tenancy.CreateProject(ctx, tenancyadmin.ProjectCreateCommand{
		Actor:     actor,
		ProjectID: "project-a",
		Name:      "Project a",
		CallerKey: "caller-project-create",
		Content:   requestContent("project-create"),
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if project.Version != 1 || project.Replayed {
		t.Fatalf("project create result = %+v; want committed version 1", project)
	}
	projectRow, found := r.h.snapshot(tenant, "projects", "project-a")
	if !found || mustInt(t, projectRow, "version") != 1 {
		t.Fatalf("project row = %+v found=%t; want committed version 1", projectRow, found)
	}

	updated, err := r.tenancy.UpdateProject(ctx, tenancyadmin.ProjectUpdateCommand{
		Actor:           actor,
		ProjectID:       "project-a",
		ExpectedVersion: 1,
		Name:            "Project a renamed",
		CallerKey:       "caller-project-update",
		Content:         requestContent("project-update"),
	})
	if err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	if updated.Version != 2 || updated.Replayed {
		t.Fatalf("project update result = %+v; want committed version 2", updated)
	}
	projectRow, _ = r.h.snapshot(tenant, "projects", "project-a")
	if got := mustInt(t, projectRow, "version"); got != 2 {
		t.Fatalf("project version = %d; want 2", got)
	}

	// A stale project update is a version_conflict and persists no row change,
	// receipt or event.
	eventsBefore := len(r.h.store.Events(tenant))
	_, err = r.tenancy.UpdateProject(ctx, tenancyadmin.ProjectUpdateCommand{
		Actor:           actor,
		ProjectID:       "project-a",
		ExpectedVersion: 1,
		Name:            "stale",
		CallerKey:       "caller-project-stale",
		Content:         requestContent("project-stale"),
	})
	requireFaultClass(t, err, fault.VersionConflict)
	projectRow, _ = r.h.snapshot(tenant, "projects", "project-a")
	if got := mustInt(t, projectRow, "version"); got != 2 {
		t.Fatalf("project version after stale update = %d; want the untouched 2", got)
	}
	if got := len(r.h.store.Events(tenant)); got != eventsBefore {
		t.Fatalf("event rows = %d after a stale update; want %d", got, eventsBefore)
	}
	r.h.requireReceiptAbsent(tenant, "principal-admin", "project.update", "caller-project-stale")

	granted, err := r.tenancy.GrantMembership(ctx, tenancyadmin.MembershipGrantCommand{
		Actor:           actor,
		Membership:      tenancy.Membership{Scope: tenant, PrincipalID: "principal-b", Roles: []tenancy.Role{tenancy.RoleViewer}},
		ExpectedVersion: 0,
		CallerKey:       "caller-membership-grant",
		Content:         requestContent("membership-grant"),
	})
	if err != nil {
		t.Fatalf("GrantMembership: %v", err)
	}
	if granted.Version != 1 || granted.Replayed {
		t.Fatalf("membership grant result = %+v; want committed version 1", granted)
	}
	page, err := r.h.membershipFor(tenant, "principal-b")
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if len(page.Memberships) != 1 || page.Memberships[0].Version != 1 {
		t.Fatalf("memberships = %+v; want one committed version 1 grant", page.Memberships)
	}

	// A grant at a version the committed row no longer has (the row was
	// revoked) is a version_conflict and inserts nothing.
	revoked, err := r.tenancy.RevokeMembership(ctx, tenancyadmin.MembershipRevokeCommand{
		Actor:           actor,
		Scope:           tenant,
		PrincipalID:     "principal-b",
		ExpectedVersion: 1,
		CallerKey:       "caller-membership-revoke",
		Content:         requestContent("membership-revoke"),
	})
	if err != nil {
		t.Fatalf("RevokeMembership: %v", err)
	}
	if revoked.Version != 2 || revoked.Replayed {
		t.Fatalf("membership revoke result = %+v; want committed version 2", revoked)
	}
	page, err = r.h.membershipFor(tenant, "principal-b")
	if err != nil {
		t.Fatalf("ListMemberships after revoke: %v", err)
	}
	if len(page.Memberships) != 0 {
		t.Fatalf("memberships = %+v; want the revoked grant gone", page.Memberships)
	}

	// An equal-content revoke retry replays the committed result even though the
	// row its unit deleted is gone.
	replayed, err := r.tenancy.RevokeMembership(ctx, tenancyadmin.MembershipRevokeCommand{
		Actor:           actor,
		Scope:           tenant,
		PrincipalID:     "principal-b",
		ExpectedVersion: 1,
		CallerKey:       "caller-membership-revoke",
		Content:         requestContent("membership-revoke"),
	})
	if err != nil {
		t.Fatalf("RevokeMembership replay: %v", err)
	}
	if !replayed.Replayed || replayed.Version != 2 {
		t.Fatalf("membership revoke replay = %+v; want replayed version 2", replayed)
	}

	// A different caller key at the revoked version conflicts and persists
	// nothing.
	_, err = r.tenancy.RevokeMembership(ctx, tenancyadmin.MembershipRevokeCommand{
		Actor:           actor,
		Scope:           tenant,
		PrincipalID:     "principal-b",
		ExpectedVersion: 1,
		CallerKey:       "caller-membership-revoke-stale",
		Content:         requestContent("membership-revoke-stale"),
	})
	requireFaultClass(t, err, fault.VersionConflict)
	r.h.requireReceiptAbsent(tenant, "principal-admin", "membership.revoke", "caller-membership-revoke-stale")

	// A grant at a stale expected version after the row is gone conflicts.
	_, err = r.tenancy.GrantMembership(ctx, tenancyadmin.MembershipGrantCommand{
		Actor:           actor,
		Membership:      tenancy.Membership{Scope: tenant, PrincipalID: "principal-b", Roles: []tenancy.Role{tenancy.RoleViewer}},
		ExpectedVersion: 1,
		CallerKey:       "caller-membership-grant-stale",
		Content:         requestContent("membership-grant-stale"),
	})
	requireFaultClass(t, err, fault.VersionConflict)

	// A demoted actor (Viewer, not Admin) cannot commit an administration unit:
	// the adapter re-evaluates the required role inside the transaction.
	r.h.seedMembership(tenant, "membership-demoted", "principal-demoted", 1, tenancy.RoleViewer)
	demoted := humanPrincipal(tenant, "principal-demoted", tenancy.RoleViewer)
	_, err = r.tenancy.GrantMembership(ctx, tenancyadmin.MembershipGrantCommand{
		Actor:           demoted,
		Membership:      tenancy.Membership{Scope: tenant, PrincipalID: "principal-b", Roles: []tenancy.Role{tenancy.RoleViewer}},
		ExpectedVersion: 0,
		CallerKey:       "caller-membership-demoted",
		Content:         requestContent("membership-demoted"),
	})
	requireFaultClass(t, err, fault.Forbidden)
	r.h.requireReceiptAbsent(tenant, "principal-demoted", "membership.grant", "caller-membership-demoted")
	page, err = r.h.membershipFor(tenant, "principal-b")
	if err != nil {
		t.Fatalf("ListMemberships after demoted refusal: %v", err)
	}
	if len(page.Memberships) != 0 {
		t.Fatalf("memberships = %+v; a demoted actor committed a grant", page.Memberships)
	}
}

// TestRealResourceBindingLifecycle drives the real resource.Service through
// register, read, update and delete over the adapter, including the stale
// version conflict.
func TestRealResourceBindingLifecycle(t *testing.T) {
	ctx := context.Background()
	r := newRealServices(t)
	scope := projectScope("tenant-a", "project-a")
	actor := humanPrincipal(scope, "principal-admin", tenancy.RoleAdmin)
	r.h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	r.h.seedResource(scope, "resource-a")

	registered, err := r.bindings.RegisterBinding(ctx, resource.RegisterBindingCommand{
		Actor:            actor,
		Scope:            scope,
		BindingID:        "binding-a",
		ResourceID:       "resource-a",
		EnforcingService: "service-a",
		TrustedOrigins:   []string{"https://git.example"},
		Operations:       []resource.Operation{resource.OperationGitRefUpdate},
		ReportableEvents: []string{"git.ref.updated"},
		Metadata:         map[string]string{"repo": "kodama"},
		CallerKey:        "caller-binding-register",
		Content:          requestContent("binding-register"),
	})
	if err != nil {
		t.Fatalf("RegisterBinding: %v", err)
	}
	if registered.Version != 1 || registered.Replayed {
		t.Fatalf("binding register result = %+v; want committed version 1", registered)
	}
	read, err := r.bindings.ReadBinding(ctx, resource.ReadBindingCommand{Actor: actor, Scope: scope, BindingID: "binding-a"})
	if err != nil {
		t.Fatalf("ReadBinding: %v", err)
	}
	if read.Version != 1 || read.ResourceID != "resource-a" {
		t.Fatalf("binding = %+v; want the committed version 1 value", read)
	}

	updated, err := r.bindings.UpdateBinding(ctx, resource.UpdateBindingCommand{
		Actor:            actor,
		Scope:            scope,
		BindingID:        "binding-a",
		ResourceID:       "resource-a",
		EnforcingService: "service-a",
		TrustedOrigins:   []string{"https://git.example"},
		Operations:       []resource.Operation{resource.OperationGitRefUpdate},
		ReportableEvents: []string{"git.ref.updated"},
		Metadata:         map[string]string{"repo": "kodama", "owner": "team"},
		ExpectedVersion:  1,
		CallerKey:        "caller-binding-update",
		Content:          requestContent("binding-update"),
	})
	if err != nil {
		t.Fatalf("UpdateBinding: %v", err)
	}
	if updated.Version != 2 || updated.Replayed {
		t.Fatalf("binding update result = %+v; want committed version 2", updated)
	}

	eventsBefore := len(r.h.store.Events(scope))
	_, err = r.bindings.UpdateBinding(ctx, resource.UpdateBindingCommand{
		Actor:            actor,
		Scope:            scope,
		BindingID:        "binding-a",
		ResourceID:       "resource-a",
		EnforcingService: "service-a",
		TrustedOrigins:   []string{"https://git.example"},
		Operations:       []resource.Operation{resource.OperationGitRefUpdate},
		ReportableEvents: []string{"git.ref.updated"},
		Metadata:         map[string]string{"repo": "stale"},
		ExpectedVersion:  1,
		CallerKey:        "caller-binding-stale",
		Content:          requestContent("binding-stale"),
	})
	requireFaultClass(t, err, fault.VersionConflict)
	read, err = r.bindings.ReadBinding(ctx, resource.ReadBindingCommand{Actor: actor, Scope: scope, BindingID: "binding-a"})
	if err != nil {
		t.Fatalf("ReadBinding after stale update: %v", err)
	}
	if read.Version != 2 {
		t.Fatalf("binding version = %d after a stale update; want 2", read.Version)
	}
	if got := len(r.h.store.Events(scope)); got != eventsBefore {
		t.Fatalf("event rows = %d after a stale update; want %d", got, eventsBefore)
	}
	r.h.requireReceiptAbsent(scope, "principal-admin", "resource.binding.update", "caller-binding-stale")

	deleted, err := r.bindings.DeleteBinding(ctx, resource.DeleteBindingCommand{
		Actor:           actor,
		Scope:           scope,
		BindingID:       "binding-a",
		ExpectedVersion: 2,
		CallerKey:       "caller-binding-delete",
		Content:         requestContent("binding-delete"),
	})
	if err != nil {
		t.Fatalf("DeleteBinding: %v", err)
	}
	if deleted.Version != 3 || deleted.Replayed {
		t.Fatalf("binding delete result = %+v; want committed version 3", deleted)
	}
	if _, err := r.bindings.ReadBinding(ctx, resource.ReadBindingCommand{Actor: actor, Scope: scope, BindingID: "binding-a"}); err == nil {
		t.Fatal("ReadBinding succeeded after delete; want not_found")
	} else {
		requireFaultClass(t, err, fault.NotFound)
	}
}

// TestRealServiceAdminLifecycle drives the real service.Admin through create,
// update, disable and enable over the adapter, including a stale status change
// and the post-commit invalidation.
func TestRealServiceAdminLifecycle(t *testing.T) {
	ctx := context.Background()
	r := newRealServices(t)
	scope := projectScope("tenant-a", "project-a")
	actor := humanPrincipal(scope, "principal-admin", tenancy.RoleAdmin)
	r.h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)

	created, err := r.services.Create(ctx, service.CreateCommand{
		Actor:     actor,
		ID:        "service-a",
		Scope:     scope,
		State:     service.StateEnabled,
		Roles:     []tenancy.Role{tenancy.RoleEngineer},
		CallerKey: "caller-service-create",
		Content:   requestContent("service-create"),
	})
	if err != nil {
		t.Fatalf("service Create: %v", err)
	}
	if created.Version != 1 || created.Replayed || created.ServiceID != "service-a" {
		t.Fatalf("service create result = %+v; want committed version 1", created)
	}
	current, err := r.h.adapter.ReadService(ctx, service.ServiceReadRequest{Scope: scope, ServiceID: "service-a"})
	if err != nil {
		t.Fatalf("ReadService: %v", err)
	}
	if current.State != service.StateEnabled || current.Version != 1 {
		t.Fatalf("service = %+v; want enabled version 1", current)
	}

	updated, err := r.services.Update(ctx, service.UpdateCommand{
		Actor:           actor,
		Scope:           scope,
		ServiceID:       "service-a",
		ExpectedVersion: 1,
		Roles:           []tenancy.Role{tenancy.RoleReviewer},
		CallerKey:       "caller-service-update",
		Content:         requestContent("service-update"),
	})
	if err != nil {
		t.Fatalf("service Update: %v", err)
	}
	if updated.Version != 2 || updated.Replayed {
		t.Fatalf("service update result = %+v; want committed version 2", updated)
	}

	disabled, err := r.services.Disable(ctx, service.StatusCommand{
		Actor:           actor,
		Scope:           scope,
		ServiceID:       "service-a",
		ExpectedVersion: 2,
		CallerKey:       "caller-service-disable",
		Content:         requestContent("service-disable"),
	})
	if err != nil {
		t.Fatalf("service Disable: %v", err)
	}
	if disabled.Version != 3 || disabled.State != service.StateDisabled {
		t.Fatalf("service disable result = %+v; want disabled version 3", disabled)
	}
	if len(r.serviceInvalidations.calls) != 1 {
		t.Fatalf("service invalidations = %d; want one post-commit invalidation", len(r.serviceInvalidations.calls))
	}

	enabled, err := r.services.Enable(ctx, service.StatusCommand{
		Actor:           actor,
		Scope:           scope,
		ServiceID:       "service-a",
		ExpectedVersion: 3,
		CallerKey:       "caller-service-enable",
		Content:         requestContent("service-enable"),
	})
	if err != nil {
		t.Fatalf("service Enable: %v", err)
	}
	if enabled.Version != 4 || enabled.State != service.StateEnabled {
		t.Fatalf("service enable result = %+v; want enabled version 4", enabled)
	}

	// A stale status change conflicts and persists nothing.
	eventsBefore := len(r.h.store.Events(scope))
	_, err = r.services.Disable(ctx, service.StatusCommand{
		Actor:           actor,
		Scope:           scope,
		ServiceID:       "service-a",
		ExpectedVersion: 2,
		CallerKey:       "caller-service-stale",
		Content:         requestContent("service-stale"),
	})
	requireFaultClass(t, err, fault.VersionConflict)
	current, err = r.h.adapter.ReadService(ctx, service.ServiceReadRequest{Scope: scope, ServiceID: "service-a"})
	if err != nil {
		t.Fatalf("ReadService after stale disable: %v", err)
	}
	if current.State != service.StateEnabled || current.Version != 4 {
		t.Fatalf("service = %+v; want the untouched enabled version 4", current)
	}
	if got := len(r.serviceInvalidations.calls); got != 1 {
		t.Fatalf("service invalidations = %d after a stale disable; want 1", got)
	}
	if got := len(r.h.store.Events(scope)); got != eventsBefore {
		t.Fatalf("event rows = %d after a stale disable; want %d", got, eventsBefore)
	}
	r.h.requireReceiptAbsent(scope, "principal-admin", "service.disable", "caller-service-stale")
}

// TestRealCredentialAdminLifecycle drives the real token.CredentialAdmin and
// token.Manager through issue, rotate and revoke over the adapter, including
// equal-content replays and a stale version.
func TestRealCredentialAdminLifecycle(t *testing.T) {
	ctx := context.Background()
	r := newRealServices(t)
	scope := projectScope("tenant-a", "project-a")
	actor := humanPrincipal(scope, "principal-admin", tenancy.RoleAdmin)
	r.h.seedTenantRow("tenant-a")
	r.h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	created, err := r.services.Create(ctx, service.CreateCommand{
		Actor:     actor,
		ID:        "service-a",
		Scope:     scope,
		State:     service.StateEnabled,
		Roles:     []tenancy.Role{tenancy.RoleEngineer},
		CallerKey: "caller-credential-service-create",
		Content:   requestContent("credential-service-create"),
	})
	if err != nil {
		t.Fatalf("service Create: %v", err)
	}
	if created.Version != 1 {
		t.Fatalf("service create result = %+v; want version 1", created)
	}

	issued, err := r.credentials.Issue(ctx, token.ServiceCredentialCommand{
		Actor:           actor,
		Scope:           scope,
		ServiceID:       "service-a",
		TTL:             time.Hour,
		ExpectedVersion: 1,
		CallerKey:       "caller-credential-issue",
		Content:         requestContent("credential-issue"),
	})
	if err != nil {
		t.Fatalf("credential Issue: %v", err)
	}
	if issued.Version != 2 || issued.Replayed || issued.Token == "" {
		t.Fatalf("credential issue result = %+v; want a fresh token at version 2", issued)
	}
	current, err := r.h.adapter.ReadService(ctx, service.ServiceReadRequest{Scope: scope, ServiceID: "service-a"})
	if err != nil {
		t.Fatalf("ReadService after issue: %v", err)
	}
	if current.Version != 2 || current.TokenNonce == nil {
		t.Fatalf("service = %+v; want version 2 with the issued pair", current)
	}

	replayedIssue, err := r.credentials.Issue(ctx, token.ServiceCredentialCommand{
		Actor:           actor,
		Scope:           scope,
		ServiceID:       "service-a",
		TTL:             time.Hour,
		ExpectedVersion: 1,
		CallerKey:       "caller-credential-issue",
		Content:         requestContent("credential-issue"),
	})
	if err != nil {
		t.Fatalf("credential Issue replay: %v", err)
	}
	if !replayedIssue.Replayed || replayedIssue.Version != 2 || replayedIssue.Token != "" {
		t.Fatalf("credential issue replay = %+v; want replayed version 2 with no token", replayedIssue)
	}

	rotated, err := r.credentials.Rotate(ctx, token.ServiceCredentialCommand{
		Actor:           actor,
		Scope:           scope,
		ServiceID:       "service-a",
		TTL:             time.Hour,
		ExpectedVersion: 2,
		CallerKey:       "caller-credential-rotate",
		Content:         requestContent("credential-rotate"),
	})
	if err != nil {
		t.Fatalf("credential Rotate: %v", err)
	}
	if rotated.Version != 3 || rotated.Replayed || rotated.Token == "" {
		t.Fatalf("credential rotate result = %+v; want a fresh token at version 3", rotated)
	}
	replayedRotate, err := r.credentials.Rotate(ctx, token.ServiceCredentialCommand{
		Actor:           actor,
		Scope:           scope,
		ServiceID:       "service-a",
		TTL:             time.Hour,
		ExpectedVersion: 2,
		CallerKey:       "caller-credential-rotate",
		Content:         requestContent("credential-rotate"),
	})
	if err != nil {
		t.Fatalf("credential Rotate replay: %v", err)
	}
	if !replayedRotate.Replayed || replayedRotate.Version != 3 || replayedRotate.Token != "" {
		t.Fatalf("credential rotate replay = %+v; want replayed version 3 with no token", replayedRotate)
	}

	revoked, err := r.credentials.Revoke(ctx, token.ServiceCredentialRevokeCommand{
		Actor:           actor,
		Scope:           scope,
		ServiceID:       "service-a",
		ExpectedVersion: 3,
		CallerKey:       "caller-credential-revoke",
		Content:         requestContent("credential-revoke"),
	})
	if err != nil {
		t.Fatalf("credential Revoke: %v", err)
	}
	if revoked.Mutation.Version != 4 || revoked.Mutation.State != token.StateAbsent || revoked.Mutation.Replayed {
		t.Fatalf("credential revoke result = %+v; want absent version 4", revoked)
	}
	current, err = r.h.adapter.ReadService(ctx, service.ServiceReadRequest{Scope: scope, ServiceID: "service-a"})
	if err != nil {
		t.Fatalf("ReadService after revoke: %v", err)
	}
	if current.TokenNonce != nil || current.TokenExpiry != nil {
		t.Fatalf("service = %+v; want the revoked credential cleared", current)
	}

	replayedRevoke, err := r.credentials.Revoke(ctx, token.ServiceCredentialRevokeCommand{
		Actor:           actor,
		Scope:           scope,
		ServiceID:       "service-a",
		ExpectedVersion: 3,
		CallerKey:       "caller-credential-revoke",
		Content:         requestContent("credential-revoke"),
	})
	if err != nil {
		t.Fatalf("credential Revoke replay: %v", err)
	}
	if !replayedRevoke.Mutation.Replayed || replayedRevoke.Mutation.Version != 4 {
		t.Fatalf("credential revoke replay = %+v; want replayed version 4", replayedRevoke)
	}

	// A stale revoke conflicts and persists nothing.
	_, err = r.credentials.Revoke(ctx, token.ServiceCredentialRevokeCommand{
		Actor:           actor,
		Scope:           scope,
		ServiceID:       "service-a",
		ExpectedVersion: 3,
		CallerKey:       "caller-credential-stale",
		Content:         requestContent("credential-stale"),
	})
	requireFaultClass(t, err, fault.VersionConflict)
	r.h.requireReceiptAbsent(scope, "principal-admin", "service.credential.revoke", "caller-credential-stale")
}
