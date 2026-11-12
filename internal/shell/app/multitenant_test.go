package app

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	shellidentity "github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// tenantScopeReader is the durable execution-to-tenant ownership seam a real
// installation satisfies with controllerExecutionScopes. The test changes the
// answer between calls so a dispatcher that caches ownership is caught.
type tenantScopeReader struct {
	mu     sync.Mutex
	scopes map[id.ExecutionID]tenancy.Scope
	err    error
}

func (reader *tenantScopeReader) ExecutionScope(_ context.Context, executionID id.ExecutionID) (tenancy.Scope, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.err != nil {
		return tenancy.Scope{}, reader.err
	}
	scope, ok := reader.scopes[executionID]
	if !ok {
		return tenancy.Scope{}, fault.New(fault.DependencyUnavailable, "execution scope is unavailable")
	}
	return scope, nil
}

func (reader *tenantScopeReader) set(executionID id.ExecutionID, tenant id.TenantID) {
	reader.mu.Lock()
	reader.scopes[executionID] = tenancy.Scope{TenantID: tenant, ProjectID: "project-1"}
	reader.mu.Unlock()
}

// tenantRuntimeSpy records every operation one tenant runtime receives.
type tenantRuntimeSpy struct {
	tenant string

	mu            sync.Mutex
	calls         []string
	executions    []string
	credentials   []string
	capabilities  kernelengine.Capabilities
	capabilityErr error
}

var _ kernelengine.Runtime = (*tenantRuntimeSpy)(nil)

func (spy *tenantRuntimeSpy) record(method string, identity kernelengine.EffectIdentity) {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	spy.calls = append(spy.calls, method)
	spy.executions = append(spy.executions, string(identity.ExecutionID))
}

func (spy *tenantRuntimeSpy) Capabilities(context.Context) (kernelengine.Capabilities, error) {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	spy.calls = append(spy.calls, "Capabilities")
	if spy.capabilityErr != nil {
		return kernelengine.Capabilities{}, spy.capabilityErr
	}
	return spy.capabilities, nil
}

func (spy *tenantRuntimeSpy) Prepare(_ context.Context, request kernelengine.PrepareRequest) (kernelengine.Preparation, error) {
	spy.record("Prepare", request.Identity)
	return kernelengine.Preparation{}, nil
}

func (spy *tenantRuntimeSpy) Start(_ context.Context, request kernelengine.StartRequest) (kernelengine.StartResult, error) {
	spy.record("Start", request.Identity)
	return kernelengine.StartResult{Outcome: kernelengine.StartAccepted}, nil
}

func (spy *tenantRuntimeSpy) Observe(_ context.Context, request kernelengine.ObserveRequest) (kernel.Observation, error) {
	spy.record("Observe", request.Identity)
	return kernel.RunningObservation(), nil
}

func (spy *tenantRuntimeSpy) Logs(_ context.Context, request kernelengine.LogsRequest) (kernelengine.LogBatch, error) {
	spy.record("Logs", request.Identity)
	return kernelengine.LogBatch{NextOffset: request.AfterOffset}, nil
}

func (spy *tenantRuntimeSpy) Stop(_ context.Context, request kernelengine.StopRequest) (kernel.StopEvidence, error) {
	spy.record("Stop", request.Identity)
	return kernel.StopEvidence{}, nil
}

func (spy *tenantRuntimeSpy) Remove(_ context.Context, request kernelengine.RemoveRequest) (kernelengine.Removal, error) {
	spy.record("Remove", request.Identity)
	return kernelengine.Removal{Removed: true}, nil
}

func (spy *tenantRuntimeSpy) RefreshCredential(_ context.Context, request kernelengine.CredentialRequest) (kernelengine.CredentialRefresh, error) {
	spy.record("RefreshCredential", request.Identity)
	spy.mu.Lock()
	spy.credentials = append(spy.credentials, request.Credential)
	spy.mu.Unlock()
	return kernelengine.CredentialRefresh{Delivered: true}, nil
}

func (spy *tenantRuntimeSpy) snapshot() (calls, executions, credentials []string) {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	return append([]string(nil), spy.calls...), append([]string(nil), spy.executions...), append([]string(nil), spy.credentials...)
}

func (spy *tenantRuntimeSpy) count() int {
	calls, _, _ := spy.snapshot()
	return len(calls)
}

// tenantWorkspaceSpy records every workspace operation one tenant receives.
type tenantWorkspaceSpy struct {
	tenant string

	mu    sync.Mutex
	calls []string
	ids   []string
}

var _ kernelengine.Workspace = (*tenantWorkspaceSpy)(nil)

func (spy *tenantWorkspaceSpy) record(method string, workspaceID id.WorkspaceID) {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	spy.calls = append(spy.calls, method)
	spy.ids = append(spy.ids, string(workspaceID))
}

func (spy *tenantWorkspaceSpy) Status(_ context.Context, workspaceID id.WorkspaceID) (kernelengine.WorkspaceObservation, error) {
	spy.record("Status", workspaceID)
	return kernelengine.WorkspaceObservation{WorkspaceID: workspaceID, Available: true}, nil
}

func (spy *tenantWorkspaceSpy) Retain(_ context.Context, workspaceID id.WorkspaceID) error {
	spy.record("Retain", workspaceID)
	return nil
}

func (spy *tenantWorkspaceSpy) Release(_ context.Context, workspaceID id.WorkspaceID) error {
	spy.record("Release", workspaceID)
	return nil
}

