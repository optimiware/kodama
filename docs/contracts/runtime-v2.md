# Runtime contract v2 (`run_shared`)

**Status:** normative contract frozen by T-109 for the [`run_shared` model](../adr/0010-run-workspace-services-builders.md); executable schemas, DTOs, fixtures and OpenAPI by T-110 under `contracts/runtime/v2/` and `api/runtime.yaml`; **not enabled**. The v1 contract ([`contracts/runtime`](../../contracts/runtime/README.md), `/runtime/v1/attempts/...`) is unchanged and remains the only wire an installation serves until T-116 enables `v2`. Behavior of the entrypoint is in the [step contract](../implementation/step-entrypoint.md#run_shared-v2-sequence).

## Version selection and compatibility

| Property | v1 (`attempt_private`) | v2 (`run_shared`) |
| --- | --- | --- |
| Pipeline format | `kodama/v1alpha1` | [`kodama/v1alpha2`](pipeline-format-v1alpha2.md) |
| Context | `Context` (v1 fixtures, frozen) | `ContextV2` with `contextVersion: 2` |
| Routes | `/runtime/v1/attempts/{attemptId}/...` | `/runtime/v2/attempts/{attemptId}/...` and `/runtime/v2/runs/{runId}/resources/{resourceKey}/...` |
| Token audiences | `kodama-runtime` | attempts `kodama-runtime`; resources `kodama-runtime-resource` |
| Result/source/artifact bodies | v1 DTOs | **The v1 DTOs, unchanged** (see below) |

A run pins `runModel` and `contextVersion` at admission and the server serves an attempt only on the matching wire. A v2 attempt reaching a v1 route, or the reverse, is `409 context_version_mismatch` with no side effects. An older controller or entrypoint that does not know `v2` must refuse the run, never adopt it with `attempt_private` semantics: the pinned runtime binding includes the `run_resources` requirement ([guarantees](run-resources.md#capabilities)), which older decoders reject as unknown, and the run-model discriminator is checked independently. `AdmitRequest.clientVersion` stays the compatibility handshake (`422 profile_unsupported`).

**Reused unchanged from v1** (same JSON, same validation, same fixtures, same canonical rules): `AdmitRequest`, `JobIdentity`, `TokenInfo`, `TokenRequest/Response`, `SecretResolveRequest/Response`, `ArtifactReserveRequest/Response`, `ArtifactComplete*`, `SourceProposalRequest/Response`, `Result`, `ResultRequest/Response`, `ErrorBody`, the strict-request/tolerant-response and explicit-null rules, integer limits and ID/timestamp patterns. **`ResultHash` is byte-for-byte the v1 canonicalization of the `result` object**, so every historical hash and golden vector stays valid and a result's hash does not depend on the context version. `DesignBaseline`/`ConformanceReport` and their digests are unchanged. v2 adds `ContextV2`, `AdmitResponseV2`, the `begin` exchange and the resource-subject messages; a Go implementation may alias v1 types but must not change them.

## Routes

All attempt routes validate audience, tenant/project, attempt, run model and current authority exactly as in v1.

| Route | Purpose |
| --- | --- |
| `POST /runtime/v2/attempts/{attemptId}/admit` | Exchange the one-time credential; returns `AdmitResponseV2 {token, context}` |
| `POST .../token` | Renew a currently authorized attempt |
| `GET .../context` | The immutable `ContextV2` |
| `POST .../begin` | **Command-begin fence** (new): durable server record taken immediately before the command starts |
| `POST .../secrets/resolve`, `POST .../artifacts`, `POST .../artifacts/{id}/complete`, `POST .../source`, `POST .../result` | As v1 |
| `POST /runtime/v2/runs/{runId}/resources/{resourceKey}/admit` | Resource-subject admission (preparation and services only) |
| `POST .../token`, `GET .../context` | Renew; `ResourceContext` |
| `POST .../secrets/resolve` | Services only; the resource's pinned bindings |
| `POST .../preparation/complete` | Preparation only; records the trusted checkout receipt |

`resourceKey` is `preparation` or `service.NAME` (`NAME` matching the service-name pattern). `workspace` and `network` resource keys have no workload identity and no routes.

## Context

`ContextV2` keeps every v1 field with its meaning (`tenantId`, `projectId`, `runId`, `stepId`, `attemptId`, `actionKey`, `definition`, `workItem`, `designBaseline`, `profile`, `command`, `inputs`, `outputs`, `secrets`, `environment`, `limits`) and defines the following. Every field is present; nullable fields are explicit `null`.

| Field | Definition |
| --- | --- |
| `contextVersion` | The integer `2` |
| `runModel` | The string `run_shared` |
| `command` | As v1 (`image`, `command`, `args`, `env`) with all expressions already resolved to strings, endpoint references included |
| `workspace.access` | `none`, `read` or `write`: the **effective** access recorded at admission, never `null` |
| `workspace.mountPath` | `/workspace` |
| `workspace.source` | `null` exactly for a source-free run. Otherwise `{repositoryUrl, inputCommit, headCommit, branch, expectedTip}`: `inputCommit` is the run's immutable pinned commit; `headCommit` is the latest commit published *in this run* by a successful writer (equal to `inputCommit` before any); `branch` and `expectedTip` are non-null only for a writer (expected tip observed at dispatch) |
| `workspace.preparation` | `null` exactly for a source-free run. Otherwise `{commit, treeId, receiptDigest}` of the recorded trusted receipt |
| `workspace.lineage` | Ordered `[{stepId, attemptId, access, publishedCommit}]` of every prior attempt in this run that **began** a command (a recorded `begin`); `publishedCommit` is non-null for an accepted writer. Empty means the scratch is exactly the prepared tree |
| `endpoints` | Map service name → `{host, ports: {PORT_NAME: integer}}`; `{}` without services |
| `egress` | The step's resolved egress profile names (pinned policy identifies their destinations) |
| `run` | `{startedAt, deadlineAt}` pinned at the start of resource holding; the deadline covers gate waits |
| `policy` | `{id, digest}` of the pinned [execution policy](run-resources.md#execution-policy) |
| `limits` | v1 `Limits` where `timeoutSeconds` is the step's execution deadline |

```json
{
  "contextVersion": 2,
  "runModel": "run_shared",
  "tenantId": "t_acme",
  "projectId": "p_inventory",
  "runId": "run_01",
  "stepId": "integration",
  "attemptId": "att_02",
  "actionKey": "run_01.integration",
  "definition": {"id": "pd_02", "digest": "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"},
  "workItem": null,
  "designBaseline": null,
  "profile": null,
  "command": {
    "image": "registry.example.invalid/test-tools@sha256:4444444444444444444444444444444444444444444444444444444444444444",
    "command": ["/bin/sh", "-ec"],
    "args": ["cd /workspace && go test ./..."],
    "env": {"PGHOST": "s-ab12cd34ef567890-db", "PGPORT": "5432", "PGUSER": "kodama"}
  },
  "workspace": {
    "access": "read",
    "mountPath": "/workspace",
    "source": {
      "repositoryUrl": "https://git.example.invalid/acme/inventory.git",
      "inputCommit": "1111111111111111111111111111111111111111",
      "headCommit": "2222222222222222222222222222222222222222",
      "branch": null,
      "expectedTip": null
    },
    "preparation": {
      "commit": "1111111111111111111111111111111111111111",
      "treeId": "3333333333333333333333333333333333333333",
      "receiptDigest": "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
    },
    "lineage": [
      {"stepId": "generate", "attemptId": "att_01", "access": "write", "publishedCommit": "2222222222222222222222222222222222222222"}
    ]
  },
  "endpoints": {"db": {"host": "s-ab12cd34ef567890-db", "ports": {"sql": 5432}}},
  "egress": [],
  "run": {"startedAt": "2026-10-26T10:00:00Z", "deadlineAt": "2026-10-26T11:00:00Z"},
  "policy": {"id": "run-v1", "digest": "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"},
  "inputs": {},
  "outputs": [{"name": "report", "type": "artifact", "path": "/kodama/outputs/tests.json"}],
  "secrets": [{"ref": "secret://project/test-db-password", "version": 3, "name": "PGPASSWORD", "delivery": "env"}],
  "environment": null,
  "limits": {"timeoutSeconds": 1200, "maxOutputBytes": 104857600, "maxOutputFiles": 100, "maxDiagnosticBytes": 10485760}
}
```

A source-free context has `workspace.source: null`, `workspace.preparation: null`, an empty `lineage` until a prior attempt began, `access: none`, and no checkout material anywhere. `endpoints` are the typed map referenced in [expressions](pipeline-format-v1alpha2.md#expressions); a service must be `ready` before any context is issued, so an attempt never receives an endpoint of a service that has not been verified.

The context contains no checkout credential. A new-format attempt workload receives none: only the preparation workload is given read-only repository access, and it is gone before the first attempt starts.

## Command-begin fence

`POST /runtime/v2/attempts/{attemptId}/begin` is made once, after secrets/identity are materialized and `/workspace` is verified, immediately before the child process starts. Request (all fields explicit):

```json
{
  "workspace": {"head": "2222222222222222222222222222222222222222", "trackedClean": true, "untrackedPresent": false, "ignoredPresent": true}
}
```

`workspace` is `null` for a source-free attempt. Response `200`:

```json
{"attemptId": "att_02", "beganAt": "2026-10-26T10:05:00Z", "workspaceState": "modified", "ignoredPresent": true}
```

The server records `beganAt` durably before replying and derives `workspaceState`: `none` (source-free), `clean` (`head == workspace.source.headCommit`, `trackedClean`, and no untracked files), otherwise `modified`. `ignoredPresent` is recorded as disclosure and does not by itself make state `modified`. The entrypoint starts the command **only after** the acknowledgment. Rules:

- Idempotent: a repeat for the same attempt returns the original record; a repeat with a different body is `409 idempotency_conflict` and starts nothing.
- A revoked/cancelled/terminal attempt gets `403 attempt_revoked`; the entrypoint must not start the command. This closes the cancel-versus-start race for scratch mutation.
- A lost acknowledgment leaves the server's record in place. The attempt is **treated as begun** (never eligible for automatic retry) even if the entrypoint, never seeing the reply, did not start the command. Conservatism is intentional; it needs no launch-history capability.
- `begin` is how "pre-command" is proven: an attempt with no recorded `begin` whose workload is verifiably stopped never ran user code. Absence of workload records is never that proof ([ADR 0008](../adr/0008-runtime-seam.md)).
- The recorded state is attached by the server to every artifact/OCI registration and to `lineage` of later attempts as provenance ([DQ-16](../specs/delivery-and-quality-gates.md#requirements)); a client cannot assert it in an artifact request.

## Resource subjects

Only **preparation** (one-shot checkout workload) and **services** have a runtime identity. They authenticate with a one-time credential and exchange it as attempts do (nonce replay, `admission_consumed`, identity match), obtaining a token of audience `kodama-runtime-resource` bound to `(tenant, project, run, resourceKey, backend instance UID)`. Authority per subject:

| Subject | Allowed | Everything else |
| --- | --- | --- |
| `preparation` | token renewal, own `context`, `preparation/complete` | denied: secrets other than the pinned read-only checkout, source, result, artifacts, begin, logs of others, other resources and every attempt route |
| `service.NAME` | token renewal, own `context`, `secrets/resolve` for its pinned bindings | denied: `source`, `result`, `artifacts`, `begin`, model/MCP/deployment proxies, preparation routes, other services' bindings, every attempt route |

An attempt token never works on a resource route and a resource token never works on an attempt route (`403 forbidden`). Revocation follows the resource's stop intent, run cancellation/terminal state, or its deadline, and is checked on every call. A supervisor shares no authority with the workload it supervises beyond the service's own pinned bindings; no preparation/service credential is placed in a mount a step can read.

`ResourceContext` (explicit nulls; `contextVersion: 2`):

| Field | Definition |
| --- | --- |
| `resourceKind` | `preparation` or `service` |
| `tenantId`, `projectId`, `runId`, `resourceKey` | Ownership; must match the token |
| `preparation` | Non-null for `preparation`: `{repositoryUrl, commit, mountPath: "/workspace", timeoutSeconds, maxBytes}` |
| `service` | Non-null for `service`: `{name, role, command: {image, command, args, env}, secrets: [SecretBinding], deadlineAt}`; `command.command` is null when the image defaults run (permitted only without secret bindings) |
| `policy` | `{id, digest}` |

`SecretBinding` is the v1 DTO. `secrets/resolve` returns a bound version only and audits `(run, resourceKey, ref, version)`; a binding made for a step is not a binding for a service even for the same ref.

`POST .../preparation/complete`, request `PreparationReceiptRequest`:

```json
{
  "commit": "1111111111111111111111111111111111111111",
  "treeId": "3333333333333333333333333333333333333333",
  "hooksDisabled": true,
  "submodules": "skipped",
  "credentialsRemoved": true
}
```

Response `{ "receiptDigest": "sha256:...", "recordedAt": "..." }`, where `receiptDigest` is the canonical digest of the request plus `runId`, `resourceKey` and the **bound volume UID** the server observed. The server records a receipt only when the token is valid, the resource is not stopping, `commit` equals the run's pinned input commit, all three safety fields have the literal values above, and no different receipt exists (`409 receipt_conflict`; an identical repeat returns the original). The workspace becomes `ready` only after the receipt is recorded **and** the preparation workload is observed stopped. The receipt is control-plane state; a marker file in the volume is never evidence, since tenant code can write the volume.

## Errors

New codes (statuses): `context_version_mismatch` (409), `receipt_conflict` (409), `resource_revoked` (403, resource tokens after stop/revocation). Existing v1 codes and statuses are unchanged, including `attempt_revoked`, `admission_consumed`, `idempotency_conflict`, `profile_unsupported`, `publication_conflict` and `result_hash_mismatch`. Error bodies never carry credentials, secret values, repository credentials or file contents.

## Fixtures T-110 provides

Under `contracts/runtime/v2/fixtures/`: valid and rejected `ContextV2` (reader, writer with `branch`/`expectedTip`, source-free, endpoints, nonempty lineage), `begin` request/response including `workspace: null`, receipt request/response, resource contexts for preparation and service, the cross-version scenarios (v2 attempt on v1 route, v1 attempt on v2 route, resource token on attempt route, attempt token on resource route), cancelled-before-begin, begin replay/conflict and lost acknowledgment. The v1 `canonical.json` hash vectors are reused as-is to prove `ResultHash` did not change, and v1 fixtures must keep passing untouched.
