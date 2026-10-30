---
id: T-166
milestone: production
status: done
depends_on: []
decision_blockers: []
owns: [docs/specs/git-repositories.md, docs/specs/workspaces.md, docs/contracts/git-service.md, docs/contracts/event-catalog.md, docs/contracts/README.md, docs/implementation/internal-git-service.md, docs/decisions.md, docs/tasks/]
check: make validate-tasks && make docs-check
resources: []
acceptance: []
---
# T-166 — Kernel Git contracts and backlog revision

## Goal
Align the Git specification, contract and plan with the ADR 0013 kernel, record the owner's revised finish line (O-36), and replace the superseded assembly backlog with T-166–T-170.

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md) as simplified by [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md), O-31, O-32, O-33, O-36.

## Deliverables
- [Git repositories](../specs/git-repositories.md): kernel model, Gerrit hierarchy over existing projects, bootstrap by push, current-authority ref policy, receive-derived push event, pipeline indexing, integration through the configured integration pipeline, knowledge base, journal-ack durability.
- [Git service contract](../contracts/git-service.md): repository routes, `/git/` endpoint and forwarding assertion, credential exchange and grant claims, worker routes that this slice implements, push event payload with inline updates, source set.
- [Event catalog](../contracts/event-catalog.md): `git.refs_updated` carries the applied updates inline and is emitted once per receive.
- [Implementation plan](../implementation/internal-git-service.md): five phases, one appended `003_git_hosting.sql` contribution, retained and superseded work, deferred scope.
- [Decision register](../decisions.md): O-36 with the revised finish line and the superseded task scopes.
- Task records T-166–T-170 and the dependency index; the superseded planned tasks become `deferred`.

## Acceptance
Declared check.

## Out of scope
Code, migrations, routes and installation.

## Completion evidence
Owner approved the plan in conversation on 2026-10-30 (O-36). Coordinator checks: `make validate-tasks` and `make docs-check` PASS. Merged into `integration/native-git`; nothing pushed.
