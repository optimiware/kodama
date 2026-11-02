# Local development and testing

## Repository tooling bootstrap

Go development needs Go **1.27.2 or later** and Make. `make lint test build` runs product formatting/vet, infrastructure-free race-enabled unit tests and the binary build. It does not start containers or need provider credentials; no Bun, OpenForge checkout or sibling directory is involved. Formatting and the repository checks are Go commands under `dev/cmd/`.

The default Go scope is `GO_CORE_PACKAGES` in Make: commands, internal modules, public API/client, contracts, reusable packages and test support. Linux-only image wrappers, development-stack programs, templates and integration/e2e drivers are checked explicitly by their owning tasks/checkpoints, not silently included through `./...` on every host. The main unit batch uses `make test-go-unit-json` from that same scope.

The development tooling is Go (`dev/cmd/devctl`, `dev/cmd/repoctl`, `dev/internal/...`); `make check-devtools` qualifies it. Python remains only in release and staging scripts. `make validate-tasks` validates the task records and `make check-task TASK=T-NNN` runs a task's declared check (see [Task checks](#task-checks-and-validation)).

OpenForge is an optional, separately installed external harness ([harness](harness.md)). Nothing here depends on it, and this repository neither installs, lints nor tests it.

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
| `docs` | `make docs-check` and, when task records changed, `make validate-tasks` |
| `tooling` | `make check-devtools` when `dev/` changed |
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

An assembly check declaring `check_resources: [services]`, or `[services, kind]` when a Kubernetes target is its subject, preflights its scope and loads those endpoints before Go tests. Preflight is read-only/bounded, checks kubectl/cluster readiness and required endpoint reachability, and reports the actual checked directory and recovery command. Missing infrastructure pauses the task without a fixer. Standalone `dev-up`, `dev-down` and diagnostics are independent of any task runner.

## Parallel environments

Use one assigned branch/worktree and exclusive owned paths per task. Independent packages run concurrently; shared app/client/API files retain one ordered owner. `--workers` controls agent concurrency, separately from Go/service/kind capacity. Unit checks take only a Go lease; real checks declare service/kind needs. No global test database, namespace, bucket prefix or mutable Git ref is shared between tests.

Give each concurrent session its own `DEV_SCOPE` (`make check-task` defaults it to the task prefix, for example `t126`) and run at most one integration job per scope. Reserve the service and `kind` stacks for actual integration: the coordinator serializes jobs that share a cluster, for example with `flock`, or lets the optional OpenForge harness lease them ([capacity](harness.md#scoped-checks-and-capacity)). Preserve process cancellation/draining and never share a scope between sessions.

## Failure injection

Unit checks model duplicate delivery, unknown outcome, stale authority, cancellation and controlled-time retry. Integration checkpoints prove actual transaction rollback/lock ordering, concurrent writers, publication CAS, callback revocation and restart. Keep incomplete real semantics explicit; do not weaken checks or install test authority into the executable.

## Task checks and validation

`make docs-check` validates Markdown links, anchors, fenced JSON and shell examples. It does not read task metadata.

`make validate-tasks` (`go run ./dev/cmd/repoctl tasks`) validates every `docs/tasks/T-*.md`:

- required frontmatter (`id`, `milestone`, `status`, `depends_on`, `decision_blockers`, `owns`), no unknown keys, `id` equal to the file-name prefix, and `status` in `planned|blocked|deferred|done`;
- `planned` and `blocked` tasks declare `owns`, `check` and `acceptance` (an empty list is valid); `blocked` tasks name at least one decision;
- dependencies exist and are acyclic, IDs are unique;
- owned paths are clean relative paths (a trailing `/` marks a directory), and two active (`planned` or `blocked`) tasks that do not depend on each other transitively never own equal or nested paths;
- the `Dependency index` table in `docs/tasks/README.md` has a row for every active task and each row's link, dependencies and status word agree with the frontmatter (a decision suffix such as `blocked (O-32)` is accepted). `done` and `deferred` tasks may be absent from the index: completed records are pruned once merged and remain in Git history.

It does not require future owned paths or check scripts to exist. Errors name the file and field.

`make check-task TASK=T-NNN` (alias `test-task`, `go run ./dev/cmd/repoctl check-task T-NNN`) runs the task's declared `check` with `bash -c` from the repository root. It sets `KODAMA_TASK_ID`, `KODAMA_RESOURCE_PREFIX` (the lowercase ID without the hyphen, for example `t126`) and, unless set, `DEV_SCOPE`, and points `FORGE_RESULTS` at a fresh file. A non-zero exit fails the task. When `acceptance` is non-empty the results must show every named test as passed and none as failed or skipped; both the aggregated object written by `make check-go` and raw `go test -json` lines are accepted, and a missing or stale file fails, as does a raw `go test -json` stream with a failed or incomplete package. An unavailable environment (exit `75`, or an `infra_pause` record such as `make check-go-integration` writes, whatever the exit status) prints "infrastructure unavailable" and exits `75` when run through `repoctl` directly; `make` shows it as an error. The legacy `LAYER` and `STAGE` overrides are rejected. The runner never schedules, creates worktrees, reviews or merges.

## Default workflow and validation cadence

The coordinator creates each task worktree under `.work/<task>/` from the integration branch and gives the implementer the task record; the implementer edits only owned paths and commits; an independent reviewer approves the exact candidate; the coordinator runs the declared checks (`make check-task TASK=T-NNN`) on that candidate and merges into the integration branch. Before merge the candidate needs passing scoped evidence and that review. Every five merges and at the end, run batch validation on the integration tip: `make lint-go test-go-unit`, then the bounded M1/M2 suites as their checkpoints land. There is no default age-triggered full suite, and publishing the integration branch to `main` is a separate explicit decision.
