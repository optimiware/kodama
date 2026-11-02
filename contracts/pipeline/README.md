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

## kodama/v1alpha3 (run_shared with a source set)

The sibling directory `v1alpha3/` (package `pipelinev1alpha3`), delivered by T-131, is the
executable schema, fixtures and goldens of the additive `kodama/v1alpha3` format of
[ADR 0011](../../docs/adr/0011-internal-tenant-git-service.md) (PX-23). It is the `kodama/v1alpha2`
document plus an optional `spec.sources` and nothing else (a test asserts the schemas are equal
apart from `$id`, `apiVersion`, the description and the three source definitions). Like `v1alpha2` it is
a separate document version: judged by the schema of its own `apiVersion`, never valid under another
version's schema and the reverse, default off in the validator (`Config.EnableV1Alpha3`, independent of
`EnableV1Alpha2`) and refused by run admission until the execution tasks implement it. Every
`v1/` and `v1alpha2/` file is byte-identical (`Test_unit_pipeline_old_formats_unchanged` pins their
trees). The prose contract of the format is
[`pipeline-format-v1alpha3.md`](../../docs/contracts/pipeline-format-v1alpha3.md); the Git contract's
[source set](../../docs/contracts/git-service.md#source-set) defines the set admission builds from it.

```yaml
spec:
  sources:
    - {repository: backend, path: backend, access: write}
    - {repository: frontend, path: frontend, access: read}
```

| Member | Rule |
| --- | --- |
| `spec.sources` | Optional list of 1–32 sources, in declaration order (the order is part of the canonical form). Absent is **not filled**: a run without sources has the project's one default repository at `.` (the single-source case), so a single-source document stays valid without it and keeps one digest in every project |
| `repository` | Logical repository name within the project (`^[a-z][a-z0-9-]{0,62}$`), unique in the document. Resolved, authorized and pinned to one commit at run admission; a document never names a commit, branch, ref, URL or repository ID |
| `path` | Checkout path relative to `/workspace`: exactly `gitv1.ValidateSourcePath` (clean, components start with a letter, digit or underscore so never `.git` or a hidden name, no `..`, at most 255 bytes). Unique, no path inside another, and `.` only as the sole entry |
| `access` | `read` or `write`. `write` is a **ceiling**: only the step holding the writer reservation may publish, to the repository's branch and nowhere else; a `read` source can never be written. A step that writes the workspace needs at least one `write` source and a `write` source needs a writing step |

New diagnostic codes: `invalid_source_path`, `source_path_overlap`, `duplicate_source_repository` (syntax
stage), `source_write_undeclared`, `source_write_unused` (semantic stage); admission refusals
`source_repository_unknown` and `sources_source_free` (spelled by `internal/pipeline`, decided by run
admission). `v1alpha3/fixtures/cases.json` is generated from the validator tests
(`KODAMA_UPDATE_GOLDEN=1 go test ./internal/pipeline -run Test_unit_pipeline_sources_declaration`),
and the canonical goldens by the same test; `service-test` is the `v1alpha2` reference vector with the
`apiVersion` replaced, digest `sha256:4ac7d72393f64a79ae72d17bdfe1e8962c4ac5b521b6c1cf369e5f9b938267c2`
derived independently with Python.

`api/pipeline.yaml` is **not** changed here; T-158 adds the `v1alpha3` description with the regenerated
aggregate and client.

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
