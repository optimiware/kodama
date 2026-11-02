# Task workflow and the optional OpenForge harness

Kodama's development, checks and task validation need only Go, GNU Make and Git. No Bun, OpenForge checkout or sibling directory is required, and no Make target or CI workflow in this repository installs, lints or tests OpenForge.

OpenForge is a separate project (a Bun/TypeScript harness embedding `@opencode/sdk` **2.1.2**), developed and installed outside this repository, that can automate the workflow below. It is an **optional external integration**: you install it yourself, outside this repository, and you supply the launcher command. This repository contributes only its configuration: the root [`openforge.json`](../../openforge.json), the task records, `Makefile` targets and `.opencode/agents/`. Kodama does not guess an OpenForge package name, location or install step.

## Default workflow (no external harness)

```text
coordinator: worktree → implementer → check → independent review → (fix) → merge into the integration branch
every five merges and at the end: batch validation on the integration tip
publication to main: a separate explicit decision
```

1. **Coordinator.** A human or an agent session picks a ready task (`planned` with every dependency `done`; see the [task queue](../tasks/README.md)) and creates its worktree from the agreed integration branch, for example `git worktree add -b task/T-126-contracts .work/T-126-contracts integration/<name>` (or `main`). Tasks with disjoint `owns` may run in parallel; each has one writer per owned path.
2. **Implementer.** A subagent (or developer) works only in that worktree and only in the task's `owns` paths, writes tests for the invariants, runs the task's check and commits to the task branch. Protected files (`go.mod`, `go.sum`) change only when the task owns them. It never pushes, rebases, merges or touches another worktree, and returns a handoff of `{ summary, tests_run, blockers }`. Role definitions for the optional harness live in `.opencode/agents/`.
3. **Independent review.** A reviewer who did not write the change inspects the exact candidate diff, with the task contract and the instructions in `AGENTS.md`, and approves or lists blocking findings. Changes after approval need a renewed review.
4. **Checks.** The coordinator runs the declared check on the candidate: `make check-task TASK=T-NNN` (or the task's `check` command directly). It runs the `check` from the task record, requires every `acceptance` name to pass and distinguishes an unavailable environment (`75`) from a code failure. Run `make validate-tasks` when task records changed and `make docs-check` for documentation.
5. **Merge.** With review approval and passing checks recorded, the coordinator merges into the integration branch (squash or fast-forward as agreed), updates the task record status to `done` (and the dependency index) and removes the task worktree and branch as described in [AGENTS.md](../../AGENTS.md#git-workflow).
6. **Batch validation.** After every five merges and when the queue is drained, run `make lint-go test-go-unit` on the integration tip, plus the bounded M1/M2 suites as their checkpoints land (for example `make check-go-integration` with the package lists in `openforge.json` `mainCheck`). A failure pauses further merges until repaired; do not add a second full suite at publication.
7. **Publication.** Moving the integration branch to `main` is a separate explicit decision by the owner.

Failure handling: a code, compiler or assertion failure goes back to the writer. A missing environment (`75`) pauses the task without charging a repair; fix the environment (`make dev-up`, `make dev-preflight`) and rerun.

## Optional OpenForge integration

OpenForge reads `openforge.json` (task glob, index file, base branch, checks, resource limits, protected paths, role models) and `.opencode/agents/`. Its checks are the same commands as above, so a task passes or fails identically with or without it. Run it through the launcher you installed, shown here as `$OPENFORGE`:

```sh
$OPENFORGE validate        # its own task-graph validation, in addition to make validate-tasks
$OPENFORGE ready
$OPENFORGE run --dry-run
$OPENFORGE run --workers 5
$OPENFORGE check-task T-080
$OPENFORGE status --json
$OPENFORGE batch
$OPENFORGE batch --retry
$OPENFORGE unpark T-080
$OPENFORGE doctor
$OPENFORGE telegram
```

`make openforge-telegram OPENFORGE='<launcher>'` is the only OpenForge-specific Make target; it fails with an actionable message when `OPENFORGE` is unset. Install and verify OpenForge by following that project's own instructions: its dependencies and tests are not part of Kodama's checks.

When it drives the task flow, it adds the following (documented by that project; its behavior is not defined by this repository):

1. **Claim & optional plan.** Ready tasks with disjoint `owns` run in parallel inside `.work/<slug>/`; `plan: true` in task frontmatter runs a read-only `planner` stage first.
2. **Implement & fix.** The `implementer` submits `{ summary, tests_run, blockers }`; on a failed check or review the `fixer` continues the same writer session.
3. **Independent review.** The read-only `reviewer` approves a candidate bound to a diff fingerprint and a qualification-context digest; `review: human` parks a task for human review.
4. **Land.** A single publication lane rebases onto `base`, re-checks if the base moved, squashes, fast-forwards and cleans up. Configure `base` in `openforge.json` for your integration branch; publication to `main` remains your explicit decision.

## Scoped checks and capacity

Tasks declare their check command, acceptance test names, and named concurrency resources in frontmatter:

```yaml
id: T-080
milestone: M1
status: planned
depends_on: [T-020, T-030, T-034, T-062, T-076]
decision_blockers: []
owns: [internal/execution/]
check: make check-go PKGS=./internal/execution/...
resources: [go]
acceptance: [unit_dispatch_replay, unit_cancel_fencing, unit_admission_pins]
```

Three generic gates govern every task, with or without a harness:

1. **Scope gate:** Every changed file must fall under `owns`. Files in `openforge.json` `protected` (`go.mod`, `go.sum`) cannot be modified unless explicitly owned by the task.
2. **Check & acceptance gate:** The task's `check` command runs with `FORGE_RESULTS` pointing at a results file (`make check-task` does this locally).
   - **Exit `0`:** Passes. When `acceptance` is non-empty, every declared name must appear in `passed` and not in `failed` or `skipped`. For Go tasks `make check-go` (`dev/cmd/repoctl`) parses `go test -json` output and writes the results; a missing scope exits `75`.
   - **Exit `75` (`EX_TEMPFAIL`), or an `infra_pause` record in the results file (even when a wrapper such as `make` masks the exit status, or exits `0`):** the infrastructure is unavailable. `make check-task` reports "infrastructure unavailable" and `repoctl check-task` exits `75`. This is not a code failure. The optional OpenForge harness additionally treats a check timeout (`124`) this way; `make check-task` has no timeout and treats any other exit as a code failure.
   - **Any other non-zero exit:** a code failure for the writer to fix.
3. **Review gate:** An independent approval with zero blocking findings (`p1`, `blocker`, `major`) on the exact candidate. Tasks with `review: human` need a human approval.

`resources` (`go`, `integration`, ...) document what a check consumes. `make check-task` does not lease them: serialize shared integration scopes yourself (a distinct `DEV_SCOPE` per session, `flock` for a shared cluster) or let the optional harness lease them using `openforge.json` `resources`.

## Periodic main check

`openforge.json` records the batch commands as `mainCheck` (`make lint-go`, `make test-go-unit`, then the bounded integration suites), `mainCheckEvery` (`5`) and `mainCheckOnDrain`. The default workflow runs the same commands by hand on the integration tip; the harness runs them on its configured `base` and records the state in `.work/.harness/main.json`, blocking landing (not implementation or review) while they fail. Repair the cause and rerun the batch (`$OPENFORGE batch --retry`).

Before and after each batch the checkout must be clean on the expected branch at the captured commit; a wrong branch, dirty tree or commit drift cannot mark the captured commit green.

## Embedded runtime and reporting (OpenForge)

When used, OpenForge embeds `@opencode/sdk` **2.1.2** without an HTTP listener, storing sessions in `.work/.harness/opencode.db` under an exclusive owner lock. At startup it loads the primary checkout's `.env` without overriding variables already set and without printing values. Active API-key credentials for configured role providers are discovered from the local OpenCode service; an environment key such as `OPENCODE_API_KEY` supplies the `opencode` provider when that discovery has no active key, and an explicit environment key wins when both exist. MCP integrations and background shells are disabled inside automated sessions; `planner` and `reviewer` are read-only, and `implementer`/`fixer` are restricted from branch switching, rebasing, merging, or pushing. Agent shells do not inherit the orchestrator's environment wholesale: the embedded SDK replaces it with the session environment, so OpenForge passes `PATH`, `HOME`, locale, `XDG_*` and `GO*`/`CGO_*` variables plus the task variables, and any names or `PREFIX*` patterns listed in `openforge.json` `envPassthrough`. Credentials stay out of agent shells. Creating `.work/.harness/HALT` gracefully stops active runs.

Unexpected worker failures cancel peer work and wait for all active workers and the periodic check before releasing the queue lock.

## Orchestrating a factory

The orchestrator playbook for the optional OpenForge harness (start and loop, triage of parked, paused and stalled tasks, prerequisite tasks, asynchronous decisions, throughput levers) is the repository skill [`orchestrate-factory`](../../.agents/skills/orchestrate-factory/SKILL.md).

## Telegram operator

`$OPENFORGE telegram` (or `make openforge-telegram OPENFORGE=...`) runs a private conversational operator beside the queue. It long-polls the Bot API, so the host does not need a public webhook. Configure it in the primary checkout `.env` or the process environment:

```sh
TELEGRAM_BOT_TOKEN=replace-with-botfather-token
TELEGRAM_OWNER_ID=123456789
TELEGRAM_CHAT_ID=          # optional; otherwise the first private /start from the owner binds the chat
OPENCODE_API_KEY=replace-with-provider-key
```

Only that numeric owner, in a private chat, can read factory state or invoke an action. Other chats are ignored or refused without factory details. `/status`, `/halt`, `/resume`, `/unpark`, `/retry-main`, and `/publish` work even when the model is unavailable. Free-text conversation uses a separate embedded session database at `.work/.harness/operator/opencode.db`, so it does not take the queue's runtime lock.

The operator can halt and resume the queue, unpark a task, retry the main check, and prepare a task or decision on an operation branch. Preparation validates the task graph. Publication fast-forwards a clean base checkout only after the owner presses Publish or sends `/publish <id>`. It does not count as a task landing and does not bypass a dirty checkout or an in-progress publication lock. Tokens and credential-named environment values are redacted from replies, and the operator cannot pass provider keys into task agent shells.

A launchd agent can keep the process up across login. Point `WorkingDirectory` at the primary checkout, invoke your `$OPENFORGE telegram` launcher, and keep the token in `.env` rather than in the plist. Stop it with SIGTERM; that does not halt the queue. Create `.work/.harness/HALT`, or send `/halt`, to stop workers.
