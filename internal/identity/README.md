# Identity, tenancy and RBAC (T-010)

The standalone identity authority: OIDC access-token verification, the mapping of a verified `(issuer, subject)` to a stable user, tenant/project bindings of five fixed roles, the current-access decision every module asks, and the operator-only bootstrap. Specification: [identity, tenancy, and RBAC](../../docs/specs/identity-tenancy-and-rbac.md); platform boundary: [platform integration](../../docs/specs/platform-integration.md); HTTP: [HTTP contract](../../docs/contracts/http-api.md) and [`api/identity.yaml`](../../api/identity.yaml).

## Baseline and authority profile

- **Subject:** T-010, source baseline `8c5a23b0148b96bc3363d5b5cabc73b03dbdd594`. The implementation candidate is recorded in the review handoff, not here.
- **Requirements:** IT-01, IT-03, IT-09, IT-10, IT-11; supporting IT-02/04/05, AC-03/04/11/12, SD-01–08. Acceptance IT-A01, IT-A07 and the identity side of IT-A04; IT-A02/A03 for the contributed persistence.
- **Decisions:** confirmed C-01/03/11/16/17, baseline P-01/02/04/05/15/16, ADRs 0002, 0003, 0006, 0007. O-18 stays open for T-061 and is **not** selected here; O-10 stays a release reconciliation decision.
- **Implemented here:** the *standalone* profile only. An Optimi-hosted installation selects another `identity.Authority` (O-18/T-061). `identity.SelectAuthority` requires an explicit `Profile`; the unselected profile is never consulted, a hosted denial, outage or missing adapter never selects the local grants, and any unclassified authority error is `ErrUnavailable`. This module does not map hosted tenants, delegate to the platform or synchronize membership.

## Package layout

| Package | Role |
| --- | --- |
| `identity` | Entities (`Binding`, `Project`), the role policy (`Decide`), use cases (`Service`, `BootstrapService`), the `Authority` seam and consumer-owned ports. Imports no pgx, net/http, go-oidc or broker. |
| `identity/oidc` | `go-oidc` discovery and claim verification with a JWKS cache that separates an unreachable issuer (`ErrUnavailable`) from a bad credential (`ErrUnauthenticated`). |
| `identity/postgres` | Store, migrations, principal mapping, bootstrap store, transaction-bound authority, audit and event recording. |
| `identity/http` | `httpapi.Authenticator` and `httpapi.Authorizer`, handlers, generated types from `api/identity.yaml`. |
| `identity/consumer` | Bridges to `work.Authority` and the transaction-bound `work.PermissionChecker`. |
| `identity/idtest` | Test fixtures (isolated PostgreSQL stack, in-memory repository). Imported only by tests. |

`test/support/oidc` holds the real-issuer fixture (an isolated oauthsonas container driven through the authorization-code flow with S256 PKCE) and an in-process signed-token stub for malformed, expired, rotated and outage cases. The stub never substitutes for the real-issuer acceptance tests.

## Roles

Five fixed roles; no custom roles or policy language. Bindings compose only at the tenant and the matching project. HTTP route actions (for example `work_item.transition`) are **not** application permissions (for example `work_item:complete`): a route rule is a coarse gate, and the owning use case repeats its precise check through the same `Authority` inside its transaction.

| Role | Permissions |
| --- | --- |
| `admin` | `membership:manage`, `project:create` (tenant scope only), `project:settings`, `project:read`, `config:read`/`config:write`, `credential:read`/`credential:manage`. Not a reviewer, engineer or analyst. |
| `analyst` | Viewer set plus `work_item:create`, `edit_spec`, `refine`. |
| `engineer` | Viewer set plus `work_item:create`, `edit_spec`, `edit_technical`, `refine`, `work`, `cancel`, `reopen`, `workspace:manage`, `run:manage`, `deployment:manage`, `config:write`, `credential:read`. |
| `reviewer` | Viewer set plus `work_item:complete` and `gate:decide` (human actors only), `reopen`, `cancel`. |
| `viewer` | `project:read`, `config:read`, `work_item:read`, `run:read`, `evidence:read`, `source:read`. |

