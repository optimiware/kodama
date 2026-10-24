---
status: accepted
---

# Git-backed workspaces with controlled publication

The KISS review replaces volume checkpoints and remote editor sessions with Git commits and ordinary human Git workflows. Each attempt uses private scratch; a workspace branch and retained refs preserve source continuity. This keeps the logical work-item/workspace pairing while removing a custom checkpoint/session subsystem.

Only the control-plane publisher receives canonical-branch write credentials. It checks active attempt ownership and performs expected-tip compare-and-swap. This prevents stale/partitioned attempts from overwriting accepted work; Pod deletion alone cannot establish that guarantee. Uncommitted scratch work is explicitly outside durable recovery.

Use normal `refs/heads/kodama/work/<id>` branches for host portability. Retained refs and backup/mirroring policy keep pinned commits reachable. A temporary Git bundle may transport changes from a workload to the publisher; Git remains the durable checkpoint authority.
