# S04 — Persistence, atomicity, and idempotency

Source: [core model](../../KODAMA.md#core-model), [tasks](../../KODAMA.md#tasks-and-role-inbox), [events](../../KODAMA.md#events-and-operations), [operations](../../KODAMA.md#public-operations). Owners: P07, P12, P13 and consuming capabilities. AC: 3, 4, 7, 12, 17, 18.

## 1. Relational model

Use ordinary SQL and explicit transactions. The following are record families, not a target table count; C02 freezes migrations, foreign keys, indexes, and transaction ports.

| Family | Required facts |
|---|---|
| Tenants/projects/memberships | Relational ownership, current human roles, versions; tenant encrypted derivation key |
| Services | Tenant/project scope, enabled state, roles excluding Admin, nullable current nonce/expiry |
| Resource bindings | Stable ID, scope, enforcing service, trusted origins, allowed operations/events, version, metadata |
| Pipelines | Stable ID, project-unique name, current exact YAML, version |
| Executions | Scope, optional task/pipeline links, initiating principal, exact YAML/hash/snapshot, state/version, nonce/expiry, workspace/effect references |
| Actions | Execution+ordered index+slug+kind, state/version, timeout/deadline, immutable request subject/assignment, outputs/staging, decision metadata |
| Tasks/messages | Content/metadata version, lifecycle version, selected pipeline/inputs/initiator, current execution, outstanding question, message author/time/refs |
| Events/audit | Stable IDs, ownership, subject/version, source/causation, committed publication intent |
| Receipts | Scoped operation/key, content fingerprint, selected snapshot where applicable, durable result |
| Verification | Calling service, actor identity, execution/action, binding/resource/operation/subject, requested approval and result |
| Secrets | Exact fields in KODAMA.md; current encrypted value and scope-unique name |
| Resources/cleanup | Stable identities, durable intents, observation evidence, pending reason/attempt bookkeeping |

Requests stay action rows. Artifacts stay output values. Queued executions are dispatch records; no separate start-intent lifecycle. Git journals, access rules, agent transcript bytes, and artifact bytes are owned outside the factory database.

Ownership and queryable states are relational. JSON may store snapshots, output objects, metadata, and bounded envelopes. Composite foreign keys/uniqueness enforce tenant/project ownership so an ID from another scope cannot be attached accidentally.

## 2. Transaction boundaries

| Commit unit | Atomic contents and checks |
|---|---|
| Admission/start | Current authority/version, exact snapshot, queued execution and actions, task update, receipt, audit/events |
| Kernel transition | Expected versions, state/output promotion or discard, effect intent, token revocation where required, audit/events |
| Request activation | Stop evidence, resolved subject/assignment/deadline, running state, token clearing, `action.requested` |
| Request resolve | Current assignment authority, live state/version/deadline, response validation, decision/actor/time, settlement/outputs, receipt/events |
| Task ask | Task version/open state, question/assignee, waiting state, active execution cancellation, token clearing, receipt/events |
| Task reply | Question/task version, assigned responder, current initiator start authority, answer/open, one queued execution/new snapshot, receipt/events |
| Task cancel | Task lifecycle/version, execution cancellation and token clearing, receipt/events |
| Token issue/rotate/revoke | Eligible principal state, nonce/expiry replacement/clearing, audit/events; delivery follows commit |
| Binding/config mutation | Admin/current authority, expected version, metadata change, audit/event |
| Verified report | Current service/binding, verification linkage, source-ID/content dedupe, causation and event |
| Push selection/result | Unique pipeline/event receipt, first definition/service/input selection, terminal result or recoverable transient state |

External registry/KMS/runtime/NATS/Git calls cannot be held inside these transactions. Internal transition callbacks may use only already-available data and transaction-local reads. Authorization-sensitive records/versions must be checked at the commit boundary; a previously calculated allow is not durable permission.

C02 defines a consistent lock order for affected task/execution/action/authority/receipt records and tests deadlock/conflict behavior. Kernel settlement and Shell operations share the same action serialization; separate locks in independent repositories are insufficient.

## 3. Optimistic versions and retries

State transitions compare expected versions. Task content has its own optimistic version; content edits cannot implicitly advance lifecycle, replace execution, or answer a question. A stale state command returns a stable conflict result without partial changes.

Receipt identity is `(tenant, project, principal, operation, caller key)` with explicit handling of tenant-scoped operations. Equal normalized request content returns the original result; changed content returns an idempotency conflict. Normalization is for operation content identity, never the exact-YAML execution hash. C01/C03 freeze treatment of JSON ordering, omitted/default values, and binary content.

Authenticate and recheck applicable current authority before exposing a receipt result. This does not repeat the effect. Authorization and verification themselves are never satisfied from cached receipts. Request-specific duplicate handling may return a previously committed answer to an authorized retry despite now-terminal state; it cannot accept a replacement or authorize another effect.

Receipts survive until project deletion; tenant-level operations need a documented equivalent lifecycle. A crash after commit/before response must converge on the same result, including execution ID. No partial answer without admission and no admitted execution without the corresponding task update/receipt.

## 4. PostgreSQL isolation

- Runtime uses a non-owner role without RLS bypass. Force RLS on scoped tables.
- Scope is established transaction-locally from verified caller context and reset by transaction completion; pool reuse cannot leak prior scope.
- Queries are parameterized. Project authority is checked in addition to tenant RLS.
- Constraints defend cross-tenant/project references, unique action order/names, valid state shape, and nonce/expiry pair consistency.
- Migrations are reviewed sequential changes with one P13 owner; never rewrite an applied migration.
- Outbox/event writes share the business transaction. Broker publication is a separate retrying reader.

The memory adapter implements one shared atomic unit with version/conflict/rollback semantics, not unrelated per-capability maps with independent locks. It is a development adapter and conformance fixture, not evidence of PostgreSQL RLS or isolation behavior.

## 5. Verification split

Before G2: SQL/query/argument/transaction ordering tests through a narrow database seam; rollback and conflict injection; migration/static constraint inspection; memory conformance; adversarial cross-scope command cases. SQL mocks must assert required behavior, not every incidental query spelling.

After G2: execute all migrations on real PostgreSQL; use the actual runtime role; test RLS, pooled-connection scope, constraints, commit rollback, concurrent settle/output/reply/cancel races, crash-after-commit receipts, and event atomicity. Mock success is never reported as database proof.
