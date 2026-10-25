# OpenForge autonomous task harness

OpenForge (`../openforge/` (a separate project)) is a project-agnostic Bun/TypeScript SDLC harness embedding `@opencode/sdk` **2.1.2**. All project-specific commands, environment variables, resource limits, and protected paths live in the root [`openforge.json`](../../openforge.json), `Makefile`, and `.opencode/agents/`.

## Commands

```sh
make openforge-install
bun ../openforge/src/cli.ts validate
bun ../openforge/src/cli.ts ready
bun ../openforge/src/cli.ts run --dry-run
bun ../openforge/src/cli.ts run --workers 5
bun ../openforge/src/cli.ts check-task T-080
bun ../openforge/src/cli.ts status --json
bun ../openforge/src/cli.ts batch
bun ../openforge/src/cli.ts batch --retry
bun ../openforge/src/cli.ts unpark T-080
bun ../openforge/src/cli.ts doctor
bun ../openforge/src/cli.ts telegram
```

## Task flow

```text
ready → (opt-in plan) → implement → check → review → (fix in writer session) → land
every N merges or at drain: mainCheck
```

1. **Claim & optional plan:** Ready tasks with disjoint `owns` run in parallel inside `.work/<slug>/`. By default, the implementer plans in its own session; setting `plan: true` in task frontmatter runs the read-only `planner` stage first.
2. **Implement & fix (shared writer session):** The `implementer` codes inside owned paths and submits `{ summary, tests_run, blockers }` via `openforge_submit_handoff`. When a check fails or the reviewer requests changes, the `fixer` continues the **same writer session** so no context is lost.
3. **Independent review:** The read-only `reviewer` inspects the candidate diff in a separate session (reused across follow-up review rounds) and submits `{ verdict, summary, findings }` via `openforge_submit_review`. Approval is bound to a byte-preserving SHA-256 diff fingerprint and a qualification-context digest (task contract, effective harness configuration, configured context files and reviewer instructions). The persisted `patchId` fields contain this fingerprint, rather than Git's whitespace-insensitive `patch-id`.
4. **Land:** A single publication lane rebases the task branch onto `base` (`main`). If the post-rebase fingerprint and qualification context are unchanged, approval stays valid; when `main` moved, the task's `check` command runs once on the rebased tree before squashing, fast-forwarding `main`, archiving `.harness/`, and removing the task worktree and branch. Changed context or legacy evidence without a context digest requires renewed checks and review; context changes first rebase the worktree so the reviewer sees current instructions.

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

Three generic gates govern every task:

1. **Scope gate:** Every changed file must fall under `owns`. Files listed in `openforge.json` `protected` (`go.mod`, `go.sum`) cannot be modified unless explicitly owned by the task.
2. **Check & acceptance gate:** The task's `check` command runs under the in-process resource semaphores configured in `openforge.json` (`resources`).
   - **Exit `0`:** Passes. When `acceptance` is non-empty, the harness passes `FORGE_RESULTS=<wt>/.harness/results.json` and verifies that every declared acceptance test name appears in `passed` (and not in `failed` or `skipped`). For Go tasks, `make check-go` (`dev/cmd/repoctl`) parses `go test -json` output and writes `FORGE_RESULTS`; a missing scope exits `75`.
   - **Exit `75` (`EX_TEMPFAIL`) or timeout (`124`):** Pauses the task for unavailable infrastructure without charging a repair attempt.
   - **Any other non-zero exit:** Sends the failure log tail to the `fixer` (up to `limits.repairs`).
3. **Review gate:** Requires an `approve` verdict with zero blocking findings (`p1`, `blocker`, `major`) bound to the candidate fingerprint and qualification context. Tasks with `review: human` park for human review after passing checks, including when that policy changes while a task is stopped.

## Periodic main check

After `mainCheckEvery` merges (default `5`) and at queue drain (`mainCheckOnDrain`), OpenForge runs `mainCheck` (`make lint-go`, `make test-go-unit`) on `main` and records the state in `.work/.harness/main.json`. A failing `mainCheck` blocks landing while allowing tasks to continue implementing, checking, and reviewing in parallel; repair `main` and run `bun ../openforge/src/cli.ts batch --retry`.

Before and after each command, the primary checkout must be clean on the configured base branch at the captured commit. Wrong-branch, dirty-tree, or commit-drift results block publication and cannot mark the captured commit green.

## Embedded runtime and reporting

OpenForge embeds `@opencode/sdk` **2.1.2** without an HTTP listener, storing sessions in `.work/.harness/opencode.db` under an exclusive owner lock. At startup it loads the primary checkout's `.env` without overriding variables already set and without printing values. Active API-key credentials for configured role providers are discovered from the local OpenCode service; an environment key such as `OPENCODE_API_KEY` supplies the `opencode` provider when that discovery has no active key, and an explicit environment key wins when both exist. MCP integrations and background shells are disabled inside automated sessions; `planner` and `reviewer` are read-only, and `implementer`/`fixer` are restricted from branch switching, rebasing, merging, or pushing. Agent shells do not inherit the orchestrator's environment wholesale: the embedded SDK replaces it with the session environment, so OpenForge passes `PATH`, `HOME`, locale, `XDG_*` and `GO*`/`CGO_*` variables plus the task variables, and any names or `PREFIX*` patterns listed in `openforge.json` `envPassthrough`. Credentials stay out of agent shells. Creating `.work/.harness/HALT` gracefully stops active runs.

Unexpected worker failures cancel peer work and wait for all active workers and the periodic check before releasing the queue lock.

## Orchestrating a factory

The orchestrator playbook (start and loop, triage of parked, paused and stalled tasks, prerequisite tasks, asynchronous decisions, throughput levers) is the repository skill [`orchestrate-factory`](../../.agents/skills/orchestrate-factory/SKILL.md).

## Telegram operator

`bun ../openforge/src/cli.ts telegram` (or `make openforge-telegram`) runs a private conversational operator beside the queue. It long-polls the Bot API, so the host does not need a public webhook. Configure it in the primary checkout `.env` or the process environment:

```sh
TELEGRAM_BOT_TOKEN=replace-with-botfather-token
TELEGRAM_OWNER_ID=123456789
TELEGRAM_CHAT_ID=          # optional; otherwise the first private /start from the owner binds the chat
OPENCODE_API_KEY=replace-with-provider-key
```

Only that numeric owner, in a private chat, can read factory state or invoke an action. Other chats are ignored or refused without factory details. `/status`, `/halt`, `/resume`, `/unpark`, `/retry-main`, and `/publish` work even when the model is unavailable. Free-text conversation uses a separate embedded session database at `.work/.harness/operator/opencode.db`, so it does not take the queue's runtime lock.

The operator can halt and resume the queue, unpark a task, retry the main check, and prepare a task or decision on an operation branch. Preparation validates the task graph. Publication fast-forwards a clean base checkout only after the owner presses Publish or sends `/publish <id>`. It does not count as a task landing and does not bypass a dirty checkout or an in-progress publication lock. Tokens and credential-named environment values are redacted from replies, and the operator cannot pass provider keys into task agent shells.

A launchd agent can keep the process up across login. Point `WorkingDirectory` at the primary checkout, invoke `bun ../openforge/src/cli.ts telegram`, and keep the token in `.env` rather than in the plist. Stop it with SIGTERM; that does not halt the queue. Create `.work/.harness/HALT`, or send `/halt`, to stop workers.
