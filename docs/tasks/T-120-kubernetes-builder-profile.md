---
id: T-120
milestone: production
status: planned
depends_on: [T-111]
decision_blockers: []
owns: [deploy/profiles/run-builders/, docs/operations/run-builder-profile.md, docs/contracts/run-resources.md, docs/adr/0010-run-workspace-services-builders.md]
check: make check-go PKGS='./deploy/profiles/run-builders/...'
resources: [go]
acceptance: [unit_builder_profile_render, unit_builder_profile_denials]
---
# T-120 — Configure the Kubernetes builder/browser execution profile

## Goal
Configure Kubernetes to run the builders and sandboxed browser blocked by T-111. Planned by the owner; **do not execute as part of the current closeout**. Full build/runtime verification belongs to T-121.

## Contract
O-28/O-29, ADR 0010 and the T-111 report. Preserve trusted-engineer scope. The owner permits an explicitly reviewed relaxation of the builder/browser profile rather than treating the initial capability/LSM restrictions as immutable. Ordinary steps keep their restricted profile. No generic privileged-workload switch or automatic fallback.

## Deliverables
- Obtain the explicit target/installation handoff before changing live resources. Select dedicated workers or equivalent run-exclusive isolation, workload-role placement, namespaces and admission controls without weakening control-plane or unrelated tenant nodes.
- Diagnose and configure nested user/mount namespace support, subordinate mappings, AppArmor and seccomp. Prefer narrow node-installed profiles; permit additional user-namespaced capabilities or a less restrictive profile only when required, operator-selected and confined to the reviewed builder/browser boundary. A blanket shared-node relaxation, host engine socket, host namespaces or ordinary privileged tenant Pod is not authorized.
- Reconcile the owning policy/ADR before applying any exception the current contract forbids. Pin exact settings and versions, install profiles through installation management, and provide narrowly scoped PSA/admission exceptions where needed. Keep sandboxing unless an explicit subsequent owner decision changes that requirement.
- Supply reproducible installation/rendering, safe negative controls, inventory, rollback/drain instructions and the installation receipt. Retain run/tenant network separation, scoped credentials, expected-tip publication and verified teardown. Report configured, not qualified, until T-121 passes.

## Acceptance
Scoped render/denial tests and docs checks, reviewed target changes and retained configuration/preflight evidence. No build/browser support claim from rendered YAML alone. If existing deployment files outside `owns` are required, request a bounded ownership transfer first.

## Out of scope
Running this task now, broad production release, untrusted-code qualification, ordinary tenant privilege expansion and declaring P1 passed.
