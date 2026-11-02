// Package kernelrt implements the kernel runtime ports of
// internal/kernel/runtime for Kubernetes: [runtime.Runtime] for step workloads
// (one Job per attempt) and [runtime.ResourceManager] for run-owned workspaces
// (one claim per run) and services (one Pod and Service per declared service).
//
// It imports the kernel contract, internal/domain and the bounded HTTP adapter
// internal/kube/kubernetes only. It does not import the legacy execution,
// gate, workspace or runtime-admission packages, nor their Kubernetes
// executor in the parent package; the retained behavior is ported:
//
//   - Stable identity. Every object is named by a hash of (scope, kind,
//     identity) and carries the full identity as annotations. Start and Ensure
//     are idempotent: an object with the same identity and specification
//     digest is adopted, one with another digest fails with
//     [runtime.ErrSpecMismatch], and one that does not carry the caller's
//     tenant, project, run and identity is foreign and never touched.
//   - Fence record. Before any effect a per-identity record Secret is created
//     (or re-read). It stores the highest writer fence, a state (active,
//     stopped, removed), one in-flight marker per create operation and the UID
//     of each object the identity established. A create or stop from an older
//     fence is refused with [runtime.ErrStaleFence]; a stopped or removed
//     identity is never started again, which also rejects a delayed create.
//     The record is never deleted, not even when the identity is removed: it is
//     the durable closure and fence evidence, so an old writer cannot recreate
//     what was removed at any later time. An effect that provably never
//     reached the cluster wraps [runtime.ErrNotReached]; every other failure
//     is uncertain and the caller observes by identity.
//   - Reserved effects. Every create (Job, claim, Pod, Service, step Secret)
//     first reserves its operation's marker with one version-checked write that
//     succeeds only while the record is active and the writer's fence is
//     current, so a paused writer resumed after a newer Stop or Remove creates
//     nothing. After the create it rechecks authority and deletes what a
//     revoked writer created. A marker is only cleared by the operation that
//     set it and only when its outcome is known; closing an identity never
//     erases it. Removal is confirmed only when every operation's marker is
//     older than [Options.CreateQuiescence].
//   - Lost state is not recreated. The UID of each established object is
//     recorded; an established object that disappears is reported lost (not
//     silently replaced, which would run a step twice or hand a run an empty
//     workspace), and a different instance under the name is refused. A
//     definitely refused first create may be retried.
//   - Re-read before effect. Every delete re-reads the live object, verifies
//     its ownership annotations and Job-controlled Pods, and deletes with a UID
//     precondition. A name that now holds another object is never deleted.
//     Pods whose Job is gone are observed and stopped like the Job's own.
//   - Absence is evidence only when it is safe. A create whose outcome is
//     unknown leaves an in-flight marker; until [Options.CreateQuiescence]
//     has passed, a missing object is reported as unknown, not absent or
//     removed.
//   - Restricted workloads, verified in full. Every container is non-root (the
//     configured UID/GID), has no privilege escalation, drops all capabilities,
//     uses the RuntimeDefault seccomp profile, has CPU, memory and
//     ephemeral-storage requests and limits, runs a digest-pinned image, and
//     mounts no service account token. Adoption and observation verify the
//     whole server document of the Job template, each live Pod (which may carry
//     a scheduler-assigned nodeName and the API server's documented defaults),
//     the workspace claim and the Service against the permitted shape and the
//     recorded intent (volumes, mounts, credentials, claim name); any other
//     field is refused until reviewed.
//   - Workspace release. Removing a workspace reports [ReleaseEvidence]: the
//     claim is gone, every volume whose claimRef names it (not only the one
//     seen bound) is gone and not attached, and enough time has passed since
//     the claim disappeared for a binding in progress to have shown. A retained
//     volume is reported as failed cleanup, never as removed.
//   - Orphan reconciliation. [Adapter.Reconcile] removes run-owned objects of
//     one scope that no committed state references, after a grace period,
//     and reports what it could not remove.
//
// # Capabilities
//
// The adapter declares non_root, resource_limits, image_pinning,
// adopt_workload, fenced_stop and run_services; those are enforced by this
// package or by the kubelet from the objects it writes. It declares
// network_isolation only when the operator attests, through
// [Config.NetworkPolicyEnforced], that the cluster's CNI enforces
// NetworkPolicy; even then every start that requires it verifies the
// namespace's effective policy: a default-deny policy exists and no other
// policy that selects the adapter's Pods allows traffic beyond the same
// namespace, DNS and public web egress. It never declares
// persistent_services: project and tenant lifetime services are refused.
//
// # Limits of this evidence
//
// The package is unit-tested against an in-memory cluster double. Those tests
// prove the mapping from kernel intent to API objects and back. They do not
// prove CSI provisioning or reclaim, CNI enforcement, quota admission, image
// pulling or kubelet behavior; that is the real conformance qualification.
//
// Not provided by this slice and refused (never silently ignored) with an
// error wrapping both [runtime.ErrNotReached] and [runtime.ErrCapability]:
// workspace preparation from sources or credentials (the kernel leaves the
// locator opaque and no preparation image contract exists), service secret
// bindings, and non-pipeline service lifetimes.
//
// Log reads are bounded snapshots of the step container; Follow is satisfied
// by the snapshot because the Kubernetes adapter does not stream.
package kernelrt
