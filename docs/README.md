# Kodama documentation

Kodama is an API-first, autonomous software factory built around four functional layers ([ADR 0013](adr/0013-two-layer-architecture-and-domain-simplification.md)):
- **Layer 1 (Pipeline Runtime kernel):** YAML pipelines (`.kodama/<processname>.yaml`), runs and steps, shared run workspaces, container services, declared secrets, durable waits, cancellation, and recovery.
- **Layer 2 (Native Shell):** identity and authorization, projects, project-unique pipelines, tasks whose progress follows their selected pipeline, native Git, secret custody, metadata-only artifacts, HTTP API, Kodama MCP server, CLI, persistence/runtime adapters, and the central NATS JetStream bus.
- **Layer 3 (Agentic services):** external coding harnesses (`opencode`, `claude-code`, `pi`), the native `kodama agent` runtime, sessions, tools, AI Gateway, and the external MCP proxy.
- **Layer 4 (External behavior):** pipeline YAML, scripts, container images, hooks, and delivery/QA/notification recipes that extend Kodama without changing its code.

## Start here

| I want to... | Read |
| --- | --- |
| Understand the architecture | [ADR 0013](adr/0013-two-layer-architecture-and-domain-simplification.md) and [System architecture](book/05-system-architecture.md) |
| Understand the domain and data model | [Domain and lifecycles](book/03-domain-and-lifecycles.md) and [Database schema](implementation/database-schema.md) |
| Plan the code and spec cleanup | [Pipeline-runtime simplification plan](implementation/pipeline-runtime-simplification.md) |
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
| [Identity, tenancy, and RBAC](specs/identity-tenancy-and-rbac.md) | Shell | External OAuth/OIDC, user RBAC, tenant isolation, and project-space workload permissions |
| [Git repositories and knowledge base](specs/git-repositories.md) | Shell | Native Git service, Gerrit-style `<tenant>/<project>/*` hierarchy, `.kodama/*.yaml`, and Markdown knowledge base |
| [Pipeline execution and services](specs/pipeline-execution.md) | Kernel | `.kodama/<processname>.yaml`, runs and steps, durable waits, and container services |
| [Workspaces](specs/workspaces.md) | Kernel | Shared `/workspace` per pipeline run and source preparation |
| [Task tracker and role inbox](specs/work-management.md) | Shell | Tasks that select a pipeline, execution-progress projection, and wait-based inbox |
| [Secrets and configuration](specs/variables-and-secrets.md) | Kernel & shell | Declared secret references in the kernel; encrypted tenant/project custody in the shell |
| [Artifacts, releases, and promotions](specs/delivery-and-quality-gates.md) | Shell & Layer 4 | Metadata-only external artifacts and delivery/promotion recipes |
| [API, `kodactl` CLI, and MCP](specs/api-cli-and-integrations.md) | Shell | Unified HTTP API, Kodama MCP server, `kodactl` CLI, and NATS signals |
| [Agent runtime](specs/agent-runtime.md) | Agentic | Container harnesses (`opencode`, `claude-code`, `pi`) and the native `kodama agent` runtime |
| [AI gateway and MCP proxy](specs/ai-gateway-and-mcp-proxy.md) | Agentic | OpenAI-compatible model gateway, external MCP proxy, and `agent_sessions` |
| [Software design](specs/software-design.md) | Cross-cutting | Layer dependency direction, lightweight ports/adapters, and environments |
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

- [ADR 0013: Pipeline-runtime kernel and four-layer architecture](adr/0013-two-layer-architecture-and-domain-simplification.md)
- [Database schema](implementation/database-schema.md)
- [Pipeline-runtime simplification plan](implementation/pipeline-runtime-simplification.md)
- [Repository layout](implementation/repository-layout.md)
- [Project status](status.md) and [assembly status](implementation/assembly-status.md)
- [Task backlog](tasks/README.md)
- [Capability traceability](traceability.md)
- [Decision register](decisions.md)
- [Architecture decisions](adr/README.md)
- [Repository README](../README.md)
- [Contributing](../CONTRIBUTING.md) and [instructions for agents](../AGENTS.md)
