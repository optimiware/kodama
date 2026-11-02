# Runtime contract v1

The frozen wire contract between the in-Pod `kodama step` entrypoint and the control plane's `/runtime/v1/attempts/{attemptId}` interface, delivered by T-003. The behavior it serves is specified in the [step entrypoint contract](../../docs/implementation/step-entrypoint.md); this directory owns the exact shapes.

| Path | Contents |
| --- | --- |
| [`v1/schema.json`](v1/schema.json) | JSON Schema 2020-12: one definition per message in `$defs` |
| [`../../api/runtime.yaml`](../../api/runtime.yaml) | OpenAPI 3.1 for the eight routes, generated from the Go operation table |
| `v1/*.go` | `runtimev1` package: DTOs, strict decoding, validation, `CheckResult`, error codes/statuses, `Operations`, `Canonicalize`, `ResultHash`, `DesignBaseline`, `ConformanceReport`, `CheckConformance` |
| [`v1/fixtures/`](v1/fixtures/) | `cases.json` (payloads), `checks.json` (result vs. context), `scenarios.json` (replayable exchanges), `canonical.json` (hash vectors), `design.json` (baseline/report cases, conformance checks, digest vectors) |
| [`../../internal/domain/`](../../internal/domain/) | Shared IDs, `Scope`, versions, error codes |

## Rules consumers rely on

- **Strict requests, tolerant responses.** `DecodeRequest` rejects unknown, misspelled, differently cased, duplicate and omitted fields; `DecodeResponse` tolerates additional fields but still requires the contracted ones. Unknown enum values are not success.
- **Explicit nulls.** Every DTO writes every field. A nullable field is present as `null`; omitting it is an error (and a different hash).
- **Integers only.** No floating point; integers lie within ±(2^53−1) and never pass through `float64`.
- **IDs are opaque** (`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`). Ownership comes from scope comparison, never from an ID's spelling or prefix. A missing or foreign artifact is the same `404 not_found`.
- **Token and context are separate.** `AdmitResponse` carries a token (audience `kodama-runtime`, expiring, bounded by `maxExpiresAt`) and the immutable `Context`. Upload URLs are transfer instructions, not identities.

## Decisions frozen here

- **Admission replay.** The one-time credential is exchanged with a client `nonce`. The same credential and nonce after a lost response returns a fresh token under the same `maxExpiresAt`; a different nonce after consumption is `409 admission_consumed`. A revoked or cancelled attempt is `403 attempt_revoked`; a Job/Pod identity that does not match is `403 forbidden`.
- **Artifacts.** `POST /artifacts` returns `201` for a new reservation and `200` for an identical repeat; different metadata for the same name is `409 idempotency_conflict`. File artifacts start `reserved` with an `upload`; OCI registrations are `registered` with a null `upload`. Completion with the wrong digest is `422 digest_mismatch`; a missing object is `409 artifact_incomplete`.
- **Source.** `200 accepted` and `202 pending` carry a `SourceProposalResponse`; repeating a proposal observes the same publication. A lost expected-tip race is `409 publication_conflict` with `details.observedTip`. A completed writer reports `sourceCommit`; a nonwriter reports `null`.
- **Result.** The request carries the normalized `result` and its `resultHash`. The server recomputes the hash (`422 result_hash_mismatch`). Repeating an accepted result returns the original acknowledgment (`200`); a different result is `409 result_conflict`. `completed` requires exit code 0, null reason and question, and exactly the declared outputs; `failed` requires a reason code and no outputs or source; `needs_input` requires a non-blank question (at most 4096 characters) and no outputs or source. `exitCode` is `null` when no exit status was observed. Failed and `needs_input` attempts keep `diagnosticArtifactIds`. The server derives `attemptStatus`; cancellation and timeout can supersede `completed`.
- **Wrapper compatibility.** `AdmitRequest.clientVersion` lets the server answer `422 profile_unsupported` for an incompatible entrypoint/profile wrapper pairing.

## Canonical result hash

`ResultHash` is `sha256:` plus the lowercase hex SHA-256 of the canonical form of the normalized `result` object only (not `resultHash` or any transport field):

