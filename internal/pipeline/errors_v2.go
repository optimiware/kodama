package pipeline

// Diagnostic codes introduced by kodama/v1alpha2 (docs/contracts/
// pipeline-format-v1alpha2.md). Like every code they are stable identifiers; the
// messages are constant text that never echoes document content.
const (
	// CodeFormatDisabled: the document is kodama/v1alpha2 and the installation
	// has not enabled the format. Representability is not installation support.
	CodeFormatDisabled = "format_disabled"

	CodeWorkspaceFromRemoved = "workspace_from_removed"
	CodeUnknownService       = "unknown_service"
	CodeUnknownPort          = "unknown_port"
	CodeDuplicatePort        = "duplicate_port"
	CodeInvalidProbe         = "invalid_probe"
	CodeInvalidResources     = "invalid_resources"
	CodeInvalidStorage       = "invalid_storage"
	CodeInvalidEdge          = "invalid_edge"
	CodeInvalidEgress        = "invalid_egress"
	CodeInvalidRole          = "invalid_role"

	CodeEdgeUndeclared               = "edge_undeclared"
	CodeServiceSelfReference         = "service_self_reference"
	CodeServiceStepReference         = "service_step_reference"
	CodeServiceSecretRequiresCommand = "service_secret_requires_command"
	CodeStepTimeoutExceedsRun        = "step_timeout_exceeds_run"
)

// Admission refusal codes of the run_shared model. This module defines the
// spelling so execution, the API and the CLI share one set; run admission (not
// document validation) decides when each applies, because each depends on the
// run, the installation policy or the selected runtime.
const (
	AdmissionFormatRetired             = "format_retired"
	AdmissionCapabilityUnsupported     = "capability_unsupported"
	AdmissionPolicyUnavailable         = "policy_unavailable"
	AdmissionLimitExceedsPolicy        = "limit_exceeds_policy"
	AdmissionEgressProfileUnknown      = "egress_profile_unknown"
	AdmissionWorkspaceNoneSourceBacked = "workspace_none_source_backed"
	AdmissionWorkspaceAccessSourceFree = "workspace_access_source_free"
	AdmissionAgentSourceFree           = "agent_source_free"
)

// v2ConstantMessages are the messages of the diagnostics the shared kodama/v1alpha1
// rules fill with document identifiers (step IDs, input and output names). The
// kodama/v1alpha2 contract promises constant messages that echo no document
// content, so a v1alpha2 validation replaces these, by code, after the shared
// rules ran. The kodama/v1alpha1 messages are unchanged. The path still locates
// the finding.
var v2ConstantMessages = map[string]string{
	CodeDuplicateStep:   "the step id is already used by another step",
	CodeCycle:           "the dependencies contain a cycle",
	CodeParallelWriters: "two steps write the workspace but neither depends on the other",
	CodeUnknownInput:    "the referenced input is not declared",
	CodeUnknownStep:     "the referenced step does not exist",
	CodeNotAncestor:     "the referenced step is not a dependency of this step, so its outputs are not available",
	CodeUnknownOutput:   "the referenced step declares no such output",
	CodeTypeMismatch:    "the value has a type this field does not accept",
}

// constantMessages applies v2ConstantMessages to the collected findings.
func (d *diagnostics) constantMessages() {
	for i := range d.list {
		if m, ok := v2ConstantMessages[d.list[i].Code]; ok {
			d.list[i].Message = m
		}
	}
}
