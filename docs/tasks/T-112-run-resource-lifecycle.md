---
id: T-112
milestone: production
status: planned
depends_on: [T-110, T-119]
decision_blockers: []
owns: [internal/execution/, internal/app/migrate.go, internal/app/migrate_test.go, internal/app/m3_test.go]
check: make check-go PKGS='./internal/execution/... ./internal/app/...' && flock /tmp/opencode/kodama-run-resources-integration.lock make check-go-integration DEV_SCOPE=t112 DEV_STACK=services PKGS='./contracts/events/v1/... ./internal/execution/...'
resources: [go, integration]
acceptance: [unit_run_resources_marker_uses_one_spare_bit, unit_resource_capability_set_is_a_separate_named_set, unit_resource_names_endpoints_and_fingerprint_are_frozen, unit_uid_binding_never_adopts_a_foreign_instance, unit_preparation_receipt_digest_object_is_frozen, unit_execution_policy_digest_roles_and_ceilings, unit_required_resource_capabilities_derive_from_the_document, unit_shared_pins_resolve_endpoints_access_order_and_capacity, unit_run_resource_state_machine, unit_capacity_units_fit_every_quantity, unit_execution_order_gate_occupies_its_position, unit_resource_audit_id_satisfies_the_audit_identifier_domain, unit_pipeline_v2_step_codec_round_trip, shared_resource_contract, shared_forced_rls_ownership_and_guards, shared_start_is_atomic_with_its_effects, shared_concurrent_starts_never_exceed_capacity, shared_migration_is_additive]
---
# T-112 — P2: run-resource lifecycle and persistence

## Goal
Implement [P2](../implementation/run-workspace-services-builders.md#p2--run-resource-lifecycle-and-persistence), independently of P1 after the contract checkpoint. Freeze the execution-owned companion seam before adapter work.

## Contract
ADR 0010/O-28, normative T-109 checkpoint `9db322ffd37a4279f8a7ac1a404a06e46533527d` and executable T-110 checkpoint `6343b9e5f75b20e41e28b62b418069bb1811950f`. Keep the existing six-method attempt runtime port; add the reviewed `run_resources` marker without overflowing the existing six-used-bit `uint8`, and keep finer resource capabilities in a separate named set. Optional launch-history proof stays separate. Provider types remain adapter-private. Preserve quota/work → run/attempt → authority → workspace → event lock order and audit/outbox atomicity.

The reviewed event/identity clarification checkpoint is T-119 at `b75dca7411bdc556f84ab7b674ce5d2f81da8841`: consume `eventsv1.RunResourceStateChanged` and `RunResourceID(tenant, project, run, key)` for domain-valid scoped audit/event IDs. Same-phase intent/evidence/reason updates are valid. The owning contract now fixes the 4-byte big-endian framing; no underscore-flattened identity remains. These are dependencies for the held candidate's repair, not a claim that its missing producer or lifecycle findings are resolved.

## Deliverables
- Run-resource DTOs/ensure-observe-logs-stop-remove port and memory/PostgreSQL persistence of pre-effect intent, immutable scope/backend/policy/spec identity, UID, preparation/readiness, deadlines, stop/removal evidence and safe reasons; no plaintext/scratch persistence.
- Transactional ordered workspace-holder admission separate from logical writer admission; readiness/preparation barrier; reserve service/active-attempt/PVC capacity atomically and release only after verified settlement.
- Restart adoption, duplicate/reordered wakeups, cancel/create races, quota safety, terminal/orphan cleanup and run deadline including gates; no silent scratch/service reconstruction. Pre-command-only automatic retries and explicit unknown-effect barriers.
- Safe observation/API contributions without new business run enums or fake attempts/checkpoints. Keep new-format admission closed unless the selected runtime declares the complete required companion capabilities.
- Freeze the exact preparation-receipt digest object and verify its workspace UID binding. Extend the frozen step codec with round-trip evidence before allowing any new-format admission; replace T-110's deliberate v1-only codec regression in that same change. Pure runtime-v2 derivations do not replace persisted command-begin/cancellation/replay guards.
- Implement source-free shared cleanup admission/lifecycle as well as source-backed work runs under the explicit complete capability/policy gate. Deployment authorization remains mandatory; no work-item/source lookup, preparation workload or Git credential exists for source-free runs. Default/unconfigured composition still refuses both new-format paths.
- Consume T-119's resource event contract and append every resource transition to history, audit and transactional outbox together in memory and PostgreSQL. Resource-only outbox refusal must roll back the whole transition; do not defer this requirement to P6.
- Ordered integration-owner exception: append `RunResourceMigrations()` after the last existing application migration contribution in the two explicitly owned app files, with a manifest regression. The changed PostgreSQL store unconditionally reads new columns, so delaying this additive migration until P6 breaks legacy runs. This exception enables no new route, backend, policy or shared-run admission; all broader assembly remains with T-116. Seed real legacy runs in upgrade tests, not an empty table.
- The integration owner also authorizes the bounded `internal/app/m3_test.go` assertion update: preserve the old migration tail in order and append only `execution-run-resources`. This aligns the existing M3 expectation with the same additive migration; it does not authorize broader app changes or weaker ordering checks.

## Acceptance
The declared check requires scoped units including the bounded app migration join, then the real service-backed check across the event contract and all execution packages; its final results contain the pinned unit and SQL acceptance names. Prove actual RLS/rollback/races, unknown launch, cleanup restart and existing writer/late-result regressions. Unit evidence alone cannot satisfy real SQL claims. `TestUnitSharedResourceContract` must also pass on memory, and SQL shared acceptance must not skip.

Hold the single manual integration lease `/tmp/opencode/kodama-run-resources-integration.lock` around the real SQL check, serialized with T-111 qualification. This does not prevent parallel unit work. Orchestrator owns scope teardown; never tear down another task's resources.

## Out of scope
Provider adapters, authority/publication changes owned by T-113, public API fragments/client generation, broader assembly and deployment. T-116 owns the `api/execution.yaml` integration handoff together with aggregate/client generation.

## Blocking review handoff
Candidate `503083853da63269e48d8985870e0b9debf826d1` is not approved or merged. Independent lifecycle review (`ses_e85e4b966ffe0osulKCMt16Lwf`) and persistence review (`ses_e85e4b891ffe3XrWO1W3fIA0MU`) identified collision-as-cleanup, begin replay after revocation/deadline, stale/reordered readiness, transactional holder-release race, result/run-deadline omission, redrive capability/barrier omission, policy-loss cleanup, delayed service stop after nonretryable begun failure, unsolicited stopped-resource loss, attempt-slot reservations, gate order, missing source-free admission/outbox, legacy migration wiring and unequal memory/SQL immutability guards. Each requires a deterministic negative regression in the appropriate shared/SQL suite before renewed approval. T-119 freezes the missing event contract and identity encoding first; retain the existing task worktree for repair.
