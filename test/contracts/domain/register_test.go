package domaincontract

// resource.binding.register authority.
//
// The registry row requires `membership` and `resource_ownership`, and the
// `resource_ownership` read is the executor read of the `resources` row:
// absent means NotFound. Registration is the operation that creates that row,
// so its read must accept absence. The rule:
//
//   - resource.binding.register CREATES the `resources` row when it is absent
//     (identity = ResourceID, owner = the committing scope, version 1) in the
//     same atomic unit, and its `resource_ownership` read is satisfied when the
//     row is absent or owned by exactly this scope;
//   - a row owned by another scope is refused as the uniform `not_found` class,
//     so registration is no existence oracle;
//   - update, delete, read and report keep requiring an existing row owned by
//     exactly this scope; absent and foreign are both `not_found`.
//
// The declared rule is executable in internal/shell/resource:
//
//	resource.RegisterResourceRow(scope, resourceID, existing *ResourceRow)
//	    (ResourceRow, bool, error)   // bool reports "created"
//	resource.RequireOwnedResourceRow(scope, resourceID, existing *ResourceRow)
//	    (ResourceRow, error)
//
// These vectors pin the declarations with a reference fake; no store,
// PostgreSQL adapter or process is involved.

import (
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

var (
	bindingScope = tenancy.Scope{
		TenantID:  id.TenantID("tenant-1"),
		ProjectID: id.ProjectID("project-1"),
	}
	registerOtherProjectScope = tenancy.Scope{
		TenantID:  id.TenantID("tenant-1"),
		ProjectID: id.ProjectID("project-2"),
	}
	tenantWideScope          = tenancy.Scope{TenantID: id.TenantID("tenant-1")}
	registerOtherTenantScope = tenancy.Scope{
		TenantID:  id.TenantID("tenant-2"),
		ProjectID: id.ProjectID("project-1"),
	}

	bindingResource = resource.ResourceID("resource-1")
	bindingIdentity = resource.BindingID("binding-1")
)

func sampleBinding() resource.Binding {
	return resource.Binding{
		ID:               bindingIdentity,
		ResourceID:       bindingResource,
		Scope:            bindingScope,
		EnforcingService: "svc-git",
		Operations:       []resource.Operation{resource.OperationGitRefUpdate},
		ReportableEvents: []string{"git.ref.updated"},
	}
}

// TestRegisterOperationDeclaresResourceOwnership pins the registry rows of the
// ownership rule. Registration and verified reports re-read resource_ownership
// inside the commit; update/delete keep their binding_version row and read
// keeps its membership-only row, because the owned-row requirement for those
// operations is the uniform not_found visibility rule, not an authority check.
func TestRegisterOperationDeclaresResourceOwnership(t *testing.T) {
	for _, name := range []authorize.OperationName{
		"resource.binding.register",
		"resource.report",
	} {
		operation, ok := authorize.LookupOperation(name)
		if !ok {
			t.Fatalf("operation %q is not registered", name)
		}
		if !declaresCheck(operation, authorize.CheckResourceOwnership) {
			t.Fatalf("operation %q does not declare the %q authority read", name, authorize.CheckResourceOwnership)
		}
	}
	for _, name := range []authorize.OperationName{
		"resource.binding.update",
		"resource.binding.delete",
	} {
		operation, ok := authorize.LookupOperation(name)
		if !ok {
			t.Fatalf("operation %q is not registered", name)
		}
		if !declaresCheck(operation, authorize.CheckBindingVersion) {
			t.Fatalf("operation %q no longer declares its binding_version authority read", name)
		}
		if declaresCheck(operation, authorize.CheckResourceOwnership) {
			t.Fatalf("operation %q gained a resource_ownership authority row; the owned-row requirement is the not_found visibility rule", name)
		}
	}
	read, ok := authorize.LookupOperation("resource.binding.read")
	if !ok {
		t.Fatal("operation \"resource.binding.read\" is not registered")
	}
	if declaresCheck(read, authorize.CheckResourceOwnership) {
		t.Fatal("resource.binding.read must not invent a resource_ownership authority check; its absence is the uniform not_found class")
	}
	if !declaresCheck(read, authorize.CheckMembership) {
		t.Fatal("resource.binding.read no longer declares its membership authority read")
	}
}

func declaresCheck(operation authorize.Operation, check authorize.AuthorityCheck) bool {
	for _, declared := range operation.Authority {
		if declared == check {
			return true
		}
	}
	return false
}

// TestRegisterCreatesAbsentResourceRowAtVersionOne pins the create-if-absent
// half: an absent row is created with the resource identity, the committing
// scope and version 1, and a later register of an owned row reuses it.
func TestRegisterCreatesAbsentResourceRowAtVersionOne(t *testing.T) {
	row, created, err := resource.RegisterResourceRow(bindingScope, bindingResource, nil)
	if err != nil {
		t.Fatalf("register with an absent row was refused: %v", err)
	}
	if !created {
		t.Fatal("register with an absent row did not report a created row")
	}
	if row.ID != bindingResource {
		t.Fatalf("created row identity = %q; want %q", row.ID, bindingResource)
	}
	if row.Scope != bindingScope {
		t.Fatalf("created row scope = %#v; want the committing scope %#v", row.Scope, bindingScope)
	}
	if row.Version != 1 {
		t.Fatalf("created row version = %d; want 1", row.Version)
	}

	existing := row
	reused, createdAgain, err := resource.RegisterResourceRow(bindingScope, bindingResource, &existing)
	if err != nil {
		t.Fatalf("register with an owned row was refused: %v", err)
	}
	if createdAgain {
		t.Fatal("register with an owned row reported a second creation")
	}
	if reused != existing {
		t.Fatalf("register with an owned row = %#v; want the existing row %#v", reused, existing)
	}
}

// TestRegisterRefusesForeignResourceRow pins the foreign-owner rule: a row
// owned by any other scope is the uniform not_found class, never forbidden, and
// the foreign row is not modified. "Owned by this scope" is exact equality, so
// a tenant-wide row is not the project row.
func TestRegisterRefusesForeignResourceRow(t *testing.T) {
	foreignScopes := []tenancy.Scope{registerOtherProjectScope, tenantWideScope, registerOtherTenantScope}
	for _, foreign := range foreignScopes {
		existing := resource.ResourceRow{ID: bindingResource, Scope: foreign, Version: 7}
		_, created, err := resource.RegisterResourceRow(bindingScope, bindingResource, &existing)
		if err == nil {
			t.Fatalf("register accepted a row owned by %#v", foreign)
		}
		if created {
			t.Fatalf("refused register reported a created row for owner %#v", foreign)
		}
		class, ok := fault.ClassOf(err)
		if !ok || class != fault.NotFound {
			t.Fatalf("foreign-owner refusal class = %v; want %q", class, fault.NotFound)
		}
		if class == fault.Forbidden {
			t.Fatalf("foreign owner %#v was reported forbidden instead of not_found", foreign)
		}
		if existing.Scope != foreign || existing.Version != 7 {
			t.Fatalf("refused register mutated the foreign row: %#v", existing)
		}
	}
}

// TestRequireOwnedResourceRow pins the existing-row rule for update, delete,
// read and report: only an exactly-owned row is visible; absent and foreign are
// the same not_found class.
func TestRequireOwnedResourceRow(t *testing.T) {
	owned := resource.ResourceRow{ID: bindingResource, Scope: bindingScope, Version: 3}
	row, err := resource.RequireOwnedResourceRow(bindingScope, bindingResource, &owned)
	if err != nil {
		t.Fatalf("owned row was refused: %v", err)
	}
	if row != owned {
		t.Fatalf("owned row = %#v; want %#v", row, owned)
	}

	_, absentErr := resource.RequireOwnedResourceRow(bindingScope, bindingResource, nil)
	absentClass, ok := fault.ClassOf(absentErr)
	if !ok || absentClass != fault.NotFound {
		t.Fatalf("absent row class = %v; want %q", absentClass, fault.NotFound)
	}

	foreign := resource.ResourceRow{ID: bindingResource, Scope: registerOtherProjectScope, Version: 3}
	_, foreignErr := resource.RequireOwnedResourceRow(bindingScope, bindingResource, &foreign)
	foreignClass, ok := fault.ClassOf(foreignErr)
	if !ok || foreignClass != fault.NotFound {
		t.Fatalf("foreign row class = %v; want %q", foreignClass, fault.NotFound)
	}
	if absentClass != foreignClass {
		t.Fatalf("absent class %q differs from foreign class %q; the read is an existence oracle", absentClass, foreignClass)
	}
}

// TestOwnershipRulesRefuseInvalidValues pins that both declared rules reject an
// invalid scope or resource identity before any decision.
func TestOwnershipRulesRefuseInvalidValues(t *testing.T) {
	cases := []struct {
		name       string
		scope      tenancy.Scope
		resourceID resource.ResourceID
	}{
		{name: "empty scope", scope: tenancy.Scope{}, resourceID: bindingResource},
		{name: "invalid tenant", scope: tenancy.Scope{TenantID: id.TenantID("bad/id")}, resourceID: bindingResource},
		{name: "empty resource identity", scope: bindingScope, resourceID: ""},
		{name: "invalid resource identity", scope: bindingScope, resourceID: resource.ResourceID("bad/id")},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, _, err := resource.RegisterResourceRow(testCase.scope, testCase.resourceID, nil); !isInvalidInput(err) {
				t.Fatalf("RegisterResourceRow class = %v; want %q", classOf(err), fault.InvalidInput)
			}
			if _, err := resource.RequireOwnedResourceRow(testCase.scope, testCase.resourceID, nil); !isInvalidInput(err) {
				t.Fatalf("RequireOwnedResourceRow class = %v; want %q", classOf(err), fault.InvalidInput)
			}
		})
	}
}

