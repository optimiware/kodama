# Interface contracts

These documents define Kodama's external and internal contracts under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md):

| Contract | Owns | Future machine-readable form |
| --- | --- | --- |
| [HTTP interface](http-api.md) | Routes, request/response conventions, errors, concurrency | OpenAPI, generated with `oapi-codegen` |
| [Pipeline format](pipeline-format.md) | Definition syntax, typed inputs/outputs, validation | JSON Schema plus semantic graph validation |
| [Event catalog](event-catalog.md) | Envelope, event names, payloads, delivery rules | Versioned JSON Schemas |
| [Pipeline format `v1alpha2`](pipeline-format-v1alpha2.md) | Shared run workspace, services, total deadline, endpoint expressions, canonical form, order (not enabled) | `contracts/pipeline/` v2 schema (T-110) |
| [Runtime v2](runtime-v2.md) | `run_shared` context, command-begin fence, preparation/service subjects (not enabled) | `contracts/runtime/` v2 schemas and fixtures (T-110); API/aggregate integration (T-116) |
| [Run resources and execution policy](run-resources.md) | Policy pin, capabilities, resource identity, companion port, lifecycle ordering, reasons | Execution DTOs/port (T-112), policy values (T-111) |
| [Pipeline format `v1alpha3`](pipeline-format-v1alpha3.md) | `v1alpha2` plus the declared source set `spec.sources`, canonical form, diagnostics (designed, not enabled) | `contracts/pipeline/` v1alpha3 schema and fixtures (T-131); API/aggregate integration (T-158) |
| [Runtime v3](runtime-v3.md) | `run_shared` with a source set: origin, per-attempt Git credential delivery, per-source preparation, per-repository commit outputs (designed, not enabled) | `contracts/runtime/` v3 schemas and fixtures (T-131); API/aggregate integration (T-158) |
| [Git service](git-service.md) | Repository resource, `/git/` endpoint, credentials and grants, forwarding assertion, worker routes, `git.refs_updated`, trigger configuration, source set (partly enabled: T-167–T-170) | `contracts/git/` schemas and fixtures; routes and client (T-170) |

Behavioral requirements and acceptance remain in [specifications](../README.md#contributors). A contract describes how to invoke behavior, not a second implementation of its business rules.

## Versioning policy

- HTTP uses `/api/v1`. Compatible additions preserve existing required fields and semantics.
- Pipeline authoring begins at `kodama/v1alpha1`. Stored definitions are immutable; changing content creates a new definition version. `kodama/v1alpha2` is a separate format and run model (`run_shared`); `kodama/v1alpha3` is the same model plus a declared source set; each run pins its model, runtime context version and execution policy at admission. Historical definitions, digests and result hashes are never rewritten, old runs drain under their original semantics, and an installation opts into the new format explicitly before closing the old one.
- The runtime wire is versioned by route and context: `/runtime/v1` (frozen), `/runtime/v2` (additive messages; the result/source/artifact bodies and `ResultHash` are shared) and `/runtime/v3` (additive: origin, source set, Git delivery and per-repository commit outputs; `ResultHash` algorithm unchanged). A controller refuses a run model it does not implement.
- Domain events have envelope `schemaVersion` plus an event `type`. Breaking payload changes require a new supported schema version or event type with an explicit migration path.
- Adapter compatibility records pin provider/harness/MCP versions separately from product interface versions.

Unknown HTTP response fields are tolerated by clients. Unknown request fields and unknown pipeline fields are rejected in the initial interface to expose misspellings. Enum expansion needs explicit client compatibility review; an unknown value is displayed as unknown rather than treated as success.

## Examples and validation

Examples use identifiers such as `t_acme`, `p_inventory`, and `wi_1042` only for readability. Production IDs are opaque strings with a documented implementation format. Example domains and image digests identify placeholders.

During implementation, generate or maintain schemas next to the relevant modules, validate all published examples against them, and run semantic tests for ownership, concurrency, DAG rules, and failure behavior. A syntactically valid definition can still fail capability or policy validation.