func (spy *tenantWorkspaceSpy) snapshot() []string {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	return append([]string(nil), spy.calls...)
}

// tenantActivation is the durable workspace-to-tenant ownership seam. A
// missing workspace answers with not_found exactly like the store-backed
// implementation, so the dispatcher must not turn unknown ownership into a
// successful release.
type tenantActivation struct {
	mu     sync.Mutex
	owners map[id.WorkspaceID]id.TenantID
	err    error
}

func (activation *tenantActivation) PreviouslyActivated(context.Context, id.WorkspaceID) (bool, error) {
	return false, nil
}

func (activation *tenantActivation) OwningTenant(_ context.Context, workspaceID id.WorkspaceID) (id.TenantID, error) {
	activation.mu.Lock()
	defer activation.mu.Unlock()
	if activation.err != nil {
		return "", activation.err
	}
	tenant, ok := activation.owners[workspaceID]
	if !ok {
		return "", fault.New(fault.NotFound, "workspace has no persisted execution")
	}
	return tenant, nil
}

const (
	tenantATest = id.TenantID("tenant-a")
	tenantBTest = id.TenantID("tenant-b")
	tenantXTest = id.TenantID("tenant-x")
)

func newRoutingFixture(t *testing.T) (*tenantRuntime, *tenantWorkspace, *tenantScopeReader, *tenantActivation, *tenantRuntimeSpy, *tenantRuntimeSpy, *tenantWorkspaceSpy, *tenantWorkspaceSpy) {
	t.Helper()
	runtimeA, runtimeB := &tenantRuntimeSpy{tenant: string(tenantATest)}, &tenantRuntimeSpy{tenant: string(tenantBTest)}
	workspaceA, workspaceB := &tenantWorkspaceSpy{tenant: string(tenantATest)}, &tenantWorkspaceSpy{tenant: string(tenantBTest)}
	scopes := &tenantScopeReader{scopes: map[id.ExecutionID]tenancy.Scope{}}
	activation := &tenantActivation{owners: map[id.WorkspaceID](id.TenantID){}}
	tenants := []ProductionTenant{
		{TenantID: tenantATest, Runtime: runtimeA, Workspace: workspaceA},
		{TenantID: tenantBTest, Runtime: runtimeB, Workspace: workspaceB},
	}
	runtime, err := newTenantRuntime(scopes, tenants)
	if err != nil {
		t.Fatalf("newTenantRuntime: %v", err)
	}
	workspace, err := newTenantWorkspace(activation, tenants)
	if err != nil {
		t.Fatalf("newTenantWorkspace: %v", err)
	}
	return runtime, workspace, scopes, activation, runtimeA, runtimeB, workspaceA, workspaceB
}

// runtimeOperations enumerates every Kernel runtime operation with a request
// whose only interesting field is the execution identity.
func runtimeOperations(executionID id.ExecutionID) []struct {
	name string
	run  func(context.Context, kernelengine.Runtime) error
} {
	identity := kernelengine.EffectIdentity{
		ExecutionID: executionID, ActionID: "action-1", WorkloadID: "workload-1", IdempotencyKey: "idempotency-1",
	}
	return []struct {
		name string
		run  func(context.Context, kernelengine.Runtime) error
	}{
		{"Prepare", func(ctx context.Context, runtime kernelengine.Runtime) error {
			_, err := runtime.Prepare(ctx, kernelengine.PrepareRequest{Identity: identity})
			return err
		}},
		{"Start", func(ctx context.Context, runtime kernelengine.Runtime) error {
			result, err := runtime.Start(ctx, kernelengine.StartRequest{Identity: identity, WorkspaceID: "workspace-1"})
			if err == nil && result.Outcome != kernelengine.StartAccepted {
				return fmt.Errorf("start outcome = %s; want the routed adapter's answer", result.Outcome)
			}
			return err
		}},
		{"Observe", func(ctx context.Context, runtime kernelengine.Runtime) error {
			_, err := runtime.Observe(ctx, kernelengine.ObserveRequest{Identity: identity, WorkspaceID: "workspace-1"})
			return err
		}},
		{"Logs", func(ctx context.Context, runtime kernelengine.Runtime) error {
			batch, err := runtime.Logs(ctx, kernelengine.LogsRequest{Identity: identity, AfterOffset: 3, Limit: 2})
			if err == nil && batch.NextOffset != 3 {
				return fmt.Errorf("log batch next offset = %d; want the routed adapter's answer", batch.NextOffset)
			}
			return err
		}},
		{"Stop", func(ctx context.Context, runtime kernelengine.Runtime) error {
			_, err := runtime.Stop(ctx, kernelengine.StopRequest{Identity: identity, WorkspaceID: "workspace-1"})
			return err
		}},
		{"Remove", func(ctx context.Context, runtime kernelengine.Runtime) error {
			removal, err := runtime.Remove(ctx, kernelengine.RemoveRequest{Identity: identity, WorkspaceID: "workspace-1"})
			if err == nil && !removal.Removed {
				return fmt.Errorf("removal = %+v; want the routed adapter's confirmed removal", removal)
			}
			return err
		}},
		{"RefreshCredential", func(ctx context.Context, runtime kernelengine.Runtime) error {
			evidence, err := runtime.RefreshCredential(ctx, kernelengine.CredentialRequest{Identity: identity, Credential: "credential-sentinel"})
			if err == nil && !evidence.Delivered {
				return fmt.Errorf("credential refresh = %+v; want the routed adapter's confirmation", evidence)
			}
			return err
		}},
	}
}

