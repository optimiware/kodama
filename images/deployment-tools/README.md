# images/deployment-tools — ordinary deployment tools (task T-065)

The restricted, ordinary-container tools contribution for build/preview/stage
deployment ([delivery spec](../../docs/specs/delivery-and-quality-gates.md#build-and-deployment-are-normal-steps),
C-14 and O-12): a pinned image holding `kubectl`, `curl`, `jq`, `git` and three
example scripts — **apply**, **lookup** and **delete** — that a pipeline step runs
as its `command`. It is **not a deployment engine**: there is no controller, no
generic cloud adapter and no privileged builder. The records, annotation
contract and cleanup authorization it consumes are T-035's
([`internal/delivery`](../../internal/delivery/README.md), `api/delivery.yaml`,
`internal/delivery/testdata`); scheduling and the cleanup run are T-066; the real
record/apply acknowledgment-loss join is T-067. Requirements: DQ-07–DQ-10.
Acceptance evidence is [`test/integration/deployment-tools`](../../test/integration/deployment-tools/README.md).

## Layout

| Path | Purpose |
| --- | --- |
| `Containerfile`, `build.sh` | Pinned image build (podman), `evidence/build.json` |
| `scripts/apply.sh` | register → verify → record intent → apply → observe → report |
| `scripts/lookup.sh` | read-only: the recorded deployment + the objects found by annotation |
| `scripts/delete.sh` | scoped, repeatable delete of exactly the recorded owned objects |
| `scripts/lib.sh` | shared functions (record interface client, observation, rendering) |
| `examples/manifests/` | example manifests using the `@@IMAGE@@` token |
| `evidence/build.json` | what the last documented build produced |

## What the image is, and is not

* **Pinned**: base `debian@sha256:5ae3c39e…` (bookworm-slim, the same base as the
  T-031 image); apt packages from the `snapshot.debian.org` timestamp
  `20260824T000000Z` (installed versions in `/opt/kodama/deploy/packages.txt`);
  `kubectl` 1.37.1 with the per-architecture sha256 of
  [`dev/versions.lock.json`](../../dev/versions.lock.json) (a test keeps the two
  equal; the download is verified inside the build); the scripts copied from this
  directory (their sha256 in `/opt/kodama/deploy/scripts.sha256` and
  `evidence/build.json`).
* **Reproducible**: every layer timestamp is the epoch (`--timestamp 0`) and apt
  state is scrubbed. `build.sh --verify-reproducible` rebuilds without cache and
  records whether the two image IDs are identical (compare cold builds: a cached
  build keeps the base image's own history timestamp); the result of the last run is in
  `evidence/build.json` (`reproducibility`) and the task handoff. Reproducibility
  of the *registry* digest depends on the pusher; the evidence records the digest
  of each documented push.
* **Unprivileged**: `USER 65532:65532`, no `ENTRYPOINT`/`CMD`, no container runtime,
  no daemon socket, no credential baked in. It runs under the platform's ordinary
  step restrictions — every capability dropped, `no-new-privileges`, read-only root
  filesystem (only `/tmp` is written), bounded processes and memory — and the
  acceptance tests run the scripts exactly that way. It needs **no** privileged or
  host-mounted runtime.
* **Namespace-scoped**: the only cluster authority is the project's kubeconfig
  secret (O-12: the environment's `kubeconfig` credential, delivered with
  `delivery: file` as `$KODAMA_SECRETS_DIR/KUBECONFIG`). Every kubectl call names the deployment's
  namespace explicitly, the namespace comes from the **recorded configuration**
  (the receipt), never from an input, and the credential's RBAC is the boundary:
  the scripts additionally refuse cluster-scoped kinds and objects naming another
  namespace before they record or change anything.

The image also contains `git`, which `kodama step` needs for the checkout. M-4 of
the [harness image](../harness-opencode/README.md#mismatches-found-and-who-owns-the-next-step)
(Git's ownership check on a root-owned `emptyDir` workspace) applies to every
workload image under the Kubernetes runtime and is owned by T-016/T-021; this image
does not carry a shim for it.

## Build

```bash
images/deployment-tools/build.sh [--tag TAG] [--push REPOSITORY] [--out DIR] [--verify-reproducible]
```

Prints `image: <repository>@sha256:<digest>` (pushed) or the local image ID and
writes `build.json`. The acceptance run builds the image itself with this script.

## Running a script as a step

The step entrypoint exports `KODAMA_ATTEMPT_ID`, `KODAMA_RUN_ID`, `KODAMA_STEP_ID`,
`KODAMA_OUTPUTS_DIR`, `KODAMA_RESULTS_FILE`, `KODAMA_SECRETS_DIR`, and writes the
attempt's token to `/kodama/identity/token`. It does **not** export the platform
origin (M-1 of the harness image): the step supplies `KODAMA_SERVER_URL` in its
`command.env`. Illustrative step (placeholders; expression and input syntax belong
to the [pipeline format](../../docs/contracts/pipeline-format.md)):

```yaml
- id: deploy
  kind: container
  image: registry.example.invalid/kodama-deployment-tools@sha256:<digest>   # this image
  environment: preview              # pins the configuration and the kubeconfig credential at admission
  workspace: {access: read, from: run}
  command: ["/opt/kodama/deploy/apply.sh"]
  env:
    KODAMA_SERVER_URL: https://kodama.example.invalid
    DEPLOY_MANIFESTS: /workspace/deploy
    DEPLOY_REPOSITORY_URL: ${{ inputs.repository }}
    DEPLOY_COMMIT: ${{ inputs.commit }}
    DEPLOY_IMAGE_ARTIFACT_ID: ${{ steps.build.outputs.image.artifactId }}
    DEPLOY_IMAGE: ${{ steps.build.outputs.image.registryRef }}@${{ steps.build.outputs.image.digest }}
  timeoutSeconds: 600
  outputs:                          # exactly the documented apply outputs (or a DEPLOY_OUTPUTS subset)
    deployment_id: {type: string}
    deployment_status: {type: string}
    configuration_digest: {type: string}
    image_digest: {type: string}
    source_commit: {type: string}
    ready: {type: boolean}
    object_count: {type: integer}
```

## Contract

### Common environment

| Variable | Meaning |
| --- | --- |
| `KODAMA_SERVER_URL` | required; `https`, plain `http` only for loopback |
| `KODAMA_ATTEMPT_ID`, `KODAMA_RUN_ID`, `KODAMA_STEP_ID` | from the entrypoint; the logical action key is `RUN:STEP` |
| `KODAMA_TOKEN_FILE` | default `/kodama/identity/token`; re-read on every request, sent to `curl` on stdin (never in argv) |
| `KODAMA_CA_FILE` | default `/kodama/bootstrap/ca.crt`; used when the file exists |
| `KODAMA_RESULTS_FILE` | required; the result manifest |
| `KUBECONFIG` | default `$KODAMA_SECRETS_DIR/KUBECONFIG`; exactly one file |
| `DEPLOY_OUTPUTS` | optional comma list restricting the outputs written (an undeclared output is refused by the entrypoint); an unknown name fails with `outputs_unknown` |
| `RT_ATTEMPTS`, `RT_RETRY_DELAY` | retries of an unanswered record call (default 5, 1 s); every call is idempotent by its logical action |

### Exit status and result manifest

A script exits **0 whenever a result manifest was written** — `completed` with
outputs, or `failed` with a reason code — because `kodama step` ignores the
manifest when the exit status is not 0 and would replace the reason with
`exit_nonzero`. Exit 2 is misuse (a missing required environment variable the
script cannot even report), exit 3 an unwritable manifest. Exit zero alone never
completes a step: the entrypoint validates the declared outputs. Logs go to stderr
and never carry the token, the kubeconfig or secret values; kubectl's own text is
logged for people and decides nothing.

### `apply.sh`

| Variable | Meaning |
| --- | --- |
| `DEPLOY_MANIFESTS` | file or directory (`*.yaml`, `*.yml`, `*.json`, non-recursive); `@@IMAGE@@` is replaced by `DEPLOY_IMAGE` |
| `DEPLOY_REPOSITORY_URL`, `DEPLOY_COMMIT` | the candidate source (`KODAMA_INPUT_REPOSITORY_URL`, `KODAMA_INPUT_COMMIT` are fallbacks) |
| `DEPLOY_IMAGE_ARTIFACT_ID`, `DEPLOY_IMAGE` | the registered image artifact and its **digest-pinned** reference (`<repo>@sha256:<64 hex>`) |
| `DEPLOY_READY_TIMEOUT` | seconds to wait for readiness, default 120 |
| `DEPLOY_ADOPT_EXISTING` | `true` allows replacing same-named objects that no deployment created |
| `DEPLOY_ALLOWED_IMAGES` | optional comma list of digest-pinned auxiliary (sidecar) images allowed next to the verified image; also honoured by `lookup.sh` |

Sequence and recovery (DQ-08):

1. Validate inputs and parse the manifests locally. A malformed input, an
   unparsable manifest, a cluster-scoped kind or a nameless/`generateName` object
   fails before any call. So does the **image binding** (below).
2. Register the pre-apply deployment record (`POST …/deployments`). It is
   idempotent by the logical action: a retry after a lost response, a restart or a
   retry attempt receives the one original record and its annotation values. A
   namespace other than the recorded one fails (`manifest_namespace`) before any
   outcome is reported.
3. Cross-check the receipt's server-verified source commit, repository, image
   digest and artifact with the inputs (`provenance_mismatch` applies nothing),
   and bind the rendered manifests to the receipt's verified digest again.
4. Branch on the **record**: `succeeded` → verify the live objects by annotation
   and replay the outputs, never apply again; `failed` → `previously_failed`;
   `running` with `outcomeUnknown` → **never apply**: observe the recorded objects
   by annotation and settle by observation (all exist → wait for readiness and
   report `succeeded`/`rollout_timeout`; none → `failed apply_not_observed`;
   some → `failed apply_partial`); `running` → resume (see below); `pending` →
   first apply.
5. Observe the intended objects. An existing object that no deployment created is
   not overwritten (`object_conflict`) unless `DEPLOY_ADOPT_EXISTING=true`; an object
   of another deployment is taken over (the stage case: new deployment, same names).
6. Report `applying` with the intended identities **before the first external
   change**. An unacknowledged report stops the script (`callback_unavailable`):
   nothing was applied.
7. `kubectl apply` of the objects annotated with **every** annotation of the
   receipt (`kodama.optimi.io/deployment-id`, `…/action-key`,
   `…/configuration-digest` in the published fixtures), into the recorded
   namespace. A definite refusal (forbidden, invalid, admission) is reported
   `failed apply_rejected` with what exists. An indefinite failure is reported
   `unknown` (`apply_unknown`): the next attempt reconciles and never repeats it.
8. Observe the live objects and decide readiness **only** from their structured
   status (`READY_JQ` in `lib.sh`: Deployment/StatefulSet/DaemonSet generations and
   replica counts, Job completions, Pod Ready condition, PVC Bound, other kinds
   exist). Report `succeeded` with every object's observed `uid` and `ready: true`,
   or `failed rollout_timeout`. An unacknowledged final report is
   `outcome_unacknowledged`; the retry observes, re-applies the *same* objects
   idempotently and reports again.

**Image binding.** The outputs name the verified image, so the verified image is
what must run. In the rendered manifests every pod spec — any object holding
`containers`, `initContainers` or `ephemeralContainers` (Pod, Deployment,
StatefulSet, DaemonSet, ReplicaSet, Job, CronJob, and custom resources embedding
one) — may only use images whose **content digest** is the receipt's verified digest
(write `@@IMAGE@@`) or one listed in `DEPLOY_ALLOWED_IMAGES`; a tag or a reference
without a digest is never bound (`image_not_bound`). At least one container must run
the verified image (`image_not_deployed`). Nothing is registered or applied when the
binding fails. The binding is verified again on the **live** objects: a settled
action that is replayed, an unknown outcome being reconciled and the final
observation all fail `image_mismatch` (settling the record `failed`, never
`succeeded`) when a live workload runs another image **or when no live container
of the owned workloads runs the verified image any more** (the application
container was removed, or replaced by an allowed auxiliary image), and `lookup.sh`
reports the verdict `image_mismatch` in both cases. Presence of the verified digest
is tracked across all observed owned workloads (`hasv` per object). Annotations,
healthy auxiliary containers and readiness never vouch for the image.

Outputs (`completed`): `deployment_id`, `deployment_status` (`succeeded`),
`configuration_digest`, `image_digest`, `source_commit` — all from the **verified
receipt**, not from the inputs — `ready` (boolean, true only after the
observation above) and `object_count` (integer).

### `lookup.sh`

Read-only. `DEPLOY_ACTION_KEY` defaults to `$KODAMA_RUN_ID:$KODAMA_STEP_ID`; the
record interface refuses any key but the attempt's own. Prints one JSON document
on stdout and exits 0 whenever the observation completed:

```json
{"verdict":"applied_ready",
 "deployment":{"id":"dep_…","status":"succeeded","reason":"ready","outcomeUnknown":false,
               "namespace":"…","configurationDigest":"sha256:…","imageDigest":"sha256:…",
               "sourceCommit":"…","actionKey":"run_…:deploy"},
 "annotations":{"kodama.optimi.io/deployment-id":"dep_…","…":"…"},
 "observed":[{"apiVersion":"apps/v1","kind":"Deployment","namespace":"…","name":"…",
              "present":true,"owned":true,"holder":"dep_…","uid":"…","ready":true}]}
```

`owned` means every receipt annotation matches the live object; `holder` is the
deployment id the object claims (null when unmanaged). Verdicts: `unregistered`
(no record), `registered` (record, no intent), `failed`, `absent`, `partial`,
`foreign` (a recorded name is held by something that is not this deployment),
`image_mismatch` (an owned live workload runs an image other than the verified or
allowed ones, or none runs the verified image), `applied_not_ready`, `applied_ready`. `observed[]` also carries `rv`
(resourceVersion) and `bound` (the image binding holds). It also writes outputs `deployment_id`,
`deployment_status`, `configuration_digest`, `image_digest`, `source_commit`,
`verdict` and `ready` (`unregistered` writes only `verdict` and `ready`).

### `delete.sh`

Run as the cleanup step of a cleanup run bound to one deployment.
`DEPLOY_DEPLOYMENT_ID` is required.

**Authority first.** Being allowed to *read* a deployment is not being allowed to
clean it (the originating step may read it). Before any Kubernetes mutation the
script (1) requires the record to show a requested cleanup — `requested`, or
`failed`/`unknown` from an earlier run (`cleanup_not_requested` otherwise) — and
(2) obtains an **acknowledged** cleanup-specific authorization by publishing the
cleanup result `unknown` with reason `delete_started`, which only a cleanup run
bound to exactly this deployment may record (`callback_denied` for the originating
step or any unbound run). If the run dies afterwards the record keeps that visible
exception and the cleanup is retried. A cleanup already recorded `done` is only
replayed (nothing to delete; `cleanup_already_done` if recorded objects exist
again).

**Conditional delete.** Only the objects the record lists are candidates. Each is
handled alone: observe it, require every receipt annotation (and the recorded
`uid`), then delete it through the API with `preconditions` carrying the observed
`uid` **and** `resourceVersion` (`kubectl delete --raw`, API paths from the
server's discovery document). A replacement object, or the same object after its
annotations changed hands, makes the server answer Conflict and nothing is deleted;
the script observes again and decides on the new state: still ours (for example only
a label or status changed) — delete again with the fresh version; no longer ours —
left untouched and reported (`failed object_not_owned`, kept visible by the
record). Convergence is judged by observation, waiting only for the uid this run
deleted: absent objects count as deleted, so a repeat after a lost acknowledgment
reports the same completed cleanup (`done` lists every owned object).
`delete_incomplete` means an object this run deleted still exists; an unobservable
cluster is reported `unknown`, never "gone". Outputs: `deployment_id`,
`cleanup_state` (`done`), `deleted_count`.

### Reason codes (the failed manifest's `reason`)

| Code | Meaning |
| --- | --- |
| `input_invalid` | a required input is missing or malformed (commit, digest-pinned image, artifact id, …) |
| `manifest_invalid`, `manifest_cluster_scoped`, `manifest_namespace` | the manifests cannot be applied by this tool |
| `kubeconfig_missing` | no namespace-scoped kubeconfig is delivered |
| `callback_unavailable` | the record interface did not answer after retries |
| `callback_denied`, `callback_rejected`, `callback_conflict`, `deployment_not_found` | the record interface refused (authority/pin, validation, changed immutable inputs, unknown) |
| `outcome_unknown` | the record refused because the previous apply's outcome is unknown |
| `provenance_mismatch` | the verified receipt is not the source/image the step was given |
| `object_conflict` | an unmanaged object already has an intended name |
| `cluster_forbidden`, `cluster_unavailable` | the scoped credential was refused / the cluster did not answer |
| `apply_rejected`, `apply_unknown` | `kubectl apply` was refused / gave no definite answer |
| `apply_not_observed`, `apply_partial` | an unknown outcome was settled by observation |
| `rollout_timeout` | the objects were not all ready in time |
| `outcome_unacknowledged` | the work is done but its report was not acknowledged; retry |
| `previously_failed`, `recorded_state_diverged` | the action settled failed; the settled objects are absent/foreign/not ready |
| `image_not_bound`, `image_not_deployed` | a manifest runs an image that is not the verified (or an allowed) digest / none runs the verified image; nothing was registered or applied |
| `image_mismatch` | a live workload runs another image than the verified one, or no live container runs it any more (replay, reconciliation or final observation) |
| `cleanup_not_requested`, `cleanup_already_done` | `delete.sh` found no requested cleanup / a completed one whose objects exist again; nothing was deleted |
| `conflict_exhausted` | an object kept changing under the delete's preconditions; it was not deleted |
| `object_not_owned`, `delete_incomplete`, `nothing_recorded` | cleanup exceptions |
| `outputs_unknown` | `DEPLOY_OUTPUTS` names an output the script does not write |

## Limits (honest list)

* The scripts were qualified against the **real runtime handlers** of
  `internal/delivery/http` over a fixture service holding the real
  `delivery.Deployment` aggregate in memory, not against PostgreSQL or the real
  admission/context readers (T-035 qualified those with real stores; T-067 owns the
  joined real record/apply path).
* The readiness definition covers the common workload kinds; custom resources
  count as ready when they exist. A project needing more writes its own script.
* Image binding covers pod specs (containers arrays). A custom resource whose controller
  runs images named in other fields is not inspected; a project needing that writes
  its own script.
* `delete.sh` records `unknown`/`delete_started` before deleting, so a run that dies
  mid-cleanup leaves the record at the visible exception `unknown` (retried).
* The Kubernetes runtime's M-4 (Git ownership of the workspace) and M-1 (platform
  origin not exported to the command) are inherited, not fixed here.
