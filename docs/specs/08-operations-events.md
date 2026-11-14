# S08 — Operations, public edges, events, and push starts

Source: [events](../../KODAMA.md#events-and-operations), [push](../../KODAMA.md#pipeline-push-triggers-and-userland-services), [public operations](../../KODAMA.md#public-operations). Owners: P07, P08, P18, P21, P22. AC: 3, 14, 15, 19.

## 1. One operation router

Every application operation has one typed schema, authorization rule, shared handler binding, result schema, and error mapping. REST and MCP JSON-RPC decode into the same operation invocation. `kodactl` uses REST through `client`. NATS never submits operations.

C01/C03 freeze the complete 58-operation inventory and 1:1 REST/MCP parity before builders start. Required families:

- Tenant/project/membership, authenticated `principal.whoami` discovery, project listing, and resource binding administration/listing.
- Pipeline apply/read/list/update/delete and immutable execution start.
- Secret metadata read/list, write and delete; service account create/list/update/enable/disable and credential lifecycle.
- Task create/read/list/edit/comment/select/start/create-and-start/ask/reply/cancel/history/inbox.
- Execution inspect/list/cancel, action outputs and request resolution, retained logs.
- Actor/approval verification, custom event publication, verified resource reporting, authorized event reads/subscription setup.
- Optional configured model/tool surfaces with the same authorizer.

Product-fixed endpoints:

| Method/path | Meaning |
|---|---|
| `PUT /api/v1/executions/{execution}/actions/{action}/outputs` | Stage container outputs |
| `POST /api/v1/executions/{execution}/actions/{action}/resolve` | Settle assigned request |
| `POST /api/v1/tasks/{task}/ask` | Atomic conversation handoff |
| `POST /api/v1/tasks/{task}/reply` | Reply and queue fresh execution |
| `POST /api/v1/verify` | Service-plus-actor facts |
| `GET /api/v1/executions/{execution}` | Inspect recorded execution/evidence |

Other routes are implementation decisions frozen in C03. Expected versions and idempotency keys have one consistent transport representation; REST `GET` routes accept an empty request body as `{}` while non-`GET` methods continue to require an explicit JSON object body. Unknown fields/operations and oversized input are refused. Authenticate before resource lookup exposes scoped information; map error codes consistently. Credential issuance returns secrets only in its authorized response, never general reads.

The MCP Streamable HTTP edge (`/mcp`) negotiates MCP protocol versions (`2025-11-25`, `2025-06-18`, `2025-03-26`, `2024-11-05`, with `2025-06-18` fallback for other `202x-xx-xx` revisions), accepts `ping` (`{}`) and JSON-RPC notifications (`notifications/initialized`, `notifications/cancelled` -> `202 Accepted` with empty body), returns `405 Method Not Allowed` (`Allow: POST`) on `GET`/`DELETE /mcp`, and responds `401 Unauthorized` with `WWW-Authenticate: Bearer` (plus RFC 9728 `resource_metadata` pointing to `/.well-known/oauth-protected-resource` when discovery is configured) on `unauthenticated` calls. `tools/list` advertises underscore-sanitized tool names (`^[A-Za-z0-9_-]{1,64}$`) with descriptions, `tools/call` accepts both underscore and canonical dotted operation names, and `EncodeMCPResult` emits standard MCP `CallToolResult` (`content` + `structuredContent`).

REST handler tests use in-process request/recorder objects; MCP tests use message fixtures and a fake dispatcher. Before G2 neither edge is connected to real capabilities. Schema parity tests enumerate the operation registry and ensure neither edge invents grants or hidden operations.

## 2. Event contract

A versioned envelope contains stable event ID, type, authenticated/verified source, occurrence timestamp, tenant/project scope, applicable subject identity/version, optional correlation/causation, and bounded data or authorized durable references. C03 freezes names, size limits, cursor behavior, and serialization fixtures.

State mutation, audit, and event publication intent commit together in PostgreSQL. The event table is the durable stream; publishing retries retain the same event IDs. Lifecycle events follow durable state, not speculative workload observations. Custom observations are authenticated and recorded before routing, and cannot impersonate lifecycle or decision facts.

NATS subject scope/source derives from identity. Separate Shell-owned facts and Userland observations. JetStream supplies durable at-least-once event delivery/replay; Core NATS supplies live logs/progress. Consumers deduplicate by event ID and tolerate reordering. Browsers get read-only WebSocket subscriptions.

## 3. Verified resource reports

An enforcing service submits its stable source event ID, allowed event type, resource, effect subject, and verification ID. Validate:

1. Current reporting service and binding permit the resource/event/schema.
2. Verification belongs to that service and matches resource, operation, and reported effect fields.
3. Actor/execution causation comes from the verification record, not the report body.
4. `(service, source-event-ID)` is new or identical content; changed replay is refused.

Accepted effects can be reported after the actor token expires/is revoked. Reporting still authenticates the service and never grants new actor authority. Custom publication cannot create a trusted `git.ref.updated` report. Failed report retry repeats reporting, not the effect.

## 4. Push processor

The processor scans **accepted events in the Shell event table**, not NATS. It selects API-posted pipeline definitions with exact `on.push` ref matches and their bound repository/source. Ignore deletions and any execution-originated push, including delayed reports and cross-pipeline cases.

For each `(pipeline ID, event ID)`, one common receipt freezes event content identity, first selected YAML, automatic-start service, supplied inputs, and eventual result. Definition/binding edits cannot create a different replay selection or duplicate execution. Current authority is still rechecked before a new effect; stored selection never becomes a permission cache.

Use the shared start operation. Record permanent refusal and publish its event; retry transient dependencies without skipping unfinished work. Advance the cursor only after recorded results for relevant selections. A resumable internal receipt state may record selection while external image resolution is pending; it is not another execution/start-intent lifecycle. The queued execution remains the sole dispatch record.

## 5. Broker independence and credential enforcement

With NATS unavailable, execution observation/advancement, request settlement, deadlines, task reply/queue, push handling, and dispatch continue from persisted state. Publication backlog grows visibly and drains with original IDs when the broker returns. Broker notifications may accelerate work, never own it.

NATS authentication/authorization derives current scope/roles/credential state. Rotation, revoke, disablement, request entry, expiry and terminal state invalidate live connections/subscriptions. Define the connection revalidation/invalidation mechanism in C02 and test it with a fake broker before real verification. No command subject or actor-specified scope is permitted.

## 6. Unit scenarios

- **E-ROUTER:** REST/MCP operation and error parity; unknown/invalid input; single dispatcher call.
- **E-RECEIPT:** duplicate/changed keys, crash after durable result, current-auth refusal before retry exposure.
- **E-STREAM:** state/event atomic command, stable IDs over publication failures, bounded backlog/log handling.
- **E-INGRESS:** wrong service/binding/type/verification/subject; forged scope/source; late valid report; changed source-ID replay.
- **E-PUSH:** exact refs, deletion/execution exclusion, config edits, current service revoke, transient failure and cursor recovery.
- **E-BROKER:** fake outage cannot block domain calls; scope filters; live connection invalidation.

Actual broker permissions, JetStream replay, PostgreSQL polling, cross-edge receipts, and outage behavior are integration tasks after G2.
