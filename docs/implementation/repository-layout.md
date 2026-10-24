# Repository layout and package ownership

**Layout and ownership direction:** one Go module contains vertical capability packages. Many foundation packages, runtime contracts, CLI contributions, and the chart already exist; some paths below are planned. A package or passing package test is not evidence that its routes, migrations, commands, or workers are wired into the executable. The task index records merged contributions; composition and milestone acceptance establish usable slices.

```text
cmd/kodama/                 # main delegates to internal/app
internal/
  app/                     # composition only; selects adapters from KODAMA_ENV; integration-task owner
  memdb/                # in-memory transaction kernel behind every memory adapter
  domain/                  # IDs, scope, error codes; no all-purpose service interfaces
  storage/                 # pgx, WithTenant, migration runner, base migrations
  httpapi/                 # HTTP/version/idempotency/cursor helpers
  events/                  # outbox, JetStream, inbox, retained observation sequence
  identity/                # OIDC, memberships, fixed role checks
  work/                    # work items, history, WIP
  secrets/                 # variables, encryption, typed secret resolution
  workspace/               # Git refs, source publication and reconciliation
  pipeline/                # immutable definitions and validation
  artifacts/               # S3 metadata/transfers
  entrypoint/              # in-Pod orchestration and runtime client
  runtimeauth/             # attempt token issuance/admission/revocation
  execution/               # run/step/attempt state, scheduler and the runtime port
  kube/                    # Kubernetes runtime: Job templates/create/observe/delete
  runtime/                 # Podman and test-only Process runtimes, bootstrap, runtimetest
  results/                 # result verification and finalization interface
  logs/                    # live proxy and retained logs
  observation/             # scoped SSE
  agents/                  # profile expansion
  gateway/                 # one model protocol and usage counters
  mcpproxy/                # tool mediation
  platformintegration/     # hosted core adapters implementing consumer-owned ports; T-061
  delivery/                # environment/deployment records, configuration and outcomes
  deliverycleanup/         # deployment expiry policy → ordinary cleanup-run admission
  integrations/            # native push hooks, later provider adapters, ChatOps consumers
  metrics/                 # SQL summaries
  qa/                      # QA profile and evidence-to-defect mapping
  cli/                     # command modules; no domain logic
api/                       # module-owned OpenAPI fragments
contracts/runtime/         # shared runtime JSON schemas/fixtures
deploy/helm/kodama/         # chart
dev/                       # Compose dependencies, fixtures, probes, locks
scripts/dev/               # Compose services and kind harness, stack chosen by DEV_STACK (Python, stdlib only)
test/e2e/                  # milestone-specific suites
templates/                 # application blueprint templates
```

## Parallel boundaries

Capability packages own their DDD entities, application use cases, ports/adapters, SQL queries/migrations, HTTP fragment, tests, and exported constructor/handler. Prefer small consumer-owned Go interfaces for persistence, external effects, and cross-package operations as described in [software design](../specs/software-design.md). `internal/domain` contains only shared value types; it is not a central registry of entities or interfaces. Package boundaries do not require additional Go modules, services, or mechanically repeated layers.

Keep these boundaries within each module's owned path. For example, `internal/work/domain/` can own work-item entities, the `internal/work` package can own use cases and ports, and `internal/work/postgres/` and `internal/work/http/` can own adapters. Use subpackages where needed to enforce inward imports; do not pre-create identical layers for every module. Adapter representations map to domain values, and concrete adapters are injected by `internal/app`.

`internal/app` composes concrete dependencies, registers routes, orders migrations, and starts/stops workers explicitly. Only the designated assembly/integration task edits it after scaffolding. Tasks add a capability-local constructor/route/migration/command contribution and document it in their completion report. Central dependency files (`go.mod`, `go.sum`) belong to the bootstrap/integration owner, subject to the harness's narrow dependency allowlist. T-070 → T-071 → T-027 → T-072 → T-039 → T-075 → T-046 serializes the central files; incremental assembly is executable evidence before final milestone joins.