1. The input passes the strict parser: UTF-8, no duplicate keys, integers only (no fraction, exponent, `+`, leading zero or `-0`), range ±(2^53−1), no control characters in strings, no unpaired surrogate escapes, nesting depth at most 32, no trailing data.
2. Output has no whitespace outside strings; object keys are sorted by code point (equivalently, by UTF-8 bytes); array order is preserved.
3. Nulls, empty objects and empty arrays are written explicitly.
4. Strings escape only `"`, `\`, and `\b \f \n \r \t`; every other character below U+0020 is `\u00xx` in lowercase hex. All else, including `/`, `<`, `>`, `&`, U+007F, U+2028 and non-ASCII text, is literal UTF-8. No Unicode normalization.

Any incompatible change needs a new contract version. The expected canonical bytes and hashes in `canonical.json` and `cases.json` were derived with Python `json` and `hashlib`, independently of the Go code.

## Design baseline and conformance report

Delivered for AR-14, DQ-14 and SD-08 by the [design conformance procedure](../../docs/implementation/design-conformance.md). There is no separate resource: the baseline is the nullable `designBaseline` field of the immutable `Context`, and the report is the payload of an ordinary declared artifact that T-031 produces and T-034 binds into the existing gate subject.

- **Baseline** (`DesignBaseline`): references only. `requirements` (stable IDs such as `SD-01`, `PX-A01`), `decisions` (`C-`, `P-`, `O-` IDs), `sources` (HTTPS repository without credentials, **Git commit** revision, relative path that cannot escape), and `requiredChecks` (from the pinned project specification or profile, never chosen by the agent). A branch, tag or `latest` is not a valid revision, so references cannot advance mid-run.
- **Digest.** `digest` is SHA-256 over the canonical JSON of exactly those four lists (using the same canonicalization as `ResultHash` and gate subjects), excluding the `digest` field and any later candidate or report. Every list must be strictly sorted without duplicates (sources by repository, path, revision), so one baseline has one digest. `NewDesignBaseline` sorts and computes it; `Validate` rejects a digest that does not match its references (`baseline_digest_mismatch`).
- **Report** (`ConformanceReport`): `baselineDigest`, `candidateCommit`, affected `requirements`, `checks` (`passed`, `failed` or `not_run`, each with `evidenceArtifactIds`), `unresolvedDeviations` (requirement and reason) and `disposition`. A `passed` check must cite evidence. `disposition` is derived, not chosen: `conforms` exactly when there are no unresolved deviations and every listed check passed, otherwise `blocked`. Checks, requirements and evidence IDs are strictly sorted. `ReportDigest` is SHA-256 over the canonical report.
- **`CheckConformance(report, baseline, candidate)`** returns, in order: `invalid_request` (structure), `conformance_candidate_mismatch`, `conformance_baseline_mismatch` (stale baseline), `conformance_incomplete` (a required check is absent), `conformance_blocked` (unresolved deviation or a check that did not pass). These are not runtime wire codes and have no HTTP status; T-034 maps them. Extra checks beyond the required ones are allowed.

A syntactically valid, conforming report does not prove architectural correctness; reviewers assess its claims and evidence. Ownership and provenance of the evidence artifacts are verified by the gate against the artifact store, not here.

## Using the fixtures

`cases.json` entries state the message `type`, the expected verdict of the schema (`schemaValid`, `null` when only the strict parser applies) and of the Go contract (`valid`, `code`). A payload the Go contract accepts must also pass the schema. `checks.json` runs `CheckResult` with trusted artifact metadata. `scenarios.json` lists ordered exchanges for the failure cases the entrypoint, auth and result tasks must reproduce: cancelled admission, wrong Job, wrong audience, refresh after revoke, incompatible wrapper, duplicate and conflicting results, late result, foreign or incomplete artifacts, publication conflict and response loss, and undeclared secrets.

These are shape and protocol fixtures. They do not verify database state, cancellation races, Git publication or storage; T-016, T-019 and T-022 replay them against their implementations.

## Checks and regeneration

```bash
go test -race -count=1 ./contracts/... ./internal/domain/...
go test ./contracts/runtime/v1 -run TestOpenAPI -update   # after changing Operations
```

`TestRuntimeContractFixtures` (PX-10, AR-06, AC-12) and `TestDesignContextFixtures` (AR-14, DQ-14, SD-08) are the T-003 acceptance checks. The schema's string patterns are asserted equal to the Go patterns, and `api/runtime.yaml` is asserted equal to the document generated from `Operations`. Changing a contract requires agreement from the dependent tasks (T-016, T-019, T-021, T-022).

## Runtime contract v2 (`run_shared`): shapes only, not enabled

[`v2/`](v2/) (package `runtimev2`, delivered by T-110) is the executable form of
[runtime v2](../../docs/contracts/runtime-v2.md). `v1/` is untouched: its schema, fixtures, canonical vectors, validators and
`api/runtime.yaml` are byte-identical, and `api/runtime.yaml` stays the frozen v1 operation table until T-116 integrates the v2
fragments and regenerates the aggregate and client together.

| Path | Contents |
| --- | --- |
| `v2/schema.json` | `ContextV2`, `AdmitResponseV2`, begin, resource-subject, `PreparationReceipt*` and `ErrorResponse` definitions. Everything shared with v1 is referenced from the unchanged v1 schema resource (`https://kodama.invalid/contracts/runtime/v1/schema.json`); consumers compile both |
| `v2/*.go` | `ContextV2` and its pins (workspace source/preparation/lineage, endpoints, egress, run window, policy), `BeginRequest/Response`, `ResourceIdentity`, `ResourceAdmitRequest/Response`, `ResourceTokenInfo/Request/Response` (audience `kodama-runtime-resource`), `ResourceContext`, `PreparationReceiptRequest/Response`; aliases of the unchanged v1 DTOs; `DecodeRequest`/`DecodeResponse` (the v1 decoders); the v2 error codes `context_version_mismatch`, `receipt_conflict`, `resource_unbound`, `resource_revoked` and `Status`; the `Operations` table; the pure `DeriveBegin`, `CheckBeginResponse`, `CheckReceipt`, `CheckAudience`, `CheckResourceCall`, `CheckResourceEnvelope` (a resource token's `maxExpiresAt` never exceeds the resource's `deadlineAt`, enforced by `ResourceAdmitResponse.Validate`) and `ResourceKeyOf` (returns an error for an unknown kind or invalid service name). Every v2 attempt route lists `forbidden` because a resource-audience token is refused on it; the frozen v1 tables are unchanged |
| `v2/fixtures/cases.json`, `scenarios.json` | Generated from typed bases and mutations (`KODAMA_UPDATE_GOLDEN=1 go test ./contracts/runtime/v2`); the schema judges every payload independently and a drift test pins the files |