// TestTenantRuntimeRoutesEveryOperationByExecutionOwnership proves persisted
// execution ownership selects exactly one tenant runtime for each of the eight
// Kernel operations, and that the other tenant receives no call at all. The
// routed adapter's own answer is asserted, so a dispatcher that swallowed the
// error or returned a zero value would not pass.
func TestTenantRuntimeRoutesEveryOperationByExecutionOwnership(t *testing.T) {
	runtime, _, scopes, _, runtimeA, runtimeB, _, _ := newRoutingFixture(t)
	executionA, executionB := id.ExecutionID("execution-a"), id.ExecutionID("execution-b")
	scopes.set(executionA, tenantATest)
	scopes.set(executionB, tenantBTest)

	for _, operation := range runtimeOperations(executionA) {
		t.Run(operation.name+" tenant-a", func(t *testing.T) {
			beforeA, beforeB := runtimeA.count(), runtimeB.count()
			if err := operation.run(context.Background(), runtime); err != nil {
				t.Fatalf("%s: %v", operation.name, err)
			}
			if got := runtimeA.count() - beforeA; got != 1 {
				t.Fatalf("tenant-a %s calls = %d; want 1", operation.name, got)
			}
			if got := runtimeB.count() - beforeB; got != 0 {
				t.Fatalf("tenant-b received %d %s calls for tenant-a's execution", got, operation.name)
			}
			_, seen, _ := runtimeA.snapshot()
			if len(seen) == 0 || seen[len(seen)-1] != string(executionA) {
				t.Fatalf("tenant-a %s execution = %v; want %s", operation.name, seen, executionA)
			}
		})
	}
	for _, operation := range runtimeOperations(executionB) {
		t.Run(operation.name+" tenant-b", func(t *testing.T) {
			beforeA, beforeB := runtimeA.count(), runtimeB.count()
			if err := operation.run(context.Background(), runtime); err != nil {
				t.Fatalf("%s: %v", operation.name, err)
			}
			if got := runtimeB.count() - beforeB; got != 1 {
				t.Fatalf("tenant-b %s calls = %d; want 1", operation.name, got)
			}
			if got := runtimeA.count() - beforeA; got != 0 {
				t.Fatalf("tenant-a received %d %s calls for tenant-b's execution", got, operation.name)
			}
		})
	}
	for _, spy := range []*tenantRuntimeSpy{runtimeA, runtimeB} {
		_, _, credentials := spy.snapshot()
		if len(credentials) != 1 || credentials[0] != "credential-sentinel" {
			t.Fatalf("tenant %s credentials = %v; want its own single delivered credential", spy.tenant, credentials)
		}
	}
}

// TestTenantRuntimeUnknownOwnershipHasZeroEffects pins the fail-closed rule for
// every operation: an unreadable ownership fact and an unserved tenant both
// refuse before any adapter call, and the refusal is never a not_found that the
// Kernel could read as successful absence or cleanup evidence.
func TestTenantRuntimeUnknownOwnershipHasZeroEffects(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		setup func(*tenantScopeReader)
		class fault.Class
	}{
		{
			name: "unreadable ownership",
			setup: func(scopes *tenantScopeReader) {
				scopes.err = fault.New(fault.DependencyUnavailable, "execution scope is unavailable")
			},
			class: fault.DependencyUnavailable,
		},
		{
			name: "tenant is not served",
			setup: func(scopes *tenantScopeReader) {
				scopes.set("execution-1", tenantXTest)
			},
			class: fault.Forbidden,
		},
		{
			name: "ownership has no tenant",
			setup: func(scopes *tenantScopeReader) {
				scopes.scopes["execution-1"] = tenancy.Scope{}
			},
			class: fault.Forbidden,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			runtime, _, scopes, _, runtimeA, runtimeB, _, _ := newRoutingFixture(t)
			testCase.setup(scopes)
			for _, operation := range runtimeOperations("execution-1") {
				err := operation.run(context.Background(), runtime)
				if err == nil {
					t.Fatalf("%s succeeded with unknown ownership", operation.name)
				}
				class, ok := fault.ClassOf(err)
				if !ok || class == fault.NotFound {
					t.Fatalf("%s refusal class = %s (typed=%t); unknown ownership must never be not_found", operation.name, class, ok)
				}
				if class != testCase.class {
					t.Fatalf("%s refusal class = %s; want %s", operation.name, class, testCase.class)
				}
			}
			if runtimeA.count() != 0 || runtimeB.count() != 0 {
				t.Fatalf("adapters received calls (%d/%d) despite unknown ownership", runtimeA.count(), runtimeB.count())
			}
		})
	}
}

// TestTenantRuntimeNeverCachesOwnership proves routing re-reads the durable
// ownership fact on every call. A dispatcher that memoized the first answer
// would keep routing an execution to the wrong tenant after a restart or a
// corrected attribution.
func TestTenantRuntimeNeverCachesOwnership(t *testing.T) {
	runtime, _, scopes, _, runtimeA, runtimeB, _, _ := newRoutingFixture(t)
	execution := id.ExecutionID("execution-1")
	scopes.set(execution, tenantATest)
	if _, err := runtime.Start(context.Background(), kernelengine.StartRequest{Identity: kernelengine.EffectIdentity{ExecutionID: execution}}); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	scopes.set(execution, tenantBTest)
	if _, err := runtime.Start(context.Background(), kernelengine.StartRequest{Identity: kernelengine.EffectIdentity{ExecutionID: execution}}); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if runtimeA.count() != 1 || runtimeB.count() != 1 {
		t.Fatalf("routed calls tenant-a/tenant-b = %d/%d; want one each after ownership changed", runtimeA.count(), runtimeB.count())
	}
}

