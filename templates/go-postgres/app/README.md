# Inventory exceptions (Go + PostgreSQL)

A standalone application generated from the Kodama Go/PostgreSQL blueprint. It reviews inventory gaps between
an ERP and physical counts and requests ERP adjustments with durable, idempotent, reconcilable intent.

It is an independent Go module. It imports nothing from a software factory or hosting platform, and it
serves requests with no factory available.

| Read | For |
| --- | --- |
| [CONTEXT.md](CONTEXT.md) | domain terms |
| [docs/specification.md](docs/specification.md) | authoritative behavior and requirement IDs |
| [docs/architecture.md](docs/architecture.md) | where code belongs |
| [docs/operations.md](docs/operations.md) | deployment, migrations, delivery contract and recovery |
| [AGENTS.md](AGENTS.md) | instructions for coding agents |
| [api/openapi.yaml](api/openapi.yaml) | the HTTP interface |

## Commands

```bash
go test ./internal/... ./cmd/...                      # no services needed
export INVENTORY_TEST_ADMIN_URL="<superuser PostgreSQL URI of a disposable server>" INVENTORY_REQUIRE_POSTGRES=1
go test ./migrations/... ./internal/exceptions/postgres/... ./test/acceptance/...   # real PostgreSQL

go run ./cmd/inventory migrate --config config/migrate.example.json   # owner identity
go run ./cmd/inventory serve   --config config/api.example.json
go run ./cmd/inventory worker  --config config/worker.example.json
```

The example configuration references secret files; see `docs/operations.md`.
