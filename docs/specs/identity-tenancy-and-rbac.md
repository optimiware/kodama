# Identity, tenancy, and RBAC

**Scope:** Layer 2 native shell authentication, authorization, and tenant isolation ([ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md)).

## Simple two-tier permission model

Kodama uses a straightforward permission model separating **human configuration authority** from **project-space runtime access**:

1. **User RBAC (`users`, `memberships`):**
   - Users and operators authenticate via external OAuth/OIDC (`github.com/coreos/go-oidc/v3` and `golang.org/x/oauth2`, with PKCE for `kodactl`).
   - RBAC bindings (`memberships`) at tenant (`project_id IS NULL`) or project scope define what a user can configure or resolve:

| Role | Permissions |
| --- | --- |
| `admin` | Manage tenant/project settings, memberships, secrets metadata, and MCP servers |
| `engineer` | Configure `.kodama/*.yaml` pipelines and scoped services; clone/push Git repositories; create tasks; start/rerun runs |
| `reviewer` | Resolve pipeline waits assigned to the reviewer role |
| `operator` | Manage tenant/project container services, resolve operator-assigned waits, and inspect runtime state |
| `viewer` | Read-only access to projects, tasks, runs, logs, and artifact metadata |

2. **Project-space workload permissions (Steps and Agents):**
   - Agents (Mode A and Mode B) and pipeline steps executing inside a project are issued a scoped run token (`KODAMA_TOKEN`).
   - That token automatically grants access to what has been provisioned and configured in that project space:
     - The project's Git repositories (`<tenant>/<project>/*`) and read access to the tenant knowledge base (`<tenant>/context`).
     - Secrets and services scoped to the tenant, project, or pipeline run.
     - Tasks, artifact metadata registration, AI Gateway, and the external MCP proxy within that project.
   - Workloads cannot access another tenant's resources or elevate human RBAC memberships.

## Tenant isolation baseline

- **Database (PostgreSQL RLS):** Every tenant-owned table in the [current schema](../implementation/database-schema.md) carries `tenant_id` with forced Row-Level Security (`tenant_id = kodama_tenant_id()`). Project-owned tables enforce composite `(tenant_id, project_id)` foreign keys.
- **Compute & network:** In Kubernetes (`production`), each tenant executes in isolated tenant namespaces with non-root containers, resource quotas, and network policy denying cross-tenant traffic, control-plane database/broker access, and cloud metadata endpoints.
- **Git storage:** Each tenant's repositories live on that tenant's isolated Git service volume.

## Requirements

| ID | Requirement |
| --- | --- |
| IT-01 | Human API, MCP, and `kodactl` requests MUST authenticate via external OAuth/OIDC and check current tenant/project `memberships`. |
| IT-02 | All database tables MUST enforce tenant ownership via composite keys and forced PostgreSQL RLS under a non-owner runtime role. |
| IT-03 | Steps and agents executing in a project MUST receive project-space runtime access to that project's configured repositories, secrets, services, tasks, AI Gateway, and external MCP proxy, and MUST be denied cross-tenant access. |
| IT-04 | Resolving a pipeline wait MUST verify that the caller currently holds the wait's assigned role or correlation in that project or tenant and that the run, step, and wait are current. |
| IT-05 | Cross-tenant references MUST return non-disclosing `404 not_found` responses. |

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| IT-A01 | Unauthenticated or unauthorized role requests are rejected across HTTP, MCP, Git, and `kodactl` | IT-01, IT-05 |
| IT-A02 | Pooled PostgreSQL connections and RLS policies prevent any cross-tenant read or write | IT-02, IT-05 |
| IT-A03 | A pipeline step or `kodama agent` in project A can access project A's repos, secrets, services, and tasks, but cannot access tenant B | IT-03, IT-05 |
| IT-A04 | Only a user currently holding the wait's assigned role can resolve it; a stale wait, wrong run, or foreign task is refused | IT-04 |