// TestTenantRuntimeCapabilitiesAreTheSafeIntersection pins that the
// composition advertises only capabilities every served tenant adapter
// supports. A union would admit a pipeline to a tenant whose adapter cannot run
// it.
func TestTenantRuntimeCapabilitiesAreTheSafeIntersection(t *testing.T) {
	runtime, _, _, _, runtimeA, runtimeB, _, _ := newRoutingFixture(t)
	runtimeA.capabilities = kernelengine.Capabilities{Names: []string{"process", "kubernetes", "logs"}}
	runtimeB.capabilities = kernelengine.Capabilities{Names: []string{"kubernetes", "logs", "git"}}
	capabilities, err := runtime.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if got, want := capabilities.Names, []string{"kubernetes", "logs"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("capabilities = %v; want the intersection %v", got, want)
	}
	if capabilities.Supports("process") || capabilities.Supports("git") {
		t.Fatalf("capabilities = %v; want neither tenant-only capability", capabilities.Names)
	}

	runtimeB.capabilityErr = fault.New(fault.DependencyUnavailable, "capabilities are unavailable")
	if _, err := runtime.Capabilities(context.Background()); err == nil {
		t.Fatal("Capabilities succeeded while one tenant adapter refused")
	}
}

// TestTenantWorkspaceRoutesEveryOperationByWorkspaceOwnership proves Status,
// Retain and Release each reach only the adapter of the workspace's durable
// owning tenant.
func TestTenantWorkspaceRoutesEveryOperationByWorkspaceOwnership(t *testing.T) {
	_, workspace, _, activation, _, _, workspaceA, workspaceB := newRoutingFixture(t)
	workspaceAID, workspaceBID := id.WorkspaceID("workspace-a"), id.WorkspaceID("workspace-b")
	activation.owners[workspaceAID] = tenantATest
	activation.owners[workspaceBID] = tenantBTest

	operations := []struct {
		name string
		run  func(context.Context, id.WorkspaceID) error
	}{
		{"Status", func(ctx context.Context, workspaceID id.WorkspaceID) error {
			observation, err := workspace.Status(ctx, workspaceID)
			if err == nil && (!observation.Available || observation.WorkspaceID != workspaceID) {
				return fmt.Errorf("observation = %+v; want the routed adapter's answer", observation)
			}
			return err
		}},
		{"Retain", func(ctx context.Context, workspaceID id.WorkspaceID) error {
			return workspace.Retain(ctx, workspaceID)
		}},
		{"Release", func(ctx context.Context, workspaceID id.WorkspaceID) error {
			return workspace.Release(ctx, workspaceID)
		}},
	}
	for _, operation := range operations {
		for _, testCase := range []struct {
			workspaceID id.WorkspaceID
			owner       *tenantWorkspaceSpy
			other       *tenantWorkspaceSpy
		}{
			{workspaceAID, workspaceA, workspaceB},
			{workspaceBID, workspaceB, workspaceA},
		} {
			t.Run(operation.name+" "+testCase.owner.tenant, func(t *testing.T) {
				ownerBefore, otherBefore := len(testCase.owner.snapshot()), len(testCase.other.snapshot())
				if err := operation.run(context.Background(), testCase.workspaceID); err != nil {
					t.Fatalf("%s: %v", operation.name, err)
				}
				if got := len(testCase.owner.snapshot()) - ownerBefore; got != 1 {
					t.Fatalf("%s calls for %s = %d; want 1", operation.name, testCase.owner.tenant, got)
				}
				if got := len(testCase.other.snapshot()) - otherBefore; got != 0 {
					t.Fatalf("%s calls for %s = %d; want 0", operation.name, testCase.other.tenant, got)
				}
			})
		}
	}
}

// TestTenantWorkspaceUnknownOwnershipHasZeroEffects proves an unreadable or
// unserved workspace ownership fact refuses every workspace operation without
// an adapter call. In particular a missing persisted execution (the store's
// not_found) must surface as a retryable dependency fault: Release must never
// become the Kernel's "nothing left to release" success path for a workspace
// whose tenant is unknown.
func TestTenantWorkspaceUnknownOwnershipHasZeroEffects(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		setup func(*tenantActivation)
		class fault.Class
	}{
		{
			name: "workspace has no persisted ownership",
			setup: func(*tenantActivation) {
				// No owner entry: OwningTenant answers not_found.
			},
			class: fault.DependencyUnavailable,
		},
		{
			name: "ownership lookup fails",
			setup: func(activation *tenantActivation) {
				activation.err = fault.New(fault.DependencyUnavailable, "workspace activation state is unreachable")
			},
			class: fault.DependencyUnavailable,
		},
		{
			name: "tenant is not served",
			setup: func(activation *tenantActivation) {
				activation.owners["workspace-1"] = tenantXTest
			},
			class: fault.Forbidden,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, workspace, _, activation, _, _, workspaceA, workspaceB := newRoutingFixture(t)
			testCase.setup(activation)
			for _, run := range []struct {
				name string
				call func() error
			}{
				{"Status", func() error { _, err := workspace.Status(context.Background(), "workspace-1"); return err }},
				{"Retain", func() error { return workspace.Retain(context.Background(), "workspace-1") }},
				{"Release", func() error { return workspace.Release(context.Background(), "workspace-1") }},
			} {
				err := run.call()
				if err == nil {
					t.Fatalf("%s succeeded with unknown workspace ownership", run.name)
				}
				class, ok := fault.ClassOf(err)
				if !ok || class == fault.NotFound {
					t.Fatalf("%s refusal class = %s (typed=%t); unknown ownership must never be not_found", run.name, class, ok)
				}
				if class != testCase.class {
					t.Fatalf("%s refusal class = %s; want %s", run.name, class, testCase.class)
				}
			}
			if len(workspaceA.snapshot()) != 0 || len(workspaceB.snapshot()) != 0 {
				t.Fatalf("workspace adapters received calls (%v/%v) despite unknown ownership", workspaceA.snapshot(), workspaceB.snapshot())
			}
		})
	}
}

