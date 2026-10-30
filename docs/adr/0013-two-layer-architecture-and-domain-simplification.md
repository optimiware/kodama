---
status: accepted
---

# ADR 0013: Two-layer functional architecture and domain simplification

**Date:** 2026-10-30
**Status:** Accepted
**Supersedes:** [ADR 0004](0004-logical-workspaces.md), [ADR 0005](0005-git-backed-workspaces.md), [ADR 0012](0012-container-sdlc-and-artifact-releases.md); simplifies [ADR 0002](0002-postgres-state-and-events.md), [ADR 0010](0010-run-workspace-services-builders.md), and [ADR 0011](0011-internal-tenant-git-service.md).

## Context

Over successive specification and autonomous implementation passes, Kodama's design drifted from its founding vision—an elegant, Drone CI-inspired container pipeline kernel paired with an autonomous agentic software factory—into a 74-table PostgreSQL schema and 30+ internal packages.

Specifically, complexity accumulated in five areas:
1. **Pre-release backwards compatibility:** The specification carried multiple pipeline formats (`v1alpha1`, `v1alpha2`, `v1alpha3`), dual workspace models (`attempt_private` vs `run_shared`), and dual Git models (external S3 bundle-publisher CAS vs internal two-phase `reference-transaction` SQL journals) despite the repository still being pre-release.
2. **Satellite tables for every subsystem:** Events grew to 12 SQL tables (quarantine, replay, recovery fences, dedup boundaries); secrets/variables grew to 8 tables; delivery/gates/ChatOps grew to 11 tables; execution grew to 11 tables; AI/MCP grew to 11 tables.
3. **Kanban and governance ceremony:** Work tracking accumulated Kanban WIP capacity locks, Lean/Kaizen SQL flow-interval views, `content_revision` vs `version` split, and a dedicated canonical-JSON-digest `gates` subsystem, rather than a simple per-project task state machine.
4. **Blurred boundary between core primitives and agentic features:** Agent profiles, design-conformance evidence checks, and QA defect mappers were hardcoded into the core pipeline validator and gate engine.
5. **Knowledge base ceremony:** Engineering guidelines, skills, and application blueprints were modeled as versioned "chassis" and conformance-baseline pins rather than simple Markdown repositories in Git.

Because Kodama is pre-release, we can remove all legacy compatibility shims and cut the design down to a small set of composable primitives.

## Decision

### 1. Two functional layers: Core Kernel (Layer 1) and Agentic Shell (Layer 2)

Kodama is organized into two strict functional layers:

#### Layer 1: The Core ("The Kernel" — Drone CI-inspired primitives)
Layer 1 provides deterministic container orchestration, source hosting, state-machine task tracking, and messaging. It knows nothing about LLM prompts or agent harnesses:
1. **YAML Pipelines (`.kodama/<processname>.yaml`):**
   - Pipelines are defined in Git under `.kodama/<processname>.yaml` or registered via the API / `kodactl` CLI.
   - The entire YAML definition is persisted in a single `definition` column on the `pipelines` table.
   - A pipeline run (`runs`) executes steps (`run_steps`, one row per step per run) inside defined containers mounting authorized **secrets** and a **shared `/workspace`**.
2. **Scoped Container Services (`services`):**
   - Services are support containers (databases, caches, mocks, preview servers) that support pipeline runs and can run at **tenant**, **project**, or **pipeline (run)** scope.
3. **Native Git Service with Gerrit-style hierarchy (`repositories`):**
   - Kodama embeds a native Git HTTP service (`git http-backend` / receive-pack / upload-pack) isolated per tenant.
   - Repositories use a Gerrit-style hierarchical path namespace:
     - `<tenant>/context` → Tenant-wide repository (`project_id = NULL`).
     - `<tenant>/<project>/*` → Automatically associated with `<project>` (e.g., `<tenant>/<project>/context`, `<tenant>/<project>/component-a`, `<tenant>/<project>/deploy`).
4. **Task Tracker with per-project state machine and Role Inbox (`tasks`):**
   - Tasks follow a state machine defined per project in SQL (`projects.task_workflow`, e.g., `plan -> implement -> review -> test -> deploy_preview -> done`, plus `parked`).
   - **No separate Gate subsystem:** Human validations, architectural clarifications, production promotions, and incident triage are modeled uniformly by **parking** a task (`state = parked`, `blocked_by_id`) and creating a blocking task assigned to a **role inbox** (`assigned_role`, e.g., `reviewer` or `operator`). Resolving the inbox task emits a NATS signal that unblocks the parked task and triggers the next pipeline.
5. **Metadata-only Artifacts (`artifacts`) & Encrypted Secrets (`secrets`):**
   - Production artifacts (OCI images, packages, bundles, preview deployments) are published to external registries/services by pipeline steps; Kodama stores **metadata only** (`name`, `kind`, `uri`, `digest`, `metadata`).
   - Secrets are encrypted key-value pairs scoped to tenant or project and mounted into steps and services.
6. **NATS JetStream as the internal message bus:**
   - NATS captures and relays all system signals: Git push/tag/merge triggers, task state transitions, step/agent coordination signals, releases, and promotions.
