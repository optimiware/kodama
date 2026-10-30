# Runtime contract v3 (`run_shared` with a source set)

**Status:** designed by T-131 for [ADR 0011](../adr/0011-internal-tenant-git-service.md) (PX-23–PX-25, GR-06); executable schema, DTOs, validators and fixtures in [`contracts/runtime/v3/`](../../contracts/runtime/v3/) (the API description `api/runtime.yaml` and the public client are integrated by T-158, not here); **not enabled by default and not served**. A fixture proves a shape, never behavior: no handler, grant issuance, credential delivery, preparation or push exists because these messages do. The v1 ([`/runtime/v1`](../../contracts/runtime/README.md)) and v2 ([runtime v2](runtime-v2.md)) contracts are unchanged; their schemas, fixtures, canonical vectors, operation tables and result hashes are byte-identical (a test pins them).

v3 is the [`run_shared`](../adr/0010-run-workspace-services-builders.md) model of runtime v2 with a run that pins a **source set** of internally hosted repositories ([Git service contract](git-service.md#source-set)). Its pipeline format is [`kodama/v1alpha3`](pipeline-format-v1alpha3.md). Behavior of the entrypoint and preparation is specified with their implementation tasks (T-151); this page owns the shapes.

## Version selection and compatibility

| Property | v2 | v3 |
| --- | --- | --- |
| Pipeline format | `kodama/v1alpha2` | `kodama/v1alpha3` |
| Context | `ContextV2`, `contextVersion: 2` | `ContextV3`, `contextVersion: 3`; `runModel` stays `run_shared` |
| Source | one repository, `workspace.source` | a source set, `workspace.sources` (one entry at `.` is the single-repository case) |
| Origin | implicit (work item or none) | explicit `origin` |
| Attempt Git credential | none (only the preparation workload reads source) | `git`: where the attempt's Git credential file and CA are, and what its grant allows |
| Source publication | `POST .../source` with a bundle | the entrypoint pushes with its grant; the result reports `commits` |
| Result | v1 `Result` (`sourceCommit`) | `ResultV3` (`commits`) |
| Routes | `/runtime/v2/...` | `/runtime/v3/attempts/{attemptId}/...` and `/runtime/v3/runs/{runId}/resources/{resourceKey}/...` |

A run pins its context version at admission and is served only on the matching wire; a v3 attempt or resource reaching another version's route, or the reverse, is `409 context_version_mismatch` with no side effects. A controller or entrypoint that does not implement v3 must refuse the run. **Reused unchanged** (aliases of the v1/v2 types, same validators): admission and attempt tokens (audience `kodama-runtime`), `TokenRequest/Response`, secrets, artifacts, `ResultResponse`, `ErrorBody` and the v2 error codes, `Endpoint`, `RunWindow`, `PolicyPin`, the strict-request/tolerant-response and explicit-null rules, integer limits, ID/timestamp patterns, and the v2 resource messages for **services**. Contexts and responses tolerate compatible additions; requests are strict. The v3 error envelope accepts the v2 codes and the Git contract's repository codes the Git grant routes return.

## Routes

All routes validate audience, tenant/project, attempt or resource, run model and current authority exactly as in v2; the tables are `runtimev3.Operations`.

| Route | Purpose |
| --- | --- |
| `POST /runtime/v3/attempts/{attemptId}/admit`, `.../token`, `GET .../context` | As v2 with `AdmitResponseV3 {token, context}` and `ContextV3` |
| `POST .../begin` | The command-begin fence over every source (`BeginRequestV3`/`BeginResponseV3`) |
| `POST .../secrets/resolve`, `POST .../artifacts`, `POST .../artifacts/{id}/complete` | As v2 |
| `POST .../git/seed` | **New.** The provisioning attempt obtains the single-use [seed grant](#runtime-issued-git-grants) of a repository it created: `GitSeedRequest` → `GitGrantResponse` |
| `POST .../git/integration` | **New.** An approved integration step obtains the [integration grant](#runtime-issued-git-grants) of its gate: `GitIntegrationRequest` → `GitGrantResponse` |
| `POST .../result` | `ResultRequestV3` |
| `POST /runtime/v3/runs/{runId}/resources/{resourceKey}/admit`, `.../token`, `GET .../context`, `POST .../secrets/resolve` (services), `POST .../preparation/complete` | The v2 resource routes with `ResourceContextV3` and `PreparationReceiptRequestV3`; audience `kodama-runtime-resource` |

There is **no source-proposal route**: a source on an internally hosted repository is pushed by the entrypoint with a Git grant (the attempt's own, or a [runtime-issued](#runtime-issued-git-grants) seed or integration grant) and reported in the result.

## Origin

Every context carries `origin`, the run's immutable record of how it started. All five members are present and null unless the kind needs them:

| `kind` | `workItemId` | `pushId`, `repositoryId`, `ref` | Context |
| --- | --- | --- | --- |
| `work_item` | set | null | `workItem` is the pinned snapshot of that item (the only kind with one) |
| `push` | null | set: the applied push record, its repository and the update's ref (never under `refs/kodama/`) | `workItem` null; the source set pins a source of that repository at the update's new commit ([PX-24](../specs/pipeline-execution.md)) |
| `manual` | null | null | `workItem` null |
| `provision` | null | null | `workItem` null; may be source-free; holds no static grant but the [seed capability](#runtime-issued-git-grants), the authority to seed the repositories it creates ([PX-25](../specs/pipeline-execution.md)) |

`repositoryId` is added to the push origin because a push record is read by repository. The agent-branch namespace of the run follows the origin: `refs/heads/kodama/agents/<workItemId>/<runId>/*` for `work_item`, `refs/heads/kodama/runs/<runId>/*` for every other kind.

**Internal deployment-cleanup runs.** `purpose` (`work` or `cleanup`) is a run attribute distinct from origin, and no runtime context carries it: a cleanup run is recognized by its shape, not a flag. A cleanup run is system-started from a recorded deployment's cleanup authorization and is none of a work item, a push or a provisioning run, so its origin is **`manual`**: `origin.workItemId`, `pushId`, `repositoryId` and `ref` null and `workItem` null (it may run after the originating work item is done or cancelled and does not consume WIP). It is source-free (`workspace.access: none`, `sources: []`, `sourceSetDigest: null`, `preparation: null`) and holds **no Git credential** (`git: null`), which also tells it apart from a `provision` run, whose `git` is non-null (it advertises the seed capability). Its definition declares no `sources` (`sources_source_free` at admission otherwise) and may be any version the installation executes for cleanup. Choosing `manual` rather than a new kind keeps the origin enumeration closed (a new kind would be a new context version); the deployment binding that authorizes the cleanup is recorded by the run snapshot and delivery, not by the runtime context.

## Context

`ContextV3` keeps every [`ContextV2`](runtime-v2.md#context) field with its meaning except `workspace`, and adds `origin` and `git`. Every field is present; nullable fields are explicit `null`.

| Field | Definition |
| --- | --- |
| `contextVersion`, `runModel` | `3`, `run_shared` |
| `origin` | As [above](#origin) |
| `workItem` | Non-null exactly for a `work_item` origin, with the same `id` |
| `workspace.access` | `none`, `read` or `write`: the attempt's effective access. `none` exactly when the run is source-free; `write` only for the attempt holding the writer reservation, and then at least one write source exists |
| `workspace.sources` | Empty exactly for a source-free run. Otherwise the pinned source set in path order: each entry is the [Git contract's source](git-service.md#source-set) (`repositoryId`, `commit` pinned at admission, `path`, run-level `access`, `branch` non-null exactly for a write source) plus `repositoryUrl` (`https`, never credentials), `headCommit` and `expectedTip` |
| `workspace.sourceSetDigest` | Null exactly for a source-free run; otherwise the set's canonical digest (`gitv1.SourceSet.Digest`), which identifies a multi-repository run in evidence and gate subjects and does not change as the run's head moves |
| `workspace.preparation` | Null exactly for a source-free run; otherwise `{sources: [{path, commit, treeId}], receiptDigest}`, one receipt per source in the same order at its pinned `commit`, from the recorded [preparation receipt](#resource-subjects) |
| `workspace.lineage` | As v2, except each entry ends with `published`: the [commit outputs](#result) the prior attempt reported (empty unless it updated refs), instead of `publishedCommit` |
| `git` | [Git credential delivery](#git-credential-delivery); non-null exactly for a source-backed or provisioning run |

`headCommit` of a source is the latest commit published to **that source's own write branch** in this run (the pinned `commit` before any); a read source's head never moves, and an agent-branch push never moves a head. A lineage entry's `published` may also list a prior attempt's runtime-issued outputs (a seeded default branch in a provisioning run, an integration push for a repository with a write source) but never a ref under `refs/heads/kodama/` outside the run's own namespace. `expectedTip` is non-null only for a write source of the writer: the tip the push must observe. A write source's `branch` is the work item's task branch (`refs/heads/kodama/work/<workItemId>`, `work_item` origin only) or a branch inside the run's own namespace; never the default branch, a tag, another run's namespace or a server ref. Source paths and URLs: every URL lies beneath `git.baseUrl`.

```json
{
  "contextVersion": 3,
  "runModel": "run_shared",
  "tenantId": "t_acme",
  "projectId": "p_inventory",
  "runId": "run_01",
  "stepId": "generate",
  "attemptId": "att_01",
  "actionKey": "run_01.generate",
  "definition": {"id": "pd_03", "digest": "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"},
  "origin": {"kind": "work_item", "workItemId": "wi_1042", "pushId": null, "repositoryId": null, "ref": null},
  "workItem": {"id": "wi_1042", "contentRevision": 2, "spec": "# Plan\nAdd a scoped review flow.", "acceptanceCriteria": [{"id": "AC-1", "text": "A reviewer resolves an exception with an audit record."}]},
  "designBaseline": null,
  "profile": null,
  "command": {"image": "registry.example.invalid/test-tools@sha256:4444444444444444444444444444444444444444444444444444444444444444", "command": ["/bin/sh", "-ec"], "args": ["cd /workspace/backend && go test ./..."], "env": {"CI": "true"}},
  "workspace": {
    "access": "write",
    "mountPath": "/workspace",
    "sources": [
      {"repositoryId": "repo_01", "commit": "1111111111111111111111111111111111111111", "path": "backend", "access": "write", "branch": "refs/heads/kodama/work/wi_1042",
       "repositoryUrl": "https://git.example.invalid/git/t_acme/p_inventory/backend.git", "headCommit": "1111111111111111111111111111111111111111", "expectedTip": "1111111111111111111111111111111111111111"},
      {"repositoryId": "repo_02", "commit": "2222222222222222222222222222222222222222", "path": "frontend", "access": "read", "branch": null,
       "repositoryUrl": "https://git.example.invalid/git/t_acme/p_inventory/frontend.git", "headCommit": "2222222222222222222222222222222222222222", "expectedTip": null}
    ],
    "sourceSetDigest": "sha256:<digest of the two-entry source set>",
    "preparation": {
      "sources": [
        {"path": "backend", "commit": "1111111111111111111111111111111111111111", "treeId": "4444444444444444444444444444444444444444"},
        {"path": "frontend", "commit": "2222222222222222222222222222222222222222", "treeId": "4444444444444444444444444444444444444444"}
      ],
      "receiptDigest": "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
    },
    "lineage": []
  },
  "git": {
    "grantId": "gg_01",
    "baseUrl": "https://git.example.invalid/git",
    "credentialFile": "/kodama/git/credential",
    "caFile": null,
    "repositories": [
      {"repositoryId": "repo_01", "update": ["refs/heads/kodama/agents/wi_1042/run_01/*", "refs/heads/kodama/work/wi_1042"]},
      {"repositoryId": "repo_02", "update": ["refs/heads/kodama/agents/wi_1042/run_01/*"]}
    ],
    "runtimeGrants": {"seed": null, "integration": {"credentialFile": "/kodama/git/integration-credential"}}
  },
  "endpoints": {}, "egress": [],
  "run": {"startedAt": "2026-10-30T10:00:00Z", "deadlineAt": "2026-10-30T11:00:00Z"},
  "policy": {"id": "run-v1", "digest": "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"},
  "inputs": {},
  "outputs": [{"name": "report", "type": "artifact", "path": "/kodama/outputs/tests.json"}],
  "secrets": [], "environment": null,
  "limits": {"timeoutSeconds": 1200, "maxOutputBytes": 104857600, "maxOutputFiles": 100, "maxDiagnosticBytes": 10485760}
}
```

The `sourceSetDigest` placeholder stands for the digest of that exact set (`gitv1.SourceSet.Digest`); the executable fixtures compute it. Run-level vocabulary: `access` of a source in the set is the run's ceiling (`write` for a source the run may write), `workspace.access` is this attempt's.

## Git credential delivery

`git` tells the entrypoint **where to find** what it needs to talk to the Git service, never the secret value. The context is not secret; the credential is a file.

| Member | Definition |
| --- | --- |
| `grantId` | Reference to the attempt's own grant (for correlation and revocation); not a credential. **Null** exactly for a provisioning run without sources |
| `baseUrl` | `https` URL of the tenant's Git endpoint without credentials, query, fragment or trailing slash; every source's `repositoryUrl` lies beneath `baseUrl + "/"`, so a credential helper scoped to it never offers the credential to another host |
| `credentialFile` | File directly under `/kodama/git/` holding the attempt's opaque credential. Written by the runtime adapter before the entrypoint starts, readable only by the workload, revoked with the attempt (a grant belongs to one attempt and dies with it); the username is the conventional `kodama`. Null exactly when `grantId` is |
| `caFile` | File directly under `/kodama/git/` with the CA bundle to trust, or `null` for the system roots |
| `repositories` | The attempt grant's repository claims, one entry per repository of the source set in order of first appearance: `{repositoryId, update}` with `update` the ref patterns the grant allows. `update` is **exactly** the run's agent-branch namespace pattern (first) and, **only when the attempt holds the writer reservation**, the exact sorted branches of the repository's write sources. Nothing else: never `refs/heads/*`, the default branch, a tag, another run's namespace or a server ref. Empty exactly when there is no attempt grant |
| `runtimeGrants` | `{seed, integration}`, each `null` or `{credentialFile}`: the [grants the attempt may request during the attempt](#runtime-issued-git-grants) and the file the entrypoint writes each credential to. `seed` is non-null exactly for a provisioning run; `integration` exactly when the source set has a write source. All files named by a delivery are distinct |

The validator requires `repositories` to equal what `GitAccess` derives from the sources and the attempt's access, so a reader's context can never list the task branch ([git grants](git-service.md#git-credentials-and-grants): "the task branch appears only in the grant of the attempt holding the writer reservation") and `GrantRepositories` converts the delivery into the grant's claims (`read: true`, those patterns, no seed, no integration). Admission issues and revokes the grant; scope, ref policy and current authority are decided by the push authorizer at every push, so this descriptor is a statement of intent and a convenience, not authority. A capability in `runtimeGrants` is likewise only advertised: the grant does not exist until the control plane answers the route.

### Runtime-issued Git grants

Two kinds of Git authority cannot exist at admission, so they are requested during the attempt and delivered as **claims plus a credential**: `GitGrantResponse {grant, credential, credentialFile}` where `grant` is the recorded [`GrantClaims`](git-service.md#git-credentials-and-grants) (not secret; validated by the Git contract's own rules), `credential` the opaque secret (shown once, never logged, like `secrets/resolve`) and `credentialFile` the file the context advertised, which the entrypoint writes with the workload's permissions only. The grant is revoked with the attempt. The Git credential helper selects the file by the repository being pushed.

**Provisioning: seed.** A provisioning run starts before its repository exists, and no grant can name a repository that does not exist; so a source-free provisioning context carries **no static grant** (`grantId`, `credentialFile` null, `repositories` empty) and only the seed capability. The flow ([PX-25](../specs/pipeline-execution.md), [bootstrap](../specs/git-repositories.md)):

1. The attempt creates the repository through the product API or MCP during the attempt (an ordinary, authorized create in the run's admitted project); the control plane records that this run created it.
2. `POST /runtime/v3/attempts/{attemptId}/git/seed {repositoryId}` (idempotent until the seed is consumed: a repeat returns a fresh credential for the same grant). The control plane answers only for a repository this run created in its project (anything else is `404 not_found`, indistinguishable from an unknown one) and applies the Git contract's seed eligibility: internal, ready, empty and no unsettled push, otherwise `422 hosting_mismatch`, `409 repository_archived`, `repository_not_ready`, `repository_not_empty` or `seed_pending`. A work-item or other non-provisioning run has no seed capability and is `403`.
3. The response grant is exactly a seed claim: kind `seed`, one repository with `seed: true`, `update` exactly `["refs/heads/<defaultBranch>"]` (the repository's current default branch), issued to this attempt's workload in this run and project, lifetime at most 15 minutes. `CheckSeedGrant(ctx, req, resp, created, repo, unsettled)` encodes the order of these checks.
4. The entrypoint pushes the default branch as the receive's only update (any other update is `seed_only`) and reports the commit in the result as `{repositoryId, ref: "refs/heads/<defaultBranch>", commit, pushId}`. `CheckResult` accepts it against the seed claim the control plane issued to this attempt (any commit: the seed is the repository's content). A later attempt of the run sees it in its `lineage`.

**Integration.** An integration step pushes an approved candidate `C` to the default branch from the approved base `B` ([integration](../specs/git-repositories.md#integration), [DQ-17](../specs/delivery-and-quality-gates.md)). It is an ordinary step that need not hold the writer reservation; its context advertises `integration` when the set has a write source, and the step requests the grant after its gate approved:

1. `POST /runtime/v3/attempts/{attemptId}/git/integration {repositoryId, gateId}`. The control plane derives `IntegrationApproval {gateId, repositoryId, ref, base, candidate, currentTip}` from the gate and the repository, never from the request. No approved candidate for that gate and repository (an unapproved gate, another gate, a repository without a write source) is `403 forbidden`: **no grant exists**. If the default branch's tip is no longer the approved base, the approval no longer applies and the answer is `409 publication_conflict` (the pipeline recomputes the candidate). The answer for a default branch that is already at the approved candidate is the same status and code with `reason: already_integrated` and `observedTip` equal to the candidate: no grant is issued and no push is needed. The entrypoint reports the step completed with no commit output for that repository (this attempt has no push record to cite); any other `observedTip`, or `reason: base_moved`, means the candidate is recomputed. (Coordinator decision at T-150; the grant routes ride the `context.read` action and an integration grant is bound to the run's approved gate, not to a step.)
2. The response grant is exactly an integration claim: kind `integration`, one repository, `update` exactly the integration ref, constraint `{ref, from: B, to: C, gateId}`, issued to this attempt. `CheckIntegrationGrant` checks every member against the approval; the Git contract permits that one update only (`PermitsUpdate` is exact), so code on the task branch cannot widen it.
3. The entrypoint pushes `C` to the default branch and reports `{repositoryId, ref: <integration ref>, commit: C, pushId}`. `CheckResult` accepts an output not covered by the attempt's own grant only when an integration claim issued to this attempt has the same repository and ref **and** `to` equal to the reported commit: a commit that is not the approved candidate, another ref, another repository or another attempt's claim is `422 output_invalid`. A claim covers one output, and the context must have advertised the capability.

Ordinary attempt grants stay restricted: neither the default branch nor any ref outside the agent namespace and the writer's branches is ever in `repositories`, and an output on one is accepted only through a runtime-issued claim as above. That the push record exists, was applied and was made under that claim is verified by the consuming module against the push record.

## Begin

`BeginRequestV3 {sources: [{path, head, trackedClean, untrackedPresent, ignoredPresent}]}`, strict, observes **every** source in context order (empty for a source-free attempt). The server derives `workspaceState` (`none`, `clean` exactly when every source is at its `headCommit`, tracked-clean and without untracked files, otherwise `modified`) and `ignoredPresent` (any source), returned as `BeginResponseV3 {attemptId, beganAt, workspaceState, ignoredPresent}`; the rules of [the v2 fence](runtime-v2.md#command-begin-fence) (idempotency, revocation, lost acknowledgment, conservative unknowns) apply unchanged. An observation of other paths, a different count or another order is `invalid_request`.

## Result

`ResultRequestV3 {resultHash, result}` where `ResultV3` is the v1 `Result` with `commits` in place of `sourceCommit`: `commits: [{repositoryId, ref, commit, pushId}]`, each a branch the attempt updated, the commit the ref pointed to afterwards (non-zero) and the applied push record that made it. It is strictly ordered by `(repositoryId, ref)` with one entry per ref, so one result has one hash; only a `completed` result reports any (`failed` and `needs_input` require `[]`, as they required a null `sourceCommit`). A writer that changed nothing reports nothing for it; the lineage records what was reported.

`ResultHash` is `sha256:` plus the hex SHA-256 of the [v1 canonical form](../../contracts/runtime/README.md#canonical-result-hash) of the `ResultV3` object, unchanged algorithm. A v3 result never has the same canonical bytes as a v1 result (`commits` versus `sourceCommit`), so no historical hash can be confused. Independent vectors (Python `json`/`hashlib`) are in `contracts/runtime/v3/fixtures/canonical.json`.

`CheckResult(result, ctx, artifacts, issued...)` applies the v1 checks for outputs and artifacts and requires every commit output to be covered by the attempt's own grant (a repository of the grant and a ref one of its `update` patterns selects) or by one of the runtime-issued claims the control plane recorded for this attempt (`issued`, loaded from its own records, never from the caller; see [runtime-issued grants](#runtime-issued-git-grants)): a reader reporting the task branch, an ordinary attempt reporting the default branch, another run's namespace or an unknown repository is `422 output_invalid`. That the push record exists, was applied, moved the ref to that commit and was authorized by this attempt's grant is verified by the consuming module against the push record, never by this contract; a commit output is a reference to evidence, not evidence.

## Resource subjects

The preparation and the services of a v3 run are served on `/runtime/v3/runs/{runId}/resources/{resourceKey}` with the v2 identity, token and audience rules (`kodama-runtime-resource`; a resource token never works on an attempt route and the reverse). `ResourceContextV3` has `contextVersion: 3`, `resourceKind` `preparation` or `service`, the key `preparation` or `service.NAME`, exactly one of `preparation` and `service`, and the policy pin.

**Preparation: one workload for all sources.** `PreparationContextV3 {sources, mountPath, timeoutSeconds, maxBytes, maxAttempts, deadlineAt, git}`:

- `sources` lists every checkout `{repositoryId, repositoryUrl, commit, path}` in path order, a valid source set (access and branch are irrelevant to a read-only checkout); it equals the attempts' sources (`CheckPreparationMatches`), derived by `WorkspaceV3.SourcePreparation`.
- `git` is the attempt's delivery **without update rights**: `{grantId, baseUrl, credentialFile, caFile, repositories}` with `repositories` the plain repository IDs once each in order of first appearance. Its grant is kind `preparation` (read-only, no update pattern anywhere), delivered as a file like the attempt's, revoked when the preparation ends; the checkout hardening of the receipt is unchanged.
- `timeoutSeconds` bounds every try and every source together; `maxAttempts` is the try count inside this one workload.

`POST .../preparation/complete` takes `PreparationReceiptRequestV3 {sources: [{path, commit, treeId}], hooksDisabled, submodules, credentialsRemoved, attempts}`: one receipt per source in order at its pinned commit, the three safety fields with their literal values (`true`, `skipped`, `true`) and `attempts` in `1..maxAttempts` (`CheckReceipt`). The response is the unchanged v2 `PreparationReceiptResponse {receiptDigest, recordedAt}`; the digest is opaque here and is what attempt contexts carry in `workspace.preparation.receiptDigest`. A different recorded receipt is `409 receipt_conflict`.

**Services are unchanged.** `ResourceContextV3` with `resourceKind: service` carries the v2 `ServiceContext` exactly, and the admit, token and secrets messages are the v2 ones. A service never mounts `/workspace`, holds no Git credential and does not depend on the source set, so nothing in its message needs to change; only the context version differs, so a v3 run's service is never served as a v2 one.

## Failure and recovery

No new recovery rule: admission replay, token renewal, revocation, cancellation, lost acknowledgments and unknown outcomes are those of v1/v2. A lost result acknowledgment replays the same hash; a different result is `409 result_conflict`. Pushes already applied by a failed or cancelled attempt settle independently in the Git service, and a rerun is a new run that pins a new source set.

## Machine-readable form

[`contracts/runtime/v3/schema.json`](../../contracts/runtime/v3/schema.json) (referencing the unchanged v1 and v2 schemas and the Git contract schema), the Go contract with `Operations`, `DeriveBegin`, `CheckBeginResponse`, `CheckResult`, `CheckSeedGrant`, `CheckIntegrationGrant`, `CheckReceipt`, `CheckPreparationMatches`, `ResultHash`, and generated fixtures in `contracts/runtime/v3/fixtures/` (see the [package README](../../contracts/runtime/README.md#runtime-contract-v3-run_shared-with-a-source-set-shapes-only-not-enabled)). The public API description and client are integrated by T-158.
