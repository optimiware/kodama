# `kodama step` contract

**Scope:** shared contract checkpoint T-003, implementation T-016/T-019/T-022. This is the runtime keystone for independent executor, runner, and result work.

## Runtime layout

An init container copies a static, architecture-matching `kodama` binary into a shared tools volume. The primary workload image must contain Git and the declared command dependencies. It starts `/kodama/bin/kodama step`. The image runs as non-root under the fixed Kubernetes policy.

| Path | Purpose |
| --- | --- |
| `/workspace` | Private disposable checkout; no shared writable PVC |
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

## Finalization races and tests

Late results after revocation cannot change state or publish source. An uploaded object alone is not an accepted artifact. Cancellation during upload can leave an orphan cleaned by retention. Git success followed by database failure is reconciled from the pending publication fields and remote ref.

Required shared fixtures: duplicate result, conflicting hash, missing output, exit failure with log, path escape, wrong tenant artifact, cancelled admission, token refresh denial, publication conflict, publication response loss, and incompatible wrapper profile. T-003 freezes shapes in [`contracts/runtime`](../../contracts/runtime/README.md) (schemas, OpenAPI, Go DTOs, canonical hash rules, fixtures, retry semantics); module and kind integration tests verify behavior rather than treating fakes as production evidence.
