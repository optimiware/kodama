# 4. Lean flow and governance

Use a few observable mechanisms rather than a generic governance engine.

| Principle | Mechanism |
| --- | --- |
| Kanban | Explicit states, human pull, one active WIP limit per project |
| JIT | Start private execution compute when a step is eligible |
| Jidoka / autonomation | Stop dependent progression on failed evidence or unknown external outcome; expose a reason |
| Kaizen | Review SQL flow summaries and create an ordinary improvement work item |

Work-item WIP and runtime concurrency are independent. Blocked active items still count toward WIP. A pending gate can hold business WIP without consuming a Job. All admission/reopen/rework entries to active use the same transactional limit.

Humans decide business acceptance. One-reviewer pipeline gates protect explicit execution progression. No quorum/override/flow-policy language is needed initially. Authorized completion records current candidate/content and a reason, including acknowledged business findings.

M3 derives lead time, active time, blocked time, review time, and reopen intervals from work history using SQL views. Model/compute telemetry can be correlated with work without introducing a billing subsystem. Define observation windows and incomplete-item handling alongside reports.

Improvement work follows the same ordinary path: observe friction, create a task, change a pipeline/blueprint, apply the new immutable version, and compare results. Existing runs retain their pinned definitions. See [work management](../specs/work-management.md) and [roadmap](07-delivery-roadmap.md).
