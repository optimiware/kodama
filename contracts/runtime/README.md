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
