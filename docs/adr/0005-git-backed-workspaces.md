---
status: accepted
---

# Git-backed workspaces with controlled publication

The KISS review replaces volume checkpoints and remote editor sessions with Git commits and ordinary human Git workflows. Each attempt uses private scratch; a workspace branch and retained refs preserve source continuity. This keeps the logical work-item/workspace pairing while removing a custom checkpoint/session subsystem.

Only the control-plane publisher receives canonical-branch write credentials. It checks active attempt ownership and performs expected-tip compare-and-swap. This prevents stale/partitioned attempts from overwriting accepted work; Pod deletion alone cannot establish that guarantee. Uncommitted scratch work is explicitly outside durable recovery.

Use normal `refs/heads/kodama/work/<id>` branches for host portability. Retained refs and backup/mirroring policy keep pinned commits reachable. A temporary Git bundle may transport changes from a workload to the publisher; Git remains the durable checkpoint authority.

## Amendment — run-shared scratch (ADR 0010, 2026-10-26)

[ADR 0010](0010-run-workspace-services-builders.md) amends only the *private scratch per attempt* requirement, and only for the versioned `run_shared` model: one disposable scratch workspace per run, prepared once, mounted by one attempt at a time and fenced by verified stop, with cumulative edits. The rationale above is retained in full: Git commits/refs remain the only durable checkpoint authority, only the control-plane publisher holds branch-write credentials, publication stays expected-tip CAS with ancestry validation, Pod deletion or an expired claim is not a fence, and uncommitted scratch is outside durable recovery (scratch survival is never a checkpoint). Private scratch per attempt (WS-05 `[attempt_private]`) remains the executable contract for `kodama/v1alpha1` runs until the documented cutover; requirement IDs are labeled by model, not renumbered. Contract: [workspaces](../specs/workspaces.md#run-scratch-run_shared).

## Amendment — internal Git hosting (ADR 0011, 2026-10-30)

[ADR 0011](0011-internal-tenant-git-service.md) amends *publisher-only write authority* and *temporary bundle transport* for **internally hosted repositories** only. There, an attempt pushes its own assigned branches with a short-lived grant that the Git service authorizes per ref against current authority at `pre-receive`; Git's expected-old-value check is the compare-and-swap; a journaled push record replaces the publisher's pending-publication fields; integration grants alone advance the protected default branch. Retained: Git commits are the only durable checkpoint, stale or revoked attempts cannot write, human pushes conflict instead of being overwritten, unknown outcomes block writer reassignment, and pinned commits stay reachable through retained refs. Because Kodama controls the host, those retained refs use `refs/kodama/runs/*`. External repositories keep this ADR's original decision unchanged. Contract: [Git repositories](../specs/git-repositories.md).
