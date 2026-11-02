# 2. Personas and user journeys

| Persona | Work |
| --- | --- |
| Engineer | Configure projects, `.kodama/<processname>.yaml` pipelines, and knowledge base repos; clone/push code; supervise autonomous runs |
| Reviewer / Approver | Resolve role-inbox decision and production promotion tasks (`kodactl` / MCP / API) |
| Operator / Admin | Manage tenant/project RBAC memberships, secrets, scoped services, and cluster installation |
| Agent (Mode A or B) | Execute coding, review, testing, guardrail, and release steps inside a project space |

## Journey J-01: Autonomous task to preview and promotion

1. **Task creation:** A user or upstream integration creates a task in project `erp`, selecting its project-unique `sdlc` pipeline.
2. **Planning:** The task's run starts `sdlc`; a Mode B `kodama agent` reads `<tenant>/context` and `<tenant>/erp/context`, inspects the target repository, writes the implementation plan into the task context, and continues to the next step.
3. **Implementation:** A Mode A step (`opencode` or `claude-code`) implements the feature in `/workspace`, runs tests against a project- or pipeline-scoped PostgreSQL service, commits and pushes the branch to the native Git service, and continues.
4. **Agentic review and testing:** A Mode B `kodama agent` runs guardrails and validation skills against the diff and test outputs. The pipeline selects the rework or test branch from the recorded result.
5. **Preview and promotion wait:** The pipeline builds and publishes the artifact to an external registry, records artifact metadata in Kodama, deploys the preview, and enters a durable wait assigned to the `reviewer` role.
6. **Human promotion decision:** A reviewer checks the role inbox via `kodactl` or MCP, inspects the preview and artifact metadata, and records an approval or rejection against the exact run, step, and wait. NATS relays the committed signal, the same run resumes, and the pipeline selects the promotion or rework branch.

## Journey J-02: Ambiguity and wait resolution

When an agent encounters a missing requirement or policy conflict during `plan` or `implement`:

1. The pipeline enters a durable wait assigned to the appropriate role inbox (for example `engineer` or `reviewer`) with the specific question and expected input.
2. No container compute or worker is held while the run waits; retained workspace and service lifetimes are bounded and explicit.
3. A human records an answer through `kodactl` or MCP against that exact wait.
4. NATS emits the committed wait signal, the same run resumes, and the pipeline selects its answered or rework branch with the recorded decision in the run context.

## Journey J-03: Multi-tenant isolation

An admin creates a tenant and projects. Tenant and project knowledge bases (`<tenant>/context`, `<tenant>/<project>/context`), repositories (`<tenant>/<project>/*`), secrets, services, and database rows (enforced via PostgreSQL RLS) are isolated per tenant. Steps and agents running in `<project>` can access only what is configured in that project space and tenant context.
