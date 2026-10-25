---
id: T-106
milestone: production
status: deferred
depends_on: [T-104, T-090]
decision_blockers: []
owns: [docs/operations/tls-hardening.md]
---
# T-106 — Production TLS hardening with a private CA

## Goal
Bring the `production` profile to verified TLS between every internal component using an installation-operated private CA, and retire the stage deviations recorded in [O-27](../decisions.md#open-decisions-and-resolved-selections). **This task needs a reviewed design before it is scheduled**; the sections below list the facts and the questions that design must answer, not decisions.

## Context (established while qualifying the Scaleway stage cluster)
- The chart's `external` profile requires `tls://` for NATS ([schema](../../deploy/helm/kodama/values.schema.json)); the `development` profile permits plaintext. The server records `profile` but does not branch on it. The stage runs NATS as plaintext `nats://` with token authentication under `profile: development` (O-27), with `KODAMA_ENV=production` still refusing every development/test adapter.
- The server's NATS client (`connectNATS`) and its OIDC discovery/JWKS client use only the system trust store. `config.tls.customCA=true` projects `ca.crt`, but the server uses it only for Git (`KODAMA_GIT_CA_FILE`). The runtime callback and registry have their own CA handling. A private CA therefore cannot yet secure NATS or the OIDC issuer without extending the container trust store.
- The stage issuer is the `oauthsonas` persona fixture (O-27), reached through the public ingress; a private-CA certificate for the real issuer must be trusted by the server and by every client of the installation. Replacing the fixture with a real issuer is part of this design.
- PostgreSQL connections use `sslmode=require` (encrypted, server not verified).
- The stage uses no private CA: public ingress certificates (including the OIDC issuer) come from Let's Encrypt through cert-manager, NATS is plaintext with a token, and the runtime callback trust is the Let's Encrypt root bundle. A private CA, its rotation and its distribution are entirely unbuilt.

## Design questions to settle first
1. **CA ownership and lifecycle.** Where the CA key lives (OpenTofu state, cert-manager, a cloud private CA), validity, rotation and revocation, and how leaf renewal reaches running Pods without a restart or with a defined one.
2. **Trust distribution.** Whether the server honours the projected CA for NATS and OIDC (product change in `internal/app`), the chart gains a trust-bundle option, or the platform distributes a bundle (for example Kubernetes ClusterTrustBundle or trust-manager). Which owner changes which file, given the ordered shared-composition owners.
3. **NATS.** Server-only TLS or mutual TLS; per-client identities versus the shared token; subject-level authorization; how JetStream restart and reconnect behave across certificate rotation.
4. **OIDC issuer topology.** Public versus private issuer, hostname and issuer-URL stability, hairpin access from Pods, and how the CLI and test clients obtain trust.
5. **Database.** `verify-full` against the provider CA for managed PostgreSQL and how the CA bundle is delivered to server and migration Jobs.
6. **Workloads.** Which CA bundle tenant Jobs receive, and how it stays distinct from the control-plane bundle.
7. **Interim controls and exit.** NetworkPolicy and token hygiene while NATS is plaintext, the measurable condition that ends the stage deviation, and the chart/schema change (if any) so `external` is the only production profile.
8. **Hosted authority (T-061).** Which parts change when Platform's OAuth issuer, rather than Dex, is the authority.

## Provisional acceptance (to be defined when scheduled)
Candidate checks, not yet contractual: an installation with NATS TLS reaches readiness under `profile: external`; a Pod with a wrong or expired certificate is refused; certificate rotation completes without losing committed events; OIDC and NATS clients fail closed on an untrusted CA; PostgreSQL verifies its server certificate; the stage deviation (O-27) is removed.

## Out of scope
Public certificate issuance, untrusted-code isolation (T-055), and recovery drills (T-029).
