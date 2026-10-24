# Real-cluster acceptance tests for T-021

These tests run the Kubernetes executor (`internal/kube`) against a real kind cluster,
real PostgreSQL, the real execution (T-020) and runtime admission (T-019) services, and
the actual T-016 entrypoint (`kodama step`) running inside the Jobs. No NATS, S3 or Git
is needed. Resource prefix: `t021` (cluster `t021`, namespaces `t021-<random>-a|b`,
images `t021.local/*`).

## Running

```sh
make tools                          # pinned kind and kubectl (dev/versions.lock.json)
test/integration/kube/run.sh        # creates/reuses cluster t021, runs the tests with -race
KUBE_TEARDOWN=1 test/integration/kube/run.sh   # also deletes the cluster afterwards
```

The runner exports `dev/.state/t021/kubeconfig`, which the tests find by themselves
(`KODAMA_TEST_KUBECONFIG` overrides it), and sets `KODAMA_REQUIRE_KUBERNETES=1` and
`KODAMA_REQUIRE_POSTGRES=1`: a missing cluster or PostgreSQL **fails** the tests instead of
skipping them, and the runner refuses to start without `KODAMA_TEST_POSTGRES_ADMIN_URL`
(see `test/support/postgres`). Without those variables missing dependencies skip, and
`make test-task TASK=T-021` counts skipped acceptance tests as missing.
The container provider is `DEV_PROVIDER` (podman on Darwin, docker elsewhere, as in the
Makefile); the runner exports it as `KIND_EXPERIMENTAL_PROVIDER` and the fixture uses the same
value for image builds and node access. Nothing is auto-detected.

Each invocation builds the fixture images in its own temporary directory with its own random
tag and, when the tests finish, removes only that directory, those local tags and the imported
references in the node. Concurrent or leftover invocations never share a build context or tag.

## Fixtures and pins

* Cluster: kind and kubectl versions and the node image digest come from
  `dev/versions.lock.json`. The node image carries kind's default CNI (kindnetd, which
  enforces NetworkPolicy with nftables). Enforcement is **probed, not assumed**:
  `Test_restricted_job` connects from a Pod to the allowed runtime endpoint and to
  disallowed destinations and fails unless only the former is open.
* Images: [`fixtures/pins.json`](fixtures/pins.json) records the pinned base image
  (`alpine:3.22@sha256:5291…8fa8`) and the way locally built workload and injector images
  are pinned: they are built from `fixtures/Containerfile.*`, loaded into the node's
  containerd, and referenced as `t021.local/<name>@sha256:<digest of the OCI index>`.
  The entrypoint is built from this repository (`stepbin/`), never baked into the workload image.
* Namespaces are provisioned per test with the baseline the executor requires (Pod Security
  `restricted`, tenant annotation, token-less service account, ResourceQuota, default-deny and
  runtime-egress NetworkPolicies); production provisioning belongs to deployment owners.
* The runtime server is the real T-019 handler over TLS, with a result route that calls
  `execution.CommitResult`, listening on the host; Pods reach it through the provider's host
  address. Its CA is trusted by the entrypoint through the Job's configured CA file.
* The fixtures tolerate a VM clock that lags the host (certificates are valid from 24 hours ago).
* Time-sensitive waits are generous because the cluster shares a loaded machine.

## Acceptance map

| Test | File | Covers |
| --- | --- | --- |
| `Test_create_ack_crash` (named `create_ack_crash`) | `create_ack_crash_test.go` | PX-A02, PX-03, PX-04 — adoption after a lost create acknowledgment and controller restart |
| `Test_foreign_job_collision` (`foreign_job_collision`) | `foreign_job_collision_test.go` | PX-03, PX-04, IT-07 — foreign, changed and forged Jobs are never adopted, mutated or deleted |
| `Test_cancel_create_race` (`cancel_create_race`) | `cancel_create_race_test.go` | PX-08, PX-A06, IT-08 — cancellation during create |
| `Test_restricted_job` (`restricted_job`) | `restricted_job_test.go` | PX-04, IT-07, IT-08, IT-A06 — restricted Pod, bounded storage, token delivery, enforced default-deny network |
| `Test_credential_recovery` | `credential_recovery_test.go` | PX-03 — credential/registration/Secret response loss and races |
| `Test_deadlines_and_missing_workload` | `deadlines_test.go` | PX-09, PX-A06 — startup vs execution timeout, disappeared workload, unknown effects |
| `Test_cleanup_recovery` | `cleanup_test.go` | PX-08, PX-A06 — delete response loss, finalizers, UID replacement, a same-name replacement that is not deleted once the recovery Secret is gone, a genuinely delayed initial create, lookup outages, a lost recovery Secret keeping committed launch evidence, started evidence persisted in the recovery record before deletion and surviving a restarted executor and the Pod's disappearance, forged orphan candidates left untouched, invalid (malformed/foreign-record) and forged-fingerprint recovery Secrets left untouched while a valid aged orphan Secret is collected |
| `Test_namespace_policy_semantics` | `namespace_test.go` | PX-04, IT-07 — the real API's NetworkPolicy port ranges, Job-controller-label-selected permissive policies and scoped/absent quotas are refused by a fresh preflight and by `Dispatch` before any Secret or Job exists; the compliant namespace dispatches |
| `Test_runtime_job_ownership` | `ownership_test.go` | PX-03, PX-04, IT-07 — foreign namespaces, unrelated Pods, wrong owners, UID substitution, cancelled admission/refresh |
| `Test_result_not_inferred_from_job` | `result_test.go` | PX-03, IT-08 — completed Jobs never manufacture success or cleanup |

Under `-tags unit` (`make check-task` without `LAYER=all`) every test here skips before touching the
cluster, container runtime or PostgreSQL (`kubetest.Require` calls `layer.SkipService`); those skips are
not acceptance evidence. Only `test/integration/kube/run.sh` or `LAYER=all` discharges the named tests.

Unit and HTTP-fixture tests (`Test_policy_and_fingerprint`, `Test_workload_transitions`,
`Test_kubernetes_failure_semantics` and others) live next to the code in `internal/kube/`.

## Limits of this evidence

* The result route in `stack_test.go` (`runtimeServer.result`) is **component test
  infrastructure**: it calls `execution.CommitResult` so the entrypoint can finish. It is not
  evidence of assembled result finalization, which T-022/T-062 qualify.
* Forged leftover **Pods** cannot be created reliably against a real cluster (the garbage
  collector removes Pods whose owner does not exist); the forged-Pod and wrong-UID cases are
  covered by `internal/kube/reconcile_test.go` (`Test_reconcile_orphan_ownership`), while the
  kind suite covers forged Jobs and real Pods.
* The preflight subset is conservative (no port ranges; only unscoped quotas count); the fixtures
  provision the explicit ports and unscoped quota it accepts. Service-account checks are the explicit
  forbidden-permission list, not proof of every RBAC permission.
* This is standalone, trusted-engineer component evidence. Hosted qualification is T-061.
