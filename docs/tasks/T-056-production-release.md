---
id: T-056
milestone: production
status: deferred
depends_on: [T-029, T-061, T-090, T-106]
decision_blockers: []
owns: [deploy/helm/kodama/, internal/lifecycle/, internal/app/production-lifecycle.go, internal/app/run.go, api/lifecycle.yaml, test/e2e/production/, docs/operations/installation-and-configuration.md, docs/releases/, README.md, LICENSE, CHANGELOG.md]
---
# T-056 — Supported production and public release

## Goal
Publish only the operation/isolation/capacity guarantees demonstrated by the implemented system.

## Contract
Use Kodama and target public repository/module `github.com/optimiware/kodama` under resolved O-01. O-02 selects Apache-2.0, including commercial use under its terms; package the root license and required third-party notices without imposing an additional commercial-use restriction. Contributions follow Section 5 unless explicitly stated otherwise. Coordinate a bounded mechanical public-module/import migration before publishing; obtain explicit ownership for files outside this task rather than rewriting other modules implicitly.

Keep Go 1.27 (currently 1.27.2); do not downgrade to Platform's older toolchain. O-10's reference workload is 20 tenants, 100 projects and 20 concurrent attempts, with declared per-tenant bounds plus concurrent API/SSE/model/webhook activity. Record hardware, dependency/image pins, latency/queueing/throughput and failure recovery before publishing supported capacity. Implement the approved O-11 retention/offboarding windows and measure its RPO/RTO.

O-07 starts with trusted-engineer staging on regular Kubernetes containers. Qualify this declared restricted-container profile and consume the required recovery evidence before any supported production claim. T-055's stronger isolation/security extension follows this baseline release; gVisor, Substrate and other backends are not prerequisites or advertised capabilities here. Full air-gap support is excluded from the first release under O-08. Platform owns subscription billing, commercial usage/access and entitlements under O-13; Kodama contributes runtime usage/outcome evidence and execution limits only.

Consume T-061's real-platform acceptance and supported core revision/interface record before claiming Optimi-hosted authority integration, including when execution is trusted-engineer. Wire the explicit authority profile and its adapter/migration contributions; standalone bootstrap must never become a hosted fallback. Hosted identity integration does not imply support for mutually untrusted code.

## Deliverables
Retention/offboarding coordinator and scoped tenant disable/delete interface, its route/migration contribution, upgrade/retention/offboarding tests, supported version matrix, measured limits, verified installation/uninstall commands, Apache-2.0 license/third-party notices, release notes/images/chart provenance.

## Acceptance
`production_upgrade`, `tenant_offboarding`, `reference_load`, and `profile_install_restore` verify IT-12, VS-10/VS-12, WS-12, PX-11 and the approved operations contract. Exercise the exact O-10 reference workload and report supported limits; verify immediate access revocation, the seven-day settled active-data deletion objective, disclosed thirty-day backup expiry and retained references/keys that still prevent deletion.

`hosted_core_compatibility` verifies PI-01–PI-10 with T-061's current evidence and the release's actual platform revision/configuration, including disabled-tenant admission and retained-history behavior.

`profile_install_restore` and `reference_load` identify the regular-container trusted-engineer profile explicitly and verify its existing restrictions/limits. The deferred T-055 scenarios remain required before a later untrusted-execution profile is advertised; they are not silently marked passed by this release.

## Out of scope
Inventing license terms, promising unmeasured SLOs, or silently adding commercial features.

## Gaps recorded by T-104 that block a supported claim
Resolve or explicitly scope out before any supported-production statement: private-CA TLS and a real issuer ([T-106](T-106-production-tls-hardening.md), O-27); direct project-secret delivery ([T-107](T-107-direct-secret-delivery.md)); finalization/diagnostic signals ([T-108](T-108-execution-finalization-diagnostics.md)); recovery and PITR ([T-029](T-029-recovery-qualification.md)). A T-104 or T-090 receipt is bound to its commit, contract and values and does not carry to a later release candidate.

## Future shaping after release decisions
Before scheduling, consider separate lifecycle/offboarding component, release packaging/version reconciliation, capacity qualification and final install/restore join. Record exact paths and dependencies first, serializing chart/root-file owners after T-029/T-044/T-061. Retain all named acceptance against the selected O-01/O-02/O-10 baseline; T-055 follows this trusted-engineer release. T-061 must qualify the supported upstream contracts under resolved O-18/O-19; later adapter/caller/credential splits cannot be treated as done from local fixtures.
