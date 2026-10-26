---
id: T-108
milestone: production
status: planned
depends_on: [T-104]
decision_blockers: []
owns: [internal/execution/result.go, internal/execution/reconcile.go, internal/diag/]
check: deploy/stage/scripts/gate.sh --gate readiness --cloud scw --contract "$KODAMA_STAGE_CONTRACT" --values "$KODAMA_STAGE_VALUES"
resources: [go, integration]
acceptance: [kube_m1_roundtrip]
---
# T-108 — Result finalization ordering and actionable dispatch diagnostics

## Goal
Remove two operability gaps observed on the installed Kubernetes server, and explain one unexplained timeout. Scheduled by the owner on 2026-10-26.

## Context (established while qualifying T-104)
- **Acknowledgment versus termination.** After a writer's publication conflict the failed result was accepted (durable, hash recorded) and the server removed the workload while the entrypoint was still waiting for the acknowledgment. The entrypoint logged `finalization did not complete within its bound: context canceled`, the server logged an unclassified `submitResult … unavailable (503)`, and the pod exit looked like a failure. The outcome was correct but the signals are contradictory. `CommitResult` terminates revoked attempts before the acknowledgment returns.
- **Unclassified errors.** Refused dispatches and failed result commits log only `unavailable`/`outcome=unavailable` without the failing dependency, operation or HTTP status (finding 9 in [the T-104 report](../acceptance/kube-full.md#findings-from-the-first-installation) needed code reading; this one needed database inspection). Logs must stay free of credentials, signed URLs and provider bodies.
- **Unexplained writer timeout.** One trivial writer step ended `execution_timeout` once across six focused runs on a reused session; it did not recur in the readiness gate. No timing evidence was captured.

## Resolution (2026-10-26)
- **Acknowledgment before termination.** `CommitResult` no longer stops the workload of an *accepted* result before returning the acknowledgment: the stop of that attempt follows after `Options.TerminationDelay` (default 2s), detached from the request and guarded against a panic in the runtime; siblings cancelled by the failure drain have no acknowledgment to protect and stop at once. It is not left to the reconciler. The first version did that, which would have left the Job of every run that the result itself made terminal: `Store.Active` lists only queued and running runs and `ReconcileRun` returns early for a terminal one, so only the committing call can still stop its workload (found in review of the first version, after a readiness and a staging receipt had already run it; those receipts therefore do not cover the version that is merged). A late or refused result, which has no acknowledgment to protect, is still stopped at once. As before, a stop lost to a crash between the commit and the stop is not retried for a terminal run; a graceful shutdown inside the delay loses it the same way, and a replayed commit does not schedule one. The kube-full roundtrip's publication-conflict case now requires the server to have logged the writer's result as accepted and never as unavailable, and the conflicted writer's Job to be removed.
- **Classified, credential-free causes.** `internal/diag` renders only fragments an error vouches for (`diag.Safe`): the executor's fixed-text errors, the Kubernetes client's operation/class/HTTP status, and the results module's classified message. Execution and results logs gain a `detail` attribute (for example `kube: dependency unavailable: reading the Job <- kubernetes: get job: unavailable (HTTP 503)`). Transport causes, peer error text and anything not opted in stay out; the helper bounds length and strips control characters.
- **Timing for timeouts.** A missed deadline logs `execution: attempt deadline missed` with the attempt, step, reason, now, the startup and execution deadlines, whether dispatch was confirmed, whether a workload was bound and whether the step started. The unexplained writer timeout has not recurred; the next one is diagnosable.

- **A stalled request no longer holds the attempt (found while qualifying T-090).** A trivial step ran, printed its output and then went silent until the execution deadline failed it (`execution_timeout`), although the command had succeeded. The entrypoint's HTTP client had no per-request deadline (`Timeout: 0`), so one request that stalled after the connection was up waited for the whole five-minute finalization budget, longer than the step's own deadline. Each API request now has a bound (default 30s, response body included; two minutes at least for a source proposal, which publishes to Git while the request waits) and each upload a size-aware bound (60s plus one second per MiB, capped at 10 minutes); a timed-out try is retried like any transient failure and logged as `runtime request timed out; will retry` with the operation and the bound. The entrypoint also logs `command finished` and `finalizing`, so a future stall is located by the first line that is missing. The stalled request itself was inferred from the evidence (the command's output was the last line before `context canceled`, and the server saw no further request), not observed.

Qualified live at `0a905ef5989d` (the readiness and staging receipts; the first staging attempt, at `4daa4516f044`, is what exposed the unbounded request, and review of `8448adc623b5` what exposed the terminal-run stop).

Not changed: result acceptance rules, the attempt reason mapping (`command_failed` for non-output failures is intentional) and Job lifetime policy.

## Questions that were settled first
1. Whether the acknowledgment is returned before termination (and termination retried by the reconciler) or the entrypoint treats a durable-but-unacknowledged result as success, and which preserves the existing replay and revocation guarantees.
2. The allowlisted fields of a classified failure log (dependency, operation, status, retryable) and where they are emitted.
3. Which timestamps (admission, checkout, launch, result, termination) are recorded to make a timeout diagnosable.

## Provisional acceptance
A lost-acknowledgment and a termination-during-acknowledgment test on the Kubernetes stage installation end with consistent entrypoint, server and attempt records; a refused dispatch and a failed result commit each log a classified, credential-free cause; a repeated trivial-writer soak run either does not time out or yields diagnosable timing.

## Out of scope
Changing result acceptance rules, the attempt reason mapping (`command_failed` for non-output failures is intentional) or Job lifetime policy.
