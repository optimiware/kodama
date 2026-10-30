# Artifacts, releases and publication

> **Superseded for product direction by [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md).** Retained as the O-34 design history. The governing model is metadata-only external artifacts recorded in `artifacts`; release and promotion behavior is an ordinary pipeline plus role-inbox task, not a Kodama-hosted catalogue or receipt state machine.

**Scope:** historical direction under [ADR 0012](../adr/0012-container-sdlc-and-artifact-releases.md); superseded by ADR 0013. Existing verified file/OCI storage is implemented in `internal/artifacts`; release/version discovery, workload fetch and publication receipts are not carried into the ADR 0013 scope. [Contract](../contracts/artifacts-and-releases.md), [tasks and handoff](../implementation/container-sdlc-refinement.md).

## Purpose and small model

Retain selected pipeline outputs, identify a deployable set across runs and record where its copies are available. The artifact module stays in the existing server, with PostgreSQL metadata, S3-compatible file storage and standard OCI registries.

| Record | Identity and contents |
| --- | --- |
| Artifact | Tenant/project, immutable ID, producer run/step/attempt, output name, kind (`file` or `oci`), digest, media type, size where applicable, source/input provenance and verified storage reference |
| Release | Tenant/project, ID, name, version label, immutable manifest digest, named artifact ID/digest entries and source/producer references; retained or explicitly retired |
| Publication receipt | Tenant/project, artifact ID/digest, destination identity/reference/digest, producing run/step/attempt and verification time/evidence; an observation of a completed copy, not a durable availability promise |

An artifact's existing integer `version` is a concurrency revision. Release labels such as `1.4.2` or `build-318` identify application versions; Kodama does not generate or increment them. A release groups artifacts without copying their bytes. Source references allow an exact repository/commit set, compatible with the native Git design; absent source is explicit, not fabricated for source-free packaging.

## Storage and collection

- Binaries, archives, JSON/YAML/HTML, logs and reports are file artifacts. Collection remains explicit and bounded under `/kodama/outputs`; files in `/workspace` survive only for the run unless published to Git or retained as artifacts.
- Package directories with tar/zip or another ordinary tool. The first service does not host report websites or interpret coverage formats.
- Container images and OCI Helm charts live in registries. A chart `.tgz` can also be a file artifact. Preserve media types so clients can distinguish images, charts and other OCI content.
- A file becomes usable only after digest/size verification and sealing. An OCI registration verifies the addressed manifest and records whether verification also covered its referenced content. The existing manifest-only check must not be described as full pull/retention verification.
- An installation supplies registry access/policy. An optional packaged registry may follow; it uses standard OCI distribution and the same catalogue interface.

## Release creation and retention

Resolve the manifest from usable, same-project, accepted producer outputs. Validate each reference and its declared digest; retain exact source/input provenance even when outputs came from different runs or sources. A release is not a claim that all artifacts came from one commit or that any human approved it. Diagnostic-only outputs are not deployable entries; inspection links can remain evidence outside the deployable set.

The unique key is `(tenant, project, name, version)`. Identical canonical manifests return the existing release; different contents conflict. No mutable `latest`, channel, or promotion alias is required. Resolution for a run records the release ID/digest and exact artifact IDs/digests before use. Environment promotion reuses these identities.

Retain artifacts before acknowledging release creation, serialize pins against deletion and reconcile a failed/unknown multi-record commit without dropping a live reference. A release retirement changes retention eligibility, not its immutable manifest. An artifact remains protected while any retained run, gate, deployment, release or recovery window requires it. Keep metadata/tombstones after content expiry. The O-11 periods remain objectives to implement and qualify, not a claim that the current server already enforces them.

Registry retention is an external storage responsibility even when a registry is bundled. Record missing content and refuse use; never substitute a tag's new target. A database pin alone cannot prevent an external operator deleting an image or chart.

## Consumption by a later run

Within a run, use the shared workspace. Across runs, admit exact artifact/release references and grant only those inputs to the consuming attempt. Authorize access to the original record and retention state; an ID is not a capability. Fetch uses a short-lived scoped transfer and verifies the expected digest before making a local file available. A repeated fetch of identical content is safe; mismatch/missing content is a visible failure. The entrypoint does not implicitly extract archives or overwrite source. A script may explicitly copy/extract into its workspace using its chosen tooling.

OCI consumption yields a digest-pinned reference and authorized credential binding where declared. It does not imply that a registry is reachable from production. Existing human metadata/download access remains separate from workload input grants.

