// Package runner progresses pipeline runs.
//
// The durable progression engine (Engine) drives a run through the kernel ports
// of internal/kernel/runtime only: a StateStore, a Runtime, a ResourceManager, a
// SecretResolver, a WorkspacePreparer, and a Clock. It holds no run state of its
// own. Every decision is made from the durable record loaded under a claim, so
// any engine instance can take over a run that another one left behind:
//
//   - A run is claimed with a lease and a monotonic fence. Every commit carries
//     the fence and renews the lease; a superseded writer is refused and stops.
//   - The plan is the immutable admission snapshot in the record, never re-read
//     from a pipeline definition.
//   - Intent before effect. A command naming the workload or resource identity,
//     its specification digest and the fence commits first; only then is the
//     backend called. A launch that returns ErrNotReached is the only one issued
//     again, as the next attempt; every other failure is uncertain and is
//     reconciled by observing the workload by its stable identity, never
//     launched again.
//   - One executable step holds the workspace at a time. A step settles, its
//     secrets are revoked and its workload is removed before the next step starts.
//   - Eligibility is evaluated once, when every prerequisite is terminal: the
//     join, then the when predicate, then a protected step's requirements. The
//     predicate's result and a digest of what it read are committed with the
//     step's move and reused on recovery.
//   - A wait commits before it is announced. Resolution, expiry and cancellation
//     are one version-checked decision; the engine consumes the recorded decision
//     in one commit, and the deadline's declared branch is taken through when.
//   - Authority ends when a step settles. The identity of a step attempt's secret
//     material (runtime.MaterializationRef, derived from scope, run, step and
//     attempt) is committed to the record before the SecretResolver is asked to
//     create it, and committed again once the resolver revoked it. Any engine,
//     after a takeover or a crash at any point, revokes the recorded
//     materializations of settled steps by that identity alone, before any further
//     step is evaluated and before cleanup completes, retrying a failed revocation
//     in place, and then resumes the run. The one hold that remains is a real
//     condition: with material to revoke and no resolver configured, the engine
//     holds the run (DriveHeld, ReasonRevocationNoResolver) rather than advance
//     or skip.
//   - Cancellation blocks new execution but still permits stopping, removal and
//     reconciliation.
//   - Execution outcome and cleanup are separate. A run is terminal only once its
//     owned workloads and resources are confirmed removed, or after the bounded
//     cleanup attempts are exhausted with what remains recorded.
//
// A step's declared outputs and the claim's life are part of the record and the
// store contract: a succeeded step's outputs are committed with its settlement
// (runtime.StepOutputs) and read from the record by every later claim, so an
// OutputCollector is only the source at settlement; and an engine that parks a run
// on an open wait releases its claim (StateStore.Release), so the next claim made
// when a signal or the deadline wakes the run does not wait for the lease to
// expire.
//
// The package also still holds the earlier blocking executor (Runner, Backend,
// Process, Podman, Kube and the Git source preparation), which the shell's
// current composition runs on the host store. It is the transitional owner named
// by the pipeline-runtime plan and is removed when the shell cuts over to Engine
// (P3); the two are deliberately not wired together.
package runner
