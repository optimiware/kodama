# State capability

`internal/state` is the server-side generic persistent state capability. It is
not a deployment module and does not know any provider or plugin schema.

The in-memory adapter and HTTP handler implement the first vertical slice. A
PostgreSQL adapter, migration, quota policy and artifact-retention join are the
next implementation checkpoint; the memory adapter is held to the same
conditional semantics but is not SQL/RLS evidence.

## Rules

- Scope is derived from trusted runtime authority, never a caller body.
- Keys are relative and remain inside the authorized binding prefix.
- A read returns an opaque JSON value and an ETag.
- Create uses `If-None-Match: *`; update/delete require `If-Match`.
- An ETag contains a generation and revision, so deleting and recreating a key
  cannot accept a stale writer's token.
- Values and references are bounded. Large content belongs in artifacts.
- Artifact references are explicit retention roots; arbitrary artifact IDs in a
  value are not retention authority.
- No unconditional writes, watch streams, query language or automatic TTL
  deletion exist in this slice.

## HTTP shape

```text
GET    /runtime/v3/attempts/{attemptId}/state/{binding}/entries?key=...
GET    /runtime/v3/attempts/{attemptId}/state/{binding}/entries?prefix=...&cursor=...&limit=...
PUT    /runtime/v3/attempts/{attemptId}/state/{binding}/entries?key=...
DELETE /runtime/v3/attempts/{attemptId}/state/{binding}/entries?key=...
```

The `BindingResolver` implemented by composition authenticates the attempt and
returns the namespace, prefix, read/write authority. The state package never
trusts a namespace supplied by the plugin.
