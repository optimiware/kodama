# T-001 review handoff

## Change boundary

- Branch: `task/T-001-scaffold`; base: local `main` at `fa954ea`.
- Worktree: `.work/T-001-scaffold/` under the primary checkout.
- Status: merged to `main` (commit `0c848fe`) after local Podman verification against the refreshed dependency set (below). No independent reviewer approved it; the owner directed the merge. Docker/Linux CI has not run.
- Main changes: Go process scaffold using stdlib `flag`; health OpenAPI; locked dependencies/tools; Compose-managed dependency containers plus a kind cluster; Pod-native protocol probes; teardown acceptance; Make targets; CI; configuration/runbook.
- Layout: the scaffold harness was Python in `scripts/dev/` (task ownership updated to include it); it has since been replaced by the Go tooling in `dev/cmd/` and `dev/internal/`. `dev/` keeps `compose.yaml`, the Containerfile, Go fixtures/probes, contracts, locks, and documentation.
- Owner-directed contract changes: LocalStack replaces MinIO; oauthsonas replaces the proposed generic/Keycloak fixture; Compose runs the dependency containers. Matching task/spec/decision text is updated. T-012 retains local-key adapter implementation; T-001 supplies its key fixture.

## Dependency refresh

Owner-requested updates on 2026-10-19:

- NATS **2.15.1-alpine** (latest patch of the 2.15 series) and LocalStack **4.14.0**, each pinned by a multi-architecture (arm64 + amd64) digest re-verified against Docker Hub metadata. Pins now live in `dev/compose.yaml`.
- Go packages and tools refreshed with `go get -u`: AWS SDK core **1.47.3**, credentials **1.20.9**, S3 **1.114.3**, JWT **5.3.1**, pgx **5.11.0**, nats.go **1.54.0**, actionlint **1.7.12** (already latest).
- **One deliberate exception:** `go.yaml.in/yaml/v4` is held at **v4.0.0-rc.3**, not the latest rc.6. actionlint 1.7.12 declares rc.3 and does not compile against rc.6 (`yaml.ParserError` and error-field API changes). This was found when the refresh was first verified. Revisit when actionlint releases support for a newer yaml/v4. `go get -u` will re-select rc.6 and break `make lint`; re-pin after any bulk upgrade.
- `go list -m -u all` otherwise shows upgrades only for modules in the module graph that nothing in this repository imports (test dependencies of dependencies).

## Verification record

Run locally on 2026-10-19 with macOS ARM64, Go 1.27.2, Podman 6.1.4 (rootful AppleHV VM, 8 CPUs / 8 GiB), podman-compose 1.6.0 as the `podman compose` provider, kind 0.33.0, Kubernetes 1.37.0, and kubectl 1.37.1, against the final code and dependencies:

| Command | Result |
| --- | --- |
| `make fmt lint test build` | Passed: formatting, vet, actionlint, configuration/digest-pin validation (8 images), 110-document link/anchor/JSON/shell checks, Go race tests, ten Python tests, static host build |
| `go mod verify`, `go mod tidy -diff`, `git diff --check` | Passed: modules verified, no lock drift, no whitespace errors |
| `dev/.state/bin/kodama server --help` | Passed: flags for HTTP address, log level, shutdown timeout |
| `make scaffold-smoke` (fresh scope, three runs; now `make dev-smoke`) | Passed each time: server, PostgreSQL, JetStream, LocalStack S3, HTTPS Git, oauthsonas PKCE, Pod registry push, kubelet digest pull (about 2 minutes) |
| `make dev-up` then `make test-integration` on an existing scope | Passed: Compose re-up and kubeconfig re-export are idempotent; EndpointSlices unchanged; probe passes again |
| `make dev-teardown-check` | Passed: mismatched node identity rejected; one owned container removed to model partial cleanup; repeated cleanup; independent cluster/ConfigMap and user kubeconfig preserved |
| `make dev-diagnostics` then `make dev-down` | Passed: report set produced with no credential or SQL material; complete teardown |

After each acceptance run no Compose containers, named volumes, state directories, or sentinel clusters remained, and no global kubeconfig changed. Cached images/tools and the Podman VM remain.

## Code-review findings addressed

Bugs: the Git fixture rejected chunked pushes (Git switches to chunked above 1 MiB; now spooled with a 64 MiB cap and covered by a 2 MiB push in the probe); Git probe failures discarded stderr; negative checks (read-only push, invalid token, ID-token audience, code replay) passed on any error and now assert HTTP 403/401, `ErrTokenInvalidAudience`, and HTTP 400.

Risks: Git service gate is default-deny with exactly one `service` value, tokens are whitespace-trimmed and must differ, and the `Authorization` header is not forwarded to the CGI process; the probe now proves the runtime role differs from the migration role and cannot create tables, verifies the ID token for the client audience and nonce, and copies the platform manifest from registry image indexes; the development CA is valid for one year with random serials and `MaxPathLenZero`; the Alpine release moved from code to `versions.lock.json`.

Simplifications: dependency containers moved out of Python into `dev/compose.yaml` (the Kubernetes workload generator, PVC/probe manifests, registry bookkeeping, and `chmod` workaround are gone). the environment script was about the same length as before (432 vs 446 lines) because it gained fixture generation and label-based ownership handling, so the gain is in responsibilities, not line count. JetStream uses the current `jetstream` package; the Containerfile uses `COPY --chmod`, a bind-mounted package, and an explicit `HOME`.

Not changed: LocalStack does not enforce S3 signature validation by default, so the S3 check proves the client signs requests and the protocol works, not credential enforcement. The docs state this scope.

## Review focus

1. `internal/app` is process composition/lifecycle only; dependency clients and fixture behavior stay under `dev/`.
2. `/readyz` is process readiness, not database/dispatch readiness. Health contract tests exercise the published response constants.
3. Teardown safety: kind nodes are bound to journaled container IDs and the cluster label; Compose containers are selected only by a random 128-bit ownership label, never by name or by the shared `kind` network, which teardown leaves untouched. Isolated namespaces are test scopes, not completed tenant isolation.
4. Fixture files are mode 0644 inside a 0700 scope directory so containers running as other UIDs can read them. Treat them as development-only credentials.
5. oauthsonas is a single-instance, in-memory development issuer. The probe validates access tokens and the ID token, and rejects API-audience ID-token use, code replay, and incorrect PKCE verifiers.
6. LocalStack verifies S3 protocol behavior; production IAM, artifact persistence/retention, and product authentication are later tasks.
7. Docker/Linux CI is configured and statically validated, but has not run. Docker Compose v2 and the containerfile's BuildKit-only features (`RUN --mount`, `COPY --chmod`) are assumed on the runner. ARM64 Podman acceptance does not establish Docker qualification.

The scaffold merged to `main` as a single commit on the owner's instruction and the task is marked `done`. Docker/Linux CI has not yet run; treat the first CI run as the remaining qualification of that path. T-003 and T-018 depend on this checkpoint.