func classOf(err error) fault.Class {
	class, _ := fault.ClassOf(err)
	return class
}

func isInvalidInput(err error) bool {
	class, ok := fault.ClassOf(err)
	return ok && class == fault.InvalidInput
}

// ownershipStore is the reference fake: it applies the declared rule to an
// in-memory resources/bindings pair. It is not an adapter and proves no SQL; it
// pins the behavior a store adapter must implement for the same operations.
type ownershipStore struct {
	resources map[resource.ResourceID]resource.ResourceRow
	bindings  map[resource.BindingID]resource.Binding
}

func newOwnershipStore() *ownershipStore {
	return &ownershipStore{
		resources: map[resource.ResourceID]resource.ResourceRow{},
		bindings:  map[resource.BindingID]resource.Binding{},
	}
}

func (store *ownershipStore) existingRow(resourceID resource.ResourceID) *resource.ResourceRow {
	row, ok := store.resources[resourceID]
	if !ok {
		return nil
	}
	return &row
}

// register is the resource.binding.register path: the row is created when
// absent, in the same unit as the binding write.
func (store *ownershipStore) register(scope tenancy.Scope, binding resource.Binding) (resource.BindingResult, error) {
	row, created, err := resource.RegisterResourceRow(scope, binding.ResourceID, store.existingRow(binding.ResourceID))
	if err != nil {
		return resource.BindingResult{}, err
	}
	store.resources[row.ID] = row
	stored := binding
	stored.Scope = scope
	if created {
		stored.Version = 1
	} else {
		current, ok := store.bindings[stored.ID]
		if !ok {
			return resource.BindingResult{}, fault.New(fault.NotFound, "binding is not visible in this scope")
		}
		stored.Version = current.Version + 1
	}
	store.bindings[stored.ID] = stored
	return resource.BindingResult{Version: stored.Version}, nil
}

