# Blueprint guidance: Go + PostgreSQL

**Status:** executable template, version `1.0.0`, implemented by T-043 in [`templates/go-postgres/`](../../../templates/go-postgres/). It generates a self-contained application and is not a shared runtime library: a generated application imports nothing from Kodama and serves requests without any factory. Record the adopted template version (`GENERATED_FROM.json`: `templateVersion`, `templateLockSha256`, `treeSha256`) and the generated application's commit in the work item and workspace. The delivery scripts are exercised against a real cluster, registry, PostgreSQL and development identity provider; integration with a running factory, human gates and deployment acceptance is qualified by T-046 and T-044. See [Acceptance evidence and limits](#acceptance-evidence-and-limits).

## Generate and use the template

```bash
sh templates/go-postgres/generate.sh --module example.com/acme/inventory --dest ../inventory --git
cd ../inventory
go test ./internal/... ./cmd/...        # service-free checks
```

The generator validates the module path, preserves executable scripts, and is reproducible: one template and module path give one tree digest and, with `--git`, one commit. The generated application's own entry points are [`README.md`](../../../templates/go-postgres/app/README.md), its [specification](../../../templates/go-postgres/app/docs/specification.md) (requirement IDs `INV-01`..`INV-18`), [architecture](../../../templates/go-postgres/app/docs/architecture.md), [operations](../../../templates/go-postgres/app/docs/operations.md) (delivery contract, migrations, recovery), [OpenAPI](../../../templates/go-postgres/app/api/openapi.yaml), `AGENTS.md` and three focused skills. Pins (Go `1.27.2`, `pgx` `v5.11.0`, `go-oidc` `v3.21.0`, ko, kubectl, base images) are in [`template.lock.json`](../../../templates/go-postgres/template.lock.json).

## Suitable applications

Business APIs, integration-heavy internal applications, background processing, and ERP/CRM gap-filling services with explicit data ownership and operational requirements. A frontend can be selected separately when the application needs one.

## Suggested application layout

```text
AGENTS.md
README.md
CONTEXT.md
cmd/
  api/
  worker/
internal/
  identity/
  tenancy/
  exceptions/
  erp/
  delivery/
migrations/
api/
ci/
  quality.sh
  build.sh
  deploy-preview.sh
  deploy-stage.sh
  e2e.sh
docs/
  architecture.md
  operations.md
  adr/
```

The implemented template follows this layout with two commands, `cmd/inventory` (`serve`, `worker`, `migrate`, `seed`) and `cmd/delivery` (`build-verify`, `deploy`, `e2e`, `snapshot-digest`), the standard-library router, `pgx` with embedded forward-only migrations and a small application-owned migration runner. Module names should follow an application's actual domain; the layout illustrates the inventory-exception example, not a mandatory set of Go packages.

## Application tenancy and identity

- An application tenant owns its users' memberships and tenant-scoped records.
- Authenticate through the application's selected OIDC provider or identity integration.
- Determine accessible application tenants from verified identity and membership records.
- Treat an incoming tenant selector as a requested scope that needs authorization.
- Carry tenant identity through module interfaces and database operations.
- Use tenant-keyed relations and deliberate RLS/role design for shared-table deployments.
- Give background workers and ERP integrations scoped identities with attributable actions.

The application does not require a live request to Kodama for each user authorization. Its runtime identity and tenancy policy are part of its own deployment.

## Module design

Expose small interfaces for business operations such as resolving an exception. Keep authorization context, accepted inputs, outcome/error semantics, and idempotency explicit. The exception module owns the resolution rules; the ERP adapter owns protocol and reconciliation details.

Inject external adapters rather than creating provider clients inside domain operations. Test the module's observable behavior through its interface. Add an adapter seam when an actual provider or environment variation needs it.

## Data and migration conventions

Use explicit migrations with reviewable forward behavior. Document compatibility between old/new application versions during deployment. Keep schema ownership and runtime database roles separate. Test tenant filtering and ownership constraints through the actual query path.

