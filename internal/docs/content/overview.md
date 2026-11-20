# Kodema Overview

Kodema is an API-first, multi-tenant software factory that runs coding agents and container pipelines on Kubernetes (or local Podman).

## Core model

> A **pipeline** is an ordered list of **actions**; an action runs a container or requests an answer; an **execution** runs a pipeline once; a **task** is a conversation that starts executions.

Hierarchy:

- **Tenant** — isolated organization (row-level security, dedicated derivation key, isolated Kubernetes namespace/network policy, isolated Git workers).
- **Project** — unit of visibility and configuration inside a tenant (pipelines, tasks, executions, resource bindings, secrets, service accounts).
- **Resource binding** — registered external resource (such as a Git repository) owned and enforced by a service.
- **Pipeline** — named YAML definition managed strictly through the API (`pipeline.apply`, `pipeline.update`, `pipeline.delete`). Kodema never auto-syncs pipelines from Git pushes.
- **Execution** — one immutable run of a pipeline (stored or inline YAML). Admission snapshots exact YAML bytes, resolves images to immutable digests, and captures inputs and any task conversation.
- **Action** — one step in an execution: either a **container action** or a **request action** (`role/<role>` or `service/<id>`).
- **Task** — persistent conversation with title, Markdown description, messages, userland JSON `metadata`, selected pipeline/inputs, and execution history. States: `open`, `waiting`, `done`, `cancelled`.
- **Service account** — project-scoped automation principal (`kodsvc.*`) with non-admin roles.

## Identity and roles

Five roles exist per tenant/project:

- `admin` — tenant/project configuration, memberships, resource bindings, service accounts. **Grants no review authority.**
- `engineer` — repositories, pipelines, tasks, and starting executions.
- `reviewer` — answering `role/reviewer` requests.
- `operator` — runtime operations, answering `role/operator` requests, retrying failed tasks.
- `viewer` — read-only project operational visibility.

Every project member and enabled project service account can read non-secret operational state (`execution.*`, `task.*`, `pipeline.*`, `resource.binding.*`, `repository.list`, `secret.metadata.*`, `service.list`, `event.read`, `event.subscribe`). Secret values are never readable through management APIs.

## Projects, repositories, and pipelines

- **Projects:** `kodema project create <id> [--name <name>] [--copy-members-from <project>]` (or MCP `project_create`) creates a project in the caller's tenant and initializes creator or copied project memberships automatically; `kodema project list` (or MCP `project_list`) defaults `tenant_id` from the caller's authenticated tenant.
- **Repositories:** `kodema repo create <name> [--project <id>]` (or MCP `repository_create`) provisions a Git repository over HTTP and registers its project resource binding in one step; `kodema repo list [--project <id>]` (or MCP `repository_list`) lists registered Git repositories.
- **Pipelines:** `kodema pipeline apply <file-or-dir>` (deriving pipeline names from filenames without `.yaml`/`.yml`), `kodema pipeline list`, `kodema pipeline show <name|id> [--yaml]`, and `kodema pipeline delete <name|id>`.

## API and MCP conventions

- REST (`/api/v1/...`) and MCP (`/mcp`) dispatch through one shared router and authorizer with 1:1 operation parity.
- In MCP, tool names use underscores (for example `task_create_and_start`) and accept either flat top-level arguments or the wrapped `{"input": {...}, "expected_version": ..., "idempotency_key": ...}` envelope.
- State-changing operations require caller-supplied `idempotency_key` (retries with identical input converge; changed input fails with `changed_idempotency_content`).
- Versioned mutations require `expected_version` (stale versions fail with `version_conflict`; re-read the entity and retry).
- Discover every operation offline with `kodema call --list` and inspect its input JSON Schema with `kodema call --schema <operation>`.

## Further reading

- `kodema://docs/pipelines` (`kodema docs pipelines`) — pipeline YAML schema and execution rules
- `kodema://docs/workload` (`kodema docs workload`) — container action contract, `/kodema/*` files, env vars, LLM proxy, toolbox
- `kodema://docs/git-rules` (`kodema docs git-rules`) — Git service, repository conventions, and `admin/conf/git.conf`
- `kodema://docs/workflows` (`kodema docs workflows`) — approvals, task handoffs, delivery, and operator recovery
- `kodema://docs/context` (`kodema docs context`) — shared company/project Git context repos and skills
