# Delivery and quality gates

**Scope:** M2 build/preview and one-reviewer gates; M3 stage, E2E, exploratory QA, and human acceptance.

## Small model

A candidate is a source commit, scoped by tenant/project/repository. Evidence is an artifact reference with producer/provenance and a kind. A gate stores one subject, `pending|approved|rejected`, decision actor/reason, expiry, and optional invalidation timestamp. A decision is immutable once recorded. Applicability is rechecked; expired/superseded is a reason, not another status machine.

One subject shape is used everywhere:

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

For agent-produced software changes, required evidence includes the [design conformance report](../implementation/design-conformance.md) for the candidate and its pinned design baseline. It is an ordinary artifact covered by the existing evidence ID/digest in the canonical subject, not another approval resource. Unresolved material deviations block progression. An accepted contract change needs updated authoritative sources and fresh applicable evidence; human approval cannot silently waive a conflicting contract.

## Build and deployment are normal steps

Build scripts run in rootless/restricted containers (`attempt_private`) or under the pinned rootless-builder [execution policy](../contracts/run-resources.md#execution-policy) (`run_shared`, designed and unqualified; ko, Buildah and a BuildKit mode are supported only per their qualification evidence), register verified image digests, and emit ordinary outputs. Deploy scripts use a project-owned namespace-scoped kubeconfig secret. An environment is a namespace/target, credential reference, explicit runtime configuration, and preview TTL.

Before applying external changes, a script creates a deployment row through the scoped interface using its stable step action key. That row is the durable intention; no extra intent/action subsystem exists. It contains candidate, artifact digest, configuration digest, environment, originating attempt, external resource identities, and result. The script applies manifests annotated with deployment/action identity, then reports readiness. Duplicate/unknown calls observe those identities before applying again.

Deployment status uses `pending|running|succeeded|failed` plus `reason`, `expires_at`, and `deleted_at`. `reason=outcome_unknown` blocks automatic repeat. Cleanup is a narrowly authorized ordinary container step launched as an internal project cleanup run; it deletes recorded owned objects and sets `deleted_at`. It cannot delete arbitrary tenant resources.

The declared environment is pinned at admission, not the first deployment callback. Delivery publishes immutable configuration; execution persists authorized identity/exact credential versions into runtime context. Later edits cannot rewrite admitted inputs; current revocation still governs new use. The [pinning contract](../implementation/environment-pinning.md) assigns execution to T-076, records/provider to T-035, unit-tested binding to T-077, wiring to T-072 and actual SQL/cleanup qualification to T-067/T-038 before M2 acceptance. Cleanup uses the recorded deployment's retained configuration.

## Provenance in shared scratch (`run_shared`)

Normative design; not delivered. In a shared run, files written by an earlier step can feed a later build, so evidence must say what it was built from. At command begin the entrypoint reports the workspace head and whether tracked files, untracked files and ignored files differ from it; the server records `workspaceState` (`clean`, `modified`, `none` for source-free) and `ignoredPresent` with the attempt and attaches them, with the input/head commits, prior-attempt lineage, each step's image digest, service image digests, the pinned policy digest and the builder mode, to every artifact and OCI registration the attempt produces ([begin fence](../contracts/runtime-v2.md#command-begin-fence)). A client cannot assert this metadata.

- Evidence is bound to a candidate only when its `workspaceState` is `clean` at the candidate commit. `modified` evidence remains ordinary, visibly labeled diagnostic evidence and does not satisfy candidate-bound requirements (DQ-14, protected successors); a changed tree cannot masquerade as an unchanged approved commit.
- A writer step commits generated changes, which creates a new candidate that requires fresh applicable evidence and invalidates an old approval through the existing applicability rules (DQ-04, DQ-05). Gate subjects keep their existing shape; the gate verifies the evidence's recorded provenance at request and at progression (DQ-01).
- `ignoredPresent` (for example a restored dependency cache) is disclosed with the evidence and does not block by default; an installation or project may later require it false.
- Promotion reuses a registered image digest independently of scratch. Dirty/generated build inputs are accurately recorded, never hidden.

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
| DQ-A10 | Missing, wrong-candidate, stale-baseline, or materially unresolved conformance evidence blocks approval/progression; a reviewed contract update and fresh report can satisfy the existing gate subject | DQ-01, DQ-02, DQ-04, DQ-14 |
| DQ-A11 | Environment/credential changes after admission do not alter the persisted context; real callbacks/cleanup remain scoped to the recorded configuration, and partial admission/pin failures roll back together | DQ-01, DQ-04, DQ-08, DQ-15 |
| DQ-A12 | `[run_shared]` A build from modified or generated scratch is labeled and cannot satisfy candidate-bound evidence; clean evidence, source/tool/service/image provenance and gate invalidation after a writer's new commit remain correct | DQ-01, DQ-04, DQ-05, DQ-07, DQ-14, DQ-16 |

DQ-A08 is retired with the override engine.