// update is the resource.binding.update path: it requires an existing owned
// row and never creates one.
func (store *ownershipStore) update(scope tenancy.Scope, binding resource.Binding) (resource.BindingResult, error) {
	if _, err := resource.RequireOwnedResourceRow(scope, binding.ResourceID, store.existingRow(binding.ResourceID)); err != nil {
		return resource.BindingResult{}, err
	}
	current, ok := store.bindings[binding.ID]
	if !ok {
		return resource.BindingResult{}, fault.New(fault.NotFound, "binding is not visible in this scope")
	}
	stored := binding
	stored.Scope = scope
	stored.Version = current.Version + 1
	store.bindings[stored.ID] = stored
	return resource.BindingResult{Version: stored.Version}, nil
}

// deleteBinding is the resource.binding.delete path.
func (store *ownershipStore) deleteBinding(scope tenancy.Scope, bindingID resource.BindingID) error {
	binding, ok := store.bindings[bindingID]
	if !ok {
		return fault.New(fault.NotFound, "binding is not visible in this scope")
	}
	if _, err := resource.RequireOwnedResourceRow(scope, binding.ResourceID, store.existingRow(binding.ResourceID)); err != nil {
		return err
	}
	delete(store.bindings, bindingID)
	return nil
}

// readBinding is the resource.binding.read path.
func (store *ownershipStore) readBinding(scope tenancy.Scope, bindingID resource.BindingID) (resource.Binding, error) {
	binding, ok := store.bindings[bindingID]
	if !ok {
		return resource.Binding{}, fault.New(fault.NotFound, "binding is not visible in this scope")
	}
	if _, err := resource.RequireOwnedResourceRow(scope, binding.ResourceID, store.existingRow(binding.ResourceID)); err != nil {
		return resource.Binding{}, err
	}
	return binding, nil
}

