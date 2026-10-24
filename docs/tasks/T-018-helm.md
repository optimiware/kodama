---
id: T-018
milestone: M1
status: done
depends_on: [T-001]
decision_blockers: []
owns: [deploy/helm/kodama/]
---
# T-018 — Helm installation

## Goal
Package the fixed initial server/execution profile.

## Contract
Use [installation](../operations/installation-and-configuration.md) and frozen scaffold configuration. One server replica, migration command, scoped service accounts, restricted tenant Jobs, quotas/network policy, secret references.

## Deliverables
Chart/values schema, development and external-dependency examples, image/binary injection configuration, probes, retention-aware uninstall documentation in chart README.

## Acceptance
`helm_render_profiles` and `helm_policy_assertions` validate schemas/resources and absence of plaintext credentials/privilege escalation. Real install is T-026; covers IT-07/IT-08 and PX-04.

## Out of scope
Operator, bundled production database promises, or untrusted-hosted isolation claims.
