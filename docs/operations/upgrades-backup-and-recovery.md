# Upgrades, backup, and recovery

## Recovery inventory

| Data | Durable source |
| --- | --- |
| Product/run/review/audit/events | PostgreSQL backup/PITR |
| Source checkpoints | Git work and retained run refs, plus declared repository backup/mirror |
| Evidence/transcripts/logs | S3-compatible artifacts with verified digests |
| Images | OCI registry retention by digest |
| Encrypted secrets | PostgreSQL envelopes plus retained KMS key versions |
| External execution/effects | Kubernetes attempt labels/UIDs, publication fields, deployment rows/annotations |
| Run resources (`run_shared`) | Execution's durable resource intents and evidence; scratch contents and service state are disposable and **never** recovered |

Uncommitted scratch files, shared run scratch and service state are explicitly outside recovery; a lost run is rerun from a pinned Git commit. Git and object retention must cover every supported database recovery point. Upload/publication is confirmed before metadata claims durability; subsequent garbage collection respects referenced runs/gates and backup windows.

## Approved retention and recovery baseline

The owner accepted this O-11 baseline on 2026-10-20. These are implementation and qualification objectives; T-029/T-056 must measure them on the supported installation before they become advertised service guarantees.

| Area | Approved baseline |
| --- | --- |
| Complete-factory RPO | At most 15 minutes of recoverable committed state/source loss |
| Complete-factory RTO | At most 4 hours to recover and safely resume the declared business loop |
| PostgreSQL recovery | Continuous WAL/PITR with a 30-day supported recovery window |
| Git recovery | Protected work/run/release refs plus a declared backup/mirror at most 15 minutes behind; refs/objects cover every supported database recovery point |
| Ordinary run evidence/artifacts | 90 days, extended while referenced by retained runs, gates, supported releases or recovery points |
| Execution logs | 30 days, subject to explicit retained-evidence references |
| Audit metadata | 1 year; raw logs/content are not a substitute for audit |
| Raw model/tool content | Off by default; explicitly authorized opt-in retention starts at 7 days |
| Preview environments | 24-hour default TTL, explicitly configurable |
| Released images/source | Retain while the release is supported and while any reference/recovery window requires it |
| Tenant offboarding | Revoke new access/admission immediately; target ordinary active-data deletion within 7 days after safe settlement; disclose backup expiry up to 30 days |
| KMS/key versions | Retain every version needed by supported encrypted records, pins and backup recovery points |

Record actual dependency versions, hardware and measured recovery results. Object/image retention, Git mirrors, PostgreSQL recovery and KMS access must produce a consistent recoverable set; a database-only restore does not meet the RPO/RTO. Ordinary TTLs cannot garbage-collect pinned evidence or a retained release's source. Online content expiry does not imply immediate deletion from backups.

Offboarding reports separately identify approved retained audit metadata, active reference pins, supported-release material and backup/key expiry, including any outstanding deletion exception. They must not report complete erasure while required retained copies remain. Broker/outbox/inbox deduplication and cursor windows retain their independently qualified protocol horizons; the numbers above do not silently redefine safe replay.

## Upgrade

A `run_shared` run records its model, runtime context version and execution policy; an older controller refuses it rather than adopting it with old semantics. Before rolling the controller back, stop admission, cancel or drain `run_shared` runs and reconcile their resources. Policy versions are append-only and retained while any nonterminal run pins them.

Pin the release's supported dependency/schema/client versions. Verify a usable backup and KMS/Git/artifact recovery path. Pause dispatch when mixed-version execution is unsupported. Apply ordered expand/contract migrations under a lock, roll the server, run identity/source/execution/result smoke tests, then resume dispatch. Binary rollback is permitted only with a compatible schema; otherwise use the documented restore procedure and reconcile post-backup effects.

## Restore procedure

1. **Quiesce:** pause dispatch and side-effecting consumers. Fence old control-plane instances and publishers. Revoke/rotate publisher Git and target credentials where an old process could retain authority.
2. **Restore:** recover PostgreSQL, Git refs/objects, artifacts/images, and KMS access to the selected consistent recovery window. Verify referenced digests/commits/decryptability.
3. **Rotate:** replace workload signing keys and revoke old attempt admission. Existing remote operations/credentials still require explicit settlement; key rotation alone is not fencing.
4. **Inventory:** match run resources (scratch, services, preparation, network boundary) by scope, spec fingerprint and UID, adopt intact ones or fail their run explicitly, and reap orphans; match Jobs by attempt identity/spec/UID, identify orphaned workloads, and stop/reconcile them. Inspect pending Git publication fields against remote refs. Observe deployments by their recorded identities before any repeat apply.
5. **Rebuild coordination:** release writer/capacity only after publication/effect uncertainty is resolved. Replay outbox events and use consumer inboxes/external records to prevent duplicate effects.
6. **Resume:** admit a bounded set of new work, inspect reconciliation backlog, and complete the foundation tracer with restored data.

## Failure playbook

| Failure | Action |
| --- | --- |
| Lost API response | Retry keyed create; reread versioned edits |
| Crash after Job create | Observe deterministic matching Job before another create |
| Lost Git push response | Read remote expected/proposed ref before publication retry/reassignment |
| Human push race | Report CAS conflict; retain proposal for explicit rework |
| NATS outage/loss | Preserve outbox, recover broker, deduplicate/reconcile replay |
| Node loss | Recover from accepted Git commit; report uncommitted loss and possible unknown effects |
| Scratch or service lost mid-run (`run_shared`) | The run fails with an explicit reason once resources are verified stopped; rerun with new resources. No empty replacement under the same run |
| Stuck resource cleanup | The run stays nonterminal with visible `resource_stop_unconfirmed`/`cleanup_pending`; retried; capacity stays reserved until verified removal; operator resolves the backend object, never a foreign one |
| Unknown deployment | Observe recorded target objects/action identity before repeating |
| KMS failure | Recover key/provider access; never substitute plaintext or destroy retained envelopes |

## Restore drills and acceptance

T-029 demonstrates real database/Git/blob/KMS recovery and old-authority revocation. T-056 measures declared RPO/RTO and scale. A drill finishes only when a tenant can inspect recovered work, safely publish source, run a container, and retrieve evidence without duplicated external effects. Any missing object/commit/key is a visible unresolved recovery exception.

## Retention and offboarding

Stop admission, revoke access, settle active work, and apply scoped deletion across metadata, Git refs, artifacts, registry, logs, and previews. Preserve explicit backup expiry/key-retention windows in the deletion report. Never destroy a shared wrapping key to delete one tenant while retained data still requires it.

Git garbage collection and repository deletion are operational dependencies: retained refs must be protected/backed up for the promised recovery window. Artifact orphan cleanup is separate from deletion of referenced evidence.
