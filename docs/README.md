# Kodama documentation

Kodama is an API-first, autonomous software factory built around two functional layers ([ADR 0013](adr/0013-two-layer-architecture-and-domain-simplification.md)):
- **Layer 1 (The Core Kernel):** Drone CI-inspired YAML pipelines (`.kodama/<processname>.yaml`), shared workspace container steps, scoped container services (`tenant`, `project`, or `pipeline`), a native Gerrit-style Git service, a per-project state-machine task tracker with role-inbox decisions, metadata-only external artifacts, encrypted secrets, and a NATS JetStream message bus.
- **Layer 2 (The Agentic Software Factory):** External coding harnesses (`opencode`, `claude-code`, `pi`) and the native `kodama agent` runtime (with AI Gateway, MCP Gateway, sessions, skills, guardrails, and Git-backed Knowledge Base) operating as a clean shell over the Layer 1 kernel.

## Start here

| I want to... | Read |
| --- | --- |
| Understand the simplified architecture | [ADR 0013](adr/0013-two-layer-architecture-and-domain-simplification.md) and [System architecture](book/05-system-architecture.md) |
| Understand the domain and 15-table data model | [Domain and lifecycles](book/03-domain-and-lifecycles.md) and [Database schema](implementation/database-schema.md) |
| See what works today | [Project status](status.md) |
| Integrate with the API, MCP, or `kodactl` | [Contracts](contracts/README.md) and the [HTTP interface](contracts/http-api.md) |
| Install and operate it | [Installation and configuration](operations/installation-and-configuration.md) |
| Contribute code | [CONTRIBUTING.md](../CONTRIBUTING.md) and the [specifications](#contributors) |

## Reading paths

### Product and domain

1. [Vision and scope](book/01-vision-and-scope.md)
2. [Personas and user journeys](book/02-personas-and-user-journeys.md)
3. [Domain and lifecycles](book/03-domain-and-lifecycles.md)
4. [Autonomous flow and role inbox](book/04-lean-flow-and-governance.md)
5. [Knowledge base and blueprints](book/06-application-blueprints.md)
6. [Delivery roadmap](book/07-delivery-roadmap.md)

Use the [glossary](../CONTEXT.md) for canonical terms.

### Contributors

Start with [ADR 0013](adr/0013-two-layer-architecture-and-domain-simplification.md), [system architecture](book/05-system-architecture.md), [database schema](implementation/database-schema.md), and the [decision register](decisions.md). Then follow the relevant specification:

| Specification | Layer | Main responsibility |
| --- | --- | --- |
| [Identity, tenancy, and RBAC](specs/identity-tenancy-and-rbac.md) | Layer 1 | External OAuth/OIDC, user RBAC, tenant isolation, and project-space workload permissions |
| [Git repositories and knowledge base](specs/git-repositories.md) | Layer 1 | Native Git service, Gerrit-style `<tenant>/<project>/*` hierarchy, `.kodama/*.yaml`, and Markdown knowledge base |
| [Pipeline execution and services](specs/pipeline-execution.md) | Layer 1 | `.kodama/<processname>.yaml`, `pipelines` > `runs` > `run_steps`, and `tenant`/`project`/`pipeline` container services |
| [Workspaces](specs/workspaces.md) | Layer 1 | Shared `/workspace` per pipeline run and direct native Git checkout/push |
| [Task tracker and role inbox](specs/work-management.md) | Layer 1 | Per-project SQL state machine, NATS transition triggers, parked tasks, and role-inbox decisions |
| [Secrets and configuration](specs/variables-and-secrets.md) | Layer 1 | Encrypted tenant/project secrets mounted into container steps and services |
| [Artifacts, releases, and promotions](specs/delivery-and-quality-gates.md) | Layer 1 | Metadata-only external artifacts, release/promotion pipelines, and role-inbox promotion approvals |
| [API, `kodactl` CLI, and MCP](specs/api-cli-and-integrations.md) | Layer 1 & 2 | Unified HTTP API, MCP server, `kodactl` CLI, and NATS signals |
| [Agent runtime](specs/agent-runtime.md) | Layer 2 | Mode A container harnesses (`opencode`, `claude-code`, `pi`) and Mode B native `kodama agent` runtime |
| [AI gateway and MCP gateway](specs/ai-gateway-and-mcp-proxy.md) | Layer 2 | OpenAI-compatible model gateway, MCP gateway, and `agent_sessions` |
| [Software design](specs/software-design.md) | Cross-cutting | Lightweight hexagonal ports/adapters, package boundaries, and environments |
| [Platform integration](specs/platform-integration.md) | Cross-cutting | Optimi Platform core boundary and standalone vs hosted operation |

### Operators

1. [System architecture](book/05-system-architecture.md)
2. [Installation and configuration](operations/installation-and-configuration.md)
3. [Observability and audit](operations/observability-and-audit.md)
4. [Upgrades, backup, and recovery](operations/upgrades-backup-and-recovery.md)
5. [Identity, tenancy, and RBAC](specs/identity-tenancy-and-rbac.md)

### Integrators

- [Contract overview](contracts/README.md)
- [HTTP interface](contracts/http-api.md)
- [Pipeline format](contracts/pipeline-format.md)
- [Event catalog (NATS signals)](contracts/event-catalog.md)
- [Git service](contracts/git-service.md)
- [Example index](examples/README.md)

## Documentation conventions

### Authority and status

- **Confirmed:** selected by the project owner; changes require an explicit decision.
- **Proposed:** the implementation baseline for the active phase.
- **Open:** a choice needing an owner decision or implementation evidence.
- **Later:** a documented target beyond the current phase.

The [decision register](decisions.md) and [ADR 0013](adr/0013-two-layer-architecture-and-domain-simplification.md) record these statuses. Retired legacy IDs remain listed in the [KISS review](review-kiss.md#retired-requirement-ids).

## Project navigation

- [ADR 0013: Two-layer architecture and simplification](adr/0013-two-layer-architecture-and-domain-simplification.md)
- [Database schema (15-table model)](implementation/database-schema.md)
- [Repository layout](implementation/repository-layout.md)
- [Project status](status.md) and [assembly status](implementation/assembly-status.md)
- [Task backlog](tasks/README.md)
- [Capability traceability](traceability.md)
- [Decision register](decisions.md)
- [Architecture decisions](adr/README.md)
- [Repository README](../README.md)
- [Contributing](../CONTRIBUTING.md) and [instructions for agents](../AGENTS.md)
