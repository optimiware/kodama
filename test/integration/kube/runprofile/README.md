# Run-resource profile qualification driver (T-111 / P1)

Bounded real-infrastructure probes for the rootless run-resource profile of
[ADR 0010](../../../../docs/adr/0010-run-workspace-services-builders.md). This
is **not** the legacy kind-only attempt suite in the parent directory: it
assumes no local kind container, imports no image into a node, derives no host
callback address and installs nothing. Its report is
[`docs/acceptance/run-resource-profile.md`](../../../../docs/acceptance/run-resource-profile.md).

## Delivery status

T-111 delivers this **driver**, its offline safety/verdict tests and an honest,
historical record of one blocked stage run. It does **not** deliver a qualified
profile. Kubernetes node/Pod Security configuration is planned as T-120 and the
full real qualification (this package's `Test_run_resource_profile_qualification`
passing, retained under the single integration lease) as T-121; downstream
workspace/Kubernetes/Podman tasks are gated on T-121.

## What it does

Against one explicitly named target it creates three task-owned namespaces
(`krp-t111-<random>`, `…-na`, `…-nb`; ResourceQuota-bounded, labelled
`openforge.dev/task=t111` and `openforge.dev/run=<random>`), runs the probes
below with only digest-pinned public images (`pins.json`), retains receipts for
every command (`receipts.jsonl`, secrets redacted, stdin only hashed) and
writes `report.json`/`report.md`. Every manifest passes `Guard` first: no
privileged Pods, host namespaces, `hostPath`, unconfined seccomp/AppArmor,
host-user mapping, node pinning or capability beyond the user-namespace set.

| Phase | Checks |
| --- | --- |
| inventory | Kubernetes/CRI/kernel/CNI/CSI/cgroup/LSM/seccomp/user-mapping, observed image digests |
| storage | `storage.rwop_sequential`, `rwop_exclusive`, `idmapped_ownership`, `release_delete` |
| registry | `fixture.registry`: TLS + htpasswd registry, dependency/GOPROXY server, base image |
| ko | `build.ko`, `network.builder_egress` |
| buildah | `build.buildah_vfs`, `build.buildah_overlay_fuse`, `psa.builder_pod` |
| buildkit | `build.buildkit_default`, `build.buildkit_selected` |
| postgres / browser | `service.postgres`, `service.browser` |
| network | `network.matrix` (live controls, then default deny), `network.nested` |
| teardown | UID/label-checked namespace deletion; PV and VolumeAttachment removal |

The verdict is `qualified` only when every required check passed **and**
cleanup was verified. A missing node prerequisite is `blocked` with an exact
installation handoff, never a pass.

## Running

```sh
export KODAMA_RUNPROFILE_KUBECONFIG=/path/to/kubeconfig   # explicit; ambient KUBECONFIG is ignored
export KODAMA_RUNPROFILE_TARGET=scaleway-stage-probe      # name written into the report
export KODAMA_RUNPROFILE_STORAGE_CLASS=sbs-default        # CSI class that must give RWOP + idmapped mounts
export KODAMA_RUNPROFILE_REPORT_DIR=/path/to/retained
go test -count=1 -timeout 90m -v -run Test_run_resource_profile_qualification ./test/integration/kube/runprofile/
```

Offline layer (`make check-go PKGS='./test/integration/kube/runprofile/...'`, `-tags unit`):
the real test is **skipped** by `layer.SkipService` and the `Test_unit_runprofile_*`
tests (guard, configuration, pins, verdicts, redaction, quiet output, dependency
archive, lease) plus the cleanup regressions run against controlled `kubectl`
stubs and `httptest`, never a cluster. That skip is not qualification evidence.
Without `-tags unit` the real test never skips and fails on a missing target or
any blocked/unproven required row.

The test takes the shared manual lease `/tmp/opencode/kodama-run-resources-integration.lock`
itself (do not wrap it in `flock`). It fails, never skips, without a target.
Required tools: `kubectl` (a version at least the server's) and the Go
toolchain (to compile the static build helper).

`make check-go-integration DEV_SCOPE=… DEV_STACK=cluster PKGS=…` cannot express
this target: its preflight provisions/loads a stock-kind dev scope and clears
`KODAMA_TEST_KUBECONFIG`. T-121 must run the command above directly under the
lease; see the report.

## Driver safety boundaries

`Guard` allows only Namespace (task prefix), ResourceQuota, PersistentVolumeClaim,
Pod, Service, Secret and NetworkPolicy, all labelled with the task and run, and
refuses privileged/host/unconfined shapes. Teardown deletes a namespace only
through the API with `uid`/`resourceVersion` preconditions (via `kubectl proxy`),
refuses on identity or label mismatch, and reports cleanup as verified only when
every PV, VolumeAttachment and namespace lookup succeeded and found nothing.
Namespaces are create-only (an existing name is refused, never adopted) and
their UID is taken from the creation response. The verdict is `qualified` only
when the full `RequiredChecks` inventory passed (missing or interrupted phases
are recorded as `not_run`/`run.interrupted`); deny claims need an open positive
control in the same run; the BuildKit row names the mode actually tested.
