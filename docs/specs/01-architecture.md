# S01 — Architecture and contracts

Source: [architecture](../../KODAMA.md#1-product-and-architecture), [implementation discipline](../../KODAMA.md#implementation-discipline).

## 1. Ownership

Kernel executes admitted ordered work. Shell coordinates, persists, and authorizes. Userland performs work and owns external resources. One Go module contains these three layers; packaging is not an additional runtime layer.

Only Shell adapters access the factory database. Userland uses `api`/`client` contracts even when built into the same executable. Kernel has no knowledge of tasks, roles, Git, HTTP, SQL, NATS, identity, or agents. Task snapshot bytes may reach preparation as opaque data; Kernel never interprets their fields.

## 2. Functional package catalog

Paths below are exact ownership targets. A listed directory may contain a few cohesive subpackages under the same owner. Contract declaration files are owned by F02/C01–C03 and excluded from builder writes.

| Owner | Paths | Responsibility |
|---|---|---|
| F01/F02 | `internal/id`, `internal/fault`, Kernel `types.go`/`ports.go` files | Minimal vocabulary, typed failures, contracts |
| K01 | `internal/kernel/definition` | Typed definition validation and direct references |
| K02 | `internal/kernel/flow` | Pure state transitions for both action kinds |
| K03 | `internal/kernel/script` | Fail-fast command scripts (output staging/settlement lives in `flow` and `engine`, with value bounds on `kernel.Outputs`) |
| K04 | `internal/kernel/testkit` | Deterministic state/runtime/clock fakes and fault schedules |
| K05 | `internal/kernel/engine` | Reconcile durable intents, observations, deadlines, cleanup |
| P01 | `internal/shell/tenancy`, `internal/shell/resource` | Tenants/projects/memberships and versioned resource bindings |
| P02 | `internal/shell/identity`, `internal/shell/token`, `internal/shell/service` | Principal verification, credential lifecycle, service management |
| P03 | `internal/shell/authorize`, `internal/shell/verify` | Shared operation grants and service-plus-actor facts |
| P04 | `internal/shell/pipeline` | YAML authoring, admission and image pinning orchestration |
| P05 | `internal/shell/execution`, `internal/shell/request` | Execution API and authorized request settlement |
| P06 | `internal/shell/task`, `internal/shell/inbox` | Conversations, atomic handoffs, derived progress |
| P07 | `internal/shell/event`, `internal/shell/receipt` | Durable publication intent, verified ingress, deduplication |
| P08 | `internal/shell/trigger` | Event-table-driven exact push starts |
| P09 | `internal/shell/secret`, `internal/shell/workspace`, `internal/shell/log` | Secret custody, activation data, retained/live log boundaries |
| P10 | `internal/shell/gateway` | Optional model gateway |
| P11 | `internal/shell/mcpproxy` | Optional external MCP proxy |
| P12 | `internal/shell/store/memory` | Development transaction implementation |
| P13 | `internal/shell/store/postgres` | Plain SQL, migrations, RLS, atomic commit units |
| P14 | `internal/shell/runtime/process` | Explicit host-process development adapter |
| P15 | `internal/shell/runtime/podman` | Test-environment container adapter |
| P16 | `internal/shell/runtime/kube` | Production Jobs, volumes, stop evidence |
| P17 | `internal/shell/integration/keys` | Ephemeral/keyfile/AWS/GCP/Scaleway key adapters |
| P18 | `internal/shell/integration/nats` | Scoped events/logs and live credential invalidation |
| P19 | `internal/shell/integration/oidc`, `internal/shell/integration/platform` | OIDC access-token checks and hosted authority |
| P20 | `internal/shell/integration/registry` | Immutable image resolution |
| P21 | `internal/shell/transport/ops`, `internal/shell/transport/httpapi`, `internal/shell/transport/mcp` | Single router and two thin public edges |
| P22 | `client`, `internal/userland/cli` | REST client, `kodactl`, login/credential helper |
| P23 | `internal/userland/git` | Independent Git service, rules, journal, retention |
| P24 | `internal/userland/session`, `internal/userland/skill` | Durable agent context; Markdown skill declarations and loader (`skill` is frozen by C-PUBLIC and no production package imports it yet) |
| P25 | `internal/userland/agent` | Native agent runtime using public contracts |
| P26 | `internal/userland/services` | Email/webhook/timer/responders and merge serialization |
| P27 | `examples` | Pipelines, images, context and delivery recipes |
| P28 | `internal/installation`, `deploy` | Config/preflight, Helm, backup/restore entrypoints |
| P29 | `internal/shell/store/units/execution` | Execution, request, and Kernel `engine.State` store adapters over neutral `store` seams |
| P30 | `internal/shell/store/units/work` | Task, inbox, pipeline, and trigger store adapters over neutral `store` seams |
| P31 | `internal/shell/store/units/identity` | Tenancy, resource-binding, identity, authority, credential, and service store adapters |
| P32 | `internal/shell/store/units/platform` | Event outbox/ingress/read, verification, secret-lookup, gateway, and MCP store adapters |
| P33 | `internal/shell/store/units/readers` | Store-backed readers for task execution views, log ownership, event cursors/validity, token receipts, and verification |

`api` contains public DTOs/schemas only and is frozen by C03. `internal/installation` is configuration/tooling, not a fourth product layer or alternate composition root. `internal/shell/app`, Userland app wiring, `cmd/kodama`, `cmd/kodactl`, and `test/{component,integration,e2e,qualification}` are reserved for integration tasks after G2.

## 3. Import rules

| Importer | Allowed | Forbidden |
|---|---|---|
| `api`, `id`, `fault` | Standard library | Product implementations, transport clients |
| Kernel | Standard library, `id`, `fault`, acyclic Kernel dependencies | Shell/Userland and infrastructure clients |
| Shell capability | Kernel types/logic, neutral types, consumer-owned contracts | Concrete stores/runtimes/transports, Userland |
| Shell adapter | Consumed contracts, approved technology dependency | Another capability's implementation to bypass its port |
| Shell transport | Public schema, verified-principal boundary, router contract | SQL, duplicate authorization/lifecycle rules |
| Userland/client | Public schema/client, neutral values, own adapters | Kernel/Shell internals, factory DB/key access |
| Composition | Completed packages needed for its process | Shared mutable global registries, `init` side effects |

Consumer-owned interfaces live with the consumer; no universal `Repository`, service locator, generic event engine, or global `contracts` god-package. A provider can implement several small interfaces without depending on a consumer implementation. Shared declarations must form an acyclic graph.

## 4. Port contract shape

Every effectful port specifies input identity/version, success evidence, ambiguous failure, idempotency, cancellation, and ownership of retries. `context.Context` is first on blocking calls. Return a structured observation, not a boolean that collapses uncertainty.

Kernel state ports must support load, compare-and-commit transitions/intents, and scanning due/unfinished work. The commit contains expected execution/action versions, output changes, cancellation/deadline effects, and credential-invalidation intent. Shell applies its audit/events/token changes atomically without injecting identity policy into Kernel.

Shell transaction ports express **complete commit units**, such as `AskTask`, `ReplyAndAdmit`, or `ResolveRequest`; not independently committing `SaveTask` and `CancelExecution` calls. Capability transitions run with authoritative locked/versioned state and an explicit current time. The adapter is persistence, not another state machine.

External calls occur outside database transactions. Read/resolve external input first, then validate relevant versions and authority when committing. Persist external-effect intent before starting it; retry through observation, not guessed success.

## 5. Errors and determinism

Required error classes: invalid input, unauthenticated, forbidden, not found, version conflict, changed idempotency content, expired/deadline, unsupported capability, dependency unavailable, uncertain external effect, and internal failure. Stable public codes map consistently across REST/MCP; private causes never expose tokens or secrets.

Inject clock, ID generation, randomness, external clients, and command execution at real boundaries. Production randomness is cryptographic. Runtime errors distinguish a proven pre-effect refusal from a lost post-effect response. Business terminal state and pending cleanup are separate fields.

## 6. Unit-stage boundary

Functional packages are implemented against frozen interfaces and deterministic fakes. Calling a pure Kernel helper or using public value types is allowed. Wiring real Shell capabilities together, launching services/processes, and testing infrastructure semantics wait for G2. Unit tests exercise one capability and its faked collaborators, including full transactional command payloads and rollback behavior of the in-memory adapter.

Every package documents its owned invariant, its ports, and the real proof still required. `unit-complete` never means PostgreSQL atomicity, container isolation, or end-to-end authority has been proven.