// report is the resource.report path: the ownership read must resolve an
// existing owned row before the report is accepted.
func (store *ownershipStore) report(scope tenancy.Scope, bindingID resource.BindingID) error {
	binding, ok := store.bindings[bindingID]
	if !ok {
		return fault.New(fault.NotFound, "binding is not visible in this scope")
	}
	_, err := resource.RequireOwnedResourceRow(scope, binding.ResourceID, store.existingRow(binding.ResourceID))
	return err
}

// TestOwnershipReferenceStore pins the observable end-to-end rule with the
// reference fake: register creates version 1, re-register reuses the row, a
// foreign row is not_found, and update/delete/read/report never create a row.
func TestOwnershipReferenceStore(t *testing.T) {
	store := newOwnershipStore()
	binding := sampleBinding()

	result, err := store.register(bindingScope, binding)
	if err != nil {
		t.Fatalf("register with an absent row was refused: %v", err)
	}
	if result.Version != 1 {
		t.Fatalf("registered binding version = %d; want 1", result.Version)
	}
	row, ok := store.resources[bindingResource]
	if !ok {
		t.Fatal("register did not create the resources row")
	}
	if row.ID != bindingResource || row.Scope != bindingScope || row.Version != 1 {
		t.Fatalf("created resources row = %#v; want {%q %#v 1}", row, bindingResource, bindingScope)
	}

	// A second register of the owned resource reuses the version-1 row.
	replayed, err := store.register(bindingScope, binding)
	if err != nil {
		t.Fatalf("second register was refused: %v", err)
	}
	if replayed.Version != 2 {
		t.Fatalf("second register binding version = %d; want 2", replayed.Version)
	}
	row, ok = store.resources[bindingResource]
	if !ok || row.Version != 1 {
		t.Fatalf("second register recreated the resources row: %#v", row)
	}
	if len(store.resources) != 1 {
		t.Fatalf("register left %d resources rows; want 1", len(store.resources))
	}

	// Update, read and report succeed against the owned row.
	if _, err := store.update(bindingScope, binding); err != nil {
		t.Fatalf("update against an owned row was refused: %v", err)
	}
	if _, err := store.readBinding(bindingScope, bindingIdentity); err != nil {
		t.Fatalf("read against an owned row was refused: %v", err)
	}
	if err := store.report(bindingScope, bindingIdentity); err != nil {
		t.Fatalf("report against an owned row was refused: %v", err)
	}
	if err := store.deleteBinding(bindingScope, bindingIdentity); err != nil {
		t.Fatalf("delete against an owned row was refused: %v", err)
	}
}

// TestOwnershipReferenceStoreForeignRow pins that a foreign row blocks
// registration with not_found and stays untouched, and that update, delete,
// read and report refuse it with the same class.
func TestOwnershipReferenceStoreForeignRow(t *testing.T) {
	store := newOwnershipStore()
	store.resources[bindingResource] = resource.ResourceRow{
		ID:      bindingResource,
		Scope:   registerOtherProjectScope,
		Version: 4,
	}
	binding := sampleBinding()

	_, err := store.register(bindingScope, binding)
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("register against a foreign row class = %v; want %q", class, fault.NotFound)
	}
	if _, ok := store.bindings[bindingIdentity]; ok {
		t.Fatal("refused register wrote the binding")
	}
	if row := store.resources[bindingResource]; row.Scope != registerOtherProjectScope || row.Version != 4 {
		t.Fatalf("refused register mutated the foreign row: %#v", row)
	}

	store.bindings[bindingIdentity] = binding
	for name, call := range map[string]func() error{
		"update": func() error { _, err := store.update(bindingScope, binding); return err },
		"delete": func() error { return store.deleteBinding(bindingScope, bindingIdentity) },
		"read":   func() error { _, err := store.readBinding(bindingScope, bindingIdentity); return err },
		"report": func() error { return store.report(bindingScope, bindingIdentity) },
	} {
		if class, ok := fault.ClassOf(call()); !ok || class != fault.NotFound {
			t.Fatalf("%s against a foreign row class = %v; want %q", name, class, fault.NotFound)
		}
	}
}

