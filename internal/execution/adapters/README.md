# internal/execution/adapters

Bridges from execution's consumer-owned ports to its peers. T-097 adds the two production joins below; nothing binds them yet (`internal/app` is T-101's, so they are unregistered until then). Neither is edited into the delivery service or the execution store.

## Transaction-bound delivery reader (`txdelivery.go`)

`BindDeliveryEnvironments` is the binder `execpg.Peers.Environments` (`execpg.EnvironmentBinder`) takes. For each admission transaction it joins `NewTxEnvironments` to a `TxDeliveryReader` over that transaction:

| Read | Source |
| --- | --- |
| current authority | `deliveryadapters.Access` over `identitypg.NewTxAuthority(tx)`; takes the user's authorization revision `FOR SHARE` until the admission ends |
| retained environment snapshot | one `SELECT … FROM environment_snapshots` in the same transaction |
| deployment of a cleanup run | one `SELECT … FROM deployments` in the same transaction |

Every statement carries tenant and project in its `WHERE` clause and the row's own scope is checked again. No network call is made and no row lock is taken beyond the identity revision share, so the execution lock order is unchanged. A work run needs the actor's current `deployment:manage`; a cleanup run needs its own deployment's cleanup to be requested and not complete (`checkRetained`).

| Condition | Result |
| --- | --- |
| revoked or stale authority (`ErrDenied`) | `execution.ErrDenied`; no snapshot is read |
| hidden actor, missing, foreign, altered or invalid snapshot | `ErrAdmissionRefused` |
| database, authority or decode failure | `ErrUnavailable` (never a pass) |
| binder that cannot be joined to a transaction | closed: every binding is `ErrUnavailable` |

Exact credential versions are not revalidated here: the secrets owner revalidates them when the admission registers its pins in the same transaction. `test/integration/delivery/stack/txdelivery.go` is the earlier test-local reference.

## Runtime secret route (`secretroute.go`)

`SecretRoute.Resolve(ctx, attempt, bearer, req, requestID)` serves the frozen `resolveSecret` operation. It authenticates with the runtime authority (`runtimeauth.CheckSession` for `secret.resolve`, in a tenant transaction), reads the durable grant's immutable pinned context (digest-checked, identity-checked), and hands the secrets module's `Resolver` a per-request `AttemptAuthority` that repeats that check before and after the decryption. The tenant and project come from the grant, never the request.

* Only the pinned environment's credentials resolve. The permitted set is the context's `Secrets` that equal, exactly (reference, version, name and delivery), a binding of the environment's retained configuration snapshot, read by the context's `Environment` pin (`EnvironmentSnapshots`, i.e. `delivery/postgres.Store.GetSnapshot`) in the grant's scope. A step-only binding, any other reference or version, and every reference of an attempt that pins no environment are `forbidden` and never reach the store. A missing, foreign, altered or other-pin snapshot is `not_found` and resolves nothing; an unreadable snapshot is `unavailable`.
* Repetition follows the secrets module: a second resolution of a declared version in the same attempt is a new, separately authorized and audited release of the same pinned value; it is not cached and not refused. It is refused as soon as the attempt is cancelled, revoked, past its action ceiling or on a retired signing generation.
* Failures are `*runtimev1.ContractError`: invalid credential, `attempt_revoked`, `forbidden`, `not_found` (foreign or unknown reference), `unavailable` (outage, uncertain audit, key failure). No value, reference or cause appears in an error.

T-101 mounts the route on `POST /runtime/v1/attempts/{attemptId}/secrets/resolve` and enables the `runtime-secret-resolution` capability.

## Tests

Unit tests over port doubles (a scripted transaction, the in-memory runtime authority and a controlled version source): `Test_delivery_tx_environments`, `Test_runtime_secret_route` (its fixture pins both environment credentials and a step-only secret). Real PostgreSQL, RLS and rollback evidence belongs to T-038/T-067.

```sh
make check-go PKGS="./internal/delivery/... ./internal/gates/... ./internal/execution/..."
```
