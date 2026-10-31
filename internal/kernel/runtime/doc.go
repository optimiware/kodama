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
//   - A wait commits before it is announced and takes exactly one decision
//     (resolved, expired or cancelled) in one version-checked step, so
//     resolution, expiry and cancellation race on a single check against the
//     store's clock. A wait declares a closed set of outcomes and at most one
//     approving outcome; only that outcome can satisfy a protected step's
//     requirement, and a rejection is an ordinary recorded outcome, not a
//     failure.
//   - Signals are durable. A [SignalIdentity] is (tenant, project, caller
//     principal, caller key). [RouteSignal] looks the identity up before any
//     wait matching: a [SignalRecord] (original target and the complete
//     decision) or a smaller [SignalTombstone] (a refusal that resolved no
//     target) returns its recorded result permanently and is never re-routed.
//     Neither expires; they are removed only with their project. A changed
//     request under a recorded key is [ErrKeyReuse] and writes nothing.
//   - There is no condition step kind. Step kinds are container and wait;
//     conditional eligibility is a when predicate that the engine evaluates once
//     over persisted values. The step retains a [PredicateEvaluation] (result and
//     a digest of the values read), committed with the step's move and never
//     changed afterwards ([CheckRunUpdate]); recovery reuses it. A Transition
//     advances the run version by exactly one: [Run.EvaluateStep] records an
//     evaluation and moves its step in that single increment, and
//     [Run.Sealed] collapses any other batch of moves into one.
//   - An admitted [Plan] is plain values for the frozen pipeline format: join,
//     declared outputs, bindings, and run inputs that keep their declared type
//     ([InputValue]) so the no-coercion grammar can tell the string "1" from the
//     integer 1. Parsing and evaluation live outside this package.
//   - A claim is a lease with owner, expiry and a monotonic [Fence]. The owner
//     extends it with [RenewClaim], which keeps the fence and is persisted
//     atomically; [AcquireClaim] never renews. A stale writer cannot commit or
//     renew after takeover. Database fencing is not enough for
//     external effects: the fence travels in every [RuntimeBinding] and
//     [ResourceHandle] so an adapter can refuse a delayed create.
//   - A step's declared outputs are durable. A [StepOutputs] record is committed
//     in the same [Transition] that settles the step as succeeded ([CheckOutputs]
//     refuses names the plan did not declare, oversized or non-text values, and
//     outputs added after the settlement) and is the authority every later claim
//     reads; no engine-side store backs it.
//   - A holder can give a claim up without ending the run ([StateStore.Release],
//     [ReleaseClaim]): scope, owner and fence are checked, the fence and the run
//     are kept, the lease ends now, and a holder that no longer holds the claim
//     changes nothing. A run parked on an open wait is released so resumption does
//     not wait for the lease.
//   - Secret material is revocable from the record. A [MaterializationRef] is the
//     scoped identity of one step attempt's private material, derived from scope,
//     run, step and attempt, recorded as issued before [SecretResolver.Resolve] is
//     called and as revoked after [SecretResolver.Revoke] confirmed
//     ([CheckMaterializations]); revocation needs the identity alone, never a
//     directory, so a replacement engine revokes what it never saw.
//   - Execution outcome and cleanup are separate. A run is terminal only after
//     its owned resources are confirmed stopped and removed, or after bounded
//     cleanup attempts are exhausted with the retained resource identities
//     recorded. Workloads whose stop or removal is uncertain are retained by
//     their [RuntimeBinding], like resources. After exhaustion only a cleanup
//     claim is admitted, so late reconciliation can still confirm the retained
//     items without reopening execution.
//   - Durable records carry the execution evidence recovery needs: step
//     attempts and results, command bindings and outcomes, the latest workload
//     observation per attempt and recorded wait decisions ([RunRecord]), so a
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