Module migrations use globally unique IDs prefixed by task ID and sequence, for example `T011_001_work.sql`. The migration manifest orders dependencies explicitly; lexical task ID alone does not establish order. Each module exports its embedded migrations. Base identity/tenant migrations precede all tenant-owned foreign keys. Cross-module schema changes are owned by the referencing module and declared in the task DAG.

## Shared contract checkpoint

T-003 freezes runtime context/result/source-proposal/artifact envelopes and primitive IDs/errors. T-004 freezes HTTP conventions. T-020 freezes run-service mutation semantics before the executor/result/gate tasks start. These checkpoints are sufficient for fakes; an entire domain-interface framework is unnecessary.

## Package seam contracts

This is an owner/checkpoint map, not a second interface registry. Existing signatures and fixtures remain authoritative in the owning package. Future contributions supply exact constructors and fixtures at their checkpoint; a missing adapter is a prerequisite, not permission to insert an allow-all fake into production.

| Seam / authority owner | Transaction ownership | Failure and replay | Wiring checkpoint | Fixture / evidence owner |
| --- | --- | --- | --- | --- |
| `identity` → capability-owned authorization ports; hosted core remains Platform-owned | Caller-bound current authorization inside tenant mutation; RLS under non-owner role | Denial/outage stays closed; revocation serialized, never cached into a permit | T-070 standalone; T-061/T-056 hosted | Identity pooled-context/revocation tests; PI fixtures after O-18 |
| `execution` admission → `work`, `pipeline`, `secrets`, `workspace` | Execution PostgreSQL `Peers`/`Env` join admission lock, pins, reservations, history/audit/outbox; Git retention outside SQL | Revision/tip conflict rolls back; retained-ref intent is redriven with same run | T-062 → T-070 | Existing execution PostgreSQL tests + T-062 joined rollback/RLS checks |
| `execution.Runtime` → `kube`, `runtime/podman`, `runtime/process`; `runtimeauth` attempt admission | Committed dispatch intent first; runtime grant admission locks execution before grant; runtime calls outside SQL; removal bookkept after settlement | Same attempt/workload identity on redrive; instance bound at dispatch; collision and another backend denied; lost workload fails; unknown effect needs evidence | T-086 → T-071 | `runtimetest.Contract` on Process and real Podman; T-086 entrypoint roundtrip; T-021 real kind races; T-019 grant fixtures; T-071 restart/cancel |
| `results` → execution finalizer, artifact evidence, workspace publisher, runtime authority | Guarded `results/postgres` stores join session check to owning mutation; artifact protection precedes commit; Git/S3 outside SQL | Receipt-first same hash returns original acknowledgment; conflict differs; Git-success/SQL-failure reconciles, never assumes success | T-062 → T-071 | T-022 (merged at `deff2e1`) preserves result ordering; T-062 verifies selected decorators and cancellation/grant rollback |
| Gate subject/decision (`gates`) → execution progression | Gate owner decides once with audit/outbox; consumption rechecks current subject and authority in its protected transition | Stale evidence, revoked reviewer or changed config blocks progression; no Job/WIP reservation for gate | T-034 → T-084 unit policy → T-072 wiring → T-073/T-038 checkpoint | T-034 fixtures; T-084 deterministic tests; T-073 actual rollback/restart/revocation |
| `logs` → attempt authority, Kubernetes reader, artifacts; entrypoint upload | Scoped read; artifact owner commits retained metadata; no SQL lock across live stream/upload | Bound tail; duplicate upload/link safe; partial/missing-node logs explicitly incomplete | T-023 + T-063 → T-064 → T-071 | Log service/entrypoint fixtures, then T-064 real access/fallback checks |
| Profiles (`agents`) → execution resolver; OpenCode wrapper → gateway/MCP | Immutable profile/context pins; gateway admission owns atomic request cap; MCP owns caller/session scope | Protocol mismatch blocks image qualification; partial/unknown tool effects are not automatically replayed | T-074 protocol checkpoint → T-031; T-072 activation | T-030 expansion fixtures, T-032/T-033 real protocol scopes, T-031 real image smoke |
| Deployment records (`delivery`) → runtime provenance; scripts → Kubernetes | Delivery implements execution's immutable snapshot resolver; execution pins identity/credentials before dispatch; pre-apply/action row precedes network calls | Absent/foreign/mismatched pin rejected; response loss uses recorded action/annotations; mutable settings cannot replace admitted configuration | T-076/T-035/T-062 → T-077/T-066 unit binding → T-072 wiring → T-065/T-067/T-038 checkpoint | Unit binding fixtures; actual provider/cleanup/SQL and apply/reconcile at T-067 |
| `deliverycleanup` → execution cleanup admission | Deployment-bound cleanup authorization; execution owns run transaction, no work-item WIP | Stable deployment/action binding, expiry and deletion reconcile independently of work completion | T-066 → T-067 → T-072 | T-066 real scoped persistence/admission checks; T-067 real cleanup |
| `qa` → work defects, artifacts, deployment evidence | Work owner commits deduped defect/history/audit/outbox; evidence remains deployment/config-bound | Retry preserves defect identity; foreign/stale evidence rejected; QA cannot approve | T-040 → T-075/T-046 unit wiring → T-044 local checkpoint; T-069 browser scope deferred | Deterministic mapping; actual local joins at T-044; later browser/profile evidence at T-069 |
| Memory adapters → `memdb`; PostgreSQL twins | One `memdb.Update` per use-case call; peers join the caller's `Tx`; PostgreSQL keeps `WithTenant` | Update is all-or-nothing; a `Tx` reads one tenant; a memory adapter never certifies SQL, RLS or lock order | T-087 → T-088/T-080/T-081 → T-071 | `<module>test.Contract` on memory (unit) and PostgreSQL (`test` environment) |
| CLI/HTTP/event contributions → owning use cases | No client-side business transaction; inbox + local effect atomic where required | Public idempotency/version rules survive retries; external notifications use durable delivery records | T-070/T-071/T-072/T-075, then T-027/T-039/T-046 | Package command/handler fixtures plus assembled public-client and event checks |

