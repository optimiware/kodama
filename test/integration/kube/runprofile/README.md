# Run-resource profile qualification driver (T-111 / P1)

Bounded real-infrastructure probes for the rootless run-resource profile of
[ADR 0010](../../../../docs/adr/0010-run-workspace-services-builders.md). This
is **not** the legacy kind-only attempt suite in the parent directory: it
assumes no local kind container, imports no image into a node, derives no host
callback address and installs nothing. Its report is
[`docs/acceptance/run-resource-profile.md`](../../../../docs/acceptance/run-resource-profile.md).

## Delivery status (O-30)

T-111 delivered the driver and an honest, blocked record of one historical stage
run. [O-30](../../../../docs/decisions.md#revised-scope--2026-10-29-o-30) revised
what is qualified; T-124 aligns the driver with it so T-121 can run it unchanged
against the stage cluster. This package qualifies nothing by itself: the profile
is **not** qualified until T-121 records a passing
`Test_run_resource_profile_qualification` under the single integration lease.

## What is qualified

ko and Buildah run as **ordinary steps** in the `step` role of seam S3
([plan](../../../../docs/implementation/run-workspace-services-builders.md#integration-seams-frozen-by-t-122)),
on the regular stage workers. BuildKit and builder services (T-123) and browser
support are out of scope: they have no probes, pins, checks or verdict rows.

| Phase | Checks (all required) |
| --- | --- |
| inventory | Kubernetes/CRI/kernel/CNI/CSI/cgroup/LSM/seccomp/user-mapping, observed image digests |
| storage | `storage.rwop_sequential` (RWOP, sequential sharing across images/UIDs), `rwop_exclusive`, `idmapped_ownership`, `release_delete` (Delete reclaim) |
| registry | `fixture.registry`: TLS + htpasswd registry, in-run dependency/GOPROXY server, base image; per-namespace tenant network policies |
| ko | `step.role_shape.ko` (the ko Pod carries the same S3 step role as Buildah, checked on the live Pod and from inside), `build.ko` (dependency fetch, authenticated push, digest match) |
| buildah | `psa.builder_pod` (dry-run admission of the step role), `step.role_shape` (live Pod object and in-Pod view equal the role), `build.buildah_vfs` (build, push, digest match); optional `build.buildah_procmount_unmasked`; `build.buildah_overlay_fuse` is recorded `not_advertised` (the role has no devices) |
| postgres | `service.postgres`: readiness gate, write by one client, read by another, wrong password refused |
| network | `network.matrix`: the tenant model with live positive controls |
| teardown | UID/label-checked namespace deletion; PV and VolumeAttachment removal; verified cleanup gates the verdict |

### The step role (what ko and Buildah Pods get)

Every `run_shared` step, ko and Buildah alike, gets exactly seam S3 and
nothing wider (there is no restricted-compatible exception for ko): runAsUser/runAsGroup **1000** (the pinned
Buildah image's `build` user) and `runAsNonRoot`, `hostUsers: false`, all
capabilities dropped then **`SETUID` and `SETGID`** added, `allowPrivilegeEscalation: true`
(the setuid/file-capability `newuidmap`/`newgidmap` helpers), `seccompProfile`
Localhost `kodama/kodama-rootless-build-v1.json` and `appArmorProfile` Localhost
`kodama-rootless-build-v1` (installed by T-120, never `Unconfined`). One optional
variant adds `procMount: Unmasked`; it runs only if the default role fails inside
the build, and a pass there leaves `build.buildah_vfs` **blocked** with a policy
handoff, because the frozen role does not set it. There is no `SYS_ADMIN`, root or
`SYS_CHROOT` variant. Before building, the driver runs the role in an inspection
Pod and verifies the process really sees it (exact bounding set `0xc0`, escalation
permitted, an enforced seccomp filter and the AppArmor profile, a 65,536-ID mapping) and that
the image's `/etc/subuid` and `/etc/subgid` entries for `build` (stock
`build:1:999`, `build:1001:64535`) lie inside the Pod range. Only if they do not,
a Secret file is mounted over both files with that layout; the report records
which was used.

The Buildah probe: `buildah build --isolation=chroot --storage-driver=vfs` with
graph and run roots under `HOME` (`/kodama/home`, a bounded `emptyDir`), a
Containerfile `RUN` that fetches the in-run fixture, creates files owned by
several UIDs/GIDs inside the Pod range (0, 1000, 4242:4343, 60000:60001) and
`buildah push` with the run's authfile to the authenticated registry; the pushed
layer, the dependency marker and the registry's manifest digest against the
digest Buildah reported are verified.

### Network (tenant model)

Policies per namespace mirror seam S8: default deny, DNS, any Pod of the tenant
namespace may reach the service ports of any other (no run selector), and TCP
80/443 to `0.0.0.0/0` except private, link-local, CGNAT and loopback ranges.
Live controls are measured before the policy, the verdict after it:
same-tenant other-run clients reach the service (pod IP and Service DNAT) in both
directions; the foreign tenant namespace, the Kubernetes API and
`169.254.169.254` are denied; public HTTPS stays open, proven by a complete
TLS exchange with certificate verification (`curl https://1.1.1.1/` from a
client Pod of the ko image, reported as the `verify=` result), not by a plaintext
request to port 443. Every deny claim (foreign tenant, Kubernetes API, metadata)
uses a connect-only probe (`nc -z -w 4` in the pinned httpd image, whose BusyBox
provides it): an accepted connection is `open` however the peer then behaves, and
only a refused, unroutable or connection-establishment-timed-out connect is
`closed`. BusyBox `wget: download timed out` can follow an accepted connection, so
the HTTP probes used for the allow claims classify it `error`, never `closed`. Every probe reports its exit status; the driver classifies
status plus output as `open`, `closed` or `error`, and an unrecognised failure,
DNS failure, TLS failure or missing tool is an `error` that fails the check, never
`open`. A deny
claim about the API or metadata whose control is not open is **blocked** with a
narrowed claim, never passed; the foreign-tenant claim and every allow claim
without a live control fail; a claim that is never tested fails.

The verdict is `qualified` only when every required check passed **and**
cleanup was verified. A missing node prerequisite is `blocked` with an exact
installation handoff, never a pass.

## Running

Use the wrapper; it refuses missing inputs, runs the real test **without**
`-tags unit`, takes no second lease (the test takes it) and never prints
credentials:

```sh
export KODAMA_RUNPROFILE_KUBECONFIG=/path/to/kubeconfig   # explicit; ambient KUBECONFIG is ignored
export KODAMA_RUNPROFILE_TARGET=scaleway-stage            # name written into the report
export KODAMA_RUNPROFILE_STORAGE_CLASS=sbs-default        # CSI class that must give RWOP + idmapped mounts
export KODAMA_RUNPROFILE_REPORT_DIR=/path/to/retained     # receipts.jsonl, report.json, report.md
export FORGE_RESULTS=/path/to/results.json                # optional: go test -json outcomes (else a temp file is printed)
test/integration/kube/runprofile/verify.sh
```

It runs `go test -count=1 -timeout 90m -json -run Test_run_resource_profile_qualification ./test/integration/kube/runprofile/`
from the repository root and exits non-zero on any failure, and also when the
test was skipped or never reported a pass. `kubectl` (at least the server's
version) and the Go toolchain (to compile the static build helper) are required;
`KODAMA_RUNPROFILE_KUBECTL` and `KODAMA_RUNPROFILE_COMMIT` are optional.

Offline layer (`make check-go PKGS='./test/integration/kube/runprofile/...'`, `-tags unit`):
the real test is **skipped** by `layer.SkipService` and the `Test_unit_runprofile_*`
tests (guard, configuration, pins, verdicts, redaction, quiet output, dependency
archive, lease, step role shape, tenant network verdicts, verify wrapper) plus the
cleanup regressions run against controlled `kubectl` stubs, a stub `go` and
`httptest`, never a cluster. That skip is not qualification evidence. Without
`-tags unit` the real test never skips and fails on a missing target or any
blocked/unproven required row. `make check-go-integration` cannot express this
target (it provisions a stock-kind dev scope and clears `KODAMA_TEST_KUBECONFIG`).

## Driver safety boundaries

`Guard` allows only Namespace (task prefix), ResourceQuota, PersistentVolumeClaim,
Pod, Service, Secret and NetworkPolicy, all labelled with the task and run, and
refuses privileged/host/unconfined shapes, any container that does not set `drop: [ALL]`,
`runAsNonRoot: true` and a non-zero `runAsUser` (independently of added capabilities), every capability other than
`SETUID`/`SETGID` (so `SYS_ADMIN` and `SYS_CHROOT`), Localhost profiles other than the two
installed ones, `procMount: Unmasked` outside the complete step role shape (`hostUsers: false`,
both Localhost profiles, exactly `SETUID`+`SETGID`, escalation allowed), `hostUsers: true` and `hostPath`. Teardown deletes a namespace only
through the API with `uid`/`resourceVersion` preconditions (via `kubectl proxy`),
refuses on identity or label mismatch, and reports cleanup as verified only when
every PV, VolumeAttachment and namespace lookup succeeded and found nothing.
Namespaces are create-only (an existing name is refused, never adopted) and
their UID is taken from the creation response. The verdict is `qualified` only
when the full `RequiredChecks` inventory passed (missing or interrupted phases
are recorded as `not_run`/`run.interrupted`); deny claims need an open positive
control in the same run.