// TestTenantRoutingConcurrentExecutionsStayIsolated drives both tenants at the
// same time through one dispatcher. Every call must land on the adapter of the
// execution's own tenant, so a shared or swapped adapter cannot pass under the
// race detector.
func TestTenantRoutingConcurrentExecutionsStayIsolated(t *testing.T) {
	runtime, workspace, scopes, activation, runtimeA, runtimeB, workspaceA, workspaceB := newRoutingFixture(t)
	const callsPerTenant = 64
	start := make(chan struct{})
	var wait sync.WaitGroup
	errs := make(chan error, 2*callsPerTenant)
	for index := range callsPerTenant {
		executionA := id.ExecutionID(fmt.Sprintf("execution-a-%d", index))
		executionB := id.ExecutionID(fmt.Sprintf("execution-b-%d", index))
		scopes.set(executionA, tenantATest)
		scopes.set(executionB, tenantBTest)
		workspaceAID := id.WorkspaceID(fmt.Sprintf("workspace-a-%d", index))
		workspaceBID := id.WorkspaceID(fmt.Sprintf("workspace-b-%d", index))
		activation.owners[workspaceAID] = tenantATest
		activation.owners[workspaceBID] = tenantBTest
		for _, executionID := range []id.ExecutionID{executionA, executionB} {
			wait.Go(func() {
				<-start
				if _, err := runtime.Start(context.Background(), kernelengine.StartRequest{Identity: kernelengine.EffectIdentity{ExecutionID: executionID}}); err != nil {
					errs <- err
				}
			})
		}
		for _, workspaceID := range []id.WorkspaceID{workspaceAID, workspaceBID} {
			wait.Go(func() {
				<-start
				if err := workspace.Retain(context.Background(), workspaceID); err != nil {
					errs <- err
				}
			})
		}
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent routing: %v", err)
	}
	if got := runtimeA.count(); got != callsPerTenant {
		t.Fatalf("tenant-a runtime calls = %d; want %d", got, callsPerTenant)
	}
	if got := runtimeB.count(); got != callsPerTenant {
		t.Fatalf("tenant-b runtime calls = %d; want %d", got, callsPerTenant)
	}
	if got := len(workspaceA.snapshot()); got != callsPerTenant {
		t.Fatalf("tenant-a workspace calls = %d; want %d", got, callsPerTenant)
	}
	if got := len(workspaceB.snapshot()); got != callsPerTenant {
		t.Fatalf("tenant-b workspace calls = %d; want %d", got, callsPerTenant)
	}
	// Every recorded execution/workspace identity must carry its adapter's own
	// tenant prefix, so a crossed dispatch is caught even if the counts match.
	_, executionsA, _ := runtimeA.snapshot()
	for _, execution := range executionsA {
		if len(execution) < len("execution-a-") || execution[:len("execution-a-")] != "execution-a-" {
			t.Fatalf("tenant-a runtime received foreign execution %q", execution)
		}
	}
	_, executionsB, _ := runtimeB.snapshot()
	for _, execution := range executionsB {
		if len(execution) < len("execution-b-") || execution[:len("execution-b-")] != "execution-b-" {
			t.Fatalf("tenant-b runtime received foreign execution %q", execution)
		}
	}
}

// tenantIdentityResolver is the deliverer's execution identity seam.
type tenantIdentityResolver struct {
	executionIDs map[id.ExecutionID]id.WorkloadID
}

func (resolver *tenantIdentityResolver) ResolveExecutionIdentity(_ context.Context, executionID id.ExecutionID) (kernelengine.EffectIdentity, error) {
	workloadID, ok := resolver.executionIDs[executionID]
	if !ok {
		return kernelengine.EffectIdentity{}, fault.New(fault.DependencyUnavailable, "execution identity is unavailable")
	}
	return kernelengine.EffectIdentity{ExecutionID: executionID, ActionID: "action-1", WorkloadID: workloadID}, nil
}

