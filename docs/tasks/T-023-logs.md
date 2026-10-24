---
id: T-023
milestone: M1
status: done
depends_on: [T-015, T-019, T-020]
decision_blockers: []
owns: [internal/logs/, api/logs.yaml]
check: make check-go PKGS=./internal/logs/...
resources: [go]
---
# T-023 — Scoped log service and retention contract

## Goal
Make useful logs available without requiring Loki.

## Contract
Follow [observability](../operations/observability-and-audit.md): authorize an attempt, construct Kubernetes query server-side, bound output, and retain log artifacts through wrapper/controller fallback.

## Deliverables
Live/tail API, log artifact linkage/completeness metadata, server-side controller fallback, safe redaction and error handling. Export a constructor/routes and a narrow Kubernetes log-reader port; test the real reader and artifact metadata adapter in isolated scopes. Use frozen attempt/runtime contracts rather than waiting for the full executor.

T-063 owns entrypoint upload/call-site integration; T-064 owns the real wrapper/controller fallback join. This task freezes upload/link/completeness fixtures for both. Root wiring belongs to T-071.

## Acceptance
`foreign_log_selector` and `bounded_tail` cover IT-03/IT-11, AC-06 and VS-11, using real authorization/persistence and bounded streaming checks where required. The original wrapper_log_upload obligation moves to T-063 and missing_node_tail to T-064 (PX-10); neither is satisfied by this component alone.

## Out of scope
Mandatory Loki, claiming logs are guaranteed complete after node loss, or arbitrary label-selector access.
