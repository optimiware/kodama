---
id: T-117
milestone: production
status: deferred
depends_on: [T-116, T-121, T-125]
decision_blockers: []
owns: [test/integration/assembly-execution/, test/integration/kube/stage/, docs/acceptance/run-resources.md, docs/status.md, docs/traceability.md]
check: test/integration/kube/stage/verify-run-shared.sh
resources: [go, integration]
acceptance: [stage_run_shared_build_and_services, stage_run_shared_faults_and_cleanup, stage_run_shared_tenant_network, stage_run_shared_legacy_drain]
---
# T-117 — P7: stage acceptance and cutover

## Goal
Accept the O-30 milestone against the assembled product on the Scaleway stage cluster, using the merged Kodama image and chart.

## Contract
ADR 0010 as amended by O-30, the qualified T-121 profile. Obtain the stage kubeconfig through the stage OpenTofu outputs; stage credentials and values never enter reports. Reuse T-121 evidence with explicit candidate/profile binding.

## Deliverables
- A source-backed `kodama/v1alpha2` run across different images: generated/untracked/ignored files persist, ko and Buildah build and push, a PostgreSQL service round trip, a writer publishes with expected-tip CAS; a source-free run.
- Faults: preparation failure, service readiness timeout and loss, pre-command retry vs post-begin refusal, cancellation, deadline during a gate wait, controller restart adopting intact resources; every resource verified removed.
- Tenant network model: same-tenant reachability, foreign tenant/platform/metadata denial, public egress.
- Cutover: `legacyFormat: drain` refuses new `v1alpha1` runs while history stays readable; status/traceability updated only for what passed.

## Acceptance
Declared real check with every named test passing on stage, retained receipts and cleanup inventories.

## Out of scope
BuildKit (T-123), browser, hosted/untrusted isolation and release claims.

## Parked (2026-10-30): resume checklist
The owner parked this task until stage access is provisioned on the operator machine. Its implementation dependencies are merged (T-113–T-116, T-120, T-122, T-124) and the stage profile is qualified (T-121). Reschedule it (`status: planned`) when the prerequisites below hold; nothing in the suite exists yet.

**State on the stage cluster at parking time**
- The node-profile installer DaemonSet runs in `kodama-node-profiles` on all four workers with seccomp `kodama-rootless-build-v1` and AppArmor `kodama-rootless-build-v2` loaded ([runbook](../operations/run-builder-profile.md)). The unreferenced confined v1 AppArmor profile stays loaded until a node reboot.
- Kodama still runs image `0.0.10` with chart `run_shared` disabled; tenant namespace `kodama-tenant-stage` enforces Pod Security `restricted`. No migration `005` has been applied there.

**Prerequisites (operator)**
1. Stage access on the machine running the suite: the stage kubeconfig, and either the stage OpenTofu state (`STAGE_STATE_DIR` for `deploy/stage/scripts/deploy.sh`) or an explicit owner authorization for a direct `helm upgrade` of the `kodama` release (which leaves OpenTofu's view stale until the next apply).
2. Push access to the Scaleway stage registry (`podman login rg.fr-par.scw.cloud`), or an operator-published image. Stage images are built from a clean candidate with the commit label and pushed by the operator ([stage README](../../deploy/stage/README.md)); the GHCR image built on push to `main` is not what stage pulls.
3. Pushed `main` containing at least T-115/T-121 (`origin/main` had `74096e26` at parking time).
4. A mirror of the pinned builder images (Buildah at least) in an installation registry: quay.io collected two pinned Buildah index digests within a day.

**Steps**
1. Build and push the Kodama image for the candidate; record its digest.
2. Follow the [cutover order](../operations/run-shared.md#order-of-operations-cutover) with the [stage values](../operations/run-shared.md#the-stage-values): drain active `kodama/v1alpha1` runs; `kodama migrate` (migration `005`); upgrade with `runShared.enabled=true`, `nodeProfilesInstalled=true`, `admission.runShared: closed` and `admission.legacyFormat: drain` (owner decision: switch over at enablement), which makes the tenant namespace Pod Security `baseline`.
3. Once the server is composed, register a `kodama/v1alpha2` version of each project's `kodama-cleanup` definition (the new format cannot be registered earlier; until then expired-deployment cleanup admissions are refused with `format_retired` and retried). Then open admission (`runShared: open`) under a new policy id, retaining the previous body.
4. Implement `test/integration/kube/stage/verify-run-shared.sh` and the four pinned acceptances above, then run them; record `docs/acceptance/run-resources.md` and update status/traceability only for what passed.

**Known limits to keep in mind**
- Agent, QA and delivery context readers decode only runtime context v1, so agent and deployment steps fail closed inside a `run_shared` run; container steps (ko, Buildah, services) are unaffected.
- Process and Podman provide lifecycle evidence only and admit no `run_shared` run; their composition in `development`/`test` is [T-134](T-134-local-run-shared-wiring.md).
