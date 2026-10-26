# Rootless run-resource profile qualification (T-111 / P1)

**Status: P1 is not qualified; this is a historical, honestly blocked record.** T-111 closes as *driver and report delivery only*. The owner moved Kubernetes configuration to planned T-120 and the full real qualification (`Test_run_resource_profile_qualification` passing, retained under the single integration lease) to planned T-121; downstream T-113/T-114/T-115 are gated on T-121. Nothing in T-120 has been applied and no broader relaxation (node AppArmor profile, Pod Security level) has been made. On the one real run below, node prerequisites outside this task's authority were missing: run-scoped storage, ko, PostgreSQL and the outer CNI were proven on that run, Buildah, BuildKit and the browser sandbox were not, and nothing here substitutes unit, template or legacy kind evidence for them. In the offline `-tags unit` layer the real test is skipped by design; that skip is not qualification evidence, and the full acceptance is transferred intact to T-121.

**Historical receipts.** Everything below was produced by the driver at commit `8e60d6f2`, before the closeout driver-safety fixes (closed namespace-kind allowlist, precondition-based namespace deletion, and cleanup that cannot be reported verified after a failed lookup). The delivered driver differs from that candidate, so these receipts are not retargeted to it: they remain the exact record of the earlier candidate. In particular the run's "cleanup verified" came from the earlier name-only teardown; the observations behind it (namespaces absent, no PV or attachment left) were additionally confirmed afterwards with read-only `kubectl get`, but T-121 must obtain cleanup evidence from the delivered driver. The original history is archived by the coordinator outside Git.

