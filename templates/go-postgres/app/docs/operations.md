# Operations

## Runtime dependencies and processes

| Process | Purpose | Needs | Must not have |
| --- | --- | --- | --- |
| `inventory serve` | HTTP API | runtime database role, OIDC provider reachable (readiness), cursor key | ERP credential, migration credential |
| `inventory worker` | ERP intents for one tenant | runtime database role, the tenant's ERP credential, a `worker` membership for its service ID | migration credential, other tenants' credentials |
| `inventory migrate` | schema changes | owner (migration) database role | — |
| `inventory seed` | explicit test/preview provisioning | owner database role | never run in production against real tenants |

None of them contacts a software factory. The API stays live and serves requests while the factory is
down (`blueprint_go_tenant` proves it with every factory endpoint unreachable).

**Database roles.** The migration role owns the schema. The runtime role (`inventory_runtime` by default) is
created by provisioning with `LOGIN NOSUPERUSER NOBYPASSRLS` and only the grants the migrations give it. Never
point `serve` or `worker` at the owner role.

**ERP scopes.** `inventory:adjust` and `inventory:read`, issued per application tenant. Outgoing endpoint: only
`erp.baseUrl`. The sample protocol is in [`internal/erp/protocol.md`](../internal/erp/protocol.md); real ERP
credentials are never used in tests or previews.

**Identity.** `oidc.issuer` and `oidc.audience` are the application's own provider. Memberships are created by
provisioning (`inventory seed` for preview data); the example file is `config/seed.example.json`.

## Configuration

`config/*.example.json` document every setting; they are validated by tests. Secrets are file references
(`urlFile`, `cursorKeyFile`, `credentialFiles`) pointing at files mounted by the runtime's secret mechanism.
There is no inline-secret field. Start-up fails with every problem listed.

## Migrations and compatibility

Migrations are forward-only, ordered, checksummed, run one transaction each and serialize on an advisory lock.
Run `inventory migrate` (owner role) **before** the new version serves traffic; the delivery step does this with
a Job whose credential never reaches the API or worker pods.

Compatibility rule for a rolling deployment (old and new versions run together):

1. **Expand:** add tables, columns (nullable or defaulted) and indexes in release N. Release N−1 keeps working.
2. **Switch:** release N+1 uses the new structure.
3. **Contract:** drop the old structure in release N+2, after N−1 can no longer run.

An application binary refuses a database that has migrations it does not know (`database_newer`), so a rollback
of the code to a release older than the schema is stopped rather than corrupting data; roll back by deploying
the previous release only while it is still compatible with the schema (expand-only steps), otherwise restore
or fix forward. A changed applied migration is refused (checksum). Readiness stays false until the schema is at
the version the binary expects.

**Recovery.** There are no automatic down-migrations. A failed migration rolls back completely (no ledger row);
fix the file and rerun. For a destructive mistake, restore from backup or add a corrective forward migration.

## Delivery contract

The ordinary scripts are `ci/quality.sh`, `ci/build.sh`, `ci/deploy-preview.sh`, `ci/deploy-stage.sh` and
`ci/e2e.sh`. They run in the digest-pinned tools image ([`deploy/tools`](../deploy/tools/Containerfile)) as a
non-root user with a read-only root filesystem, no added capabilities, no Docker socket and no host mounts;
scratch space is a bounded temporary directory.

**Step entrypoint variables:** `KODAMA_OUTPUTS_DIR` (declared file artifacts), `KODAMA_RESULTS_FILE` (scalar and
OCI results), `KODAMA_SECRETS_DIR` (delivered secrets), `KODAMA_ATTEMPT_ID`.

**Explicit installation inputs** (pipeline `env`, never hidden defaults): `KODAMA_RUNTIME_URL` (the factory
runtime API origin reachable from step Pods) and `KODAMA_RUNTIME_TOKEN_FILE` (the renewable token file the
entrypoint maintains). The token is read from the file at call time and never from an environment variable.

| Script | Does | Evidence |
| --- | --- | --- |
| `quality.sh` | gofmt, vet, module verification, shell syntax, import boundary, unit tests, then real-PostgreSQL migration, tenancy and acceptance tests. A missing `INVENTORY_TEST_ADMIN_URL` fails. | `quality.json`, `quality.log` |
| `build.sh` | clean pinned checkout; `ko` builds a reproducible image (digest-pinned base, fixed timestamp, `-trimpath`), pushes it, and the helper verifies the digest in the registry and that the built commit is the pinned one | OCI output `{digest, registryRef}`, `build-metadata.json` (source commit, tool image, Go version, digest) |
| `deploy-preview.sh`, `deploy-stage.sh` | see below | `deployment.json`; outputs `deployment_id`, `app_url` |
| `e2e.sh` | verifies the live target **is** the intended deployment, then tests the workflow and tenant isolation | `e2e.json` |

