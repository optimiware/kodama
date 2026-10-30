# Artifacts, releases, and promotions

**Scope:** Layer 1 metadata-only external artifacts (`artifacts`), container-step deployments/releases, and role-inbox promotion approvals ([ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md)).

## Small model

Under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md), Kodama does **not** host production artifact blobs in S3 and does **not** maintain a separate `environments`, `deployments`, or canonical-digest `gates` subsystem:

1. **Metadata-only artifacts (`artifacts`):**
   - Pipeline steps build and publish artifacts (OCI container images, Helm charts, binaries, static bundles, preview deployments, test reports) directly to external registries or services using standard tools (`ko`, `buildah`, `crane`, `helm`, `wrangler`, `terraform`, `kubectl`) and mounted project `secrets`.
   - Kodama records **metadata only** in the `artifacts` table: `name`, `kind` (`image`, `package`, `preview`, `report`, `release`), external `uri`, immutable `digest`, `run_id`, `step_id`, `task_id`, and `metadata` (JSONB).
2. **Releases and deployments are ordinary pipeline steps:**
   - Preview deployments (`.kodama/preview.yaml`), staging deployments, and production promotions (`.kodama/promote.yaml`) are ordinary YAML pipelines triggered by NATS signals (`git.tag`, `task.transitioned`, `task.resolved`).
   - Deployment configuration and manifests live in Git (for example `<tenant>/<project>/deploy`).
3. **Human validation and promotion via Role Inbox:**
   - Most of the SDLC (`plan -> implement -> review -> test -> deploy_preview`) is validated autonomously by Layer 2 `kodama agent` guardrails and test steps.
   - When human approval is required (typically for **production promotion** or **incident repair**), the pipeline parks the task (`state: parked`) and creates a `promotion` or `decision` task assigned to a **role inbox** (`assigned_role: reviewer` or `operator`), referencing the candidate commit and `artifacts` metadata.
   - When a reviewer resolves the inbox task via `kodactl`, MCP, or API, NATS relays `task.resolved`, unblocking the task and triggering `.kodama/promote.yaml`.

## Provenance in shared scratch (`run_shared`)

Every recorded `artifacts` row links to its producing `run_id`, `step_id`, `task_id`, repository commit SHA, and external `digest` (`sha256:...`), providing end-to-end provenance from task -> Git commit -> pipeline step -> external artifact URI.

## Requirements

| ID | Requirement |
| --- | --- |
| DQ-01 | Production artifacts MUST be published to external registries/services by pipeline steps; Kodama MUST persist metadata, external URI, digest, and run/step/task provenance in `artifacts`. |
| DQ-02 | Layer 2 validation gates (filtering, guardrails, test verification) MUST execute as container/agent steps that transition tasks autonomously on pass or fail. |
| DQ-03 | Human promotion and validation gates MUST use `parked` tasks blocked by role-inbox tasks (`kind: promotion \| decision`), without holding idle container compute. |
| DQ-04 | Resolving a promotion task in the role inbox MUST verify the caller's role and emit a NATS signal to trigger the promotion pipeline. |

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| DQ-A01 | A build step pushes an image to an external registry and registers its URI and digest in `artifacts` | DQ-01 |
| DQ-A02 | Preview pipeline parks the task and creates a `promotion` task in the `reviewer` inbox; resolving it triggers `.kodama/promote.yaml` | DQ-02, DQ-03, DQ-04 |
