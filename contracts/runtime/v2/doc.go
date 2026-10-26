// Package runtimev2 is the wire contract of the run_shared runtime interface
// (/runtime/v2): the immutable ContextV2, the command-begin fence, and the
// resource-subject messages of the preparation workload and run services. The
// prose contract is docs/contracts/runtime-v2.md.
//
// It reuses the frozen v1 DTOs, validators, strict/tolerant decoding and
// canonicalization of package runtimev1 unchanged for everything the two
// versions share (admission, tokens of the attempt audience, secrets,
// artifacts, source proposals, results and ResultHash): those are exported here
// as aliases only, so a result's hash never depends on the context version and
// no v1 validator is modified. New messages never reuse the v1 token or Job
// identity types, because those require the attempt audience and a Job/Pod.
//
// Like v1, the package holds shapes, strict decoding, validation, error codes
// and statuses, an operation table and two pure derivations (begin and
// receipt). It contains no server, client, storage or enforcement: the HTTP
// handlers, authority and lifecycle that serve these messages are delivered by
// later tasks, and a fixture here proves a shape, never that an endpoint
// behaves. The package is not enabled: until the installation serves v2, only
// /runtime/v1 exists.
package runtimev2

// Interface constants.
const (
	Version = "v2"
	// AttemptBasePath and ResourceBasePath are the two route families.
	AttemptBasePath  = "/runtime/v2/attempts/{attemptId}"
	ResourceBasePath = "/runtime/v2/runs/{runId}/resources/{resourceKey}"
	// AttemptAudience is the audience of attempt tokens, identical to v1.
	AttemptAudience = "kodama-runtime"
	// ResourceAudience is the audience of preparation and service tokens.
	ResourceAudience = "kodama-runtime-resource"

	// ContextVersion is the integer every ContextV2 and ResourceContext carries.
	ContextVersion int64 = 2
	// RunModel is the run-model discriminator of a v2 context.
	RunModel = "run_shared"
	// MountPath is where every executable step sees the run scratch.
	MountPath = "/workspace"
)