For the example, the ERP owns balances and authoritative inventory records. The application owns exception review, local audit, and integration intent. Avoid claiming a local resolution succeeded until the ERP outcome is acknowledged or explicitly marked pending/unknown.

Use durable intent and a stable idempotency/reconciliation key for external writes. A local transaction cannot atomically commit a remote ERP effect; the adapter must expose the resulting states and recovery path.

## Configuration and secrets

Validate typed configuration at startup. Keep secret references and runtime values separate from source code and migration files. The deployment process may inject application credentials through its selected runtime secret mechanism. Application-secret lifecycle is distinct from the factory's own provider credentials.

The application must document required ERP scopes, database roles, and outgoing endpoints. Use restricted test credentials and controlled data in previews.

## Interface conventions

Document versioned request/response shapes, stable machine error codes, request correlation, optimistic concurrency where edits conflict, cursor pagination for large lists, and idempotency for commands with external effects. Record which errors are safe to retry and which require outcome reconciliation.

These conventions align with the factory's goals but are adopted as application guidance, not a mandatory shared runtime contract.

## Quality and delivery

| Stage | Expected evidence |
| --- | --- |
| Specification | Domain terms, acceptance IDs, tenant rules, ERP ownership/recovery behavior |
| Quality | Go formatting/static checks, meaningful module tests, real database authorization/migration tests |
| Build | Pinned toolchain/builder, image digest, source commit, build metadata |
| Preview | Scoped configuration, readiness, synthetic/authorized test data, expiration |
| Stage | Identified artifact promotion and recorded runtime configuration |
| E2E/QA | User workflows, cross-tenant denial, ERP retry/unknown-outcome behavior |
| Human review | Business acceptance tied to the inspected deployment |

The `ci` scripts implement the contracts in [ERP gap to preview](../workflows/erp-gap-to-preview.md) and [staging and acceptance](../workflows/staging-and-acceptance.md). They write declared artifacts under `KODAMA_OUTPUTS_DIR`, scalar and OCI results to `KODAMA_RESULTS_FILE`, and report deployments through the factory runtime callbacks (register, apply intent, structured outcome, lookup) using an explicit runtime origin and the entrypoint-maintained token file.

Two points where the merged contracts differ from the older workflow prose, and how the template follows the contracts:

- The pipeline validator accepts whole-scalar references only to string, boolean and integer values in `env` and `args`, so a build step's `ociArtifact` output cannot be passed to a later step in the same run, and output names are lowercase identifiers (`deployment_id`, `app_url`). The template therefore splits delivery into a quality/review/build run and separate preview and stage runs that receive the image as an `artifact` input plus its digest and repository.
- The runtime context carries the environment ID, configuration digest and secret bindings but not the environment's nonsecret settings. Runs supply the nonsecret environment snapshot as a declared input; the deploy helper verifies it against the pinned digest (the factory's exact serialization, checked by a root contract test) before registering or applying anything.

## Agent guidance to include in a generated application

The application's `AGENTS.md` should point to its domain glossary, specification, architecture, and operations guide according to the task. An implementation workflow should require identifying the relevant acceptance criterion, preserving tenant ownership, handling external uncertainty, and returning changed source plus meaningful evidence.

Keep application-specific skills focused: one for domain changes, one for database/migration work if needed, and one for delivery operations when those workflows genuinely differ. Shared rules should have one authoritative location rather than being repeated in every skill.

## Blueprint review checklist

- The application can operate with its documented runtime dependencies independently of factory availability.
- Every tenant-owned query and relation has a demonstrated isolation strategy.
- A resolution retry cannot duplicate an ERP action, or the uncertain outcome is explicitly surfaced.
- Migrations and application versions have a documented deployment/recovery path.
- Preview, stage, test evidence, and human review identify the same intended candidate/artifact.
- Agent instructions explain where to find authoritative context and what completion evidence to produce.

## Acceptance evidence and limits

