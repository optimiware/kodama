# Repository instructions

Read [AGENT.md](AGENT.md) for the contribution workflow and task-specific specification pointers before working in this repository.

## Git workflow

1. At the start of every task, including documentation work, create a dedicated task branch and Git worktree under the primary checkout's `.work/<task-id-or-slug>/`. Resume an existing task in its assigned worktree. Keep `.work/` ignored by Git.
2. Create the worktree from the agreed integration branch. Run the following from the primary checkout, substituting the task name and integration branch:

   ```bash
   mkdir -p .work
   git worktree add -b task/T-001-scaffold .work/T-001-scaffold main
   ```

3. Make task edits and run checks inside that worktree. Follow the task's owned paths and use isolated test resources as described in [the task workflow](docs/tasks/README.md#ownership-and-integration).
4. Before merge, the current changes must receive code review approval and pass the task's scoped required tests and checks. Documentation-only tasks require the relevant documentation checks. Include the commands and results in the review handoff. Address review findings, rerun affected checks, and obtain review approval for the updated changes.
5. Product-wide unit validation runs against integrated `main` every five task merges and at queue drain; M1/M2 checkpoints add bounded integration suites. Go-only work does not rerun OpenForge or development Python tests. A failed batch pauses publication; repair its cause and retry with `bun scripts/openforge/src/cli.ts batch --retry`. Do not add a second full suite at publication.
6. Keep the task branch and worktree available while review or tests are pending. Do not merge until both review approval and passing scoped checks are recorded. The coordinator marks a task complete only after the reviewed and tested changes are merged.
7. Clean up as the final step of every merge: from the primary checkout, run `git worktree remove .work/<task-id-or-slug>` and `git branch -d task/<task-id-or-slug>`, then confirm with `git worktree list` that only other tasks remain. Use `-d`, not `-D`, so Git refuses to delete an unmerged branch. Keep the worktree and branch only when the user asks to keep them or the merge has not happened. Never remove another task's worktree. Remove cached tools or test resources only when they belong to the finished task.
8. Automated runs use OpenForge in `scripts/openforge/` (Bun/TypeScript, embedded OpenCode SDK) as described in [the harness guide](docs/implementation/harness.md). Bootstrap each worktree with `make openforge-install`; checks do not install dependencies or rewrite the lockfile. Its independent reviewer-agent verdict plus passing scoped checks, bound to the exact candidate and review context, is the review approval in step 4, and its merge step performs steps 6-7 for the task it merged; batch validation runs outside the per-task publication lane. Autonomous workers never push, never touch another task's worktree, and park the task for a human on any decision they cannot make from the specifications.

## Build and test

