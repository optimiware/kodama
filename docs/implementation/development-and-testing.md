# Local development and testing

## Repository tooling bootstrap

Go development needs Go **1.27.2 or later** and Make. `make lint test build` runs product formatting/vet, infrastructure-free race-enabled unit tests and the binary build. It does not test OpenForge, start containers or need provider credentials. Formatting and the repository checks are Go commands under `dev/cmd/`.

The default Go scope is `GO_CORE_PACKAGES` in Make: commands, internal modules, public API/client, contracts, reusable packages and test support. Linux-only image wrappers, development-stack programs, templates and integration/e2e drivers are checked explicitly by their owning tasks/checkpoints, not silently included through `./...` on every host. The main unit batch uses `make test-go-unit-json` from that same scope.

OpenForge commands additionally need Bun **1.4.2** and an explicit `make openforge-install` per worktree. Checks never install dependencies or rewrite the lockfile. `make check-openforge` qualifies harness changes, not Go product changes. The development tooling is Go (`dev/cmd/devctl`, `dev/cmd/repoctl`, `dev/internal/...`); `make check-devtools` qualifies it. Python remains only in release and staging scripts.

For agent execution, `bun ../openforge/src/cli.ts doctor` checks configured role/model availability. [Runtime details](harness.md#embedded-runtime-and-reporting) distinguish credential discovery from a live-provider invocation.

## Test layers

`KODAMA_ENV` selects adapters ([ADR 0009](../adr/0009-environments.md), [environments](environments.md)). Each layer runs in the cheapest environment that can prove it.

| Layer | Environment | Proves | Dependencies |
| --- | --- | --- | --- |
| Unit | `development` | Entity rules, use cases, retry/cancellation, authority decisions, budgets, request/response handling and wiring | Go; `memdb` and other in-memory adapters, the Process runtime, controlled time, temp files and httptest |
| Adapter integration | `test` | SQL/RLS/locking/rollback, real Git/S3/NATS/provider semantics, the contract suites on the real adapter | The `services` stack; isolated test data |
| Local checkpoint | `test` | The assembled executable and public commands, actual callbacks/workers/restart in real containers | The `services` stack and the Podman runtime; `kind` only where a Kubernetes target is the subject (delivery) |
| Kubernetes conformance | `production` | Restricted Jobs, preflight, NetworkPolicy and quota, the chart | The `cluster` stack |
| Staging, live provider | `production` | The stage cluster, actual model/wrapper, cloud KMS, restore, scale and hosted authority | Deferred, separately scheduled scope |

First finish the Go core and unit tests on `development`; then qualify M1's small CI-server path on `test` (T-026). Build M2 agentic behavior with deterministic model/tool responses; then qualify real local persistence/execution at T-038. Kubernetes conformance (T-089) and stage deployment (T-090) follow. Live agent smoke, extra wrappers, chassis, external/hosted adapters and production work follow after M3. Completed adapter implementations are preserved.

Test the module's public behavior through small consumer-owned interfaces. Add a seam for a concrete external effect, not a generic framework or interface per type. In-memory adapters prove policy and are held to their PostgreSQL twin by one `Contract` suite per port; they do not prove database/provider semantics. Keep the latter as named obligations on integration tasks before the milestone is accepted.

### Workload runtimes

Execution reaches workloads only through `execution.Runtime` ([ADR 0008](../adr/0008-runtime-seam.md)). Pick the backend by what the test proves:

| Backend | Proves | Needs |
| --- | --- | --- |
| `runtime/process` | Real lifecycle in milliseconds: start/stop races, output, failure, the real `kodama step` roundtrip on the host | Go; PostgreSQL for the roundtrip |
| `runtime/podman` | The step's pinned image, isolation, limits, durable identity and the entrypoint in a container | Podman (`KODAMA_REQUIRE_PODMAN=1` fails instead of skipping); with a Podman machine, `KODAMA_TEST_PODMAN_DIR` must be a shared path (default: the user cache directory on macOS) |
| `kube` | Staging truth: restricted Jobs, preflight, orphan cleanup | kind |

Wrap a real runtime in `runtimetest.Decorator` to record calls or inject a fault before (no effect) or after (lost response) a call. Wait by repeating `Observe`, `Logs` or `Stop`, never by sleeping. Persistence suites that script observations use `runtimetest.Manual`; it starts nothing. Every adapter runs `runtimetest.Contract`. Runs that a host fixture executes are admitted under the explicit `runtimetest.NoRequirements` policy, because the default policy refuses them on a backend without image or isolation guarantees.

```sh
go test -race ./internal/runtime/...                       # Process; Podman skips without an engine
KODAMA_REQUIRE_PODMAN=1 go test ./internal/runtime/podman/  # real Podman contract
go test ./test/integration/runtime/                         # entrypoint roundtrip: Process and Podman (PostgreSQL)
```

## Validation profiles

Scheduled tasks explicitly declare the [task check contract](../tasks/README.md#task-contract). Missing metadata is an error; it never selects `./...`.

| Profile | Merge evidence |
| --- | --- |
| `component` | Scoped format/vet and `go test -race -count=1 -tags unit` over owned/declared packages; every named unit acceptance must pass |
| `assembly` | Explicit package scope, required real services, completed package runs and named adapter/checkpoint acceptance |
| `docs` | Documentation/task-graph checks |
| `tooling` | Relevant tooling checks: OpenForge and the development tooling for their inputs |
| `full` | Explicit legacy/maintenance use only; never an implicit capability-task default |

`component` unit receipts qualify a merge. `assembly` unit runs are development feedback only and cannot satisfy real acceptance. A skipped acceptance test, incomplete package execution or same-named test in another package never passes. Service fixtures use `test/support/layer.SkipService`; the unit layer also blanks service environment variables. A unit task's acceptance names must therefore name actual unit checks, not inherited service tests.

```sh
make check-task TASK=T-080
make check-task TASK=T-026 LAYER=unit  # development feedback, not checkpoint acceptance
make check-task TASK=T-026            # real checkpoint scope, preflight required
```

Formatting/vet/test scope follows the task. SQL/module/composition edits do not silently widen it. Documentation/config checks run for relevant edits. Contract changes renew affected evidence; validation metadata renews checks/review without discarding implementation. Coordinator checks are independent of worker claims.

## First usable environment

Provisioning is needed only for the `test` and `production` environments; `development` needs none. Podman is the local default on every OS; `DEV_PROVIDER=docker` explicitly selects Docker (including CI). See [the development runbook](../../dev/README.md).

`DEV_STACK` selects what `dev-up` builds: `services` (the default for `test`: dependency containers and the Podman runtime on one network, no cluster) or `cluster` (adds a scoped kind cluster, for tasks whose subject is a Kubernetes target).

`DEV_SCOPE` selects **`dev/.state/environments/<scope>/` in the checkout running the check**. Task scopes are lowercased IDs with hyphens removed (`T-026` → `t026`); periodic main checks use `batch`.

```sh
make dev-up DEV_SCOPE=batch
make dev-preflight DEV_SCOPE=batch
make dev-down DEV_SCOPE=batch
```

`dev-up` creates (in the `cluster` stack) the scoped kind cluster/kubeconfig, pinned dependency containers and private `host-env.json` with host-reachable PostgreSQL admin, NATS, S3 and registry URLs, secrets-directory and kubectl paths. Pod URLs remain separate in `endpoints.json`. Loopback ports are allocated dynamically. Credentials are never committed or printed by preflight.

An assembly check declaring `check_resources: [services]`, or `[services, kind]` when a Kubernetes target is its subject, preflights its scope and loads those endpoints before Go tests. Preflight is read-only/bounded, checks kubectl/cluster readiness and required endpoint reachability, and reports the actual checked directory and recovery command. Missing infrastructure pauses the task without a fixer. Standalone `dev-up`, `dev-down` and diagnostics work while OpenForge's queue HALT exists.

## Parallel environments

Use one assigned branch/worktree and exclusive owned paths per task. Independent packages run concurrently; shared app/client/API files retain one ordered owner. `--workers` controls agent concurrency, separately from Go/service/kind capacity. Unit checks take only a Go lease; real checks declare service/kind needs. No global test database, namespace, bucket prefix or mutable Git ref is shared between tests.

OpenForge supervises commands with cross-process leases. For manual parallel checks use:

```sh
bun ../openforge/src/cli.ts resources --resources go -- go test -race -count=1 -tags unit ./internal/execution/...
```

Reserve `services` and, where needed, `kind` for actual integration. Preserve process cancellation/draining and lease ownership; see [capacity](harness.md#scoped-checks-and-capacity).

## Failure injection

Unit checks model duplicate delivery, unknown outcome, stale authority, cancellation and controlled-time retry. Integration checkpoints prove actual transaction rollback/lock ordering, concurrent writers, publication CAS, callback revocation and restart. Keep incomplete real semantics explicit; do not weaken checks or install test authority into the executable.

## Documentation/task validation

`make docs-check` validates links/anchors/fences, task metadata/index agreement, dependencies/cycles and ownership of unordered task pairs. Before merge, the current candidate needs passing scoped evidence and independent review. Every five merges and at queue drain, validate integrated main once; initially product-wide unit checks, then the bounded M1/M2 suites as their checkpoints land. There is no default age-triggered full suite.
