# Development task graph

This backlog contains **100 tasks**, including merged history and explicitly deferred work. Keep one Go module and preserve working packages. Local M1–M3 acceptance is complete; T-105 adds secrets startup wiring. Production deployment/qualification, external adapters, chassis and additional wrappers retain their explicit scheduling requirements.

<!-- harness:summary:start -->

**Ready now:** [T-104](T-104-kubernetes-full-readiness.md).

**Done:** [T-000](T-000-kiss-baseline.md), [T-001](T-001-scaffold.md), [T-002](T-002-database.md), [T-003](T-003-shared-contracts.md), [T-004](T-004-http-foundation.md), [T-005](T-005-event-bus.md), [T-010](T-010-identity.md), [T-011](T-011-work-items.md), [T-012](T-012-secrets-variables.md), [T-013](T-013-git-workspaces.md), [T-014](T-014-pipeline-definitions.md), [T-015](T-015-artifacts.md), [T-016](T-016-step-entrypoint.md), [T-017](T-017-cli-foundation.md), [T-018](T-018-helm.md), [T-019](T-019-attempt-identity.md), [T-020](T-020-run-orchestration.md), [T-021](T-021-kubernetes-executor.md), [T-022](T-022-result-finalization.md), [T-023](T-023-logs.md), [T-024](T-024-event-streams.md), [T-025](T-025-core-cli.md), [T-026](T-026-m1-acceptance.md), [T-027](T-027-m1-integration.md), [T-028](T-028-production-kms.md), [T-030](T-030-agent-profiles.md), [T-031](T-031-opencode-harness.md), [T-032](T-032-ai-gateway.md), [T-033](T-033-mcp-proxy.md), [T-034](T-034-human-gates.md), [T-035](T-035-deployments.md), [T-036](T-036-git-webhooks.md), [T-037](T-037-delivery-cli.md), [T-038](T-038-m2-acceptance.md), [T-039](T-039-m2-integration.md), [T-040](T-040-exploratory-qa.md), [T-041](T-041-chatops.md), [T-042](T-042-flow-metrics.md), [T-043](T-043-go-blueprint.md), [T-044](T-044-m3-acceptance.md), [T-045](T-045-work-dependencies.md), [T-046](T-046-m3-integration.md), [T-062](T-062-execution-transaction-composition.md), [T-063](T-063-entrypoint-log-upload.md), [T-064](T-064-log-qualification.md), [T-065](T-065-deployment-tools.md), [T-066](T-066-deployment-cleanup.md), [T-067](T-067-deployment-qualification.md), [T-068](T-068-profile-gate-cli.md), [T-070](T-070-core-assembly.md), [T-071](T-071-execution-assembly.md), [T-072](T-072-agent-delivery-assembly.md), [T-073](T-073-gate-transaction-qualification.md), [T-074](T-074-wrapper-protocol-checkpoint.md), [T-075](T-075-business-loop-assembly.md), [T-076](T-076-environment-pinning.md), [T-077](T-077-environment-pinning-qualification.md), [T-078](T-078-entrypoint-log-mirror.md), [T-079](T-079-job-checkout-credential.md), [T-080](T-080-execution-unit-core.md), [T-081](T-081-result-log-unit-core.md), [T-082](T-082-entrypoint-unit-core.md), [T-083](T-083-agent-gateway-unit-core.md), [T-084](T-084-gate-tool-unit-core.md), [T-086](T-086-runtime-ports-adapters.md), [T-087](T-087-memory-kernel.md), [T-088](T-088-foundation-memory-adapters.md), [T-089](T-089-kubernetes-conformance.md), [T-091](T-091-test-stack.md), [T-092](T-092-cleanup-deployment-input.md), [T-093](T-093-delivery-effect-reconciler.md), [T-094](T-094-execution-claim-workspace-shape.md), [T-095](T-095-kube-runtime-contract-conformance.md), [T-096](T-096-runtime-action-ceiling.md), [T-097](T-097-delivery-production-joins.md), [T-098](T-098-podman-checkout.md), [T-099](T-099-entrypoint-artifact-naming.md), [T-100](T-100-git-trigger-production-adapters.md), [T-101](T-101-m2-production-assembly.md), [T-102](T-102-runtime-launch-evidence.md), [T-103](T-103-m1-acceptance-cleanup.md), [T-105](T-105-secrets-startup.md).

