---
status: accepted
---

# One runtime seam: Process for tests, Podman locally, Kubernetes for staging

Execution needs fast tests of the real workload lifecycle, a local end-to-end target without a cluster, and Kubernetes as the staging and production truth ([ADR 0001](0001-native-kubernetes-jobs.md)). Execution depends on one backend-neutral port, `execution.Runtime`, with six methods: `Start`, `Observe`, `Logs`, `Stop`, `Remove` and `Capabilities`. Execution has no notion of Jobs, containers or processes. A new backend implements the port; it never adds a branch to the domain.

Three adapters implement the port:

- **Process** runs host processes and is used only in tests. It runs an explicit program chosen by the test: the declared command, or the real `kodama step` entrypoint from a host binary. It claims no guarantee and never image parity.
- **Podman** runs the step's pinned image with a mounted Linux `kodama step`. It is the local end-to-end backend. It enforces image, isolation, resource limits and crash recovery, but not tenant network policy or quotas.
- **Kubernetes** runs restricted Jobs and is the staging and production truth. It enforces the image, isolation, resource-limit, network-policy, resource-quota and crash-recovery guarantees. O-25 below distinguishes these from optional launch-history proof.

Backends declare guarantees (`Capabilities`) instead of sharing a lowest common denominator. An installation's requirement policy derives what each step needs. Admission refuses a run the selected runtime cannot satisfy, then pins the backend and requirements in the run snapshot and digest. A run is never started, stopped or removed by another backend, because a backend reports a workload it never ran as absent. Every launch rechecks the current policy.

## Consequences

Lifecycle tests run the Process runtime and a thin recording, fault-injecting decorator, not a parallel full fake. Persistence tests that script observations use a minimal `Manual` runtime behind the same decorator. One contract suite qualifies every adapter against real backends.

Runtime identity is durable. Dispatch binds the instance UID the runtime reports. A dispatch-confirmed workload reported absent is lost and fails as infrastructure while keeping its launch evidence. Removal after settlement is recorded separately and retried, including for terminal runs. Adapters act only on verified instances: the Kubernetes runtime reads committed attempts from the store, and Podman reads durable records and acts by container ID.

## Launch evidence — O-25

**Owner-approved amendment, 2026-10-22; implementation: T-102.** A backend explicitly declares whether stopping a fresh workload can prove it never started. The shared contract requires `LaunchNotStarted` for that case only from a backend advertising the capability. This optional proof does not weaken the mandatory stop/fencing contract or change container/admission requirements.

Kubernetes does not advertise the capability: absence of its Secret, Job and Pods cannot distinguish no create request from an uncertain issued create whose record was lost. It reports `LaunchUnknown` without affirmative evidence; existing evidence may still establish `LaunchNotStarted` or `LaunchStarted`. Process/Podman retain stronger proof within their tested contract. The non-advertising path is tested, not skipped.

Every backend still rejects a subsequent start after stop/removal, protects foreign instances, and reports `Stopped` only when no workload or pending launch can run. Unknown launch evidence does not authorize automatic retry or release of an uncertain effect's writer barrier; the owning lifecycle must reconcile it. Preserve lost-Secret and in-flight-create regression coverage. Additional durable fence objects were considered and not selected.

Outside Kubernetes, one workload instance stands for both the Job and the Pod in the runtime admission identity. The wire contract is unchanged. The Podman runtime does not yet issue source-checkout credentials, so it runs only workspace-free attempts until checkout support is added.

[ADR 0010](0010-run-workspace-services-builders.md) proposes a companion run-resource seam and a builder-capable execution profile. This accepted port and the current restricted baseline remain in force until that proposal is accepted.
