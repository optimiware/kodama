# Inventory exceptions: specification

This is the authoritative behavior of the application. Requirement IDs (`INV-nn`) are what tests, skills
and delivery evidence cite. Decisions that the blueprint made on the owner's behalf are marked **Assumption**
and are confirmed or replaced by the application owner.

## Purpose

Warehouse staff find gaps between the ERP's balance and what they count. A reviewer decides the correct
quantity and requests an ERP adjustment. The ERP owns balances; this application owns the review,
the local audit trail and the integration intent.

## Tenancy and identity

- **INV-01** An application tenant owns its memberships and exceptions. Every tenant-owned row carries
  `tenant_id`; keys and references are tenant-composite; row-level security is forced for a runtime role that
  owns nothing and cannot bypass it.
- **INV-02** Identity is verified from the application's own OIDC provider: issuer, signature, audience, expiry
  and a non-empty subject. Role, tenant and group claims are ignored.
- **INV-03** Authority comes only from the subject's **current** membership in the **requested** tenant,
  checked on every request. A tenant in a URL is a request for scope, not authority. A subject with no
  membership gets the same `404 tenant_not_found` as a tenant that does not exist.
- **INV-04** Roles: `viewer` reads; `reviewer` and `admin` read, create and request resolutions; `worker` is a
  service role that reads and processes ERP intents. A human membership cannot run a worker and a worker
  cannot act as a human. **Assumption:** `admin` has no extra permission yet; membership administration is
  provisioning, not an application feature.
- **INV-05** The application's runtime depends only on its own database, identity provider and ERP. It does not
  call, and is not blocked by, any software factory or hosting platform.

## Exception lifecycle

States: `open → pending → resolved | failed | unknown`; `unknown → resolved | failed` after reconciliation.
`resolved` and `failed` are terminal. **Assumption:** a failed exception is not reopened; a new exception is opened.

- **INV-06** An exception has a positive gap: the ERP and counted quantities are integers within
  0..1,000,000,000 and differ.
- **INV-07** Requesting a resolution needs a reviewer or admin, a quantity, a reason (1–500 bytes), an
  `Idempotency-Key` and the expected version (`If-Match`). It moves `open → pending` and records, in one
  transaction, the new state, the durable ERP intent, the idempotency record and an audit entry.
- **INV-08** The same key with the same content returns the existing operation. The same key with different
  content is `idempotency_conflict`. A different key on a non-open exception is `invalid_state`; a stale version
  is `version_conflict`. Concurrent duplicates produce one intent and one ERP effect.
- **INV-09** Every transition is attributed to the verified subject (never to request content) in an
  append-only audit history.

## Resolution and the ERP

- **INV-10** The operation key (`inv-adj-<exception id>`) is persisted before the first ERP request and is
  reused for every retry and lookup. The ERP is called outside any SQL transaction.
- **INV-11** An exception becomes `resolved` only after the ERP acknowledges the adjustment and returns its
  reference. `pending` and `unknown` are never reported as resolved.
- **INV-12** A definite ERP refusal moves the exception to `failed` with the ERP's reason.
- **INV-13** If the outcome cannot be established (timeout, connection loss, 5xx, unavailable lookup) the
  exception becomes `unknown` with an explanation. The worker reconciles by looking up the **same** key; only
  when the ERP has no record does it send the same key and payload again. Retries are bounded with exponential
  backoff; afterwards the outcome stays visibly `unknown` for an operator.
- **INV-14** A worker lease that expires (crash, restart) lets another claim of the same intent reconcile it. It
  never authorizes a different operation key or payload.
- **INV-15** The ERP credential is scoped to one application tenant; a worker serves exactly one tenant under
  its own service identity, re-checked every cycle, and never uses a human's identity.

## Interfaces and operations

- **INV-16** The HTTP interface is versioned, documented in [`api/openapi.yaml`](../api/openapi.yaml), returns
  machine-readable errors with a retry hint and a correlation ID, uses optimistic concurrency, and bounds list
  pages with tenant- and filter-bound signed cursors.
- **INV-17** Configuration is typed and validated at start-up; every problem is reported together. Secrets are
  only referenced by protected file paths. The migration identity is separate from the runtime identity.
- **INV-18** Liveness depends on nothing; readiness reports database, schema version and identity provider.

## Acceptance scenarios

| ID | Scenario | Covers |
| --- | --- | --- |
| `blueprint_go_tenant` | Verified identity plus current membership; cross-tenant denial; scoped workers; works with factory endpoints unreachable | INV-01…05, 15, 18 |
| `erp_duplicate_resolution` | One intent and one effect under concurrent duplicates; payload conflicts; acknowledgment loss and worker restart reconcile; unresolved outcomes stay visible | INV-07…14 |
| `blueprint_go_delivery` | Quality, build, preview, stage and e2e scripts produce identified, verifiable evidence | operations |
| `runtime_config_promotion` | Stage uses the preview's image digest with distinct explicit configuration | operations |
