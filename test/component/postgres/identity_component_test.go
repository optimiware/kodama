//go:build component

package postgrescomponent

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	identityunits "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

type identityIDs struct {
	mu      sync.Mutex
	next    int
	event   int
	receipt int
}

func (ids *identityIDs) NewRowID() (string, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.next++
	return fmt.Sprintf("row-%d", ids.next), nil
}

func (ids *identityIDs) NewEventID() (id.EventID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.event++
	return id.EventID(fmt.Sprintf("event-%d", ids.event)), nil
}

func (ids *identityIDs) NewReceiptID() (id.ReceiptID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.receipt++
	return id.ReceiptID(fmt.Sprintf("receipt-%d", ids.receipt)), nil
}

func humanAdmin(scope tenancy.Scope, principal string) authorize.Principal {
	return authorize.Principal{
		ID: id.PrincipalID(principal), Kind: identity.KindHuman, Scope: scope,
		Roles: []tenancy.Role{tenancy.RoleAdmin}, Enabled: true,
	}
}

func serviceForScope(scope tenancy.Scope) string {
	if scope.ProjectID == "project-2" {
		return "service-b"
	}
	return "service-a"
}

// bindingContent is the request content every binding command of this file
// submits for one caller key.
func bindingContent(callerKey string) receipt.Content {
	return receipt.Content{MediaType: "application/json", Bytes: []byte(`{"caller":"` + callerKey + `"}`)}
}

func registerCommand(scope tenancy.Scope, actor, bindingID, resourceID, callerKey string) resource.RegisterBindingCommand {
	return resource.RegisterBindingCommand{
		Actor: humanAdmin(scope, actor), Scope: scope,
		BindingID: resource.BindingID(bindingID), ResourceID: resource.ResourceID(resourceID),
		EnforcingService: service.ServiceID(serviceForScope(scope)),
		TrustedOrigins:   []string{"https://git.example"},
		Operations:       []resource.Operation{resource.OperationGitRefUpdate},
		ReportableEvents: []string{"git.ref.updated"},
		Metadata:         map[string]string{"repo": "kodama"},
		CallerKey:        callerKey,
		Content:          bindingContent(callerKey),
	}
}

type resourceEnv struct {
	shard   *pgtest.Shard
	runtime *pgdriver.DB
	service *resource.Service
}

func newResourceEnv(t *testing.T) *resourceEnv {
	t.Helper()
	shard := migratedShard(t)
	ctx := context.Background()
	shard.SeedTenant(ctx, "tenant-1", 1)
	for _, scope := range []tenancy.Scope{
		pgtest.Scope("tenant-1", "project-1"),
		pgtest.Scope("tenant-1", "project-2"),
	} {
		shard.SeedProject(ctx, string(scope.TenantID), string(scope.ProjectID))
		shard.SeedMembership(ctx, scope, "membership-"+string(scope.ProjectID), "principal-1", 1, string(tenancy.RoleAdmin))
		shard.SeedService(ctx, scope, serviceForScope(scope), serviceForScope(scope), string(tenancy.RoleEngineer))
		if string(scope.ProjectID) == "project-1" {
			shard.SeedService(ctx, scope, "service-c", "service-c", string(tenancy.RoleEngineer))
		}
	}
	runtime := shard.Runtime(6)
	t.Cleanup(runtime.Close)
	scoped := storepg.New(runtime)
	adapter, err := identityunits.New(identityunits.Config{
		Executor: scoped, Scanner: scoped,
		Clock: integrationClock{now: pgtest.FixtureTime}, IDs: &identityIDs{},
	})
	if err != nil {
		t.Fatalf("identity.New: %v", err)
	}
	service, err := resource.NewService(resource.Config{
		Clock: integrationClock{now: pgtest.FixtureTime}, IDs: &identityIDs{},
		Bindings: adapter, Deletes: adapter, Reads: adapter, Queries: adapter,
	})
	if err != nil {
		t.Fatalf("resource.NewService: %v", err)
	}
	return &resourceEnv{shard: shard, runtime: runtime, service: service}
}

