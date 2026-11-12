// Package kube implements the production Kubernetes Job runtime adapter.
//
// The adapter talks to the cluster only through the small Cluster seam in
// api.go. It is deliberately independent of client-go: unit tests drive a
// deterministic in-memory fake, and no network, process, container or cloud
// call happens here.
//
// The adapter implements the declared runtime.Client contract exactly and also
// implements the declared Kernel engine.Workspace port for the retained
// workspace claim. Workspace availability is observation only, retention is a
// durable claim marker, and release is conditional on the absence of every
// Job/Pod reference and a matching resource version. It consumes the persisted
// engine.EffectIdentity for every operation, so a crash before or after an
// effect cannot derive a different workload, and a retry converges on one
// stable Job. Observation and stop evidence stay in the declared Kernel
// vocabulary:
//
//   - Job deletion or absence, pod disappearance, lease expiry, elapsed time
//     and transport loss never prove that a command stopped;
//   - a Job with no pod yet is still running work;
//   - two pods for one identity are unknown, never a silent replay;
//   - only an affirmatively terminated container, or the same UID-bound Job
//     suspended into an acknowledged zero active/terminating state with a fresh
//     empty pod list, confirms the stop barrier. Stop never deletes the Job and
//     never adds a foreground deletion or a pod finalizer.
//
// TTL cleanup. The workload Job is created with spec.ttlSecondsAfterFinished
// and the TTL controller removes a finished Job with foreground propagation,
// taking its pods. Stop's single UID-guarded suspension PATCH therefore also
// carries an explicit ttlSecondsAfterFinished: null, so a Job that finishes
// while it is suspended is not TTL-deleted before Remove deletes it: the
// durable object and its affirmatively terminated pod stay available as stop
// evidence. A Job that already carries a deletion timestamp (a TTL or
// foreground deletion that started before this Stop) is never suspension
// evidence and is not patched, but its workload pods are still listed and the
// independent terminated-container rule decides: an affirmatively terminated
// container can confirm, while absence of pods, a running pod and a terminating
// pod without terminated-container evidence stay pending. If the Job and every
// pod are already gone, nothing can prove termination: Stop stays pending
// rather than guessing, and cleanup waits for an operator.
//
// Node loss. If the node running a workload is lost, its kubelet stops
// reporting and cannot confirm that the workload container terminated. While
// the pod object remains, normally as a terminating pod with the last-known
// Running state and no terminated-container evidence, Stop returns PendingStop
// with a "stop unconfirmed" detail and never a confirmed stop: a lost node, a
// terminating pod, a suspended Job whose active/terminating counters have not
// settled, and elapsed time are all uncertain. Observe likewise never reports
// an exit for that pod: a terminating pod keeps its last-known running state
// and an unknown or empty phase stays unknown, so nothing on the lost node is
// reported as exited. The adapter cannot prove that a process on an unreachable
// node stopped and does not claim to. An operator must restore or replace the
// node and let the same suspended Job's pod deletion complete and its counters
// settle; only a later Stop that observes the acknowledged zero state on that
// same Job confirms. Force-deleting the pod or the Node object can make the Job
// controller report zero active/terminating from API-server bookkeeping alone,
// which the guarded Job-status branch would accept; that is not
// kubelet-confirmed process termination, so do not force removal while the
// process may still be writing to the retained workspace. Until a durable
// suspended-Job UID anchor exists, a restarted process has to re-identify the
// Job by its deterministic name and trust the UID it observes, so a Job
// recreated under that name cannot be checked against the originally launched
// UID.
//
// Preparation materializes the activation snapshot once as an immutable
// ConfigMap plus a mutable credential Secret, and attaches the execution's
// pre-existing retained workspace claim. Preparation never starts a workload
// and never recreates a missing workspace. When configured, an installation CA
// bundle is projected from a tenant-local, CA-only Secret as a fourth read-only
// volume at the fixed trust path; the adapter never reads or re-materializes it,
// and the serving private key is never projected into a workload.
package kube