Contract: [ADR 0010](../adr/0010-run-workspace-services-builders.md), [P1](../implementation/run-workspace-services-builders.md#p1--qualify-and-freeze-the-deployable-profile), [T-111](../tasks/T-111-rootless-profile-qualification.md). Driver and its README: [`test/integration/kube/runprofile/`](../../test/integration/kube/runprofile/README.md). Scope: trusted-engineer; this is bounded probing of a platform, not product runtime behavior and not a stage rollout approval.

## Target and exact inputs

| Item | Value |
| --- | --- |
| Target | Shared Scaleway stage cluster (4 workers), probed read-write **only** inside three task-owned namespaces `krp-t111-<run>`, `-na`, `-nb` (ResourceQuota-bounded, labelled `openforge.dev/task=t111`, `openforge.dev/run=<run>`), deleted by UID/label-checked teardown. No node, kubelet, containerd, sysctl, LSM, seccomp, CSI, CNI or global policy change; no foreign namespace reused; no privileged/hostPath/hostNetwork Pod; no Unconfined profile ever requested (the driver's `Guard` refuses it) |
| Kubernetes | server and kubectl v1.37.1, linux/amd64 |
| Nodes | Ubuntu 24.04.5 LTS, kernel `6.8.0-136-generic`, cgroup v2 (`cpuset cpu io memory hugetlb pids rdma misc`), containerd 2.3.6, kubelet systemd cgroup driver, `seccompDefault: false`, `max_user_namespaces=2147483647` |
| LSM / seccomp | Pods run `cri-containerd.apparmor.d (enforce)` and `Seccomp: 2` (RuntimeDefault); `kernel.apparmor_restrict_unprivileged_userns=1`; no `/dev/fuse`, no `/dev/net/tun` in a default Pod |
| User mapping | Pods with `hostUsers: false` get a distinct 65536-ID range each (e.g. `0 1182859264 65536`); no subordinate-ID files are needed for the single mapping |
| CNI | Cilium `v1.20.2` (tunnel routing, kube-proxy replacement) |
| CSI / class | Scaleway CSI `scaleway-csi:v0.3.6`, class `sbs-default`, `Delete` reclaim, `WaitForFirstConsumer`, ext4 mounted `idmapped` |
| Images (pinned by index digest, observed identical on the nodes) | ko `ghcr.io/ko-build/ko@sha256:ae515f52…0a82`; Buildah `quay.io/buildah/stable@sha256:0b88f54d…24dd` (1.43.5); BuildKit `docker.io/moby/buildkit@sha256:f68752a5…4f89` (rootless); registry `docker.io/library/registry@sha256:d4032d4c…c6c3`; httpd `docker.io/library/httpd@sha256:070da766…3893c`; PostgreSQL `docker.io/library/postgres@sha256:266d3c25…917e7b`; browser `docker.io/chromedp/headless-shell@sha256:f76fce29…c7c`. Full digests: [`pins.json`](../../test/integration/kube/runprofile/pins.json). No image is built or published by this task |

## Command and retained receipts

```sh
export KODAMA_RUNPROFILE_KUBECONFIG=/path/to/kubeconfig   # explicit; ambient KUBECONFIG ignored
export KODAMA_RUNPROFILE_TARGET=scaleway-stage-probe
export KODAMA_RUNPROFILE_STORAGE_CLASS=sbs-default
export KODAMA_RUNPROFILE_REPORT_DIR=/path/to/retained
go test -count=1 -timeout 90m -v -run Test_run_resource_profile_qualification ./test/integration/kube/runprofile/
```

The test takes the shared lease `/tmp/opencode/kodama-run-resources-integration.lock` itself. Without a target it **fails** (no skip). Final run (driver commit `8e60d6f2`): run id `7a310c`, 21:35:59–21:42:40 UTC, verdict `blocked`, test result FAIL, cleanup verified (three namespaces deleted with matching UID/labels, no PV, VolumeAttachment or namespace left). Receipts: 318 commands in `receipts.jsonl` (sha256 `ccee4dd5…b2ac`) and `report.json` (sha256 `fa3ee2d5…9cc8`) under `/tmp/opencode/t111/reports6/krp-t111-7a310c/` on the recording machine; they are not committed. Earlier runs (`ff1d19` aborted, `1474e2`, `1fc625`, `ae05dd`, `ea32e2`) exercised driver defects (missing namespace, tar trailer, idle port-forward, httpd applet) and are superseded.

### Limits of the historical verdicts (post-review driver repairs)

A later review found four defects in the driver that produced these receipts; they are repaired in the delivered driver and **not** retargeted onto the receipts below:

* **Network claims.** The old verdict logic let a deny claim stand without an open positive control for that destination: the Kubernetes API and public-address targets were not required to show a live control. The historical "outer CNI" and "builder egress" claims for the API and `1.1.1.1:443` therefore need per-receipt review of the recorded `control=` values (the run's evidence lists `control=open` for both, which supports that run, but the verdict logic itself did not require it). The delivered driver now reports `blocked` with narrowed claims when such a control is absent or closed. Full network default-deny qualification belongs to T-121.
* **Selected BuildKit mode.** The old matrix row was labelled no-process-sandbox regardless of what was tested, and a default-mode success would have been projected onto it. In this historical run both BuildKit modes were blocked, so no pass was projected; the row stays blocked. The delivered driver names the mode actually tested and does not claim a run-exclusive service or process-tree teardown without evidence.
* **Completeness.** The old verdict only examined recorded checks; the delivered driver requires the full inventory and records missing or interrupted phases. This run recorded every required check, so its blocked verdict is unaffected.
* **Namespace ownership.** The old driver created namespaces with `apply`, which could have adopted a same-named foreign namespace; the delivered driver is create-only and takes the UID from the creation response. This run created fresh randomly named namespaces, and I do not know of a collision, but its receipts predate the guarantee.

I do not claim any outcome of the historical target beyond what its receipts show; T-121 must re-run everything with the delivered driver.

### Check-target note

The legacy `make check-go-integration DEV_SCOPE=… DEV_STACK=cluster` cannot express this target and was **not** run for the real probe: its preflight provisions or loads the stock-kind `dev` scope and blanks `KODAMA_TEST_KUBECONFIG`, so success would describe different infrastructure. T-111's own check is the offline `make check-go PKGS='./test/integration/kube/runprofile/...'`. T-121 should run the command above directly under the lease (or adopt a check form that treats the `KODAMA_RUNPROFILE_*` variables as the scope).

## Support matrix (final run)

| Combination | Status | Evidence (check ids) |
| --- | --- | --- |
| RWOP run claim, sequential sharing across images/UIDs (httpd uid 10001 → postgres uid 20002 → httpd uid 30003, shared fsGroup 4000), idmapped ext4, Delete reclaim | **qualified** | `storage.rwop_sequential`, `rwop_exclusive` (second Pod `0/4 nodes… ReadWriteOncePod already in-use` while held, ran after delete), `idmapped_ownership` (mount `ext4 rw,relatime,idmapped`; later Pods saw true owner `10001:4000`, `20002:4000` under three distinct mappings), `release_delete` (VolumeAttachment gone after Pod deletion, PV removed after claim deletion) |
| ko build + push to authenticated TLS registry with Go dependency via in-run GOPROXY | **qualified** | `build.ko`: digest `sha256:99c30a3a…5341` identical in ko output and registry; executable `ko-app/koapp` uid 0; module `example.com/dep` cached from the in-run proxy; Pod restricted-compatible, no added capabilities; anonymous registry access returned 401 (`fixture.registry`) |
| Explicit registry + dependency egress for builder role | **qualified** | `network.builder_egress`: registry and deps open; live in-run `www`, Kubernetes API, `1.1.1.1:443` closed |
| Outer CNI under default deny | **qualified** | `network.matrix`: positive controls open for same-run, other-run, foreign namespace (pod IP and Service), API and public; after policy only same-run (pod IP and Service DNAT) open; other-run and foreign-namespace clients denied toward the run |
| PostgreSQL 17 non-root (uid 70, user namespace, read-only root, bounded emptyDir) | **qualified** | `service.postgres`: readiness gate, write by one client Pod, read by another, wrong password refused |
| Bounded `/dev/shm` (64Mi memory tmpfs) | qualified in isolation | `service.browser` evidence: `tmpfs 65536`, over-fill refused; the combined browser row is blocked |
| Buildah VFS (`RUN`, multi-UID, dependency egress) | **blocked** | `build.buildah_vfs`, `psa.builder_pod` |
| BuildKit rootless default and `--oci-worker-no-process-sandbox` | **blocked** | `build.buildkit_default`, `build.buildkit_selected` |
| Browser with its sandbox (never `--no-sandbox`) | **blocked** | `service.browser` |
| Buildah overlay/FUSE | not advertised | `build.buildah_overlay_fuse`: no `/dev/fuse`; nothing exercised |
| Nested NAT networking (slirp4netns/pasta/CNI-in-Pod) | not advertised | no `/dev/net/tun`; `network.nested` only proves a nested user+net namespace has loopback and no route while the outer Pod keeps exactly its run-scoped access |
| BuildKit run-exclusive service teardown, privileged disposable worker | not qualified | not reachable until a BuildKit mode runs; host privilege was not selected or provisioned |

## Why the blocked rows are blocked (evidence)

* Buildah, tried in three permitted Pod shapes (non-root without caps; non-root with `SETUID,SETGID,SYS_ADMIN` in the bounding set; uid 0 in a user namespace with `SYS_CHROOT`), all `RuntimeDefault`, none unconfined: every shape fails inside `buildah build` while creating the nested user/mount namespace. Diagnosis from the same Pod: `unshare -U` succeeds, `unshare -Urm` fails (`write failed /proc/self/uid_map: Operation not permitted` / `can't mount none on / … Permission denied`) under `cri-containerd.apparmor.d (enforce)` with `apparmor_restrict_unprivileged_userns=1`. The runtime's default seccomp profile only allows nested user namespaces when `SYS_ADMIN` is in the bounding set, and Pod Security **baseline and restricted both warn** on that shape (`psa.builder_pod`); the control shape without added capabilities is clean (that is why ko, PostgreSQL and the servers run under restricted-compatible settings).
* BuildKit: `buildctl-daemonless` cannot reach `buildkitd.sock` in either mode because rootlesskit cannot create its mount namespace (same denial).
* Chromium with its sandbox: `Zygote process exited prematurely … (namespace sandbox)` with `SYS_ADMIN` in the bounding set; `No usable sandbox` without it. The driver never passes `--no-sandbox`.

## Installation handoffs (T-114 / T-116; not performed here)

1. **Builder/browser node security profile.** Provide a node-installed, builder-only AppArmor profile (referenced as `appArmorProfile: Localhost`) or a dedicated builder node pool/runtime class that permits unprivileged user namespaces with mounts for those Pods only, or a different node OS/kernel setting. This is a node/installation change requiring the installation owner; relaxing the shared default, `Unconfined` profiles and privileged Pods were deliberately not used. Re-run this qualification after it exists.
2. **Pod Security.** A namespace or exemption whose level admits user-namespaced Pods with `SYS_ADMIN` (and `SETUID`/`SETGID` for Buildah) for builder and browser Pods; the stage tenant namespace enforces `restricted`. Keep services, ko builds and steps restricted.
3. **Decision points once 1–2 exist:** the selected BuildKit mode (default vs no-process-sandbox in a run-exclusive service, with process-tree teardown evidence), whether Buildah needs `fsGroup`/subordinate-ID files beyond the image defaults, and whether an exclusive disposable privileged worker is wanted (requires its own provisioning/deletion proof before being advertised).
4. **Fixture settings frozen so far (P4/P6 may rely on them):** `hostUsers: false` on every run Pod; RuntimeDefault seccomp; `drop: [ALL]`, non-root UID, `allowPrivilegeEscalation: false`; RWOP PVC from a CSI class with ext4 idmapped mounts and `Delete` reclaim, fsGroup shared across steps; bounded memory `/dev/shm`; services/builders reached through run-scoped label NetworkPolicies with a DNS allow and explicit registry/dependency egress; per-run registry credential delivered as a Docker config Secret mounted read-only, TLS CA mounted read-only; destinations in the final run were only the in-namespace registry and dependency server.

## Acceptance coverage

| Acceptance (IT-14, IT-15, IT-16 and P1 exit) | Result |
| --- | --- |
| `run_resource_profile_qualification` | **not passed** — blocked by the handoffs above; the test fails by design in a non-unit run, is skipped (not passed) under `-tags unit`, and moves to T-121 |
| IT-14 nested/outer default deny with real CNI | passed for pod IP and Service DNAT, same-run vs other-run vs foreign namespace vs API vs public, with live controls; nested NAT networking not advertised |
| IT-15 ko / Buildah real build and push | ko passed; Buildah blocked |
| IT-16 selected BuildKit mode | blocked |
| P1: storage/idmap/release/reclaim | passed |
| P1: PostgreSQL / browser | PostgreSQL passed; browser sandbox blocked |
| Offline coverage of the delivered driver (T-111 acceptances `unit_runprofile_*`) | guard refusals and closed object kinds, fail-closed configuration, digest pins, verdict logic that never qualifies blocked or unproven profiles, receipt redaction, quiet output, dependency archive trailer, lease exclusivity; plus cleanup regressions with controlled `kubectl` stubs: failed lookups are never verified, foreign replacements are never deleted, no name-only delete fallback |