// TestResourceRegisterCreateIfAbsentOnPostgreSQL proves the register rule on
// real PostgreSQL: the first registration creates the resources row, a
// same-scope re-register reuses it, and a foreign scope gets the uniform
// not_found without writing a binding.
func TestResourceRegisterCreateIfAbsentOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newResourceEnv(t)
	scope1 := pgtest.Scope("tenant-1", "project-1")
	scope2 := pgtest.Scope("tenant-1", "project-2")

	registerObserver := env.shard.ObserveUnit(t, "identity", "binding.register")
	first, err := env.service.RegisterBinding(ctx, registerCommand(scope1, "principal-1", "binding-1", "resource-1", "register-1"))
	if err != nil {
		t.Fatalf("first register: %v%s", err, pgtest.CauseText(err))
	}
	if first.Version != 1 {
		t.Fatalf("first register version = %d; want 1", first.Version)
	}
	env.shard.RequireGoldenRow(t, ctx, env.runtime, scope1, "resources", map[string]any{"id": "resource-1"}, map[string]any{
		"id": "resource-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"kind": "resource", "version": int64(1), "metadata": `{}`, "created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, scope1, "resource_bindings", map[string]any{"id": "binding-1"}, map[string]any{
		"id": "binding-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"resource_id": "resource-1", "service_id": "service-a",
		"origins": `["https://git.example"]`, "operations": `["git.ref.update"]`,
		"reportable_events": `["git.ref.updated"]`, "metadata": `{"repo":"kodama"}`,
		"version": int64(1), "created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, scope1, "receipts", map[string]any{"id": "receipt-1"}, map[string]any{
		"id": "receipt-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "operation": "resource.binding.register", "caller_key": "register-1",
		"content_sha256": pgtest.ContentSHA256(t, bindingContent("register-1")),
		"result":         pgtest.GoldenJSONOf(resource.BindingResult{Version: 1}),
		"completed_at":   pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, scope1, "audit", map[string]any{"id": "event-1"}, map[string]any{
		"id": "event-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "resource.binding.register", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, scope1, "events", map[string]any{"id": "event-2"}, map[string]any{
		"id": "event-2", "tenant_id": "tenant-1", "project_id": "project-1",
		"type": "config.changed", "report_type": pgtest.NullValue,
		"subject_id": "binding-1", "subject_version": int64(1),
		"source_kind": "shell", "source_principal": "principal-1",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	registerObserver.Finish(map[string]int{"resources": 1, "resource_bindings": 1, "receipts": 1, "audit": 1, "events": 1})
	count, err := pgtest.ScalarInt(ctx, env.shard.Admin(), `SELECT count(*) FROM resources WHERE id = 'resource-1'`)
	if err != nil || count != 1 {
		t.Fatalf("resources rows = %d, err=%v; want 1", count, err)
	}
	owner, err := pgtest.ScalarString(ctx, env.shard.Admin(), `SELECT tenant_id || '/' || project_id FROM resources WHERE id = 'resource-1'`)
	if err != nil || owner != "tenant-1/project-1" {
		t.Fatalf("resources owner = %q, err=%v; want tenant-1/project-1", owner, err)
	}
	resourceRow := scopedRow(t, ctx, env.runtime, scope1, 3,
		`SELECT version::text, kind, metadata::text FROM resources WHERE id = 'resource-1'`)
	if resourceRow[0] != "1" || resourceRow[1] != "resource" || !jsonEqual(resourceRow[2], `{}`) {
		t.Fatalf("durable resource row = %v; want version 1/resource/{}", resourceRow)
	}
	env.shard.RequireGoldenRow(t, ctx, env.runtime, scope1, "resources", map[string]any{"id": "resource-1"}, map[string]any{
		"id": "resource-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"kind": "resource", "version": int64(1), "metadata": `{}`, "created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, scope1, "resource_bindings", map[string]any{"id": "binding-1"}, map[string]any{
		"id": "binding-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"resource_id": "resource-1", "service_id": "service-a",
		"origins": `["https://git.example"]`, "operations": `["git.ref.update"]`,
		"reportable_events": `["git.ref.updated"]`, "metadata": `{"repo":"kodama"}`,
		"version": int64(1), "created_at": pgtest.FixtureTime,
	})

	// Same scope re-registers the same resource under another binding: the
	// resources row is reused, not duplicated.
	reregister := registerCommand(scope1, "principal-1", "binding-2", "resource-1", "register-2")
	reregister.EnforcingService = "service-c"
	reregistered, err := env.service.RegisterBinding(ctx, reregister)
	if err != nil {
		t.Fatalf("same-scope re-register: %v%s", err, pgtest.CauseText(err))
	}
	if reregistered != (resource.BindingResult{Version: 1}) {
		t.Fatalf("same-scope re-register result = %+v; want version 1, not replayed", reregistered)
	}
	count, _ = pgtest.ScalarInt(ctx, env.shard.Admin(), `SELECT count(*) FROM resources WHERE id = 'resource-1'`)
	if count != 1 {
		t.Fatalf("resources rows after re-register = %d; want 1", count)
	}

	// A foreign scope cannot reuse the resource row: uniform not_found, no
	// binding and no second resources row.
	_, err = env.service.RegisterBinding(ctx, registerCommand(scope2, "principal-1", "binding-3", "resource-1", "register-3"))
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("foreign-scope register error = %v; want not_found", err)
	}
	if count, _ := pgtest.ScalarInt(ctx, env.shard.Admin(), `SELECT count(*) FROM resource_bindings WHERE id = 'binding-3'`); count != 0 {
		t.Fatal("foreign-scope register committed a binding")
	}

	// Read and update keep requiring the existing owned row. The read must
	// return the durable binding identity, scope and decoded column values, not
	// merely succeed.
	binding, err := env.service.ReadBinding(ctx, resource.ReadBindingCommand{Actor: humanAdmin(scope1, "principal-1"), Scope: scope1, BindingID: "binding-1"})
	if err != nil {
		t.Fatalf("read registered binding: %v", err)
	}
	if binding.ID != "binding-1" || binding.ResourceID != "resource-1" || binding.Scope != scope1 ||
		binding.EnforcingService != "service-a" || binding.Version != 1 {
		t.Fatalf("read binding = %+v; want binding-1/resource-1/service-a at version 1 in tenant-1/project-1", binding)
	}
	if len(binding.TrustedOrigins) != 1 || binding.TrustedOrigins[0] != "https://git.example" ||
		len(binding.Operations) != 1 || binding.Operations[0] != resource.OperationGitRefUpdate ||
		len(binding.ReportableEvents) != 1 || binding.ReportableEvents[0] != "git.ref.updated" ||
		binding.Metadata["repo"] != "kodama" {
		t.Fatalf("read binding decoded values = %+v; want the seeded origins/operations/events/metadata", binding)
	}
	update := resource.UpdateBindingCommand{
		Actor: humanAdmin(scope1, "principal-1"), Scope: scope1,
		BindingID: "binding-1", ResourceID: "resource-1", EnforcingService: service.ServiceID("service-a"),
		TrustedOrigins: []string{"https://git.example"}, Operations: []resource.Operation{resource.OperationGitRefUpdate},
		ReportableEvents: []string{"git.ref.updated"}, Metadata: map[string]string{"repo": "changed"},
		ExpectedVersion: 1, CallerKey: "update-1",
		Content: bindingContent("update-1"),
	}
	bindingUpdateObserver := env.shard.ObserveUnit(t, "identity", "binding.update")
	updated, err := env.service.UpdateBinding(ctx, update)
	if err != nil || updated.Version != 2 {
		t.Fatalf("update binding: %v (version=%d)", err, updated.Version)
	}
	env.shard.RequireGoldenRow(t, ctx, env.runtime, scope1, "resource_bindings", map[string]any{"id": "binding-1"}, map[string]any{
		"id": "binding-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"resource_id": "resource-1", "service_id": "service-a",
		"origins": `["https://git.example"]`, "operations": `["git.ref.update"]`,
		"reportable_events": `["git.ref.updated"]`, "metadata": `{"repo":"changed"}`,
		"version": int64(2), "created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, scope1, "receipts", map[string]any{"id": "receipt-4"}, map[string]any{
		"id": "receipt-4", "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "operation": "resource.binding.update", "caller_key": "update-1",
		"content_sha256": pgtest.ContentSHA256(t, bindingContent("update-1")),
		"result":         pgtest.GoldenJSONOf(resource.BindingResult{Version: 2}),
		"completed_at":   pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, scope1, "audit", map[string]any{"id": "event-7"}, map[string]any{
		"id": "event-7", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "resource.binding.update", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, scope1, "events", map[string]any{"id": "event-8"}, map[string]any{
		"id": "event-8", "tenant_id": "tenant-1", "project_id": "project-1",
		"type": "config.changed", "report_type": pgtest.NullValue,
		"subject_id": "binding-1", "subject_version": int64(2),
		"source_kind": "shell", "source_principal": "principal-1",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	bindingUpdateObserver.Finish(map[string]int{"resource_bindings": 1, "receipts": 1, "audit": 1, "events": 1})
	deleteObserver := env.shard.ObserveUnit(t, "identity", "binding.delete")
	deleted, err := env.service.DeleteBinding(ctx, resource.DeleteBindingCommand{
		Actor: humanAdmin(scope1, "principal-1"), Scope: scope1, BindingID: "binding-1",
		ExpectedVersion: 2, CallerKey: "delete-1",
		Content: bindingContent("delete-1"),
	})
	if err != nil {
		t.Fatalf("delete binding: %v", err)
	}
	if deleted != (resource.BindingDeleteResult{Version: 3}) {
		t.Fatalf("delete binding result = %+v; want version 3, not replayed", deleted)
	}
	env.shard.RequireRowAbsent(t, ctx, env.runtime, scope1, "resource_bindings", map[string]any{"id": "binding-1"})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, scope1, "receipts", map[string]any{"id": "receipt-5"}, map[string]any{
		"id": "receipt-5", "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "operation": "resource.binding.delete", "caller_key": "delete-1",
		"content_sha256": pgtest.ContentSHA256(t, bindingContent("delete-1")),
		"result":         pgtest.GoldenJSONOf(resource.BindingDeleteResult{Version: 3}),
		"completed_at":   pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, scope1, "audit", map[string]any{"id": "event-9"}, map[string]any{
		"id": "event-9", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "resource.binding.delete", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, scope1, "events", map[string]any{"id": "event-10"}, map[string]any{
		"id": "event-10", "tenant_id": "tenant-1", "project_id": "project-1",
		"type": "config.changed", "report_type": pgtest.NullValue,
		"subject_id": "binding-1", "subject_version": int64(3),
		"source_kind": "shell", "source_principal": "principal-1",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	deleteObserver.Finish(map[string]int{"resource_bindings": 1, "receipts": 1, "audit": 1, "events": 1})
}

// TestResourceRegisterTwoScopeRaceOnPostgreSQL races two registrations of the
// same resource from different projects. PostgreSQL's resources primary key
// admits one insert; the loser must observe the uniform not_found, never the
// underlying unique-violation/version_conflict, and must write no binding.
func TestResourceRegisterTwoScopeRaceOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newResourceEnv(t)
	scope1 := pgtest.Scope("tenant-1", "project-1")
	scope2 := pgtest.Scope("tenant-1", "project-2")

	raceObserver := env.shard.ObserveUnit(t, "identity", "binding.register.conflict")
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for index, scope := range []tenancy.Scope{scope1, scope2} {
		wg.Add(1)
		go func(index int, scope tenancy.Scope) {
			defer wg.Done()
			<-start
			_, errs[index] = env.service.RegisterBinding(ctx, registerCommand(scope, "principal-1",
				fmt.Sprintf("binding-%d", index+1), "resource-race", fmt.Sprintf("register-%d", index+1)))
		}(index, scope)
	}
	close(start)
	wg.Wait()

	successes, notFounds := 0, 0
	for _, err := range errs {
		if err == nil {
			successes++
			continue
		}
		class, ok := fault.ClassOf(err)
		if !ok || class == fault.VersionConflict || class == fault.InternalFailure {
			t.Fatalf("loser error = %v; want uniform not_found, not conflict/internal", err)
		}
		if class != fault.NotFound {
			t.Fatalf("loser error class = %q; want not_found", class)
		}
		notFounds++
	}
	if successes != 1 || notFounds != 1 {
		t.Fatalf("successes=%d not_found=%d; want exactly one each", successes, notFounds)
	}
	resources, err := pgtest.ScalarInt(ctx, env.shard.Admin(), `SELECT count(*) FROM resources WHERE id = 'resource-race'`)
	if err != nil || resources != 1 {
		t.Fatalf("resources rows = %d, err=%v; want 1", resources, err)
	}
	bindings, err := pgtest.ScalarInt(ctx, env.shard.Admin(), `SELECT count(*) FROM resource_bindings WHERE resource_id = 'resource-race'`)
	if err != nil || bindings != 1 {
		t.Fatalf("bindings = %d, err=%v; want exactly one committed", bindings, err)
	}
	// The durable winner is visible to the application role, and the loser's
	// binding is absent. A tenant-wide read covers either winning project.
	tenant := pgtest.TenantScope("tenant-1")
	requireScopedCount(t, ctx, env.runtime, tenant,
		`SELECT count(*) FROM resources WHERE id = 'resource-race'`, 1, "durable resources")
	requireScopedCount(t, ctx, env.runtime, tenant,
		`SELECT count(*) FROM resource_bindings WHERE resource_id = 'resource-race'`, 1, "durable bindings")
	winnerIndex := 0
	if errs[0] != nil {
		winnerIndex = 1
	}
	winnerScope := []tenancy.Scope{scope1, scope2}[winnerIndex]
	winnerBinding := fmt.Sprintf("binding-%d", winnerIndex+1)
	winnerCaller := fmt.Sprintf("register-%d", winnerIndex+1)
	env.shard.RequireGoldenRow(t, ctx, env.runtime, winnerScope, "resources", map[string]any{"id": "resource-race"}, map[string]any{
		"id": "resource-race", "tenant_id": "tenant-1", "project_id": string(winnerScope.ProjectID),
		"kind": "resource", "version": int64(1), "metadata": `{}`, "created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, winnerScope, "resource_bindings", map[string]any{"id": winnerBinding}, map[string]any{
		"id": winnerBinding, "tenant_id": "tenant-1", "project_id": string(winnerScope.ProjectID),
		"resource_id": "resource-race", "service_id": serviceForScope(winnerScope),
		"origins": `["https://git.example"]`, "operations": `["git.ref.update"]`,
		"reportable_events": `["git.ref.updated"]`, "metadata": `{"repo":"kodama"}`,
		"version": int64(1), "created_at": pgtest.FixtureTime,
	})
	// Both racers allocate ids from one shared counter before either commits,
	// so which receipt/audit/event ids the winner holds depends on goroutine
	// interleaving. Each goroutine draws its receipt id first, then its audit
	// id, then its event id; the winner's value is one of those candidates.
	env.shard.RequireGoldenRow(t, ctx, env.runtime, winnerScope, "receipts", map[string]any{"caller_key": winnerCaller}, map[string]any{
		"id": pgtest.GoldenOneOf("receipt-1", "receipt-2"), "tenant_id": "tenant-1", "project_id": string(winnerScope.ProjectID),
		"principal_id": "principal-1", "operation": "resource.binding.register", "caller_key": winnerCaller,
		"content_sha256": pgtest.ContentSHA256(t, bindingContent(winnerCaller)),
		"result":         pgtest.GoldenJSONOf(resource.BindingResult{Version: 1}),
		"completed_at":   pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, winnerScope, "audit", map[string]any{"operation": "resource.binding.register"}, map[string]any{
		"id": pgtest.GoldenOneOf("event-1", "event-2", "event-3"), "tenant_id": "tenant-1", "project_id": string(winnerScope.ProjectID),
		"actor": "principal-1", "operation": "resource.binding.register", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, winnerScope, "events", map[string]any{"subject_id": winnerBinding}, map[string]any{
		"id": pgtest.GoldenOneOf("event-2", "event-3", "event-4"), "tenant_id": "tenant-1", "project_id": string(winnerScope.ProjectID),
		"type": "config.changed", "report_type": pgtest.NullValue,
		"subject_id": winnerBinding, "subject_version": int64(1),
		"source_kind": "shell", "source_principal": "principal-1",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	raceObserver.Finish(map[string]int{"resources": 1, "resource_bindings": 1, "receipts": 1, "audit": 1, "events": 1})
}