Generated from task frontmatter by `bun scripts/openforge/src/cli.ts index --write`; do not edit by hand.

<!-- harness:summary:end -->

## Delivery order

Work climbs three environments ([ADR 0009](../adr/0009-environments.md), [environments](../implementation/environments.md)): `development` (in memory, no containers), `test` (Podman services and runtime, no cluster) and `production` (Kubernetes). Each step is useful alone; only the sixth needs a cluster.

| Phase | Work | Environment | Merge evidence |
| --- | --- | --- | --- |
| M1 runtime seam | T-086 runtime port, Process/Podman runtimes and Kubernetes runtime reshape | `development`, `test` | Owned-package unit tests; real PostgreSQL/Podman contract and entrypoint roundtrip run explicitly |
| M1 memory kernel | T-087 `memdb` → T-088 foundation memory adapters and contracts | `development` | Owned-package unit tests; each `Contract` passes on the memory adapter |
| M1 Go core | T-080 execution, T-081 results/logs and T-082 entrypoint run in parallel, execution and results on memory stores | `development` | Owned-package Go unit tests, race detection and independent review |
| M1 test stack | T-091 cluster-free `services` stack and template-database clones | `test` | Python unit tests; explicit stack and clone checks |
| M1 wiring | T-071 (`KODAMA_ENV` guards) → T-027 (development composition smoke) | `development` | Scoped unit/httptest checks with injected adapters and no container |
| M1 checkpoint | T-026 | `test` | The `services` stack and the Podman runtime; actual pipeline, logs/artifacts, cancellation, restart and tenant denial; every `Contract` on PostgreSQL |
| M1 Kubernetes component conformance | T-102 → T-089 | `production` runtime (kind) | The `cluster` stack; capability-aware shared runtime contract, real Job lifecycle, restricted baseline and chart install/configuration refusal; O-24 excludes full server readiness |
| M1 defect cleanup | T-103 | `test` | Spool initialization, specific output-failure reasons and repaired real-service assembly checks; required before full Kubernetes acceptance |
| M2 Go core | T-036 native trigger, T-037 CLI, T-077 environment binding, T-083 agents/model and T-084 gates/tools run in parallel; T-066 follows T-077 | `development` | Deterministic Go tests with small ports, memory adapters and controlled time |
| M2 wiring | T-072 → T-039 | `development` | Scoped unit tests; scripted model/tool responses only in tests |
| M2 checkpoint | T-065 tools and T-073 SQL gate join in parallel; T-067 delivery join → T-038 | `test` (kind only as the delivery target) | Prepared Podman stack; real persistence/execution, deterministic agent/model responses |
| M3 business loop | T-075 → T-046 → T-044 | `development`, then `test` | Unit-tested wiring, then a bounded local integration checkpoint |
| Secrets startup wiring | T-028/T-044/T-103 → T-105 | Configuration and offline adapter tests | Selected client, bounded probe, service injection and cleanup; Helm projection |
| Full Kubernetes readiness | T-105/T-103 → T-104 | `production` (kind with supported KMS/TLS) | Explicitly scheduled; live KMS acceptance, ready chart and original assembled M1 roundtrips retained from T-089 |
| Staging | T-104 → T-090 trusted-engineer deployment to the stage cluster | `production` (stage cluster) | Explicitly scheduled; full-readiness prerequisite, its own handoff and smoke evidence |
| After M3, explicitly scheduled | T-047 Supabase, T-059 chassis, T-069 browser QA, T-085 live agent/wrapper, other M4 and production tasks | `production` | Their separately declared integration/release evidence |

