---
id: T-065
milestone: M2
status: done
depends_on: [T-016, T-022, T-035, T-091]
decision_blockers: []
owns: [images/deployment-tools/, test/integration/deployment-tools/]
check: make check-go-integration PKGS=./test/integration/deployment-tools/...
resources: [go, integration]
acceptance: [deployment_tools_restricted, deployment_annotation_lookup, deployment_tools_outputs]
---
# T-065 — Ordinary deployment tools image and scripts

## Goal
Build the restricted ordinary-container tools contribution separated from T-035's records.

## Contract
Consume T-035's pre-apply/action-key/configuration/provenance fixtures. Namespace-scoped kubeconfig and stable deployment annotations enable lookup after response loss; command output alone never proves readiness. C-14/O-12 select ordinary scripts, not a deployment engine.

## Deliverables
Pinned reproducible tools image, example apply/lookup/delete scripts, explicit output contract and Go-driven integration tests executing those real scripts. Test real Kubernetes under the selected execution restrictions; use fixtures at factory callback boundaries. Record image build/script/cluster commands and results alongside the declared package checks.

## Acceptance
`deployment_tools_restricted`, `deployment_annotation_lookup`, and `deployment_tools_outputs` verify DQ-07–DQ-10: scoped apply/delete, annotation-bound lookup, verified image/source/config outputs and no privileged runtime requirement. T-067 owns the real record/apply acknowledgment-loss join.

## Out of scope
Delivery record tables, TTL scheduling, generic cloud adapters or selecting an additional provider.
