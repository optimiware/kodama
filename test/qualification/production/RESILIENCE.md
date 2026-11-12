# Q02 failure and resilience probes

These probes cover the failure half of Q02: the production entrypoint's own
refusals, workspace and workload loss, controller and optional-dependency
outages, terminal-cleanup crash attribution, KMS negative cases and the identity
credential matrix. They are qualification checks, not component tests.

The stage probes below are real probes against a deployed revision. With no
stage inputs they skip and name every missing input (including a destructive
probe's exact confirmation token); `Q02_REQUIRE_STAGE=1` turns a missing input
into a failure. They never pass by running nothing. The product probes under
`TestHarness*` run the real command and packages, need no stage; they skip
only when a named host tool (`go`, `curl`, `sha256sum`) is missing.

## Stage probes

| Probe | Invariants |
|---|---|
| `TestStageResilienceControllerPodDeletion` | Deleting the controller mid-execution leaves the action deadline, workspace file and side-effect marker unchanged; the replacement becomes Ready, the API recovers and the execution converges with exactly one marker. |
| `TestStageResilienceNATSPodDeletion` | Deleting `nats-0` restarts a Ready pod; the committed event stream catches up with no lost or duplicated event id; the execution still converges and cleanup names its identities. |
| `TestStageResilienceBrokerOutage` | With the broker scaled to zero, an execution still commits reads and writes, then catches up after a verified in-body restore. The saved replica count is restored by cleanup on every path. |
| `TestStageResilienceUncertainStopBlocksHandoff` | A TERM-resistant predecessor whose stop is unconfirmed keeps the queued replacement blocked; cleanup identities, attempts and the pending-stop reason stay attributable; the replacement restores from the answer without replaying the first action. |
| `TestStageResilienceCleanupAttribution` | A terminal execution's cleanup names its workspace and workload; no Job, pod or unretained claim labelled with either identity remains. |
| `TestStageResilienceWorkspaceLoss` | Deleting the exact retained claim a running workload mounts fails the execution visibly; the successor is never activated; the marker appears once; cleanup still names both identities; a fresh attempt succeeds. Destructive and gated on `Q02_WORKSPACE_LOSS_CONFIRM=destroy-claim`. |
| `TestStageResilienceWorkloadPodLoss` | A force-deleted workload pod (the guard's node-loss emulation) fails the execution visibly; the Job's `backoffLimit 0` must not replay the command (the marker appears at most once; a force-killed pod may lose its final log chunk, so only a duplicate is treated as replay); cleanup stays attributable; a fresh attempt succeeds. |
| `TestStageResilienceCleanupCrashAttribution` | The probe cancels a running TERM-resistant workload, so the execution is `cancelled` while its stop is unconfirmed and cleanup stays `pending` across the forced controller deletion. A pre-crash inspect must observe cleanup `pending` with a non-empty workspace identity (an already-complete cleanup skips with the named reason `resilience-cleanup-crash-vacuous`). The controller pods are then force-deleted (`--grace-period=0 --force`) so the old process cannot finish cleanup while terminating; after they are gone the obligation must still be `pending` (an already-complete record skips with the named reason `resilience-cleanup-crash-not-attributable`), and only then may the replacement finish it: same workspace/workload identities, exactly one marker and no orphaned resources. |
| `TestStageResilienceGitDependencyOutage` | Scaling the tenant Git StatefulSet to zero does not block ordinary task-free work: the probe covers core independence from the optional dependency and the observed zero-replica outage/restore, not a Git-backed push flow. The outage is the observed zero-replica object; the saved count is restored in body with a read-back and a measured Ready time. |

## Product probes (no stage)

| Probe | Invariants |
|---|---|
| `TestHarnessProductionEntrypointRefusals` | The real `kodama server --profile=production` refuses test/dev key adapters (`keyfile`, `ephemeral`), plaintext broker transport, and the required OIDC settings and key reference when they are set explicitly empty; every refusal is non-zero, names the setting and renders no secret. |
| `TestHarnessKMSNegativeMatrix` | A foreign key family, an unconfigured version, a KMS failure, a wrong-length key and a tampered envelope are refused without plaintext; a foreign reference or unknown version never reaches the KMS; a provider error is not echoed. |
| `TestHarnessIdentityCredentialMatrix` | Admin is not Reviewer; a service holding Admin never administers; an execution may submit its own outputs but never resolve requests or administer; a role in one tenant never authorizes another; a disabled principal is refused; an unknown operation is an error. |

## Stage prerequisites

The stage probes need `Q02_API_URL`, `Q02_CA_PEM`, `Q02_TLS_SERVER_NAME`,
`Q02_ADMIN_TOKEN` or `Q02_ADMIN_TOKEN_FILE` (not both), `Q02_PROJECT_ID`, `Q02_TENANT_NAMESPACE`,
`Q02_KUBE_CONTEXT`, `Q02_EVIDENCE_DIR` and the digest-pinned `Q02_IMAGE` (a
POSIX shell, `printf`, `grep`, `echo` and `sleep`; writable `/workspace`).
`Q02_TENANT_NAMESPACE` is the derived `kodama-tenant-<tenant>` namespace, never
the bare tenant id.

Workspace loss additionally requires `Q02_WORKSPACE_LOSS_NAMESPACE` (the same
tenant namespace) and `Q02_WORKSPACE_LOSS_CONFIRM=destroy-claim`; run it only on
a disposable stage, because it deletes one retained claim.

Git outage requires `Q02_GIT_SERVICE_A` (and `Q02_TENANT_ID`) and a tenant Git
StatefulSet at non-zero replicas. The probe derives the StatefulSet name from
the service name and restores the saved count. `Q02_GIT_REPOSITORY_A` is unused
by this probe: it never opens, clones or pushes to a repository, so the probe
proves core independence and the observed zero-replica outage/restore, not a
Git-backed push flow. (`Q02_GIT_REPOSITORY_A` remains an input of the isolation
probes.)

## Notes

- No probe falls back to another identity, recreates a lost workspace or
  replays a command to make a failure look green. A lost workload or workspace
  must surface as a terminal failure with its identities still attributable.
- The KMS probe uses the production Scaleway provider through the package's
  injected client port; live provider compatibility is an environment
  qualification, not a claim from this probe. The stage runs cover Scaleway
  only.
- No token, Secret value, KMS response, log body or workspace marker is written
  to evidence.
