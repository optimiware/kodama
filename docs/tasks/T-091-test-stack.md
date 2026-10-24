---
id: T-091
milestone: M1
status: done
depends_on: [T-001, T-002, T-086]
decision_blockers: []
owns: [dev/, scripts/dev/, test/support/postgres/]
check: make test-python
resources: [go]
acceptance: [unit_stack_selection, unit_services_endpoints]
---
# T-091 — Cluster-free test stack

## Goal
Make the `test` environment start in seconds without kind, so M1 and the M2 and M3 persistence checkpoints stop paying for a cluster they do not examine ([ADR 0009](../adr/0009-environments.md)).

## Contract
`DEV_STACK` selects what `dev-up` builds. `services` provisions PostgreSQL, NATS, LocalStack S3, Git, OIDC and the registry on one scoped Podman network, and what the Podman runtime needs to start workloads that reach them and the server callback. `cluster` is today's behavior: `services` plus a scoped kind cluster. The default stays `cluster`, so existing callers are unchanged; `test` checkpoints such as T-026 select `services` explicitly. The Makefile is unchanged because `DEV_STACK` reaches the environment script from the command line. Endpoint files name host URLs and workload URLs separately for each stack, and preflight checks only what the stack provides. Nothing pre-creates a kubeconfig in `services`.

`test/support/postgres` clones instead of migrating per test: it migrates one template database per manifest digest, then creates each test's database `FROM` that template. Measure it against per-test migration and against an embedded PostgreSQL binary on the same machine. Adopt the clone only if it is at least three times faster, and adopt an embedded binary only through a recorded decision.

Optionally pre-warm the scope registry with the pinned tools image digest once per scope and publish it in the endpoint file, so delivery drivers can reuse it instead of building it per package. The existing template drivers keep building until T-065 or T-067 consumes it.

## Deliverables
The stack split with Python unit tests for stack selection and endpoint files, the template-database support with its measurement recorded in the review handoff, updated `dev/README.md` runbook and preflight, and teardown that removes exactly what each stack created.

## Acceptance
`unit_stack_selection` proves `DEV_STACK` selects the documented resource set, rejects unknown values and never creates a cluster for `services`. `unit_services_endpoints` proves the endpoint file for each stack names only the endpoints it provides. Run explicitly in the checkpoint environment: `dev-up`/`dev-preflight`/`dev-down` for both stacks, a Podman-runtime workload reaching PostgreSQL by its workload URL, and the template-database round trip including cleanup.

## Out of scope
Changing Makefile targets or the OpenForge resource leases, qualification of M1 itself (T-026), kind-specific behavior and production images.
