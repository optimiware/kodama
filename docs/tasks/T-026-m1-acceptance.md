---
id: T-026
milestone: M1
status: done
depends_on: [T-027, T-091, T-094]
decision_blockers: []
owns: [test/e2e/m1/, test/integration/assembly-execution/, docs/acceptance/m1.md, openforge.json]
check: make check-go-integration PKGS="./test/e2e/m1/... ./test/integration/assembly-execution/..."
resources: [go, integration]
acceptance: [m1_tracer, m1_restart, m1_cancel_race, m1_writer_cas, m1_duplicate_event_job, m1_tenant_matrix, execution_assembly_roundtrip, execution_assembly_cancel_restart, execution_assembly_result_guard, m1_client_roundtrip]
---
# T-026 — M1 integrated acceptance

## Goal
Prove the real foundation path, not just module fakes.

## Contract
Use the local CLI/API in the `test` environment ([ADR 0009](../adr/0009-environments.md)): `DEV_STACK=services`, the Podman runtime and real PostgreSQL/JetStream/Git/S3, with no cluster. The installed chart, kind and the Kubernetes runtime are T-089. Preflight the scope and record versions/fixture provenance. No production/cloud or live-model qualification.

## Deliverables
Create-work/source/run/report/log/restart e2e suite, failure injection, tenant-denial matrix, reproducible acceptance report and known limitations. Run every `Contract` suite from T-087/T-088/T-080/T-081 against the real PostgreSQL adapters and record the results beside the unit runs.

Own `execution_assembly_roundtrip`, `execution_assembly_cancel_restart`, and `execution_assembly_result_guard` transferred from T-071: actual callbacks, publication, revocation, rollback and replay. Once green, add bounded M1 suites to `mainCheck` in `openforge.json` alongside `make lint-go` / `make test-go-unit`. Keep image/template sources out of host core commands. Reuse prepared scope `batch`; OpenForge preflights and loads host endpoints rather than rebuilding per package. Tooling tests remain change-selected.

## Acceptance
`m1_client_roundtrip` verifies T-027's generated client against the actual server: run submission/status, identical-key replay, conflicting-key/body error and stable error decoding (AC-01/AC-02/AC-10/AC-12).

`m1_tracer`, `m1_restart`, `m1_cancel_race`, `m1_writer_cas`, `m1_duplicate_event_job`, and `m1_tenant_matrix` verify PX-A02–PX-A07 on the Podman runtime, WS-A01–WS-A08, IT-A01–IT-A05, and AC-A01–AC-A04. IT-A06 and the Kubernetes forms of PX-A02 and PX-A06 transfer to T-089 (`kube_restricted_baseline`, `kube_m1_roundtrip`, `kube_cancel_restart`).

## Out of scope
Production KMS or untrusted-hosted claims. Product bugs return to an explicitly assigned owner before acceptance passes.
