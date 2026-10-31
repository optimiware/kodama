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
//   - Authority ends when a step settles: its secrets are revoked before any
//     further step is evaluated and before cleanup completes, by every drive for
//     every settled step, so a crash or a failed release is recovered. Release
//     must be idempotent and work from the step's scope, run, step and declared
//     references alone.
//   - Cancellation blocks new execution but still permits stopping, removal and
//     reconciliation.
//   - Execution outcome and cleanup are separate. A run is terminal only once its
//     owned workloads and resources are confirmed removed, or after the bounded
//     cleanup attempts are exhausted with what remains recorded.
//
// Two limits of the frozen contract are visible here. The run record has no field
// for a step's declared outputs, so the engine reads and records them through its
// own Outputs port (the shell retains them); without one every output is missing.
// And the state store has no release: an engine that parks a run on a wait leaves
// its lease to expire, and a later claim resumes the run, so wake-up latency is
// bounded by LeaseTTL.
//
// The package also still holds the earlier blocking executor (Runner, Backend,
// Process, Podman, Kube and the Git source preparation), which the shell's
// current composition runs on the host store. It is the transitional owner named
// by the pipeline-runtime plan and is removed when the shell cuts over to Engine
// (P3); the two are deliberately not wired together.
package runner
