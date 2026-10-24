# Observability and audit

Correlate tenant, project, work item, workspace, run, step, attempt, and request/trace IDs. Metadata is separate from raw logs/transcripts and required result evidence.

## Logs

Proxy live Kubernetes logs only after authorizing the specific attempt. The entrypoint uploads a log artifact at completion; a controller fallback retrieves logs when the wrapper cannot finalize. Node deletion can lose unavailable log tails, so log completeness is explicit. Optional Loki aggregation uses server-constructed tenant-scoped queries rather than caller-supplied selectors.

## Signals

| Area | Minimum signals |
| --- | --- |
| API/database | Latency, authorization/conflict/throttle rates, pool/lock pressure |
| Events | Outbox oldest age, publish errors, inbox retries, consumer lag, quarantine |
| Execution | Eligible wait, active attempts, startup/finish, pending cancellation, unknown outcomes |
| Source | Writer contention, CAS conflict, publication age/failure, retained-ref verification |
| Review/delivery | Pending review age, stale decisions, preview expiry/cleanup backlog |
| AI/MCP | Request count, observed/estimated tokens, stream failure, tool denial/outcome |
| Flow | M3 SQL active/blocked/review intervals and reopens |

Keep high-cardinality IDs in protected records/log fields rather than unbounded shared metric labels. Capacity/latency targets require the declared reference workload.

Under O-13, Platform owns commercial metering, usage-access policy, subscriptions and billing. Kodama retains attributable runtime request/token/tool/outcome metadata and enforces execution bounds, supplying scoped correlation/evidence to Platform without a pricing, invoicing or entitlement subsystem.

## Audit and content

Commit actor/action/resource/version/reason and publication intent with business changes. Audit membership/settings, secret access, run start/cancel, source publication, gate/completion, deployment/cleanup, restore/replay, and deletion. Logs do not replace these records.

Secret files stay outside source/output roots. Redact known injected values where possible; arbitrary tenant output still requires access controls and retention. Model prompts, responses, and tool content have separate opt-in retention. Required routing/usage/outcome metadata remains without raw content recording.

Use the [approved O-11 baseline](upgrades-backup-and-recovery.md#approved-retention-and-recovery-baseline): ordinary execution logs 30 days, audit metadata 1 year, raw model/tool content off by default with an authorized 7-day initial opt-in policy, and run/evidence retention extended by active references. Report backup/pin retention separately from online deletion.

## Operational checks

Trace the reference workflow from API to Job, source commit, evidence, human decision, and deployment. Confirm another tenant cannot retrieve any of these paths. During backend outages, distinguish lost diagnostics from failed required finalization. Investigate pending publication/unknown external effects before releasing ownership or retrying them.

Use the [recovery guide](upgrades-backup-and-recovery.md) for fencing, restore, and replay. Readiness reports capability-specific degradation instead of claiming the entire installation is healthy solely because its Pod is running.
