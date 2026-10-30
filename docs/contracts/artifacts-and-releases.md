# Artifact/release and generic delivery contract refinement

**Status:** ADR 0012 semantic contract; executable shapes are frozen by T-140 before implementation. Existing `api/artifacts.yaml`, `api/delivery.yaml`, pipeline/runtime schemas and hashes remain authoritative for the current executable path. Illustrations below are conceptual, not accepted request payloads or new pipeline fields.

## Immutable references

Use scoped opaque IDs and `sha256:` digests. An artifact reference carries both ID and digest. A release reference carries ID and canonical manifest digest; its project/name/version is the discovery coordinate. A location or signed URL is never an identity.

Conceptual release:

```text
project: inventory
name: application
version: 1.4.2
sources: [repository + exact commit, ...]
artifacts:
  api-image: artifact ID + OCI manifest digest
  chart: artifact ID + chart digest
  web-assets: artifact ID + archive digest
  tests: artifact ID + report digest
```

T-140 freezes a versioned canonical JSON manifest: sorted object keys and named entries, explicit optional-value rules, no floating-point numbers, and sorted source references. IDs/digests, artifact roles/media types and producer/source references are stable inputs. Storage locations, transfer URLs, timestamps and credentials are not manifest identity. Golden vectors prove equivalent ordering and reject digest-changing mutations. Historical artifact `version` and ETags retain their existing concurrency meaning.

## Public and workload operations

| Audience | Operations | Scope |
| --- | --- | --- |
| Human/integration API and CLI | Artifact list/read/download; release create/list/read/resolve/retire; publication list/read | Existing tenant/project authority; create/retire/publish are explicit engineering management actions, never implicit viewer rights |
| Producing attempt | Existing reserve/upload/complete and OCI registration; create release from authorized finalized predecessor outputs; record a publication receipt | Current runtime grant; exact owned outputs/inputs and authorized destinations |
| Consuming attempt | Resolve metadata and obtain bounded fetch instructions for admitted artifact/release inputs | Exact admitted IDs/digests; no project-wide browse or arbitrary artifact read |
| Deployment attempt | Begin/read/report its generic deployment; read the selected release/configuration | Pinned environment, artifacts/release and own stable action key |
| Cleanup attempt | Read/report the recorded deployment's cleanup | Explicit deployment binding and current authorized credentials; no arbitrary target cleanup |

Release creation can also follow a completed run through the public API; a command cannot declare unfinalized same-step outputs already accepted. Helpers use the existing runtime identity, never a human/admin credential in the container. Errors distinguish conflicts, incomplete/missing content and unavailable verification while preserving nondisclosure for foreign references. Download URLs are short-lived instructions and are omitted from persistent receipts/events/logs.

Expected CLI groups are `artifact`, `release`, `publication` and the existing `environment`/`deployment`. T-140 freezes exact flags, runtime routes, action grants and OpenAPI fragments; T-145 composes/generates the public client. No tool-name-specific command group or pipeline step kind is needed. Artifact/release references resolve into the run's pinned inputs; helpers expose files/references to ordinary shell tools.

## Publication receipt

A receipt identifies the source artifact ID/digest, authorized destination repository/object reference, observed digest, producing attempt and verifier/evidence timestamp. A stable `(artifact, destination, digest)` identity within the project permits replay. Verification is server-owned or performed by a specifically authorized verifier whose evidence is checked; caller prose or a registry tag alone is insufficient. The initial implementation should reuse the existing bounded registry/object clients rather than introduce a generic remote-operation framework.

Destination fetch policy must constrain scheme/origin, redirects and credential audience. OCI verification traverses the content graph needed to pull an index/image/chart and verifies digests; a partial graph is not a published copy. File copies verify bytes, not just an ETag. Transformations create a new derived artifact. A receipt records an observation, so use checks availability again; it never silently substitutes a different object.

## Generic environment and deployment

Environment inputs: name, target label, explicit nonsecret configuration, explicitly named secret bindings, optional preview expiry/cleanup definition. Target configuration may name Kubernetes, a cloud account, object storage or another destination. It has no mandatory namespace, kubeconfig or image. Runtime environment selection `KODAMA_ENV` remains an independent installation setting.

For the new generic contract, the configuration digest binds target/configuration and logical credential bindings (reference, name, delivery form); exact secret value versions are separate immutable **run** pins. Rotation can therefore authorize a new rollback run without pretending that the old run used different credentials. Legacy configuration digests already include value versions and keep that meaning. Migrating a legacy snapshot creates an explicitly linked generic snapshot with its own digest; it never recomputes or overwrites the historical digest.

For tracked delivery, persist before apply:

```text
deployment ID + stable step action key
tenant/project + originating run/step/attempt
environment ID + retained configuration digest
exact release ID/digest OR exact artifact set (possibly empty for infrastructure)
source references + deployment-script/definition identity
optional external references and expiry
```

The current small status machine can remain `pending|running|succeeded|failed` with `reason=outcome_unknown`, timestamps and cleanup history. External references and observations are bounded, credential-free, tool-owned metadata; Kubernetes annotations/UIDs/readiness are no longer universal fields. Record whether success is the command's report or backed by a tool observation. Querying a record never grants authority to operate on its target.

A duplicate begin for the same action and exact pins returns its existing deployment; changed pins conflict. A missing record does not prove an arbitrary command made no external change. A lost apply response is unknown until the script/operator observes the target. Neither deployment metadata nor an image/command catalogue grants automatic replay. Late/cancelled attempts cannot replace a settled outcome. Explicit reconciliation remains attributable to a currently authorized actor or newly bound reconciliation run.

Cleanup and rollback admit new runs. They retain the selected deployment/configuration and artifact identity but resolve authorized credential versions for the new run, recording those pins separately from history. Missing/revoked credentials fail; no fallback to arbitrary current settings or historical secret resurrection. An untracked deploy command is just a container attempt and makes no Kodama deployment-health claim.

## Gates and provenance

Use the existing one-reviewer gate and immutable decision. A versioned canonical subject can bind a source set and/or exact artifacts/release plus environment configuration and evidence. There is no new release approval engine. Source review requires applicable source evidence. Artifact/release review binds exact outputs and disclosed producer/input lineage; it does not assert the workspace was clean merely because the output has a digest.

Generated files remain legitimate shared state. Do not infer build provenance from `.gitignore`, a filename or a client-supplied “generated” label. Record observed workspace state and known input/output digests; absent evidence remains unknown. Edits to source invalidate source-bound approval; changes to the selected release/artifacts/configuration invalidate their approval. Existing immutable artifacts are not changed by later scratch writes.

## Version and migration checkpoint

T-140 must decide the smallest explicit version boundary for each incompatible shape (delivery requests, runtime context/provenance and gate subject), with old/new fixtures and refusal by unsupported consumers. Do not silently reinterpret v1/v2 context, change existing `ResultHash`, modify historical subject digests, or add new fields to frozen strict requests without versioning. Purely additive read/list routes can retain `/api/v1` compatibility.

Legacy image/Kubernetes records remain readable. Drain their attempts and cleanup obligations before removing old mutation routes, tool qualifications and adapters. Migration retains original IDs/pins/receipts; a legacy image deployment can project a one-artifact set without claiming a historical release existed. New generic runs use the new contract exclusively. Physical removal and rollback compatibility are explicit deliverables of T-145/T-146.