## Publication and external targets

An ordinary container copies content to target-accessible storage, then records a receipt. Secrets are declared through the same delivery path as any script. Destination origins/repositories/buckets must be authorized; server verification cannot become an arbitrary URL fetch or send credentials to an unapproved destination.

Prefer digest-preserving copies. Verification checks destination bytes, and for OCI indexes/manifests the referenced manifests/configs/layers needed for a complete pull. A transformation that changes content or manifest bytes creates a new artifact with explicit derivation; it is not a same-digest copy. Replays of the same receipt are idempotent; conflicting content is rejected. An interrupted copy has no verified receipt until observation establishes completion. It does not authorize automatic command replay.

A receipt's verification is time-bound evidence. Recheck required availability before deployment; missing copies are republished explicitly or the run fails. Production may use a private registry/bucket reachable only within its network. The deployment container can also transfer archives or apply a downloaded chart directly. Signed URLs grant access, never connectivity. No-network export/import and target-side runners need a later separate contract.

## Requirements

| ID | Requirement |
| --- | --- |
| AF-01 | Retained outputs MUST have immutable scoped identity, verified content digest and attributable producer/input provenance; metadata revisions MUST NOT be presented as release versions. |
| AF-02 | File collection and transfers MUST be explicit, bounded and path-confined; incomplete content MUST NOT become usable. Directories MAY be packaged by scripts. |
| AF-03 | Releases MUST bind named exact artifact IDs/digests; name/version reuse with different contents MUST conflict, and identical requests MUST converge. |
| AF-04 | Release creation and consumption MUST reject foreign, incomplete, expired or unaccepted deployable outputs and preserve mixed/source-free provenance accurately. |
| AF-05 | A consuming run MUST pin resolved release/artifact identities; a workload MUST read only authorized admitted inputs and verify fetched bytes before use. |
| AF-06 | Publication MUST be performed by ordinary steps; a receipt MUST distinguish artifact identity, destination and independently verified destination content. |
| AF-07 | A same-content publication receipt MUST cover the content needed to consume the artifact; OCI copies include referenced content, and changed digests MUST be represented as derived artifacts. |
| AF-08 | Retention MUST serialize with pins, protect retained releases/deployments/recovery references and preserve metadata after deletion; registry availability MUST NOT be inferred from a SQL pin. |
| AF-09 | Discovery, resolution, fetch, release creation/retirement and publication MUST enforce tenant/project/current authority, bounded lists and scoped idempotency. Receipts and events MUST omit credentials and transfer URLs. |
| AF-10 | Deployment/rollback MUST be able to consume retained exact outputs without rebuilding source or exposing Kodama storage to production; unavailable bytes MUST fail visibly. |

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| AF-A01 | A binary, chart archive and JSON/YAML/HTML reports are retained; digest/size/path mismatch and incomplete uploads fail | AF-01, AF-02 |
| AF-A02 | Concurrent identical release creation converges; changing one artifact under the same name/version conflicts | AF-03, AF-09 |
| AF-A03 | Mixed-run sources are recorded accurately; foreign, diagnostic-only, removed and unfinalized deployable entries fail | AF-01, AF-04 |
| AF-A04 | A later run fetches an admitted exact file; another artifact ID, revoked grant, changed bytes and expired transfer fail without exposing storage credentials | AF-05, AF-09 |
| AF-A05 | A normal publish container copies an image index and a chart to a second private registry; missing child manifests/layers, wrong digests and unapproved destinations prevent a receipt | AF-06, AF-07, AF-09 |
| AF-A06 | Release creation races retention and loses a response; restart preserves every committed reference and does not acknowledge a release with deleted content | AF-03, AF-04, AF-08 |
| AF-A07 | Retiring a release cannot remove an artifact still referenced by a deployment/gate/recovery window; external registry deletion is reported, not hidden | AF-08, AF-10 |
| AF-A08 | A target denied access to Kodama storage deploys from its own registry/bucket; a new rollback run reuses a prior release without a build | AF-05, AF-06, AF-10 |

## Unresolved implementation details

T-140 freezes executable schemas, canonical manifests and narrow workload operations before consumers change. Backend-specific full-copy verification and retention configuration are qualified by T-146. Optional bundled registry packaging, report-site hosting, package indexes and disconnected execution remain later scope, not blockers for configured-registry delivery.
