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
//     (or re-read). It stores the highest writer fence and a state (active,
//     stopped, removed). A create or stop from an older fence is refused with
//     [runtime.ErrStaleFence]; a stopped or removed identity is never started
//     again, which also rejects a delayed create. An effect that provably never
//     reached the cluster wraps [runtime.ErrNotReached]; every other failure
//     is uncertain and the caller observes by identity.
//   - Re-read before effect. Every delete re-reads the live object, verifies
//     its ownership annotations and Job-controlled Pods, and deletes with a UID
//     precondition. A name that now holds another object is never deleted.
//   - Absence is evidence only when it is safe. A create whose outcome is
//     unknown leaves an in-flight marker; until [Options.CreateQuiescence]
//     has passed, a missing object is reported as unknown, not absent or
//     removed.
//   - Restricted workloads. Every container is non-root (the configured
//     UID/GID), has no privilege escalation, drops all capabilities, uses the
//     RuntimeDefault seccomp profile, has CPU, memory and ephemeral-storage
//     requests and limits, runs a digest-pinned image, and mounts no service
//     account token. Adoption re-verifies these fields on the live object.
//   - Workspace release. Removing a workspace reports [ReleaseEvidence]: the
//     claim is gone, and the bound volume is gone and not attached. A retained
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
// NetworkPolicy; even then every start that requires it verifies that the
// target namespace holds a default-deny policy. It never declares
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
