# Interface contracts

These documents define Kodama's external and internal contracts under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md):

| Contract | Owns | Future machine-readable form |
| --- | --- | --- |
| [HTTP interface](http-api.md) | Routes, request/response conventions, errors, concurrency | OpenAPI, generated with `oapi-codegen` |
| [Pipeline format](pipeline-format.md) | Definition syntax, typed inputs/outputs, validation | JSON Schema plus semantic graph validation |
| [Event catalog](event-catalog.md) | Envelope, event names, payloads, delivery rules | Versioned JSON Schemas |
| [Pipeline format `v1alpha2`](pipeline-format-v1alpha2.md) | Historical shared run workspace/services model; not the target contract | Retained only while the transitional implementation is removed |
| [Runtime v2](runtime-v2.md) | Historical `run_shared` runtime context; not the target contract | Retained only while the transitional implementation is removed |
| [Run resources and execution policy](run-resources.md) | Historical policy/resource lifecycle details; reusable behavior is ported into the kernel contract | Retained only as implementation history |
| [Pipeline format `v1alpha3`](pipeline-format-v1alpha3.md) | Historical `v1alpha2` plus source set; never enabled | Retained only as implementation history |
| [Runtime v3](runtime-v3.md) | Historical source-set runtime; never enabled | Retained only as implementation history |
| [Git service](git-service.md) | Repository resource, `/git/` endpoint, credentials and grants, forwarding assertion, worker routes, `git.refs_updated`, trigger configuration, source set (partly enabled: T-167–T-170) | `contracts/git/` schemas and fixtures; routes and client (T-170) |

Behavioral requirements and acceptance remain in [specifications](../README.md#contributors). A contract describes how to invoke behavior, not a second implementation of its business rules.

## Versioning policy

- HTTP uses `/api/v1`. Compatible additions preserve existing required fields and semantics.
- The target pipeline authoring contract is the one described in [pipeline format](pipeline-format.md). The `v1alpha1`, `v1alpha2`, `v1alpha3`, runtime v2, and runtime v3 documents are historical during the simplification; they are not new product commitments. Stored definitions and runs remain immutable, and an existing database is not silently reinterpreted.
- Domain events have envelope `schemaVersion` plus an event `type`. Breaking payload changes require a new supported schema version or event type with an explicit migration path.
- Adapter compatibility records pin provider/harness/MCP versions separately from product interface versions.

Unknown HTTP response fields are tolerated by clients. Unknown request fields and unknown pipeline fields are rejected in the initial interface to expose misspellings. Enum expansion needs explicit client compatibility review; an unknown value is displayed as unknown rather than treated as success.

## Examples and validation

Examples use identifiers such as `t_acme`, `p_inventory`, and `wi_1042` only for readability. Production IDs are opaque strings with a documented implementation format. Example domains and image digests identify placeholders.

During implementation, generate or maintain schemas next to the relevant modules, validate all published examples against them, and run semantic tests for ownership, concurrency, DAG rules, and failure behavior. A syntactically valid definition can still fail capability or policy validation.
