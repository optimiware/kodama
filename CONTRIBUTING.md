# Contributing to Kodama

Thanks for your interest. Kodama is early, so the most useful contributions right now are bug reports, documentation fixes, and small focused changes to the Go code.

## Before you start

- **Open an issue first** for anything bigger than a small fix, so we can agree on the approach before you spend time on it.
- **Check what is planned.** The [project status](docs/status.md) and the [task backlog](docs/tasks/README.md) list what is done, what is next and what is deliberately deferred.
- **Security problems** go through [SECURITY.md](SECURITY.md), not a public issue.

## Set up

You need Go 1.27.2 and GNU Make 3.81+. Podman is only needed for integration tests.

```sh
git clone https://github.com/optimiware/kodama.git
cd kodama
make lint test build
```

That runs formatting, `go vet`, the unit tests and the build, with no containers.

## Make a change

1. Fork the repository and create a branch.
2. Keep the change small and focused. Add or update tests for behavior you change.
3. Update the documentation and any affected examples in the same change.
4. Run the checks that match what you touched, then open a pull request.

| If you changed | Run |
| --- | --- |
| Go code | `make lint test build` |
| Anything under `internal/app` | `make test-m1 test-m2 test-m3` |
| The HTTP API (`api/`) | `make api-generate`, then `make api-check` |
| Documentation | `make docs-check` |
| The Helm chart | `make test-chart` |
| Development tooling under `dev/` | `make check-devtools` |

Integration tests need the local service stack: see the [development runbook](dev/README.md), then run `make dev-up`, `make test-integration` and `make dev-down`.

## How the code is organised

One Go module with vertical capability packages under `internal/`, and a single server process composed explicitly in `internal/app`. Business rules live in entities and use cases; databases, Kubernetes and HTTP live in adapters. Read the [software design](docs/specs/software-design.md) before changing a boundary, and use the [glossary](CONTEXT.md) for domain terms.

Behavior is specified in `docs/`: start at the [documentation index](docs/README.md). A change that alters behavior should update its specification and acceptance scenarios too.

## Maintainers and coding agents

[AGENTS.md](AGENTS.md) holds the stricter workflow used by maintainers and coding agents: task worktrees, review gates and design discipline. External contributors do not need to follow its worktree process; a normal fork and branch is fine.

## License

Kodama is licensed under the [Apache License 2.0](LICENSE). Unless you state otherwise, any contribution you intentionally submit for inclusion is licensed under the same terms, as set out in Section 5 of the license. Third-party components keep their own licenses and attribution, for example [pkg/netpolicy](pkg/netpolicy/NOTICE).
