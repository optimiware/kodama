# `kodama work` (T-025)

Command contribution for work items: `create`, `get`, `list`, `update`, `history`, `transition`. It translates flags and files into the public requests of [`api/work.yaml`](../../../api/work.yaml) and renders the answers. Status: implemented and tested against HTTP fixtures; `Test_stale_edit_exit` also runs the real work HTTP handler, use cases and PostgreSQL store. The central executable does not register it yet (integration owner, T-027). The five contribution packages of T-025 keep their own copy of a small `util.go` (flag, request and rendering plumbing) so they stay independent of each other.

```go
root.NewRunner(cfg, work.NewCommands(), ...)   // work.NewCommands() root.Contribution
```

## Commands (proposed spellings, see assumptions)

| Command | Notes |
| --- | --- |
| `work create --kind K --title T [--spec-file F\|-] [--acceptance-file F] [--evidence-file F] [--assignee ID] [--parent ID]` | `POST /work-items`. Spec Markdown at most 65536 bytes; acceptance is `[{"id","text"}]`, evidence `[{"type","id","note"}]` |
| `work get ID` | `GET /work-items/{id}` |
| `work list [--state S]... [--kind K]... [--assignee ID] [--parent ID] [--blocked[=false]] [--limit N] [--cursor C]` | One page. Repeated filters are disjunctive; the cursor is passed through unchanged |
| `work update ID --if-version N [--title] [--spec-file] [--acceptance-file] [--evidence-file] [--assignee\|--clear-assignee] [--parent\|--clear-parent] [--blocked-reason\|--clear-blocked-reason]` | `PATCH` with `If-Match`. Omitted fields are not sent; clearing sends `null` |
| `work history ID [--limit N] [--cursor C]` | One page of immutable history |
| `work transition ID --to STATE --if-version N [--reason R] [--review-file F] [--completion-file F]` | `POST .../transitions`. Claim files are the contract's `ReviewClaim` and `CompletionClaim` JSON |

`--if-version` is mandatory for `update` and `transition`. It is the caller's observation and is sent exactly as given; the CLI never fetches a replacement. The resource `version` (the ETag, used for `--if-version`) and the `contentRevision` (what review and completion bind to) are printed as separate, labelled numbers, and large integers keep their exact spelling in JSON output.

## Ownership

The server owns every rule: transition edges and permissions, WIP admission, `workspace_busy`, history, audit, completion by a human. The CLI does not check an edge, does not submit a transition after a run succeeds, and does not complete work. `work transition ... --to done` is an explicit user command with a user-supplied completion claim file.

## Recovery

* A stale version (412) or any conflict (409) exits 4 with the server's code and request ID; the message and `details.recovery` advise rereading `work get` and `work history`. Nothing is retried or refreshed.
* A lost answer to `create`, `update` or `transition` exits 1 (or 5 on a deadline) with `outcomeUncertain` and operation-specific advice: these requests carry no idempotency key, so the guidance is to reread (`work list`, `work get`, `work history`), not to replay.
* A malformed claim file, an unknown JSON member, a missing `--if-version` or a closed-set value outside the contract fails with exit 2 before any request.

## Current assessment

Historical baseline `14f4132` (initial implementation); reassessed at `d683fe18a889b60b4da81fcab0d00ad1f15b7704` and amended in this task. Requirements AC-01–AC-06, WM-02, WM-07; decisions C-04, C-06, C-12, C-13, C-16/C-17; ADRs 0005–0007. Evidence here is component evidence: contract-shaped HTTP fixtures plus the real runner, client and shared validators; it is not an assembled M1 certification, which belongs to the integration owner (T-026/T-027). Server modules own entities, authorization, transactions, history, audit, idempotency and every state transition; this package owns only flag parsing, bounded local input and rendering. Acceptance: `Test_cli_core_journey` (revised content, history, workspace archive/restore, explicit human completion), `Test_stale_edit_exit` (real work handler, use cases and PostgreSQL: one winner, one exit-4 conflict, no retry), `Test_input_timeout_no_request`.

Bounded local input (files and redirected standard input) obeys the command context: a stalled pipe cannot outlast `--timeout` or an interrupt, the command exits 5 with empty stdout, nothing is submitted, and the outcome is not reported as uncertain because no mutation was attempted. The blocked read can never feed a later request: its bytes are discarded. Paths are type-checked (`stat`) before they are opened, so a FIFO is refused instead of blocking.

Assumptions for T-025 review and the integration owner: the flag spellings follow the [CLI walkthrough](../../../docs/examples/api/cli-and-http.md); the additions `--clear-*`, `--evidence-file`, `--review-file` and `--completion-file` are **proposed**.
