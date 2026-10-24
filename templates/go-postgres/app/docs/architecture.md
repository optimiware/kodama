# Architecture

One Go module, vertical packages, explicit wiring.

```text
cmd/inventory      serve | worker | migrate | seed        (flags, signals, exit codes only)
cmd/delivery       build-verify | deploy | e2e             (delivery helper behind ci/*.sh)
internal/app       composition root: the only place adapters are chosen and constructed
internal/tenancy   Access, roles, membership port; postgres/ adapter
internal/identity  OIDC verification (go-oidc)
internal/exceptions   entity, use cases, ports it consumes (Store, ERP); postgres/ and http/ adapters
internal/erp       HTTP adapter for the exceptions.ERP port (+ protocol.md)
internal/config    typed configuration, secret file references
internal/dbtx      tenant-bound transactions
internal/delivery  factory callbacks, environment snapshot bridge, manifests, kubectl adapter
migrations         forward-only SQL and the migration runner
api/openapi.yaml   the application's interface
```

Rules that tests enforce:

- The domain (`exceptions`, `tenancy`) imports no SQL, HTTP or provider package.
- Ports are small, owned by their consumer, and each has a real adapter and an in-memory double with the
  same documented behavior (`exceptionstest`, `erpfake`).
- All SQL goes through `dbtx.InTenant`, which sets the tenant for the transaction only, so a pooled connection
  never carries a tenant to its next user.
- Lock order is exception row, then intent row. ERP calls never happen inside a transaction.
- The application imports nothing from a software factory or hosting platform (`ci/quality.sh` checks it).

State transitions live in the entity (`exception.go`); persistence only loads, applies the entity's decision
and saves it with its intent, idempotency record and audit entry atomically.

Deviations from these conventions belong in `docs/adr/`.
