---
id: T-090
milestone: production
status: deferred
depends_on: [T-089, T-044, T-028, T-104]
decision_blockers: []
owns: [deploy/staging/, test/e2e/staging/, docs/operations/staging.md]
---
# T-090 — Trusted-engineer staging deployment

## Goal
Run the qualified appliance on Platform's stage cluster, the first staging target ([C-19](../decisions.md#confirmed-direction)), and record what that proves.

## Contract
Use `KODAMA_ENV=production` with the standalone authority, a separate Kodama control-plane namespace and release, scoped execution namespaces, distinct database roles, storage prefixes, service accounts and KMS permissions, and the cloud KMS of the installation. Regular Kubernetes containers with the existing restricted baseline are the trust model (O-07, P-14); name it in the handoff. The environment refuses every development and test adapter; the development keyfile is never a cloud KMS.

Qualify only what is deployed: the chart and images with T-089 component evidence and T-104 full-readiness evidence, the migration sequence under a database lock, readiness distinguishing dependencies, and one end-to-end run (submit a pipeline, retain logs and artifacts, cancel, restart the server and reconcile). O-24's component checkpoint alone cannot authorize staging; T-104 also requires O-23 cleanup and T-105 production startup wiring under O-26, and qualifies live KMS during deployment. A standalone smoke deployment may precede hosted qualification; do not relabel it as hosted integration (T-061).

## Deliverables
Stage values and an install/upgrade/uninstall runbook with exact commands, the stage handoff (cluster and context, namespaces, image and chart revisions, KMS and authority profile, trust model), and a bounded smoke suite with its report.

## Acceptance
Define and name the acceptance checks when this task is scheduled: install from clean, the end-to-end run, restart and reconciliation, uninstall, and refusal of a development or test adapter in the deployed configuration. Bring this check contract up to date before scheduling, as the task index requires.

## Out of scope
Production release and support claims (T-056), hosted Platform authority (T-061), measured capacity, recovery drills (T-029) and stronger isolation (T-055).