### Environment snapshot input

The merged runtime context carries the environment ID, the configuration digest and the secret bindings, but not
the environment's nonsecret settings, and delivery receipts omit them as well. The run therefore supplies the
nonsecret snapshot explicitly in the declared input `environment_snapshot` (JSON). The helper:

1. parses it strictly and requires format `kodama/environment-snapshot/v1`;
2. requires its tenant, project and environment to equal the pinned context's and its kind to match the script;
3. recomputes its digest with the factory's exact serialization and requires it to equal the pinned
   `configDigest` — a snapshot that was edited, belongs to another environment, or whose credential versions were
   rotated is refused before anything is registered or applied;
4. requires every secret binding of the snapshot to appear identically (name, reference, version, delivery) in the
   pinned context, and the deployment credential to be one of them; unrelated bindings the context also declares are
   permitted and are not consumed;
5. after registration, requires the receipt's environment, digest, namespace, source, image and action key to match.

It never fetches current environment settings as a substitute. Required settings in `config`: `OIDC_ISSUER`,
`OIDC_AUDIENCE`, `ERP_BASE_URL`, `WORKER_TENANT`, `WORKER_SERVICE_ID`, `DATABASE_RUNTIME_ROLE` (optional
`REPLICAS` 1–5, `APP_URL`, `ERP_ALLOW_INSECURE`, `OIDC_ALLOW_INSECURE`). Required credentials (bindings):
`KUBECONFIG` (file; namespace-scoped), `DATABASE_RUNTIME_URL`, `DATABASE_MIGRATION_URL`, `CURSOR_KEY`, `ERP_TOKEN`.
Configuration or credential changes create a new snapshot and therefore a new identity; retained runs keep theirs.

### Deployment and recovery

1. Verify context, snapshot, image inputs (`image`, `image_digest`, `image_repository`) and that the registry holds
   the digest. Nothing external has happened yet.
2. Register `{source, image}` with the factory (idempotent by logical action). A lost response is repeated.
3. Observe the intended objects. One that exists without this deployment's annotation is foreign: it is never
   adopted or modified and the deployment fails.
4. Report `applying` with the intended object identities, **then** apply objects carrying the receipt's annotations.
5. Observe the actual UIDs and structured readiness (Deployment rollout, migration Job success), then report
   `succeeded` with every UID and `ready: true`, or `failed` with a reason.

Deployment uncertainty is the factory's existing record, not a new status: the deployment keeps its recorded
status and carries the `outcomeUnknown` indication or the reason `outcome_unknown`; this is distinct from the
application's own exception state `unknown` (an ERP outcome awaiting reconciliation). The application adds no
persisted deployment status or competing ledger.

After a lost response or restart the helper observes by annotation. If the outcome is recorded as unknown it
**observes and settles, never re-applies**: all objects present and ready settle as succeeded; none present as
failed (`apply_not_observed`); some present as failed (`partial_apply`). If the cluster cannot be observed the
outcome stays unknown. A settled failure is not retried into an apply. A revoked or mismatched pin stops all
effects. Preview deployments keep the factory-recorded `expiresAt` in `deployment.json`; cleanup is the factory's
cleanup owner's job and this application adds no TTL scheduler.

### Provisioning a fresh environment

