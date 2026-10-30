---
status: superseded by ADR 0013
---

# ADR 0012: Container-first SDLC and artifact releases

> **Superseded by [ADR 0013](0013-two-layer-architecture-and-domain-simplification.md) (2026-10-30).**
> ADR 0013 keeps this record's container primitive, metadata-only external
> artifacts and tool-neutral delivery. It replaces the artifact catalogue,
> release-receipt and versioned-compatibility scope with the two-layer model and
> the 15-table schema. The text below is retained as history, not current
> implementation direction.

**Date:** 2026-10-30

**Decision:** owner-approved in the artifact/design discussion; [O-34](../decisions.md#container-first-sdlc-and-artifacts--2026-10-30-o-34).

**Status:** superseded by [ADR 0013](0013-two-layer-architecture-and-domain-simplification.md). Historical: accepted direction with implementation and acceptance pending at the time of decision.

**Baseline:** `github.com/optimiware/kodama` at `2ff20310e4fd5005bfb5c5ba783113e9619fb076` (integrating [ADR 0011](0011-internal-tenant-git-service.md)). Implementation handoff: [container-first SDLC refinement](../implementation/container-sdlc-refinement.md).

**Owners:** [execution](../specs/pipeline-execution.md), [agents](../specs/agent-runtime.md), [artifacts and releases](../specs/artifacts-and-releases.md), [delivery](../specs/delivery-and-quality-gates.md).

## Context

Kodama covers the software lifecycle: native repositories and work tracking, agent-assisted development, CI, publication and deployment. Its useful execution primitive is a container image, a command, explicit secrets, a shared run workspace and declared outputs. ko, Buildah, Claude Code, OpenCode, Terraform, OpenTofu, Wrangler, kubectl and Helm can all use that primitive.

C-14 already makes build and deployment normal steps, but the implementation has accumulated narrower assumptions: every deployment has an OCI image, namespace and kubeconfig; provider-specific object/readiness callbacks are part of the core; managed agent qualification can look like a prerequisite for any harness. Artifact storage verifies files and OCI registrations, but lacks named releases and a convenient cross-run consumption/publication path. A metadata revision is not a software release version.

Production is commonly external to Kodama. Its workloads may be unable to reach Kodama's object store or registry. Retaining a build and distributing it to a deployment target are distinct responsibilities.

## Decision

### One executable primitive

Build, test, agent, publish, deploy, revert and cleanup commands run as ordinary container steps. Each uses the existing attempt identity, deadlines, resource/network policy, secret delivery, logs, outputs and cancellation. No tool-specific executor, deployment plugin framework or additional job lifecycle is introduced.

An agent profile is optional versioned convenience over that primitive. Managed native Go agents (packaged as OCI images with ko) and supported harness recipes may add context, tools, structured reports and mediated model access. A user-supplied harness can instead run directly with an explicitly bound API key. Generic container admission does not require a wrapper catalogue, model gateway or MCP proxy. Claims about proxy budgets, tool mediation and harness compatibility apply only to the path actually used. Human gates remain separate non-compute steps.

### Three lifetimes

- **Run workspace:** shared working files for one run; disposable. Files flow between steps without artifact upload/download within that run.
- **Git:** durable source history and coordination. Repository/ref authority follows the selected hosting contract; ADR 0011's internal grants and the legacy external publisher are not replaced here.
- **Artifacts:** explicitly retained outputs for inspection, later runs and release/deployment. A workspace is not automatically archived. Directories are packaged by ordinary tools.

Use “run workspace” for execution and “source workspace” for the legacy durable work-item/Git association. Preserve existing API identifiers during migration. Native Git's task-branch association remains the authority for its replacement.

### Small native artifact and release layer

Extend `internal/artifacts` in the existing server:

1. **Artifact:** immutable bytes or an OCI reference, with digest, media type, ownership and producer/input provenance. S3 holds files; a standard OCI registry holds images and OCI charts.
2. **Release:** a project-scoped name/version and immutable manifest of named artifact IDs/digests and source/producer references. Versions are supplied by the pipeline/user; SemVer is a convention, not a version-allocation service. Reusing a version with different contents conflicts.
3. **Publication receipt:** a verified destination copy of an artifact, including destination identity and digest. It does not move or change the original artifact and is not a second orchestration state machine.

Provide scoped discovery, exact resolution and download/fetch helpers. Retained releases/deployments pin their artifacts; expiry cannot silently remove their bytes. OCI references alone do not guarantee registry retention or the continued availability of manifests and layers.

The first implementation uses installation-configured OCI registries. Bundling an established private registry is an optional later packaging feature, not a new registry protocol/server implementation or a prerequisite for the release API. Reports remain ordinary downloadable artifacts; HTML site hosting and package-manager registries are later features.

### Tool-neutral deployment records

An environment is a named target with explicit configuration and secret bindings; Kubernetes namespace/kubeconfig are recipe configuration, not required domain fields. A deployment records its exact release or artifact set, source references, configuration digest, originating run/step/action and reported outcome. It may have an expiry and an explicit cleanup definition.

Commands may deploy without requesting a Kodama deployment record; they still have attempt logs/results. A step requesting tracked delivery records intention before the external mutation and reports attributable outcome/optional external references. Kodama records what the script observed; a generic successful exit is not a claim that every remote resource is healthy. Readiness and reconciliation are tool/script responsibilities.

Remove mandatory Kubernetes annotation/UID/readiness protocols and the global image/command qualification catalogue from the new generic path. Preserve their legacy readers/drain support until old runs and cleanup obligations settle. Do not add equivalent provider-specific requirements under new names.

### Build once, publish where reachable, deploy by identity

Publication is an ordinary command using tools such as crane, skopeo, oras, Helm or a storage CLI and declared secrets. A deployment container may fetch an internal chart/archive and upload/apply it remotely; production itself need not contact Kodama. Images normally go to a registry reachable by production, which can remain private. Publication verifies the destination digest and, for OCI content, the complete referenced content needed to pull it.

Rollback is a new pipeline run selecting an earlier release and an explicit configuration snapshot, using credentials authorized and pinned for the new run. It never rewrites historical records or resurrects revoked credentials. Reverting source is a Git change; redeploying a release is a delivery action. Neither promises automatic reversal of database/infrastructure changes. Terraform/OpenTofu live state remains in their locking-capable backend, not the artifact service.

### Provenance describes inputs, not a requirement to commit build products

Record source commits, producing pipeline/step images, prior-attempt lineage, observed source/workspace state and consumed artifact digests. A source commit alone is not proof of all build inputs. Generated files are legitimate run state and may become retained artifacts without becoming Git commits.

Keep one human gate mechanism. A source-candidate approval cannot be reused for changed source; an artifact/release approval binds exact artifact/manifest digests and their disclosed provenance. Generated files do not invalidate an already pinned immutable artifact. Unknown or modified source must not be relabeled as clean. The current blanket DQ-16 restriction is superseded for the new contract by DQ-18/DQ-19, with old subjects/hashes kept intact.

## Compatibility and reduction

The [implementation handoff](../implementation/container-sdlc-refinement.md) assigns the executable contract checkpoint, removal inventory and ordered task scopes. The checkpoint must version incompatible context, delivery and gate shapes before changing consumers. Keep historical definitions/results/decisions readable and drain active legacy effects; compatibility is bounded migration work, not a permanent second deployment engine.

This amends O-12's universal Kubernetes delivery requirement, the managed-agent-only reading of C-15/O-04, and ADR 0010's blanket scratch-cleanliness condition for evidence. It retains shared-workspace exclusion, tenant scope, scoped secrets, expected-tip Git writes, human completion and conservative handling of unknown external effects. It does not schedule native Git implementation, live providers, remote runners or air-gap support.

## Consequences and alternatives

- The core becomes smaller: generic execution plus durable metadata; tool ecosystems supply deployment behavior.
- Scripts own provider-specific readiness and recovery. No automatic replay of commands with unknown effects is inferred from a missing receipt or a failed process.
- Release retention and target-registry retention both matter for rollback. A URL, tag or database row is not a durable copy.
- A full artifact platform (package indexes, replication engine, registry implementation, HTML hosting) would add a new product; a manifest/catalogue over existing storage meets the immediate need.
- Keeping deployment tied to image + Kubernetes namespace would exclude binary, static-site and infrastructure workflows despite using the same container runtime.
- Removing all metadata would make repeatable promotion, audit and rollback difficult. Small immutable manifests and receipts retain those capabilities without interpreting Terraform, Helm or Wrangler.
