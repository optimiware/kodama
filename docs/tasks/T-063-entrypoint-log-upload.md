---
id: T-063
milestone: M1
status: done
depends_on: [T-016, T-023]
decision_blockers: []
owns: [internal/entrypoint/]
check: make check-go PKGS=./internal/entrypoint/...
resources: [go]
---
# T-063 — Entrypoint retained-log upload

## Goal
Complete T-023's wrapper upload obligation without mixing entrypoint edits into the log service.

## Contract
Consume T-023's artifact/link/completeness fixtures and existing runtime admission/upload contracts. Bounded log collection and redaction must preserve command cancellation, exit/result ordering and diagnostic handling.

## Deliverables
Log-upload extension and its actual entrypoint call sites, digest/size/completeness metadata, duplicate/lost-response handling and tests. Ownership includes the entrypoint package because an unused helper would not satisfy this task; limit edits to log collection/upload integration. Export the contribution for T-071; T-064 qualifies the real wrapper/controller join.

## Acceptance
`wrapper_log_upload`, `log_upload_retry`, and `log_upload_interruption` cover AC-06, PX-10 and VS-11. Check bounded/redacted bytes, repeated upload identity and partial collection under cancellation; rerun the existing entrypoint source/result/process tests. Real storage/stream semantics required by the adapter must use isolated scopes; T-064 supplies the real Pod lifecycle join.

## Out of scope
Log service routes, root CLI wiring, guaranteed completeness after node loss, or unrelated entrypoint refactoring.
