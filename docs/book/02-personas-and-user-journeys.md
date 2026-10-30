# 2. Personas and user journeys

| Persona | Work |
| --- | --- |
| Engineer | Configure projects, `.kodama/<processname>.yaml` pipelines, and knowledge base repos; clone/push code; supervise autonomous runs |
| Reviewer / Approver | Resolve role-inbox decision and production promotion tasks (`kodactl` / MCP / API) |
| Operator / Admin | Manage tenant/project RBAC memberships, secrets, scoped services, and cluster installation |
| Agent (Mode A or B) | Execute coding, review, testing, guardrail, and release steps inside a project space |

## Journey J-01: Autonomous task to preview and promotion

1. **Task creation:** A user or upstream integration creates a task in project `erp` (`state: plan`).
2. **Planning (`.kodama/plan.yaml`):** The task creation signal on NATS triggers `.kodama/plan.yaml`. A Mode B `kodama agent` reads `<tenant>/context` and `<tenant>/erp/context`, inspects the target repository, writes the implementation plan into the task context, and transitions the task to `implement`.
3. **Implementation (`.kodama/implement.yaml`):** The transition signal on NATS triggers `.kodama/implement.yaml`. A Mode A step (`opencode` or `claude-code`) implements the feature in `/workspace`, runs tests against a project- or pipeline-scoped PostgreSQL service, commits and pushes the branch to the native Git service, and transitions the task to `review`.
4. **Agentic review & testing (`.kodama/review.yaml`):** A Mode B `kodama agent` runs guardrails and validation skills against the diff and test outputs. If checks fail, it transitions the task back to `implement` with feedback; if checks pass, it transitions the task to `deploy_preview`.
5. **Preview & promotion inbox (`.kodama/preview.yaml`):** The pipeline builds and publishes the artifact to an external registry, records artifact metadata in Kodama, deploys the preview, and **parks** the task (`state: parked`) behind a new `promotion` task assigned to the `reviewer` role inbox.
6. **Human promotion decision:** A reviewer checks their role inbox via `kodactl` or MCP, inspects the preview and artifact metadata, and resolves the promotion task. NATS relays the resolution signal, unblocking the task and triggering `.kodama/promote.yaml`.

## Journey J-02: Ambiguity and parked task resolution

When an agent encounters a missing requirement or policy conflict during `plan` or `implement`:
1. The agent transitions the task to `parked` and creates a blocking `decision` task assigned to the appropriate role inbox (for example `engineer` or `reviewer`) with the specific question.
2. No container compute is held while the task is parked.
3. A human answers and resolves the inbox task through `kodactl` or MCP.
4. NATS emits `task.resolved`, unblocking the parent task and re-triggering the pipeline with the recorded decision in the task context.

## Journey J-03: Multi-tenant isolation

An admin creates a tenant and projects. Tenant and project knowledge bases (`<tenant>/context`, `<tenant>/<project>/context`), repositories (`<tenant>/<project>/*`), secrets, services, and database rows (enforced via PostgreSQL RLS) are isolated per tenant. Steps and agents running in `<project>` can access only what is configured in that project space and tenant context.
