---
id: T-124
milestone: production
status: planned
depends_on: [T-111, T-122]
decision_blockers: []
owns: [test/integration/kube/runprofile/]
check: make check-go PKGS='./test/integration/kube/runprofile/...'
resources: [go]
acceptance: [unit_runprofile_guard, unit_runprofile_configuration, unit_runprofile_pins, unit_runprofile_verdicts, unit_runprofile_redaction, unit_runprofile_quiet_output, unit_runprofile_dependency_archive, unit_runprofile_lease, unit_runprofile_step_role_shape, unit_runprofile_tenant_network_verdicts, unit_runprofile_verify_wrapper]
---
# T-124 — Align the profile qualification driver with O-30

## Goal
Make the T-111 driver qualify exactly the O-30 profile so T-121 can run it unchanged against stage.

## Contract
O-30, run-resources contract, frozen seam S3. Keep the driver's guard, create-only namespaces, UID-preconditioned cleanup, redaction, lease and fail-never-skip behavior.

## Deliverables
- Remove the browser phase and BuildKit probes and their pins; the required inventory becomes storage, ko, Buildah, PostgreSQL, network and teardown.
- Builder Pods use exactly the `step` role of seam S3: non-root UID 1000 (the Buildah image's `build` user), `hostUsers: false`, all capabilities dropped plus `SETUID`/`SETGID`, `allowPrivilegeEscalation: true`, Localhost seccomp `kodama/kodama-rootless-build-v1.json` and AppArmor `kodama-rootless-build-v1`; an optional `procMount: Unmasked` variant. Remove the `SYS_ADMIN` and root variants; the guard refuses them.
- Buildah: `RUN`, multiple UID/GID ownership inside the 65,536 Pod range, chroot isolation, VFS storage under `HOME`, dependency fetch and authenticated push.
- Network verdicts for the tenant model with live positive controls: same-tenant other-run reachable, foreign tenant namespace, Kubernetes API and metadata denied, public HTTPS open.
- `verify.sh`: explicit-target wrapper that runs the real test without `-tags unit`, captures JSON outcomes into `FORGE_RESULTS`, refuses missing inputs and takes no second lease.

## Acceptance
Declared offline check with every named test passing. No real-target claim: T-121 owns the run.
