---
id: T-121
milestone: production
status: done
depends_on: [T-120, T-124]
decision_blockers: []
owns: [docs/acceptance/run-resource-profile.md, deploy/profiles/run-builders/, test/integration/kube/runprofile/pins.json]
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

## Completion evidence
Installed the T-120 node profiles on all four stage workers and ran `verify.sh` against the Scaleway stage. Verdict `qualified` (run `fbaeac`, exact candidate `cfcc0e3b`, cleanup verified); see the [report](../acceptance/run-resource-profile.md). Getting there needed two owner-visible changes: Buildah re-pinned twice (quay.io collects rebuilt digests), and, because AppArmor 4.0 cannot express the propagation flags Buildah's chroot isolation uses on its `/dev` remount, the owner selected (O-30 amendment, 2026-10-30) the versioned step profile `kodama-rootless-build-v2` (`flags=(unconfined)` + `userns,`), implemented under a bounded coordinator exception across the profiles, driver guard, chart policy and docs. Independent review (`ses_e74b90e49ffeePQofqu3D3gQLh`) approved that change; its minor documentation finding is fixed. Coordinator checks: `make check-go` over profiles, driver, kube, app and chart, `make test-chart`, `make docs-check` all pass. Nodes still carry the unreferenced confined v1 AppArmor profile until reboot. Squashed to one commit; nothing pushed.