// TestTenantRuntimeServesTheCredentialDeliverer proves the credential renewal
// bridge is bound to the dispatcher, not to a single tenant: a delivery for a
// tenant-b execution reaches only tenant-b's runtime, carries the delivery
// token unchanged and leaves tenant-a untouched.
func TestTenantRuntimeServesTheCredentialDeliverer(t *testing.T) {
	runtime, _, scopes, _, runtimeA, runtimeB, _, _ := newRoutingFixture(t)
	execution := id.ExecutionID("execution-b")
	scopes.set(execution, tenantBTest)
	deliverer, err := bridges.NewDeliverer(runtime, &tenantIdentityResolver{
		executionIDs: map[id.ExecutionID]id.WorkloadID{execution: "workload-b"},
	})
	if err != nil {
		t.Fatalf("NewDeliverer: %v", err)
	}
	delivery := token.Delivery{
		Scope:       tenancy.Scope{TenantID: tenantBTest, ProjectID: "project-1"},
		PrincipalID: "principal-1",
		ExecutionID: execution,
		Kind:        token.MutationIssue,
		Token:       "credential-sentinel",
		Target:      "runtime",
	}
	if err := deliverer.Deliver(context.Background(), delivery); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if runtimeA.count() != 0 {
		t.Fatalf("tenant-a received %d deliverer calls for tenant-b's execution", runtimeA.count())
	}
	_, _, credentials := runtimeB.snapshot()
	if len(credentials) != 1 || credentials[0] != "credential-sentinel" {
		t.Fatalf("tenant-b credentials = %v; want the delivered token once", credentials)
	}
}

// TestTenantDispatchersRefuseIncompleteTenantSets pins construction: an empty
// set and a missing adapter are refused before the composition can route
// anything.
func TestTenantDispatchersRefuseIncompleteTenantSets(t *testing.T) {
	runtimeSpy := &tenantRuntimeSpy{tenant: string(tenantATest)}
	workspaceSpy := &tenantWorkspaceSpy{tenant: string(tenantATest)}
	scopes := &tenantScopeReader{scopes: map[id.ExecutionID]tenancy.Scope{}}
	activation := &tenantActivation{owners: map[id.WorkspaceID](id.TenantID){}}
	for _, testCase := range []struct {
		name          string
		tenants       []ProductionTenant
		wantRuntime   bool
		wantWorkspace bool
	}{
		{"empty", nil, true, true},
		{"missing runtime", []ProductionTenant{{TenantID: tenantATest, Workspace: workspaceSpy}}, true, false},
		{"missing workspace", []ProductionTenant{{TenantID: tenantATest, Runtime: runtimeSpy}}, false, true},
		{"invalid tenant", []ProductionTenant{{TenantID: "Not A Tenant", Runtime: runtimeSpy, Workspace: workspaceSpy}}, true, true},
		{"duplicate tenant", []ProductionTenant{
			{TenantID: tenantATest, Runtime: runtimeSpy, Workspace: workspaceSpy},
			{TenantID: tenantATest, Runtime: runtimeSpy, Workspace: workspaceSpy},
		}, true, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := newTenantRuntime(scopes, testCase.tenants); (err != nil) != testCase.wantRuntime {
				t.Fatalf("newTenantRuntime error = %v; want refusal %t", err, testCase.wantRuntime)
			}
			if _, err := newTenantWorkspace(activation, testCase.tenants); (err != nil) != testCase.wantWorkspace {
				t.Fatalf("newTenantWorkspace error = %v; want refusal %t", err, testCase.wantWorkspace)
			}
		})
	}
	if _, err := newTenantRuntime(nil, []ProductionTenant{{TenantID: tenantATest, Runtime: runtimeSpy}}); err == nil {
		t.Fatal("newTenantRuntime accepted a nil ownership reader")
	}
	if _, err := newTenantWorkspace(nil, []ProductionTenant{{TenantID: tenantATest, Workspace: workspaceSpy}}); err == nil {
		t.Fatal("newTenantWorkspace accepted a nil ownership reader")
	}
}

// recordingFakeRuntime records the execution identities the Kernel dispatches
// to one tenant runtime and then delegates to the deterministic fake.
type recordingFakeRuntime struct {
	*fakeRuntime
	mu         sync.Mutex
	executions map[id.ExecutionID]int
}

func newRecordingFakeRuntime() *recordingFakeRuntime {
	return &recordingFakeRuntime{fakeRuntime: &fakeRuntime{}, executions: map[id.ExecutionID]int{}}
}

func (runtime *recordingFakeRuntime) Start(ctx context.Context, request kernelengine.StartRequest) (kernelengine.StartResult, error) {
	runtime.mu.Lock()
	runtime.executions[request.Identity.ExecutionID]++
	runtime.mu.Unlock()
	return runtime.fakeRuntime.Start(ctx, request)
}

func (runtime *recordingFakeRuntime) startedExecutions() []id.ExecutionID {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	executions := make([]id.ExecutionID, 0, len(runtime.executions))
	for execution := range runtime.executions {
		executions = append(executions, execution)
	}
	return executions
}

