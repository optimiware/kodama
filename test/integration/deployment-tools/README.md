# test/integration/deployment-tools — acceptance of the deployment tools (T-065)

Resource prefix `t065`. Acceptance checks `deployment_tools_restricted`,
`deployment_annotation_lookup` and `deployment_tools_outputs` (DQ-07–DQ-10), run by
`Test_deployment_tools_restricted`, `Test_deployment_annotation_lookup` and
`Test_deployment_tools_outputs`. The image, scripts and output contract are in
[`images/deployment-tools`](../../../images/deployment-tools/README.md).

```bash
make check-go-integration PKGS=./test/integration/deployment-tools/...
# or, against an existing kind cluster:
KODAMA_TEST_KUBECONFIG=<kubeconfig> go test -race -count=1 ./test/integration/deployment-tools/...
```

Nothing skips: a missing cluster (`dev/.state/environments/<scope>/kubeconfig`
or `KODAMA_TEST_KUBECONFIG`), container provider or failed image build **fails**
the test. `KODAMA_TOOLS_IMAGE` names a prebuilt local image and skips the build.
The static contract tests need none of this and also run in the unit layer.

## What runs for real

| Real | Notes |
| --- | --- |
| The scripts and the image | `images/deployment-tools/build.sh` builds the image; `apply.sh`, `lookup.sh` and `delete.sh` run in it under Podman with the ordinary-step restrictions: `--user 65532`, `--cap-drop=ALL`, `no-new-privileges`, read-only root filesystem, a 64 MiB `/tmp`, bounded pids and memory, no runtime socket and nothing privileged. The first test probes `CapEff`/`CapBnd`, `NoNewPrivs`, the read-only root and the absence of any container runtime *inside* the container |
| Kubernetes | the task's kind cluster. Each test creates its own namespace under **restricted Pod Security** and a ServiceAccount whose Role covers only configmaps, services, deployments, statefulsets and jobs of that namespace; the script receives only that namespace-scoped kubeconfig. The image under test is also loaded into the node and run as the example Deployment's workload (`sleep`, non-root, read-only), so readiness is a real rollout |
| The record interface routes | the real `internal/delivery/http` runtime handlers (route table, DTOs, ETags, error mapping) on the real `httpapi` router over TLS with a test CA the script verifies |
| The deployment state machine | the real `delivery.Deployment` aggregate: `BeginApply`, `MarkOutcomeUnknown`, `Settle`, `RecordCleanup`, annotation values, action-key fingerprints |

| Fixture (the factory callback boundary) | Why |
| --- | --- |
| `callbacks_test.go` — a `RuntimeService` over an in-memory map | persistence, attempt authentication, the pinned-context reader, the artifact-provenance reader and the cleanup binding reader are T-035/T-077 modules qualified elsewhere; T-067 owns the real record/apply acknowledgment-loss join over PostgreSQL. The fixture is also where request loss and response loss are injected, and where `onApplying` inspects the cluster at the instant the intent is recorded |

## Acceptance map

