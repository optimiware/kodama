package bootstrap

import (
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/execution"
)

// LaunchContext is the runtime context a runtime hands its Program for an
// attempt, decoded as the version the spec carries: an attempt-private spec
// pins a v1 context (AttemptSpec.DecodeContext, unchanged), a run_shared spec
// (Shared != nil) pins the ContextV2 that Issuer registers (DecodeContextV2),
// and a document of the other version is refused as unreadable, so a runtime
// never starts an attempt its issuer cannot admit.
//
// For a run_shared attempt the result is the v1 projection of the ContextV2
// that a Program can read: the fields the two wires share, with a source-free
// workspace. The attempt's source is the run workspace its preparation filled,
// so no checkout material applies and the v2 workspace, endpoints, egress, run
// window and policy stay in the pinned document the entrypoint is admitted with.
func LaunchContext(spec execution.AttemptSpec) (runtimev1.Context, error) {
	if spec.Shared == nil {
		return spec.DecodeContext()
	}
	c, err := spec.DecodeContextV2()
	if err != nil {
		return runtimev1.Context{}, err
	}
	return runtimev1.Context{
		TenantID: c.TenantID, ProjectID: c.ProjectID, RunID: c.RunID, StepID: c.StepID, AttemptID: c.AttemptID, ActionKey: c.ActionKey,
		Definition: c.Definition, WorkItem: c.WorkItem, DesignBaseline: c.DesignBaseline, Profile: c.Profile, Command: c.Command,
		Workspace: runtimev1.Workspace{Access: runtimev1.AccessNone}, Inputs: c.Inputs, Outputs: c.Outputs, Secrets: c.Secrets,
		Environment: c.Environment, Limits: c.Limits,
	}, nil
}
