---
status: proposed
---

# ADR 0011: Provider-neutral deployment artifacts, with OCI as one kind

**Date:** 2026-10-29  
**Status:** proposed. This record changes the recommended delivery model; it does not change the current executable contract.  
**Repository baseline:** `github.com/optimiware/kodama` at `9b582a43`.  
**Affected owners:** [delivery and quality gates](../specs/delivery-and-quality-gates.md), [HTTP contract](../contracts/http-api.md), [pipeline format](../contracts/pipeline-format.md), and the delivery/artifacts packages.  
**Related decisions:** [ADR 0006](0006-hexagonal-domain-design.md) (consumer-owned ports), [ADR 0009](0009-environments.md) (environment-selected adapters).

## Context

Delivery currently models the deployable output as an image:

- `delivery.Image` is an artifact ID plus digest, and `VerifyImage` requires an OCI artifact with the `output` role, a usable upload, the requested digest, the deployment source commit, and an accepted `ociArtifact` output from the producing attempt ([`internal/delivery/provenance.go`](../../internal/delivery/provenance.go), [`registration.go`](../../internal/delivery/registration.go)).
- A deployment record stores `Image` and calls the producer's tool image part of its provenance ([`internal/delivery/deployment.go`](../../internal/delivery/deployment.go)).
- The pipeline format exposes `ociArtifact` as a step output type; a later step cannot consume that output directly, and the documented workaround is a separate deploy run receiving the artifact ID, digest and registry reference as inputs ([`docs/contracts/pipeline-format.md`](../contracts/pipeline-format.md), [`docs/examples/blueprints/go-postgres.md`](../examples/blueprints/go-postgres.md)).
- Deployment records additionally require a Kubernetes namespace and Kubernetes object references, so the current executable target is Kubernetes-specific. That target limitation is separate from this decision.

This shape is correct for a container image, but it conflates three different things:

1. **Distribution format:** an OCI image or OCI artifact in a registry.
2. **Deployment content:** a runnable image, a Worker bundle, a static-site bundle, or another provider-specific package.
3. **Deployment target:** Kubernetes, Cloudflare Workers/Pages, or another provider that knows how to install that content.

A Next.js application deployed to Cloudflare Workers is a concrete example. The build produces `.open-next/worker.js` plus assets, not a runnable OCI image. Recording that bundle as an “image”, or forcing it through an OCI layer only to extract it again, misstates what was built and what the provider actually deployed. It also prevents a provider-neutral promotion model: stage and production should share one immutable artifact digest even when the target is not a container runtime.

OCI remains an excellent artifact distribution mechanism. The problem is making it the only deployment semantic.

## Decision

### 1. Delivery owns a provider-neutral deployment artifact

Replace the OCI-only `Image` value at the delivery boundary with a `DeploymentArtifact` value. The initial shape is:

```json
{
  "artifactId": "art_01",
  "kind": "worker-bundle",
  "digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "mediaType": "application/vnd.optimi.website.bundle.v1+tar+zstd",
  "sizeBytes": 12345678,
  "source": {
    "repositoryUrl": "https://git.example.invalid/acme/website.git",
    "commit": "1111111111111111111111111111111111111111"
  },
  "producer": {
    "attemptId": "att_01",
    "runId": "run_01",
    "stepId": "build",
    "toolImage": "registry.example.invalid/website-build@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  }
}
```

Required invariants:

- `artifactId`, `kind`, `digest`, `mediaType`, `source` and `producer` are server-verified, never caller-asserted.
- `kind` is an open, validated vocabulary. Initial kinds are `oci-image`, `oci-artifact`, `worker-bundle` and `static-bundle`; an installation or target adapter may support only a subset.
- `oci-image` and `oci-artifact` preserve today's behavior for container and OCI targets.
- `worker-bundle` and `static-bundle` are deployment content, not container images. Their media types and manifest layouts are owned by the target adapter, not by the delivery module.
- Artifact retention, source provenance and producer acceptance apply equally to every kind.
- The artifact kind never grants target authority. A recognized `worker-bundle` is not proof that a project may deploy to Cloudflare; the target adapter and current authorization still decide that.

`VerifyImage` becomes `VerifyArtifact`, or is replaced by an artifact verifier plus kind-specific validators. The old name must not quietly accept non-image content.

### 2. Target adapters declare the artifact kinds they accept

An environment/target adapter publishes the artifact kinds it can deploy. Admission and deployment registration fail closed when the target does not accept the requested kind. This keeps OCI and non-OCI targets from silently interpreting each other's content.

Examples:

| Target | Accepted kinds |
| --- | --- |
| Kubernetes | `oci-image` |
| Cloudflare Workers | `worker-bundle` (and later `static-bundle` for a Pages target) |
| A static object-store target | `static-bundle` |

The adapter owns provider-specific verification and observation. This ADR does not select a Cloudflare target model; it removes the artifact model's assumption that every deployable thing is an OCI image.

### 3. Build output and run handoff

The pipeline contract gains a deployment-artifact output in a versioned format. The existing `ociArtifact` spelling remains unchanged for `kodama/v1alpha1` and its historical definitions; it must not be reinterpreted as “any artifact”.

A build step declares the output and returns its identity in the result manifest. The artifact is stored according to its kind:

- OCI images and OCI artifacts remain in the OCI registry.
- Worker bundles and static bundles use the existing S3-backed artifact store, with a short-lived scoped download for the deploy step.