| Test | What it proves |
| --- | --- |
| `Test_deployment_tools_restricted/no_privileged_runtime` | the scripts' container has no capability, cannot gain any, has a read-only root, runs as 65532 and contains no container runtime, socket or builder |
| `…/scoped_apply_and_delete` | a full apply and cleanup through the namespace-scoped credential: no object exists when the intent is recorded; the record ends `succeeded` with every object's live uid and `ready`; objects carry every receipt annotation; an unrelated object is untouched; the token never reaches a log; the cleanup run deletes exactly the owned objects, completes `deletedAt` while the record keeps its outcome and objects, and a repeat converges |
| `…/credential_is_namespace_scoped` | the credential is refused in other namespaces, for secrets, for namespaces, nodes and cluster roles; the script refuses another namespace, a cluster-scoped kind and a `generateName` object before reporting any outcome; a credential scoped to another namespace cannot deploy here (`cluster_forbidden`, nothing applied, no intent recorded) |
| `…/delete_never_touches_foreign_objects` | an object taken over by another deployment (annotation changed) and a same-named replacement that copied the annotations (different uid) are left alone; the exception stays visible on the record |
| `…/cleanup_authority_precedes_any_deletion` | the originating step's token can read its deployment but is not a cleaner: with no cleanup request it is refused (`cleanup_not_requested`) and with a request it is refused by the acknowledged cleanup authorization (`callback_denied`) — in both cases no Kubernetes delete is even attempted, every owned object keeps its uid and the record is unchanged; a bound run without a request is refused too; the bound run reports `unknown/delete_started` before it deletes and `done` after |
| `…/delete_is_conditional_on_the_observed_object` | a wrapper on `PATH` pauses each `kubectl delete --raw` between the script's observation and its delete (a deterministic interleaving; the delete itself is the real kubectl): a same-uid Deployment whose annotation changes hands and a ConfigMap replaced by a new object that copied the annotations both make the server answer Conflict (uid/resourceVersion preconditions); the script observes again, deletes neither and reports `object_not_owned` |
| `…/delete_retries_after_a_benign_change` | a label change of an owned object between observation and delete costs one conflict; the script observes again, finds it still owned and deletes it with the fresh resourceVersion; the cleanup completes |
| `…/cleanup_requires_the_deployment_binding` | a cleanup run bound to another deployment, and a step attempt of an unrelated action, cannot read or clean the deployment |
| `Test_deployment_annotation_lookup/lookup_is_read_only_and_annotation_bound` | `unregistered`, `registered`, `applied_ready`, `image_mismatch`, `applied_not_ready` (a rollout that cannot finish while the record says succeeded), `foreign`, `partial`, `absent` verdicts computed from live status and annotations; a lookup changes neither the record (GETs only) nor any `resourceVersion`; a live workload on another image is the verdict `image_mismatch` with `ready=false` |
| `…/lost_apply_acknowledgments_reconcile_without_duplicates` | (a) the intent is durable, its response is lost: nothing is applied, lookup shows the intent, the retry settles the same deployment with one set of objects; (b) the success report is lost *before* it is processed: lookup finds the applied objects by annotation while the record is running, the retry observes the same uids and settles; (c) lost *after* processing: the retry only verifies and reports nothing; (d) one lost response is absorbed by the script's own idempotent retry |
| `…/unknown_outcome_is_settled_by_observation_never_reapplied` | with an unknown outcome the script never reports `applying` and never applies (a live replica count that a re-apply would reset survives), settles succeeded when everything exists, failed `apply_not_observed` when nothing does, `apply_partial` for some; a failed action is never applied again |
| `…/unmanaged_objects_are_not_overwritten` | an object no deployment created is not replaced and no intent is recorded, unless the step explicitly adopts it |
| `…/lookup_is_bound_to_the_attempts_own_action` | another action key or a forged token is refused; no identity is disclosed |
| `Test_deployment_tools_outputs/documented_output_set_carries_verified_identities` | exactly the documented outputs, with the record's verified deployment id, configuration/image digests and source commit; the live objects carry the same configuration digest and the Deployment runs the digest-pinned image; stdout is empty; replay and output selection; the cleanup outputs |
| `…/mismatching_source_or_image_applies_nothing` | an unverified image or commit is refused at registration; malformed inputs call nothing; changed inputs for the same action are a conflict, not a second deployment; even an interface that returns another deployment's identities cannot make the script apply (`provenance_mismatch`) |
| `…/manifest_images_are_bound_to_the_verified_digest` | valid, registered inputs with manifests running another image (hard-coded other digest, a tag, a foreign sidecar, a foreign init container, a CronJob) are rejected `image_not_bound` with no verified-image output, no record call and nothing applied; manifests with no container of the verified image are `image_not_deployed`; an explicitly allowed digest-pinned sidecar is accepted and is refused without the allow-list |
| `…/live_images_are_verified_on_replay_and_reconciliation` | a settled deployment whose live workload now runs another image is not replayed as success (`image_mismatch`, no output, no apply); an unknown outcome is settled `failed image_mismatch`, never `succeeded` |
| `…/the_verified_image_must_stay_deployed` | with an allowed auxiliary container deployed next to the verified one, removing the application container or replacing its image with the allowed one leaves healthy, annotated, bound objects but no verified-image container: replay is `image_mismatch` with no outputs and no reports, lookup is `image_mismatch` with `ready=false`, and an unknown-outcome reconciliation settles `failed image_mismatch`; the untouched control is `applied_ready` and replays |
| `…/readiness_is_observed_not_printed` | `kubectl` prints `deployment.apps/… created` for a Deployment that can never run: the step still fails `rollout_timeout`, the record is `failed` with no object ready and no success report, and the failure is final for the action |
| `…/a_privileged_workload_never_reads_as_ready` | restricted Pod Security refuses the privileged Pods of an accepted Deployment; the step fails `rollout_timeout` |
| `Test_tools_*` (static) | the image pins match `dev/versions.lock.json`; base/snapshot/checksum/non-root pins; scripts parse and contain no insecure patterns; the token reaches `curl` on stdin; every route the scripts call is published by `deliveryhttp.RuntimeRoutes` and every published route is used; every reason code and output is documented; `evidence/build.json` records the scripts' current checksums |

## Limits

See the image README's limits. The record interface is a fixture over the real
handlers and aggregate; T-067 qualifies the PostgreSQL-backed join. The tests use
host networking because the kind API server and the TLS fixture listen on loopback;
that does not weaken the privilege restrictions asserted inside the container.
