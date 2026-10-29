# Kodama documentation

Kodama is an API-first software factory: coding agents and container pipelines do the work, and people make the decisions. These documents describe what it does, how it is built and how to run it.

## Start here

| I want to... | Read |
| --- | --- |
| See what works today | [Project status](status.md) |
| Understand the idea | [Vision and scope](book/01-vision-and-scope.md), then [domain and lifecycles](book/03-domain-and-lifecycles.md) |
| See it in a real scenario | [ERP gap to preview](examples/workflows/erp-gap-to-preview.md) |
| Understand the architecture | [System architecture](book/05-system-architecture.md) and the [ADR index](adr/README.md) |
| Integrate with the API | [Contracts](contracts/README.md) and the [HTTP interface](contracts/http-api.md) |
| Install and operate it | [Installation and configuration](operations/installation-and-configuration.md) |
| Contribute code | [CONTRIBUTING.md](../CONTRIBUTING.md) and the [specifications](#contributors) |

The documents are specifications: they describe the required behavior, and [project status](status.md) says how much of it exists. Package implementation, executable wiring, milestone acceptance and production qualification are separate evidence boundaries.

## Reading paths

### Product and domain

1. [Vision and scope](book/01-vision-and-scope.md)
2. [Personas and user journeys](book/02-personas-and-user-journeys.md)
3. [Domain and lifecycles](book/03-domain-and-lifecycles.md)
4. [Lean flow and governance](book/04-lean-flow-and-governance.md)
5. [Application blueprints](book/06-application-blueprints.md)
6. [Delivery roadmap](book/07-delivery-roadmap.md)

Use the [glossary](../CONTEXT.md) for canonical terms.

### Contributors

Start with [system architecture](book/05-system-architecture.md), the [decision register](decisions.md), and the [ADR index](adr/README.md). Then follow the relevant specification:

| Specification | Prefix | Main responsibility |
| --- | --- | --- |
| [Platform integration](specs/platform-integration.md) | PI | Company-core ownership, hosted authority, standalone adapters, and cross-repository acceptance |
| [Software design](specs/software-design.md) | SD | Preferred hexagonal ports/adapters, DDD entities, and dependency boundaries |
| [Work management](specs/work-management.md) | WM | Intent, board flow, dependencies, and revisions |
| [Workspaces](specs/workspaces.md) | WS | Working continuity, access, checkpoints, and ownership |
| [Pipeline execution](specs/pipeline-execution.md) | PX | Durable orchestration and container execution |
| [Agent runtime](specs/agent-runtime.md) | AR | Harness profiles, tools, budgets, and structured results |
| [Identity, tenancy, and RBAC](specs/identity-tenancy-and-rbac.md) | IT | Authentication, authorization, and isolation |
| [AI gateway and MCP proxy](specs/ai-gateway-and-mcp-proxy.md) | GM | Model access and tool mediation |
| [Variables and secrets](specs/variables-and-secrets.md) | VS | Configuration and encrypted credential handling |
| [Delivery and quality gates](specs/delivery-and-quality-gates.md) | DQ | Evidence, approvals, environments, and promotion |
| [API, CLI, and integrations](specs/api-cli-and-integrations.md) | AC | Public access, external integrations, and notifications |
| [External MCP server](specs/mcp-server.md) | MS | OAuth-protected MCP access to the public API |

### Operators

1. [System architecture](book/05-system-architecture.md)
2. [Installation and configuration](operations/installation-and-configuration.md)
3. [Observability and audit](operations/observability-and-audit.md)
4. [Upgrades, backup, and recovery](operations/upgrades-backup-and-recovery.md)
5. [External MCP server](operations/mcp-server.md)
6. [Identity, tenancy, and RBAC](specs/identity-tenancy-and-rbac.md)

### Integrators

- [Contract overview](contracts/README.md)
- [HTTP interface](contracts/http-api.md)
- [External MCP server](specs/mcp-server.md)
- [Pipeline format](contracts/pipeline-format.md)
- [Event catalog](contracts/event-catalog.md)
- [Example index](examples/README.md)

## Documentation conventions

### Authority and status

- **Confirmed:** selected by the project owner; changes require an explicit decision.
- **Proposed:** the recommended implementation baseline; details need validation during the relevant milestone.
- **Open:** a choice needing an owner decision or implementation evidence.
- **Later:** a documented target beyond the initial milestone.

The [decision register](decisions.md) records these statuses. Confirmed choices establish direction; detailed specifications are proposed implementation contracts unless a narrower status is stated. An ADR preserves the rationale for a consequential choice. If documents conflict, resolve the conflict against the register and the authoritative specification before implementing it.

The glossary owns definitions. Specifications own behavior and acceptance. Contracts own wire shapes and serialization. Operations documents own installation and recovery procedures. Book chapters explain these sources and link to their details.

[Platform integration](specs/platform-integration.md) owns the company-core/agent-layer boundary. Platform remains authoritative for its own contracts; local schemas and standalone adapters do not establish hosted authority. [Design conformance](implementation/design-conformance.md) defines the review baseline and owner map that keep specifications, implementation, and evidence aligned.

### Requirements and acceptance

Each specification has stable requirement identifiers such as `PX-01` and acceptance identifiers such as `PX-A01`. Acceptance scenarios cite the requirements they verify. Normative **MUST**, **SHOULD**, and **MAY** describe obligations of the proposed contract; they do not imply an existing implementation.

Retired IDs remain reserved and are listed in the [review](review-kiss.md#retired-requirement-ids). Their removed behavior is not an active implementation requirement.

Every specification includes purpose, release scope, its model, behavioral rules, interfaces, authorization, failure and recovery, acceptance scenarios, and unresolved decisions. Prefixes and IDs remain stable when sections move.

### Examples

Examples use fabricated identifiers and reserved example domains. Pipeline image digests are syntactically shaped placeholders, not published images. Command examples describe the intended CLI. Secrets are referenced by name; secret-setting examples use conspicuous placeholder values.

Validate Markdown links/fenced syntax and examples against the owning contracts. Validate existing runtime JSON Schema and module OpenAPI fixtures as part of their task scopes; aggregate generation and regeneration drift belong to the ordered integration owners. Do not describe an existing package/schema as future-only or infer executable availability from it.

### Change review

1. Identify the authoritative specification and affected requirements.
   Record its revision and applicable upstream/blueprint references in the design baseline; link to owners rather than duplicating their inventories.
2. Update behavior, wire contracts, and examples together.
3. Add acceptance scenarios for meaningful new behavior or recovery paths.
4. Record newly resolved choices in the register; use an ADR when the trade-off is consequential.
5. Check links, terminology, lifecycle consistency, and [traceability](traceability.md).
6. Record conformance or an intentional, reviewed contract change. Material unresolved deviations block dependent work; a successful agent run or green build does not resolve them.

## Project navigation

- [Project status](status.md) and [assembly status](implementation/assembly-status.md)
- [Task backlog](tasks/README.md)
- [Capability traceability](traceability.md)
- [Decision register](decisions.md)
- [Architecture decisions](adr/README.md)
- [Repository README](../README.md)
- [Contributing](../CONTRIBUTING.md) and [instructions for agents](../AGENTS.md)
