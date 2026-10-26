---
id: T-116
milestone: production
status: planned
depends_on: [T-113, T-114, T-115]
decision_blockers: []
owns: [internal/app/, internal/cli/, api/openapi.yaml, api/client/, deploy/helm/kodama/, go.mod, go.sum, docs/operations/, docs/implementation/assembly-status.md]
check: make check-go PKGS='./internal/app/... ./internal/cli/... ./api/client/... ./deploy/helm/kodama/...'
resources: [go]
acceptance: []
---
# T-116 — P6: resource assembly, chart and migration

## Goal
Implement [P6](../implementation/run-workspace-services-builders.md#p6--assembly-chart-and-migration) as the sole shared-file integration owner.

## Contract
ADR 0010/O-28 and immutable prerequisites. Production refuses Process/Podman/memory/keyfile fallbacks. Keep old records/hashes and drain old runs under their pinned policy; an older controller must refuse new-format runs. Never advertise unqualified builder combinations.

## Deliverables
- Wire resource ports/stores/auth/preparation/logs/workers/migrations explicitly per environment, freeze additive migration ordering and expose safe authorized diagnostics/CLI.
- Render qualified policy/PSA pin/LSM/admission/storage/UID/network/quotas/deadlines/BuildKit mode and preflight actual capabilities. Replace overlapping broad allow policies without weakening independent control-plane restrictions.
- Regenerate aggregate OpenAPI/client from module fragments and verify compatibility. Enable new-format admission only with the complete qualified selected profile.
- Document deadlines/gate costs/cleanup/offboarding/uninstall, old-version drain/refusal/cutover/rollback and exact remaining P7 qualification.

## Acceptance
Scoped check, `make test-chart`, `make api-check`, `make test-m1 test-m2 test-m3`; migration baseline/new upgrade fixtures and composed resource flow. Pin real names/commands before review. Documentation and wiring are not real deployment evidence.

## Out of scope
Other deferred release/hosted/TLS work and unapproved stage installation mutation; T-117 owns integrated rollout.
