---
id: T-125
milestone: production
status: done
depends_on: []
decision_blockers: []
owns: [CONTEXT.md, docs/README.md, docs/adr/, docs/specs/, docs/contracts/, docs/book/, docs/examples/, docs/decisions.md, docs/traceability.md, docs/implementation/internal-git-service.md, docs/implementation/repository-layout.md, docs/implementation/database-schema.md, docs/tasks/]
check: make docs-check
resources: []
acceptance: []
---
# T-125 — Internal Git service: documentation and ADR checkpoint

## Goal
Record the owner's [O-31](../decisions.md#internal-git-service--2026-10-30-o-31) direction (Kodama hosts its projects' repositories with native Git, tenant isolation from the first release) as one reviewed architecture before any dependent implementation starts.

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md); new [Git repositories](../specs/git-repositories.md) specification (GR) and [Git service contract](../contracts/git-service.md). Preserve historical rationale and existing requirement IDs; amend O-03 and ADRs 0004/0005 explicitly; keep the current external-host model authoritative for existing repositories and admitted runs.

## Deliverables
- ADR 0011, decision register (O-31, P-17, open O-32/O-33, O-03 amendment), ADR index and amendments to ADRs 0004/0005.
- Glossary, vision, domain and architecture chapters, roadmap.
- GR specification; amendments to workspaces (WS-17–WS-19), pipeline execution (PX-23–PX-25), identity (IT-17–IT-19), interfaces (AC-13–AC-15), delivery (DQ-17), agent runtime (AR-15) and software design.
- Git service contract; HTTP routes and event types; contract index.
- Implementation plan, package/seam ownership, schema sketch, examples, traceability.
- Task records T-126–T-133 and the T-117 dependency on this checkpoint.

## Acceptance
`make docs-check` passes; independent review approves the candidate. Requirement-to-acceptance mappings and owned-path ordering are checked by review until `bun ../openforge/src/cli.ts validate` can run.

## Out of scope
Code, schemas, operations runbooks (T-132) and status claims (T-133).

## Completion evidence
Independent review (`ses_e752bfd28ffeLRTpvP54wvFvaL`) requested changes twice: first for push reconciliation without receive attribution or fencing, parallel agents inside one run contradicting ADR 0010, unowned multi-repository delivery and API fragments, publisher retirement while external hosting remains, O-33 not reflected in readiness and P-07's persistent-volume rule; then for an ABA ordering flaw in prepared-only settlement (outcome entries are written after lock release). Both were repaired (two-phase `reference-transaction` fence with a receive-attributed ref journal; settlement from `prepared` order plus a locked read; run-scoped agent branches; ownership reassignment; explicit push policy) and the third candidate was approved. Coordinator checks on the final candidate: `make docs-check` PASS (143 files); a task-graph script found no missing dependencies, cycles or unordered owned-path overlaps; every new requirement is cited by an acceptance scenario. `bun ../openforge/src/cli.ts validate` was unavailable (no OpenForge checkout). Squashed to one commit on `main`; nothing pushed.