A deploy run receives, at minimum:

- `artifactId` as an `artifact` input;
- `digest` as a verified string input;
- `mediaType` or a stable kind label.

The server pins these values in the run input snapshot. The deploy step must be able to obtain the content by the recorded identity: a registry pull for OCI kinds, or a scoped artifact download for bundle kinds. A step receiving only an opaque artifact ID is not enough.

The deploy adapter rechecks that the content it obtained matches the pinned digest and source before applying anything. It records the provider's deployment/version identity separately from the artifact digest.

### 4. API compatibility

The public API adds a generic `artifact` member to deployment creation and deployment reads. During the compatibility window:

- OCI deployments continue to accept and return the existing `image` member.
- New non-OCI deployments require `artifact`.
- A request naming both must be accepted only when they describe the same artifact; conflicting values are refused.
- Existing clients that understand only `image` keep working for OCI targets.

The exact deprecation/removal point belongs to the API versioning policy, not this ADR. No existing declared client is broken by an additive member.

### 5. Provenance is unchanged in strength

The following checks remain mandatory for every artifact kind:

- the artifact belongs to the tenant/project;
- it is a usable output of the attempt/run/step named by the producer;
- it was produced from exactly the deployment's source commit;
- the requested digest matches the stored artifact;
- the artifact is still retained or protected against cleanup;
- the attempt's admitted context names the artifact as an input;
- the target accepts the artifact kind.

A caller cannot assert a digest, media type or producer. A step cannot promote an artifact the run was not given. An artifact cannot satisfy candidate-bound evidence merely because it exists in the project.

### 6. Promotion and storage semantics

- Stage and production consume the same `artifactId` and digest, even when their target configuration differs.
- A target adapter may always re-fetch the recorded digest; it must not rebuild or mutate the artifact during promotion.
- Retention pins are keyed by deployment identity and apply to every artifact kind. Cleanup of a bundle follows the same retention and audit rules as an OCI image.
- The HTTP API exposes the artifact kind and media type so an operator can see whether a deployment used an image, a Worker bundle or a static bundle.
- The provider's version/deployment ID is recorded alongside the artifact; it is not the artifact digest.

## Consequences

### Positive

- Non-container targets can be deployed without pretending their content is an OCI image.
- Build and deploy remain separate, independently retryable runs with a content-addressed handoff.
- Stage-to-production promotion can reuse one artifact digest across target types.
- OCI remains available and unchanged for Kubernetes and other container targets.
- Retention, provenance and admission checks stay in one delivery model instead of being duplicated by provider-specific deployment records.
- The model matches the existing specification language that builds record source, tool image and output digest; “output” no longer has to mean “image”.

### Negative

- More artifact kinds mean more validation branches and a larger adapter contract test matrix.
- The pipeline format needs a versioned addition; it cannot simply rename `ociArtifact`.
- The public API needs an additive compatibility window with two members for OCI deployments.
- Bundle artifacts need a scoped runtime download path or an OCI/ORAS bridge; neither is currently implemented.
- A generic artifact does not by itself make a target deployable. The Cloudflare/Kubernetes target models still need their own adapters, observation and cleanup.

## Alternatives considered

1. **Keep OCI-only and wrap every output as an OCI artifact.** Rejected as the domain model: Cloudflare does not consume OCI, extraction is required, registries vary in artifact support, and audit records would call a Worker bundle an image. This remains a valid interim transport.
2. **Give each provider its own deployment record and artifact type.** Rejected: it duplicates provenance, retention and promotion rules, and makes cross-target promotion impossible.
3. **Replace OCI with only file artifacts.** Rejected: container targets need OCI images and registry verification. OCI should be one kind, not the only kind.
4. **Store a raw URL or arbitrary reference.** Rejected: a URL is not an owned artifact, cannot prove producer acceptance or source provenance, and is mutable.
5. **Make the target adapter verify provenance itself.** Rejected: provenance and artifact ownership belong to the delivery/artifact boundary; adapters must not reimplement or weaken them.

## Acceptance criteria for implementation

- Existing OCI image deployments produce the same verification outcomes and remain readable through the `image` API member.
- A `worker-bundle` or `static-bundle` produced by a successful attempt is deployable only by a target that declares support for that kind.
- A missing, foreign, incomplete, wrong-source, wrong-digest or unsupported-kind artifact is refused with the same non-disclosing provenance failure used today.
- Stage and production can consume the same artifact ID and digest; no rebuild is required for promotion.
- A deploy step can obtain bundle content by the recorded identity without receiving broader project or tenant authority.
- Retention and cleanup preserve or release every artifact kind by the same deployment identity.
- No call path lets an artifact kind bypass candidate-bound gates, current authorization or target admission.
- API compatibility tests cover an old OCI client, a new bundle client, and a request that supplies conflicting `image` and `artifact` members.

## Follow-up owners

This ADR deliberately stops at the artifact model. Separate decisions still own:

- the generic deployment target/environment model (provider, namespace/target fields and external resource identities);
- the Cloudflare Workers target adapter, preview cleanup and rollback;
- named egress profiles or an egress proxy for build and deploy network access;
- per-environment promotion permissions and protected environments.

The first implementation slice should add the generic value and verifier, keep OCI behavior identical, and prove one non-OCI bundle end to end before changing the pipeline or public API versions.