Completed tasks remain historical records; package completion is not milestone acceptance. No real SQL/RLS/transaction/provider behavior is certified by a mock. It is deliberately qualified later, before the corresponding checkpoint is accepted. The [roadmap](../book/07-delivery-roadmap.md) describes the product scope.

## Task contract

Scheduled (`planned` or `blocked`) tasks declare `owns`, `check`, optional `resources`, and `acceptance`:

```yaml
check: make check-go PKGS=./internal/execution/...
resources: [go]
acceptance: [unit_dispatch_replay, unit_cancel_fencing]
```

- `make check-go PKGS=...`: runs scoped `gofmt`, `go vet -tags unit`, and `go test -json -race -count=1 -tags unit`, writing test outcomes to `$FORGE_RESULTS`.
- `make test-python`: runs the `scripts/dev` unittests through `scripts/dev/python_results.py`, writing outcomes to `$FORGE_RESULTS`. Acceptance names match a test method or a test class (a class passes only when all its tests pass), e.g. class `TestUnitStackSelection` satisfies `unit_stack_selection`.
- `make check-go-integration PKGS=...`: provisions `DEV_SCOPE` when it does not exist (stack from `DEV_STACK`, default `cluster`; `KODAMA_AUTO_PROVISION=0` disables), preflights it (exiting `75` if the environment is unavailable), loads `host-env.json`, and runs scoped integration tests.
- `plan: true`: opts into the read-only planner stage before implementation (off by default).
- `review: human`: parks the task for human review after checks pass.

Statuses: `planned` becomes ready when its dependencies are `done`; `blocked` needs its named decision; `deferred` needs explicit scheduling approval; `done` means reviewed, tested, and merged.

## Ownership and integration

1. One task branch/worktree under the primary checkout's ignored `.work/`; one writer per owned path. Follow [AGENTS.md](../../AGENTS.md). Preserve existing work and resumable sessions.
2. Read the assigned task and relevant contracts. Implement domain behavior through existing small, consumer-owned interfaces. Use doubles for actual external effects; avoid a universal repository, transaction abstraction or interface per type.
3. Edit only `owns`. Task metadata/index belongs to the coordinator unless explicitly owned. Shared composition/generated API/client files have one ordered owner: T-071 → T-027 → T-072 → T-039 → T-075 → T-046. Request a bounded prerequisite for an unowned interface change.
4. Run focused checks while editing. The coordinator independently qualifies the declared task scope and obtains review on the exact candidate. Go changes do not rerun OpenForge or development Python tests. Documentation/config checks run for relevant changes, not automatically for every Go edit.
5. Commit/checkpoint before rebasing at a clean stage boundary. An interrupted dirty implementer resumes its existing invocation first. Conflicts preserve the task for reconciliation; no automatic stash/reset/discard.
6. Publish only reviewed, qualified code. Rebase movement that changes relevant inputs renews affected evidence; unrelated main movement does not trigger another full suite. Keep pending branches/worktrees; clean only the merged task's resources.

Work can merge with unit evidence before integration is available. Executable constructors still choose real adapters and fail closed; test doubles are injected by tests, never installed as permissive production fallbacks. Milestones remain unaccepted until their actual checkpoint passes.

## Acceptance ownership transfers

