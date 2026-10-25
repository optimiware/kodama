---
id: T-108
milestone: production
status: deferred
depends_on: [T-104]
decision_blockers: []
owns: [internal/execution/result.go, test/e2e/execution-finalization/]
---
# T-108 — Result finalization ordering and actionable dispatch diagnostics

## Goal
Remove two operability gaps observed on the installed Kubernetes server, and explain one unexplained timeout. **This task needs explicit scheduling.**

## Context (established while qualifying T-104)
- **Acknowledgment versus termination.** After a writer's publication conflict the failed result was accepted (durable, hash recorded) and the server removed the workload while the entrypoint was still waiting for the acknowledgment. The entrypoint logged `finalization did not complete within its bound: context canceled`, the server logged an unclassified `submitResult … unavailable (503)`, and the pod exit looked like a failure. The outcome was correct but the signals are contradictory. `CommitResult` terminates revoked attempts before the acknowledgment returns.
- **Unclassified errors.** Refused dispatches and failed result commits log only `unavailable`/`outcome=unavailable` without the failing dependency, operation or HTTP status (finding 9 in [the T-104 report](../acceptance/kube-full.md#findings-from-the-first-installation) needed code reading; this one needed database inspection). Logs must stay free of credentials, signed URLs and provider bodies.
- **Unexplained writer timeout.** One trivial writer step ended `execution_timeout` once across six focused runs on a reused session; it did not recur in the readiness gate. No timing evidence was captured.

## Questions to settle first
1. Whether the acknowledgment is returned before termination (and termination retried by the reconciler) or the entrypoint treats a durable-but-unacknowledged result as success, and which preserves the existing replay and revocation guarantees.
2. The allowlisted fields of a classified failure log (dependency, operation, status, retryable) and where they are emitted.
3. Which timestamps (admission, checkout, launch, result, termination) are recorded to make a timeout diagnosable.

## Provisional acceptance
A lost-acknowledgment and a termination-during-acknowledgment test on the Kubernetes stage installation end with consistent entrypoint, server and attempt records; a refused dispatch and a failed result commit each log a classified, credential-free cause; a repeated trivial-writer soak run either does not time out or yields diagnosable timing.

## Out of scope
Changing result acceptance rules, the attempt reason mapping (`command_failed` for non-output failures is intentional) or Job lifetime policy.
