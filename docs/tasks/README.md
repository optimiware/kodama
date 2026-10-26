# Development task graph

This backlog holds the **18 planned and deferred tasks**. Completed work is not listed here: its task records were removed once merged and remain in Git history (last present at commit `354b760`). Keep one Go module and preserve working packages. Production deployment and qualification, external adapters, the chassis and additional wrappers each need explicit scheduling before they start.

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

T-112's first candidate is held for material review repairs. [T-119](T-119-run-resource-event-contract.md) is reviewed, checked and merged at `b75dca74`, so T-112 can now repair against the complete event contract. The bounded T-112 migration-manifest exception preserves the existing PostgreSQL assembly without enabling shared runs. T-111 qualification continues independently; no passing P1 report has been recorded.

## Task contract

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

## Verification and scope

`make docs-check` validates links, metadata/index agreement, dependency cycles and exclusive ownership for all unordered pairs. Completed/deferred task records may retain historical metadata; scheduling deferred work requires bringing its check contract up to date. Do not claim a production, hosted-authority, live-provider or browser qualification from local unit/core-checkpoint evidence.
