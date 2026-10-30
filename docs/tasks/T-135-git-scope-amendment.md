---
id: T-135
milestone: production
status: done
depends_on: [T-125, T-165]
decision_blockers: []
owns: [docs/decisions.md, docs/adr/0011-internal-tenant-git-service.md, docs/specs/git-repositories.md, docs/contracts/git-service.md, docs/implementation/internal-git-service.md, docs/examples/workflows/internal-git-task-to-deploy.md, docs/book/07-delivery-roadmap.md, docs/tasks/]
check: make docs-check validate-tasks
resources: [go]
acceptance: []
---
# T-135 — Internal Git service: O-32/O-33 scope and executable task graph

## Goal
Record the owner's resolution of O-32 (no provider import or mirroring; bootstrap by an engineer's push under a single-use seed grant) and O-33 (default branch protected by default), and replace T-126–T-133 with an executable graph of disjoint, ordered tasks for subagents merged into `integration/native-git`.

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md). GR-17/GR-A12 are deferred, not renumbered; new GR-18–GR-20 and GR-A14–GR-A15.

## Deliverables
- Decision register (O-32, O-33 resolved; P-18), ADR 0011 consequences, GR bootstrap section and requirements, contract seed grant and default policy, example, roadmap.
- Implementation plan: task graph, migration numbering, integration workflow, installation transition and recovery ownership.
- Task records T-126–T-139, T-147–T-158 and the dependency index.

## Acceptance
`make docs-check` and `make validate-tasks` pass; independent review approves.

## Out of scope
Code.

## Completion evidence
Independent review requested changes once (non-atomic multi-ref seeds, T-158 lacking `internal/app/apispec/`, T-153's wrapper tests not executed by its check) and approved after the fixes, with one minor example fix applied. Coordinator checks after rebasing onto T-165: `make docs-check` PASS (161 files), `make validate-tasks` PASS (64 tasks). Merged into `integration/native-git`.
