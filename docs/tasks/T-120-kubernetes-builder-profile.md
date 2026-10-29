---
id: T-120
milestone: production
status: done
depends_on: [T-111, T-122]
decision_blockers: []
owns: [deploy/profiles/run-builders/, docs/operations/run-builder-profile.md]
check: make check-go PKGS='./deploy/profiles/run-builders/...'
resources: [go]
acceptance: [unit_builder_profile_render, unit_builder_profile_denials, unit_builder_profile_seccomp_allows_userns_without_sys_admin, unit_builder_profile_apparmor_rules]
---
# T-120 — Configure the Kubernetes builder step profile

## Goal
Provide the installable node profiles that let ordinary `run_shared` step Pods run Buildah rootless on the regular Scaleway stage workers within the O-30 ceiling, which the T-111 probe showed `RuntimeDefault` and the default AppArmor profile deny.

## Contract
O-30 and seam S3. Non-root, `hostUsers: false`, only `SETUID`/`SETGID`, no `SYS_ADMIN`, no privileged tenant Pod, no host namespaces/paths/engine socket. The profiles are installed by installation management (a privileged installer DaemonSet in an installation-owned namespace is installation management, not tenant execution). `attempt_private` workloads and the shared defaults are unchanged.

## Deliverables
- `seccomp/kodama-rootless-build-v1.json`: the container runtime default plus unconditional `clone`/`clone3`/`unshare`/`setns`/`mount`/`umount2`/`pivot_root`/`mount_setattr`/`open_tree`/`move_mount`/`fsopen`/`fsmount`/`fsconfig`/`keyctl` needed by user-namespaced builders, still denying kernel-module, reboot, kexec, BPF and similar host operations.
- `apparmor/kodama-rootless-build-v1`: permits `userns` creation and the mount/remount/pivot_root operations inside the user namespace; keeps the runtime default's denials of host `/proc`/`/sys` writes.
- Installer manifests (`installer/`): a digest-pinned DaemonSet that copies the seccomp profile to `/var/lib/kubelet/seccomp/kodama/` and loads the AppArmor profile, reports installed versions, and an uninstall procedure. Go renderer/validator with tests.
- `docs/operations/run-builder-profile.md`: install, verify, roll back; the Pod fields that reference the profiles; what stays forbidden.

## Acceptance
Declared check with every named test passing. Installing on stage and the real qualification belong to T-121; rendered files are not support evidence.

## Out of scope
Chart integration (T-116), the qualification run (T-121), BuildKit (T-123).

## Completion evidence
Implemented on `task/T-120-kubernetes-builder-profile` (`0a4ce3c0`, review repairs `0f1489e1`, `98583caf`). Independent review (`ses_e768fb712ffeGtaq3uzvNoPALI`) approved the first candidate with minor findings, then requested changes on the repair (installer readiness never restored after a transient inspection failure) and approved `98583caf`. Coordinator checks on the final candidate: `make check-go PKGS='./deploy/profiles/run-builders/...'` exit 0, 8/8 tests PASS including the four pinned acceptances; `make docs-check` PASS. Configured, not qualified: nothing was installed on any node; T-121 installs and qualifies on stage. Squashed to one commit; nothing pushed.
