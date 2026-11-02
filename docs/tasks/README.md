# Development task graph

This backlog holds the planned and deferred tasks. Completed work is not listed here: its task records were removed once merged and remain in Git history (last present at commit `354b760`). Keep one Go module and preserve working packages. Production deployment and qualification, external adapters, the chassis and additional wrappers each need explicit scheduling before they start.

**Ready by dependencies** (`planned`, every dependency `done`): none. The P2 queue is drained; P3 starts on a new branch after publication. **Blocked on a decision:** none. Other planned tasks wait for the dependencies shown in the [dependency index](#dependency-index).

**Deferred** (needs explicit scheduling): [T-029](T-029-recovery-qualification.md), [T-047](T-047-supabase-blueprint.md), [T-050](T-050-engineer-workbench.md), [T-051](T-051-analyst-interface.md), [T-052](T-052-native-harness.md), [T-053](T-053-claude-harness.md), [T-054](T-054-codex-harness.md), [T-055](T-055-hosted-isolation.md), [T-056](T-056-production-release.md), [T-057](T-057-operator-assessment.md), [T-058](T-058-operator.md), [T-059](T-059-chassis-assessment.md), [T-060](T-060-m4-integration.md), [T-061](T-061-platform-integration.md), [T-069](T-069-qa-profile-qualification.md), [T-085](T-085-live-agent-qualification.md), [T-106](T-106-production-tls-hardening.md), [T-123](T-123-buildkit-builder-service.md).

This summary is maintained by the coordinator by hand from the task frontmatter; it is not generated. The frontmatter and the dependency index govern, and `make validate-tasks` checks the index against them.

## Delivery order

Work climbs three environments ([ADR 0009](../adr/0009-environments.md), [environments](../implementation/environments.md)): `development` (in memory, no containers), `test` (Podman services and runtime, no cluster) and `production` (Kubernetes). Local M1–M3 composition acceptance is merged; what remains is below.

| Phase | Work | Environment | Merge evidence |
| --- | --- | --- | --- |
| Full Kubernetes readiness | T-104 | `production` (stage cluster `kodama-stage-scw`, live KMS) | Done: readiness requalified at `0a905ef5989d` with the staging gate ([report](../acceptance/kube-full.md)); live KMS acceptance, ready chart and original assembled M1 roundtrips retained from T-089 |
| Staging | T-104 → T-090 trusted-engineer deployment to the stage cluster | `production` (stage cluster) | Explicitly scheduled; full-readiness prerequisite, its own handoff and smoke evidence |
| After M3, explicitly scheduled | T-047 Supabase, T-059 chassis, T-069 browser QA, T-085 live agent/wrapper, other M4 and production tasks | `production` | Their separately declared integration/release evidence |

Package completion is not milestone acceptance. No real SQL/RLS/transaction/provider behavior is certified by a mock. It is deliberately qualified later, before the corresponding checkpoint is accepted. The [roadmap](../book/07-delivery-roadmap.md) describes the product scope.

### Shared run workspace implementation (owner scheduled 2026-10-26)

[O-28](../decisions.md#run-workspace-and-builders--2026-10-26-o-28) schedules the [P0–P7 plan](../implementation/run-workspace-services-builders.md). T-109 updates the owning contracts; [T-118](T-118-pipeline-version-admission.md) independently fences the current executor to its implemented document version before T-110 extends shared parsing. T-110 depends on both and implements the wire/parser checkpoint. T-111 (real profile qualification) and T-112 (lifecycle) follow independently; T-113/T-114/T-115 wait for both, followed by T-116 assembly and T-117 acceptance. Other deferred tasks are not promoted. The task frontmatter and this scheduling notice govern these tasks; `make validate-tasks` checks them.

T-118 is reviewed, unit-tested and merged locally at `16a5d0df`; T-109's reviewed specification checkpoint is merged at `9db322ff`; T-110's reviewed executable schema/parser checkpoint is merged at `6343b9e5`. P0 is complete; T-111 (real profile qualification) and T-112 (execution lifecycle/persistence) are ready independently. New-format registration stays off by default and the current executor refuses it: no shared-workspace/service runtime or builder qualification is enabled by P0.

T-119 is merged at `b75dca74`. T-112's repaired lifecycle/persistence component is reviewed, checked and merged as one squashed commit `978a4e6b`; T-111's reviewed driver/blocked-evidence delivery is merged as one squashed commit `dea45c6c`. The bounded migration join preserves the legacy PostgreSQL assembly, without enabling shared runs. No passing full P1 qualification is claimed.

**Owner closeout update (O-29):** T-111 closes reviewed qualification-driver and blocked-evidence delivery, not full P1. [T-120](T-120-kubernetes-builder-profile.md) (configure the role-scoped Kubernetes profile) and [T-121](T-121-run-profile-requalification.md) (real verification) remain **planned for later** and were not executed. T-113/T-114/T-115 depend on T-121, so closing T-111 cannot bypass qualification. The current work is reviewed/tested/squashed/merged; no cluster mutation or wider queue restart was included. The existing HALT remains in place pending an explicit request to resume.

**Owner scope revision (O-30, 2026-10-29):** [O-30](../decisions.md#revised-scope--2026-10-29-o-30) supersedes the O-29 gating. [T-122](T-122-o30-scope-amendment.md) amends the contracts and admission derivation and freezes the integration seams. Then T-113 (authority/entrypoint), T-114 (Kubernetes), T-115 (local runtimes), T-120 (node profiles) and [T-124](T-124-runprofile-driver-o30.md) (driver update) proceed in parallel on unit/component evidence; T-116 assembles; T-121 installs and qualifies the profile on stage and T-117 accepts the merged image and chart there. BuildKit moves to deferred [T-123](T-123-buildkit-builder-service.md); browser work is removed.

**Run-shared status and parked work (2026-10-30):** T-122, T-120, T-124, T-114, T-113, T-116, T-121 and T-115 are reviewed, checked and merged; `run_shared` is composed for Kubernetes (off by default) and the builder profile is qualified on stage, with the owner's AppArmor amendment (versioned step profile `kodama-rootless-build-v2`) and immediate-cutover decision recorded under [O-30](../decisions.md#revised-scope--2026-10-29-o-30). **Parked by the owner** until stage access is provisioned on the operator machine: [T-117](T-117-run-resource-acceptance.md) (stage acceptance and cutover; see its resume checklist). **Outstanding, unparked:** [T-134](T-134-local-run-shared-wiring.md) (Podman/Process composition; local lifecycle evidence only), the v1-only context readers of agent, QA and delivery steps inside `run_shared` runs (no task yet; they fail closed), and deferred [T-123](T-123-buildkit-builder-service.md) (BuildKit).

```text
T-122 ─┬─ T-113 ─┐
       ├─ T-114 ─┼─ T-116 ─┐
       ├─ T-115 ─┤         ├─ T-117 ── (T-123 deferred)
       ├─ T-120 ─┴─ T-121 ─┘
       └─ T-124 ──┘
```

### Internal Git service (owner scheduled 2026-10-30)

[O-31](../decisions.md#internal-git-service--2026-10-30-o-31) scheduled the internal Git service ([ADR 0011](../adr/0011-internal-tenant-git-service.md)); O-32 and O-33 were resolved the same day (no provider import; bootstrap by an engineer's push; default branch protected by default). The earlier graph (T-126–T-164) delivered the native worker, tenant hosting, credentials and the legacy assembly against the pre-ADR 0013 server; those contributions are merged history.

[O-36](../decisions.md#finishing-the-git-service-on-the-kernel--2026-10-30-o-36) replaces the remaining assembly, installation and acceptance work: the Git service now finishes against the transitional ADR 0013 kernel over the current 16-table product schema, and full container, Kubernetes and end-to-end acceptance runs on `main` after the merge. Every task branches from and merges into `integration/native-git` (worktree `.work/native-git-integration/`); publication to `main` is a separate owner decision. [O-38](../decisions.md#pipeline-runtime-and-four-layer-refinement--2026-10-30-o-38) subsequently replaces the target kernel/shell boundary and the task-state Git approval rule; see the [pipeline-runtime plan](../implementation/pipeline-runtime-simplification.md).

```text
T-166 → T-167 → T-168 → T-169 → T-170
```

T-167 keeps the native worker and appends one `003_git_hosting.sql` contribution. T-168 replaces the push-record journal with the worker journal plus `repositories.journal_ack` and one `git.refs_updated` event per receive. T-169 gives runs a pinned checkout and a run-scoped credential. T-170 makes it runnable in `development` and `test` and publishes an accurate API document.

Superseded and deferred by O-36: [T-132](T-132-git-service-assembly.md), [T-133](T-133-git-service-acceptance.md), [T-139](T-139-local-git-installation.md), [T-152](T-152-runtime-git-adapters.md), [T-154](T-154-git-cli.md), [T-156](T-156-git-chart-operations.md), [T-157](T-157-stage-git-installation.md), [T-158](T-158-git-api-aggregate.md). Their records retain the merged code contributions and open findings.

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

- `make check-go PKGS=...`: runs scoped `gofmt`, `go vet -tags unit`, and `go test -json -race -count=1 -tags unit`, writing test outcomes to the file named by `$FORGE_RESULTS` when it is set (`make check-task` sets it).
- `make check-devtools`: formats, vets and tests the Go development tooling under `dev/`. It writes no results file, so a task whose check is `make check-devtools` declares `acceptance: []`.
- `make check-go-integration PKGS=...`: provisions `DEV_SCOPE` when it does not exist (stack from `DEV_STACK`, default `cluster`; `KODAMA_AUTO_PROVISION=0` disables), preflights it (exiting `75` if the environment is unavailable), loads `host-env.json`, and runs scoped integration tests.
- `make check-task TASK=T-NNN`: runs the task's declared `check` from the repository root with `KODAMA_TASK_ID`, `KODAMA_RESOURCE_PREFIX` and `DEV_SCOPE` set and a fresh `FORGE_RESULTS`, and verifies every `acceptance` name as passed. It neither schedules nor merges.
- `plan: true`: asks the coordinator (or an optional external harness) for a read-only plan before implementation (off by default).
- `review: human`: a human approves the candidate after checks pass.

Statuses: `planned` becomes ready when its dependencies are `done`; `blocked` needs its named decision; `deferred` needs explicit scheduling approval; `done` means reviewed, tested, and merged.

## Ownership and integration

1. One task branch/worktree under the primary checkout's ignored `.work/`; one writer per owned path. Follow [AGENTS.md](../../AGENTS.md). Preserve existing work and resumable sessions.
2. Read the assigned task and relevant contracts. Implement domain behavior through existing small, consumer-owned interfaces. Use doubles for actual external effects; avoid a universal repository, transaction abstraction or interface per type.
3. Edit only `owns`. Task metadata/index belongs to the coordinator unless explicitly owned. Shared composition/generated API/client files have one ordered owner: the `internal/app` composition owner. Request a bounded prerequisite for an unowned interface change.
4. Run focused checks while editing. The coordinator independently qualifies the declared task scope and obtains review on the exact candidate. Go changes do not rerun development-tooling or Python tests. Documentation/config checks run for relevant changes, not automatically for every Go edit.
5. Commit/checkpoint before rebasing at a clean stage boundary. An interrupted dirty implementer resumes its existing invocation first. Conflicts preserve the task for reconciliation; no automatic stash/reset/discard.
6. Publish only reviewed, qualified code. Rebase movement that changes relevant inputs renews affected evidence; unrelated main movement does not trigger another full suite. Keep pending branches/worktrees; clean only the merged task's resources.

Work can merge with unit evidence before integration is available. Executable constructors still choose real adapters and fail closed; test doubles are injected by tests, never installed as permissive production fallbacks. Milestones remain unaccepted until their actual checkpoint passes.

## Acceptance ownership transfers

| Source | Unit contribution | Required real acceptance owner |
| --- | --- | --- |
| T-089 (O-24 transfer) | Full installed-server readiness and assembled Kubernetes M1 acceptance | T-104: original `kube_m1_roundtrip`, `kube_cancel_restart`, `kube_chart_install`, including the same backend-parameterized `execution_assembly_*` tests; T-090 depends on completion |
| T-038 original live agent | Local deterministic agentic checkpoint | T-085: real pinned wrapper/model/provider, rework, needs-input and budget smoke |

## Periodic validation and failure handling

After five task merges, and at queue drain, validate captured `main` once. Initially this is core Go unit coverage (`GO_CORE_PACKAGES` / `make test-go-unit-json`), excluding image wrappers, templates and integration drivers. The M1 and M2 checkpoints add bounded integration. Reuse a prepared `batch` scope and isolate data. Run development-tooling and Python suites only when their inputs changed. No 30-minute age trigger or second full suite at publication.

A code/compiler/assertion failure goes to a focused fixer. Missing environment or timeout pauses infrastructure with a receipt and actionable reason, without consuming code-repair budget. Cancellation preserves resumable work. A failed batch pauses publication, not implementation/review. See [task coordination](../implementation/harness.md) and [testing](../implementation/development-and-testing.md).

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
| [T-171 Kernel runtime contract and dependency boundary](T-171-kernel-runtime-contract.md) | — | done |
| [T-172 Freeze the pipeline runtime contract decisions](T-172-contract-decisions.md) | — | done |
| [T-173 Relocate shell responsibilities out of the kernel](T-173-kernel-shell-relocation.md) | — | done |
| [T-174 Memory state adapter and kernel state contract suite](T-174-kernel-state-contract.md) | T-171, T-173 | done |
| [T-175 Durable progression engine](T-175-kernel-engine.md) | T-171, T-172, T-174 | done |
| [T-176 Process runtime behind the kernel contract](T-176-process-runtime.md) | T-171 | done |
| [T-177 Podman runtime behind the kernel contract](T-177-podman-runtime.md) | T-171 | done |
| [T-178 Kubernetes runtime behind the kernel contract](T-178-kubernetes-runtime.md) | T-171 | done |
| [T-179 Pipeline runtime schema expansion](T-179-schema-expansion.md) | T-172 | done |
| [T-180 Traceability for the frozen pipeline-runtime contract](T-180-traceability.md) | T-172 | done |
| [T-181 Repair the legacy run_shared fixture](T-181-legacy-run-shared-fixture.md) | T-176 | done |
| [T-182 PostgreSQL kernel state adapter](T-182-runstate-postgres.md) | T-174, T-179, T-185, T-186, T-189, T-192 | done |
| [T-183 Project pipeline registry](T-183-pipeline-registry.md) | T-179 | done |
| [T-184 Task tracking and execution projection](T-184-task-tracking.md) | T-179, T-182, T-188, T-195 | done |
| [T-185 Durable run-state schema for the kernel record](T-185-durable-run-state-schema.md) | T-179 | done |
| [T-186 Step outputs and claim release in the kernel contract](T-186-kernel-outputs-and-release.md) | T-175 | done |
| [T-187 Git integration authority from the run, not the task](T-187-git-integration-authority.md) | T-183 | done |
| [T-188 Shell outbox and NATS bus](T-188-shell-outbox-nats.md) | T-174 | done |
| [T-189 Wait decision ordering and materialization records](T-189-wait-and-materialization-schema.md) | T-185 | done |
| [T-190 Migrate the dispatcher to the durable consumer-name scheme](T-190-dispatch-consumer-name.md) | T-188 | done |
| [T-191 Shell source preparation and secret custody adapters](T-191-shell-sources-and-secrets.md) | T-186, T-187 | done |
| [T-192 Self-contained signal decision reasons](T-192-signal-decision-reason.md) | T-189 | done |
| [T-193 Outbox cursor must not skip concurrently committed events](T-193-outbox-cursor-ordering.md) | T-188 | done |
| [T-194 Post-P2 handoff for P3–P5](T-194-post-p2-handoff.md) | T-184 | done |
| [T-195 Carry the task through admission and persist it on the run](T-195-admission-task-link.md) | T-182 | done |
| [T-117 Stage acceptance and cutover](T-117-run-resource-acceptance.md) | T-116, T-121, T-125 | deferred (parked until stage access) |
| [T-120 Kubernetes builder step profile](T-120-kubernetes-builder-profile.md) | T-111, T-122 | done |
| [T-121 Install and qualify the stage run-resource profile](T-121-run-profile-requalification.md) | T-120, T-124 | done |
| [T-122 O-30 scope amendment](T-122-o30-scope-amendment.md) | T-109, T-110, T-112 | done |
| [T-123 BuildKit builder service](T-123-buildkit-builder-service.md) | T-117 | deferred |
| [T-124 Align the qualification driver with O-30](T-124-runprofile-driver-o30.md) | T-111, T-122 | done |
| [T-125 Internal Git service: documentation and ADR](T-125-git-service-architecture.md) | — | done |
| [T-126 Internal Git service: contract checkpoint](T-126-git-service-contracts.md) | T-135 | done |
| [T-127 Repository lifecycle and persistence](T-127-repository-lifecycle.md) | T-126 | done |
| [T-128 Native Git worker](T-128-git-worker.md) | T-126 | done |
| [T-129 Tenant Git hosting on Kubernetes](T-129-tenant-git-hosting.md) | T-126 | done |
| [T-130 Push authorization, journal and events](T-130-git-push-events.md) | T-127, T-128, T-136 | done |
| [T-131 Source-set pipeline and runtime contracts](T-131-git-source-contracts.md) | T-126 | done |
| [T-132 Internal Git service assembly](T-132-git-service-assembly.md) | T-116, T-134, T-128, T-138, T-160, T-161, T-162, T-163, T-164, T-150, T-152, T-153, T-154, T-155, T-156, T-158 | deferred |
| [T-133 Internal Git service acceptance](T-133-git-service-acceptance.md) | T-117, T-132, T-138, T-139, T-152, T-153, T-157 | deferred |
| [T-165 Repository tooling without OpenForge](T-165-tooling-separation.md) | T-125 | done |
| [T-166 Kernel Git contracts and backlog revision](T-166-kernel-git-contracts.md) | — | done |
| [T-167 Kernel Git hosting, authority and receive events](T-167-kernel-git-hosting.md) | T-166 | done |
| [T-168 Push to pipeline and run dispatch](T-168-push-run-dispatch.md) | T-167 | done |
| [T-169 Kernel run Git sources and credentials](T-169-run-git-sources.md) | T-168 | done |
| [T-170 Kernel Git runnability and closeout](T-170-kernel-git-closeout.md) | T-169 | done |
| [T-135 Internal Git service: O-32/O-33 scope and executable task graph](T-135-git-scope-amendment.md) | T-125, T-165 | done |
| [T-136 Git authority: grants, credentials and worker identity](T-136-git-authority.md) | T-126 | done |
| [T-137 Native Git triggers and task routing](T-137-native-git-triggers.md) | T-130, T-148 | done |
| [T-138 Git maintenance, backup, restore and offboarding](T-138-git-operations.md) | T-128, T-129, T-130, T-160 | done |
| [T-139 Local development and test installation with native Git](T-139-local-git-installation.md) | T-132, T-165 | deferred |
| [T-147 Workspace repository associations and internal checkpoints](T-147-workspace-repositories.md) | T-127, T-130, T-131, T-136 | done |
| [T-148 Run origins and source sets in execution](T-148-execution-sources.md) | T-131, T-147 | done |
| [T-149 Source-set provenance and integration approval](T-149-source-provenance.md) | T-136, T-148 | done |
| [T-150 Runtime Git authority and result finalization](T-150-runtime-git-authority.md) | T-130, T-136, T-148, T-149 | done |
| [T-151 Workload programs for source sets and pushes](T-151-workload-git-programs.md) | T-131, T-136 | done |
| [T-152 Runtime adapters for source sets and Git credentials](T-152-runtime-git-adapters.md) | T-115, T-129, T-150, T-151 | deferred |
| [T-153 Agent wrapper and policy compatibility](T-153-agent-git-compatibility.md) | T-131, T-148 | done |
| [T-154 CLI for repositories and Git credentials](T-154-git-cli.md) | T-116, T-158 | deferred |
| [T-155 Product MCP tools](T-155-product-mcp.md) | T-127, T-136, T-148 | done |
| [T-156 Chart and operations for native Git](T-156-git-chart-operations.md) | T-116, T-129 | deferred |
| [T-157 Stage installation with native Git](T-157-stage-git-installation.md) | T-132, T-156 | deferred |
| [T-158 API aggregate and client for the Git service](T-158-git-api-aggregate.md) | T-116, T-127, T-130, T-136, T-137, T-147, T-148, T-149 | deferred |
| [T-159 Git worker: journal only applied updates; definite unreadable journal](T-159-git-worker-journal-accuracy.md) | T-128 | done |
| [T-160 Git worker object operations for maintenance and recovery](T-160-git-worker-object-operations.md) | T-159 | done |
| [T-161 Tenant-wide Git grant revocation](T-161-git-tenant-revocation.md) | T-136 | done |
| [T-162 Entrypoint restores source HEADs after publishing](T-162-entrypoint-head-restore.md) | T-151 | done |
| [T-163 Agent pins in shared (v2/v3) contexts](T-163-shared-context-agent-pins.md) | T-148, T-153 | done |
| [T-164 Entrypoint handles an already-integrated approval](T-164-entrypoint-already-integrated.md) | T-150, T-162 | done |
| [T-140 Container-first SDLC executable contracts](T-140-container-sdlc-contracts.md) | T-116, T-131 | deferred |
| [T-141 Artifact catalogue and immutable releases](T-141-artifact-release-catalog.md) | T-140 | deferred |
| [T-142 Container inputs, secrets and provenance](T-142-container-inputs-and-provenance.md) | T-141 | deferred |
| [T-143 Generic deployment and cleanup records](T-143-generic-delivery-records.md) | T-142 | deferred |
| [T-144 Publication verification and tool recipes](T-144-artifact-publication-recipes.md) | T-143 | deferred |
| [T-145 SDLC assembly and legacy surface removal](T-145-container-sdlc-assembly.md) | T-144, T-132 | deferred |
| [T-146 Real release/deploy/rollback acceptance](T-146-container-sdlc-acceptance.md) | T-145, T-117, T-133 | deferred |

## Verification and scope

`make docs-check` validates Markdown links, anchors, fenced JSON and shell examples; it does not read task metadata. `make validate-tasks` validates every task record's frontmatter (required keys, statuses, `check`/`acceptance` for `planned` and `blocked` tasks, decision blockers for `blocked` tasks), unique IDs, existing and acyclic dependencies, clean relative owned paths, exclusive ownership between active tasks that do not depend on each other (equal or nested paths), and that this index has a row for every `planned` or `blocked` task whose link, dependencies and status word match the frontmatter (`blocked (O-32)` is accepted). `done` and `deferred` tasks may be absent from the index: completed task records are pruned once merged and remain in Git history, and a retained record or row may keep historical metadata; scheduling deferred work requires bringing its check contract up to date. It does not require future owned paths or check scripts to exist. Do not claim a production, hosted-authority, live-provider or browser qualification from local unit/core-checkpoint evidence.
