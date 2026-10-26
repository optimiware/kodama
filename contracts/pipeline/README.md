# contracts/pipeline

Frozen structural contract of a pipeline definition document
(`apiVersion: kodama/v1alpha1`), owned by T-014. The prose contract is
[pipeline format](../../docs/contracts/pipeline-format.md); this directory makes it
executable.

| Path | Purpose |
| --- | --- |
| `v1/schema.json` | JSON Schema (2020-12): container, agent and gate step alternatives, inputs, conditions, workspace sources, retry, outputs, secret references; unknown fields rejected |
| `v1/schema.go` | `SchemaJSON()` and the compiled `Schema()` accessor |
| `v1/fixtures/valid/` | Valid documents in YAML and JSON (including the planned M2 agent/gate shape) |
| `v1/fixtures/cases.json` | Rejected documents with the diagnostic code each must produce |
| `v1/fixtures/canonical/` | Golden canonical bytes and digests |

## kodama/v1alpha2 (`run_shared`)

The sibling directory `v1alpha2/` (package `pipelinev1alpha2`) holds the executable schema,
fixtures and goldens of the [`kodama/v1alpha2` format](../../docs/contracts/pipeline-format-v1alpha2.md),
delivered by T-110. It is a separate document version: a document is judged by the schema of its own
`apiVersion`, a `v1alpha2` document is never valid under the `v1/` schema and the reverse, and every
`v1/` golden, digest and diagnostic is unchanged. **Representability is not installation support**:
the validator rejects the format with `format_disabled` unless `Config.EnableV1Alpha2` is set, and run
admission refuses it until the run-resource capability checks exist (T-112, T-116).

| Path | Purpose |
| --- | --- |
| `v1alpha2/schema.json` | JSON Schema (2020-12): services, ports, resources, storage, readiness, edges, egress names, run deadline, `workspace` without `from`; unknown and raw Pod/security/mount/volume/network fields rejected |
| `v1alpha2/schema.go` | `SchemaJSON()`, `Schema()`, `APIVersion` |
| `v1alpha2/fixtures/valid/` | Valid documents: the contract example, the reference vector (YAML and JSON), the four argv rows plus the explicit-empty spelling, edges and endpoints with a builder service, source-free cleanup, order with a gate, agent and gate |
| `v1alpha2/fixtures/cases.json` | Rejected documents with the diagnostic code, path and stage each must produce (generated from the validator tests; `schemaValid` is the schema's own verdict, so a case the validator rejects at its syntax stage may be schema-valid) |
| `v1alpha2/fixtures/canonical/` | Golden canonical bytes and digests. `service-test` is the contract's published vector (`sha256:368c96…10e2`), derived independently with Python |

Regenerate goldens and cases deliberately and review the diff:
`KODAMA_UPDATE_GOLDEN=1 go test ./internal/pipeline -run 'Test_v1alpha2_(canonical_goldens|invalid_documents)'`.

Hand-off for T-116 (the sole owner of public-API integration): `api/pipeline.yaml` (bundled into the
generated aggregate and client) is **not** changed here and still documents only `kodama/v1alpha1`.
T-116 adds the `v1alpha2` document description, the `format_disabled` and `workspace_from_removed`
diagnostics and the regenerated aggregate/client together, behind the installation's explicit enablement.

## Schema versus enabled behavior

The schema says which shapes exist. It does not say which step kinds an
installation executes, and it cannot express graph, reference, source-ordering or
capability rules. `internal/pipeline` enforces those on top of it, with the
enabled kinds and profile lookup injected. The agent and gate shapes are
representable so the planned M2 format is not a breaking change, but M1 wiring
enables containers only and rejects other kinds with `kind_disabled`.

## Canonical form and digest

Documents are normalized by applying only documented defaults (absent `needs`,
`args`, `env`, `secretFiles`, `outputs` and `inputs` become empty; `input.required`
false; `workspace` `{access: none}`; `retry` one attempt with no backoff or
classes), then serialized as UTF-8 JSON with object keys in byte order, no
insignificant whitespace, no HTML escaping, array order preserved and integers as
plain decimals. The digest is `sha256:` plus the lowercase hex SHA-256 of those
bytes. It includes `metadata.name` and excludes database identity, ownership,
timestamps and the allocated version. Changing this form is a breaking change to
every stored digest; update the goldens deliberately
(`KODAMA_UPDATE_GOLDEN=1 go test ./internal/pipeline -run Test_canonical_digest`)
and review the diff.

## Diagnostics

A rejected document yields a list of stable `code` + document `path` + constant
`message`, bounded to 25 entries. Messages never echo commands, environment values
or other document content. Syntax-stage failures (not one well-formed document of
this shape) are separate from semantic-stage failures (graph, reference, type,
source, capability, policy). The codes are the `Code*` constants of
`internal/pipeline/errors.go`.

## Registration request and response

HTTP routes and examples are in [`api/pipeline.yaml`](../../api/pipeline.yaml).
`POST .../pipeline-definitions` takes the JSON document as the body and returns
`201` for a new immutable version or `200` for canonically identical existing
content, with the metadata (`id`, `name`, `version`, `digest`, `createdBy`,
`createdAt`) and the canonical `definition`. The `ETag` is the version. No
`If-Match` or `Idempotency-Key` is needed: registration is content-addressed.

## Checks

```sh
go test -race -count=1 ./contracts/pipeline/... ./internal/pipeline/...
```
