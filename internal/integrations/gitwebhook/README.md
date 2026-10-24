# internal/integrations/gitwebhook

Native Git push trigger (task T-036, decision O-03): a server-side `post-receive` hook on a Git host the installation controls sends an authenticated notification; Kodama verifies it and starts a run for the exact announced commit. HTTP contract and wire format: [`api/git-webhooks.yaml`](../../../api/git-webhooks.yaml). Example sender: [`example/post-receive`](example/post-receive).

**Status.** Implemented and unit-tested against in-memory persistence and Git/admission doubles. It is **not wired**: nothing registers the route, a PostgreSQL store, the real `workspace.RefReader` or `execution` admission yet. T-038 owns that join (`native_hook_roundtrip`) and real Git/persistence behavior. Management use cases (`Install`, `SetEnabled`) are Go-level; their HTTP exposure belongs to the assembly that owns identity routing.

**What this is not.** No Git server or transport, no GitHub/provider adapter, no universal provider abstraction, and no payload field is ever executed or used as a URL, path, tenant or credential.

## Trust model

| Input | Authority |
| --- | --- |
| Trigger ID in the URL | An address only. |
| `Kodama-Signature` | HMAC-SHA256 under the trigger's hook secret over `trigger ‖ delivery ‖ timestamp ‖ SHA-256(body)`; ±5 min clock window. |
| Tenant / project | Only the trigger record. The payload cannot carry them (unknown members are rejected). |
| `repository` member | A claim checked against the trigger's bound repository; the project's *current* repository must still equal it. |
| Commit | Announced by the hook, then proved on the remote: branch exists and its history contains it, read with the project's **read-only checkout credential**. The tip is never used; branch movement cannot change the run's commit. |
| Run authority | The installing principal's current `ActionRun`, re-checked every delivery. The hook secret is not an identity. |

The hook secret must be a dedicated secret (≥32 bytes), never the checkout or publisher credential; the publisher reference is never offered to the Git port.

## Delivery

Order: authenticate → parse → **durable claim** `(trigger, delivery id)` → replay a finished record → repository identity → branch filter → current repository binding → installer authority → exact commit on remote → `BeginAdmission` → admission phase → finish with audit. A forged request never reaches the claim, so it cannot burn an identifier. Every terminal result of an authenticated, well-formed delivery (started, ignored, or any refusal) is persisted atomically with its audit, so the identifier is deduplicated whatever the outcome: replay returns the recorded result (even through repository changes or outages), and the same identifier with another body is `delivery_conflict`. A dependency outage leaves the claim pending and the retry resumes it.

**Admission phase and fences.** `BeginAdmission` durably marks the delivery `Admitting` before the first admission attempt; from then a run may exist. The phase first recovers a run already admitted under the idempotency key `git-hook:<trigger>:<delivery>` (`Runs.FindRun`), with no authority or branch re-check, so a lost response or lost finish followed by installer revocation or branch movement still yields the actual run and its `run_started` audit. To create a run that does not exist, the processor must hold the delivery's **admission lease** (`Store.AcquireAdmission`, `AdmissionLease` = 2 min, `StartRun` bounded to 30 s). Only the holder calls `StartRun` or records a refusal, so success and refusal are mutually exclusive durable outcomes: a competing processor gets `ErrAdmissionBusy` (an `ErrUnavailable`, HTTP 503, retry) and the store rejects a refusal finish from a non-holder. **Refusal ledger.** A refusal is first decided in admission's own ledger (`Runs.DecideRefusal`, atomic with run creation for the key): if a run exists it wins and is recorded as `started`; otherwise the refusal is made durable (first reason wins) and `StartRun` for that key then creates nothing and returns `ErrAdmissionClosed` — even for a holder that stalled before its call, lost its lease, and resumes after a replacement recorded the refusal. The delivery records a rejection only from that ledger result, so a rejected delivery can never acquire an unrecorded run, and a lost finish replays the same reason. Under the lease the project's current repository is re-checked (a rebound project is a recorded `repository_rebound` refusal, never a new run) and the verified `RepositoryID` is carried in `RunRequest` so admission can preserve and enforce it. A processor that fails non-terminally releases the lease at once; a vanished holder delays others only until expiry. The lease only limits contention; correctness against a stalled holder rests on the ledger. T-038 must implement `StartRun`/`FindRun`/`DecideRefusal` over one admission store with that atomicity (idempotent by key, refusing a conflicting `RepositoryID`). A pre-admission result (ignored/rejected) cannot be recorded for an `Admitting` delivery; that processor joins the admission. Filters are explicit (`main`, `release/*`; never "all").

## Persistence seam

`Store` (ports.go) owns triggers, delivery records and audit, writing audit in the transaction of the change. `gitwebhooktest.Store` proves policy only (uniqueness, first-finish-wins, optimistic version). A PostgreSQL adapter must give `ClaimDelivery` a unique `(trigger_id, delivery_id)` key and decide a retention horizon for delivery records (not chosen here: records are kept).

## Audit

`git_trigger.installed|enabled|disabled`, `git_hook.authentication_failed` (known triggers only; unknown triggers have no tenant to attribute), `git_hook.rejected`, `git_hook.ignored`, `git_hook.duplicate`, `git_hook.run_started`. Records carry delivery ID, ref, commit, run and a stable reason; never signatures, bodies or secrets. Refusal audits are best effort; the `run_started`/`rejected` outcome audit is atomic with the delivery record. Rate limiting of unauthenticated traffic is an edge concern of the assembly.

## Acceptance

`TestForgedSignature`, `TestDuplicateDelivery` (incl. ignored/foreign/replay-after-repository-change outcomes), `TestAdmissionLeaseExcludesRefusal`, `TestStaleHolderCannotCreateRunAfterRefusal`, `TestRefusalLedger`, `TestAdmissionRecoveryRevalidatesRepository`, `TestAdmissionRecovery` (lost response/finish plus revocation and branch movement, concurrent conflicting observations), `TestForeignRepository`, `TestExactCommitTrigger` (AC-07, AC-A05, PX-02), plus `TestHookEndpoint*`, `TestExampleHookScript` (the shipped script against the real handler) and fragment contract tests. Baseline: spec AC-07/AC-A05, decision O-03, PX-02.