// TestOwnershipReferenceStoreAbsentRow pins that the existing-row operations
// never create the row: an absent row is not_found for update, delete, read and
// report, and the resources table stays empty.
func TestOwnershipReferenceStoreAbsentRow(t *testing.T) {
	store := newOwnershipStore()
	binding := sampleBinding()
	// The binding exists but its resources row does not.
	store.bindings[bindingIdentity] = binding

	for name, call := range map[string]func() error{
		"update": func() error { _, err := store.update(bindingScope, binding); return err },
		"delete": func() error { return store.deleteBinding(bindingScope, bindingIdentity) },
		"read":   func() error { _, err := store.readBinding(bindingScope, bindingIdentity); return err },
		"report": func() error { return store.report(bindingScope, bindingIdentity) },
	} {
		err := call()
		class, ok := fault.ClassOf(err)
		if !ok || class != fault.NotFound {
			t.Fatalf("%s against an absent row class = %v; want %q", name, class, fault.NotFound)
		}
	}
	if len(store.resources) != 0 {
		t.Fatalf("existing-row operations created %d resources rows; only register may create one", len(store.resources))
	}
}

// TestRegisterAuthorityReadAcceptsAbsence pins the authority-read split:
// absence satisfies register's `resource_ownership` read
// because the register decision rule replaces the executor read, while
// update/delete/read/report keep the existing-row rule and report the same
// not_found class for an absent row.
func TestRegisterAuthorityReadAcceptsAbsence(t *testing.T) {
	row, created, err := resource.RegisterResourceRow(bindingScope, bindingResource, nil)
	if err != nil {
		t.Fatalf("register authority read refused an absent row: %v", err)
	}
	if !created {
		t.Fatal("register authority read accepted an absent row but did not decide the insert")
	}
	if row.ID != bindingResource || row.Scope != bindingScope || row.Version != 1 {
		t.Fatalf("register decision = %#v; want {%q %#v 1}", row, bindingResource, bindingScope)
	}

	_, absentErr := resource.RequireOwnedResourceRow(bindingScope, bindingResource, nil)
	absentClass, ok := fault.ClassOf(absentErr)
	if !ok || absentClass != fault.NotFound {
		t.Fatalf("update/delete/read/report absent-row class = %v; want %q", absentClass, fault.NotFound)
	}
	if absentClass == fault.VersionConflict {
		t.Fatal("the existing-row rule surfaced an absent row as version_conflict")
	}
}

// guardedResourceTable simulates the `resources` primary-key guard: a
// conditional insert succeeds only when the row is absent and otherwise
// returns the row that won, never a conflict fault. It is a reference fake,
// not a SQL adapter; it pins the rule the adapter must implement.
type guardedResourceTable struct {
	rows map[resource.ResourceID]resource.ResourceRow
}

func newGuardedResourceTable() *guardedResourceTable {
	return &guardedResourceTable{rows: map[resource.ResourceID]resource.ResourceRow{}}
}

// conditionalInsert attempts the unique-id insert. It reports the row present
// after the attempt: inserted=true when this caller won, false when the
// primary-key guard returned the existing winner.
func (table *guardedResourceTable) conditionalInsert(row resource.ResourceRow) (resource.ResourceRow, bool) {
	if winner, ok := table.rows[row.ID]; ok {
		return winner, false
	}
	table.rows[row.ID] = row
	return row, true
}

