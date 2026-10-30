# Development task graph

This backlog holds the planned and deferred tasks. Completed work is not listed here: its task records were removed once merged and remain in Git history (last present at commit `354b760`). Keep one Go module and preserve working packages. Production deployment and qualification, external adapters, the chassis and additional wrappers each need explicit scheduling before they start.

<!-- harness:summary:start -->

**Ready now:** none. Everything left is deferred and needs explicit scheduling.

**Deferred:** [T-029](T-029-recovery-qualification.md), [T-047](T-047-supabase-blueprint.md), [T-050](T-050-engineer-workbench.md), [T-051](T-051-analyst-interface.md), [T-052](T-052-native-harness.md), [T-053](T-053-claude-harness.md), [T-054](T-054-codex-harness.md), [T-055](T-055-hosted-isolation.md), [T-056](T-056-production-release.md), [T-057](T-057-operator-assessment.md), [T-058](T-058-operator.md), [T-059](T-059-chassis-assessment.md), [T-060](T-060-m4-integration.md), [T-061](T-061-platform-integration.md), [T-069](T-069-qa-profile-qualification.md), [T-085](T-085-live-agent-qualification.md).

Generated from task frontmatter by `bun ../openforge/src/cli.ts index --write`; do not edit by hand.

<!-- harness:summary:end -->

## Delivery order

Work climbs three environments ([ADR 0009](../adr/0009-environments.md), [environments](../implementation/environments.md)): `development` (in memory, no containers), `test` (Podman services and runtime, no cluster) and `production` (Kubernetes). Local M1–M3 composition acceptance is merged; what remains is below.

| Phase | Work | Environment | Merge evidence |
| --- | --- | --- | --- |
| Full Kubernetes readiness | T-104 | `production` (stage cluster `kodama-stage-scw`, live KMS) | Done: readiness requalified at `0a905ef5989d` with the staging gate ([report](../acceptance/kube-full.md)); live KMS acceptance, ready chart and original assembled M1 roundtrips retained from T-089 |
| Staging | T-104 → T-090 trusted-engineer deployment to the stage cluster | `production` (stage cluster) | Explicitly scheduled; full-readiness prerequisite, its own handoff and smoke evidence |
| After M3, explicitly scheduled | T-047 Supabase, T-059 chassis, T-069 browser QA, T-085 live agent/wrapper, other M4 and production tasks | `production` | Their separately declared integration/release evidence |

Package completion is not milestone acceptance. No real SQL/RLS/transaction/provider behavior is certified by a mock. It is deliberately qualified later, before the corresponding checkpoint is accepted. The [roadmap](../book/07-delivery-roadmap.md) describes the product scope.

### Shared run workspace implementation (owner scheduled 2026-10-26)

