---
id: T-001
milestone: foundation
status: done
depends_on: [T-000]
decision_blockers: []
owns: [go.mod, go.sum, Makefile, .github/workflows/, dev/, scripts/dev/, cmd/kodama/main.go, internal/app/, README.md]
---
# T-001 — Scaffold and local dependencies

## Goal
Give every agent one reproducible build/test environment.

## Contract
Follow [layout](../implementation/repository-layout.md) and [testing](../implementation/development-and-testing.md). A provisional local `module kodama` avoids blocking on public naming. Freeze configuration names and dependency versions before module tasks start.

## Deliverables
Go entrypoint/server stub, formatting/lint/test CI, Make targets, Compose-managed dependency containers and kind bootstrap with PG/JetStream/LocalStack S3/test HTTPS Git/oauthsonas OIDC/registry, isolated test prefixes, dependency lockfiles. See the [development runbook](../../dev/README.md) and [configuration contract](../../dev/configuration.md).

## Acceptance
`scaffold_smoke`: fresh setup builds the binary, starts dependencies, and verifies Pod-reachable endpoints. `dev_teardown`: scoped cleanup leaves unrelated resources intact. Supports PX-04 and AC-12.

## Out of scope
Business modules, Helm production claims, or choosing an open-source license.
