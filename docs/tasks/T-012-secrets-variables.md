---
id: T-012
milestone: M1
status: done
depends_on: [T-004, T-005]
decision_blockers: []
owns: [internal/secrets/, api/configuration.yaml]
review: human
---
# T-012 — Variables and encrypted typed secrets

## Goal
Supply deterministic project configuration and scoped credentials.

## Contract
Implement [configuration](../specs/variables-and-secrets.md), including resolver and key-wrapping interface for the later production adapter. Plain variables and project/input precedence are distinct from encrypted secrets.

Keep declared-reference/version resolution behind a consumer-owned port. The encrypted local store is the standalone adapter; hosted core custody/version semantics require O-19/T-061. Do not silently resolve a pinned Kodama version to the platform's newest current value.

## Deliverables
Versioned tables/routes, development keyfile KEK adapter, scope-key wrap, HKDF value keys, revoke/rewrap/delete metadata, declared-reference resolver and audit, OpenAPI.

## Acceptance
`context_swap`, `pinned_rotation`, `revoked_resolution`, `no_plaintext_metadata`, and `precedence` cover VS-01–VS-09/VS-11/VS-12. Development restore fixture seeds VS-10 qualification.

## Out of scope
The AWS, GCP, and Scaleway adapters (T-028), tenant/environment inheritance, or a generic connection resource.
