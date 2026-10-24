---
id: T-064
milestone: M1
status: done
depends_on: [T-021, T-022, T-023, T-063]
decision_blockers: []
owns: [test/integration/logs/]
check: make check-go-integration PKGS=./test/integration/logs/...
resources: [go, integration]
---
# T-064 — Live/retained log qualification

## Goal
Prove the log service, entrypoint upload and controller fallback cooperate on the real Pod/artifact path.

## Contract
Consume merged T-021/T-022/T-023/T-063 constructors and fixtures in a local test composition. Product root wiring remains T-071's. Preserve T-023's original missing-node and authorization obligations.

## Deliverables
Isolated kind/PostgreSQL/S3 qualification suite for live/tail access, wrapper retention, failed upload/controller fallback, duplicate linkage, redaction and completeness. Record resource scopes and actual commands/results.

## Acceptance
`missing_node_tail`, `retained_log_join`, and `log_cross_tenant_join` cover PX-10, AC-06, IT-03/IT-11 and VS-11. A missing node produces explicit incomplete evidence, not fabricated complete logs; foreign attempts/selectors remain denied across live and retained paths. Exercise T-063's real wrapper upload and bounded tail after restart.

## Out of scope
Production Loki qualification, root composition edits or marking M1 accepted.