7. **Unified Access & Simple Permission Model:**
   - Users and operators access Kodama through the **HTTP API**, **MCP server**, or **`kodactl` CLI**, authenticated via external OAuth/OIDC with RBAC.
   - **Permission model:**
     - **User RBAC** (`memberships`) defines what users can configure or operate (projects, pipelines, secrets, tasks RW, role-inbox resolution).
     - **Project-space runtime access:** Agents and steps executing inside a project automatically have access to what has been provisioned and configured in that project space (repositories, mounted secrets, services, tasks, knowledge base, and gateways).

#### Layer 2: The Agentic Software Factory ("The Shell over the Kernel")
Layer 2 complements Layer 1 like a shell over a kernel without changing or interfering with Layer 1's core design. Agentic work is executed in two ways, both reusing Layer 1 container steps:
- **Mode A — External Agent Harnesses in Container Steps:**
  - Ordinary container steps running `opencode`, `claude-code`, `pi`, or other coding harnesses with mounted secrets and the shared `/workspace`, configured directly in `.kodama/<processname>.yaml`.
- **Mode B — Native Kodama Agent Runtime (`kodama agent`):**
  - Container steps running an image with the `kodama` binary runtime and native access to Layer 1 features.
  - Uses the **OpenAI-compatible AI Gateway**, **MCP Gateway**, `agent_sessions`, and embedded tools/skills.
  - Specialized for fine-grained software factory steps: planning, decision-making, filtering, guardrails, validation gates, embedded skills, Git operations, task state transitions, and knowledge-base queries.

### 2. Git-native Knowledge Base Layer

Skills, context, blueprints, and engineering guidelines are stored as Markdown in the native Git service using Gerrit-style repository conventions:
- `<tenant>/context` — Tenant-wide context, security rules, global skills, and architectural guidelines.
- `<tenant>/<project>/context` — Project-specific architecture, domain glossary, and conventions.
- `<tenant>/<project>/<component>` — Component source and component-local instructions.
- `<tenant>/<project>/deploy` — Deployment manifests, environment topology, and promotion rules.

This requires zero dedicated SQL tables and allows both humans and agents (Mode A and Mode B) to read, branch, and update knowledge using standard Git operations.

### 3. Simplified Core Database Schema (15 Tables)

The target schema has 14 domain tables plus required audit/outbox: 15 product tables with tenant RLS isolation. The original total of 14 omitted `audit_events` from its count:

```text
tenants
 ├── memberships          (user_id, tenant_id, project_id?, role)
 ├── repositories         (tenant/context at tenant level; tenant/<project>/* auto-linked to project)
 ├── secrets              (tenant- or project-scoped encrypted secrets)
 ├── services             (tenant-, project-, or pipeline-scoped container services)
 ├── mcp_servers          (tenant- or project-scoped MCP server registrations)
 ├── audit_events         (lightweight append-only audit & NATS outbox)
 └── projects             (includes task_workflow state-machine definition)
      ├── tasks           (state machine + blocked_by_id + assigned_role inbox)
      ├── pipelines       (name + whole .kodama/<process>.yaml definition)
      └── runs            (pipeline execution instance)
           ├── run_steps       (one row per step in a run)
           ├── artifacts       (metadata for external artifacts)
           └── agent_sessions  (native Kodama agent session state, messages, and token usage)
```
*(Plus `users` for OAuth principal mapping.)*

## Consequences

- **Reduction in surface area:** The Phase 2 schema has 15 tables. Phase 3 removes the legacy runtime/deployment consumers and wires the new contracts; schema replacement alone does not complete those removals.
- **Maximum autonomy by default:** Pipelines chain naturally via NATS signals and task state transitions (`plan -> implement -> review -> test -> deploy_preview`), pausing for humans only when a task is parked behind a role-inbox task (such as production promotion or incident decisions).
- **Clean implementation roadmap:**
  - **Phase 1:** Update specifications, contracts, book chapters, and schema documentation in `docs/`.
  - **Phase 2:** Replace the PostgreSQL schema and domain models with the 15-table baseline.
  - **Phase 3:** Consolidate `internal/` packages around the two functional layers and `kodactl`.

## Phase 2 persistence refinement

The [schema implementation](../implementation/database-schema.md) stores one complete JSON pipeline document, with immutable copies on runs. Repository paths are derived from tenant/project keys and relative names. SQL validates nullable service ownership, same-project references and run/step attribution; runtime grants protect immutable input/evidence fields.

Project workflows define all business and inbox state names. Parking keeps the business state and records `parked_at`, `blocked_by_id`, `resume_state`. This refines the earlier shorthand `state=parked`. Resolution records an explicit outcome: rejection does not resume a promotion. One state-to-pipeline trigger map supports initial entry and same-state resumption. These details are implemented in `internal/tasks`, with a shared memory/PostgreSQL contract.

An existing database fails the new baseline's checksum check; the runner does not delete or transform it. New-schema server activation requires Phase 3 adapter wiring. See the [Phase 2 handoff](../implementation/simplification-phase-2.md).
