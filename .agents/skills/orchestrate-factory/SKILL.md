---
name: orchestrate-factory
description: Run an OpenForge task factory as its orchestrator. Start and monitor the harness, triage parked, paused and stalled tasks, create prerequisite tasks, record asynchronous owner decisions, keep main green and raise throughput. Use whenever you are asked to drive a task queue to completion, restart or unblock the harness, or report factory progress.
---

# Orchestrating a task factory

An OpenForge factory (`../openforge/` (a separate project), configured by the root `openforge.json`) turns task files in `docs/tasks/` into merged commits: implement, check, independent review, fix, land. You are the **orchestrator**. The workers do the coding; you keep the queue moving, repair what the harness cannot repair alone, and keep the owner informed without ever waiting on them.

Read first: `docs/implementation/harness.md` (flow and gates), `docs/tasks/README.md` (ownership and index rules), `docs/decisions.md` (open and resolved decisions), `AGENTS.md`.

## Rules of engagement

1. **Never block on a question.** The owner answers asynchronously. When a task needs a human decision, record an **Open** row in `docs/decisions.md` (options, a recommendation, who it blocks), mark the task `status: blocked` with `decision_blockers: [O-xx]`, and keep every unrelated task running. Decide yourself only what is engineering, not scope, priority or product. If you do decide under standing authority, record it as resolved-by-orchestrator so the owner can override.
2. **Keep your own footprint small.** You may edit task metadata (`owns`, `depends_on`, `acceptance` notes), the index, `docs/decisions.md`, harness code and prompts, and tiny prerequisite fixes. Product code belongs to tasks: if a defect is outside every running task's owned paths, create a prerequisite task rather than patching it yourself.
3. **Commit orchestrator edits immediately, in one step, and never while a main check is running.** Landing refuses a dirty primary checkout ("Primary checkout must be clean on main"), and a commit during a main check fails it with "checkout changed while checking". Write the files, validate, commit in a single command. Check `ps` for `make lint-go|test-go-unit|check-go-integration` first.
4. **Secrets stay in the process.** OpenForge loads the primary checkout `.env` at startup and does not override variables already set. Existing `source .env` still works. Never print values, never write them to a file or a prompt. `OPENCODE_API_KEY` configures the `opencode` provider and is not passed to task agent shells.
5. **Only remove what the repository owns.** Tear down dev scopes recorded in `dev/.state/environments/` with `make dev-down DEV_SCOPE=...`, and orphans whose backing scope you just removed. Leave anything you cannot tie to this repository.
6. **Do not edit a worktree that has a running agent.** You may rebase, edit `.harness/state.json` and add commits only to a **parked** task's worktree.

## Start and loop

```sh
make openforge-install
bun ../openforge/src/cli.ts doctor && bun ../openforge/src/cli.ts validate
bun ../openforge/src/cli.ts ready
# start (background; keep the log somewhere writable)
set -a; source .env; set +a
bun ../openforge/src/cli.ts run --workers 8 > /tmp/<name>/openforge-run.log 2>&1
```

Then loop: wait a fixed interval (a backgrounded `sleep 295` that notifies you), run the report, act, repeat. Do not busy-poll.

Check progress with the OpenForge project's own reports (see its README in `../openforge`) and the run log. The report scripts that used to live here moved with OpenForge. Judge a task by its last shell command: a task that ran a command within a few minutes is working, however long it has been in one phase (implement runs of 60 to 110 minutes are normal for acceptance suites; the agent timeout is `agentTimeoutSeconds`).

Stop gracefully with `touch .work/.harness/HALT`; the process exits when active agents yield (usually 1 to 3 minutes). Remove the file before restarting. **A restart is how new harness code, prompts and config take effect**, and it is also how paused tasks are retried. Agents resume their sessions; their worktrees persist.

The owner can do the same supervision from a private Telegram chat with `make openforge-telegram`. That process stays up when the queue drains. It records prepared task and decision changes only after the owner confirms publication; do not wait on that chat inside a worker.

## Reading the state

- `status --json` of the CLI is large; parse it, never print it.
- `.work/.harness/events.jsonl`: `merged`, `parked`, `unparked`, `agent_finished` with durations. This is the source for metrics.
- `.work/.harness/parked.json`: the full park reasons. `.work/.harness/main.json`: main-check state. `<worktree>/.harness/state.json`: per-task phase (`planned` → `implemented` → `verified` → `approved`), `candidateCommit`, `reviewRounds`, `repairReason`, `openFindings`.
- The run **exits on its own** when no task is active (all remaining are parked, blocked or waiting on parked work). A drain triggers a final main check, then the process ends: restart it after you unblock something.

