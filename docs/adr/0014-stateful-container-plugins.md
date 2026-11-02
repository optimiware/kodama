---
status: accepted
---

# ADR 0014: Stateful container plugins and deployment-core retirement

**Date:** 2026-11-02  
**Direction:** owner-requested on 2026-10-30; this record confirms the decision and the staged cutover.  
**Disposition:** accepted for new-format work. The convenience `internal/delivery` path remains the historical owner until P8 removes it; this ADR does not claim that cutover is complete.  
**Repository baseline:** `github.com/optimiware/kodama` at `74096e2623168db4502c3f2eff64721377be578e` on `main`.  
**Implementation:** [stateful container plugins and deployment-core retirement](../implementation/container-plugins.md).  
**Register:** [O-41](../decisions.md#container-plugins-and-deployment-core--2026-11-02-o-41). Supersedes [O-12](../decisions.md) for new-format application deployment while preserving O-12 as the historical selection.

## Context

Application delivery currently lives in Kodama core: `internal/delivery` owns
Environment and Deployment records, runtime deployment callbacks, a cleanup
scheduler and provider-specific Kubernetes behavior. That makes a provider
lifecycle part of the server, requires a work item for CI-style maintenance and
couples gates and QA to deployment rows.

The owner selected a generic model: deployment behavior is an ordinary
digest-pinned container plugin, persistent state is a core capability, and one
shared engine owns journaling, reservations, release identity and recovery.
Core keeps execution, credentials, artifacts, state and human decisions; the
plugin owns provider interpretation.

## Decision

1. Plugins are ordinary container steps through the existing entrypoint and
   result protocol. There is no new step kind, server plugin process, provider
   registry or nested scheduler.
2. Core provides scoped, strongly consistent per-key state with conditional
   mutation, bounded values, artifact retention references and audit. State is
   not a deployment schema and the server never interprets plugin keys.
3. Inputs, credentials and artifact descriptors are pinned at admission or
   dispatch. A mutable state read is distinguishable from an immutable input.
4. One shared deployment engine owns resource reservations, operation
   fingerprints, durable checkpoints, release history, receipt emission and
   recovery. Drivers implement only provider validation, mutation and
   observation through a narrow contract.
5. Kubernetes is the mandatory first driver replacing the legacy deployment
   tools. Cloudflare Workers and GCP Cloud Run use the same engine. Provider
   qualification is separate from code existence.
6. Source-free project runs are admitted without a work item, workspace or
   Deployment record. Source-backed CI and trigger origins build on the same
   admission path; they do not invent business tasks.
7. Gates bind immutable evidence and human authority. A plugin or state value
   cannot approve a gate. QA consumes immutable receipts and provenance rather
   than provider records in core.
8. The transition keeps one owner of each live resource. Legacy records,
   hashes and outstanding effects are exported or settled before destructive
   retirement; the legacy path is removed only after equivalent plugin paths
   pass their named tests.

## Consequences

- The core stays provider-neutral and can run plugins for non-deployment work.
- Provider behavior can change without changing the server, but the engine and
  driver contract become security- and compatibility-critical.
- The state API needs artifact retention, quotas, audit and failure semantics
  that ordinary key/value stores do not provide.
- The cutover is staged. Until P8, both the legacy delivery composition and the
  plugin path exist; new work must not silently depend on legacy deployment
  rows, callbacks or cleanup workers.
- Live Cloudflare and GCP qualification is not implied by compiled drivers or
  controlled command tests. Unsupported provider behavior is reported, never
  simulated as success.

## Alternatives considered

- **Keep delivery in core and add provider plugins behind it.** Rejected: it
  preserves the deployment entity model and provider lifecycle in the server.
- **A new plugin step kind or server extension process.** Rejected: ordinary
  containers already provide images, credentials, artifacts, cancellation and
  result handling.
- **A generic multi-key transaction/KV platform.** Rejected: per-key CAS plus
  immutable artifacts and an operation journal are sufficient and keep the
  recovery model understandable.
- **Deploy with scripts directly.** Rejected: no durable intent, release
  history, readiness observation or uncertain-effect recovery.

## Conformance

The new-format requirements are `CP-01` through `CP-14` and acceptance
scenarios `CP-A01` through `CP-A16` in the implementation plan. This ADR records
the decision; implementation and qualification status remain there and in
[assembly status](../implementation/assembly-status.md).
