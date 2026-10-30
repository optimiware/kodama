# Secrets and configuration

**Scope:** Layer 2 encrypted secret custody (`secrets`) and configuration, with declared secret references delivered by the Layer 1 kernel ([ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md)).

## Model

Under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md), configuration is kept simple:
1. **Plaintext configuration:** Non-secret environment variables and settings live directly in `.kodama/<processname>.yaml` (`env:`) or in `projects.settings` (JSONB). No separate `variables` or `variable_versions` tables are needed.
2. **Encrypted secrets (`secrets`):** Confidential credentials (API keys, registry tokens, deployment credentials, database passwords) are stored in the single `secrets` table (`id`, `tenant_id`, `project_id`, `name`, `encrypted_value`, `key_id`, `updated_at`).
   - A secret with `project_id IS NULL` is **tenant-scoped** (available to all projects in that tenant).
   - A secret with `project_id` set is **project-scoped** (overrides a tenant secret of the same name within that project).

## Encryption

Secret values are encrypted using AES-256-GCM with keys wrapped by the installation's key provider:
- `production`: Cloud KMS (AWS KMS, GCP Cloud KMS, or Scaleway Key Manager).
- `test`: Local 32-byte keyfile.
- `development`: Ephemeral in-memory key wrapper.

Secret plaintext is write-only through the API/CLI (`kodactl secrets set`) and is never returned in API responses, events, or logs.

The `secrets` row stores ciphertext, key identity, envelope metadata, a logical version and optional revocation time. Runs bind the selected version. Rotation replaces the current value: a run requesting an unavailable older version must fail, never silently receive the replacement. Historical value storage is outside this reduced schema. The kernel receives only declared references and a resolver/materialization port; custody and delivery adapters are owned by the shell.

## Runtime delivery

### Workload delivery

Steps and services in `.kodama/<processname>.yaml` declare which secrets they need (`secrets: [NAME_A, NAME_B]`):
- **Project-space authorization:** Because steps and services run inside a project space, they may mount any secret belonging to that project or shared at the tenant level.
- **Injection:** Secrets are injected into the step or service container as environment variables or mounted files under `/kodama/secrets/<name>` outside `/workspace`.
- **Gateway mediation (Layer 3):** For Mode B (`kodama agent`) and external MCP servers (`mcp_servers.secret_id`), the AI Gateway and external MCP proxy can resolve the secret server-side so upstream provider tokens never need to enter the container environment.

## Requirements

| ID | Requirement |
| --- | --- |
| VS-01 | Secrets MUST be stored encrypted in `secrets` using the environment's KMS or keyfile provider and scoped to tenant or project. |
| VS-02 | API, MCP, and `kodactl` reads MUST return secret metadata only, never plaintext. |
| VS-03 | Steps and services MUST receive only the secrets declared in their configuration within their authorized tenant/project space. |

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| VS-A01 | Setting a project secret encrypts it at rest; listing secrets returns metadata only | VS-01, VS-02 |
| VS-A02 | A pipeline step and a support service mount declared project secrets while cross-tenant secret access is denied | VS-01, VS-03 |
