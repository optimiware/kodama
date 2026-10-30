// Package runtimev3 is the wire contract of the source-set runtime interface
// (/runtime/v3) for attempts of runs that pin a source set of internally hosted
// repositories (ADR 0011, PX-23–PX-25): the immutable ContextV3 with the run's
// origin, source set and per-attempt Git credential delivery, the begin fence
// over every source, and the result with per-repository commit outputs in place
// of the single sourceCommit.
//
// It is an additive version. The v1 and v2 DTOs, validators, fixtures, schemas,
// operation tables and result hashes are untouched, and everything v3 shares
// with them (admission, attempt tokens, secrets, artifacts, errors, endpoints,
// run window, policy pin) is reused by alias or by projection onto the
// unchanged v2/v1 validators rather than copied. ResultHash is the v1
// canonicalization algorithm applied to the v3 result object; a v3 result never
// has the same bytes as a v1 result, so no historical hash can be confused.
//
// Like its predecessors the package holds shapes, strict decoding, validation,
// the operation table and pure derivations. It contains no server, client,
// storage, Git operation or enforcement: the credential file is delivered by a
// runtime adapter, the grant is issued and revoked by run admission, and the
// pushes are made by the entrypoint. A fixture here proves a shape, never that
// any of them behaves.
//
// The resource subjects of a v3 run are served on /runtime/v3 too: the one
// preparation workload checks out every source of the set (ResourceContextV3
// with PreparationContextV3, a read-only preparation Git delivery and a
// per-source PreparationReceiptRequestV3), while the service messages are the
// unchanged v2 ones (a service never mounts /workspace nor holds a Git
// credential, so nothing in them depends on the source set); only the context
// version differs, so a v3 run's resource is never served as a v2 one.
package runtimev3

// Interface constants.
const (
	Version = "v3"
	// AttemptBasePath is the route family of attempts. The token audience is the
	// unchanged attempt audience of v1 and v2.
	AttemptBasePath = "/runtime/v3/attempts/{attemptId}"
	AttemptAudience = "kodama-runtime"

	// ContextVersion is the integer every ContextV3 carries.
	ContextVersion int64 = 3
	// RunModel is the run-model discriminator of a v3 context: the run_shared
	// model of ADR 0010 with a source set.
	RunModel = "run_shared"
	// MountPath is where every executable step sees the run scratch; a source is
	// checked out at its path beneath it.
	MountPath = "/workspace"

	// GitDirectory is the only directory the Git credential and CA files live in.
	// It is created by the runtime adapter, never by the workload, and holds
	// nothing but the files the context names.
	GitDirectory = "/kodama/git"
	// GitUsername is the conventional username of the credential; the Git
	// service accepts any username at use.
	GitUsername = "kodama"
)
