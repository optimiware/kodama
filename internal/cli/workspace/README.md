# `kodama workspace` (T-025)

Command contribution for workspaces: `provision`, `get`, `sync`, `archive`, `restore`, over [`api/workspace.yaml`](../../../api/workspace.yaml). It prints the authoritative Git information the server returns and nothing more. Not yet registered in the executable (integration owner, T-027).

```go
root.NewRunner(cfg, workspace.NewCommands(), ...)
```

| Command | Request |
| --- | --- |
| `workspace provision --work ID` | `POST /work-items/{id}/workspace`, no body, key or version: idempotent by work-item ownership. 201 new, 200 existing, 202 still provisioning |
| `workspace get ID` | `GET /workspaces/{id}` |
| `workspace sync\|archive\|restore ID --if-version N` | `POST /workspaces/{id}/{action}` with the caller's `If-Match` |

Output: workspace ID, work item, repository ID, branch, accepted commit, writer attempt, in-flight publication, archive request time, safe `lastError` code, and the resource version to pass as `--if-version`. The response carries no clone URL, so none is printed or derived from the repository ID; the text tells the user to clone or fetch the branch with their own authorized Git identity. The CLI starts no Git process, opens no editor and fetches no publisher credential.

## Recovery and states

* `provisioning`, `available` and `archived` are rendered distinctly. A 202 (or `provisioning` state) says to repeat `workspace provision`.
* `409 archive_pending` is a conflict (exit 4), never a success: admission is closed and the request is retained, but settlement is not proven. The advice (also in `details.recovery`) is to poll `workspace get` and repeat with the current `--if-version`. `publication_pending`, `branch_diverged` and `workspace_state_conflict` keep the server's meaning.
* Versions are never refreshed or retried by the CLI; `restore` addresses the same workspace and never provisions a replacement.
* A lost answer is reported as an uncertain outcome with workspace-specific advice (reread with `workspace get`).

## Current assessment

Historical baseline `14f4132` (initial implementation); reassessed at `d683fe18a889b60b4da81fcab0d00ad1f15b7704` and amended in this task. Requirements AC-01–AC-06, WM-A02 context. Evidence here is component evidence: contract-shaped HTTP fixtures plus the real runner, client and shared validators; it is not an assembled M1 certification, which belongs to the integration owner (T-026/T-027). Server modules own entities, authorization, transactions, history, audit, idempotency and every state transition; this package owns only flag parsing, bounded local input and rendering. Archive and restore are exercised in `Test_cli_core_journey` (work package) with observed versions and a stale-version conflict.

Assumption for confirmation (T-013 and the integration owner): Git information is limited to the returned repository ID, branch and accepted commit plus ownership/state; no clone URL is derived and no publisher credential is fetched.
