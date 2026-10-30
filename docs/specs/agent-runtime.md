# Agent runtime

**Scope:** Layer 2 Agentic Software Factory—operating as a shell over the Layer 1 Core Kernel ([ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md)).

## Two ways of running agentic tasks

Layer 2 leverages Layer 1 primitives without changing or interfering with Layer 1's core design. Every agentic task executes inside a standard Layer 1 container step mounting the shared `/workspace` and project-space credentials:

### Mode A: External harnesses in primitive container steps
Popular coding harnesses (`opencode`, `claude-code`, `pi`, Codex, Aider) run directly as standard container steps configured in `.kodama/<processname>.yaml`:
- Mount project `secrets` (or point the harness at Kodama's internal OpenAI-compatible AI Gateway and MCP Gateway).
- Operate on the shared `/workspace` (which contains the checked-out repository plus `<tenant>/context` and `<tenant>/<project>/context`).
- Commit and push changes to the native Git service or leave files in `/workspace` for subsequent test/review steps.

```yaml
steps:
  - name: implement
    image: ghcr.io/sst/opencode:latest
    secrets: [ANTHROPIC_API_KEY]
    commands:
      - opencode run "Implement task ${KODAMA_TASK_ID} following /workspace/.context"
```

### Mode B: Native Kodama Agent runtime (`kodama agent`)
For fine-grained software factory steps, the step container includes the `kodama` binary runtime (`kodama agent`), which has native access to Layer 1 core features and project-space permissions:
- **Specialized responsibilities:**
  - **Planning & decomposition:** Read `<tenant>/context` and `<tenant>/<project>/context`, inspect repository structure, and create subtasks or implementation plans.
  - **Decision-making, filtering & guardrails:** Inspect diffs, test outputs, and security rules; decide whether to advance a task or loop it back for rework.
  - **Validation gates:** Act as autonomous quality gates between pipeline stages.
  - **Embedded skills:** Run reusable Markdown skills loaded from the Git Knowledge Base (`<tenant>/context/skills/*`, `<tenant>/<project>/context/skills/*`).
  - **Native Core API / Git / Task ops:** Perform Git operations (branch, merge, tag), transition tasks along the project state machine (`plan -> implement -> review -> test -> deploy_preview`), or park a task behind a role-inbox `decision` / `promotion` task when human judgment is needed.
- **Session persistence:** Records execution state, messages, tool calls, token usage, and structured results in `agent_sessions`.

```yaml
steps:
  - name: review-guardrail
    image: ghcr.io/optimiware/kodama:latest
    commands:
      - kodama agent run --skill review --on-pass test --on-fail implement
```

## Permissions

Both Mode A and Mode B steps inherit the **project-space permission model**: they can access everything provisioned and configured in their project space (repositories, mounted secrets, scoped services, tasks, knowledge base, AI Gateway, and MCP Gateway) using the step's `KODAMA_TOKEN`.

## Requirements

| ID | Requirement |
| --- | --- |
| AR-01 | Layer 2 agent steps (Mode A external harnesses and Mode B `kodama agent`) MUST execute as standard Layer 1 container steps sharing `/workspace` and project-space permissions. |
| AR-02 | Mode B `kodama agent` MUST support OpenAI-compatible AI Gateway calls, MCP Gateway tools, embedded skills from Git `context` repos, Git ops, and task state transitions. |
| AR-03 | Native `kodama agent` runs MUST persist session state, token usage, and structured outcomes in `agent_sessions`. |
| AR-04 | When an agent requires human input or approval, it MUST park the task (`state: parked`) and create a blocking task in the appropriate role inbox. |

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| AR-A01 | A Mode A step runs an external harness in `/workspace` with mounted secrets and subsequent steps verify the changes | AR-01 |
| AR-A02 | A Mode B `kodama agent` loads a skill from `<tenant>/<project>/context`, calls the AI and MCP gateways, records an `agent_sessions` row, and transitions the task state | AR-01, AR-02, AR-03 |
| AR-A03 | A Mode B `kodama agent` encountering an ambiguity parks the task and creates a `decision` task in the role inbox | AR-04 |
