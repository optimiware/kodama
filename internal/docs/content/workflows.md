# Kodema Workflows

## 1. Planned approval gates (same execution, preserved workspace)

Use a `request` action in pipeline YAML when human or service sign-off is part of the planned flow:

1. An earlier container action builds, tests, pushes a candidate branch/commit, and writes outputs (for example `/kodema/outputs/implement/candidate`).
2. The `request` action (`request: role/reviewer` or `request: role/operator` or `request: service/<id>`) resolves its immutable `subject`, revokes the execution token, keeps `/workspace`, and appears in the role inbox.
3. A responder inspects the inbox and settles the request:
   ```sh
   kodema inbox
   kodema approve <execution-id> [--candidate <sha>] [--comment "..."] [--output key=val]
   # or:
   kodema reject <execution-id> --reason "..."
   ```
   When a verified `git.ref.updated` event moves the target ref past the request's `expected_base` while the request is pending, `kodema inbox` flags the item as `STALE` with the superseding commit and reason. `kodema approve` refuses stale requests by default (pass `--allow-stale` to override explicitly, or `--candidate <sha>` to confirm the exact candidate commit being approved).
4. Passing advances the **same execution** and reuses `/workspace` with a fresh execution token; the next action can now fast-forward an approval-guarded Git ref matching that exact subject (or a clean disjoint rebase when the repo enables `option approve-rebase.<ref> = disjoint`). Rejecting or timing out fails the execution.

## 2. Unplanned conversation handoffs (fresh execution)

Use `task.ask` / `task.reply` when an agent needs clarification or a human decision that wasn't planned as a pipeline gate:

1. **Persist context durably first** — push a commit to a work branch, publish an artifact digest, or write an external checkpoint. Never reference disposable `/workspace` paths or process-local session IDs because `/workspace` is discarded on ask.
2. **Ask on the task:**
   ```sh
   kodema task ask --question "Should we migrate existing rows or backfill lazily?" \
     --to role/engineer \
     --ref "commit=$(git rev-parse HEAD)"
   ```
   This atomically appends the question, marks the task `waiting`, cancels the current execution, and revokes its credential.
3. **Human/service replies:**
   ```sh
   kodema inbox --kind waiting_task
   kodema task reply <task-id> --answer "Backfill lazily in a separate job."
   ```
   Replying atomically appends the answer, reopens the task, and queues a **fresh execution** starting at action 1 with the updated conversation in `/kodema/task.json`.
4. **Restore in the fresh execution:** the first action reads `/kodema/task.json`, checks out the referenced commit/digest, and continues. Missing referenced context should fail visibly.

## 3. Delivery, artifacts, and promotion

- Artifacts are digest-pinned output strings (`registry.example.com/org/app@sha256:...`), not Kodema database entities.
- Build publishes image bytes to the external registry, then submits the immutable digest as a declared output.
- Test consumes `{from: build.outputs.image}` (read from `/kodema/outputs/build/image`) without rebuilding.
- Promotion updates `<tenant>/<project>/deploy` on a work branch, requests `role/operator` approval for that exact `git.ref.update`, and fast-forwards the protected ref after approval.
- Rollback is a new promotion execution targeting an older digest.

## 4. Multi-stage tasks, metadata, and clean restart

Keep work tied to one durable task across multiple pipelines (for example `implement` -> `integrate` -> `release`):

- **Attach string metadata at creation:** `kodema task run --title "..." --pipeline implement --meta ticket=ENG-42 --meta priority=high` (or `--metadata k=v`). Metadata is immutable per execution and materialized at `/kodema/task/metadata.json` and `/kodema/task/spec.json`.
- **Chain the next pipeline stage atomically:** once stage 1 succeeds (even when the task transitioned to `done`), switch pipelines and pass outputs forward on the same task:
  ```sh
  kodema task start <task-id> \
    --pipeline integrate \
    --input-from-output branch=implement.branch \
    --input-from-output candidate=implement.candidate
  ```
- **Supersede a running or waiting execution cleanly:** when a branch is updated or a pending approval is stale, atomically cancel the active execution (marked `superseded`) and start a fresh attempt:
  ```sh
  kodema task restart <task-id> \
    [--pipeline <name|id>] \
    [--input k=v] \
    [--input-from-output k=action.output] \
    [--reason "rebased onto latest main"]
  ```

## 5. Live monitoring, inbox waiting, and multi-action logs

- **Stream project events:** `kodema events [--project <id>] [--type <type>...] [--since <cursor>] [--limit <n>] [--follow] [--output table|json]` reads committed events or live-streams decoded project events over `GET /api/v1/projects/{project}/events/stream` (`text/event-stream`, `Last-Event-ID` cursor resume, zero gaps/duplicates, NDJSON in `--follow --output json`).
- **Project-wide watch:** `kodema watch --project <id> [--until request] [--output table|json]` prints an initial active snapshot (tasks, executions, pending inbox) and streams live transitions across the project. Pass `--until request` to exit 0 as soon as a pending assigned request or waiting task question needs human attention.
- **Block until inbox work arrives:** `kodema inbox --wait [--project <id>] [--kind <kind>] [--output table|json]` exits immediately if an actionable item is already present, or waits on the live project event stream until an assigned request, waiting question, or failed task appears.
- **Follow logs across actions:** `kodema logs --follow <execution-id>` (without `--action`) follows sequential container actions in order with `==> action <name> (<id>) <==` headers (or pure NDJSON with `--output json`), drains each completed action before advancing, skips non-container request actions, and pauses cleanly with a stderr status note if the execution waits on a `request` action. Pass `--action <name|id>` to follow one specific action.

## 6. Inspecting and recovering failed work

- Inspect status and action diagnostics (including `OOMKilled` and cleanup status): `kodema inspect <execution-id>`
- Read retained logs: `kodema logs <execution-id> --action <name> --stream stderr`
- Follow a single execution to completion (with failed-action stderr tail): `kodema watch <execution-id>`
- Retry a failed open task (as Engineer or Operator): `kodema task start <task-id>` (starts a fresh execution while preserving failed history).
- Cancel work: `kodema cancel <execution-id>` or `kodema task cancel <task-id>`.
