# `kodama step` contract

**Scope:** shared contract checkpoint T-003, implementation T-016/T-019/T-022. This is the runtime keystone for independent executor, runner, and result work.

**Run models.** This document describes the current `attempt_private` behavior (wire [`/runtime/v1`](../../contracts/runtime/README.md), frozen). The designed `run_shared` model (T-109 contract, T-113 implementation; not delivered or qualified) changes source preparation, the `/workspace` mount and retry proof; its deltas are in [`run_shared` (v2) sequence](#run_shared-v2-sequence) and the [runtime v2 contract](../contracts/runtime-v2.md). Result, artifact, source-proposal and secret-resolution messages and `ResultHash` are identical in both.

**Next contract:** [ADR 0012](../adr/0012-container-sdlc-and-artifact-releases.md) adds admitted artifact fetch and tool-neutral release/delivery helpers using this same entrypoint. Direct harness/build/publish/deploy commands use ordinary exit/declared-output collection; managed profiles may require additional structured reports. T-140 freezes any incompatible extension before T-142 changes consumers; old result hashes, begin semantics and source/publication authority remain intact.

## Runtime layout

An init container copies a static, architecture-matching `kodama` binary into a shared tools volume. The primary workload image must contain Git and the declared command dependencies (`run_shared`: Git only for publication by a writer; readers need only their command dependencies). It starts `/kodama/bin/kodama step`. The image runs as non-root under the fixed Kubernetes policy.

| Path | Purpose |
| --- | --- |
| `/workspace` | `attempt_private`: private disposable checkout; no shared writable PVC. `run_shared`: the run's shared scratch, prepared once and mounted by one attempt at a time |
| `/kodama/outputs` | Bounded output collection root |
| `/kodama/results.json` | Command/wrapper result manifest |
| `/kodama/secrets` | Memory-backed injected secret files, excluded from collection |
| `/kodama/identity` | Short-lived attempt token; no platform credentials |
| `/kodama/bin` | Injected binary, read-only to command |

The Job references a one-time admission token bound to attempt/Job identity. The entrypoint exchanges it for current scoped authority before running a command. The Kubernetes executor verifies observed Job/Pod ownership; cancellation can revoke admission even if Pod creation raced with it. Token refresh requires an active attempt and has a bounded maximum lifetime.

## Internal HTTP interface

All routes are under `/runtime/v1/attempts/{attemptId}` and validate audience, tenant/project, attempt, and current authority.

| Route | Request/response responsibility |
| --- | --- |
| `POST /admit` | Exchange one-time admission credential; return scoped runtime token and immutable context |
| `POST /token` | Renew only a currently authorized attempt |
| `GET /context` | Source URL/commit, command/profile, inputs/spec snapshot with pinned design-baseline references, allowed secret references, pinned environment/configuration where declared, limits, stable action key |
| `POST /secrets/resolve` | Resolve one declared version into a scoped response/file delivery |
| `POST /artifacts` | Reserve named upload with digest/size/type; return short-lived upload instructions |
| `POST /artifacts/{id}/complete` | Verify uploaded object ownership/digest/size; mark usable |
| `POST /source` | Propose commit, expected branch tip, and temporary bundle artifact ID; return accepted/pending/conflict |
| `POST /result` | Idempotently submit the final result under attempt ID + canonical result hash |

Runtime model/MCP/deployment calls use the same attempt identity with explicitly granted actions on their owning interfaces. There is no general tenant admin credential in the Pod.

The [design baseline](design-conformance.md#review-baseline) travels in the immutable specification/instruction context; T-003 freezes its reference serialization alongside the context fixtures, as the nullable `designBaseline` field of the context ([contract](../../contracts/runtime/README.md#design-baseline-and-conformance-report)). It identifies design inputs, not additional authority. T-031 emits conformance as a declared artifact and T-034 binds that evidence through the existing gate subject.

## Result envelope

```json
{
  "outcome": "completed",
  "exitCode": 0,
  "reason": null,
  "question": null,
  "sourceCommit": "1111111111111111111111111111111111111111",
  "outputs": {"report": "art_tests"},
  "diagnosticArtifactIds": ["art_log"]
}
```

The `/result` request wraps this envelope with its `resultHash`; the frozen field rules, `exitCode: null` when no exit status was observed, and canonical hashing are in the [runtime contract](../../contracts/runtime/README.md). Allowed command/wrapper outcomes are `completed`, `failed`, `needs_input`. The server derives attempt status; cancellation/timeout can supersede an otherwise completed result. `sourceCommit` is null for nonwriters. Failed attempts can retain diagnostics, but successful descendants never consume their declared success outputs.

## Execution sequence

1. Admit and fetch pinned context; reject stale/cancelled admission before command launch.
2. Clone exact source with read-only HTTPS credentials. Disable hooks/submodule recursion in the baseline. A no-workspace step skips checkout.
3. Materialize authorized secrets outside source/output roots; start the child process group with explicit argv/env and bounded scratch/resources.
4. Capture logs and propagate cancellation/timeouts to the process group. In-Pod logging plus controller fallback handles wrapper/node failure.
5. Validate/collect declared files and manifest outputs. Reject path escape, symlink escape, excessive size, wrong types, and foreign artifact IDs.
6. For a successful writer, prepare a source commit/bundle and call `/source`; the server owns remote publication credentials and CAS. A pending/unknown publication must settle before success.
7. Submit result and retry the same result hash on transport loss. A conflicting second hash returns conflict and cannot rewrite the accepted result.
8. Exit only after durable acknowledgment or a bounded finalization failure. Controller reconciliation distinguishes an acknowledged result from a lost wrapper response; it does not infer valid outputs from exit zero.

No node/PVC checkpoint service is required. Git bundle transport is temporary and removed after publication/retention handling. Permanent source recovery uses Git refs/commits.

## `run_shared` (v2) sequence

Normative design; the entrypoint implementation is T-113. Differences from the sequence above:

1. Admit with the one-time credential on `/runtime/v2/attempts/{id}/admit` and fetch `ContextV2`. An attempt that receives a v1 context on a v2 run (or the reverse) stops before any command.
2. **No checkout.** The attempt workload holds no checkout credential and never clones, resets or cleans. A separate preparation workload with read-only credentials prepared `/workspace` once before the first attempt; the entrypoint only verifies it: the mount exists and, for a source-backed run, the context's prepared commit objects are present and the tree is a usable repository, otherwise it fails before any command with reason `workspace_unverified` (treated by execution as `workspace_lost`). Source-free runs verify only that the bound `/workspace` mount exists and is writable: it is empty **only at initial provisioning** (the first attempt, whose context lineage is empty), and every later attempt must find and leave intact whatever earlier steps wrote, so the entrypoint never requires emptiness, clears, resets or recreates it. (Which volume it is, is the controller's UID binding; a file inside the volume is not identity evidence.) Git runs with explicit sanitized configuration; repository-written config, hooks, filters, credential helpers and ownership changes are never trusted.
3. Materialize authorized secrets and identity outside `/workspace` (own memory-backed mounts, private to this attempt), then call **`POST /begin`** with the workspace head and cleanliness flags (`workspace: null` for a source-free attempt). The command starts only after the acknowledgment; a `403 attempt_revoked` or an unacknowledged call means no command runs. Begin orders command authorization against cancellation; it does not make the OS start atomic with a later cancel, so a cancellation after a recorded begin still requires the controller's verified stop. The record is what later proves "pre-command" for retry and is the source of artifact provenance.
4. Run the command with its pinned image/argv/env. Declared service endpoints arrive as resolved environment variables/arguments; the entrypoint evaluates no expressions. Build-client credentials and registry auth files live in attempt-private paths, never in the shared workspace, and are destroyed on exit.
5. Collect outputs, then for a successful writer commit **the whole shared tree's non-ignored changes** (including files generated by earlier steps), bundle and call `/source` exactly as before; the server validates expected tip and ancestry. The scratch does not create a checkpoint.
6. Submit the result with the unchanged canonical hash. Exit only after durable acknowledgment or bounded finalization failure; leave the scratch for the next holder, which the controller will not start until this workload is verified stopped.

Source-free attempts follow the same sequence with no repository. Preparation and service workloads use the resource-subject routes only ([resource subjects](../contracts/runtime-v2.md#resource-subjects)); a service supervisor, when one is required for secret delivery, execs the declared argv with signal forwarding, receives no attempt authority and shares no process namespace with a command runner.

## Finalization races and tests

Late results after revocation cannot change state or publish source. An uploaded object alone is not an accepted artifact. Cancellation during upload can leave an orphan cleaned by retention. Git success followed by database failure is reconciled from the pending publication fields and remote ref.

Required shared fixtures: duplicate result, conflicting hash, missing output, exit failure with log, path escape, wrong tenant artifact, cancelled admission, token refresh denial, publication conflict, publication response loss, and incompatible wrapper profile. T-003 freezes shapes in [`contracts/runtime`](../../contracts/runtime/README.md) (schemas, OpenAPI, Go DTOs, canonical hash rules, fixtures, retry semantics); module and kind integration tests verify behavior rather than treating fakes as production evidence.
