# Pipeline format `kodama/v1alpha3`

**Status:** designed by T-131 for [ADR 0011](../adr/0011-internal-tenant-git-service.md) (PX-23); executable schema, fixtures and validator in `contracts/pipeline/v1alpha3/` and `internal/pipeline`; **not enabled by default and not executable**. A document of this version is a [`kodama/v1alpha2`](pipeline-format-v1alpha2.md) (`run_shared`) document plus an optional `spec.sources` that declares which repositories of the project the run checks out, where, and with which access. Until the installation enables it (`Config.EnableV1Alpha3`, default off, independent of `EnableV1Alpha2`) and run admission implements the source set (T-148), every installation executes only the versions it already implements.

`v1alpha3` is a separate document version, not an extension flag. `kodama/v1alpha1` and `kodama/v1alpha2` documents are never reinterpreted: their canonical bytes, digests, schemas and fixtures are byte-identical to before (a test pins them), a `v1alpha3` document is never valid under their schemas, and `spec.sources` in an earlier version is `unknown_field`. A controller that does not implement `v1alpha3` refuses it; execution admits an exact version match only. The runtime wire of its runs is [runtime v3](runtime-v3.md).

## Document

```yaml
apiVersion: kodama/v1alpha3
kind: Pipeline
metadata:
  name: two-repositories
spec:
  timeoutSeconds: 3600
  sources:
    - {repository: backend, path: backend, access: write}
    - {repository: frontend, path: frontend, access: read}
  steps:
    - id: generate
      kind: container
      image: registry.example.invalid/go-tools@sha256:3333333333333333333333333333333333333333333333333333333333333333
      workspace: {access: write}
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace/backend && go generate ./..."]
      timeoutSeconds: 600
    - id: contract-test
      kind: container
      needs: [generate]
      image: registry.example.invalid/go-tools@sha256:3333333333333333333333333333333333333333333333333333333333333333
      workspace: {access: read}
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace/backend && go test ./... && cd ../frontend && ./check-contract.sh"]
      timeoutSeconds: 900
```

Images and names are placeholders. Everything else (services, edges, egress, run deadline, expressions, `needs`, retry, outputs, agent and gate steps, argv matrix, source defaults) is exactly the [`v1alpha2` format](pipeline-format-v1alpha2.md#fields); this page states only what differs.

## `spec.sources`

| Member | Rule |
| --- | --- |
| `spec.sources` | Optional list of 1–32 sources, in declaration order (the order is part of the canonical form). **Absent is not filled**: a run without sources has the project's one default repository at `.`, so a single-source document needs no `sources` and has one digest in every project. `sources: []` is rejected |
| `repository` | The repository's **logical name within the project** (`^[a-z][a-z0-9-]{0,62}$`, the repository-name syntax of the [Git contract](git-service.md#repository-resource)), unique in the document. Run admission resolves it, checks that the project owns it and authorizes the run's access, and pins one commit. A document never contains a commit, branch, ref, URL or repository ID |
| `path` | Checkout path relative to `/workspace`: clean, no `..`, components start with a letter, digit or underscore (never `.git` or a hidden name), at most 255 bytes: the [source set](git-service.md#source-set) path rules, enforced by the same function. Unique, no path a directory prefix of another, and `.` only as the sole source |
| `access` | `read` or `write`. `write` is a **ceiling**: only the attempt holding the writer reservation may publish, to the repository's branch and to no other ref outside the run's agent namespace, and a `read` source can never be written. It does not make a step a writer: `step.workspace.access` keeps its `v1alpha2` meaning for the whole workspace |

A step that writes the workspace needs at least one `write` source (`source_write_undeclared`) and a `write` source needs a writing step (`source_write_unused`), because a ceiling nobody can use is a misleading declaration. Writers must still be totally ordered by `needs` (`parallel_writers`), and an agent step with `workspace: {access: write}` is a writer. Which branch a `write` source updates and which commit each source pins are decided by admission, never by the document ([source set](git-service.md#source-set)): the task branch of a work-item run, a branch of the run's own namespace otherwise.

**Source-backed and source-free.** The [source defaults](pipeline-format-v1alpha2.md#source-backed-and-source-free-defaults) are unchanged. Sources describe a source-backed run; a run without source (an internal deployment-cleanup run) with a document that declares sources is refused at admission (`sources_source_free`). A `v1alpha3` document without `sources` is valid for both kinds of run with one digest. A name that does not resolve to a repository of the project is refused at admission as `source_repository_unknown`; a repository of another project is indistinguishable from an unknown one. These two refusals are admission outcomes: registration cannot know the project's repositories and never fails on an unknown name, because repositories may be created after the definition.

## Canonical form

The `v1alpha2` serialization and defaults, with `sources` rendered as given (an array of objects with sorted keys `access`, `path`, `repository`) and **not** filled when absent. The digest includes `apiVersion`. Consequently a `v1alpha3` document without `sources` is the `v1alpha2` canonical form with a different `apiVersion` and a different digest. Reference vector, derived independently with Python from the [`v1alpha2` vector](pipeline-format-v1alpha2.md#canonical-form): the document of that vector with `apiVersion: kodama/v1alpha3` has digest `sha256:4ac7d72393f64a79ae72d17bdfe1e8962c4ac5b521b6c1cf369e5f9b938267c2`. Swapping the order of two sources changes the digest.

## Validation

Everything in [`v1alpha2` validation](pipeline-format-v1alpha2.md#validation) applies. Added diagnostics (stable `code` + `path` + constant `message`, no document content echoed):

| Code | Stage | Cause |
| --- | --- | --- |
| `invalid_source_path` | syntax | A path violates the rules above (also reported for schema pattern, length and `..` failures) |
| `source_path_overlap` | syntax | Two paths equal or nested, or `.` next to another source |
| `duplicate_source_repository` | syntax | One repository declared twice |
| `source_write_undeclared` | semantic | A writing step and no `write` source |
| `source_write_unused` | semantic | A `write` source and no writing step |

Other source-member failures use the existing codes: `missing_field`, `unknown_field` (including `commit`, `branch`, `ref`, `url`, `repositoryId`), `invalid_format` (name), `invalid_value` (`access`, empty list), `invalid_type`, `limit_exceeded` (more than 32). Admission refusals added to the [`v1alpha2` list](pipeline-format-v1alpha2.md#format-availability-and-admission-refusals): `sources_source_free`, `source_repository_unknown`. Like `v1alpha2`, registration returns `format_disabled` while the installation has not enabled the format, with no lookup and nothing persisted.

## Machine-readable form

[`contracts/pipeline/v1alpha3/schema.json`](../../contracts/pipeline/v1alpha3/schema.json) (the `v1alpha2` schema plus `sources`, asserted by a test), valid documents, canonical goldens and rejected cases in `contracts/pipeline/v1alpha3/fixtures/`, and the validator in `internal/pipeline` (`EffectiveSources` gives run admission the declared sources or the implicit default). The public API description (`api/pipeline.yaml`) and generated client are integrated by T-158.

## Migration

From `v1alpha2`: change `apiVersion`; to check out several repositories add `sources` (and make sure exactly the steps that publish write the workspace); a single-repository definition needs nothing else. Registering the result creates a new definition; the old one, its digest and its runs are untouched.
