# Artifacts, releases, and promotions

**Scope:** Layer 2 metadata-only external artifacts and Layer 4 delivery/promotion recipes under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md) and [O-38](../decisions.md#pipeline-runtime-and-four-layer-refinement--2026-10-30-o-38).

## Small model

Kodama does **not** host production artifact blobs in S3 and does **not** maintain a separate `environments`, `deployments`, canonical-digest `gates`, release-receipt, or promotion state machine.

1. **Metadata-only artifacts (`artifacts`):**
   - Pipeline steps build and publish artifacts (OCI images, Helm charts, binaries, static bundles, preview deployments, test reports) directly to external registries or services using standard tools (`ko`, `buildah`, `crane`, `helm`, `wrangler`, `terraform`, `kubectl`) and declared secrets.
   - Kodama records metadata only in the `artifacts` table: `name`, `kind` (`image`, `package`, `preview`, `report`, `release`), external `uri`, immutable `digest`, `run_id`, `step_id`, `task_id`, and `metadata` (JSONB).
2. **Delivery is ordinary pipeline content:**
   - Preview, staging, production promotion, rollback, and cleanup are pipeline steps or separate pipelines. Their YAML, scripts, images, configuration, and credentials are Layer 4 behavior.
   - Deployment manifests live in Git (for example `<tenant>/<project>/deploy`).
   - Kodama records what a step reports; a successful container exit is not a claim that every external resource is healthy. Readiness, retries, and reconciliation are the recipe's responsibility, with conservative handling of unknown outcomes.
3. **Human approval is a pipeline wait:**
   - When a process requires human judgment, the pipeline declares a durable wait assigned to a role or correlation.
   - The role inbox lists tasks whose current wait expects that role. Resolution records an immutable input against the exact run, step, and wait; the pipeline selects the next branch.
   - A successful signal is not approval by itself. A rejection or answer cannot take an approval branch unless the pipeline explicitly expresses that path.
   - A changed candidate requires a new wait and a new applicable decision. Historical decisions remain immutable.

## Provenance

A build may consume files produced by earlier steps. Evidence records the source commits, the workspace state at command begin, prior-step lineage, tool and service image digests, and the pinned execution policy. A client cannot assert this metadata.

### Provenance in shared scratch (run_shared)

This anchor is retained for historical examples. The `run_shared` model and its command-begin provenance fence described the superseded dual-format execution stack. Current provenance follows the rules above; the [pipeline-runtime simplification plan](../implementation/pipeline-runtime-simplification.md) removes the legacy contracts.

- Evidence is candidate-bound only when the workspace is clean at the candidate commit. Modified evidence is ordinary diagnostic evidence and does not satisfy a candidate-bound requirement.
- A writer step commits generated changes, creating a new candidate that requires fresh evidence and a new applicable wait.
- Generated files may become retained artifacts without becoming Git commits. The provenance record states what actually produced them.
- Promotion may reuse a registered image digest independently of the run workspace.

## Multiple repositories and integration

A candidate may span several repositories. Evidence and wait inputs identify the run's pinned source set: each repository and exact commit. Delivery pins all commits together but does not claim atomic Git updates across repositories.

Integration is an ordinary pipeline step. The shell issues integration authority only for the configured integration pipeline, the eligible step, the exact default-branch ref, the expected old commit, and the new candidate, after any required wait/input is satisfied. A moved base requires a new candidate. The native Git worker independently checks current authority and fast-forward.

## Requirements

| ID | Requirement |
| --- | --- |
| DQ-01 | Source, artifact, evidence, and delivery references MUST share verified ownership and provenance. |
| DQ-06 | Automated QA MUST produce evidence and defects separately from human decisions. |
| DQ-07 | Builds MUST record source inputs, tool image digests, and output digests; promotion SHOULD reuse the same image. |
| DQ-10 | Tests and QA MUST identify the deployment or configuration inspected; findings are ordinary defect tasks with evidence links. |
| DQ-23 | A human approval MUST be a durable pipeline wait resolved against the exact run, step, and wait with current actor authority; a signal alone MUST NOT authorize a protected effect. |
| DQ-24 | A changed candidate, source set, or configuration MUST require a new applicable wait decision and MUST NOT reuse an earlier approval. |
| DQ-25 | Delivery recipes MUST record enough intent and outcome metadata to distinguish success from an unknown external effect; an unknown outcome MUST NOT be blindly replayed. |
| DQ-26 | Integration authority MUST be issued only for the configured integration pipeline, eligible step, exact ref, expected old commit, and candidate; a task status string or bare event MUST NOT authorize it. |

DQ-02–DQ-05, DQ-08–DQ-09, DQ-11, DQ-13–DQ-17, and DQ-A01–DQ-A17 are retired by the [ADR 0013 amendment](../adr/0013-two-layer-architecture-and-domain-simplification.md). They described the removed gate, deployment, release-receipt, and workflow-promotion subsystems. Historical acceptance reports that cite them remain historical evidence for the superseded model.

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| DQ-A18 | A build step publishes an image to an external registry and registers its URI and digest; a delivery step consumes that exact digest and records its outcome | DQ-01, DQ-07, DQ-25 |
| DQ-A19 | A promotion pipeline waits for a reviewer; one authorized approval resumes the same run, a duplicate or late signal changes nothing, and rejection follows the rejected branch without authorizing promotion | DQ-23, DQ-24, DQ-26 |
| DQ-A20 | A candidate changes after approval; the earlier decision cannot authorize the new candidate, and a new wait is required | DQ-24, DQ-26 |
