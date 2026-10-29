---
id: T-117
milestone: production
status: planned
depends_on: [T-116, T-121]
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
