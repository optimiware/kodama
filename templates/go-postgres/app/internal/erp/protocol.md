# Sample ERP adjustment protocol

This is the replaceable contract the `internal/erp` adapter speaks. It is a sample, not a vendor
protocol: Optimi makes no claim that any ERP implements it. A real integration provides the same
behavior behind `exceptions.ERP` and is qualified by its own owner.

All requests carry `Authorization: Bearer <tenant-scoped credential>`. The credential is issued per
application tenant and grants only `inventory:adjust` and `inventory:read` for that tenant's data. A
credential for tenant A used under tenant B's path is refused (`403`).

## Apply

`PUT /v1/tenants/{tenantId}/adjustments/{operationKey}`

```json
{"itemRef": "WH1/SKU-9", "quantity": 7, "reason": "recount confirmed", "payloadHash": "<sha256 hex>"}
```

`operationKey` is stable for one reviewed adjustment and is persisted before the first request. The
ERP applies each key at most once per tenant and returns the original outcome on replay.

| Status | Body | Meaning |
| --- | --- | --- |
| 200, 201 | `{"status":"acknowledged","reference":"erp-123"}` | Applied (or already applied); `reference` identifies the ERP record |
| 422 | `{"status":"rejected","reason":"unknown item"}` | Definitively refused; nothing was applied |
| 409 | `{"error":"payload_conflict"}` | The key exists with a different payload; nothing new was applied |
| 401, 403 | | Credential or scope refused; configuration error, nothing was applied |
| 429, 5xx, timeout, connection loss | | **Unknown**: the adjustment may or may not have been applied |

## Lookup

`GET /v1/tenants/{tenantId}/adjustments/{operationKey}`

| Status | Meaning |
| --- | --- |
| 200 | Body as for apply: `acknowledged` or `rejected` |
| 404 | The ERP has no record of this key |
| 429, 5xx, timeout | Unknown; the lookup itself could not establish the outcome |

## Application obligations

- A response that is not an acknowledgment or a rejection leaves the exception **unknown**, never resolved.
- After a lost response or a worker crash the application looks the key up first. Only on `404` does
  it send the same key and payload again; the ERP's key deduplication makes that safe.
- Unknown outcomes are retried with bounded exponential backoff, then left visibly unknown for an
  operator. A lease expiry never authorizes an unrelated replacement write.
- Required scopes: `inventory:adjust`, `inventory:read`. Outgoing endpoint: the configured `erp.baseUrl` only.
