# Go + PostgreSQL blueprint template

The executable form of the [Go/PostgreSQL blueprint](../../docs/examples/blueprints/go-postgres.md): one working
inventory-gap application (review an ERP-versus-count gap, request an idempotent ERP adjustment, never claim
resolution before acknowledgment), its migrations, tenant authorization, a controlled ERP peer, restricted
tools image, ordinary delivery scripts and validated pipeline examples.

It is **not** a shared library. Generation copies a self-contained Go module; the generated application imports
nothing from Kodama and runs with no factory available.

## Generate

```bash
sh templates/go-postgres/generate.sh --module example.com/acme/inventory --dest ../inventory --git
```

The module path is validated, scripts keep their executable bits, and the output is reproducible: the same
template and module path give the same tree digest and (with `--git`) the same commit. `GENERATED_FROM.json`
records the template version, the lock digest and the tree digest. [`template.lock.json`](template.lock.json) pins
Go, modules, base images and tools; `app/deploy/tools/lock.env` and `app/.ko.yaml` hold the corresponding
checksums and digests.

## Layout

| Path | Contents |
| --- | --- |
| `app/` | the generated application (see `app/README.md`) |
| `app/docs/specification.md` | authoritative behavior and `INV-nn` requirement IDs |
| `app/pipelines/` | quality/build and preview/stage pipeline examples with `@@placeholders@@` |
| `app/ci/` | the five ordinary scripts and the pipeline renderer |
| `app/deploy/tools/` | the digest-pinned tools image, ko/kubectl locks |
| `acceptance/` | root-module drivers that generate a fresh application and run the named acceptance tests |
| `docs/handoff.md` | baseline, assumptions, commands run with results, and what remains for T-046/T-044 |

## Acceptance

```bash
make check-task TASK=T-043              # fast loop: no services (domain tests, contracts, generation); never qualifying evidence
make dev-up DEV_SCOPE=t043              # a kind cluster, registry, PostgreSQL and OIDC provider for the real delivery suites
# real delivery qualification: hold go, services and kind throughout
bun ../openforge/src/cli.ts resources --resources go,services,kind --execution-timeout 3000 -- \
  go test -race -count=1 -timeout=40m ./templates/go-postgres/acceptance/...
```

`make check-task TASK=T-043 LAYER=all` reserves only the `go,services` leases, and provisioning a scope earlier does
not keep its `kind` lease held while tests run, so the real cluster suites are qualified under the outer wrapper above
(the coordinator owns complete-layer qualification).

The root drivers generate a new application for each test and run its scenario tests; a skipped service test is
not acceptance. They are split into separate packages so that they can run in parallel:

| Package | Named test | Runs |
| --- | --- | --- |
| `acceptance/` | `Test_blueprint_go_tenant`, `Test_erp_duplicate_resolution` | the application's tenancy, identity, migration and ERP tests with real PostgreSQL |
| `acceptance/` | `TestDeliveryScriptContracts`, `TestRuntimeConfigPromotionContracts`, `TestDeliveryRecovery`, `TestFactoryContracts`, `TestSnapshotBindingContract`, `TestConformanceEvidenceDeclaration`, `TestGeneratedAppIndependent` | host-level scripts with controlled factory and cluster doubles (supplementary, not real-cluster evidence), merged-contract checks and generation |
| `acceptance/delivery/` | `Test_blueprint_go_delivery` | `test/realdelivery` `TestRealDelivery` |
| `acceptance/promotion/` | `Test_runtime_config_promotion` | `TestRealPromotion` |
| `acceptance/recovery/` | `TestRealDeliveryRecovery` | `TestRealDeliveryRecovery`, `TestRealDeliveryScriptRefusal` |

The last three run the five ordinary scripts in the restricted tools image against a provisioned scope of the
repository's development environment (`dev/.state/environments/<DEV_SCOPE>`, default `t043`; `INVENTORY_DEV_ENV`
overrides the directory): a real kind cluster reached with a namespace-scoped credential, an OCI registry,
PostgreSQL and the development OIDC provider. A scope that is not provisioned fails them rather than skipping.
The factory's runtime callbacks and the ERP peer are the only fixtures. The setup, networking, cleanup and the
qualified provider are described in the generated application's
[operations guide](app/docs/operations.md#real-delivery-tests).

Required for the complete layer: `KODAMA_TEST_POSTGRES_ADMIN_URL` (a disposable PostgreSQL superuser URI),
`KODAMA_REGISTRY_URL` (an OCI registry reachable from the host), the provisioned development scope above, a
container runtime (`podman` or `docker`; only Podman has been qualified) and network access for module and
base-image downloads. Measured on the qualification machine with warm caches (delivery, promotion and recovery
packages running concurrently): delivery about 398 s, promotion 224 s, recovery 164 s, whole run about 9.5 minutes.
Cold image caches or slower machines may exceed Go's default 10-minute package timeout, and the application's own
suites allow 25 minutes, so pass an explicit `-timeout` (for example `40m`) as shown above.
