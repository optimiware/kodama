---
name: inventory-domain
description: Change inventory-exception behavior (states, resolution rules, ERP outcomes, HTTP interface) without breaking tenancy or ERP uncertainty rules.
---

Use for changes under `internal/exceptions`, `internal/erp`, `internal/tenancy` or `api/openapi.yaml`.

1. Read [specification](../../../docs/specification.md) sections "Tenancy" and "Resolution and the ERP", and the
   requirement you are serving.
2. Change the entity (`internal/exceptions/exception.go`) first; add the transition to the table in
   `exception_test.go` including its rejected neighbours.
3. Extend a port only when a use case needs it; update the in-memory double in `exceptionstest` and the
   PostgreSQL adapter together, and keep their behavior identical.
4. New external outcomes need a visible state or detail, a bounded retry rule and a reconciliation path. A lost
   response must never become a second effect.
5. Update `api/openapi.yaml` with the code; `openapi_test.go` checks the routes and error codes.
6. Evidence: `go test ./internal/... ./test/acceptance/...` with a PostgreSQL admin URL for the real-database tests.
   Name the candidate commit and report unresolved deviations in the conformance report (`AGENTS.md`).
