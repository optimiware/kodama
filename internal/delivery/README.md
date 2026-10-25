# internal/delivery

Environment and deployment records (task T-035). It supports build/preview/stage
bookkeeping around ordinary scripts; it is **not** a deployment engine, applies
nothing to a cluster and schedules nothing.

Status: the record package, its adapters, the PostgreSQL store and migration,
the HTTP contract (`api/delivery.yaml`) with its public and runtime handlers, the
published fixtures and the component tests are implemented in this package. The
tests run the real store, migration, execution/secrets/artifact/runtime-authority
adapters and the real handlers over PostgreSQL; they are **component evidence**,
not production qualification. Nothing registers these routes, migrations or
adapters in the composition root yet (T-072); the real admission transaction,
retention and cleanup bindings are qualified by T-077 and the end-to-end path by
T-067. See [downstream responsibilities](#downstream-responsibilities).

Not in this package: Kubernetes apply or delete, scheduling, secret decryption,
execution or result mutation. The tools image and scripts belong to T-065, the
TTL scheduler and cleanup run to T-066.

## Layout

| Path | Purpose |
| --- | --- |
| `environment.go`, `snapshot.go` | `Environment` aggregate; immutable `ConfigurationSnapshot` and its canonical digest |
| `deployment.go`, `provenance.go` | `Deployment` state machine, object identities, annotations; `VerifyImage` |
| `ports.go`, `validation.go` | consumer-owned ports; pinned-context checks (`CheckPinned`, `CheckEnvironment`) |
| `service.go`, `environments.go`, `registration.go`, `outcomes.go`, `cleanup.go` | use cases (`beginRuntime` authenticates and checks the retained pin for every runtime callback) |
| `adapters/` | identity, secrets, execution, artifact, runtime-authority adapters; `execution.EnvironmentResolver`; `execution.DeploymentCleanup` |
| `postgres/` | store, migration `T035_001_delivery.sql`, guard, events, history; `keyed.go` joins human creation to the HTTP foundation's keyed transaction |
| `http/` | public `Handler` and runtime `Runtime`, DTOs generated from `api/delivery.yaml` (`delivery.gen.go`), `dto.go`, `errors.go`, `strict.go` |
| `deliverytest/` | component fixtures: real PostgreSQL schemas and adapters; controlled doubles only for the OCI registry, Kubernetes Job observation and Git sources, plus `Pins` (immutable-pin edits) and `Bindings` (scope-aware cleanup bindings) |
| `testdata/` | published JSON exchanges (see [fixtures](#fixtures)) |
| `integration/` | component tests over real PostgreSQL (resource prefix `t035`) |

## Model

* **Environment** — project-owned: `kind` (`preview` with a TTL, default 24 hours
  per O-11 and configurable; `stage` without), a `target` label, one `namespace`,
  a kubeconfig credential reference (must be a project-owned `kubeconfig` secret;
  access stays namespace-scoped by that credential), explicit script credentials
  (`{name, ref, delivery: file|env}`, O-19) and explicit nonsecret `config`.
* **ConfigurationSnapshot** — immutable, per `(environment, digest)`. The digest
  is SHA-256 over canonical JSON of kind, target, namespace, config, TTL and the
  exact bindings (reference, **pinned version**, name, delivery). It never covers
  a secret value or a timestamp. Edits and rotation create *new* snapshots; a
  retained one is never changed (a database trigger refuses `UPDATE`/`DELETE`).
* **Deployment** — the durable pre-apply row. `pending → running → succeeded|failed`
  plus `reason`, `expires_at`, `deleted_at`. `running` with `reason=outcome_unknown`
  blocks another apply until settled. One deployment per `(tenant, project,
  actionKey)`; its ID is derived from that triple, so a retry or a concurrent
  duplicate always names the same record.
* **History** — append-only `deployment_history` rows (durable receipts); outbox
  `deployment.state_changed` for each status transition.

## Lifecycle of a script's callbacks

All runtime calls authenticate `deployment.call` for the route attempt, then read
the attempt's **immutable pinned context** and check it against the retained
snapshot. Nothing ever reads the environment's current settings.

1. `POST /runtime/v1/attempts/{attemptId}/deployments` — registers
   `{source, image}`. Everything else (run, step, action key, originating
   attempt, environment pin) comes from the pinned context. The image must be a
   usable OCI output, the context's pinned input, of exactly this source, accepted
   by a succeeded producer (DQ-01/DQ-07). Response: the receipt, including the
   **annotation values** to put on every applied object.
2. `POST …/deployments/{id}/outcome` with `state: applying` and the intended
   object identities — **before** the first external change.
3. Apply the objects with the annotations (names are adapter-configured;
   `delivery.DefaultAnnotations` is the published fixture configuration:
   `kodama.optimi.com/deployment-id`, `…/action-key`, `…/configuration-digest`).
4. `POST …/outcome` with `state: succeeded` and every object's observed `uid` and
   `ready: true` (structured readiness; exit status and output text are not
   evidence), or `failed` with a reason code, or `unknown`.
5. After a lost response, a restart or a retry attempt of the same step:
   `GET …/deployments/by-action/{actionKey}` (the key must be the attempt's own)
   returns the same identities and status. Observe the objects by their
   annotations before applying again; **never** re-apply while `reason` is
   `outcome_unknown`: settle it (`succeeded` with observed UIDs, or `failed`) by
   observation.

Retries: replays of identical reports return the stored receipt; a different
report for a settled outcome is `409 conflict`; a refused apply while the outcome
is unknown is `409 outcome_unknown`. `503 unavailable` means nothing was
acknowledged — repeat the same call.

## Cleanup (record boundary only)

`POST /api/v1/…/deployments/{id}/cleanup` records the request under `If-Match`
(`Service.RequestCleanupAt` compares the expected version inside the store's
write on the locked row, so a stale request records nothing; the scheduler-facing
`Service.RequestCleanup` stays unconditional and idempotent);
`ExpiredDeployments` enumerates the past-TTL, undeleted set; `RetainedView`
returns the retained snapshot and the exact owned objects;
`adapters.Cleanup` implements `execution.DeploymentCleanup` (recorded + requested
+ undeleted). A cleanup **run** reports through
`POST …/deployments/{id}/cleanup-result`; it must prove its binding through the
injected `delivery.CleanupBindings` reader (environment equality alone is not
proof; no reader or binding denies). A completed cleanup sets `deleted_at` and keeps
the outcome, objects and history; failures/unknown are kept as visible exceptions
and retried. Scheduling, the cleanup run and Kubernetes deletes belong to T-066/T-067.

## Construction and wiring order

```go
manifest := storage.NewManifest(append([]storage.Contribution{
    storage.BaseContribution(), eventspg.Migrations(), httppg.Migrations(), identitypg.Migrations(),
    workpg.Migrations(), secretspg.Migrations(), pipelinepg.Migrations(), workspacepg.Migrations(),
    artifactspg.Migrations(),
}, append(execpg.Migrations(), rtpg.Migrations(), deliverypg.Migrations())...)...)

guard, _ := deliverypg.NewGuard(attemptAuthority, keyRing.Generation, clock)
store, _ := deliverypg.NewStore(deliverypg.Config{DB: runtimePool, Events: deliverypg.EventsFrom(eventStore), Guard: guard})
svc, _ := delivery.NewService(delivery.Config{Store: store, Access: access, Credentials: creds,
    Contexts: executionAdapter, Producers: executionAdapter, Artifacts: artifactAdapter, Authority: authority, Cleanups: bindings})
resolver, _ := adapters.NewEnvironments(svc)           // execution.NewServiceWithEnvironments(..., resolver)
cleanup, _ := adapters.NewCleanup(svc)                 // execution.Dependencies.Cleanup

idem, _ := httppg.NewIdempotency(httppg.Config{DB: runtimePool})
keyed, _ := deliverypg.NewKeyed(idem, store)           // human creation only; never the runtime path
codec, _ := httpapi.NewCursorCodec(signingKey)
public, _ := deliveryhttp.NewHandler(svc, keyed, codec, deliveryhttp.DefaultActions())
public.RegisterRoutes(humanRouter)                     // router prefix /api/v1
runtime, _ := deliveryhttp.NewRuntime(svc, deliveryhttp.DefaultRuntimeActions())
runtime.RegisterRoutes(runtimeRouter)                  // router prefix /runtime/v1, attempt-token authenticator
```

`deliverytest.Contributions()` is the executable form of the manifest order. The
delivery migration must follow the execution tables (`step_attempts`) and the
artifact and runtime-grant contributions; once deployed the order must not
change (the ledger refuses a pending migration ordered before an applied one).
Required missing collaborators fail construction (`Store`, `Access`,
`Credentials`); the runtime collaborators disable only what needs them and
never loosen a check.

**Missing-capability refusal.** Without `Authority` no runtime callback is
served (`ErrNotConfigured`, `501 not_configured`); without `Contexts` every
protected callback is denied as `pin_required` (`403`) because a missing pin
never falls back to the environment's current settings; without `Producers` and
`Artifacts` no deployment can be registered (`501`); without `Cleanups` no
cleanup callback is authorized (`403 forbidden`). `Registrar` (the keyed adapter)
and the cursor codec are required by `NewHandler`.

**Routes.** `Handler.RegisterRoutes` registers, beneath
`/api/v1/tenants/{tenantId}/projects/{projectId}`: `GET, POST /environments`,
`GET, PATCH /environments/{id}`, `GET, POST /deployments`, `GET /deployments/{id}`
and `POST /deployments/{id}/cleanup`. `Runtime.RegisterRoutes` registers, beneath
`/runtime/v1/attempts/{attemptId}/deployments`: `POST ""`, `GET /by-action/{actionKey}`,
`GET /{id}`, `POST /{id}/outcome` and `POST /{id}/cleanup-result`.
`deliveryhttp.PublicRoutes` and `RuntimeRoutes` export the inventory (method,
path, authorization action) the contract test compares with `api/delivery.yaml`.
There is no environment deletion, deployment mutation or settled-outcome route
on the human audience.

**Action policy.** `deployment.call` is granted on its own interface: the
composition root's `ActionPolicy` for deployment steps must add
`runtimeauth.ActionDeploymentCall` (see `deliverytest.DeploymentActions`), and the
grant registered at admission must include it. `results/postgres.DefaultActions`
deliberately does not.

## Permission mapping (router `Authorizer` actions → identity permissions)

| Route | Action (`deliveryhttp.DefaultActions`) | Identity permission |
| --- | --- | --- |
| `GET …/environments`, `GET …/environments/{id}` | `environment.list`, `environment.read` | `run:read` |
| `POST …/environments`, `PATCH …/environments/{id}` | `environment.create`, `environment.update` | `deployment:manage` |
| `GET …/deployments`, `GET …/deployments/{id}` | `deployment.list`, `deployment.read` | `run:read` |
| `POST …/deployments`, `POST …/deployments/{id}/cleanup` | `deployment.create`, `deployment.cleanup` | `deployment:manage` |

The service rechecks the same permissions through `delivery.Access` (current
bindings on every call) so a mis-mapped router cannot widen access.

Runtime routes use `deliveryhttp.DefaultRuntimeActions`
(`deployment.runtime.register|lookup|read|outcome|cleanup_result`). They name no
identity permission: authority is the attempt's `deployment.call` grant, checked
by `Authority.Authenticate` and repeated under lock by `postgres.Guard` in the
mutating transaction (no receipt exception: a revoked, terminal or
wrong-generation attempt is denied even for a replay). The handler forwards the
bearer to the service; the tenant and project come from the grant, never from the
request.

## Public creation, idempotency and replay

`POST …/deployments` takes `{attemptId, source, image}` and an `Idempotency-Key`.
It never accepts the current environment's settings: the environment pin, run,
step and action key come from the **originating attempt's immutable pinned
context** and its retained snapshot. The handler:

1. `Service.PlanRegistrationForAttempt` — authorizes the human, reads the pinned
   context, verifies the retained configuration and the image's provenance and
   pins the image against retention (all outside the keyed SQL transaction).
2. `postgres.Keyed.RegisterKeyed` — one transaction commits the deployment,
   history, audit entry, `deployment.state_changed` outbox event and the stored
   response. A replay never runs it and is authorized again against the stored
   deployment before the body is released; a changed body is
   `409 idempotency_conflict`.
3. `Service.CompleteRegistration` / `CompleteReplay` — reconcile the preparation
   pin against the authoritative record.

Independently of the HTTP key (which expires), one logical action key owns one
deployment: a different key for the same request observes the original
(`200`, `created: false`). A failed or unknown-outcome commit keeps the image
protected and the same request, repeated, observes whatever committed.



## Downstream responsibilities

* **T-065** (tools image, scripts) develops against the receipt, annotation and
  structured-outcome contract in `api/delivery.yaml` and `testdata/`.
* **T-066** (scheduler, cleanup-run contribution) uses `ExpiredDeployments`,
  `RequestCleanup`, `RetainedView` and supplies the `CleanupBindings` reader.
* **T-077** qualifies the real admission/retention/revocation/cleanup join with the
  provider this module exports; the component tests here use the real
  `execution.EnvironmentResolver` adapter over real execution and secrets tables
  but are not that qualification.
* **T-067** proves admission → context → deployment → apply/reconcile/cleanup
  end to end (`apply_ack_loss`, `cleanup_after_work_done`, `cleanup_scope` stay
  there and in T-066 and are not claimed here).
* **T-072** activates the qualified path in the composition root.

## Fixtures

`testdata/` publishes request/response exchanges for ordinary scripts, annotation
reconciliation and deployment-bound cleanup: `registration`, `pre_apply_intent`,
`annotation_receipts`, `lookup`, `structured_outcomes`, `unknown_outcome_refusal`,
`cleanup`, `pinned_configuration` (credential references, pinned digests) and
`stage_promotion` (the same verified image digest promoted to a true stage
environment). Every identifier is a `t035` placeholder. `http/contract_test.go`
validates each exchange against `api/delivery.yaml` and decodes it through the
generated types; `integration/http_test.go` runs the real handlers and compares
the real status, headers and body structure with them. T-065 develops its
scripts against them; they are not a runnable deployment.

## Artifact retention

The artifact owner's pin API opens its own transaction, so registration cannot
be atomic with it. The image is pinned first and idempotently under the
deployment's deterministic identity; an unknown commit outcome keeps the pin; a
definite refusal reconciles and releases it only when no committed deployment
uses that artifact; `Service.ReconcileRetention` resolves an uncertain case from the
authoritative record and is safe to repeat.

## Notifications

The merged event catalog has `deployment.state_changed` but no environment event
(`configuration.changed.kind` accepts only variable/secret). Environment changes
are therefore audited and versioned (`ETag`) without being published as another
aggregate's event. A catalog extension belongs to its owner.
