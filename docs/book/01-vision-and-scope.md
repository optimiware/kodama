# 1. Vision and scope

Kodama is an API-first software factory built by combining simple, powerful primitives ([ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md)).

Optimi Platform is the company core; Kodama is its tenant agent and SDLC layer. The [platform integration contract](../specs/platform-integration.md) separates core authority from SDLC ownership while keeping standalone installations self-contained.

## Four functional layers

### Layer 1: The Pipeline Runtime kernel
Inspired by Drone CI, Layer 1 provides deterministic execution:
1. **YAML pipelines (`.kodama/<processname>.yaml`):** Define container steps, waits, conditions, and support services. The whole YAML definition is persisted once in PostgreSQL.
2. **Runs and steps:** A run executes one pipeline snapshot against pinned inputs. Steps share a disposable `/workspace`; a durable wait pauses the same run without holding a worker.
3. **Container services:** Support containers (databases, caches, mocks, preview servers) run at **tenant**, **project**, or **pipeline (run)** scope.
4. **Declared secrets:** Steps request named secret references; the shell resolves and delivers them outside `/workspace`.
5. **Recovery and cancellation:** Stable workload identity, confirmed cleanup, deadlines, and conservative handling of unknown outcomes.

The kernel has no dependency on Git hosting, tasks, SQL/NATS/HTTP clients, identity, or agents.

### Layer 2: The native shell
The shell owns native services around the kernel:
1. **Native Git service:** Internal hosting with a Gerrit-style path hierarchy (`<tenant>/context`, `<tenant>/<project>/<repo>`) for checkouts, branches, merges, releases, and Markdown knowledge bases.
2. **Project pipeline registry:** Project-unique pipelines, Git indexing, triggers, and admission.
3. **Task tracker and wait-based Role Inbox:** A task selects its pipeline and records execution progress. When human input is required, the pipeline waits; the inbox lists tasks whose current wait expects the caller's role.
4. **Metadata-only artifacts:** Production artifacts are published to external registries or services; Kodama records only their metadata, digest, and provenance.
5. **NATS system bus:** Carries committed events, Git triggers, wait signals, execution observations, and monitoring subscriptions.
6. **Unified access and RBAC:** Users and operators access Kodama via the HTTP API, Kodama MCP server, or `kodactl` CLI, authenticated with external OAuth/OIDC and governed by per-tenant/project RBAC.

### Layer 3: Agentic services
Agentic work runs through the same container-step primitive:
- **External harnesses:** `opencode`, `claude-code`, `pi`, and other harnesses run in container steps with declared secrets and the shared workspace.
- **Native Kodama Agent runtime:** A container step containing the `kodama` runtime (`kodama agent`), backed by an OpenAI-compatible AI Gateway, external MCP proxy, session persistence (`agent_sessions`), and access to shell operations (Git, task progress, knowledge base, filtering, guardrails, and embedded skills).

### Layer 4: External behavior
Pipeline YAML, scripts, container images, hooks, and delivery/QA/notification recipes implement specialized behavior without changing the kernel or shell.

## Principles

- **One process definition:** A project owns project-unique pipelines; a task selects one. Business phases are pipeline steps, not a second workflow engine.
- **Simple primitives, powerful composition:** Pipelines, services, tasks, and Git compose through explicit contracts. The current database inventory is in the [schema](../implementation/database-schema.md).
- **Maximum autonomy, minimal human waits:** Agents drive the SDLC autonomously; humans intervene through explicit pipeline waits for architectural clarifications, production promotions, or incident repair.
- **Simple permission model:** User RBAC governs what users can configure (pipelines, secrets, tasks RW); agents and steps automatically have access to what is provisioned and configured in their project space.
- **Git-native knowledge base:** Context, skills, blueprints, and deployment guidelines live as Markdown in Git (`<tenant>/context`, `<tenant>/<project>/context`, `<tenant>/<project>/<component>`, `<tenant>/<project>/deploy`).