What the fixtures are: protocol and shape examples. A scenario proves that its request and response bodies decode, that the status
and code belong to the operation, and that the recorded exchange is consistent with the pure derivations (begin state, replay,
receipt, audience, resource kind). It proves **nothing** about an HTTP handler, authority, credentials, the begin fence's
database linearization against cancellation, preparation lifecycle, or revocation: those belong to T-113 (entrypoint and
authority) and T-116 (assembly) and are tested there.

Rules specific to v2: the unchanged v1 messages (`AdmitRequest`, `TokenInfo` of audience `kodama-runtime`, secrets, artifacts,
source proposals, `Result`/`ResultRequest`/`ResultResponse`) are reused with their v1 validators, and `ResultHash` is the v1
canonicalization of the result object, so every historical hash holds (`Test_runtime_v2_result_hash_unchanged`). The resource
subject types never reuse the v1 token or Job identity types. `receiptDigest` is carried as an opaque `Digest`: the exact
canonical object it covers (request plus run, resource key and observed volume UID) is for T-112, which owns the receipt
store. Engineering bounds chosen here where the contract is silent: lineage at most 8192 entries, preparation `maxAttempts`
1–100, service secret bindings at most 80, element and value sizes 8192 bytes.

```sh
go test -race -count=1 ./contracts/runtime/...
KODAMA_UPDATE_GOLDEN=1 go test ./contracts/runtime/v2   # after changing a base, then review the diff
```

## Runtime contract v3 (`run_shared` with a source set): shapes only, not enabled

[`v3/`](v3/) (package `runtimev3`, delivered by T-131) is the executable form of the source-set additions of
[ADR 0011](../../docs/adr/0011-internal-tenant-git-service.md) (PX-23–PX-25, GR-06) for **attempts**.
`v1/` and `v2/` are untouched: their schemas, fixtures, canonical vectors, validators, operation tables and
`api/runtime.yaml` are byte-identical (`Test_unit_runtime_old_hashes_unchanged` pins the trees), and
`api/runtime.yaml` stays the frozen v1 table until T-158 integrates the v3 fragments. The prose contract is
[runtime v3](../../docs/contracts/runtime-v3.md).

