# `kodama run` (T-025)

Command contribution for pipeline runs: `start`, `get`, `list`, `steps`, `cancel`, `watch`, `logs`, over [`api/runs.yaml`](../../../api/runs.yaml), [`api/observation.yaml`](../../../api/observation.yaml) and [`api/logs.yaml`](../../../api/logs.yaml). Not yet registered in the executable (integration owner, T-027).

```go
c, err := run.NewCommands(run.Config{
    Tokens:                svc,   // the runner's root.TokenResolver; used by the watch adapter
    HTTP:                  httpClient,
    AllowInsecureLoopback: dev,   // mirrors root.RunnerConfig
    // Observer: func(root.EffectiveContext) (run.Observer, error)  // optional replacement transport
    // Watch: run.WatchOptions{MaxReconnects, BackoffMin, BackoffMax, IdleTimeout, MaxFrameBytes}
})
```

## Commands

| Command | Notes |
| --- | --- |
| `run start --work W --definition D --content-revision N --source-commit SHA --idempotency-key K [--input NAME=VALUE]... [--input-json NAME=JSON]...` | `POST /runs`. Every flag but the inputs is mandatory. `--input` is a string (an artifact is its ID); `--input-json` is one JSON string, boolean or integer. Duplicate names, a missing `=`, floats, `null` and containers are usage errors; types are never guessed |
| `run get ID`, `run list [--status S]... [--work W] [--limit] [--cursor]`, `run steps ID [--limit] [--cursor]` | Reads; lists are one page with the cursor passed through |
| `run cancel ID` | Bodyless `POST .../cancel`. Reports the returned state; the text says cancellation is a request and the run stays non-terminal until the server settles it |
| `run watch ID [--exit-on-terminal]` | Observes until the run is terminal (below) |
| `run logs ID [--step S] [--attempt A] [--tail N] [--follow] [--source auto\|live\|wrapper\|controller]` | Reads one attempt's bounded log (below) |
| `run logs ID --resource preparation\|service.NAME [--tail N]` | Reads the bounded log of a `run_shared` run's preparation or a support service (T-116); excludes `--step`, `--attempt`, `--follow` and `--source`; same meta/chunk/summary protocol with `resource` in the meta record, rendered only when it is the requested resource's and complete |

## Watch

`watch` is the only command that keeps local state, and only about its own subscription: initial snapshot, observing, reconnecting, resynchronizing, terminal. It reads the run, subscribes to `GET /runs/{id}/events` as SSE through a replaceable, read-only observation adapter (`run.Observer`, `NewHTTPObserver`), and applies frames:

* The resume cursor advances only after a complete, validated frame (`snapshot`, an event with its cursor, `checkpoint`). Validation covers the cursor format, a snapshot's `resources` array and entries, a checkpoint's cursor, the `resync_required` payload and, for this run's `run.state_changed`, a valid resource version and target state; a frame that fails it moves neither the cursor nor the deduplication state. A frame cut off by a disconnect is dropped. Events are deduplicated by ID; a run resource version never regresses.
* After a disconnect (or idle silence beyond `IdleTimeout`) it reconnects with `Last-Event-ID`, with bounded exponential backoff (honouring `Retry-After` up to the bound; the value is read like the shared client reads it: a strict number of seconds or a future HTTP date, anything else is ignored) and at most `MaxReconnects` consecutive reconnects without progress.
* `410 cursor_expired` or a `resync_required` frame fetches the JSON snapshot from the same scoped route and resumes from its cursor. Any other cursor error (for example `400 invalid_cursor`) terminates; the CLI never silently starts a fresh subscription.
* 401/403/404 terminate observation (exit 3 or 1 per the foundation); 429/5xx/transport breaks retry within bounds. Events for another tenant or project, malformed, oversized or out-of-order frames, and redirects terminate with exit 1.
* A terminal state seen on the stream is confirmed by reading the full run, which is also the result. A watch of any terminal run succeeds; `--exit-on-terminal` exits 1 (code `run_failed` or `run_cancelled`, stdout empty) for `failed` and `cancelled`. A deadline or interruption exits 5 with nothing on stdout. Response bodies and timers are released on every exit.
* A passing run never transitions or completes the work item; the text says so.

