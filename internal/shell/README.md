# internal/shell

The Native Shell layer of the [two-layer architecture](../../docs/adr/0013-two-layer-architecture-and-domain-simplification.md).
It sits above the Pipeline Runtime kernel (`internal/kernel`) and below the
agentic services. The layer rules in
[repository layout](../../docs/implementation/repository-layout.md#layer-dependency-rules)
apply: the shell may depend on kernel contracts; the kernel never imports the shell.

These packages were relocated from `internal/kernel/` by T-173 (P1.0 boundary
relocation, see the
[simplification plan](../../docs/implementation/pipeline-runtime-simplification.md#p10-boundary-relocation)).
The move is mechanical: directories, import paths and nothing else. The `package`
clauses, exported names, SQL and behaviour are unchanged, so each package below
is still a coarse legacy package, not yet its final shell shape.

## Frozen destination manifest

Do not add packages here, or move a package to another destination, without
updating this manifest and the plan. Phases refer to the
[simplification plan](../../docs/implementation/pipeline-runtime-simplification.md).

| Package today | Package clause | Responsibility today | Destination and splitting phase |
| --- | --- | --- | --- |
| `internal/shell/api` | `api` | HTTP surface of the simplified assembly: tenant routes for projects, repositories, pipelines, runs and tasks, plus the Git hosting, run-context and policy routes. | Splits into `shell/api` (HTTP and SSE), `shell/mcp` and `shell/cli` handlers over shell use cases (P2.6). Integration authority is already in `shell/git` (T-187); the remaining Git hosting policy follows it (P2.4). |
| `internal/shell/bus` | `bus` | Carries committed events from the audit stream to consumers: bus interface, in-memory bus, JetStream bus and publisher port. | Splits into the transactional outbox (state mutation and outbox intent in one transaction) and the NATS adapter `internal/natsbus` (P2.3). |
| `internal/shell/dispatch` | `dispatch` | Turns an applied push (`git.refs_updated`) into indexed pipelines and runs. | Moves into `shell/pipelines` (source indexing and triggers) (P2.2, P2.3). |
| `internal/shell/store` | `store` | PostgreSQL persistence of the simplified assembly: tenant-scoped operations for pipelines, runs, steps, tasks, repositories, resources and dispatch, plus the in-memory Git store. | Splits into `shell/pipelines`, `shell/tasks` and the kernel run-state adapter (`runstate`) behind kernel ports (P2.1–P2.3). |
| `internal/shell/git` | `git` | Git integration authority (T-187, P2.4): a pure decision over the run's durable record, the repository's integration pipeline and the worker's view of the ref, issued as an exact ref and commit pair and rechecked at every receive. No task field, resolution row or event is an input. | Stays; the remaining Git hosting policy joins it (P2.4). |
| `internal/shell/secrets` | `secret` | Envelope encryption of one revision of a Kodama secret: data-key wrap, seal and open under the installation KEK provider. | Becomes `shell/secrets` (custody and key providers), bound to the kernel secret-resolver port (P2.5). The package clause is renamed to `secrets` at that point. |
| `internal/shell/serve` | `serve` | Composes the simplified kernel: store, executor, HTTP API, scheduler loop, Git, event and run-source wiring; the cutover target of `kodama server --assembly kernel`. | Dissolves into `internal/app` composition (P3.2); event and run-source wiring follow the packages it assembles. |

## Not moved

- `internal/kernel/pipeline`: pipeline model; remains in the kernel.
- `internal/kernel/runner`: the blocking runner; remains the transitional
  executor until the replacement engine (P1.3) lands. Only its imports of the
  moved packages changed.
