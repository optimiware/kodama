---
id: T-037
milestone: M2
status: done
depends_on: [T-025, T-035]
decision_blockers: []
owns: [internal/cli/environment/, internal/cli/deployment/]
check: make check-go PKGS="./internal/cli/environment/... ./internal/cli/deployment/..."
resources: [go]
acceptance: [deployment_scope, deployment_cli_retry]
---
# T-037 — Environment and deployment CLI

## Goal
Expose environment/configuration and deployment operations through public interfaces.

## Contract
Consume module OpenAPI fragments and existing client/output conventions. Cleanup targets recorded deployments; commands export registration contributions for T-072. Profile and evidence-bound gate commands are owned by T-068.

## Deliverables
Environment/configuration commands, deployment read/create/cleanup, JSON/error/replay fixtures. No root command or aggregate client edits.

## Acceptance
`deployment_scope` and `deployment_cli_retry` verify AC-01/AC-02/AC-05 and DQ-09 through public HTTP command tests. The original stale_gate_cli, decision_retry and json_review_evidence obligations move to T-068 (DQ-03/DQ-04); T-039 retains the complete CLI join.

## Out of scope
Client-side authorization shortcuts or implicit approval from viewing a report.
