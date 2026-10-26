---
id: T-109
milestone: production
status: done
depends_on: []
decision_blockers: []
owns: [CONTEXT.md, docs/adr/, docs/specs/, docs/contracts/, docs/book/, docs/examples/, docs/traceability.md, docs/review-kiss.md, docs/implementation/step-entrypoint.md, docs/implementation/repository-layout.md, docs/operations/]
check: make docs-check
resources: []
acceptance: []
---
# T-109 — P0: shared run resource contract checkpoint

## Goal
Reconcile the authoritative contracts for the owner-scheduled [plan](../implementation/run-workspace-services-builders.md#p0--contract-checkpoint), before changing executable behavior.

## Contract
Intentional change from repository `7a66d3493f2bc69d197406f757841e603efbb0c1` and design `0b12febef5b9e092982af74e2c951afe2dd80c19`, ADR 0010/O-28. Read the glossary, platform boundary, affected owning specs and design-conformance procedure. Preserve Git checkpoint/CAS authority, historical IDs, old canonical bytes/results, tenant/credential isolation and explicit uncertainty. New format is `kodama/v1alpha2`; new context is versioned separately from unchanged result envelopes. Existing execution remains the old profile until migration/qualification.

## Deliverables
- Update glossary and workspace, execution, identity, credentials, delivery and software-design contracts, pipeline format, runtime/entrypoint explanations, ADR 0005/0008/0010, examples and traceability together. Retain historical rationale and label superseded per-attempt requirements by format, not silent ID reuse.
- Freeze source-backed/source-free admission defaults, service shapes/limits/probes/endpoint expressions/declared service-to-service edges, total run timeout, deterministic topological order with declaration tie-breaks and gates in order, serialized and fenced handoff, resource-loss and pre-command-only retry semantics.
- Freeze version compatibility/cutover and policy-pin semantics. Describe execution-owned companion resources (ensure/observe/logs/stop/remove), identity/fingerprint/UID and ready/unknown/stopped/removed evidence. Account for the current `uint8` guarantee capacity without moving provider types into domain contracts.
- Add/reserve WS-13–16, PX-17–22, IT-14–16, VS-14 and DQ-16 and associated acceptance IDs from the plan after collision checks. State which combinations still require real qualification; never claim supported builders from documentation.

## Acceptance
`make docs-check`; semantic review of ordering, authority, failure/cleanup/recovery, requirement-to-acceptance coverage, examples and backward compatibility. Record exact candidate and results. Existing v1alpha1 examples may remain as explicitly historical/current-format examples; add distinct new-format fixtures.

## Out of scope
Runtime/product changes, generated API/client, task metadata and operations qualification. P-06/P-07/P-11/P-14/O-07 register amendments are coordinator-owned and must be requested at handoff if needed. The coordinator owns plan progress and task index.

## Completion evidence
Contract checkpoint merged locally as `9db322ffd37a4279f8a7ac1a404a06e46533527d` after independent review of the corrected exact candidate (session `ses_e864038ceffeU5n6hbS8BZk6iM`, approve, zero findings) and coordinator `make docs-check` PASS (121 files). The canonical v1alpha2 reference vector was independently re-derived by the writer; existing v1 bytes/hashes and executable behavior were untouched. Review fixes froze resource-specific identity/token envelopes, image argv defaults, one-workload preparation retry, source-free scratch retention, begin/lineage provenance, mandatory network/default-deny capabilities and terminal cleanup. The finished worktree/branch were removed; nothing was pushed. Decision-register amendments are recorded by the coordinator. T-110 owns executable schema/parser fixtures; no runtime or builder qualification is claimed here.
