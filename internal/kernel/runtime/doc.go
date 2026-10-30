// Package runtime is the contract of Kodama's standalone pipeline runtime: the
// execution model (runs, steps, waits, resources, claims, command intents) and
// the narrow ports an execution engine depends on. It holds value types, pure
// invariants and interfaces only. It contains no engine, no adapter and no
// scheduler, and it imports nothing but the standard library and
// internal/domain, so the kernel runs without PostgreSQL, a broker, Git, HTTP,
// tasks, identity or agents.
//
// The process definition stays in internal/kernel/pipeline; this package
// models one admitted execution of it. A [Plan] is the admission-time snapshot
// of a pipeline document in execution order, expressed as plain value types
// rather than a second document model.
//
// # Transaction and effect boundary
//
// State and effects never share a transaction:
//
//   - State commits with outbox intent. [StateStore.Commit] persists a run
//     transition, the wait and command records it changes, and the [Event]
//     intents describing it in one atomic, claim-fenced, version-checked step.
//     The shell publishes those events afterwards; the kernel never publishes.
//   - Command intent is committed before an external effect. A [Command]
//     naming the workload or resource identity, its immutable specification
//     digest and the writer's fence is recorded first; only then does the
//     engine call a [Runtime] or [ResourceManager]. External calls run outside
//     any transaction.
//   - Uncertain outcomes are reconciled, never replayed. An effect that
//     returns an error is classified by [ClassifyEffect]: only an error that
//     wraps [ErrNotReached] proves the command never reached the backend and
//     permits a retry. Every other failure is uncertain; the engine observes
//     the workload or resource by its stable identity and adopts or stops it.
//   - A wait commits before it is announced, and one version-checked decision
//     settles the race between resolution, expiry and cancellation. A
//     duplicate resolution returns the recorded result.
//   - A claim is a lease with owner, expiry and a monotonic [Fence]. The owner
//     extends it with [RenewClaim], which keeps the fence and is persisted
//     atomically; [AcquireClaim] never renews. A stale writer cannot commit or
//     renew after takeover. Database fencing is not enough for
//     external effects: the fence travels in every [RuntimeBinding] and
//     [ResourceHandle] so an adapter can refuse a delayed create.
//   - Execution outcome and cleanup are separate. A run is terminal only after
//     its owned resources are confirmed stopped and removed, or after bounded
//     cleanup attempts are exhausted with the retained resource identities
//     recorded. Workloads whose stop or removal is uncertain are retained by
//     their [RuntimeBinding], like resources. After exhaustion only a cleanup
//     claim is admitted, so late reconciliation can still confirm the retained
//     items without reopening execution.
//   - Durable records carry the execution evidence recovery needs: step
//     attempts and results, command bindings and outcomes, the latest workload
//     observation per attempt and consumed wait results ([RunRecord]), so a
//     restarted engine reconciles by stable identity with no transient backend
//     configuration.
//
// # Scope
//
// Tenant and project scope ([domain.Scope]) are explicit fields on every
// command, handle, binding, observation, signal, claim and event. No port
// reads scope, identity or time from ambient context. Resource ownership is
// separate from the requesting run's scope: a tenant-lifetime service is owned by
// the tenant alone ([domain.Owner] without a project) and shared by its
// projects, while every handle keeps its owner and lifetime.
//
// # Workspace ownership
//
// Exactly one port creates or adopts a run's workspace: [ResourceManager],
// through Ensure on a workspace [ResourceSpec]. [WorkspacePreparer] only
// supplies the pinned [PreparationInputs] that Ensure consumes; it never
// creates, adopts, mutates or removes a workspace itself.
package runtime
