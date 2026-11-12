# S02 — Kernel lifecycle and recovery

Source: [pipeline execution](../../KODAMA.md#2-pipeline-execution), [recovery](../../KODAMA.md#lifecycle-and-recovery). Owners: K01–K05. AC: 1, 4, 6, 9, 11–13, 16.

## 1. Durable model

An execution has immutable admitted definition/input data, ordered action identities, state, current action index, concurrency version, workspace identity, and attributable pending effects/cleanup. An action has kind, slug/index, state/version, resolved timeout duration, activation/deadline timestamps, nullable outputs, and kind-specific data.

Container data includes pinned image, literal environment, command script, declared secret names, optional declared output names, stable workload identity, launch intent and observations. Request data includes assignment as an opaque validated value, unresolved subject at admission, immutable resolved subject at activation, and settlement metadata supplied through an authorized Shell boundary.

Kernel never authenticates the answerer. A Shell transaction validates the actor and passes an authorized decision fact to the same transition function used by Kernel tests. Decision/actor/time/subject/version are stored separately from workload-controlled output strings.

States are exactly:

```text
execution: queued | running | succeeded | failed | cancelled
action:    pending | running | succeeded | failed | cancelled
```

Resource preparation, launch uncertainty, stop observation, and cleanup are reconciler substates/evidence, not extra public execution states.

## 2. State transition contract

| Current condition | Fact/command | Required transition and intent |
|---|---|---|
| Queued, dispatch barriers satisfied | Activate first action | Execution running; persist action running, activation time, absolute deadline, stable effect identity |
| Pending next action, previous succeeded and stopped | Activate container | Persist intent, then Prepare/start through ports |
| Pending request, previous workloads stopped | Activate request | Resolve subject; persist subject/assignment/deadline; invalidate credential; request becomes answerable |
| Running container | Confirmed exit zero | Succeed once; promote staged outputs; advance only when stop confirmed |
| Running container | Confirmed nonzero exit | Fail action/execution; discard staged outputs; cancel pending actions `upstream_failed` |
| Running request before deadline | Authorized pass | Succeed once, record response; same execution may advance |
| Running request before deadline | Authorized fail | Fail action/execution; retain response for inspection; cancel pending actions |
| Any running action at/past deadline | Expire | Fail with `timeout`; invalidate execution authority; stop workload if any |
| Queued/running execution | Cancel | Cancel active and pending actions; discard staging; revoke authority; stop/cleanup |
| Any terminal state | Duplicate matching fact | No outcome change; return existing result where retry contract applies |
| Any terminal state | Conflicting settlement | Refuse; never reopen or replace history |
| Retained workspace missing | Reconcile | Fail visibly; cancel pending work; never reconstruct/replay scratch state |

Preparation or subject-resolution failure can settle the selected action as failed before any workload/request becomes usable. Persist a diagnostic and fail the execution; never expose an answerable request with an incomplete subject.

At most one action is active. Pending actions after failure are cancelled, never skipped. A terminal success requires every action to have succeeded. Cancellation/timeout may make the outcome terminal while workload stop or cleanup remains pending; the stop barrier still prevents any workspace handoff or replacement dispatch.

## 3. Time and race linearization

- Admission resolves each duration in `[1 minute, 30 days]`; queue time consumes none of it.
- Activation persists `deadline = activation_time + timeout` in UTC before making work usable. Restart does not recalculate it.
- An answer is timely only when the authoritative transaction time is **strictly before** the persisted deadline. At equality it expires. Sender timestamps are ignored.
- Resolution, cancellation, expiry, and output submission serialize on authoritative execution/action versions and the same commit boundary.
- Reconcile re-reads after a conflict. It does not replay a stale transition or overwrite the winner.
- Identical request/output retries return the recorded result after current caller validation. New content or a replacement decision is refused.

No execution-wide deadline, command resume, backward transition, dynamic action append, or implicit retry exists.

## 4. Output contract

One nullable JSON object contains at most eight declared UTF-8 string values, each at most 4,096 bytes. Byte length, invalid UTF-8, undeclared keys, and non-string values are rejected. Output submission is optional for containers; declared fields do not create a required custom success outcome. A missing downstream value fails subject resolution/preparation as applicable.

Container submission is accepted only for the current running container action and becomes immutable staging. An identical map retry converges; a different map is refused. A runtime-confirmed successful settlement promotes it. Failure/cancel/timeout discards it. The runtime never reads container output files.

Request response values undergo declared-schema validation within settlement. Failed-request answers remain inspectable in the decision record and never appear in downstream materialization. Only earlier succeeded actions are visible to subject lookup or `/kodama/outputs`.

## 5. Reconciler algorithm

1. Load execution/action, intents, persisted deadlines, and resource observations through injected ports.
2. Observe any outstanding external effect before deciding whether to retry it.
3. Feed an explicit fact and time to pure transition logic.
4. Compare-and-commit changed state and stable effect identities before performing effects.
5. Execute only committed intents; record observations with version checks.
6. Reschedule unfinished work using persisted state/due times. Polling/restart scan is authoritative; notifications only accelerate it.

Runtime observation vocabulary must include **never started with affirmative proof**, **running**, **exited with status and stop confirmation**, **absent**, and **unknown**. `absent` alone is neither proof of nonexecution nor proof of stop. Stable workload IDs cannot be reused for a fresh command attempt.

Lost start response: observe the original identity. If running/exited, reconcile it. If unknown/absent without sufficient evidence, keep visibly uncertain and do not launch again. Relaunch is allowed only with affirmative proof the command never ran. A lease expiration or Kubernetes object deletion never supplies that proof alone.

Relaunch is retire-then-replace: on affirmative never-started proof, persist the identity as retiring with its proof, remove it (idempotently, only that identity's resources), then commit the replacement identity; cancellation/expiry during retirement settles normally and cleanup removes the retiring identity from the persisted proof; `absent` still never supplies the proof.

Lost stop response: observe until stop is confirmed or leave attributable pending cleanup. An expired lease cannot release workspace ownership. Request waits retain the workspace without a live workload. Fresh executions get different identities/workspaces and no inherited approvals.

## 6. Runtime and resource ports

| Port behavior | Contract |
|---|---|
| `Prepare` | Materialize activation data once per workload, attach existing execution workspace, no start on failure |
| `Start` | Stable identity, fail-fast script, pinned image; result distinguishes accepted, proven-not-started, unknown |
| `Observe` | Evidence about the same identity, including exit/stop and workspace availability |
| `Logs` | Ordered/identified chunks, bounded reads, no lifecycle authority |
| `Stop` | Request termination; success acknowledgment alone is not fabricated stop evidence |
| `Remove` | Idempotent cleanup after appropriate stop; retain actionable resource IDs on failure |
| `RefreshCredential` | Replace token material without replay/restart; recover failed delivery |
| `Capabilities` | Explicit admitted runtime requirements; reject unsupported work before dispatch |

Resource and state ports are separate from the runtime where ownership differs, but all effect intent is recoverable. Bounded cleanup retry count/backoff is installation policy; exhaustion leaves visible pending work, not a falsely successful cleanup record.

The workspace port's `Retain` is called before the activation commit, so the activating action is still pending when it runs and a committed activation always has a retained workspace: a request is never answerable and a workload never starts without one. This is the one effect that precedes its commit. It is safe because `Retain` is idempotent and keyed by the workspace identity persisted at admission: a failed or interrupted `Retain` commits nothing and a later pass repeats it, and terminal cleanup releases the workspace of an execution that never activated. A retryable `Retain` failure (dependency unavailable, version conflict, uncertain external effect) settles nothing; any other failure fails the execution as `workspace_lost` before the action becomes usable.

## 7. Required unit scenarios

Use deterministic fake time and fault barriers; no sleeps, broker, real processes, or Shell composition.

- **K-ORDER:** zero/one/many container/request combinations; only success advances; no overlapping active action.
- **K-DEADLINE:** queued delay, exact deadline boundary, restart with old deadline, late answer, timeouts across both kinds.
- **K-REQUEST:** pass/fail/expiry/cancel; immutable subject; missing subject; competing facts and identical retry.
- **K-OUTPUT:** byte/key/type limits, staging/settlement races, failed answer retention, successful-only visibility.
- **K-RECOVER:** crash before intent, after intent/before start, after start/before response, after settlement/before next activation.
- **K-STOP:** lost stop, absent workload, expired controller lease, confirmed stop, replacement barrier.
- **K-WORKSPACE:** retained across waits/restarts, missing workspace failure, no recreated scratch state.
- **K-CLEANUP:** repeat removal, failure attribution, bounded retry, terminal outcome remains immutable.
- **K-INDEPENDENT:** Kernel imports and tests cannot depend on Shell or infrastructure clients.

G1 requires all scenarios, including generated legal/illegal transition sequences and concurrent commit conflict schedules. Real process/container conformance is reserved for I02 after G2.
