# T-064 — live/retained log qualification

Assembly acceptance of the log path on the real Pod/artifact path: the entrypoint's
container-log mirror (T-078) and retained upload (T-063), the controller's
server-side fallback (T-023), the runtime/results/artifact handlers (T-019/T-022/
T-062), the Kubernetes executor (T-021) and the log service and HTTP route (T-023),
composed **locally** in this test package. Product root wiring remains T-071's; no
production code, migration or shared file changes here.

Resource prefix: **`t064`** (namespaces `t064-<random>-a|b`, images `t064.local/*`,
S3 subprefix `<KODAMA_S3_PREFIX>t064-<random>/`, fixture identifiers). The shared
PostgreSQL helper keeps its own random database names.

## Baseline

| | |
| --- | --- |
| Requirements | PX-10, AC-06 (resource-authorized log queries), IT-03, IT-11, VS-11; PX-A07, IT-A04 |
| Decisions | C-01, C-02, C-03, C-15, C-16; P-01, P-05, P-07, P-10; O-06, O-07, O-11 (resolved); ADRs 0001, 0002, 0003, 0006 |
| Specs | [observability and audit](../../../docs/operations/observability-and-audit.md), [step entrypoint](../../../docs/implementation/step-entrypoint.md), [`contracts/runtime/`](../../../contracts/runtime/) |
| Merged inputs | T-021, T-022/T-062, T-023, T-063, T-078 (mirror) |
| Not claimed | Production Loki, hosted/K8s node deletion, root composition (T-071), marking M1 accepted |

## Acceptance map

| Name | Test | Covers |
| --- | --- | --- |
| `missing_node_tail` | `Test_missing_node_tail` (`missing_node_tail_test.go`) | A real response interrupted after real mirrored bytes is a **partial/interrupted** retained tail (a strict prefix of the real mirror, never complete, never extended); an unavailable backend is retryable unavailable evidence with no artifact; a removed workload is `workload_missing` evidence with no fabricated artifact and is not retried; the partial tail survives a restart and a removed workload; an unobserved Job is explicit (B-3) |
| `retained_log_join` | `Test_retained_log_join` (`retained_log_join_test.go`) | Ordinary stdout/stderr reach the real Kubernetes log through the real mirror (no shell tee); delivered credentials, tokens and real secrets (file and env) never reach the Kubernetes log, retained logs, controller captures, service reads or Job/Pod manifests; wrapper log + bound sidecar observed after the finalization barrier; failed command / missing output keep diagnostics; sidecar-less and early-observed logs stay `unknown`; truncation; failed wrapper upload falls back to controller collection; lost responses and concurrent Observe/Collect on one connection are stable; wrapper capture preferred over controller capture; execution results never change |
| `bounded_tail` | `Test_retained_log_join/bounded_tail` (`bounded_tail_test.go`) | Default (100) and explicit tails, ceiling, oversized/malformed tails rejected before any backend read, line/byte/line-count/scan bounds, NDJSON framing (meta, chunks, summary), live tail and follow of a running Pod, same answers after a restart with the workload gone |
| `log_cross_tenant_join` | `Test_log_cross_tenant_join` (`log_cross_tenant_join_test.go`) | Foreign tenant/project/run/attempt/artifact/runtime-session access is one indistinguishable 404 across live, wrapper, controller and auto paths with **no** Kubernetes API, live-reader or object-store read; foreign linkage refused by the store; RLS under the non-owner role; copied sidecar never inherits completeness; revoked membership; pooled single-connection tenant reuse |
| `foreign_log_selector` | `Test_log_cross_tenant_join/foreign_log_selector` | Namespace/Pod/container/label/field/URL/backend selectors rejected before any backend; foreign namespace, replacement Job UID, init/unknown container and path injection refused by the real Kubernetes reader without a log request; decoy Pods in the tenant's and another tenant's namespace never read |

All tests fail (not skip) when PostgreSQL, S3 or Kubernetes is unavailable (`TestMain`
defaults `KODAMA_REQUIRE_*=1`); under `-tags unit` they skip before touching any
service and are not acceptance evidence.

## Composition and fixture boundaries

Real: kind cluster Jobs/Pods (restricted Pod Security, default-deny + single runtime
egress NetworkPolicy, enforced), the T-021 executor/observer/reconciler, PostgreSQL
under a separate owner role and non-owner runtime roles (the merged migration order
plus `runtimeauth` and `logs`), S3 (LocalStack) through the scoped signed-upload
path, the runtime-enabled execution store with the guarded result path, the real
runtimeauth/results/artifact services and HTTP handlers behind one TLS endpoint Pods
reach through the provider's host address, the real entrypoint (`kodama step`,
`entrypoint.Run`, built static by `stepbin/`), the real secrets resolver, the real
Kubernetes log reader, the log service/controller/store/HTTP route, and the
standalone identity authority. Concurrency subtests use a single-connection
PostgreSQL pool for the log, artifact and identity stores, so a path that holds a
connection while asking a peer for another would deadlock.