// register runs one register attempt from an already-captured read: read,
// decision, conditional insert and, on a lost insert, the in-transaction
// re-read and re-application of the decision rule.
func (table *guardedResourceTable) register(scope tenancy.Scope, resourceID resource.ResourceID, read *resource.ResourceRow) (resource.ResourceRow, bool, error) {
	decision, created, err := resource.RegisterResourceRow(scope, resourceID, read)
	if err != nil {
		return resource.ResourceRow{}, false, err
	}
	if !created {
		return decision, false, nil
	}
	winner, won := table.conditionalInsert(decision)
	if won {
		return winner, true, nil
	}
	// The insert hit the primary-key guard: re-read the winner in the same
	// transaction and re-apply the decision rule. The conflict is never
	// surfaced as a fault.
	return resource.ResolveRegisterConflict(scope, resourceID, winner)
}

// TestRegisterConflictWinnerIsFirstCommitter pins the two-scope race. Both
// scopes capture an absent read before either commits; the first insert wins,
// and every loser re-reads the winner in the same transaction. A foreign-scope
// loser gets not_found and leaves the winner row unmodified; a same-scope loser
// reuses the winner row and reports (existing, false). No conflict or
// version_conflict is ever surfaced for the resources row.
func TestRegisterConflictWinnerIsFirstCommitter(t *testing.T) {
	table := newGuardedResourceTable()
	// Both scopes read the row absent before either insert commits. The
	// sequential execution below is deterministic and models the same stale
	// read a barrier would expose.
	winner, created, err := table.register(bindingScope, bindingResource, nil)
	if err != nil {
		t.Fatalf("first register was refused: %v", err)
	}
	if !created || winner.Scope != bindingScope || winner.Version != 1 {
		t.Fatalf("first register = %#v created=%v; want the %#v version-1 winner", winner, created, bindingScope)
	}

	// A foreign scope still holds its absent read and loses the insert.
	foreignRow, foreignCreated, foreignErr := table.register(registerOtherProjectScope, bindingResource, nil)
	if foreignCreated {
		t.Fatal("the foreign-scope loser reported a created row")
	}
	if foreignErr == nil {
		t.Fatal("the foreign-scope loser was not refused")
	}
	class, ok := fault.ClassOf(foreignErr)
	if !ok || class != fault.NotFound {
		t.Fatalf("foreign-scope loser class = %v; want %q", class, fault.NotFound)
	}
	if class == fault.VersionConflict {
		t.Fatal("the resources-row conflict was surfaced as version_conflict")
	}
	if foreignRow != (resource.ResourceRow{}) {
		t.Fatalf("foreign-scope loser returned a row: %#v", foreignRow)
	}
	if stored := table.rows[bindingResource]; stored != winner {
		t.Fatalf("foreign-scope loser modified the winner row: got %#v; want %#v", stored, winner)
	}

	// A same-scope caller with the same stale absent read loses the insert and
	// reuses the winner row.
	reused, reusedCreated, err := table.register(bindingScope, bindingResource, nil)
	if err != nil {
		t.Fatalf("same-scope loser was refused: %v", err)
	}
	if reusedCreated {
		t.Fatal("same-scope loser reported a created row")
	}
	if reused != winner {
		t.Fatalf("same-scope loser row = %#v; want the winner %#v", reused, winner)
	}
	if len(table.rows) != 1 {
		t.Fatalf("the race left %d resources rows; want 1", len(table.rows))
	}
}

// TestResolveRegisterConflictValidatesWinner pins that the conflict helper
// applies the same input checks as the decision rule before it decides.
func TestResolveRegisterConflictValidatesWinner(t *testing.T) {
	cases := []struct {
		name   string
		winner resource.ResourceRow
	}{
		{
			name:   "id mismatch",
			winner: resource.ResourceRow{ID: resource.ResourceID("resource-2"), Scope: bindingScope, Version: 1},
		},
		{
			name:   "zero version",
			winner: resource.ResourceRow{ID: bindingResource, Scope: bindingScope},
		},
		{
			name:   "invalid scope",
			winner: resource.ResourceRow{ID: bindingResource, Scope: tenancy.Scope{TenantID: id.TenantID("bad/id")}, Version: 1},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, _, err := resource.ResolveRegisterConflict(bindingScope, bindingResource, testCase.winner); !isInvalidInput(err) {
				t.Fatalf("ResolveRegisterConflict class = %v; want %q", classOf(err), fault.InvalidInput)
			}
		})
	}
}
