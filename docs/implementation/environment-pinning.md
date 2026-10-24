# Environment pinning contract and ownership

**Status:** T-076 supplies the execution contract; T-035 implements the delivery snapshot provider. T-077 unit-tests transaction/retention/revocation/cleanup bindings, T-072 wires them and T-067/T-038 qualify the actual joined path before M2 acceptance. Doubles are not evidence of SQL or production support.

## Component boundary — T-035

Delivery owns project-scoped environments, immutable configuration snapshots and deployment records. Its adapter implements execution's `EnvironmentResolver.ResolveEnvironment(context.Context, EnvironmentRequest) (EnvironmentSnapshot, error)`. The request contains verified `Scope`, `Actor` and `EnvironmentID`; the response carries the same scope, `Pin` (`id`, `configDigest`) and exact `Secrets` bindings. Canonical digests cover configuration and references, never secret plaintext. The provider checks current access and retains an immutable snapshot; returning a foreign scope/ID, malformed digest, conflicting credential version/name or denied lookup refuses admission. Since runtime resolution identifies a credential by reference/version, a step cannot alias one reference/version under different binding names or delivery forms; a completely identical explicit binding is reused.

Delivery consumes a required pinned-context reader for runtime callbacks. It compares the attempt's authorized context, source/artifact provenance, action key and environment/configuration pin against the requested deployment. An absent or mismatched pin fails closed. It must not substitute the environment's current mutable configuration on the first deployment callback.

T-035 component tests may supply explicit immutable environment pins through that consumer-owned reader, while using real delivery persistence and artifact/provenance adapters. They prove the record boundary, not executable wiring or transaction qualification. T-035 consumes the merged T-076 Go contract and does not wait for T-077; T-077 needs the real provider T-035 supplies. Missing pins always fail closed.

## Execution contribution — T-076

The bounded execution-side contribution does not edit T-062's active transaction paths:

1. Resolve each declared step environment under the verified tenant/project and current admission authority, before command dispatch. Preserve the existing prepare-outside-transaction and commit/revalidation boundaries.
2. Persist `Snapshot.Environments` as per-step `StepEnvironment` values in the existing run JSONB. Include them in snapshot identity; omit the field for historical/no-environment snapshots so their old JSON/digest remains unchanged. Register merged exact credential bindings through the existing admission secret-pin transaction; failure rolls back run, steps, pins, history/audit/outbox together.
3. Populate the existing `Context.Environment` shape (`id`, `configDigest`) from that persisted pin. Keep credential references/versions in their existing declared secret-binding path. No new runtime DTO or wire field is implied.
4. Keep `Planner.Plan` pure: no database, delivery or network lookup during Job planning. Retries, restarts and replicas rebuild context from admitted pins rather than resolving mutable settings again.
5. Preserve transactional run-authority and secret-revocation checks. T-077 implements delivery snapshot/current-revocation binding with unit tests; T-067 qualifies actual SQL/retention/cleanup behavior before M2 acceptance. Cleanup stays refused until its binding resolves the recorded deployment's retained configuration and exact credentials, never current mutable settings.
6. `NewServiceWithEnvironments` injects the optional resolver explicitly while preserving the existing constructor and active `Dependencies` contract. Ordinary M1 runs remain available without a resolver; environment declarations then refuse admission. Context construction has no resolver access and cannot silently omit a declared environment pin.

T-076 tests use real execution/secret PostgreSQL adapters and a bounded environment-provider double. T-077's real provider/transaction binding joins the T-062 conventions, with additive migrations only if required for retained snapshot ownership. No generic framework, network call under a SQL lock, new deployment engine or plaintext snapshot is needed.

## Acceptance and activation

- T-035: real record uniqueness/RLS/provenance and callback rejection for absent/foreign/mismatched pins; immutable-pin fixtures are component evidence only.
- T-076: the concrete Go seam, real execution/secret admission persistence, editing/restart stability, rollback, foreign-scope denial and historical identity compatibility; no real delivery qualification is claimed.
- T-077: real delivery retention/current-authority/credential and deployment-bound cleanup qualification after T-035/T-062/T-076; owns the ordered adapter/PostgreSQL changes and preserves the original full pinning acceptance.
- T-065: tools/scripts may develop from T-035's published fixture contract without waiting for the pinning implementation.
- T-066: cleanup admission consumes the qualified T-077 binding and preserves the recorded deployment/configuration identity.
- T-067: joins the real components, including T-077, and verifies admission → context → deployment → apply/reconcile/cleanup before claiming integration.
- T-072: activates only the qualified merged path. Root wiring alone cannot supply a missing execution capability, and fixtures do not satisfy this activation gate.

The selected pipeline language, runtime context fields, O-11 retention and O-19 secret-delivery decisions remain the inputs. This checkpoint assigns missing implementation ownership without relaxing configuration identity, current authorization or existing delivery acceptance.