## Triage playbook

| Symptom | Cause | Action |
| --- | --- | --- |
| `Review not approved after N rounds` with concrete findings in owned code | A real defect the fixer has not closed | `unpark TASK`. It resets rounds, repairs, findings and `repairReason`; the fixer continues in the same session. Repeat only while the findings change round to round. |
| Same, but the findings need changes outside the task's `owns`, or work another task owns | Missing prerequisite | Create a bounded prerequisite task (below); make the parked task depend on it; after it merges, rebase the worktree, advance state, unpark. |
| Reviewer keeps finding deeper edge cases each round | Review regress | Scope the reviewer: block only on defects in owned paths and the contract; hardening and out-of-scope items are `minor` follow-ups naming the owner. Raise `limits.reviewRounds` modestly. |
| `Implementer/Fixer blockers: ...` that are only caveats | Agent put a caveat in `blockers` | Work is already committed. In the task worktree set `state.json` `phase` to `implemented` and `candidateCommit` to `git rev-parse HEAD`, then `unpark`. Prompts now say caveats belong in `summary`. |
| Blockers that name a real product defect outside owned paths | Missing production join | Same as the prerequisite row. |
| `Primary checkout must be clean on main` | You (or a stray file) left `main` dirty at landing time | Commit or remove the change, then `unpark`. |
| `PAUSED: Development environment failed ... missing dev scope` (exit 75) | Integration check needs a scope | `check-go-integration` now provisions a missing scope (`DEV_STACK`, default `cluster`). If it still pauses, provision it in the worktree (`make dev-up DEV_SCOPE=<id> DEV_PROVIDER=podman DEV_STACK=...`) and restart: paused tasks are not retried during the same run. |
| Check fails with readiness timeouts or "kernel key quota nearly spent" | Host resources starved by stale containers | `cat /proc/key-users` for your uid near 200/200 is the sign. Tear down finished scopes and orphan proxies (rule 5), then let the fixer or the next check retry. |
| A fixer round runs for hours and the reason was `check failed`, not review findings | The fixer repairs **any** writer-session problem, and failed checks are the usual one. Each verification is a full integration run of 8 to 15 minutes, so ten iterations is two hours | Read `<worktree>/.harness/check.log` for the failing tests, then the fixer's own logs and `stuck`. Ask what the gate runs that the fixer does not: stack, scope, timeout, env. |
| Gate fails scenarios that pass for the fixer (e.g. "needs the `cluster` stack ... kubeconfig is missing") | The task's `check:` has no `DEV_STACK`, and an **existing scope of another stack shadows auto-provisioning** (`host-env.json` already exists; `up` keeps the recorded stack) | Put the stack in the task's `check:` (`DEV_STACK=cluster make check-go-integration ...`). To switch a scope: confirm nothing is using it (below), `make dev-down`, remove the state dir if it lingers, `dev-up` with the right stack. `cluster` is a superset of `services`. |
| Integration packages die with `panic: test timed out after 10m0s` | Go's default test timeout on a loaded host | `make check-go-integration` passes `-timeout 30m` (`KODAMA_GO_TEST_TIMEOUT`). Agents' own integration runs are outside the harness resource semaphore, so several tasks contend; expect slowdowns. |
| Parked implementer says a small repair is outside its `owns` and the owner already scheduled the defect | Ownership gap | Extend `owns` with only the files named (a few lines), say why in the task file, validate, commit, `unpark`. A larger or unscheduled gap is a prerequisite task or a decision. |
| Main check failed (`blocked: true`) | Real regression, a flake, or a commit during the check | Read `.work/.harness/main-check.log`. Rerun the failing test alone and under contention. If it is a real defect (a rare one is still a defect), fix the root cause on `main` with a regression test; then `bun ../openforge/src/cli.ts batch --retry`. "checkout changed while checking" only needs the retry. Landing is gated while blocked; implementing and reviewing continue. |
| Process exited, no `HALT` file | Drain, not a crash | `status`: if everything left is parked or blocked, fix that, then restart. |
| A task is `verified` or `implemented` for a long time with a recent shell | Long check or fixer | Wait. Integration checks run minutes; implement runs hours. |
| Unparked task lost the guidance you wrote into `state.json` | `unpark` bumps the generation and `init()` clears findings and repair fields | Put guidance in the task file's contract text (the prompt includes it), or edit `state.json` only after the harness has re-initialised the task. |
| Task branch carries a duplicate of a commit you made on `main` | `main` was rebased or the commit rewritten | `git -C <worktree> rebase main` (duplicates drop out) while the task is parked. |

