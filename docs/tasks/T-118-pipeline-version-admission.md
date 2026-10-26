---
id: T-118
milestone: production
status: planned
depends_on: []
decision_blockers: []
owns: [internal/execution/admission.go, internal/execution/pipeline_version_test.go]
check: make check-go PKGS='./internal/execution/...'
resources: [go]
acceptance: [unit_pipeline_version_guard]
---
# T-118 — Fail-closed pipeline-version admission prerequisite

## Goal
Before shared parsing learns new document versions, explicitly limit the current executor to its implemented `kodama/v1alpha1` semantics. This bounded safety prerequisite can proceed alongside T-109 because it preserves, rather than changes, the current authoritative contract.

## Contract
Executable baseline `7a66d3493f2bc69d197406f757841e603efbb0c1`; ADR 0008/C-21 capability refusal, current pipeline format and PX admission invariants. O-28 schedules new semantics but does not authorize the old executor to reinterpret them. Registration permissions and historical readability are not execution permissions.

## Deliverables
- A narrow, fail-closed definition-version check in the shared admission path used by ordinary and source-free cleanup preparation. Accept exactly the implemented current format; refuse empty, unknown and future versions with a stable classified, sensitive-value-free error before any run/resource side effect.
- Unit test `Test_unit_pipeline_version_guard` covering current/empty/future/unknown version decisions through a small admission-owned helper where needed. Preserve all existing successful v1 admission and cleanup behavior. Do not expose a permissive production configuration or change runtime guarantees.
- T-110 later tests real v2 definitions once its parser makes them constructible; T-112 owns any eventual supported new-format admission.

## Acceptance
`make check-go PKGS='./internal/execution/...'` passes including `Test_unit_pipeline_version_guard` and existing ordinary/cleanup admission tests. Record exact candidate, commands/results and the shared call path used by both admission variants. No new parser support is claimed by this prerequisite.

## Out of scope
New-format contracts/parser, run resources, authority/publication changes, API/client, task metadata, other deferred work and deployment.