The root-module acceptance drivers in [`templates/go-postgres/acceptance/`](../../../templates/go-postgres/acceptance/) generate a fresh application per test. The tenant and ERP drivers run it against real PostgreSQL. The delivery, promotion and recovery drivers (separate packages that can run in parallel) run the generated application's `test/realdelivery` suites: the five ordinary scripts execute in the restricted tools image against a real kind cluster reached with a namespace-scoped credential, a real OCI registry, real PostgreSQL and the development OIDC provider. `make check-task TASK=T-043` runs only the service-free groups and is never qualifying evidence. For the real suites provision a scope (`make dev-up DEV_SCOPE=t043`) and run them under the outer wrapper `bun scripts/openforge/src/cli.ts resources --resources go,services,kind --timeout 3000 -- go test -race -count=1 -timeout=40m ./templates/go-postgres/acceptance/...`, because the complete layer reserves only `go,services` and a provisioned scope does not hold its `kind` lease during tests. Measured on the qualification machine with warm caches the delivery, promotion and recovery packages took about 398, 224 and 164 seconds; cold caches or slower machines may exceed Go's default 10-minute package timeout, so pass an explicit `-timeout`. A missing scope fails instead of skipping.

| Test | Proves |
| --- | --- |
| `Test_blueprint_go_tenant` | verified OIDC identity plus current application membership, cross-tenant denial, worker isolation, a worker that waits for provisioning in a fresh environment, operation with factory endpoints unreachable |
| `Test_erp_duplicate_resolution` | concurrent duplicates give one intent and one ERP effect; changed payloads conflict; acknowledgment loss and worker restart reconcile one operation; unresolved outcomes stay visible |
| `Test_blueprint_go_delivery` | in the tools image under the restricted baseline: real quality checks against PostgreSQL; two builds of one commit push one reproducible digest with source, tool image and platform evidence, pulled by digest and run; preview deployment through real migration Job, API and worker Deployments; evidence equal to the live object UIDs; e2e against the deployed API that ends in an ERP acknowledgment through the deployed worker |
| `Test_runtime_config_promotion` | preview and stage share one image digest with distinct namespaces, databases, credentials and explicit configuration; a live deployment or configuration that is not the one under test is refused before any workflow write; an edited snapshot cannot replace the pinned one |
| `TestRealDeliveryRecovery` | lost registration, apply-intent and success responses; a lost apply response settled by observing the objects real `kubectl` created; restart with an unknown outcome observed and never re-applied; revoked authority, mismatched pins, RBAC denial, foreign objects, migration failure and bounded readiness |
| `TestDeliveryRecovery`, `TestDeliveryScriptContracts`, `TestFactoryContracts`, `TestGeneratedAppIndependent` | supplementary: in-memory fault injection, host-level scripts with doubles, merged pipeline validation and snapshot serialization, independence and reproducible generation |

Checklist mapping: independence from the factory (`blueprint_go_tenant`); tenant isolation by query, relation and RLS (`blueprint_go_tenant`, `TestTenantTransactions`); retries cannot duplicate or must surface uncertainty (`erp_duplicate_resolution`); migrations with a documented compatibility and recovery path (`TestMigrations`, application operations guide, migration failure in `TestRealDeliveryRecovery`); preview, stage, tests and review identify one candidate and artifact (`blueprint_go_delivery`, `runtime_config_promotion`; the human review binding is T-044); agent instructions with authoritative context and completion evidence (`AGENTS.md` and skills).

Limits that remain with their owners: the ERP protocol is a sample and no vendor integration; the factory's runtime callbacks and the ERP peer are controlled fixtures, so assembled factory admission, authorization, gates, exploratory QA and human completion are T-046 and T-044 acceptance; the component evidence comes from one provider (Podman on macOS arm64) and one cluster version (kind, Kubernetes v1.37), which does not qualify Docker or another cluster; the tools image digest is recorded by the installation that builds it. The handoff and assumptions are in [`templates/go-postgres/docs/handoff.md`](../../../templates/go-postgres/docs/handoff.md).

## Evolution

Record deviations in application ADRs. Feed repeated improvements back into the blueprint through an improvement proposal. Introduce reusable Go libraries only after common behavior across actual applications justifies their interface and compatibility burden.