Test-local pieces (existing ports only):

* `tenantPlacement` — each tenant's own namespace and the primary container.
* `deliveredValues` — the `logs.Redactions` source: the admission credential and
  runtime tokens observed at the runtime boundary plus the seeded secret values.
* `tapLive`/`apiTap` — record every live open and API request; inject node loss at
  the selected Pod's log transport (connection reset after N real bytes, backend
  unavailable). No node is stopped or deleted.
* The runtime endpoint (`runtime_test.go`): dispatches to the real handlers, records
  events, injects per-attempt faults (refuse, lose the response), and forwards only
  `PUT` of an object under this test's bucket prefix whose exact URL a reservation
  issued; the artifact store signs for the endpoint's origin through
  `s3.Config.TransferClient`, so the signed Host/semantics are preserved.
* Secret pins — the internal cleanup run this fixture uses (no Git checkout) pins no
  secrets (work runs do, at admission); `pinSecrets` registers the pin a work run
  would hold through the secrets owner's own API before the Pod can resolve.
* `hold` — scripts wait 6 s before running so the reconciler persists the Job UID
  first (see B-3).
* Nonwriter/no-checkout commands only: source publication is not in this suite.

## Blockers and findings

* **B-1 (closed):** retained container-log transport. T-078's `entrypoint.Run`
  mirrors bounded, redacted command output to the container stderr; the tests use
  that real path (no shell tee) and assert the mirror's exported markers
  (`entrypoint.MirrorTruncationMarker`) as loss indicators.
* **B-2 (open, outside T-064):** marker-derived completeness. The controller records
  a clean EOF as `unknown/controller_capture` and does not recognize
  `MirrorTruncationMarker`, `MirrorGapMarker` or `MirrorDisabledMarker`, so a
  retrieved mirror that ended in a marker is `unknown`, not `partial`. This is
  conservative (never falsely complete). `truncation_and_mirror_markers` asserts the
  conservative behavior (never complete, marker preserved in the retained bytes, the
  wrapper's own sidecar declares `partial/truncated`) and logs B-2; it does **not**
  require marker-derived `partial`, fake a truncated read or impose a smaller limit.
* **B-3 (finding, owner T-020/T-021/T-023):** execution records the observed Job UID
  only when its observer sees the workload before the result lands, and a late
  observation of a terminal attempt rewrites nothing. A command that finishes first
  leaves `ObservedJobUID` empty, so every log read is `workload not observed` and the
  controller records `workload_missing` for a Job and Pod that still exist (verified
  by `job_not_yet_observed`; the evidence is explicit unavailable, never a fabricated
  log). The fixture avoids it by holding commands for 6 s while the reconciler
  observes. A production cadence or a join on the persisted Job name is for the
  owners.
* **B-4 (finding, owner T-016/T-063 vs T-021):** any declared secret fails in a real
  Pod: the entrypoint `chmod`s `/kodama/secrets`, the memory-backed volume root is
  root-owned (group-writable via `fsGroup`), and the non-root user gets
  `chmod /kodama/secrets: operation not permitted`. The fixture's `stepbin` rewrites
  the Job's `--secrets-dir=/kodama/secrets` argument to a subdirectory of that volume
  (the entrypoint's documented flag); nothing else is altered.

## Verification

```sh
gofmt -l test/integration/logs
go vet ./test/integration/logs/...
source .harness/test-env.sh && go test -race -count=1 -timeout=30m -v ./test/integration/logs/...
```

Last run (single invocation, all services real): `gofmt` clean; `go vet` clean;
`ok kodama/test/integration/logs 242s`, `Test_missing_node_tail`,
`Test_retained_log_join` (including `bounded_tail`) and `Test_log_cross_tenant_join`
(including `foreign_log_selector`) PASS, no skips. The suite builds the entrypoint,
loads two images, creates two namespaces per top-level test and runs about 30 Jobs.

## Cleanup inventory (all task-owned)

* Namespaces `t064-<random>-a|b` — deleted by each test's cleanup (the cluster, its
  nodes and other tenants' namespaces are never touched).
* Images `t064.local/{workload,injector}:<id>` in the container provider and the
  node's containerd (`ctr -n k8s.io images rm`), and the temporary build directory.
  The pinned Alpine base image is a shared pull cache and is left in place.
* S3 objects under `<KODAMA_S3_PREFIX>t064-<random>/` (removed by the S3 support
  helper; it refuses any prefix that does not end in `/`).
* PostgreSQL: per-test databases and roles of the shared helper (dropped by it).
* The TLS runtime endpoint and every test HTTP server are closed at test end.

## Limits of this evidence

Node loss is emulated at the Pod's log transport and by removing the fixture-owned
Job and Pods; physical node deletion, hosted and Loki qualification and M1 acceptance
are not claimed. The mirror is unverified transport; only a settled verified sidecar
makes a wrapper capture `complete`.
