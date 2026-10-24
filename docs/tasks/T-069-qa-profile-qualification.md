---
id: T-069
milestone: M3
status: deferred
depends_on: [T-047]
decision_blockers: []
owns: [images/exploratory-qa/, profiles/exploratory-qa/, test/integration/qa/]
check: make check-go-integration PKGS=./test/integration/qa/...
resources: [go, integration]
---
# T-069 — Exploratory QA image/profile qualification

## Goal
Qualify a real browser-agent profile consuming T-040's evidence-to-defect mapping.

## Contract
Use the merged real harness/gateway/MCP/runtime path and a known deployment/configuration with pinned acceptance context. Browser-tool scope and report/screenshot provenance must survive retries. QA cannot satisfy the human gate.

## Deliverables
Pinned reproducible image/profile, browser-tool allow-list, actual exploratory run and evidence/defect qualification. Record image/script and Go-suite commands/results in isolated real deployment/browser scopes. T-046 owns process/profile registration and the final business-loop join.

## Acceptance
When scheduled, also own deferred `blueprint_factory_integration` from T-046: actual template build/preview/stage/evidence, including each scheduled blueprint. Coordinate T-047's Supabase scope before claiming both stacks qualified.

`qa_profile_real_run`, `qa_browser_scope`, and `qa_profile_evidence_join` verify AR-05/AR-12, DQ-06/DQ-10 and WM-01. Recheck T-040's `qa_wrong_deployment`, `qa_defect_retry`, `qa_evidence_links` and `qa_cannot_approve` through the real profile; screenshots and findings retain the correct deployment/configuration identity. T-044 retains full human completion acceptance.

## Out of scope
A findings service, automatic human approval, unqualified browser images or central app edits.
