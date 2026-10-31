# Pipeline runtime: handoff after P2

Entry point for whoever continues after the P2 integration branch is published. It records state and next steps and links to the owning documents; it does not restate the [simplification plan](pipeline-runtime-simplification.md). Delivery decisions are [O-39](../decisions.md#pipeline-runtime-delivery-decisions--2026-10-31-o-39).

## Where the work stands

The kernel and the shell capability packages exist and are tested, but the new engine is not composed. Four P2 changes already run inside the transitional `kernel` assembly (`internal/shell/serve`, `internal/shell/api`): the outbox publisher and NATS bus (T-188), run-derived Git integration authority (T-187), the dispatcher's durable name (T-190) and `sources.NewGit` checkout for the old runner (T-191). Everything else is unused by an executable: the assembly still runs the old blocking `runner.Runner`, `runner.Engine` is exercised only by tests, and the run-state adapters, pipeline registry, `sources.Preparer` and secret custody have no caller outside tests. P3 is the cutover.

- **Branch.** `task/adr13-pipeline-runtime` (worktree `.work/adr13-pipeline-runtime`), 166 commits ahead of `main` at `f56c679f`, with the per-task merge commits kept. `main` and `origin/main` are untouched by this work.
- **Delivered, by task** (records in [docs/tasks](../tasks/README.md)):
  - Contract and boundary: T-171 (runtime ports), T-172 (frozen decisions), T-173 (shell relocation), T-180 (traceability), T-181 (legacy fixture repair).
  - Kernel: T-174 (memory state and contract suite), T-175 (durable engine), T-186 (outputs, claim release); backends T-176 Process, T-177 Podman, T-178 Kubernetes.
  - Schema (migrations 004–007): T-179, T-185, T-189, T-192.
  - Shell: T-182 (PostgreSQL state), T-183 (pipeline registry), T-187 (integration authority from the run), T-188 (outbox and NATS), T-190 (dispatcher durable name), T-191 (sources and secret custody), T-193 (outbox cursor guarantee).
- **Not delivered.** [T-184](../tasks/T-184-task-tracking.md) (task tracking and projection, P2.2) was still `planned` when this was written, and T-194 depends on it. No task record covers the P2.6 transport adapters (task API, `kodactl`, native MCP over the task use cases); `internal/shell/api/tasks.go` is still the pre-existing workflow-based route. The plan assigns transport adapter implementation and their package tests to [P2.6](pipeline-runtime-simplification.md#p26-api-cli-and-mcp-adapters), and O-39 finishes P2 before publication; it does not defer them. Publication is therefore conditional on completing P2.6 (task API, `kodactl` and native MCP adapters over the new task use cases) or on an explicit owner-approved scope amendment recorded in the decision register. P3 route registration is not a substitute: P3 registers adapters that P2 already implemented. T-184 and [T-195](../tasks/T-195-admission-task-link.md) (below) must be merged first.
- **Task linkage.** Nothing in the product persists `runs.task_id`: admission does not carry the task, the PostgreSQL run insert omits the column and the runtime role cannot update it. `tasks.current_run_id` is constrained to the task's own run, so task Start and Rerun cannot succeed against the real stores until T-195 merges; T-184's PostgreSQL projection evidence links the run as the table owner. The P2 task journey is not demonstrated until T-195 shows the link from a non-owner role.
- **Evidence.** Each task recorded its own check. T-182 additionally ran its 127 tests against real PostgreSQL 17.11 with a non-owner role (0 skipped). `make lint-go test-go-unit` exited 0 (no failing package) on the T-194 worktree, whose Go code is that of integration tip `f56c679f`; it ran without services, so it proves no PostgreSQL or NATS behaviour. T-184 will move the tip, so the coordinator reruns the batch on the final publication tip. Kubernetes backend behaviour is proven against a fake client only (T-178); real conformance is a P3 gate.

## P3 — composition and cutover

Plan: [P3](pipeline-runtime-simplification.md#p3--composition-and-interfaces). O-39 requires one reviewed change that switches everything at once, with the full acceptance suite and a documented rollback.

- **One composition** in `internal/app` replacing `--assembly core` and `--assembly kernel`; adapters chosen by `KODAMA_ENV`; `production` keeps refusing development and test adapters.
- **Backend convergence:** run the shared kernel lifecycle contract against real Process, Podman and Kubernetes; T-134 (Podman/Process run-resource wiring) folds in here. T-117 stays parked.
- **Workload identity, callbacks and logs:** port only the current contract; drop `/runtime/v1`–`v3` routes no client uses.
- **Served interfaces:** publish only served routes in `api/openapi.yaml`, regenerate `api/client`, register CLI and MCP commands, and pass `make api-check`.
- **Deployment and developer workflow:** one chart and settings document, `dev/` scopes, Make and CI targets.
- **Exit gate:** a task runs select → start → steps → wait → authorized signal → continuation on real adapters; Git-triggered and task-free runs work; restart, cancellation, cleanup and broker outage are exercised; the product refuses an incomplete environment.

## P4 — deletion

Plan: [P4](pipeline-runtime-simplification.md#p4--deletion-of-the-superseded-implementation). O-39: the superseded implementation is deleted immediately after cutover.

- Write the **retirement inventory** first: component, replacement, migrated consumers, regression evidence, deletion owner.
- Delete in **dependency order**, not phase order: workflow code, legacy execution and runtime contract versions, gate/delivery engines, legacy work/workspace/metrics/QA, composition wiring, then API fragments and fixtures. `go.mod`/`go.sum` last.
- **Preserve before deleting:** port lifecycle and recovery regressions, RLS and authority tests, Git worker helpers and secret envelope helpers that survive.
- Schema contraction is the schema owner's final slice (see gaps).
- **Exit gate:** [P4.5](pipeline-runtime-simplification.md#p45-exit-gate): no production import reaches a deleted engine, the kernel import check passes, the generated API and client carry no retired operation, and scoped checks pass per deletion task. Separately, record the before/after counts in the plan's [completion report](pipeline-runtime-simplification.md#completion-report).

## P5 — externalization

Plan: [P5](pipeline-runtime-simplification.md#p5--externalization-and-layer-completion). O-39 defers it to a follow-up workstream.

- **Layer 3 adaptation:** agents, AI gateway and external MCP proxy onto the shell contracts and waits. It gates deletion of the legacy packages those consumers import, so P4 must not delete them before it is done or consciously carved out.
- **Layer 4 recipe set:** SDLC pipeline, rework, promotion wait, build/publish, deploy/rollback, notification, hooks. Examples may be drafted early; execution claims need integrated evidence.
- **Exit gate:** [P5.6](pipeline-runtime-simplification.md#p56-exit-gate): layer import rules hold, a plain pipeline runs with Layer 3 absent, an agentic pipeline uses the same container and wait contracts, delivery, QA and notification behaviour live in recipes rather than core engines, and status matches the implemented surface.

## First three P3 tasks to create

Proposed ids are not assigned; paths follow the plan's [workstream table](pipeline-runtime-simplification.md#workstream-ownership).

1. **Production source and secret seams** (owns `internal/shell/sources/`, `internal/shell/secrets/`, `internal/shell/serve/runsources.go`; needs T-191, T-187). Implement the production `Keys`, `Tips` and vault `Source`. Without them the engine cannot prepare a run or resolve a declared secret outside tests, so the next task has no working inputs.
2. **Engine host over real adapters** (owns a new `internal/shell/engine/`, plus `internal/shell/runstate/postgres/` for the legacy-row filter below; needs 1, T-175, T-182, T-188, T-186). Build the worker that runs `runner.Engine` over PostgreSQL state, one selected backend, the preparer, the resolver and the outbox publisher, with restart and adoption evidence against real PostgreSQL and the Process backend. This is the first time the kernel runs outside a unit test, and it must exist before anything can be cut over.
3. **Single composition skeleton** (owns `internal/app/`, `cmd/kodama/`; needs 2 and T-184). Compose host, shell, listener and workers behind one constructor with explicit startup order and fail-closed dependencies, still selectable beside the old assemblies until the switch. Composition has one ordered owner, so this task fixes the paths the interface, deployment and acceptance slices then share.

Workload identity (P3.3) and backend qualification need only task 2 or the contracts, and can start in parallel once these exist. The P2.6 transport adapters are not P3 work: they are a P2 deliverable (see the publication checklist) and need only T-184.

## Known gaps and deferred items

- **No production `Tips`, `Keys` or vault `Source`.** `sources.Preparer` requires `Keys` and `Tips`, and `secrets` requires a `Source`; their only implementations are test doubles. `sources.CredentialSource` serves run Git credentials and delegates every other reference to its `Next` source, which has no production implementation. First P3 task above.
- **`Runnable` lists legacy-composition runs.** The PostgreSQL `Runnable` query filters on terminality and lease only, over the shared `runs` table, so a non-terminal run created by the old execution engine is listed. Harmless while the engine is not wired; P3 must filter or drain those rows before the engine host claims work.
- **Dispatcher cursor reset.** T-190 moved the dispatcher from `kernel-git-dispatch` to `kodama_shell_git-dispatch`. A durable consumer is keyed by name, so a deployment that had the old one replays from the start once; the work is idempotent (`store.Dispatched`). The retired consumer is left unused in JetStream.
- **Outbox guarantees.** The `audit_events_unpublished` index exists (migration 006). A late-committing event is covered by the next completed reconciliation pass, not by a time bound; delivery is at-least-once with consumer deduplication (T-193).
- **Legacy schema contraction waits for cutover.** `projects.task_workflow` and the task columns `state`, `parked_at`, `resume_state`, `blocked_by_id`, `resolution`, `resolved_at` are still present: migrations 004–007 do not drop them ([schema notes](database-schema.md)). O-39: drop them only after the single composition is live and executions are drained or settled; map existing tasks to pipelines explicitly or refuse.
- **Frozen initial values (O-39).** Wait timeout 1 minute to 30 days; at most 8 outcomes; 4 KiB answers and outputs; `on_expiry` defaults to `fail`, `join` to `all`; `approving` optional unless a protected step requires the wait. Defined in the [pipeline format](../contracts/pipeline-format.md). Changing them is a contract change, not an implementation choice.
- **Task-run linkage (T-195).** `runs.task_id` is never written, so `tasks.current_run_id` cannot be set by the runtime role; see [where the work stands](#where-the-work-stands). Open until T-195 merges with non-owner PostgreSQL evidence.
- **Signal records.** Cancelled-decision records written before migration 007 have no stored reason and fail closed on replay; they are never reconstructed.
- **Unqualified:** Kubernetes backend conformance (T-178 used a fake client) and the new engine on a stage. The plan's [P1 evidence note](pipeline-runtime-simplification.md#p17-exit-gate) applies: a real controller-crash restart is a P3 qualification.

## Publication checklist

Publication is a separate, explicit owner decision ([O-39](../decisions.md#pipeline-runtime-delivery-decisions--2026-10-31-o-39)); it is not implied by finishing P2.

1. T-184, [T-195](../tasks/T-195-admission-task-link.md) and T-194 merged; task records and the [task index](../tasks/README.md) closed; `make validate-tasks docs-check` clean. T-195's non-owner PostgreSQL admission and linkage evidence (`Test_unit_runstate_postgres_task_link` plus the real-database run) is recorded; until then the P2 task journey (select, start, rerun, project) must not be described as working against real stores.
2. P2.6 resolved: the task API, `kodactl` and native MCP adapters over the new task use cases are implemented and package-tested, or the owner has approved and recorded a scope amendment that moves them. Do not treat P3 route registration as deferral of the adapter implementation.
3. Branch shape: fast-forward `main` to the integration tip, keeping the per-task merge commits and review history. No squash, no rewrite.
4. Batch evidence recorded for the exact tip: `make lint-go test-go-unit`. Add no second suite at publication.
5. The owner approves publication, including **pushing `main` to `origin`**, which is part of the same decision.
6. The image workflow tags the release; make no manual version bump.
7. After publication, P3 starts from a new integration branch off the new `main`; remove the finished integration worktree and branch with `git branch -d`.
