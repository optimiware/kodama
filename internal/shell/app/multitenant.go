// Multi-tenant routing for the production Kernel ports.
//
// A production installation may serve several tenants from one control plane.
// The Kernel's runtime and workspace ports carry only an effect or
// workspace identity, never a tenant, so the composition resolves the durable
// owner first and then delegates to exactly one tenant-bound adapter. The
// ownership facts are read on every call from the same durable seams the
// existing single-tenant composition uses: controllerExecutionScopes for an
// execution and workspaceActivation.OwningTenant for a workspace.
//
// An unreadable ownership fact or a tenant this installation does not serve
// refuses before any adapter call. The refusal is never fault.NotFound, because
// the Kernel treats not_found on cleanup as proof that there is nothing left to
// release; unknown ownership must stay a retryable failure instead of
// successful absence or cleanup evidence.
package app

import (
	"context"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
)

// tenantRuntime routes every Kernel runtime operation to the adapter of the
// tenant that durably owns the execution.
type tenantRuntime struct {
	scopes  executionScopeReader
	tenants map[id.TenantID]kernelengine.Runtime
	order   []id.TenantID
}

var _ kernelengine.Runtime = (*tenantRuntime)(nil)

// newTenantRuntime validates the tenant set and builds the dispatcher. Every
// tenant needs its own runtime adapter; a duplicate or unserved set is refused
// before anything can route.
func newTenantRuntime(scopes executionScopeReader, tenants []ProductionTenant) (*tenantRuntime, error) {
	if scopes == nil {
		return nil, fault.New(fault.InvalidInput, "multi-tenant runtime requires an execution ownership reader")
	}
	if len(tenants) == 0 {
		return nil, fault.New(fault.InvalidInput, "multi-tenant runtime requires at least one tenant")
	}
	runtime := &tenantRuntime{
		scopes:  scopes,
		tenants: make(map[id.TenantID]kernelengine.Runtime, len(tenants)),
		order:   make([]id.TenantID, 0, len(tenants)),
	}
	for _, tenant := range tenants {
		if _, err := id.ParseTenantID(string(tenant.TenantID)); err != nil {
			return nil, fault.New(fault.InvalidInput, "multi-tenant runtime tenant id is invalid")
		}
		if tenant.Runtime == nil {
			return nil, fault.New(fault.InvalidInput,
				"multi-tenant runtime requires a runtime adapter for every tenant")
		}
		if _, exists := runtime.tenants[tenant.TenantID]; exists {
			return nil, fault.New(fault.InvalidInput,
				"multi-tenant runtime requires distinct tenants")
		}
		runtime.tenants[tenant.TenantID] = tenant.Runtime
		runtime.order = append(runtime.order, tenant.TenantID)
	}
	return runtime, nil
}

// adapter resolves the one tenant runtime that owns the execution. Nothing is
// called before the durable fact is read and matched.
func (runtime *tenantRuntime) adapter(ctx context.Context, executionID id.ExecutionID) (kernelengine.Runtime, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scope, err := runtime.scopes.ExecutionScope(ctx, executionID)
	if err != nil {
		return nil, err
	}
	adapter, ok := runtime.tenants[scope.TenantID]
	if !ok {
		return nil, fault.New(fault.Forbidden,
			"execution tenant is not served by this installation")
	}
	return adapter, nil
}

// Capabilities is the safe intersection of every served tenant adapter's
// capabilities: a union would admit a pipeline to a tenant whose adapter cannot
// run it. There is no execution identity to route this port by, so the answer
// is installation-wide.
//
// A tenant that advertises no capability contributes an empty list, and an
// empty list intersected with anything is empty; the first tenant is tracked by
// an explicit flag rather than a nil slice, because an empty first list must
// never be mistaken for "not seen yet". A single adapter that cannot report its
// capabilities fails the whole read with no advertised capabilities: the
// composition never claims a capability it could not confirm for every served
// tenant.
func (runtime *tenantRuntime) Capabilities(ctx context.Context) (kernelengine.Capabilities, error) {
	if err := ctx.Err(); err != nil {
		return kernelengine.Capabilities{}, err
	}
	intersection := []string{}
	first := true
	for _, tenant := range runtime.order {
		capabilities, err := runtime.tenants[tenant].Capabilities(ctx)
		if err != nil {
			return kernelengine.Capabilities{}, err
		}
		if first {
			intersection = append(intersection, capabilities.Names...)
			first = false
			continue
		}
		intersection = intersectNames(intersection, capabilities.Names)
	}
	return kernelengine.Capabilities{Names: intersection}, nil
}

// intersectNames keeps the first list's order and drops every name the second
// list does not declare.
func intersectNames(first, second []string) []string {
	supported := make(map[string]bool, len(second))
	for _, name := range second {
		supported[name] = true
	}
	intersection := make([]string, 0, len(first))
	for _, name := range first {
		if supported[name] {
			intersection = append(intersection, name)
		}
	}
	return intersection
}

func (runtime *tenantRuntime) Prepare(ctx context.Context, request kernelengine.PrepareRequest) (kernelengine.Preparation, error) {
	adapter, err := runtime.adapter(ctx, request.Identity.ExecutionID)
	if err != nil {
		return kernelengine.Preparation{}, err
	}
	return adapter.Prepare(ctx, request)
}

