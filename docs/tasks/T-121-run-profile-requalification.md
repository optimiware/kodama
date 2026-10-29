---
id: T-121
milestone: production
status: planned
depends_on: [T-120, T-124]
decision_blockers: []
owns: [docs/acceptance/run-resource-profile.md]
check: test/integration/kube/runprofile/verify.sh
resources: [go, integration]
acceptance: [run_resource_profile_qualification]
---
# T-121 — Install and qualify the stage run-resource profile

## Goal
Install the T-120 profiles on the Scaleway stage workers and run the T-124 driver there, completing P1 for the O-30 profile.

## Contract
O-30. Obtain the stage kubeconfig through the stage OpenTofu outputs; keep credentials out of receipts. Keep `Test_run_resource_profile_qualification` a real acceptance: no skip, mock or `--version` substitute.

## Deliverables
- Install the profiles with the T-120 installer and record the installation receipt.
- Run `verify.sh` against stage: RWOP/idmapped multi-image sharing and reclaim, ko and Buildah build and push under the `step` role, PostgreSQL readiness and round trip, the tenant network model with live controls, verified cleanup.
- Update the report and support matrix only for combinations that passed; anything blocked stays blocked.

## Acceptance
The declared check reports `Test_run_resource_profile_qualification` passed with no required row skipped.

## Out of scope
Assembled-product acceptance (T-117), BuildKit (T-123), browser.
