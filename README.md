# Kodama

Kodama is an API-first, multi-tenant software factory. Engineers configure
projects, pipelines, credentials and guidance; users submit tasks. Container
pipelines, agents and ordinary tools produce source changes, tested artifacts
and deployed software. Humans answer questions and approve candidates through
a role inbox.

A pipeline is an ordered list of actions; an action runs a container or
requests an answer; an execution runs a pipeline once; a task is a
conversation that starts executions.

## Status

The current version is in [`VERSION`](VERSION). The changes and known
limitations of each version are in [CHANGELOG.md](CHANGELOG.md).

## Quick start

A single-host test installation on rootless Podman (Podman 5, `git` 2.44, `jq`,
`openssl`, `curl`, `socat`):

```sh
git clone --branch v0.1.0 https://github.com/optimiware/kodama && cd kodama
podman pull ghcr.io/optimiware/kodama/kodama:0.1.0
KODAMA_LOCAL_KODAMA_IMAGE=ghcr.io/optimiware/kodama/kodama:0.1.0 ./deploy/podman/install.sh
```

Then follow the [Podman quickstart](docs/runbooks/podman-quickstart.md) to
[run a first pipeline](docs/runbooks/podman-quickstart.md#run-a-first-pipeline).

## Documentation

| Document | Contents |
|---|---|
| [KODAMA.md](KODAMA.md) | The product specification and acceptance criteria. It governs on conflict. |
| [docs/README.md](docs/README.md) | Detailed specifications and runbooks. |
| [docs/runbooks/podman-quickstart.md](docs/runbooks/podman-quickstart.md) | A single-host test installation on rootless Podman. |
| [docs/runbooks/production-deployment.md](docs/runbooks/production-deployment.md) | Deployment on Kubernetes (Scaleway, AWS, GCP). |
| [docs/runbooks/release.md](docs/runbooks/release.md) | How a version is built and published. |

## Repository layout

| Path | Contents |
|---|---|
| `cmd/kodama` | The server binary: `server`, `git`, `agent`, `services`, `migrate`, `bootstrap-admin`. |
| `cmd/kodactl` | The command-line client. |
| `api`, `client` | The public API declarations and the Go client. |
| `internal` | The Kernel, the Shell and Userland. |
| `deploy` | The image, the Helm charts, the Podman bundle and the OpenTofu roots per cloud. |
| `examples` | Example pipelines and knowledge files. |
| `test` | Contract, component, integration, e2e and qualification suites. |

## Build and test

The Go version is pinned in [`.go-version`](.go-version).

```sh
make build    # bin/kodama and bin/kodactl, stamped with the version and revision
make check    # gofmt, vet, unit tests, race tests, architecture and docs checks
make dist     # dist/: kodactl archives, Helm chart packages, SHA256SUMS (needs helm)
```

The suites that need real infrastructure (Podman, kind, the stage) are
described in
[docs/runbooks/unified-test-suite.md](docs/runbooks/unified-test-suite.md).

## License

Apache-2.0. See [LICENSE](LICENSE).
