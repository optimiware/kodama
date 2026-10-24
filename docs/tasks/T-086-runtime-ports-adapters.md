---
id: T-086
milestone: M1
status: done
depends_on: [T-001, T-016, T-019, T-020, T-021, T-022, T-023, T-030, T-031, T-032, T-034, T-035, T-040, T-062, T-063, T-064, T-070, T-076, T-078, T-079]
decision_blockers: []
owns: [internal/execution/, internal/kube/, internal/runtime/, internal/runtimeauth/action.go, test/support/podman/, test/integration/runtime/, internal/agents/adapters/, internal/agents/integration/, internal/gateway/adapters/runtime.go, internal/gateway/gatewaytest/, internal/gates/gatestest/, internal/gates/integration/, internal/gates/postgres/lock_order_test.go, internal/delivery/deliverytest/, internal/qa/qatest/, internal/logs/adapters/execution.go, internal/logs/logstest/, internal/results/postgres/authorization.go, internal/entrypoint/job_checkout_test.go, internal/app/migrate.go, internal/app/migrate_test.go, test/integration/kube/, test/integration/logs/, test/integration/results/, test/integration/harness-opencode/, test/integration/execution-composition/]
check: make check-go PKGS="./internal/execution/... ./internal/kube/... ./internal/runtime/..."
resources: [go]
acceptance: [runtime_admission_binding, runtime_binding_gates_actions, dispatch_binds_the_workload_instance, observing_running_workloads, remove_settled, attempt_spec, runtime_surface, process_contract, stop_while_starting, output_is_drained, entrypoint_program, admission_credential]
---
# T-086 — Runtime ports and adapters

## Goal
Give execution one backend-neutral runtime seam, so lifecycle tests run real workloads in milliseconds, Podman is the local end-to-end target and Kubernetes stays the staging truth ([ADR 0008](../adr/0008-runtime-seam.md)).

## Contract
`execution.Runtime` is execution's only workload dependency: `Start`, `Observe`, `Logs`, `Stop`, `Remove`, `Capabilities`. Execution names attempts' workloads (`AttemptSpec`, `Workload`, `WorkloadName`, `WorkloadUID`), never Jobs, containers or processes. Persistence columns and public HTTP fields keep their names.

Admission applies the installation's requirement policy, refuses a runtime lacking a required guarantee and pins `Snapshot.Runtime` with the digest; claims recheck the current policy, and no other backend starts, stops or removes a bound run, and a run admitted without a runtime is never dispatched (PX-15). Redrive applies the same launch check as a claim. Dispatch binds the reported instance UID; a dispatch-confirmed workload reported absent fails as lost; `Stop` confirms only once no launch can still arrive; settled workloads are removed with durable, retried bookkeeping (`T086_001`, a new final migration contribution that the application manifest lists after the runtime authority schema, so deployed ledgers upgrade in order) (PX-16).

Adapters act only on verified instances: `kube.Runtime` reads committed attempts from the durable store and requires `kube.NewPlanner`; `podman.Runtime` keeps durable records and acts by container ID; `process.Runtime` is test-only and runs explicit programs. Both non-Kubernetes runtimes run the real `kodama step` through `bootstrap` (credential persisted before registration) and implement `runtimeauth.JobObserver`; one instance stands for Job and Pod. Source checkout outside Kubernetes is refused until supported.

## Deliverables
The port and its use cases in `internal/execution`; `kube.Runtime` replacing `kube.Executor`; `internal/runtime/{process,podman,bootstrap,runtimetest}`; the recording, fault-injecting `runtimetest.Decorator` and `runtimetest.Manual` replacing `executiontest.FakeExecutor`; `runtimetest.Contract`; the backend-parameterized entrypoint roundtrip in `test/integration/runtime`.

## Acceptance
Unit: the named `acceptance` tests in the checked packages. Real services, run explicitly: `Test_process_runtime_lifecycle` and `Test_execution_migrations` (PostgreSQL), `Test_podman_contract`, `Test_podman_durable_identity`, `Test_podman_job_observer` (Podman) and `Test_runtime_roundtrip` (PostgreSQL and Podman) cover PX-A10 and PX-A11.

## Ordering
Lands before T-077, T-080 to T-084 and the `internal/app` chain, whose owned paths it renames into runtime terms; root wiring of the runtime stays with T-071.

## Out of scope
Root composition, binding `Runtime.Logs` into the logs module, source checkout for Podman and Process, and qualification of the Kubernetes changes on kind (T-089).
