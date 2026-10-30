---
id: T-134
milestone: production
status: planned
depends_on: [T-115, T-116]
decision_blockers: []
owns: [internal/app/runtime_select.go, internal/app/runtime_select_test.go, internal/app/runshared_local.go, internal/app/runshared_local_test.go]
check: make check-go PKGS='./internal/app/...' && make test-m1 test-m2 test-m3
resources: [go]
acceptance: [unit_app_local_run_resources_wired]
---
# T-134 — Wire the Podman and Process run-resource ports

## Goal
T-116 composed `run_shared` for Kubernetes only. After T-115 merges, compose the Podman (`test`) and Process (`development`) companion ports for lifecycle evidence, as T-116's handoff describes: a `SharedRuntime{Resources, Workloads, Policies}` in `newProcessRuntime` and `newPodmanRuntime`, registrar bridges reusing the Kubernetes error mapping, and `podman.PreparationCheckout`.

## Contract
O-30: Kubernetes is the delivery backend. Process and Podman advertise only what they enforce and lack the `network_policy` guarantee, so admission still refuses `run_shared` on them; their ports are exercised by the companion-port contract suite and explicit lifecycle tests, never through a relaxed admission. `production` keeps refusing both.

## Acceptance
Declared check with `Test_unit_app_local_run_resources_wired` passing: the ports are composed in `development`/`test`, admission still refuses `run_shared` there, and `production` refuses them.