[O-28](../decisions.md#run-workspace-and-builders--2026-10-26-o-28) schedules the [P0–P7 plan](../implementation/run-workspace-services-builders.md). T-109 updates the owning contracts; [T-118](T-118-pipeline-version-admission.md) independently fences the current executor to its implemented document version before T-110 extends shared parsing. T-110 depends on both and implements the wire/parser checkpoint. T-111 (real profile qualification) and T-112 (lifecycle) follow independently; T-113/T-114/T-115 wait for both, followed by T-116 assembly and T-117 acceptance. Other deferred tasks are not promoted. The generated summary above has not been regenerated: the separate OpenForge checkout is unavailable. The task frontmatter and this explicit scheduling notice govern these new tasks until `index --write` and `validate` can run.

T-118 is reviewed, unit-tested and merged locally at `16a5d0df`; T-109's reviewed specification checkpoint is merged at `9db322ff`; T-110's reviewed executable schema/parser checkpoint is merged at `6343b9e5`. P0 is complete; T-111 (real profile qualification) and T-112 (execution lifecycle/persistence) are ready independently. New-format registration stays off by default and the current executor refuses it: no shared-workspace/service runtime or builder qualification is enabled by P0.

T-119 is merged at `b75dca74`. T-112's repaired lifecycle/persistence component is reviewed, checked and merged as one squashed commit `978a4e6b`; T-111's reviewed driver/blocked-evidence delivery is merged as one squashed commit `dea45c6c`. The bounded migration join preserves the legacy PostgreSQL assembly, without enabling shared runs. No passing full P1 qualification is claimed.

**Owner closeout update (O-29):** T-111 closes reviewed qualification-driver and blocked-evidence delivery, not full P1. [T-120](T-120-kubernetes-builder-profile.md) (configure the role-scoped Kubernetes profile) and [T-121](T-121-run-profile-requalification.md) (real verification) remain **planned for later** and were not executed. T-113/T-114/T-115 depend on T-121, so closing T-111 cannot bypass qualification. The current work is reviewed/tested/squashed/merged; no cluster mutation or wider queue restart was included. The existing HALT remains in place pending an explicit request to resume.

**Owner scope revision (O-30, 2026-10-29):** [O-30](../decisions.md#revised-scope--2026-10-29-o-30) supersedes the O-29 gating. [T-122](T-122-o30-scope-amendment.md) amends the contracts and admission derivation and freezes the integration seams. Then T-113 (authority/entrypoint), T-114 (Kubernetes), T-115 (local runtimes), T-120 (node profiles) and [T-124](T-124-runprofile-driver-o30.md) (driver update) proceed in parallel on unit/component evidence; T-116 assembles; T-121 installs and qualifies the profile on stage and T-117 accepts the merged image and chart there. BuildKit moves to deferred [T-123](T-123-buildkit-builder-service.md); browser work is removed.

```text
T-122 ─┬─ T-113 ─┐
       ├─ T-114 ─┼─ T-116 ─┐
       ├─ T-115 ─┤         ├─ T-117 ── (T-123 deferred)
       ├─ T-120 ─┴─ T-121 ─┘
       └─ T-124 ──┘
```

### Internal Git service (owner scheduled 2026-10-30)

[O-31](../decisions.md#internal-git-service--2026-10-30-o-31) schedules the [internal Git service plan](../implementation/internal-git-service.md) ([ADR 0011](../adr/0011-internal-tenant-git-service.md)). [T-125](T-125-git-service-architecture.md) records the architecture; [T-126](T-126-git-service-contracts.md) freezes the contracts; T-127 (repository lifecycle), T-128 (Git worker) and T-129 (tenant hosting) proceed in parallel on new packages; T-130 adds the push journal and triggers. T-131 and T-132 edit files owned by T-113 and T-116 and therefore follow them. T-133 (stage acceptance and cutover) is blocked on O-32. T-117 now depends on T-125 because both own `docs/traceability.md`. The generated summary above has not been regenerated; this notice and the task frontmatter govern these tasks until `index --write` and `validate` run.

```text
T-125                  → T-126
T-126                  → T-127, T-128, T-129
T-127, T-128           → T-130
T-113, T-130           → T-131
T-116, T-127 … T-131   → T-132
T-117, T-132           → T-133 (blocked: O-32)
```

## Task contract

### Container-first SDLC and artifacts (O-34; planning handoff)

The owner approved [ADR 0012](../adr/0012-container-sdlc-and-artifact-releases.md) and the [implementation plan](../implementation/container-sdlc-refinement.md). [T-140–T-146](../implementation/container-sdlc-refinement.md#ordered-implementation-tasks) are reconciled with ADR 0011 (`T-125–T-133`) and implemented serially in `.work/artifact-vision-review`.

```text
T-116, T-131 → T-140 → T-141 → T-142 → T-143 → T-144 ─┬→ T-145 ─┬→ T-146
T-132 ────────────────────────────────────────────────┘        │
T-117, T-133 ──────────────────────────────────────────────────┘
```

The generated summary above remains untouched while the separate OpenForge checkout is unavailable; the explicit section and dependency index include this addition.

### Metadata

Scheduled (`planned` or `blocked`) tasks declare `owns`, `check`, optional `resources`, and `acceptance`:

```yaml
check: make check-go PKGS=./internal/execution/...
resources: [go]
acceptance: [unit_dispatch_replay, unit_cancel_fencing]
```

- `make check-go PKGS=...`: runs scoped `gofmt`, `go vet -tags unit`, and `go test -json -race -count=1 -tags unit`, writing test outcomes to `$FORGE_RESULTS`.
- `make check-devtools`: formats, vets and tests the Go development tooling under `dev/`. It does not emit `FORGE_RESULTS`; tooling tasks name the Go test function in `acceptance`.
- `make check-go-integration PKGS=...`: provisions `DEV_SCOPE` when it does not exist (stack from `DEV_STACK`, default `cluster`; `KODAMA_AUTO_PROVISION=0` disables), preflights it (exiting `75` if the environment is unavailable), loads `host-env.json`, and runs scoped integration tests.
- `plan: true`: opts into the read-only planner stage before implementation (off by default).
- `review: human`: parks the task for human review after checks pass.

Statuses: `planned` becomes ready when its dependencies are `done`; `blocked` needs its named decision; `deferred` needs explicit scheduling approval; `done` means reviewed, tested, and merged.

## Ownership and integration

1. One task branch/worktree under the primary checkout's ignored `.work/`; one writer per owned path. Follow [AGENTS.md](../../AGENTS.md). Preserve existing work and resumable sessions.
2. Read the assigned task and relevant contracts. Implement domain behavior through existing small, consumer-owned interfaces. Use doubles for actual external effects; avoid a universal repository, transaction abstraction or interface per type.
3. Edit only `owns`. Task metadata/index belongs to the coordinator unless explicitly owned. Shared composition/generated API/client files have one ordered owner: the `internal/app` composition owner. Request a bounded prerequisite for an unowned interface change.
4. Run focused checks while editing. The coordinator independently qualifies the declared task scope and obtains review on the exact candidate. Go changes do not rerun OpenForge or development Python tests. Documentation/config checks run for relevant changes, not automatically for every Go edit.
5. Commit/checkpoint before rebasing at a clean stage boundary. An interrupted dirty implementer resumes its existing invocation first. Conflicts preserve the task for reconciliation; no automatic stash/reset/discard.
6. Publish only reviewed, qualified code. Rebase movement that changes relevant inputs renews affected evidence; unrelated main movement does not trigger another full suite. Keep pending branches/worktrees; clean only the merged task's resources.

Work can merge with unit evidence before integration is available. Executable constructors still choose real adapters and fail closed; test doubles are injected by tests, never installed as permissive production fallbacks. Milestones remain unaccepted until their actual checkpoint passes.

## Acceptance ownership transfers

| Source | Unit contribution | Required real acceptance owner |
| --- | --- | --- |
| T-089 (O-24 transfer) | Full installed-server readiness and assembled Kubernetes M1 acceptance | T-104: original `kube_m1_roundtrip`, `kube_cancel_restart`, `kube_chart_install`, including the same backend-parameterized `execution_assembly_*` tests; T-090 depends on completion |
| T-038 original live agent | Local deterministic agentic checkpoint | T-085: real pinned wrapper/model/provider, rework, needs-input and budget smoke |

## Periodic validation and failure handling

After five task merges, and at queue drain, validate captured `main` once. Initially this is core Go unit coverage (`GO_CORE_PACKAGES` / `make test-go-unit-json`), excluding image wrappers, templates and integration drivers. The M1 and M2 checkpoints add bounded integration. Reuse a prepared `batch` scope and isolate data. Run OpenForge/Python tooling suites only when their inputs changed. No 30-minute age trigger or second full suite at publication.

A code/compiler/assertion failure goes to a focused fixer. Missing environment or timeout pauses infrastructure with a receipt and actionable reason, without consuming code-repair budget. Cancellation preserves resumable work. A failed batch pauses publication, not implementation/review. See [OpenForge](../implementation/harness.md) and [testing](../implementation/development-and-testing.md).

Agent concurrency follows `--workers`; resource capacity separately bounds Go, service and kind jobs. Unit tasks use only Go capacity. Integration jobs declare their service/kind needs and run only after scope preflight. The queue HALT does not disable standalone environment maintenance.

## Dependency index

| Task | Dependencies | Status / decision |
| --- | --- | --- |
| [T-029 Production source/data/key recovery](T-029-recovery-qualification.md) | — | deferred |
| [T-047 Supabase/Hono/Vite blueprint](T-047-supabase-blueprint.md) | — | deferred |
| [T-050 Engineer workbench](T-050-engineer-workbench.md) | T-061 | deferred |
| [T-051 Analyst input and review](T-051-analyst-interface.md) | T-050 | deferred |
| [T-052 Native harness adapter](T-052-native-harness.md) | — | deferred |
| [T-053 Claude Code wrapper](T-053-claude-harness.md) | — | deferred |
| [T-054 Codex wrapper](T-054-codex-harness.md) | — | deferred |
| [T-055 Untrusted hosted execution qualification](T-055-hosted-isolation.md) | T-056 | deferred |
| [T-056 Supported production and public release](T-056-production-release.md) | T-029, T-061, T-090, T-106 | deferred |
| [T-057 Assess operator need](T-057-operator-assessment.md) | T-056 | deferred |
| [T-058 Implement justified operator automation](T-058-operator.md) | T-057 | deferred |
| [T-059 Tenant/project Markdown chassis guidance](T-059-chassis-assessment.md) | — | deferred |
| [T-060 Integrate selected M4 product extensions](T-060-m4-integration.md) | T-051, T-052, T-053, T-054, T-056 | deferred |
| [T-061 Optimi Platform integration and hosted acceptance](T-061-platform-integration.md) | — | deferred |
| [T-069 Exploratory QA image/profile qualification](T-069-qa-profile-qualification.md) | T-047 | deferred |
| [T-085 Live agent and wrapper qualification after M3](T-085-live-agent-qualification.md) | — | deferred |
| [T-090 Trusted-engineer staging deployment](T-090-staging-deployment.md) | T-104 | done |
| [T-104 Full Kubernetes installation readiness](T-104-kubernetes-full-readiness.md) | — | done |
| [T-106 Production TLS hardening with a private CA](T-106-production-tls-hardening.md) | T-104, T-090 | deferred |
| [T-107 Direct project-secret delivery to ordinary steps](T-107-direct-secret-delivery.md) | T-104 | done |
| [T-108 Result finalization ordering and actionable dispatch diagnostics](T-108-execution-finalization-diagnostics.md) | T-104 | done |
| [T-113 One preparation, scoped authority and cumulative publication](T-113-run-workspace-authority.md) | T-112, T-122 | done |
| [T-114 Kubernetes run resources and execution policy](T-114-kubernetes-run-resources.md) | T-112, T-122 | done |
| [T-115 Podman and Process run resources](T-115-local-run-resources.md) | T-112, T-122 | done |
| [T-116 Resource assembly, chart and migration](T-116-run-resource-assembly.md) | T-113, T-114, T-115, T-120 | done |
| [T-134 Wire the Podman and Process run-resource ports](T-134-local-run-shared-wiring.md) | T-115, T-116 | planned |
| [T-117 Stage acceptance and cutover](T-117-run-resource-acceptance.md) | T-116, T-121, T-125 | planned |
| [T-120 Kubernetes builder step profile](T-120-kubernetes-builder-profile.md) | T-111, T-122 | done |
| [T-121 Install and qualify the stage run-resource profile](T-121-run-profile-requalification.md) | T-120, T-124 | done |
| [T-122 O-30 scope amendment](T-122-o30-scope-amendment.md) | T-109, T-110, T-112 | done |
| [T-123 BuildKit builder service](T-123-buildkit-builder-service.md) | T-117 | deferred |
| [T-124 Align the qualification driver with O-30](T-124-runprofile-driver-o30.md) | T-111, T-122 | done |
| [T-125 Internal Git service: documentation and ADR](T-125-git-service-architecture.md) | — | done |
| [T-126 Internal Git service: contract checkpoint](T-126-git-service-contracts.md) | T-125 | planned |
| [T-127 Repository lifecycle and persistence](T-127-repository-lifecycle.md) | T-126 | planned |
| [T-128 Native Git worker](T-128-git-worker.md) | T-126 | planned |
| [T-129 Tenant Git hosting on Kubernetes](T-129-tenant-git-hosting.md) | T-126 | planned |
| [T-130 Push journal, events and triggers](T-130-git-push-events.md) | T-127, T-128 | planned |
| [T-131 Task, agent and source integration](T-131-git-source-integration.md) | T-113, T-130 | planned |
| [T-132 API, CLI, MCP and chart assembly](T-132-git-service-assembly.md) | T-116, T-127, T-128, T-129, T-130, T-131 | planned |
| [T-133 Kubernetes acceptance and cutover](T-133-git-service-acceptance.md) | T-117, T-132 | blocked (O-32) |
| [T-140 Container-first SDLC executable contracts](T-140-container-sdlc-contracts.md) | T-116, T-131 | deferred |
| [T-141 Artifact catalogue and immutable releases](T-141-artifact-release-catalog.md) | T-140 | deferred |
| [T-142 Container inputs, secrets and provenance](T-142-container-inputs-and-provenance.md) | T-141 | deferred |
| [T-143 Generic deployment and cleanup records](T-143-generic-delivery-records.md) | T-142 | deferred |
| [T-144 Publication verification and tool recipes](T-144-artifact-publication-recipes.md) | T-143 | deferred |
| [T-145 SDLC assembly and legacy surface removal](T-145-container-sdlc-assembly.md) | T-144, T-132 | deferred |
| [T-146 Real release/deploy/rollback acceptance](T-146-container-sdlc-acceptance.md) | T-145, T-117, T-133 | deferred |

## Verification and scope

`make docs-check` validates links, anchors and JSON/shell fences. Task metadata/index agreement, dependency cycles and exclusive ownership are additional checks (`make validate-tasks` when the separate OpenForge checkout is available); do not infer them from a docs-check pass. Completed/deferred task records may retain historical metadata; scheduling deferred work requires bringing its check contract up to date. Do not claim a production, hosted-authority, live-provider or browser qualification from local unit/core-checkpoint evidence.