| Source | Unit contribution | Required real acceptance owner |
| --- | --- | --- |
| T-071 | Execution wiring/lifecycle/guard selection | T-026: `execution_assembly_roundtrip`, `execution_assembly_cancel_restart`, `execution_assembly_result_guard` |
| T-036 | Authenticated trigger/dedup/exact-commit policy | T-038: `native_hook_roundtrip`, including real Git and durable admission |
| T-077 | Environment/cleanup binding | T-067: `environment_pinned_at_admission`, `environment_edit_preserves_pin`, `environment_pin_rollback`, `environment_pin_restart_scope` |
| T-066 | Cleanup policy/retry | T-067: real persistence, concurrent claims, rollback/restart and restricted deletion |
| T-026 | Restricted baseline and Kubernetes lifecycle (the M1 checkpoint now runs on Podman) | T-089: `kube_restricted_baseline`, `kube_component_roundtrip`, `kube_component_cancel_restart`, `kube_chart_fail_closed`; component evidence only under O-24 |
| T-089 (O-24 transfer) | Full installed-server readiness and assembled Kubernetes M1 acceptance | T-104: original `kube_m1_roundtrip`, `kube_cancel_restart`, `kube_chart_install`, including the same backend-parameterized `execution_assembly_*` tests; T-090 depends on completion |
| T-072 | Agent/gate/delivery wiring | T-038: `agent_delivery_assembly`, `gate_consumption_atomicity`, `delivery_assembly_replay` |
| T-038 original live agent | Local deterministic agentic checkpoint | T-085: real pinned wrapper/model/provider, rework, needs-input and budget smoke |
| T-075/T-046 | Business-loop wiring | T-044: real notifications/gates/replay; blueprint/browser qualification remains deferred, not waived |

Existing completed splits (T-062 transactions, T-063/T-064 logs, T-068 CLI, T-074 protocol, T-076 environment contract, T-078/T-079 entrypoint) are preserved.

## Periodic validation and failure handling

After five task merges, and at queue drain, validate captured `main` once. Initially this is core Go unit coverage (`GO_CORE_PACKAGES` / `make test-go-unit-json`), excluding image wrappers, templates and integration drivers. T-026 adds bounded M1 integration; T-038 adds M2. Reuse a prepared `batch` scope and isolate data. Run OpenForge/Python tooling suites only when their inputs changed. No 30-minute age trigger or second full suite at publication.

A code/compiler/assertion failure goes to a focused fixer. Missing environment or timeout pauses infrastructure with a receipt and actionable reason, without consuming code-repair budget. Cancellation preserves resumable work. A failed batch pauses publication, not implementation/review. See [OpenForge](../implementation/harness.md) and [testing](../implementation/development-and-testing.md).

Agent concurrency follows `--workers`; resource capacity separately bounds Go, service and kind jobs. Unit tasks use only Go capacity. Integration jobs declare their service/kind needs and run only after scope preflight. The queue HALT does not disable standalone environment maintenance.

## Dependency index