No role grants secret plaintext (there is no such permission). Unknown permissions are denied. `work_item:complete` and `gate:decide` additionally require a verified **human** actor; workload identities hold no human role. Outcomes are distinct: `ErrHidden` (no binding reaches the target; rendered exactly like a missing resource), `ErrDenied` (a member lacking the permission), and anything else is an outage, never access.

## Transaction ordering and recovery

Every permission-sensitive write runs in one `storage.WithTenant` transaction under the non-owner runtime role: project row lock (`FOR NO KEY UPDATE`, project-scoped changes only), then authorization revision rows in ascending user ID (`FOR UPDATE` for users whose bindings change, `FOR SHARE` for the acting administrator), then reads and mutations, with the audit and event append **last** (the order the work module documents). State, audit, authorization revision and outbox commit together or not at all; a backlog rejection rolls everything back. An unknown commit outcome is `ErrUnavailable`; clients reread by stable resource identity or unique key before retrying, and a duplicate create never emits another audit row or event.

`NewTxAuthority(tx)` (and `consumer.BindPermissions`) check permission inside another module's mutating transaction and keep the user's revision `FOR SHARE` until that transaction ends: a revocation either committed first (and is seen) or waits for the protected mutation to finish. Nothing caches positive results; events are notifications, not authority.

`membership.changed.effectiveVersion` is the user's monotonically increasing authorization revision in the tenant, retained after the last binding is removed. A project-scoped binding change advances the project version once, through either entry point (the membership routes or `PATCH` project `roleBindings`).

## Bootstrap

`identity.BootstrapService` is an explicitly invoked, operator-only service; no HTTP route exists. It requires the standalone profile, a configured eligible `(issuer, subject)` verified through the normal verifier **before** the administrative transaction opens, and a separate administrative database (`postgres.NewBootstrapStore`). It atomically provisions the tenant, user mapping, tenant-wide `admin` binding, revision, audit, event and the durable `identity_bootstrap` record, which the runtime role can neither read nor write. Concurrent invocations queue; an identical retry observes the committed result; changed parameters conflict; revoking the initial admin later does not re-enable bootstrap. T-027 owns command wiring.

## Integration exports

- Constructors: `identity.NewService`, `NewStandaloneAuthority`, `SelectAuthority`, `NewBootstrapService`; `oidc.NewVerifier`; `identitypg.NewStore`, `NewBootstrapStore`, `NewTxAuthority`, `Migrations`; `identityhttp.NewAuthenticator`, `NewAuthorizer`, `DefaultRules`, `NewHandler`, `(*Handler).RegisterRoutes`; `consumer.NewWorkAuthority`, `BindPermissions`.
- Migrations: none of its own; the identity tables are in the baseline schema (`internal/storage/migrations/001_schema.sql`).
- Wiring owned by the composition root: the explicit authority profile, issuer and API audience, the injected HTTP client and cursor codec, runtime versus operator pools, the event appender, extra `Rule`s for actions of later modules (a missing rule denies), and bootstrap invocation.
- Later consumers (logs, streams, source, artifacts, human gates) ask `identity.Authority` with the permissions above; the resource owner remains responsible for scoped lookup, and identity never reads another module's tables.

## Verification

```sh
KODAMA_REQUIRE_POSTGRES=1 go test -race -count=1 ./internal/identity/... ./test/support/oidc/...
go generate ./internal/identity/... && git diff --exit-code -- internal/identity/http/identity.gen.go
make fmt lint test
make test-task TASK=T-010
```

Real-issuer tests use the digest-pinned oauthsonas image in `dev/compose.yaml`, following the provider rule (`DEV_PROVIDER`, default Podman on every OS, explicit Docker override for CI). They skip in the unit layer; `KODAMA_REQUIRE_POSTGRES=1` makes unavailable integration fixtures fail. A skipped acceptance is not evidence.
