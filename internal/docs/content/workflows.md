# Kodema Workflows

## 1. Planned approval gates (same execution, preserved workspace)

Use a `request` action in pipeline YAML when human or service sign-off is part of the planned flow:

1. An earlier container action builds, tests, pushes a candidate branch/commit, and writes outputs (for example `/kodema/outputs/implement/candidate`).
2. The `request` action (`request: role/reviewer` or `request: role/operator` or `request: service/<id>`) resolves its immutable `subject`, revokes the execution token, keeps `/workspace`, and appears in the role inbox.
3. A responder inspects the inbox and settles the request:
   ```sh
   kodema inbox
   kodema approve <execution-id> [--comment "..."] [--output key=val]
   # or:
   kodema reject <execution-id> --reason "..."
   ```
4. Passing advances the **same execution** and reuses `/workspace` with a fresh execution token; the next action can now fast-forward an approval-guarded Git ref matching that exact subject. Rejecting or timing out fails the execution.

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

## 4. Inspecting and recovering failed work

- Inspect status and action diagnostics (including `OOMKilled` and cleanup status): `kodema inspect <execution-id>`
- Read retained logs: `kodema logs <execution-id> --action <name> --stream stderr`
- Follow a running execution: `kodema watch <execution-id>`
- Retry a failed open task (as Engineer or Operator): `kodema task start <task-id>` (starts a fresh execution while preserving failed history).
- Cancel work: `kodema cancel <execution-id>` or `kodema task cancel <task-id>`.