| Task | Dependencies | Status / decision |
| --- | --- | --- |
| [T-000 KISS documentation baseline](T-000-kiss-baseline.md) | — | done |
| [T-001 Scaffold and local dependencies](T-001-scaffold.md) | T-000 | done |
| [T-002 Database and tenant transactions](T-002-database.md) | T-003 | done |
| [T-003 Freeze runtime contract fixtures](T-003-shared-contracts.md) | T-001 | done |
| [T-004 HTTP conventions and concurrency](T-004-http-foundation.md) | T-002, T-003 | done |
| [T-005 Durable event bus](T-005-event-bus.md) | T-002, T-003 | done |
| [T-010 OIDC and five-role authorization](T-010-identity.md) | T-004, T-005 | done |
| [T-011 Work items, history, and WIP](T-011-work-items.md) | T-004, T-005 | done |
| [T-012 Variables and encrypted typed secrets](T-012-secrets-variables.md) | T-004, T-005 | done |
| [T-013 Git workspaces and controlled publication](T-013-git-workspaces.md) | T-004, T-005 | done |
| [T-014 Pipeline parser and immutable registration](T-014-pipeline-definitions.md) | T-004, T-005 | done |
| [T-015 Artifact storage and scoped transfer](T-015-artifacts.md) | T-004, T-005 | done |
| [T-016 In-Pod step entrypoint](T-016-step-entrypoint.md) | T-003 | done |
| [T-017 CLI transport, context, and PKCE](T-017-cli-foundation.md) | T-004 | done |
| [T-018 Helm installation](T-018-helm.md) | T-001 | done |
| [T-019 Attempt admission and runtime tokens](T-019-attempt-identity.md) | T-002, T-003, T-010 | done |
| [T-020 Durable run orchestration](T-020-run-orchestration.md) | T-005, T-011, T-012, T-013, T-014 | done |
| [T-021 Native Job executor and reconciliation](T-021-kubernetes-executor.md) | T-016, T-019, T-020 | done |
| [T-022 Runtime results and source finalization](T-022-result-finalization.md) | T-015, T-019, T-020 | done |
| [T-023 Scoped log service and retention contract](T-023-logs.md) | T-015, T-019, T-020 | done |
| [T-024 Authorized durable observation streams](T-024-event-streams.md) | T-004, T-005, T-010 | done |
| [T-025 Core user CLI](T-025-core-cli.md) | T-017, T-011, T-012, T-013, T-014, T-015, T-020, T-023 | done |
| [T-026 M1 integrated acceptance](T-026-m1-acceptance.md) | T-027, T-091, T-094 | done |
| [T-027 M1 composition and contract integration](T-027-m1-integration.md) | T-010, T-011, T-012, T-013, T-014, T-015, T-016, T-018, T-019, T-020, T-021, T-022, T-023, T-024, T-025, T-064, T-071, T-088 | done |
| [T-028 Production KMS adapters](T-028-production-kms.md) | T-012 | done |
| [T-029 Production source/data/key recovery](T-029-recovery-qualification.md) | T-028, T-044, T-105 | deferred |
| [T-030 Agent profile expansion](T-030-agent-profiles.md) | T-012, T-020 | done |
| [T-031 Verified OpenCode headless wrapper](T-031-opencode-harness.md) | T-016, T-030, T-032, T-033, T-074 | done |
| [T-032 First model gateway protocol](T-032-ai-gateway.md) | T-010, T-012, T-020 | done |
| [T-033 Streamable HTTP MCP proxy](T-033-mcp-proxy.md) | T-010, T-012, T-019, T-020 | done |
| [T-034 Canonical human gates](T-034-human-gates.md) | T-010, T-015, T-020 | done |
| [T-035 Environment and deployment records](T-035-deployments.md) | T-012, T-020, T-022, T-076 | done |
| [T-036 Native Git push trigger](T-036-git-webhooks.md) | T-010, T-012, T-013, T-014, T-020 | done |
| [T-037 Environment and deployment CLI](T-037-delivery-cli.md) | T-025, T-035 | done |
| [T-038 M2 agent-to-preview acceptance](T-038-m2-acceptance.md) | T-026, T-039, T-065, T-067, T-073, T-101 | done |
| [T-039 M2 composition](T-039-m2-integration.md) | T-031, T-032, T-033, T-034, T-035, T-036, T-037, T-068, T-072 | done |
| [T-040 QA evidence-to-defect mapping](T-040-exploratory-qa.md) | T-011, T-015, T-030, T-035 | done |
| [T-041 Durable outbound ChatOps links](T-041-chatops.md) | T-005, T-010 | done |
| [T-042 SQL flow summaries](T-042-flow-metrics.md) | T-011 | done |
| [T-043 Executable Go/PostgreSQL blueprint](T-043-go-blueprint.md) | T-014, T-030 | done |
| [T-044 Full SDLC acceptance](T-044-m3-acceptance.md) | T-038, T-046 | done |
| [T-045 Work dependency graph](T-045-work-dependencies.md) | T-011 | done |
| [T-046 M3 composition](T-046-m3-integration.md) | T-040, T-041, T-042, T-043, T-045, T-075 | done |
| [T-047 Supabase/Hono/Vite blueprint](T-047-supabase-blueprint.md) | T-014, T-030 | deferred |
| [T-050 Engineer workbench](T-050-engineer-workbench.md) | T-044, T-061 | deferred |
| [T-051 Analyst input and review](T-051-analyst-interface.md) | T-050 | deferred |
| [T-052 Native harness adapter](T-052-native-harness.md) | T-038 | deferred |
| [T-053 Claude Code wrapper](T-053-claude-harness.md) | T-038 | deferred |
| [T-054 Codex wrapper](T-054-codex-harness.md) | T-038 | deferred |
| [T-055 Untrusted hosted execution qualification](T-055-hosted-isolation.md) | T-038, T-056 | deferred |
| [T-056 Supported production and public release](T-056-production-release.md) | T-029, T-044, T-061, T-090 | deferred |
| [T-057 Assess operator need](T-057-operator-assessment.md) | T-056 | deferred |
| [T-058 Implement justified operator automation](T-058-operator.md) | T-057 | deferred |
| [T-059 Tenant/project Markdown chassis guidance](T-059-chassis-assessment.md) | T-014, T-030 | deferred |
| [T-060 Integrate selected M4 product extensions](T-060-m4-integration.md) | T-051, T-052, T-053, T-054, T-056 | deferred |
| [T-061 Optimi Platform integration and hosted acceptance](T-061-platform-integration.md) | T-039, T-046 | deferred |
| [T-062 Execution/results transaction composition prerequisite](T-062-execution-transaction-composition.md) | T-020, T-022 | done |
| [T-063 Entrypoint retained-log upload](T-063-entrypoint-log-upload.md) | T-016, T-023 | done |
| [T-064 Live/retained log qualification](T-064-log-qualification.md) | T-021, T-022, T-023, T-063 | done |
| [T-065 Ordinary deployment tools image and scripts](T-065-deployment-tools.md) | T-016, T-022, T-035, T-091 | done |
| [T-066 Deployment expiry and cleanup-run admission](T-066-deployment-cleanup.md) | T-020, T-035, T-077, T-080 | done |
| [T-067 Deployment apply/reconcile/cleanup qualification](T-067-deployment-qualification.md) | T-021, T-022, T-035, T-065, T-066, T-077, T-091, T-092, T-093 | done |
| [T-068 Profile and evidence-bound gate CLI](T-068-profile-gate-cli.md) | T-025, T-030, T-034 | done |
| [T-069 Exploratory QA image/profile qualification](T-069-qa-profile-qualification.md) | T-038, T-040, T-047, T-067 | deferred |
| [T-070 Incremental core application assembly](T-070-core-assembly.md) | T-010, T-011, T-012, T-013, T-014, T-015, T-017, T-019, T-020, T-024, T-062 | done |
| [T-071 Incremental container execution assembly](T-071-execution-assembly.md) | T-070, T-016, T-018, T-021, T-022, T-064, T-080, T-081, T-082, T-086, T-088 | done |
| [T-072 Incremental manual agent/review/delivery assembly](T-072-agent-delivery-assembly.md) | T-027, T-030, T-031, T-032, T-033, T-034, T-035, T-037, T-066, T-068, T-077, T-083, T-084, T-092, T-093 | done |
| [T-073 Gate/execution transaction qualification prerequisite](T-073-gate-transaction-qualification.md) | T-034, T-062, T-086, T-091 | done |
| [T-074 Early OpenCode wrapper protocol checkpoint](T-074-wrapper-protocol-checkpoint.md) | T-016, T-030 | done |
| [T-075 Incremental business-loop assembly](T-075-business-loop-assembly.md) | T-039, T-040, T-041, T-042, T-045 | done |
| [T-076 Execution environment snapshot contract](T-076-environment-pinning.md) | T-020, T-022 | done |
| [T-077 Delivery environment and cleanup binding](T-077-environment-pinning-qualification.md) | T-035, T-062, T-076, T-080 | done |
| [T-078 Entrypoint container-log mirror](T-078-entrypoint-log-mirror.md) | T-016, T-063 | done |
| [T-079 Job checkout credential and secret-directory delivery](T-079-job-checkout-credential.md) | T-016, T-021, T-063, T-078 | done |
| [T-080 Execution core unit qualification](T-080-execution-unit-core.md) | T-020, T-030, T-034, T-062, T-076, T-086, T-087 | done |
| [T-081 Result and log core unit qualification](T-081-result-log-unit-core.md) | T-022, T-023, T-062, T-064, T-086, T-087 | done |
| [T-082 Entrypoint unit qualification](T-082-entrypoint-unit-core.md) | T-016, T-063, T-078, T-079, T-086 | done |
| [T-083 Agent and model core unit qualification](T-083-agent-gateway-unit-core.md) | T-030, T-032, T-086, T-087 | done |
| [T-084 Gate and tool core unit qualification](T-084-gate-tool-unit-core.md) | T-033, T-034, T-062, T-086, T-087 | done |
| [T-085 Live agent and wrapper qualification after M3](T-085-live-agent-qualification.md) | T-044, T-031, T-038 | deferred |
| [T-086 Runtime ports and adapters](T-086-runtime-ports-adapters.md) | T-001, T-016, T-019, T-020, T-021, T-022, T-023, T-030, T-031, T-032, T-034, T-035, T-040, T-062, T-063, T-064, T-070, T-076, T-078, T-079 | done |
| [T-087 In-memory transaction kernel](T-087-memory-kernel.md) | T-003 | done |
| [T-088 Foundation memory adapters and contracts](T-088-foundation-memory-adapters.md) | T-087, T-005, T-011, T-012, T-013, T-015, T-019 | done |
| [T-089 Kubernetes runtime and chart conformance](T-089-kubernetes-conformance.md) | T-026, T-018, T-021, T-064, T-086, T-095, T-102 | done |
| [T-090 Trusted-engineer staging deployment](T-090-staging-deployment.md) | T-089, T-044, T-028, T-104 | planned |
| [T-091 Cluster-free test stack](T-091-test-stack.md) | T-001, T-002, T-086 | done |
| [T-092 Cleanup-run deployment input handoff](T-092-cleanup-deployment-input.md) | T-065, T-066, T-077 | done |
| [T-093 Delivery effect reconciliation adapter](T-093-delivery-effect-reconciler.md) | T-035, T-065, T-077, T-080 | done |
| [T-094 Workspace-free step claim shape](T-094-execution-claim-workspace-shape.md) | T-020, T-080, T-088 | done |
| [T-095 Kubernetes runtime contract conformance](T-095-kube-runtime-contract-conformance.md) | T-021, T-086 | done |
| [T-096 Runtime action-ceiling policy](T-096-runtime-action-ceiling.md) | T-032, T-033, T-086 | done |
| [T-097 Delivery production joins](T-097-delivery-production-joins.md) | T-035, T-067, T-073, T-093 | done |
| [T-098 Podman source-checkout issuance](T-098-podman-checkout.md) | T-079, T-086, T-096 | done |
| [T-099 Artifact output naming](T-099-entrypoint-artifact-naming.md) | T-016, T-023, T-078 | done |
| [T-100 Native Git trigger production adapters](T-100-git-trigger-production-adapters.md) | T-012, T-036, T-088 | done |
| [T-101 M2 production assembly joins](T-101-m2-production-assembly.md) | T-075, T-096, T-097, T-098, T-099, T-100 | done |
| [T-102 Runtime launch-evidence capability](T-102-runtime-launch-evidence.md) | T-086, T-095 | done |
| [T-103 Remaining M1 acceptance cleanup](T-103-m1-acceptance-cleanup.md) | T-026, T-099, T-101 | done |
| [T-104 Full Kubernetes installation readiness](T-104-kubernetes-full-readiness.md) | T-089, T-105, T-103 | planned |
| [T-105 Secrets startup wiring](T-105-secrets-startup.md) | T-028, T-044, T-103 | done |
| [T-106 Production TLS hardening with a private CA](T-106-production-tls-hardening.md) | T-104, T-090, T-105 | deferred |

## Verification and scope

`make docs-check` validates links, metadata/index agreement, dependency cycles and exclusive ownership for all unordered pairs. Completed/deferred task records may retain historical metadata; scheduling deferred work requires bringing its check contract up to date. Do not claim a production, hosted-authority, live-provider or browser qualification from local unit/core-checkpoint evidence.
