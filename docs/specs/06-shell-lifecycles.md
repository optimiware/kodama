# S06 — Shell executions, requests, tasks, and configuration

Source: [Shell capabilities](../../KODAMA.md#3-shell-capabilities). Owners: P01, P04–P06. AC: 2, 4, 16–18.

## 1. Execution operations

Start accepts an authorized project pipeline or eligible task-free inline YAML and uses immutable admission. The queued execution is the durable dispatch record. Cancellation atomically stops future work and clears authority, then Kernel reconciliation handles stopping/cleanup. Inspection exposes state, actions, recorded outputs/decisions, deadlines, and pending resource/cleanup reasons.

Container output submission targets exact execution/action and expected eligible state. It validates the current execution credential, action kind, declared schema, staging identity, and serialized settlement rules in S02. Custom events can never settle an action.

Shell passes Kernel transitions through transaction ports that couple state to credential clearing, audit, and events. It must not implement another execution state machine or advance on broker delivery.

## 2. Request resolution

An activated request is a running action with fixed subject, assignment and deadline, no workload, and no execution credential. It leaves the execution running and any task open. The inbox is a query over this state, not an independently mutable request entity.

Resolution contains exact execution/action, expected action version, `decision: pass|fail`, declared response object, and idempotency identity. In one transaction:

1. Validate the current authenticated respondent and owning scope.
2. Validate running execution/action, kind, version, and authoritative time before deadline.
3. For `role/<role>`, require a human currently holding that role. For `service/<id>`, require that exact enabled service.
4. Validate response schema and immutable retry identity.
5. Apply Kernel settlement; record decision, actor/time, output data, receipt, audit/events.

Pass advances the same execution/workspace after the stop barrier. Fail keeps the rejected answer inspectable and fails execution; timeout/cancel record no answer. Cancel an active request by cancelling its execution. Duplicate identical authorized retry returns the recorded result; replacements fail.

## 3. Task model and transitions

Task content includes title, Markdown description/messages, bounded metadata, selected pipeline/inputs, initiating principal, execution history, current execution, and outstanding question/assignee. Metadata is opaque Userland state; the Shell versions/snapshots/publishes it without interpreting labels or workflow stages.

| Operation | Preconditions | Atomic result |
|---|---|---|
| Create | Current create authority | Open task, no execution |
| Edit/comment | Content version and authority | Content/message update only |
| Select pipeline | Explicit versioned operation and authority | Selection/inputs plus start-authority principal recorded |
| Start/create-and-start | Open task; current authority; an operator-only start also requires a failed current execution | Queued execution snapshot, current pointer/history, receipt/events |
| Ask | Open task; authorized human or its current execution; expected task version | Question/assignment, waiting state, cancel nonterminal execution (reason `requested`, as an explicit cancel), revoke credential |
| Reply | Matching outstanding question/version and assigned respondent; initiator still authorized to start | Answer, reopen, one fresh queued execution with updated conversation snapshot |
| Cancel | Versioned current authority | Cancelled task and nonterminal execution, credential revocation |
| Current execution succeeds | Task still open and current pointer matches | Done |
| Current execution fails | Task open | Remains open for operator follow-up |

States are `open`, `waiting`, `done`, `cancelled`. A waiting task cannot use ordinary start. Stale replies cannot reopen cancelled work. Edits/comments cannot answer, start, replace execution, change pipeline selection implicitly, or alter decisions.

Besides the caller-supplied task version, ask and cancel are bound to the current execution's version, and a reply to the version of the execution it replaces: a concurrent execution commit refuses them with `version_conflict`, nothing is committed, and the caller re-reads the task and retries with the same idempotency key.

A replacement execution may be admitted and queued while the predecessor is still nonterminal; it becomes the current execution and dispatches only after **all prior workloads are confirmed stopped**. Reply/stop confirmation may arrive in either order. Use durable predecessor/resource evidence; do not rely on the last NATS message or a lease timeout.

## 4. Handoff semantics

Ask cancels the current execution and discards any staged outputs. It creates a conversation message, not a request action and not approval evidence. Before asking, the workload saves context in a durable external store and references it in the question. Disposable paths and process-local IDs cannot be restoration sources.

Ask refuses a `done` or `cancelled` task with `version_conflict` before it reads the current execution or reaches the commit. A `waiting` task is not refused up front, because it may be a retry of an ask whose response was lost: the commit resolves the receipt first, so an equal idempotency key replays the stored result, and any other ask fails the commit's `open`-state guard with `version_conflict`.

Reply checks current assignment and the initiating principal's current start authority; response authority grants no execution permissions or pipeline choice. Failure to authorize/admit rolls back the reply and state change. One receipt returns the same new execution ID on duplicate retry.

The new execution starts at the first action with new workspace/credential/deadlines and its own decisions. `/kodama/task.json` contains the updated conversation at admission: a JSON array of messages, each with author, time, body, and optional references; task metadata is recorded in the execution snapshot, not in this file. The Userland restoring action loads referenced context; a missing reference fails **that action**, rather than silently starting from incomplete context. Kernel does not understand agent checkpoints.

## 5. Progress and inbox

Task progress combines task state with a rebuildable execution/action view. It cannot maintain a divergent task `running`/`failed` progress field. Only current execution success while open marks done. Older events cannot overwrite waiting/cancelled state or newer work.

Inbox query applies current scope/roles and returns active human-assigned requests, waiting human-assigned tasks, and failed open tasks for authorized operators. Services see only explicitly service-assigned questions/requests. Stable pagination and authoritative state prevent stale events from stranding a request.

## 6. Configuration capabilities

Admin operations manage tenants/projects/memberships, service enablement/roles/credentials, and resource bindings. Binding mutation increments a version and commits audit/event with the metadata change. A binding contains scope, enforcing service, trusted endpoint origins, verifiable operations, reportable events, and bounded metadata. Registration does not provision resources or store service access rules/bytes.

Scope comes from ownership, not caller body assertions. Stored secret management reads expose metadata only. All configuration operations share the router, authorizer, versions, receipts where required, and event pattern.

## 7. Unit scenarios

- **S-REQUEST:** exact assignment, stale version, deadline equality, pass/fail/timeout/cancel races; rejected answers not downstream.
- **S-START:** create does not start; one current execution; atomic create-and-start; duplicate receipt returns one execution.
- **S-HANDOFF:** ask/cancel conflict, staged-output discard, reply rollback on admission/authority failure, reply/stop ordering.
- **S-PROGRESS:** older success ignored; current failure remains open; retry preserves history; waiting/cancelled never overwritten.
- **S-EDIT:** content-version conflict and separation from state/pipeline operations.
- **S-INBOX:** current role removal, explicit service assignment, operator failures, stable pagination.
- **S-CONFIG:** tenant/project mismatch, service Admin rejection, binding version/event, origin validation.

Use fake transaction/authority/admission ports and inspect complete commit units. Real atomicity and API/Kernel composition wait for G2.
