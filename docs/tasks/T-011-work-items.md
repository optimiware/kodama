---
id: T-011
milestone: M1
status: done
depends_on: [T-004, T-005]
decision_blockers: []
owns: [internal/work/, api/work.yaml]
---
# T-011 — Work items, history, and WIP

## Goal
Implement the small work-management model.

## Contract
Own [work management](../specs/work-management.md): spec/acceptance content revision, versioned edits/transitions, blocked reason, human completion, one active WIP limit. Inject authorization and writer/dependency checks through narrow consumer interfaces.

## Deliverables
Tables/queries/routes/OpenAPI, history/evidence links, transactional WIP admission, cancellation request integration seam, deny-by-default completion.

## Acceptance
`concurrent_spec_edit`, `last_wip_slot`, `reopen_wip`, `human_completion`, and `cancel_no_new_work` cover WM-01–WM-04 and WM-06–WM-11. Real DB tests establish contention semantics.

## Out of scope
M3 dependency graph, separate blocker/finding resources, and configurable completion policies.