| Path | Contents |
| --- | --- |
| `v3/schema.json` | `ContextV3`, `AdmitResponseV3`, `Origin`, `SourceV3`, `WorkspaceV3`, `PreparationV3`, `LineageEntryV3`, `CommitOutput`, `GitDelivery`, `BeginRequestV3`/`BeginResponseV3`, `ResultV3`, `ResultRequestV3`, `ResourceContextV3`, `PreparationContextV3`, `PreparationGit`, `ResourceAdmitResponseV3`, `PreparationReceiptRequestV3`. Everything shared is referenced from the unchanged v1 and v2 schema resources, and Git value syntaxes from the Git contract schema; consumers compile all four |
| `v3/*.go` | The DTOs, `DecodeRequest`/`DecodeResponse` (the v1 decoders), `Operations`, `Patterns`, the pure `DeriveBegin`, `CheckBeginResponse`, `CheckResult`, `CanonicalResult`, `ResultHash`, and the context helpers `AgentPattern`, `GitAccess`, `GitDelivery.GrantRepositories`, `WorkspaceV3.SourceSet` |
| `v3/fixtures/cases.json` | Generated from typed bases and path mutations (`KODAMA_UPDATE_GOLDEN=1 go test ./contracts/runtime/v3`); the schema judges every payload independently and a drift test pins the file |
| `v3/fixtures/canonical.json` | Result canonical forms and hashes derived independently with Python `json`/`hashlib` |

Rules specific to v3 (the context version is `3`, the run model is `run_shared`, the token audience is the unchanged
`kodama-runtime`):

- **Origin.** `{kind, workItemId, pushId, repositoryId, ref}`, every member present and null unless its kind needs it: `work_item`
  has `workItemId` and the pinned `workItem` snapshot (the only kind with one); `push` has `pushId`, `repositoryId` and the update's
  `ref` (never a server ref) and pins a source of that repository, with no work item; `manual` and `provision` have none. A
  `provision` run is the only one that may be source-free and still hold a Git credential, for the repositories it creates.
  `repositoryId` is an addition to the minimal origin: a push record is read by repository.
- **Source set.** `workspace.sources` is empty exactly for a source-free run, otherwise it projects (`WorkspaceV3.SourceSet`) to a valid
  `gitv1.SourceSet`: ordered by path, unique non-overlapping safe paths, `.` only alone, non-zero commits, a write source with a branch
  and a read source with null. `sourceSetDigest` is that set's digest (`gitv1.SourceSet.Digest`), so evidence and gate subjects identify
  a multi-repository run, and it does not change as the run's head moves. Each source adds the clone URL (`https`, no credentials),
  `headCommit` (the latest commit published to the source's own branch in this run, the pinned commit before any) and `expectedTip`, non-null
  only for a write source of the attempt holding the writer reservation (`workspace.access: write`, which then needs a write source).
  A write source's branch is the work item's task branch (work-item origin only) or a branch inside the run's own namespace; never the
  default branch or another run's. `preparation` is one receipt per source (same order and path, the pinned commit, the observed tree)
  and one digest. A single source at `.` is the single-repository case.
- **Git delivery.** `git` is non-null exactly when the attempt holds a Git grant or may request one (a source-backed or provisioning run) and is a
  **location, never a secret**: `grantId` (reference, for correlation and revocation; null for a provisioning run without sources, which has no static grant because no grant can name a repository that does not exist yet), `baseUrl` (`https`, no credentials, query or trailing
  slash; every source URL lies beneath it, so a helper scoped to it never offers the credential to another host), `credentialFile`
  and `caFile` (null for the system roots), each a file directly under `/kodama/git`, written by the runtime adapter before the entrypoint
  starts, readable only by the workload and revoked with the attempt. The file holds the opaque credential and the username is `kodama`.
  `repositories` mirrors the grant's repository claims exactly (`GitAccess`, `GrantRepositories`): per repository in order of first
  appearance, the run's **agent-branch namespace** (`refs/heads/kodama/agents/<workItemId>/<runId>/*`, or `refs/heads/kodama/runs/<runId>/*`
  without a work item) and, **only for the attempt holding the writer reservation**, the exact sorted branches of that repository's write
  sources (the task branch), and `runtimeGrants {seed, integration}` the grants requested during the attempt (see below). Validation requires equality, so a reader's context can never list the task branch and no context lists
  `refs/heads/*`, a default branch, another run's namespace or anything under `refs/kodama/`.
