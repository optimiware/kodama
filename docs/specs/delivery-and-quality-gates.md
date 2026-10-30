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

Normative design; not delivered. In a shared run, files written by an earlier step can feed a later build, so evidence must say what it was built from. At command begin the entrypoint reports the workspace head and whether tracked files, untracked files and ignored files differ from it; the server records `workspaceState` (`clean`, `modified`, `none` for source-free) and `ignoredPresent` with the attempt and attaches them, with the input/head commits, prior-attempt lineage, each step's image digest, service image digests, the pinned policy digest and the builder mode, to every artifact and OCI registration the attempt produces ([begin fence](../contracts/runtime-v2.md#command-begin-fence)). A client cannot assert this metadata.

- Evidence is bound to a candidate only when its `workspaceState` is `clean` at the candidate commit. `modified` evidence remains ordinary, visibly labeled diagnostic evidence and does not satisfy candidate-bound requirements (DQ-14, protected successors); a changed tree cannot masquerade as an unchanged approved commit.
- A writer step commits generated changes, which creates a new candidate that requires fresh applicable evidence and invalidates an old approval through the existing applicability rules (DQ-04, DQ-05). Gate subjects keep their existing shape; the gate verifies the evidence's recorded provenance at request and at progression (DQ-01).
- `ignoredPresent` (for example a restored dependency cache) is disclosed with the evidence and does not block by default; an installation or project may later require it false.
- Promotion reuses a registered image digest independently of scratch. Dirty/generated build inputs are accurately recorded, never hidden.

## Multiple repositories and integration (internal hosting, designed)

Under [ADR 0011](../adr/0011-internal-tenant-git-service.md) a candidate may span several repositories. The subject then carries `sourceSet`: the digest of the run's pinned set and, for each repository sorted by repository ID, its candidate commit and, for a repository the run may write, the integration base (the default branch and its tip when the gate was requested). `candidateCommit` stays in the public representation and presents the primary source's commit (the first writable source in repository order, else the first); it never defines the subject. A single-repository subject is unchanged. Evidence, OCI provenance and deployments record the same set. Delivery of a set pins all its commits together; it does not claim atomic Git updates across repositories.

[Integration](git-repositories.md#integration) uses the existing gate: the gate's subject names the integration candidate and its expected base; approval lets the run's integration step receive a grant for exactly that default-branch update. A moved base invalidates the approval through the existing applicability checks (DQ-04, DQ-05). An approval is applicable only while the default branch is at its base. Once the gate step has passed, the approval is consumed: the branch moving to the candidate is the integration's success, not a moved base. An integration grant is issued only for a gate that a person approved and who still holds the reviewer authority; a branch at the candidate is answered "already integrated", and any other branch needs a new candidate.

## Requirements

| ID | Requirement |
| --- | --- |
| DQ-01 | Source, artifact, evidence, and deployment references MUST share verified ownership/provenance. |
| DQ-02 | A gate MUST snapshot the canonical subject and required evidence. |
| DQ-03 | A decision MUST check human role, expected gate version, and subject digest, then remain immutable. |
| DQ-04 | Progression MUST recheck current content/source, expiry, reviewer authority, and deployment configuration. |
| DQ-05 | Changed requirements/candidate MUST invalidate old approval for new actions without changing historical results. |
| DQ-06 | AI QA MUST produce evidence/defects separately from human decisions. |
| DQ-07 | Builds MUST record source/tool image/output digest; promotion SHOULD reuse the same image. |
| DQ-08 | Deployment rows MUST exist before external apply and provide duplicate/unknown-outcome reconciliation identity. |
| DQ-09 | Preview expiry MUST trigger idempotent cleanup and retain delivery history. |
| DQ-10 | Tests/QA MUST identify the deployment/configuration inspected; findings are defect work items with evidence links. |
| DQ-11 | Rejection MUST preserve evidence and require explicit rework/new applicable review. |
| DQ-13 | Completion MUST be an explicit authorized human transition with current content/candidate and a reason. |
| DQ-14 | Agent-produced software candidates MUST include candidate-bound design conformance evidence in the gate subject; missing/stale evidence or unresolved material deviations MUST block approval/progression. |
| DQ-15 | A declared step environment/configuration and its credential versions MUST be pinned with run admission and preserved in runtime context across edits/retries/restart; delivery/cleanup MUST reject missing, foreign or mismatched pins rather than resolve current settings as a substitute. |
| DQ-16 | `[run_shared]` Evidence produced in shared scratch MUST record source commits, workspace state at command begin, prior-attempt lineage and tool/service/builder digests. Evidence that is not `clean` at the candidate commit MUST NOT satisfy candidate-bound gate or protected-successor requirements or be presented as built from the unchanged approved candidate; changed scratch or candidate MUST invalidate old approval through existing applicability checks. |
| DQ-17 | `[internal hosting]` Gate subjects, evidence and deployments for a multi-repository candidate MUST identify every repository and commit of its source set (deployment records carry the set beside the primary source's repository and commit); an integration grant MUST be issued only for the default-branch update named by an approved, still-applicable subject. |

DQ-12 (generic overrides) is retired. Already-started external actions cannot be undone by invalidation; record them and use explicit remediation. Runtime decisions use current authority, not a historical event as permission.

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| DQ-A01 | A build step pushes an image to an external registry and registers its URI and digest in `artifacts` | DQ-01 |
| DQ-A02 | Preview pipeline parks the task and creates a `promotion` task in the `reviewer` inbox; resolving it triggers `.kodama/promote.yaml` | DQ-02, DQ-03, DQ-04 |
