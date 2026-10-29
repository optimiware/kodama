---
id: T-123
milestone: production
status: deferred
depends_on: [T-117]
decision_blockers: []
owns: []
---
# T-123 — BuildKit builder service (deferred by O-30)

## Goal
Deliver the ADR 0010 §6 builder service after the O-30 milestone: a `role: builder` service running a qualified BuildKit mode (rootless, or `--oci-worker-no-process-sandbox` in its own process namespace, or a host-privileged mode on an exclusive disposable isolated worker), per-run client authentication, whole-process-tree and cache teardown.

## Contract
ADR 0010 §6, run-resources [builder services](../contracts/run-resources.md#builder-services), IT-16/IT-A12. Until this task is scheduled and qualified, no backend advertises `builder_service` or `builder_isolated_worker` and admission refuses `role: builder`.

## Before scheduling
Needs explicit owner scheduling, the qualified O-30 baseline (T-117) and a decision on the selected mode. Bring owned paths, check and acceptance up to date then.
