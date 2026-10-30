# State capability

`internal/state` is the server-side generic persistent state capability. It is
not a deployment module and does not know any provider or plugin schema.

The memory and PostgreSQL adapters share the conditional contract. The
PostgreSQL migration includes forced tenant RLS, binding-relative key checks, a
per-project entry/byte quota trigger and live lifecycle/RLS tests. Certificate
retention joins and audit/outbox events remain integration checkpoints.

## Rules

- Scope is derived from trusted runtime authority, never a caller body.
- Request keys and list prefixes are relative to the authorized binding prefix;
  the server resolves the absolute storage key and returns relative keys/cursors.
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