// TestServedInstallationExecutesTwoTenantsConcurrently is the acceptance
// test: one served composition dispatches concurrent executions of two
// tenants, and each execution reaches only the runtime adapter of its own
// persisted tenant. The ownership assertion reads the persisted scope back
// through the same reader the production composition uses, so a dispatcher
// that guessed from the request would not pass.
func TestServedInstallationExecutesTwoTenantsConcurrently(t *testing.T) {
	config := fullTestProfileConfig(t)
	memoryStore, ok := config.Adapters.Store.(*memory.Store)
	if !ok {
		t.Fatalf("test composition store = %T; want the memory store", config.Adapters.Store)
	}
	ctx := context.Background()
	cipher, err := keys.NewCipher(config.Adapters.Keys, rand.Reader)
	if err != nil {
		t.Fatalf("keys.NewCipher: %v", err)
	}
	if err := seedFirstAdmin(ctx, memoryStore, memoryStore, config.Adapters.IDs, cipher,
		config.InstallationID, secret.MinKeyVersion, &Bootstrap{
			TenantID:       "tenant-2",
			TenantName:     "Tenant Two",
			ProjectID:      "project-2",
			ProjectName:    "Project Two",
			AdminPrincipal: "admin-2",
			AdminRoles:     []string{string(tenancy.RoleAdmin)},
		}, config.Adapters.Clock); err != nil {
		t.Fatalf("seed tenant-2: %v", err)
	}
	if err := seedMembership(ctx, memoryStore, memoryStore, config.Adapters.IDs,
		tenancy.Scope{TenantID: "tenant-2", ProjectID: "project-2"}, "admin-2",
		[]tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer}, config.Adapters.Clock.Now()); err != nil {
		t.Fatalf("seed tenant-2 project membership: %v", err)
	}
	config.Dev.Tokens = append(config.Dev.Tokens, DevToken{
		Credential: "dev-tenant-two-token",
		Principal: shellidentity.Principal{
			ID:      "admin-2",
			Kind:    shellidentity.KindHuman,
			Scope:   tenancy.Scope{TenantID: "tenant-2", ProjectID: "project-2"},
			Roles:   []tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer},
			Enabled: true,
			Version: 1,
		},
		ExpiresAt: config.Adapters.Clock.Now().Add(24 * time.Hour),
	})
	authenticator, err := NewDevAuthenticator(config.Adapters.Clock, config.Dev.Tokens)
	if err != nil {
		t.Fatalf("NewDevAuthenticator: %v", err)
	}
	config.Adapters.Principal = authenticator

	scopes, err := newControllerExecutionScopes(config.Adapters.Controller)
	if err != nil {
		t.Fatalf("newControllerExecutionScopes: %v", err)
	}
	activation, err := newWorkspaceActivation(config.Adapters.Controller)
	if err != nil {
		t.Fatalf("newWorkspaceActivation: %v", err)
	}
	runtimeA, runtimeB := newRecordingFakeRuntime(), newRecordingFakeRuntime()
	tenants := []ProductionTenant{
		{TenantID: "tenant-1", Runtime: runtimeA, Workspace: &fakeRuntime{}},
		{TenantID: "tenant-2", Runtime: runtimeB, Workspace: &fakeRuntime{}},
	}
	dispatcherRuntime, err := newTenantRuntime(scopes, tenants)
	if err != nil {
		t.Fatalf("newTenantRuntime: %v", err)
	}
	dispatcherWorkspace, err := newTenantWorkspace(activation, tenants)
	if err != nil {
		t.Fatalf("newTenantWorkspace: %v", err)
	}
	config.Adapters.Runtime = dispatcherRuntime
	config.Adapters.Workspace = dispatcherWorkspace

	app, err := New(ctx, config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := app.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := app.Close(closeCtx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	handler := app.Handler()

	// The tenant-wide administrator needs the project Engineer role the
	// pipeline and task operations require; the seed deliberately leaves
	// project grants to the public operation.
	pipelines := map[string]struct {
		project    string
		token      string
		adminToken string
		adminID    string
	}{
		"tenant-1": {project: "project-1", token: "dev-project-token", adminToken: "dev-admin-token", adminID: "admin-1"},
		"tenant-2": {project: "project-2", token: "dev-tenant-two-token", adminToken: "dev-tenant-two-token", adminID: "admin-2"},
	}
	recorder, _, publicError := restCall(t, handler, http.MethodPut,
		"/api/v1/projects/project-1/memberships/admin-1", "dev-admin-token",
		api.MembershipGrantRequest{Roles: []string{string(tenancy.RoleAdmin), string(tenancy.RoleEngineer)}}, nil, "membership-tenant-1")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("tenant-1 membership.grant status=%d error=%+v", recorder.Code, publicError)
	}

	// Both tenants prepare a pipeline before the concurrent task starts, so the
	// assertion is about dispatch, not admission order.
	applied := map[string]api.PipelineApplyResult{}
	for tenant, scope := range pipelines {
		recorder, result, publicError := restCall(t, handler, http.MethodPut,
			"/api/v1/projects/"+scope.project+"/pipelines/build", scope.token,
			api.PipelineApplyRequest{Name: "build", YAML: []byte(smokePipeline)}, nil, "apply-"+tenant)
		if recorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("%s pipeline.apply status=%d error=%+v", tenant, recorder.Code, publicError)
		}
		applied[tenant] = decodeBody[api.PipelineApplyResult](t, result)
	}

	start := make(chan struct{})
	var wait sync.WaitGroup
	failures := make(chan string, len(pipelines))
	for tenant, scope := range pipelines {
		wait.Go(func() {
			<-start
			recorder, _, publicError := restCall(t, handler, http.MethodPost,
				"/api/v1/projects/"+scope.project+"/tasks/create-and-start", scope.token,
				api.TaskCreateAndStartRequest{Title: "concurrent " + tenant, PipelineID: applied[tenant].PipelineID}, nil, "start-"+tenant)
			if recorder.Code != api.HTTPStatusSuccess {
				failures <- fmt.Sprintf("%s task.create_and_start status=%d error=%+v", tenant, recorder.Code, publicError)
			}
		})
	}
	close(start)
	wait.Wait()
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}

	eventually(t, 10*time.Second, func() bool {
		return len(runtimeA.startedExecutions()) > 0 && len(runtimeB.startedExecutions()) > 0
	})
	for tenant, runtime := range map[id.TenantID]*recordingFakeRuntime{"tenant-1": runtimeA, "tenant-2": runtimeB} {
		executions := runtime.startedExecutions()
		if len(executions) == 0 {
			t.Fatalf("tenant %s runtime received no execution", tenant)
		}
		for _, execution := range executions {
			scope, err := scopes.ExecutionScope(ctx, execution)
			if err != nil {
				t.Fatalf("read persisted scope of %s: %v", execution, err)
			}
			if scope.TenantID != tenant {
				t.Fatalf("tenant %s runtime received execution %s owned by %s", tenant, execution, scope.TenantID)
			}
		}
	}
	for _, executionA := range runtimeA.startedExecutions() {
		for _, executionB := range runtimeB.startedExecutions() {
			if executionA == executionB {
				t.Fatalf("both tenants received execution %s", executionA)
			}
		}
	}
}