JSON result: `{"run": <full run>, "observation": {"cursor","events","reconnects","resyncs"}}`.

## Logs

Uses the shared client's bounded raw-body mode and checks the NDJSON protocol `meta`, `chunk*`, `summary`. `--step` reads the run's step pages and resolves the attempt; several attempts need an explicit `--attempt` (listed in the error); only `attemptId`, `tail`, `follow` and `source` are ever sent. Output preserves origin, completeness, reasons, truncation and stop, and states that it is the attempt's log, not the run's outcome. Every required member is checked for presence and type (chunk `text` and `lineTruncated`; summary `lines`, `bytes`, `truncated`, `stop`; metadata identifiers and `reasons`); additive members on any record survive in the JSON output, an empty log renders `"lines": []`, and an unrecognised stop reason is a failure, not a success. A missing summary, a malformed or out-of-order record, an identity that differs from the request, or a summary stop of `authorization_changed` (exit 3), `unavailable` or `interrupted` (exit 1) fail with empty stdout; nothing partial is rendered as success.

## Recovery

`start` is keyed and never resent automatically. After an uncertain outcome the guidance is: repeat the identical command with the identical key (the stored answer is replayed after the permission is rechecked), or read `run list --work`; a changed body under a used key is a 409 `idempotency_conflict`, a new key creates another run. `cancel` is idempotent; repeating it is safe.

## Assumptions (for T-025 review and the integration owner)

1. **Observation transport.** The shared client buffers responses and cannot send `Last-Event-ID` or stream, so `watch` uses a narrow adapter (`NewHTTPObserver`) that reuses the injected token source, HTTP client and loopback policy, refuses redirects and addresses only the scoped run events route. It is replaceable through `Config.Observer`.
2. **Buffered output.** The foundation prints one document on success and nothing to stdout on failure, so `watch` consumes events internally and `logs` buffers one bounded read (the shared client bounds a response at 4 MiB; `--follow` therefore returns when the live log ends). There is no JSON-lines mode.
3. **Spellings.** `--input`, `--input-json`, `--attempt`, `--tail`, `--follow`, `--source` and `--exit-on-terminal` are proposed; `--exit-on-terminal` is from the interface specification.

## Current assessment

Historical baseline `14f4132` (initial implementation); reassessed at `d683fe18a889b60b4da81fcab0d00ad1f15b7704` and amended in this task. Requirements AC-01–AC-06. Evidence here is component evidence: contract-shaped HTTP fixtures plus the real runner, client and shared validators; it is not an assembled M1 certification, which belongs to the integration owner (T-026/T-027). Server modules own entities, authorization, transactions, history, audit, idempotency and every state transition; this package owns only flag parsing, bounded local input and rendering. Acceptance: `Test_watch_terminal_exit`, `Test_watch_resume_resync`, `Test_observer_retry_after_parity`, `Test_logs_protocol`, `Test_mutation_recovery`.

Assumptions for T-025 review and the integration owner: `run.Observer` stays a separate read-only adapter because the shared client has no streaming or `Last-Event-ID`; its reconnect, idle and frame bounds are client-local defaults; output stays buffered (watch returns one final document, `logs --follow` one bounded document).

## `run_shared` view (T-116)

`run get` additionally renders, when the server returns them for a `run_shared` run, `runModel`, `deadlineAt`, `executionOrder` with each step's effective workspace access, `cleanupReason` and the safe `resources` (key, phase, reason, ready/stopped/removed times). The JSON output keeps every member the server sent, including ones the CLI does not know. An `attempt_private` run renders exactly as before. Evidence: `Test_unit_cli_run_resources_view`.
