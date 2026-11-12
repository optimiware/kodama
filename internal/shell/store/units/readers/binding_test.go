package readers_test

import (
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// currentPrincipal resolves one presented actor credential.
type currentPrincipal struct {
	principal identity.Principal
}

func (resolver currentPrincipal) Resolve(context.Context, string) (identity.Principal, error) {
	return resolver.principal, nil
}

// seedBinding writes one resource binding row through the neutral seam.
func seedBinding(t *testing.T, backend *memory.Store, scope tenancy.Scope, identity string, resourceID resource.ResourceID, serviceID service.ServiceID, version uint64) {
	t.Helper()
	operations, err := jsonMarshal([]string{string(resource.OperationGitRefUpdate)})
	if err != nil {
		t.Fatalf("encode operations: %v", err)
	}
	origins, err := jsonMarshal([]string{})
	if err != nil {
		t.Fatalf("encode origins: %v", err)
	}
	reportable, err := jsonMarshal([]string{})
	if err != nil {
		t.Fatalf("encode reportable events: %v", err)
	}
	metadata, err := jsonMarshal(map[string]string{})
	if err != nil {
		t.Fatalf("encode metadata: %v", err)
	}
	if _, err := backend.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "resource_bindings",
			ID:     identity,
			Insert: true,
			Values: map[string]any{
				"resource_id":       string(resourceID),
				"service_id":        string(serviceID),
				"origins":           origins,
				"operations":        operations,
				"reportable_events": reportable,
				"metadata":          metadata,
				"version":           int64(version),
				"created_at":        testNow,
			},
		}},
	}); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
}

// TestBindingReaderReadsTheCurrentBinding proves the store-neutral reader
// decodes the binding and reports absence and tenant-scope misuse.
func TestBindingReaderReadsTheCurrentBinding(t *testing.T) {
	backend := newStore()
	scope := testScope()
	seedBinding(t, backend, scope, "binding-1", "resource-1", "service-1", 7)
	reader, err := readers.NewBindingReader(backend)
	if err != nil {
		t.Fatalf("NewBindingReader: %v", err)
	}
	binding, err := reader.ReadBinding(context.Background(), verify.BindingQuery{
		Scope:      scope,
		ResourceID: "resource-1",
		ServiceID:  "service-1",
	})
	if err != nil {
		t.Fatalf("ReadBinding: %v", err)
	}
	if binding.ID != "binding-1" || binding.ResourceID != "resource-1" || binding.EnforcingService != "service-1" {
		t.Fatalf("binding = %+v", binding)
	}
	if binding.Scope != scope || binding.Version != 7 {
		t.Fatalf("binding scope/version = %+v/%d; want %+v/7", binding.Scope, binding.Version, scope)
	}
	if len(binding.Operations) != 1 || binding.Operations[0] != resource.OperationGitRefUpdate {
		t.Fatalf("binding operations = %+v", binding.Operations)
	}
	if _, err := reader.ReadBinding(context.Background(), verify.BindingQuery{
		Scope:      otherScope(),
		ResourceID: "resource-1",
		ServiceID:  "service-1",
	}); !isClass(err, fault.NotFound) {
		t.Fatalf("cross-scope binding error = %v; want not_found", err)
	}
	if _, err := reader.ReadBinding(context.Background(), verify.BindingQuery{
		Scope:      tenancy.Scope{TenantID: testTenant},
		ResourceID: "resource-1",
		ServiceID:  "service-1",
	}); !isClass(err, fault.InvalidInput) {
		t.Fatalf("tenant-scope binding error = %v; want invalid_input", err)
	}
}

// TestRealVerifierUsesTheStoreNeutralBindingReader drives the real verifier onto
// the binding reader and the real record adapter.
func TestRealVerifierUsesTheStoreNeutralBindingReader(t *testing.T) {
	backend := newStore()
	scope := testScope()
	seedService(t, backend, scope, "service-1", 1, []tenancy.Role{tenancy.RoleEngineer}, nil)
	seedMembership(t, backend, scope, "membership-actor", testPrincipal, 1, tenancy.RoleEngineer)
	seedBinding(t, backend, scope, "binding-1", "resource-1", "service-1", 7)
	bindings, err := readers.NewBindingReader(backend)
	if err != nil {
		t.Fatalf("NewBindingReader: %v", err)
	}
	evidence, err := platform.NewEvidenceReader(backend)
	if err != nil {
		t.Fatalf("NewEvidenceReader: %v", err)
	}
	records, err := platform.NewRecordAdapter(backend, backend)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	actor := identity.Principal{
		ID:      testPrincipal,
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	}
	verifier, err := verify.New(verify.Config{
		Principals: currentPrincipal{principal: actor},
		Bindings:   bindings,
		Evidence:   evidence,
		Records:    records,
		Clock:      serviceClock{},
		NewID:      func() (verify.VerificationID, error) { return "verification-1", nil },
	})
	if err != nil {
		t.Fatalf("verify.New: %v", err)
	}
	attempt := verify.Attempt{
		Scope:      scope,
		Service:    identity.Principal{ID: "service-1", Kind: identity.KindService, Scope: scope, Roles: []tenancy.Role{tenancy.RoleEngineer}, Enabled: true, Version: 1},
		Credential: "presented-actor-credential",
		ResourceID: "resource-1",
		Operation:  resource.OperationGitRefUpdate,
	}
	record, err := verifier.Verify(context.Background(), attempt)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if record.Result != verify.ResultApproved {
		t.Fatalf("record = %+v; want an approved (allowed) fact", record)
	}
	if record.Approved {
		t.Fatalf("record = %+v; no approval was requested, so no approval may be recorded", record)
	}
	if record.BindingID != "binding-1" || record.ResourceID != "resource-1" || record.Operation != string(resource.OperationGitRefUpdate) {
		t.Fatalf("record linkage = %+v", record)
	}
	row, ok := backend.Snapshot(scope, "verifications", string(record.ID))
	if !ok {
		t.Fatal("verification row was not committed")
	}
	if row.Values["binding_id"] != "binding-1" || row.Values["service_id"] != "service-1" {
		t.Fatalf("stored verification = %+v", row.Values)
	}
	for name, value := range row.Values {
		if text, ok := value.(string); ok && strings.Contains(text, attempt.Credential) {
			t.Fatalf("verification column %q contains the presented credential", name)
		}
	}
	if strings.Contains(string(record.Actor.ID), attempt.Credential) {
		t.Fatal("verification record contains the presented credential")
	}

	// A resource with no binding is refused before any record is written.
	if _, err := verifier.Verify(context.Background(), verify.Attempt{
		Scope:      scope,
		Service:    attempt.Service,
		Credential: attempt.Credential,
		ResourceID: "resource-missing",
		Operation:  resource.OperationGitRefUpdate,
	}); !isClass(err, fault.NotFound) {
		t.Fatalf("missing-binding error = %v; want not_found", err)
	}
	page, err := backend.Query(context.Background(), scope, store.ScanQuery{Table: "verifications", Limit: store.MaxQueryLimit})
	if err != nil {
		t.Fatalf("scan verifications: %v", err)
	}
	if len(page.Rows) != 1 {
		t.Fatalf("verification rows = %d; a refused binding read must not write a fact", len(page.Rows))
	}
}
