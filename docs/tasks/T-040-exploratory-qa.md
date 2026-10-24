---
id: T-040
milestone: M3
status: done
depends_on: [T-011, T-015, T-030, T-035]
decision_blockers: []
owns: [internal/qa/]
check: make check-go PKGS=./internal/qa/...
resources: [go]
---
# T-040 — QA evidence-to-defect mapping

## Goal
Map deployment-bound observations to ordinary defect work items with durable evidence.

## Contract
Consume the existing agent/runtime and work/artifact/deployment owner contracts. Bind target deployment/configuration and pinned acceptance context; use ordinary defect work items with artifact links. This mapping can be developed before full M2 acceptance; T-069 owns the real browser image/profile qualification afterwards.

## Deliverables
Structured observation/report contract, finding-to-defect mapping with dedup keys, screenshot/artifact linkage and deployment provenance. Export the mapping contribution and fixtures for T-069/T-046. Exercise real work/artifact/deployment adapters for persistence, deduplication, tenant denial and rollback; use fakes only for browser/model calls.

## Acceptance
`qa_wrong_deployment`, `qa_defect_retry`, `qa_evidence_links`, and `qa_cannot_approve` cover DQ-06/DQ-10, AR-05/AR-12, and WM-01.

These original names remain here for mapping/authority behavior. T-069 adds actual image/profile/browser scope and repeats the integrated evidence checks; component completion alone does not qualify exploratory QA.

## Out of scope
A separate findings service or letting the QA agent satisfy the human gate.
