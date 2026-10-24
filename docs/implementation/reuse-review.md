# Upstream reuse review

Reviewed on 2026-10-20 against Kodama Forge baseline
`673a910af918b6113451c65bdd0454f274ec6545`. Scope: small packages,
lean design ideas and outstanding task dependencies. Source checkouts were read
only; the running harness's task worktrees were not changed.

## Sources and disposition

| Source revision | Reviewed paths | Result |
| --- | --- | --- |
| Platform `05dec0d81a15f9c3bcac826508f0342761343d38` | `core/`, `platform/api/oapi-codegen.yaml`, `platform/telemetry/logger.go`, `internal/adapters/`, `internal/urlshape/` | Keep the existing small hexagonal modules, `slog`, stdlib HTTP and generated OpenAPI types. Secrets providers were already copied. Logger setup is ordinary configuration coupled to Platform env/version packages; URL-shape code is analytics-specific. Neither merits another package here. Make portable API client generation explicit in T-027. |
| Tekton `592dfb840f02a0e9e2920f75b1737372ceb17c15` | `pkg/apis/pipeline/v1/pipeline_types.go`, `pkg/reconciler/pipeline/dag/`, `pkg/termination/` | Keep typed immutable definitions separate from run/attempt status and validate dependencies before dispatch. Kodama already has these boundaries. Tekton's API objects bring Kubernetes/Knative metadata and controller semantics; its DAG adds no needed capability over the existing validator. Termination-message results have a 4 KiB limit and different recovery semantics. Copy none. |
| Harness `71005497269fecb8f389f360a75436e305583e9f` | `netpolicy/`, `contextutil/`, `livelog/`, `stream/`, `lock/`, `app/pipeline/triggerer/dag/` | Adapt only the standard-library address classifier/dial hook into [pkg/netpolicy](../../pkg/netpolicy/README.md). Keep the existing Kubernetes/S3 logs and Postgres/NATS protocols rather than adding Redis/in-memory services. `contextutil` is a one-line stdlib composition. The DAG's skipped-dependency traversal conflicts with Kodama's explicit skip propagation. |
| Kodama prototype `03cc04b6bd1bd6b03af9436f48769bfe9089a528` | `internal/jsonutil/`, `internal/engine/output_test.go`, `adapters/openai/`, `worker_workspace_recovery_test.go` | The JSON helper preserves integer precision but does not match Forge's canonical bytes/defaults/digest contract. The model adapter buffers responses rather than implementing the required streaming gateway and depends on the embedded runtime types. Recovery tests are useful scenario references; workspace snapshots/sessions are not the appliance's Git/attempt model. Copy none. |

The local source paths were `../platform`, `vendor/tekton`, `vendor/harness` and
`../kodama` relative to the primary checkout. The new package records its exact
upstream URL and Apache license/notice. No upstream module or additional Go
dependency is required. It is staged for a consumer, not wired into runtime.

## Small design conclusions

- Keep a declarative pipeline with one ordinary workload container per attempt.
  Familiar `apiVersion`/`kind`/typed-step structure does not require CRDs or a
  second scheduler. Existing C-02/P-09 and PX-01/PX-04 already cover this.
- Keep source publication and result finalization behind their current contracts.
  Neither Kubernetes termination messages nor the prototype's hashes can replace
  those wire/recovery semantics without a migration.
- Generate boundary clients, not shared business runtimes. T-027 owns the public
  OpenAPI and generated Go client; Platform can generate its own from the same
  document. The [platform spec](../specs/platform-integration.md) records stage
  co-location, possible multiple installations, the likely dashboard host and
  Platform-owned edge/cloud delivery.

## Task changes

| Lane | Development prerequisites | Integrated proof retained |
| --- | --- | --- |
| T-041 ChatOps | T-005 event envelope + T-010 authorization; fixtures for the published gate-event references | T-046 real gate producer/current-subject links; T-044 business-loop acceptance |
| T-043 and T-047 blueprints | T-014 definitions + T-030 profile contracts; real isolated application tests and delivery scripts | T-046 factory-driven build/promotion/evidence for both templates; T-044 acceptance |
| T-027 public client | Existing M1 composition dependencies | Generated-client submission/status/error/replay tests and CI regeneration checks; later composition tasks own regeneration |

This opens three disjoint development lanes without moving milestone acceptance
or resolving provider decisions. It adds no tasks and changes no active task's
owned implementation paths. Logs still wait for executor/results, and the real
OpenCode wrapper still waits for the actual gateway/MCP implementations.

At this review's baseline, O-03 (webhook provider) blocked full M2 integration,
O-15 blocked the UI, and O-18/O-19 blocked hosted identity/custody. The subsequent
[owner update](../decisions.md#owner-update--2026-10-20) selects native push hooks,
deferred Platform-only UI, OAuth UUID tenant/role claims and versioned encrypted
secrets/proxy delivery. Actual integration evidence is still required. Stage KMS
startup wiring remains T-029's; these are coordination points, not missing utility packages.

## Review baseline and handoff

Requirements: SD-01/SD-03/SD-05–SD-08, AC-01/AC-02/AC-08–AC-12,
GM-09 and PI-03/PI-07/PI-09. Existing product behavior and wire formats remain
unchanged; C-19 records the owner's staging direction. Task dependency changes
move cross-module checks to their existing integration owners.

Before merging after the harness pauses, rebase this task onto current `main`,
preserve newly completed task statuses, and regenerate the index with
`bun ../openforge/src/cli.ts index --write`. Run `make docs-check`,
`make lint` and `make test`; retain independent review approval for the resulting
diff. Restart the harness only after the reviewed changes are merged, so newly
eligible lanes read the revised task contracts. No task is marked done by this review.
