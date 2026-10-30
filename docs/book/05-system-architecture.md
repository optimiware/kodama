# 5. System architecture

## Functional layers

Kodama is structured as a two-layer system ([ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md)) running alongside Optimi Platform ([platform integration](../specs/platform-integration.md)):

```mermaid
flowchart TB
    subgraph Access["Unified Access Surface (OAuth + RBAC)"]
        CLI["kodactl CLI"]
        API["HTTP REST API"]
        MCP["MCP Server"]
    end

    subgraph Layer2["Layer 2: Agentic Software Factory (Shell)"]
        ModeA["Mode A: Container Harnesses (opencode, claude-code, pi)"]
        ModeB["Mode B: Native kodama agent Runtime (skills, guardrails, decisions)"]
        AIGW["AI Gateway (OpenAI-compatible)"]
        MCPGW["MCP Gateway (Tool routing)"]
    end

    subgraph Layer1["Layer 1: Core Kernel (Drone CI-inspired Primitives)"]
        Pipes["Pipelines (.kodama/*.yaml) & Runs / Steps"]
        Svcs["Scoped Container Services (Tenant / Project / Pipeline)"]
        Git["Native Git Service (<tenant>/<project>/* & Knowledge Base)"]
        Tasks["Task Tracker (Project State Machine & Role Inbox)"]
        Meta["Secrets (Encrypted) & Artifacts (Metadata Only)"]
        NATS["NATS JetStream Message Bus"]
        PG[("PostgreSQL (15-table model + RLS)")]
    end

    CLI & API & MCP --> Layer1
    CLI & API & MCP --> Layer2
    ModeA & ModeB --> Layer1
    ModeB --> AIGW & MCPGW
    Pipes & Svcs & Git & Tasks & Meta <--> NATS
    Pipes & Svcs & Git & Tasks & Meta --> PG
```

## Single binary, clean roles

One static Go binary (`kodama` / `kodactl`) provides:
- `kodama server`: HTTP API, MCP server, pipeline/service scheduler, NATS consumers, AI Gateway, and MCP Gateway.
- `kodama git serve`: Per-tenant native Git smart-HTTP worker (`git http-backend`, receive/upload-pack, hooks) over the tenant's repository volume.
- `kodama step`: Thin in-container wrapper that prepares/mounts `/workspace`, injects authorized secrets, runs step commands, and reports outputs.
- `kodama agent`: Native Layer 2 agent runtime for planning, filtering, guardrails, validation, Git operations, knowledge-base lookup, and task state transitions.
- `kodactl`: User, operator, and in-container CLI.

## Responsibilities

| Component | Responsibility |
| --- | --- |
| **PostgreSQL** | 15-table relational store with tenant RLS (`tenants`, `users`, `memberships`, `projects`, `repositories`, `pipelines`, `runs`, `run_steps`, `services`, `tasks`, `artifacts`, `secrets`, `agent_sessions`, `mcp_servers`, `audit_events`). |
| **NATS JetStream** | Internal message bus capturing and relaying Git push/tag/merge triggers, task state transitions, step/agent coordination signals, releases, and promotions. |
| **Native Git service** | Hosts code and Markdown knowledge-base repositories under `<tenant>/context` and `<tenant>/<project>/*`; auto-indexes `.kodama/<processname>.yaml`. |
| **Container runtime** | Runs pipeline steps (`run_steps`) with a shared `/workspace` and support `services` (scoped to tenant, project, or pipeline) on Kubernetes (`production`), Podman (`test`), or Process (`development`). |
| **KMS / Keyfile** | Wraps encryption keys for `secrets` (AWS KMS, GCP Cloud KMS, or Scaleway Key Manager in `production`; local keyfile in `test`/`development`). |
| **External registries & targets** | Store published production artifacts (OCI images, packages, bundles); Kodama tracks metadata and digests in `artifacts`. |

## Permission model

1. **User RBAC (`memberships`):** External OAuth/OIDC authenticates users; roles (`admin`, `engineer`, `reviewer`, `operator`, `viewer`) control what a user can configure or resolve in a tenant or project.
2. **Project-space workload access:** Any step or agent running in a project is issued a scoped run token granting access to the resources provisioned and configured in that project space (the project's Git repositories, tenant/project knowledge base, mounted secrets, scoped services, tasks, and gateways) while isolating it from other tenants.
