# Delivery and quality gates

**Scope:** M2 build/preview and one-reviewer gates; M3 stage, E2E, exploratory QA, and human acceptance.

**ADR 0012 refinement:** [container-first delivery](../adr/0012-container-sdlc-and-artifact-releases.md) is accepted direction, pending T-140–T-146. The Kubernetes/image protocol below describes the legacy executable path; the [generic contract](../contracts/artifacts-and-releases.md#generic-environment-and-deployment) supersedes that specialization for new-format delivery. Existing records, runs and gate hashes retain their admitted semantics.

## Small model

A candidate is a source commit, scoped by tenant/project/repository. Evidence is an artifact reference with producer/provenance and a kind. A gate stores one subject, `pending|approved|rejected`, decision actor/reason, expiry, and optional invalidation timestamp. A decision is immutable once recorded. Applicability is rechecked; expired/superseded is a reason, not another status machine.

The current source-candidate subject shape is below. ADR 0012's later version adds exact artifact/release binding through the same gate mechanism; it does not rewrite this shape's historical hashes:

```json
{
  "tenantId": "t_acme",
  "projectId": "p_inventory",
  "workItemId": "wi_1042",
  "contentRevision": 2,
  "candidateCommit": "1111111111111111111111111111111111111111",
  "definitionId": "pd_01",
  "evidence": [{"id": "art_tests", "digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],
  "deploymentId": null,
  "deploymentConfigDigest": null
}
```

Use UTF-8 canonical JSON with lexicographically ordered keys, evidence sorted by `(id,digest)`, explicit nulls, and no floating-point values; hash with SHA-256. The server constructs the subject. Including content revision and deployment configuration prevents an old approval being reused for changed requirements or environment contents.

One human with `reviewer` decides. There are no quorum, override, or configurable gate-policy resources. Initial rules permit an author who also has reviewer authority; stricter separation is a future explicit feature.

For managed agent profiles or projects that require it, evidence includes the [design conformance report](../implementation/design-conformance.md) for the candidate and its pinned design baseline. It is an ordinary artifact covered by the evidence ID/digest, not another approval resource. Generic container execution does not inspect a command to decide whether it is an agent or impose that report on all scripts. Existing profile requirements remain pinned. Unresolved material deviations block progression where conformance is required; an accepted contract change needs updated authoritative sources and fresh applicable evidence.

## Build and deployment are normal steps

### Legacy Kubernetes recipe

Build scripts run in rootless/restricted containers (`attempt_private`) or under the pinned rootless-builder [execution policy](../contracts/run-resources.md#execution-policy) (`run_shared`, designed and unqualified; ko, Buildah and a BuildKit mode are supported only per their qualification evidence), register verified image digests, and emit ordinary outputs. Deploy scripts use a project-owned namespace-scoped kubeconfig secret. An environment is a namespace/target, credential reference, explicit runtime configuration, and preview TTL.

Before applying external changes, a script creates a deployment row through the scoped interface using its stable step action key. That row is the durable intention; no extra intent/action subsystem exists. It contains candidate, artifact digest, configuration digest, environment, originating attempt, external resource identities, and result. The script applies manifests annotated with deployment/action identity, then reports readiness. Duplicate/unknown calls observe those identities before applying again.

Deployment status uses `pending|running|succeeded|failed` plus `reason`, `expires_at`, and `deleted_at`. `reason=outcome_unknown` blocks automatic repeat. Cleanup is a narrowly authorized ordinary container step launched as an internal project cleanup run; it deletes recorded owned objects and sets `deleted_at`. It cannot delete arbitrary tenant resources.

The declared environment is pinned at admission, not the first deployment callback. Delivery publishes immutable configuration; execution persists authorized identity/exact credential versions into runtime context. Later edits cannot rewrite admitted inputs; current revocation still governs new use. The [pinning contract](../implementation/environment-pinning.md) assigns execution to T-076, records/provider to T-035, unit-tested binding to T-077, wiring to T-072 and actual SQL/cleanup qualification to T-067/T-038 before M2 acceptance. Cleanup uses the recorded deployment's retained configuration.

### Generic delivery target (ADR 0012)

Build, publish, deploy, rollback and cleanup are ordinary containers plus explicit secrets. A command may use Terraform/OpenTofu, Wrangler, kubectl/Helm, storage CLIs or any other tool compatible with the runtime policy. The core does not parse their plans, implement provider-specific apply/readiness logic or require a qualified delivery-tool image catalogue.

An environment is a named target with configuration and credential bindings. An optional expiry/cleanup definition supports previews; `preview`/`stage` is no longer an exhaustive target taxonomy. Namespace and kubeconfig are optional recipe data. A tracked deployment binds an exact [release or artifact set](artifacts-and-releases.md), source references, configuration, script/definition identity and action key before mutation; infrastructure-only delivery can have no build artifact. Outcome and external observations come from the script. An ordinary untracked command needs no deployment callback and retains its attempt evidence.

Reconciliation belongs to the tool/script or an authorized operator. Missing receipts and process failure never prove absence of external effects; unknown outcomes remain blocked from automatic replay. Optional cleanup remains an ordinary deployment-bound run. New cleanup/rollback runs use the retained target/configuration and artifacts with freshly authorized, newly pinned credential versions; original run pins/history are unchanged. Rollback is another recorded deployment, not a history rewrite or an automatic database/infrastructure undo.

Production need not reach Kodama: publish to its accessible private registry/bucket, or let the deployment container fetch internal outputs and transfer/apply them. Registry availability/retention and verified publication are governed by AF-06–AF-10. Neither a source rebuild nor a floating tag replaces an unavailable approved artifact.

## Provenance in shared scratch (`run_shared`)

**Legacy DQ-16 semantics; retained until the ADR 0012 contract cutover.** In a shared run, files written by an earlier step can feed a later build, so evidence must say what it was built from. At command begin the entrypoint reports the workspace head and whether tracked files, untracked files and ignored files differ from it; the server records `workspaceState` (`clean`, `modified`, `none` for source-free) and `ignoredPresent` with the attempt and attaches them, with the input/head commits, prior-attempt lineage, each step's image digest, service image digests, the pinned policy digest and the builder mode, to every artifact and OCI registration the attempt produces ([begin fence](../contracts/runtime-v2.md#command-begin-fence)). A client cannot assert this metadata.

- Evidence is bound to a candidate only when its `workspaceState` is `clean` at the candidate commit. `modified` evidence remains ordinary, visibly labeled diagnostic evidence and does not satisfy candidate-bound requirements (DQ-14, protected successors); a changed tree cannot masquerade as an unchanged approved commit.
- A writer step commits generated changes, which creates a new candidate that requires fresh applicable evidence and invalidates an old approval through the existing applicability rules (DQ-04, DQ-05). Gate subjects keep their existing shape; the gate verifies the evidence's recorded provenance at request and at progression (DQ-01).
- `ignoredPresent` (for example a restored dependency cache) is disclosed with the evidence and does not block by default; an installation or project may later require it false.
- Promotion reuses a registered image digest independently of scratch. Dirty/generated build inputs are accurately recorded, never hidden.

**New contract (DQ-17/DQ-18).** Retain those observations, but separate source approval from artifact/release approval using the same gate mechanism. Generated build products may flow through shared scratch and become declared artifacts without being committed. A gate reviewing exact artifacts/release binds their digests, producer/input lineage and disclosed workspace state; it does not relabel modified or unknown source as the reviewed clean candidate. Candidate-bound source evidence still requires a matching source state. A later scratch edit cannot mutate an already sealed artifact or release; changing the selected artifact/manifest, source subject or environment configuration requires fresh applicable approval. No classifier guesses source versus generated data from ignore rules or filenames. [The contract checkpoint](../contracts/artifacts-and-releases.md#gates-and-provenance) versions the subject and evidence rules explicitly.

## Requirements

| ID | Requirement |
| --- | --- |
| DQ-01 | Source, artifact, evidence, and deployment references MUST share verified ownership/provenance. |
| DQ-02 | A gate MUST snapshot the canonical subject and required evidence. |
| DQ-03 | A decision MUST check human role, expected gate version, and subject digest, then remain immutable. |
| DQ-04 | Progression MUST recheck current content/source, expiry, reviewer authority, and deployment configuration. |
| DQ-05 | Changed requirements/candidate MUST invalidate old approval for new actions without changing historical results. |
| DQ-06 | AI QA MUST produce evidence/defects separately from human decisions. |
| DQ-07 | Builds MUST record source/input provenance, tool image and output digests; promotion SHOULD reuse the same immutable outputs. |
| DQ-08 | Tracked deployment rows MUST exist before external apply and provide duplicate/unknown-outcome reconciliation identity; an ordinary untracked command makes no deployment-record claim. |
| DQ-09 | Preview expiry MUST trigger idempotent cleanup and retain delivery history. |
| DQ-10 | Tests/QA MUST identify the deployment/configuration inspected; findings are defect work items with evidence links. |
| DQ-11 | Rejection MUST preserve evidence and require explicit rework/new applicable review. |
| DQ-13 | Completion MUST be an explicit authorized human transition with current content/candidate and a reason. |
| DQ-14 | When the pinned managed profile/project requires design conformance, the gate MUST include candidate-bound conformance evidence; missing/stale evidence or unresolved material deviations MUST block approval/progression. Generic containers MUST NOT acquire this requirement merely from the command's tool name. |
| DQ-15 | A declared step environment/configuration and its credential versions MUST be pinned with run admission and preserved in runtime context across edits/retries/restart; delivery/cleanup MUST reject missing, foreign or mismatched pins rather than resolve current settings as a substitute. |
| DQ-16 | `[legacy run_shared; superseded by DQ-17/DQ-18 after ADR 0012 cutover]` Evidence produced in shared scratch MUST record source commits, workspace state at command begin, prior-attempt lineage and tool/service/builder digests. Evidence that is not `clean` at the candidate commit MUST NOT satisfy candidate-bound gate or protected-successor requirements or be presented as built from the unchanged approved candidate; changed scratch or candidate MUST invalidate old approval through existing applicability checks. |
| DQ-17 | `[ADR 0012]` Evidence MUST disclose source/input and producer lineage; generated files MAY be retained/consumed without Git publication. Modified/unknown source MUST NOT be asserted as an unchanged clean candidate. |
| DQ-18 | `[ADR 0012]` The existing gate mechanism MUST bind the actual source and/or artifact/release subject, evidence and environment configuration; changed bound inputs MUST invalidate applicability. Scratch changes alone MUST NOT change an immutable artifact's identity. |
| DQ-19 | `[ADR 0012]` Environments/deployments MUST support tool-neutral targets and exact releases/artifact sets, including infrastructure-only actions, without mandatory image, namespace, kubeconfig or Kubernetes observation fields. |
| DQ-20 | `[ADR 0012]` Tracked delivery MUST record intent before mutation and attributable outcome; tool-specific readiness/reconciliation MUST remain script-owned. Unknown effects MUST NOT trigger blind replay or inferred success. |
| DQ-21 | `[ADR 0012]` Rollback/cleanup MUST be new explicitly bound runs using retained target/configuration/artifact identity and currently authorized credential pins; historical outcomes/secret pins MUST remain unchanged. |

DQ-12 (generic overrides) is retired. Already-started external actions cannot be undone by invalidation; record them and use explicit remediation. Runtime decisions use current authority, not a historical event as permission.

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| DQ-A01 | Cross-tenant or wrong-commit evidence fails review | DQ-01, DQ-02 |
| DQ-A02 | Concurrent decisions enforce version/role and preserve one immutable result | DQ-03, DQ-06 |
| DQ-A03 | Changed spec/source/config or removed reviewer blocks stale approval | DQ-04, DQ-05 |
| DQ-A04 | Preview/stage reuse a verified digest with explicit configuration | DQ-07 |
| DQ-A05 | Lost apply response reconciles the existing deployment row/objects | DQ-08 |
| DQ-A06 | Expiry cleanup is scoped, repeatable, and retains inspected evidence | DQ-09, DQ-10 |
| DQ-A07 | AI QA files a defect; human rejection leads to fresh review | DQ-06, DQ-10, DQ-11 |
| DQ-A09 | Only a current human reviewer can complete work; passing jobs cannot | DQ-13 |
| DQ-A10 | Where the pinned profile/project requires conformance, missing, wrong-candidate, stale-baseline or materially unresolved evidence blocks approval/progression; a reviewed contract update and fresh report can satisfy the subject | DQ-01, DQ-02, DQ-04, DQ-14 |
| DQ-A11 | Environment/credential changes after admission do not alter the persisted context; real callbacks/cleanup remain scoped to the recorded configuration, and partial admission/pin failures roll back together | DQ-01, DQ-04, DQ-08, DQ-15 |
| DQ-A12 | `[run_shared]` A build from modified or generated scratch is labeled and cannot satisfy candidate-bound evidence; clean evidence, source/tool/service/image provenance and gate invalidation after a writer's new commit remain correct | DQ-01, DQ-04, DQ-05, DQ-07, DQ-14, DQ-16 |
| DQ-A13 | A build/test chain consumes generated files without committing them; a gate binds exact outputs and provenance, while changed source cannot borrow a clean-candidate approval | DQ-01, DQ-17, DQ-18 |
| DQ-A14 | Ordinary Helm and non-Kubernetes upload/deploy scripts use the same tracked record protocol; an infrastructure-only deployment needs no OCI image, and an untracked command needs no delivery callbacks | DQ-08, DQ-19, DQ-20 |
| DQ-A15 | Lost apply/report response preserves one intent with unknown outcome; a fresh observation settles it and no absent receipt authorizes replay | DQ-08, DQ-20 |
| DQ-A16 | A rollback reuses prior output digests/configuration without rebuilding; credential rotation is explicitly pinned for the new run, revocation denies use, and history is unchanged | DQ-15, DQ-21 |

DQ-A08 is retired with the override engine.