func (runtime *tenantRuntime) Start(ctx context.Context, request kernelengine.StartRequest) (kernelengine.StartResult, error) {
	adapter, err := runtime.adapter(ctx, request.Identity.ExecutionID)
	if err != nil {
		return kernelengine.StartResult{}, err
	}
	return adapter.Start(ctx, request)
}

func (runtime *tenantRuntime) Observe(ctx context.Context, request kernelengine.ObserveRequest) (kernel.Observation, error) {
	adapter, err := runtime.adapter(ctx, request.Identity.ExecutionID)
	if err != nil {
		return kernel.Observation{}, err
	}
	return adapter.Observe(ctx, request)
}

func (runtime *tenantRuntime) Logs(ctx context.Context, request kernelengine.LogsRequest) (kernelengine.LogBatch, error) {
	adapter, err := runtime.adapter(ctx, request.Identity.ExecutionID)
	if err != nil {
		return kernelengine.LogBatch{}, err
	}
	return adapter.Logs(ctx, request)
}

func (runtime *tenantRuntime) Stop(ctx context.Context, request kernelengine.StopRequest) (kernel.StopEvidence, error) {
	adapter, err := runtime.adapter(ctx, request.Identity.ExecutionID)
	if err != nil {
		return kernel.StopEvidence{}, err
	}
	return adapter.Stop(ctx, request)
}

func (runtime *tenantRuntime) Remove(ctx context.Context, request kernelengine.RemoveRequest) (kernelengine.Removal, error) {
	adapter, err := runtime.adapter(ctx, request.Identity.ExecutionID)
	if err != nil {
		return kernelengine.Removal{}, err
	}
	return adapter.Remove(ctx, request)
}

func (runtime *tenantRuntime) RefreshCredential(ctx context.Context, request kernelengine.CredentialRequest) (kernelengine.CredentialRefresh, error) {
	adapter, err := runtime.adapter(ctx, request.Identity.ExecutionID)
	if err != nil {
		return kernelengine.CredentialRefresh{}, err
	}
	return adapter.RefreshCredential(ctx, request)
}

// tenantWorkspace routes every Kernel workspace operation to the adapter of
// the tenant that durably owns the workspace's execution.
type tenantWorkspace struct {
	activation WorkspaceActivation
	tenants    map[id.TenantID]kernelengine.Workspace
}

var _ kernelengine.Workspace = (*tenantWorkspace)(nil)

// newTenantWorkspace validates the tenant set and builds the dispatcher.
func newTenantWorkspace(activation WorkspaceActivation, tenants []ProductionTenant) (*tenantWorkspace, error) {
	if activation == nil {
		return nil, fault.New(fault.InvalidInput, "multi-tenant workspace requires a workspace ownership reader")
	}
	if len(tenants) == 0 {
		return nil, fault.New(fault.InvalidInput, "multi-tenant workspace requires at least one tenant")
	}
	workspace := &tenantWorkspace{
		activation: activation,
		tenants:    make(map[id.TenantID]kernelengine.Workspace, len(tenants)),
	}
	for _, tenant := range tenants {
		if _, err := id.ParseTenantID(string(tenant.TenantID)); err != nil {
			return nil, fault.New(fault.InvalidInput, "multi-tenant workspace tenant id is invalid")
		}
		if tenant.Workspace == nil {
			return nil, fault.New(fault.InvalidInput,
				"multi-tenant workspace requires a workspace adapter for every tenant")
		}
		if _, exists := workspace.tenants[tenant.TenantID]; exists {
			return nil, fault.New(fault.InvalidInput,
				"multi-tenant workspace requires distinct tenants")
		}
		workspace.tenants[tenant.TenantID] = tenant.Workspace
	}
	return workspace, nil
}

// adapter resolves the one tenant workspace that owns the workspace. A missing
// persisted execution is the store's not_found, but for routing it is unknown
// ownership: it is reclassified as a retryable dependency failure so the Kernel
// never reads it as confirmed absence or a completed release.
func (workspace *tenantWorkspace) adapter(ctx context.Context, workspaceID id.WorkspaceID) (kernelengine.Workspace, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tenant, err := workspace.activation.OwningTenant(ctx, workspaceID)
	if err != nil {
		if class, ok := fault.ClassOf(err); ok && class == fault.NotFound {
			return nil, fault.Wrap(fault.DependencyUnavailable,
				"workspace ownership is unavailable", err)
		}
		return nil, err
	}
	adapter, ok := workspace.tenants[tenant]
	if !ok {
		return nil, fault.New(fault.Forbidden,
			"workspace tenant is not served by this installation")
	}
	return adapter, nil
}

func (workspace *tenantWorkspace) Status(ctx context.Context, workspaceID id.WorkspaceID) (kernelengine.WorkspaceObservation, error) {
	adapter, err := workspace.adapter(ctx, workspaceID)
	if err != nil {
		return kernelengine.WorkspaceObservation{}, err
	}
	return adapter.Status(ctx, workspaceID)
}

func (workspace *tenantWorkspace) Retain(ctx context.Context, workspaceID id.WorkspaceID) error {
	adapter, err := workspace.adapter(ctx, workspaceID)
	if err != nil {
		return err
	}
	return adapter.Retain(ctx, workspaceID)
}

func (workspace *tenantWorkspace) Release(ctx context.Context, workspaceID id.WorkspaceID) error {
	adapter, err := workspace.adapter(ctx, workspaceID)
	if err != nil {
		return err
	}
	return adapter.Release(ctx, workspaceID)
}