- **Runtime-issued grants.** `POST .../git/seed {repositoryId}` (provisioning only: the repository this run created in its project; the response is the Git
  contract's seed claim, one repository, `seed: true`, update exactly `refs/heads/<defaultBranch>`) and `POST .../git/integration {repositoryId, gateId}` (after the gate
  approved; the response is the integration claim `{ref, from: approved base, to: approved candidate, gateId}`; no approval is `403`, a moved base `409 publication_conflict`)
  return `GitGrantResponse {grant, credential, credentialFile}`: claims, the secret credential (never logged) and the file advertised in `runtimeGrants`. `CheckSeedGrant` and
  `CheckIntegrationGrant` apply the order of checks against what the control plane knows; `CheckResult(..., issued...)` accepts a commit output outside the attempt's own grant only
  when an issued seed claim covers its ref or an issued integration claim has the same repository and ref and `to` equal to the commit.
- **Begin** observes every source by path, in order (`BeginRequestV3.sources`, empty for a source-free attempt); `DeriveBegin` is `clean`
  exactly when every source is at its `headCommit`, tracked-clean and has no untracked file, otherwise `modified`; `ignoredPresent` is the
  disjunction and never makes the state `modified` alone.
- **Result.** `ResultV3` is the v1 `Result` with `commits` in place of `sourceCommit`: `[{repositoryId, ref, commit, pushId}]`, each a branch
  updated by the attempt, the commit the ref pointed to afterwards and the applied push record that made it, strictly ordered by repository
  and ref (one per ref) so a result has one hash; only a completed attempt reports any. `CheckResult` requires every output to name a
  repository of the grant and a ref one of its update patterns selects (a reader reporting the task branch is `output_invalid`). That the
  push record exists, was applied, moved the ref to the commit and belongs to this attempt's grant is verified by the consuming module
  against the record; this package never reaches Git. There is no source-proposal route: the entrypoint pushes with the grant.
  `ResultHash` is the v1 canonicalization of the v3 result object, so no historical hash changes and a v3 hash never equals a v1 hash.
- **Resource subjects** (`/runtime/v3/runs/{runId}/resources/{resourceKey}`, audience `kodama-runtime-resource`): `ResourceContextV3` has
  `contextVersion: 3`. The **preparation** (`resourceKey: preparation`) is **one workload for the whole source set**:
  `PreparationContextV3 {sources, mountPath, timeoutSeconds, maxBytes, maxAttempts, deadlineAt, git}` with `sources` listing every
  `{repositoryId, repositoryUrl, commit, path}` in path order (a valid source set; `WorkspaceV3.SourcePreparation` derives it from an attempt context,
  `CheckPreparationMatches` requires equality) and `git` the attempt's delivery **without update rights**: `{grantId, baseUrl, credentialFile, caFile,
  repositories}` with `repositories` the plain repository IDs once each; its grant is kind `preparation` (read-only, `GrantRepositories`), revoked when
  the preparation ends. `PreparationReceiptRequestV3 {sources: [{path, commit, treeId}], hooksDisabled, submodules, credentialsRemoved, attempts}`
  records one receipt per source, in order, at the pinned commits (`CheckReceipt`); the response is the unchanged v2 `PreparationReceiptResponse` and its
  `receiptDigest` is what the attempt context's `workspace.preparation` carries. **Services reuse the v2 messages unchanged** (`ServiceContext`,
  `ResourceAdmitRequest`, tokens, `SecretResolveRequest`): a service never mounts `/workspace`, holds no Git credential and does not depend on the
  source set; `ResourceContextV3` only changes their context version, so a v3 run's service reaching a v2 route (or the reverse) is `context_version_mismatch`.
- **Operations** (`Operations`, attempt routes relative to `/runtime/v3/attempts/{attemptId}`, resource routes to the path above): admit, token, context,
  begin, secrets, artifacts and result for attempts; admit, token, context, secrets (services) and `preparation/complete` for resources. Audience and
  kind rules are the v2 ones (`runtimev2.CheckAudience`, `CheckResourceCall`).

```sh
go test -race -count=1 ./contracts/runtime/...
KODAMA_UPDATE_GOLDEN=1 go test ./contracts/runtime/v3   # after changing a base, then review the diff
```
