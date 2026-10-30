# Kodama domain language

Kodama is a two-layer software factory ([ADR 0013](docs/adr/0013-two-layer-architecture-and-domain-simplification.md)):
- **Layer 1 (Core Kernel):** Drone CI-inspired container pipelines, scoped container services, native Git service, state-machine task tracker, metadata-only artifacts, encrypted secrets, and NATS message bus.
- **Layer 2 (Agentic Software Factory):** External container harnesses and the native `kodama agent` runtime operating as a shell over the core primitives.

## Tenancy and permissions

**Platform core**: Optimi's company-wide authority for tenant identity, entitlements, and core operations ([platform integration](docs/specs/platform-integration.md)).

**Tenant**: An isolated organization owning projects, memberships, tenant-level services, and a tenant-level knowledge base (`<tenant>/context`).
_Avoid_: Account, customer workspace.

**Project**: A tenant-owned space grouping repositories (`<tenant>/<project>/*`), pipelines, tasks, services, secrets, and artifacts.

**Membership**: A user's role binding (`admin`, `engineer`, `reviewer`, `operator`, `viewer`) at tenant or project scope, authenticated via external OAuth/OIDC.

**Project-space permission model**: User RBAC controls what a person can configure on Kodama (projects, pipelines, secrets, tasks RW, inbox resolution). Agents and pipeline steps executing inside a project automatically have access to what has been provisioned and configured in that project space.

## Layer 1: Core primitives

**Repository**: A Git repository hosted in Kodama's native tenant Git service using a Gerrit-style hierarchical path (`<tenant>/context` or `<tenant>/<project>/<name>`). Repositories under `<tenant>/<project>/*` are automatically associated with `<project>`.

**Knowledge base**: Markdown repositories in the native Git service storing skills, context, blueprints, and engineering guidelines (`<tenant>/context`, `<tenant>/<project>/context`, `<tenant>/<project>/<component>`, `<tenant>/<project>/deploy`).

**Pipeline**: A workflow defined in `.kodama/<processname>.yaml` inside a repository (or registered via API/`kodactl`) and persisted as a whole YAML/JSON definition in PostgreSQL.

**Run**: One execution of a pipeline against a repository commit and optional task, with a shared `/workspace` filesystem.

**Step (`run_step`)**: One container command execution within a pipeline run, mounting the shared `/workspace` and authorized secrets.

**Service**: A container workload (such as PostgreSQL, Valkey, a mock API, or a preview server) that supports pipeline runs and operates at **tenant**, **project**, or **pipeline (run)** scope.

**Task**: A tracked unit of work in PostgreSQL following a state machine defined per project (`plan -> implement -> review -> test -> deploy_preview -> done`, plus `parked`).
_Avoid_: Ticket, card, or separate gate/blocker entities.

**Role inbox**: A query of unresolved tasks (`kind: decision | promotion | incident`) assigned to a role (`assigned_role`). A work task may be parked behind such a request using `parked_at`, `blocked_by_id` and `resume_state`, while retaining its project-defined business state. Approval or an applicable clarification answer can resume work; rejection does not authorize promotion.

**Artifact**: Metadata recorded in PostgreSQL (`name`, `kind`, `uri`, `digest`, `metadata`) pointing to build or release outputs published to external registries or services.

**Secret**: An encrypted key-value credential scoped to a tenant or project and mounted into steps or services as environment variables or files.

**NATS message bus**: The internal NATS JetStream bus that captures and relays Git triggers, task state transitions, step/agent coordination signals, releases, and promotions.

**`kodactl`**: The unified CLI for users, operators, and in-container scripts to interact with the Kodama HTTP API and MCP endpoints.

## Layer 2: Agentic software factory

**Container harness step (Mode A)**: An ordinary Layer 1 container step running an external coding harness (`opencode`, `claude-code`, `pi`) with mounted secrets and the shared `/workspace`, configured through `.kodama/<processname>.yaml`.

**Kodama Agent runtime (Mode B)**: A container step running the native `kodama` runtime (`kodama agent`) with built-in access to Layer 1 capabilities (Git ops, task transitions, knowledge base, Kodama API/MCP) for planning, decision-making, filtering, guardrails, validation gates, and embedded skills.

**Agent session**: The persisted record (`agent_sessions`) of a native `kodama agent` execution, including model usage, messages/transcript reference, and structured outcome.

**AI gateway**: Kodama's OpenAI-compatible HTTP proxy that mediates model calls, injects server-side provider credentials, and tracks token usage per run and session.

**MCP gateway**: Kodama's Model Context Protocol proxy that exposes Kodama's own core operations (tasks, git, pipelines, knowledge base) and registered external `mcp_servers` to users and agents.