The migration Job creates the schema; tenants and memberships are then provisioned with the owner identity
(`inventory seed`, or the installation's provisioning), because no runtime process may create them. The worker is
deployed with the application, so in a fresh environment it starts before its service identity holds a membership
(or before the schema exists). `inventory worker` therefore waits, logging `worker waiting for provisioning` every
five seconds for up to 15 minutes, and starts as soon as the worker membership is current. If provisioning does not appear within that
time it exits non-zero and the platform restarts it; a worker that has started keeps running. A configuration or credential error never waits. A service identity without a
current `worker` membership never processes work, and revoking it stops a running worker on its next cycle.

### Promotion

`deploy-stage.sh` takes the same `image_digest` the preview used and the stage environment's own snapshot.
Preview and stage therefore share one image digest and differ in explicit configuration (and so in the
configuration digest and deployment identity). Nothing rebuilds.

## Real delivery tests

`test/realdelivery` runs the five ordinary scripts as the step entrypoint would, inside the tools image built from
`deploy/tools/Containerfile` and pushed to the registry (non-root, read-only root filesystem, no added capabilities,
no container socket), against real dependencies. Only two things are controlled fixtures: the factory's runtime
callbacks (`test/support/factoryfake`, an `https` origin whose throwaway CA reaches the scripts through
`SSL_CERT_FILE`, because the helper accepts plain `http` only on loopback) and the ERP peer (`erpfake`).

Prerequisites: a provisioned development scope of the Kodama repository (`make dev-up DEV_SCOPE=<scope>`: a kind
cluster, an OCI registry, PostgreSQL and the oauthsonas OIDC provider), a container runtime, network access for
module and base-image downloads, and a clean Git checkout of the generated application.

```sh
sh generate.sh --module example.invalid/inventory --dest ../inventory --git       # in the template
cd ../inventory
INVENTORY_DEV_ENV=<kodama>/dev/.state/environments/<scope> \
  go test -count=1 -timeout=40m ./test/realdelivery/
```

Holding the scope provisioned does not hold its `kind` lease while tests run. In the Kodama repository, run the
real suites under the shared resource wrapper so `go`, `services` and `kind` stay leased throughout, for example
`bun ../openforge/src/cli.ts resources --resources go,services,kind --execution-timeout 3000 -- <test command>`
(without it the wrapper's default 1800 seconds covers queueing and execution together; set `--queue-timeout` separately if admission may wait). `make check-task ... LAYER=all` reserves only `go,services`, so it does not by itself
cover the cluster. Service-free checks (`make check-task TASK=T-043`) need none of this and are never qualifying
evidence. The suites allow up to 25 minutes themselves, so give `go test` an explicit `-timeout` above the
default 10 minutes for cold caches or slower machines (as `-timeout=40m` above).

`INVENTORY_APP_SRC` (default: this checkout), `INVENTORY_REAL_PREFIX` (resource prefix, default `t043`),
`INVENTORY_CONTAINER_RUNTIME`, `INVENTORY_KUBECTL` and `INVENTORY_REAL_EVIDENCE_DIR` (retains each script's results,
artifacts and console log) are optional. Without `INVENTORY_DEV_ENV` the tests skip; a skip is not acceptance.

| Test | Proves |
| --- | --- |
| `TestRealDelivery` | the tools image toolchain under the restricted baseline; `quality.sh` reaching real PostgreSQL; two concurrent `build.sh` runs of one commit pushing one digest with source, tool image and platform evidence; that image pulled by digest and run; `deploy-preview.sh` through real migration Job, API and worker Deployments with evidence equal to the live UIDs; `e2e.sh` against the deployed API ending in an ERP acknowledgment through the deployed worker |
| `TestRealPromotion` | one image digest in a preview and a stage namespace with separate databases, credentials and configuration; a live deployment or configuration that is not the one under test refused before any workflow write; an edited snapshot refused without touching the live objects |
| `TestRealDeliveryRecovery` | lost registration, apply-intent and success responses; a lost apply response settled by observation after real `kubectl` took effect; restart with an unknown outcome observed, never re-applied; revoked authority, mismatched pins, namespace RBAC denial, foreign objects, migration failure and bounded readiness |

Every namespace, database, role, container and image repository the tests create carries the resource prefix and a
random suffix and is removed on cleanup. Bootstrap credentials (the scope's PostgreSQL superuser, the cluster
administrator) stay in the provisioning helpers: the scripts receive only a namespace-scoped kubeconfig, their
own database URLs and a dedicated disposable `quality` role. Qualified on macOS arm64 with Podman 6.1.4, kind
v0.33.0 (Kubernetes v1.37.0) and kubectl v1.37.1; this does not qualify Docker. Containers share the Podman
VM's network namespace: the host is `host.containers.internal`, and a Pod reaches the controlled ERP peer through a
selector-less Service and EndpointSlice to that address.

## Tools image pins

[`deploy/tools/lock.env`](../deploy/tools/lock.env) pins ko and kubectl by version and SHA-256;
[`.ko.yaml`](../.ko.yaml) pins the runtime base image by digest; the tools image's base is pinned by digest in the
Containerfile. `deploy/tools/fetch.sh` downloads and verifies the binaries so the image build needs no network.
Record the built tools image digest in the installation's pipeline values. Quality and build steps also need module
downloads (`GOPROXY`) unless the installation provides a module mirror.

## Known limits

- The sample ERP protocol is not a vendor integration.
- The delivery scripts, the kubectl adapter and the recovery behavior are exercised against a real cluster, registry,
  PostgreSQL and development OIDC provider by `test/realdelivery` (see [Real delivery tests](#real-delivery-tests));
  that is component evidence on one provider and one cluster version. An installation qualifies its own cluster,
  registry, network policy and identity provider, and the assembled factory's admission, gates and human acceptance
  are outside this application.
- `admin` carries no extra permission yet; membership administration is out of band.