// TestTenantRuntimeCapabilitiesIntersectEmptyLists pins the empty-list cases of
// the installation-wide capability intersection. A nil list is a tenant that
// advertises no capability at all, so the whole installation advertises
// nothing; a first/last/middle nil list must never be treated as "not seen
// yet" and replaced by another tenant's list. The result is always a non-nil
// (possibly empty) list, so an admitted pipeline can never rely on a nil value
// meaning "unknown".
func TestTenantRuntimeCapabilitiesIntersectEmptyLists(t *testing.T) {
	tenant := func(name string, names ...string) *tenantRuntimeSpy {
		return &tenantRuntimeSpy{tenant: name, capabilities: kernelengine.Capabilities{Names: names}}
	}
	three := func(first, second, third *tenantRuntimeSpy) []ProductionTenant {
		return []ProductionTenant{
			{TenantID: "tenant-a", Runtime: first},
			{TenantID: "tenant-b", Runtime: second},
			{TenantID: "tenant-c", Runtime: third},
		}
	}
	for _, testCase := range []struct {
		name    string
		tenants []ProductionTenant
		want    []string
	}{
		{
			name:    "empty first tenant",
			tenants: three(tenant("a"), tenant("b", "kubernetes"), tenant("c", "kubernetes")),
		},
		{
			name:    "empty last tenant",
			tenants: three(tenant("a", "process"), tenant("b", "process"), tenant("c")),
		},
		{
			name:    "empty middle tenant",
			tenants: three(tenant("a", "process"), tenant("b"), tenant("c", "process", "logs")),
		},
		{
			name:    "all tenants empty",
			tenants: three(tenant("a"), tenant("b"), tenant("c")),
		},
		{
			name:    "identical lists keep their order",
			tenants: three(tenant("a", "process", "logs"), tenant("b", "process", "logs"), tenant("c", "logs", "process")),
			want:    []string{"process", "logs"},
		},
		{
			name:    "disjoint non-empty lists intersect to empty",
			tenants: three(tenant("a", "process"), tenant("b", "logs"), tenant("c", "process")),
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			runtime, err := newTenantRuntime(&tenantScopeReader{scopes: map[id.ExecutionID]tenancy.Scope{}}, testCase.tenants)
			if err != nil {
				t.Fatalf("newTenantRuntime: %v", err)
			}
			capabilities, err := runtime.Capabilities(context.Background())
			if err != nil {
				t.Fatalf("Capabilities: %v", err)
			}
			if capabilities.Names == nil {
				t.Fatalf("capabilities = nil; want a non-nil (possibly empty) list")
			}
			if len(capabilities.Names) != len(testCase.want) {
				t.Fatalf("capabilities = %v; want %v", capabilities.Names, testCase.want)
			}
			for index, name := range testCase.want {
				if capabilities.Names[index] != name {
					t.Fatalf("capabilities = %v; want %v", capabilities.Names, testCase.want)
				}
			}
		})
	}
}

// TestTenantRuntimeCapabilitiesFailClosedOnAnyAdapterError pins the documented
// installation-wide rule: one tenant adapter that cannot report its
// capabilities fails the whole read, even if every other tenant answered, so
// the composition never advertises a capability it could not confirm for every
// served tenant.
func TestTenantRuntimeCapabilitiesFailClosedOnAnyAdapterError(t *testing.T) {
	for _, failing := range []string{"first", "middle", "last"} {
		t.Run(failing, func(t *testing.T) {
			spies := []*tenantRuntimeSpy{
				{tenant: "tenant-a", capabilities: kernelengine.Capabilities{Names: []string{"process"}}},
				{tenant: "tenant-b", capabilities: kernelengine.Capabilities{Names: []string{"process"}}},
				{tenant: "tenant-c", capabilities: kernelengine.Capabilities{Names: []string{"process"}}},
			}
			index := map[string]int{"first": 0, "middle": 1, "last": 2}[failing]
			spies[index].capabilityErr = fault.New(fault.DependencyUnavailable, "capabilities are unavailable")
			runtime, err := newTenantRuntime(&tenantScopeReader{scopes: map[id.ExecutionID]tenancy.Scope{}}, []ProductionTenant{
				{TenantID: "tenant-a", Runtime: spies[0]},
				{TenantID: "tenant-b", Runtime: spies[1]},
				{TenantID: "tenant-c", Runtime: spies[2]},
			})
			if err != nil {
				t.Fatalf("newTenantRuntime: %v", err)
			}
			capabilities, err := runtime.Capabilities(context.Background())
			if err == nil {
				t.Fatalf("Capabilities succeeded with the %s tenant failing; got %v", failing, capabilities.Names)
			}
			if len(capabilities.Names) != 0 {
				t.Fatalf("Capabilities returned %v with an error; want no advertised capabilities", capabilities.Names)
			}
		})
	}
}
