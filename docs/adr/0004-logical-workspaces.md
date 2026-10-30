---
status: accepted
---

# Durable logical workspaces paired with actionable work items

Human and agent work must preserve context across retries, review, and rework. Give each actionable work item one primary logical workspace whose identity and checkpoints outlive its compute sessions. The owner selected this pairing over a fresh workspace for every run or a shared project-wide working directory.

## Original consequences

The storage/session details in this section are historical; ADR 0005 supplies the current implementation direction.

Workspace identity, compute availability, and write ownership need separate records. Recovery must distinguish confirmed durable checkpoints from unsaved working state. The proposed initial implementation permits one writer and gives concurrent readers immutable checkpoints; parallel writers require isolated working copies.

The logical pairing remains accepted. [ADR 0005](0005-git-backed-workspaces.md) supersedes the initial storage/session proposal with Git commits, private clones, and controlled publication.

A failed run does not destroy the workspace. Archival, retention, and deletion are explicit lifecycle operations. Epics group work and do not receive primary workspaces.

## Amendment — project repositories (ADR 0011, 2026-10-30)

[ADR 0011](0011-internal-tenant-git-service.md) makes repositories project resources and lets a project own several. The pairing kept here becomes one task branch per actionable work item in each repository the work touches, plus agent branches beneath it; the workspace record holds those associations rather than a single repository URL. A repository exists without any work item, and a pipeline started by a developer push needs no work item. Epics still receive no task branch.