## Creating a prerequisite task

Use this when a parked task reports a defect or missing piece outside its owned paths.

1. Next free id: `ls docs/tasks | sed -E 's/T-([0-9]+).*/\1/' | sort -n | tail -1`.
2. File `docs/tasks/T-NNN-slug.md` with frontmatter `id, milestone, status: planned, depends_on, decision_blockers: [], owns, check, resources, acceptance` and sections Goal, Contract, Deliverables, Acceptance, Out of scope. Copy the exact failing evidence and, when the reporter verified a repair, the diff. `acceptance` names are the Go test names the check must report as passed (`Test_<name>` normalises to the name); say so in the Acceptance paragraph.
3. Choose `owns` narrowly and **disjoint from every running task**, or the scheduler will serialize them. Prefer new code in its own package and leave the binding in a shared assembly package to one final task.
4. Add the new id to the blocked task's `depends_on`.
5. `bun ../openforge/src/cli.ts index --write`, then `validate` and `make docs-check`, then commit in one step. The running harness reloads task files each iteration and starts the task within seconds.
6. When it merges: `git -C <worktree> rebase main`, set the parked task's state to `implemented` with the new `candidateCommit` (or leave it `planned` if it must re-implement), add a short "prerequisites landed" note to its task file, and `unpark`.

## Recording an asynchronous decision

1. Add a row to the table in `docs/decisions.md` with the next free `O-NN`: `**Open, owner decision requested (<task> blocked)**`, what was found, options A/B/C with a recommendation, and what it blocks.
2. On the task: `status: blocked`, `decision_blockers: [O-NN]`. Run `index --write` and `validate`. Commit.
3. When answered: apply the outcome (task text, `owns`, acceptance), set the task back to `planned`, clear `decision_blockers`, rewrite the row as Resolved, `unpark`.
4. Do not ask the owner interactively unless they asked you to. Report open decisions in your progress summaries.

## Raising throughput

- **Find the critical path** with `progress`: open tasks and what each waits on. Most idle workers mean a thin runnable set, not a slow harness.
- **Shared-file ownership serializes tasks.** Two running tasks may not have overlapping `owns`. Split work so adapters live in separate packages and run in parallel, and keep the single shared-assembly edit for one last task.
- **Convert findings into tasks fast.** A parked task that names missing pieces is a work order: write the prerequisites immediately so idle workers start them.
- **Make the harness remove your latency.** Auto-provisioning, the scheduler's periodic wake (unparks take effect in seconds), prompts that keep caveats out of `blockers`, and scope-aware reviews each removed a recurring manual step. When you do the same manual step twice, automate it.
- **Do not leave a task parked longer than needed**: check every few minutes and unpark real-finding parks at once.
- **Deferred work is the owner's call.** If independent deferred tasks could use idle workers, record a decision instead of promoting them.
- **Measure** with `metrics --hours N`: merges, parks and unparks, agent minutes by role. Report merges, interventions, open decisions and what is left on the critical path.

## Pitfalls seen in practice

- **Tearing down a scope before checking what is running on it.** `make dev-down` on a scope under a running test kills its database mid-run (this happened). First list the test processes and read their scope: `tr '\0' '\n' < /proc/<pid>/environ | grep DEV_`. A fixer's helper scripts may use a different scope than the harness gate; do not assume.
- Editing `state.json` of a task that is running; do it only while parked.
- Committing to `main` during a main check, or leaving `main` dirty during landing.
- Assuming a long run is stuck. Check `stuck` for a recent shell command first.
- Treating a rare test failure as a flake without trying to reproduce it under contention. One such failure was a real chunking bug (the mirror flushed a partial line when pending data exceeded a limit that was meant to bound only the unterminated tail).
- Provisioning scopes by hand and having an agent tear them down. Tell agents the orchestrator owns scopes (the prompts say so).
- Forgetting that code, prompt and config changes need a harness restart, and that the run may already have exited.
- Letting stale scopes and port-forward containers from earlier runs exhaust kernel keys.
- Verbose output: never print the full `status --json`, event logs or agent transcripts.

## Report format

When asked for progress give: tasks merged in the window and in total, completion per milestone and overall (also excluding deferred), what is running, parked or blocked and why, open decisions with their ids, interventions you made, and a rough time to finish with its assumption.