### Transaction composition checkpoint

T-062 inventories actual constructor/transaction bindings and makes bounded execution/results adapter corrections. Preserve quota/work → run/attempt → authority/grant/secret → workspace → event lock order. Gate-specific locking is verified by T-073. A peer callback joins the caller's transaction; another `WithTenant` is not equivalent. Preserve request-session guards and T-022's terminal receipt exception. Unit tasks prove policy with controlled ports; T-026/T-073/T-067/T-038 prove actual rollback, revocation, settlement and replay before milestone acceptance. T-072 may merge unit-tested wiring first; that is not SQL qualification. Wider interface changes require a bounded owner task, not a generic transaction framework.

Tests exercise observable module behavior. In-memory adapters over `memdb` support fast, independent development and run the same `Contract` suite as their PostgreSQL twin ([environments](environments.md)); integration acceptance uses PostgreSQL, JetStream, Git, S3, Podman and, where it is the subject, Kubernetes.

Hosted identity/credential/tool adapters preserve [core ownership](../specs/platform-integration.md). Consumers own the narrow ports; `internal/platformintegration` holds the qualified outbound adapters, not a second core domain model. Dependency direction, platform revision, and any intentional deviation are part of the [design conformance](design-conformance.md) handoff.

Under O-15, optional UI implementation lives in a separately owned Platform task, not a Kodama `web/` tree. O-17's chassis kit in `templates/chassis-guidance/` is Markdown-only guidance intended for tenant/project Git repositories; executable application blueprints remain separate. Public module/import migration to `github.com/optimiware/kodama` needs an explicitly scoped coordinator change across existing modules before publication, rather than implicit permission for a module worker to rename unrelated files.

See [task ownership rules](../tasks/README.md#ownership-and-integration) for worktree and merge requirements.
