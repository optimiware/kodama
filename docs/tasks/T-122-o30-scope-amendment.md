---
id: T-122
milestone: production
status: done
depends_on: [T-109, T-110, T-112]
decision_blockers: []
owns: [docs/decisions.md, docs/adr/0010-run-workspace-services-builders.md, docs/implementation/run-workspace-services-builders.md, docs/contracts/run-resources.md, docs/contracts/pipeline-format-v1alpha2.md, docs/specs/identity-tenancy-and-rbac.md, docs/specs/pipeline-execution.md, docs/examples/workflows/run-shared-build-and-services.md, CONTEXT.md, internal/execution/, docs/tasks/]
check: make check-go PKGS='./internal/execution/...'
resources: [go]
acceptance: [unit_network_model_selects_network_capabilities, unit_execution_policy_digest_roles_and_ceilings, unit_required_resource_capabilities_derive_from_the_document, unit_resource_capability_set_is_a_separate_named_set]
---
# T-122 — O-30 scope amendment: contracts and admission derivation

## Goal
Record the owner's [O-30](../decisions.md#revised-scope--2026-10-29-o-30) revision of ADR 0010 and change the merged execution code that encoded the superseded choices, before dependent implementation starts.

## Contract
ADR 0010 as amended, T-109/T-110/T-112 checkpoints. `attempt_private` behavior, v1 canonical bytes and result hashes are unchanged. The strict network model stays expressible and keeps its refusals.

## Deliverables
- Decision register (O-30), ADR amendment, run-resources and v1alpha2 contract notes, IT-14/IT-16/PX-17/PX-A13 and glossary updates, implementation-plan amendment with the frozen integration seams S1–S9.
- `ExecutionPolicy.network {isolation: tenant|run, publicEgress}` pinned in the policy body and copied into the `network` resource spec; `RequiredResourceCapabilities` adds `run_network_isolation`/`egress_default_deny` from that model; every `run_shared` run requires the `network_policy` guarantee.
- `AttemptSpec.Shared` (`SharedLaunch`: pinned step role, role digest, Pod Security pin, bound workspace handle, network model, step egress) set by execution before every `Start`; optional `RoleShape.procMount: unmasked` only with a Pod user namespace.
- Task graph: new T-123 (deferred BuildKit) and T-124 (driver update); revised T-113–T-117, T-120, T-121 dependencies, owned paths and pinned acceptance names.

## Acceptance
Declared scoped check (all execution packages, including the memory shared contract with the new O-30 and launch regressions) and `make docs-check`.

## Out of scope
Adapters, entrypoint, assembly, chart and any cluster change.

## Completion evidence
Independent review (`ses_e76a0c7a1ffevBC99N6wHBua9n`) requested changes on the first candidate (role-digest drift from nil→empty cloning; network text contradicting O-30; dispatch path untested), then approved the repaired candidate; its remaining minor test gap (first-dispatch assertion, legacy start required) was closed before merge. Coordinator checks on the final candidate: `make check-go PKGS='./internal/execution/...'` PASS with every pinned acceptance plus `unit_shared_launch_role_digest`; real-SQL `make check-go-integration DEV_SCOPE=t122 DEV_STACK=services PKGS='./internal/execution/...'` PASS with zero skips (`Test_shared_resource_contract` including the O-30 and launch subtests); `make docs-check` PASS. Squashed to one commit on `main`; nothing pushed.
