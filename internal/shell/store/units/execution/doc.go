// Package executionstore implements the execution, request and Kernel-state
// ports over the neutral store seams: store.Executor/store.Scanner for scoped
// capability units and store.Controller for the scope-free Kernel engine.
//
// Each port maps one complete domain commit unit to exactly one store.Unit:
// the capability service or the pure Kernel reducer decides every state
// transition, authorization result, receipt, audit row and event publication
// intent, and this adapter only persists the facts the unit already carries.
// There is no SQL, no concrete store import and no duplicated Kernel policy.
//
// # Shell predecessor barrier
//
// The Kernel stays task-independent, so the Shell handoff rule is enforced here
// from the persisted executions.task_id relationship: an execution may be
// discovered for activation, or commit an activation transition, only after
// every other execution of its task created before it has confirmed stop (a
// strictly later sibling is a successor; siblings created at the same instant
// are ordered by whether they have left the queue and then by identity, see
// taskBarrier). Discovery omits blocked work (keeping the predecessor's own
// cleanup and unrelated work in the same scan); the commit boundary refuses
// with version_conflict and locks every execution row the barrier relied on at
// the version it read. StopEvidence
// is the one persisted derivation shared with the task service's scoped
// ExecutionReader, so the reply decision and this authoritative barrier cannot
// drift. Unreadable evidence fails closed.
//
// # Ports
//
//	engine.State            Engine.Load, Engine.Commit, Engine.Scan
//	execution.StateReader   Store.Load
//	execution.Reader        Store.Inspect, Store.List
//	execution.StartPort     Store.Start
//	execution.TransitionPort Store.Transition
//	request.StateReader     Store.Load
//	request.ActivationPort  Store.Activate
//	request.ResolvePort     Store.Resolve
//	task.ExecutionAggregateReader Store.Load (the scoped cancel pre-image)
//
// # Reusable non-committing mapper
//
// PlanScopedCommit is the one row mapping of a flow.Commit shared by every
// Shell path: it returns the execution/action/cleanup locks and writes a scoped
// caller embeds in its own store.Unit, performs no commit and produces no
// events. The engine path (planCommit) adds the lifecycle projection; the
// sibling work store unit uses the same Plan so the task cancel reduction is
// persisted without duplicating SQL or diverging from the engine rows.
//
// # Port to table mapping
//
//	executions  id, pipeline_id, task_id, principal_id, state, version,
//	            yaml_sha256, snapshot, token_nonce, token_expires_at,
//	            workspace_id, failure, created_at
//	actions     id, execution_id, index, name, kind, state, version,
//	            timeout_ms, deadline, outputs, staged_outputs,
//	            staged_outputs_set, stop, request_subject, request_assignment,
//	            request_decision, request_actor, request_resolved_at,
//	            workload_id, launch_state, launch_key, observation,
//	            activated_at, answer_provenance
//	cleanup     deterministic id, execution_id, workload_id, workspace_id,
//	            state, attempts, last_error, next_retry_at
//	receipts/audit/events  supplied through store.Unit.Receipt/Audit/Events
//
// executions.snapshot stores the immutable admitted definition snapshot as one
// JSON document written and read only through the shared snapshotdoc package
// (exact YAML bytes, digest, inputs, admitted Env defaults, runtime
// requirements, task bytes with their present/absent marker and the admitted
// pipeline container and request definitions plus the pipeline revision). It is
// written once by Start and never rewritten, so the retained bytes round-trip
// exactly; task and trigger starts use the same codec, so every started
// execution loads through this adapter.
//
// executions.workspace_id is derived deterministically from the execution
// identity by snapshotdoc.DeriveWorkspaceID and written by Start. Load returns
// it as the execution workspace, and every container action of that execution
// carries it, including across a planned request wait (the same execution keeps
// the same workspace).
//
// Kernel evidence maps one-to-one: a launch intent to actions.workload_id,
// launch_state and launch_key; an observation to actions.observation; the
// activation time to actions.activated_at; an answer to request_decision/
// request_actor/request_resolved_at, answer_provenance and the shared
// actions.outputs response column; a typed terminal failure to executions.failure.
// CurrentAction is derived from the single running action row, and a failed
// answer's retained response is kept off Action.Outputs when Load rebuilds it.
//
// # Request activation authority
//
// The Kernel reconciler is authoritative for activating an execution's first
// pending action, including a request on a still-queued execution: advanceQueued
// reduces flow.ActivateFact and commits the complete transition through
// Engine.Commit, which persists the running action, activation time, absolute
// deadline and resolved subject. The Shell request.ActivationPort is not on that
// path and is not expected to commit for a queued execution; it emits its
// execution_running/execution_version authority reads only for a running
// execution's pending successor, and the adapter accepts exactly those reads.
// The admitted assignment is immutable, so Start writes it when the request row
// is inserted (through snapshotdoc.RequestAssignment) and the activation commit
// rewrites the same encoding beside the resolved subject; no evidence reader
// ever sees an engine-activated request row with a NULL assignment.
//
// Every commit is validated before its plan is built. An execution-only commit
// that carries any action-scoped field (a next action state or expected action
// version, a non-discard output change, a launch intent, observation, subject,
// answer or stop evidence) is invalid_input rather than silently dropped, and
// the next execution/action states and output action are validated. The
// schema has no current-action column, so the adapter never persists
// NextCurrentAction: Load derives it from the single running row. A nil value
// means "derive it from the action rows"; an explicit value that would diverge
// from that derivation is refused with invalid_input.
//
// # Scope and concurrency
//
// The Kernel stays scope-free: Engine reads and scans through store.Controller,
// and Engine.Commit lets the controller derive the owning scope from the
// execution row inside the transaction, so a unit naming a row outside that
// scope is refused. Capability ports stay scoped. Every write is one unit with
// expected versions; a receipt replay applies nothing and returns the stored
// result; request first settlement carries a transaction-clock deadline guard on
// the request deadline so a late answer cannot settle.
//
// # Current-authority guards
//
// Every current-authority read a unit carries becomes an in-transaction guarded
// lock; a check with no store mapping is invalid_input, never a silent skip.
// Membership is confirmed against the UNION of the caller's tenant-wide and
// exact-project rows: a human read re-evaluates the effective roles (including
// the required human role) and refuses with forbidden when neither row grants
// it, while pinning BOTH observed rows — a present row by memberships.id
// identity AND version, an absent row by a unique-key absence guard. A change
// to either row before the commit, including the creation of a row that was
// absent when confirmed, is a version_conflict. A project row can only add
// roles and never shadows a tenant-wide grant, and a tenant-scoped read ignores
// the project row. The conditional push-admin check keeps its single Admin
// read. principal_enabled locks the principal's committed row by kind: a
// human's membership row, a service's enabled state and version (TenantWide
// when the row is tenant-wide), or an execution's running state and version.
// service_enabled locks the exact service row's enabled state, version and
// TenantWide predicate, or, with no target, the acting service; a human or
// execution actor has no service-enablement fact and adds no lock.
// execution_running locks the execution's running state and version,
// action_deadline adds the transaction-clock deadline guard on that action, the
// versioned checks pin their declared version column, and resource ownership
// locks the resource row's immutable scope.
package executionstore
