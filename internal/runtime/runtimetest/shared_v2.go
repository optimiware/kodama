package runtimetest

import (
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// SharedSpecV2 is SharedSpec for an attempt whose pinned context is the
// run_shared ContextV2 the admission wire serves: the source-free projection of
// SharedSpec's context, bound to the same run, step, attempt and command.
//
// The bootstrap issuer decodes a run_shared attempt's context as ContextV2
// (AttemptSpec.DecodeContextV2) while a runtime's own Start decodes the v1
// context (AttemptSpec.DecodeContext), which refuses a v2 document. A spec from
// SharedSpec therefore starts under a runtime but cannot be issued a credential,
// and a spec from SharedSpecV2 can be issued one but is not startable by a
// runtime that decodes v1. A test chooses the one its seam needs.
func SharedSpecV2(t testing.TB, workspace execution.ResourceHandle, image, script string) execution.AttemptSpec {
	t.Helper()
	s := SharedSpec(t, workspace, image, script)
	c, err := s.DecodeContext()
	if err != nil {
		t.Fatal(err)
	}
	launch := s.Shared
	c2 := runtimev2.ContextV2{
		ContextVersion: runtimev2.ContextVersion, RunModel: runtimev2.RunModel,
		TenantID: c.TenantID, ProjectID: c.ProjectID, RunID: c.RunID, StepID: c.StepID, AttemptID: c.AttemptID, ActionKey: c.ActionKey,
		Definition: c.Definition, WorkItem: c.WorkItem, DesignBaseline: c.DesignBaseline, Profile: c.Profile, Command: c.Command,
		Workspace: runtimev2.WorkspaceV2{Access: runtimev1.AccessNone, MountPath: runtimev2.MountPath, Lineage: []runtimev2.LineageEntry{}},
		Endpoints: map[string]runtimev2.Endpoint{}, Egress: []string{},
		Run:    runtimev2.RunWindow{StartedAt: "2026-10-26T10:00:00Z", DeadlineAt: "2026-10-26T11:00:00Z"},
		Policy: runtimev2.PolicyPin{ID: launch.Policy.ID, Digest: runtimev1.Digest(launch.Policy.Digest)},
		Inputs: c.Inputs, Outputs: c.Outputs, Secrets: c.Secrets, Environment: c.Environment, Limits: c.Limits,
	}
	digest, doc, err := runtimeauth.PinContextV2(c2)
	if err != nil {
		t.Fatal(err)
	}
	s.Context, s.ContextDigest = doc, "sha256:"+digest.Hex()
	if _, err := s.DecodeContextV2(); err != nil {
		t.Fatal(err)
	}
	return s
}
