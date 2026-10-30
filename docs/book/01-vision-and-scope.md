# 1. Vision and scope

Kodama is an API-first software factory built by combining simple, powerful primitives ([ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md)).

Optimi Platform is the company core; Kodama is its tenant agent and SDLC layer. The [platform integration contract](../specs/platform-integration.md) separates core authority from SDLC ownership while keeping standalone installations self-contained.

## Two functional layers

### Layer 1: The Core ("The Kernel")
Inspired by Drone CI, Layer 1 provides deterministic building blocks:
1. **YAML pipelines (`.kodama/<processname>.yaml`):** Define container steps and support services. The whole YAML definition is persisted in PostgreSQL.
2. **Scoped container services:** Support containers (databases, caches, mocks, preview servers) that run at **tenant**, **project**, or **pipeline (run)** scope.
3. **Container step execution:** Pipeline runs execute step commands in defined containers mounting authorized **secrets** and a **shared `/workspace`**.
4. **Native Git service:** Internal Git hosting with a Gerrit-style path hierarchy (`<tenant>/context`, `<tenant>/<project>/<repo>`) for checkouts, branches, merges, releases, and Markdown knowledge bases.
5. **State-machine task tracker & Role Inbox:** PostgreSQL task tracker where each project defines its state machine (`plan -> implement -> review -> test -> deploy_preview -> done`). When human input or promotion approval is required, the task is **parked** (`state: parked`) and blocked by a decision/promotion task sent to a **role inbox**.
6. **Metadata-only artifacts:** Production artifacts are published to external registries or services; Kodama records only their metadata, digest, and provenance.
7. **NATS message bus:** Captures and relays Git triggers, task state transitions, step/agent coordination messages, releases, and promotions.
8. **Unified access & RBAC:** Users and operators access Kodama via the HTTP API, MCP server, or `kodactl` CLI, authenticated with external OAuth/OIDC and governed by per-tenant/project RBAC.

### Layer 2: The Agentic Software Factory ("The Shell over the Kernel")
Layer 2 complements Layer 1 like a shell over a kernel without changing or interfering with Layer 1's core design. Agentic tasks run in two modes over the same container-step primitive:
- **Mode A (External harnesses in container steps):** Run `opencode`, `claude-code`, `pi`, and other popular harnesses inside container steps with mounted secrets and the shared `/workspace`, configured through `.kodama/<processname>.yaml`.
- **Mode B (Native Kodama Agent runtime):** Run a container step containing the `kodama` runtime (`kodama agent`), backed by an OpenAI-compatible AI Gateway, MCP Gateway, session persistence (`agent_sessions`), and native access to Kodama's core capabilities (Git ops, task transitions, knowledge base, filtering, guardrails, validation gates, and embedded skills).

## Principles

- **Simple primitives, powerful composition:** A small 15-table data model (14 domain tables plus audit/outbox) where pipelines, services, tasks, and Git compose cleanly.
- **Maximum autonomy, minimal human gates:** Agents drive the SDLC autonomously from planning to preview; humans intervene primarily via the role inbox for architectural clarifications, production promotions, or incident repair.
- **Simple permission model:** User RBAC governs what users can configure (pipelines, secrets, tasks RW); agents and steps automatically have access to what is provisioned and configured in their project space.
- **Git-native knowledge base:** Context, skills, blueprints, and deployment guidelines live as Markdown in Git (`<tenant>/context`, `<tenant>/<project>/context`, `<tenant>/<project>/<component>`, `<tenant>/<project>/deploy`).