Local Go work needs Go 1.27.2, GNU Make 3.81+ and Python 3.9+ for the formatting helper. OpenForge automation additionally needs Bun 1.4.2. Containers are only needed for integration: Podman is the local default on every OS, with `DEV_PROVIDER=docker` available for CI. Use an isolated worktree as described in [Git workflow](#git-workflow). The Go module path is `github.com/optimiware/kodama`.

```sh
make lint test build     # Go unit loop: no containers or harness tests
make openforge-install   # only when using OpenForge commands
make tools               # integration tools
make dev-up DEV_SCOPE=t-001-local
make test-integration DEV_SCOPE=t-001-local
make dev-down DEV_SCOPE=t-001-local
make api-check          # the committed public API document and Go client match their sources
make test-m1            # the M1 composition under KODAMA_ENV=development: no container, no service
make test-m2            # the M2 composition (model gateway, MCP proxy, Git trigger) and client compatibility: same environment
make test-m3            # the M3 composition and contract compatibility (dependencies, flow summaries, QA mapping, ChatOps): same environment
make test-assembly      # the core assembly evidence (needs the services named below)
```

The environment includes PostgreSQL, NATS JetStream, LocalStack S3, HTTPS/token Git, [oauthsonas](https://github.com/optimiweb/oauthsonas), and a local OCI registry. The binary is written to `dev/.state/bin/kodama`; `kodama server` exposes process health/readiness (health only unless `--assembly core` is selected). See the [development runbook](dev/README.md), [configuration contract](dev/configuration.md), and [verification handoff](dev/review.md) for pinned dependencies, named acceptance checks, and qualification status.

## Release image

[Containerfile](Containerfile) builds the static `kodama` binary in a Go stage and copies it onto Debian trixie-slim with Git and CA certificates. The image entrypoint is `/usr/local/bin/kodama`, which is also the file the chart's injector copies into tenant Jobs. It runs as uid 65532.

The [image workflow](.github/workflows/image.yml) builds `linux/amd64` and `linux/arm64` on every push to `main` and publishes `ghcr.io/<owner>/kodama:<semver>`. Image tags are semver git tags with no `v` prefix. [VERSION](VERSION) commits the baseline `0.0.1`; the first main run tags that commit `0.0.1` when no semver tag exists, and each later untagged main commit is tagged with the next patch (`0.0.2`, `0.0.3`, …). A commit that already carries a semver tag is rebuilt without a new tag. Minor and major releases are manual tags. The chart still requires a digest pin; do not install from the tag alone.

```sh
podman build -f Containerfile -t localhost/kodama:0.0.1 .
```

The [task index](docs/tasks/README.md) records merged contributions and remaining joins. Incremental composition (T-070 core → T-071 execution → T-072 manual agent/delivery, with milestone joins between them) turns those contributions into usable slices; T-070, T-071, T-072, T-039, T-075 and T-046 are assembled (T-072, T-039, T-075 and T-046 over injected adapters in unit tests: the real SQL, deployment, Git-admission and Podman/kind joins are T-073, T-067 and T-038, and the real business-loop joins are T-044), and the installed round trips (T-026) are not yet recorded. Until that evidence is merged, the compiled binary is not a complete factory.

## OpenForge repository automation

[OpenForge](scripts/openforge/README.md) is the Bun/TypeScript contribution harness with an embedded OpenCode SDK 2.1.2 host. It plans, implements, independently reviews and qualifies task branches before publication. Kodama's product wrapper remains the separate T-031 milestone.

Run `make openforge-install` in worktrees using OpenForge commands; it uses the frozen package lock. Checks never install dependencies or rewrite that lock. `make check-openforge` runs typechecking, Biome and Bun tests **for harness changes**; `make test-python` is for development-tooling changes. `make test` runs only infrastructure-free Go tests. The [task queue](docs/tasks/README.md) schedules Go core → M1 checkpoint → mocked agentic core → M2 checkpoint, with production/extra wrappers/chassis deferred.

```sh
bun scripts/openforge/src/cli.ts doctor
bun scripts/openforge/src/cli.ts validate
bun scripts/openforge/src/cli.ts run --dry-run
```

See the [harness guide](docs/implementation/harness.md) for credential discovery, native role permissions, typed reports, resource leases, state-v3 migration and batch gates. Doctor qualifies runtime configuration/model availability; live-provider pilot qualification remains separate.

## Contributing

Kodama uses Apache-2.0, including commercial use under its terms. Contributions intentionally submitted for inclusion follow Section 5 of [LICENSE](LICENSE), unless explicitly stated otherwise. Third-party components retain their applicable licenses and attribution, including [pkg/netpolicy's NOTICE](pkg/netpolicy/NOTICE).

Use the [glossary](CONTEXT.md) for domain language and the [documentation conventions](docs/README.md#documentation-conventions) for specification changes. Record unresolved choices in the decision register and consequential architectural trade-offs in an ADR. Connect behavior changes to acceptance scenarios and update affected examples in the same change.

Start with [AGENT.md](AGENT.md) for the contribution workflow and task-specific specification pointers.

## Implementation status

What `kodama server` composes today, which capabilities are not wired and which acceptance evidence is still pending is recorded in [assembly and implementation status](docs/implementation/assembly-status.md). Package existence does not mean product routes, commands, migrations or workers are wired.
