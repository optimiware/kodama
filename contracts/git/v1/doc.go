// Package gitv1 is the wire contract of the internal Git service (ADR 0011,
// docs/specs/git-repositories.md, docs/contracts/git-service.md): the
// repository resource, Git grant claims, the credential exchange, the
// forwarding assertion and worker identity, the worker-to-control-plane and
// control-plane-to-worker RPC, push records, the receive-attributed ref
// journal and its settlement decision, source sets and trigger
// configuration, plus the branch namespace helpers every consumer shares.
//
// Like the runtime contracts it holds shapes, strict decoding, validation,
// stable codes and pure derivations; it contains no storage, process, route or
// signer. Requests are decoded strictly (unknown, misspelled, differently
// cased, duplicate and omitted members are rejected, so a nullable member is an
// explicit null) and responses tolerantly (compatible additions are ignored,
// required members must be present); both reuse the runtime v1 decoder, so
// numbers are integers within +/-(2^53-1) and strings are valid UTF-8.
// Members documented as optional (create and patch bodies, a trigger's
// excludeOriginRuns) are omitted, never null.
//
// The object operations of the control plane's maintenance and recovery
// (T-160: IsAncestor, HasCommit, CommitTime, CreateBundle, ReplayBundle and
// SetRestoreFence; T-168: ReadFiles, in objects.go) keep the tenant volume unmounted. Every
// request names the repository by ID only. CreateBundle answers, and
// ReplayBundle is requested, with BundleMediaType: one compact JSON line (the
// manifest, or the request) terminated by "\n", then the raw bundle bytes; read
// the line with ReadFrameLine and write it with FrameLine. A replay only creates
// refs that are missing and, with the repository fenced for restore, is the only
// mutation the worker admits. A CreateBundle manifest's Consistent flag is about concurrent mutation only: a bundle taken while the restore fence is raised is still consistent.
//
// The package does not import any capability package. Persistence, the HTTP
// and RPC servers, the signers and the Git hooks belong to the consumers named
// in contracts/git/README.md; a fixture here proves a shape, never that an
// endpoint behaves.
package gitv1

// Interface constants.
const (
	Version = "v1"

	// GrantAudience is the audience of Git grants and the Git credentials
	// derived from them.
	GrantAudience = "kodama-git"
	// ControlAudience is the audience of a worker identity: the worker calls the
	// control plane under it.
	ControlAudience = "kodama-git-control"
	// WorkerAudiencePrefix prefixes the audience of a forwarding assertion; the
	// complete audience is WorkerAudience(tenantID).
	WorkerAudiencePrefix = "kodama-git-worker:"

	// ControlBasePath is the control plane's internal route family for workers;
	// {tenantId} is the tenant of the worker identity. These routes are not part
	// of the public API.
	ControlBasePath = "/internal/git/v1/tenants/{tenantId}"
	// WorkerBasePath is the worker's route family for the control plane,
	// reached through the tenant's Service.
	WorkerBasePath = "/internal/worker/v1"
)

// WorkerAudience returns the forwarding-assertion audience of a tenant's worker.
func WorkerAudience(tenantID string) string { return WorkerAudiencePrefix + tenantID }
