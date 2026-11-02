# Agent runtime

**Scope:** Layer 3 agentic services operating through Layer 1 container steps and Layer 2 shell operations ([ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md)).

## Two ways of running agentic tasks

Layer 3 agentic services execute through Layer 1 container steps and Layer 2 shell operations without changing the kernel's execution model. Every agentic task executes inside a standard Layer 1 container step mounting the shared `/workspace` and project-space credentials:

### Mode A: External harnesses in primitive container steps
Popular coding harnesses (`opencode`, `claude-code`, `pi`, Codex, Aider) run directly as standard container steps configured in `.kodama/<processname>.yaml`:
- Mount project `secrets` (or point the harness at Kodama's internal OpenAI-compatible AI Gateway and external MCP proxy).
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
For fine-grained software factory steps, the step container includes the `kodama` binary runtime (`kodama agent`), which has native access to Layer 2 shell operations and project-space permissions:
- **Specialized responsibilities:**
  - **Planning & decomposition:** Read `<tenant>/context` and `<tenant>/<project>/context`, inspect repository structure, and create subtasks or implementation plans.
  - **Decision-making, filtering & guardrails:** Inspect diffs, test outputs, and security rules; decide whether to advance a task or loop it back for rework.
  - **Validation steps:** Act as autonomous quality checks between pipeline stages.
  - **Embedded skills:** Run reusable Markdown skills loaded from the Git Knowledge Base (`<tenant>/context/skills/*`, `<tenant>/<project>/context/skills/*`).
  - **Native shell API / Git / task operations:** Perform Git operations (branch, merge, tag), read the task's selected pipeline and projected execution progress, and request a durable pipeline wait when human judgment is needed. The pipeline defines the next branch; the agent does not mutate a separate task workflow.
- **Session persistence:** Records execution state, messages, tool calls, token usage, and structured results in `agent_sessions`.

```yaml
steps:
  - name: review-guardrail
    image: ghcr.io/optimiware/kodama:latest
    commands:
      - kodama agent run --skill review --on-pass test --on-fail implement
```

## Permissions

Both Mode A and Mode B steps inherit the **project-space permission model**: they can access everything provisioned and configured in their project space (repositories, mounted secrets, scoped services, tasks, knowledge base, AI Gateway, and external MCP proxy) using the step's `KODAMA_TOKEN`.

## Requirements

| ID | Requirement |
| --- | --- |
| AR-01 | Layer 3 agent work (Mode A external harnesses and Mode B `kodama agent`) MUST execute as standard Layer 1 container steps sharing `/workspace` and project-space permissions. |
| AR-02 | Mode B `kodama agent` MUST support OpenAI-compatible AI Gateway calls, external MCP proxy tools, embedded skills from Git `context` repos, Git operations, and reading the selected pipeline's projected task progress. |
| AR-03 | Native `kodama agent` runs MUST persist session state, token usage, and structured outcomes in `agent_sessions`. |
| AR-18 | When agent work requires human input or approval, it MUST request a durable pipeline wait with an explicit expected input and assignment; it MUST NOT mutate a separate task workflow or create a blocking task solely to resume one. |
| AR-19 | The external MCP proxy and agent tool policy MUST remain in Layer 3 and MUST NOT add execution semantics or private shell state access to the kernel. |

AR-04 is retired by the [ADR 0013 amendment](../adr/0013-two-layer-architecture-and-domain-simplification.md). The former park-and-blocking-task model is replaced by AR-18.

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| AR-A01 | A Mode A step runs an external harness in `/workspace` with mounted secrets and subsequent steps verify the changes | AR-01 |
| AR-A02 | A Mode B `kodama agent` loads a skill from `<tenant>/<project>/context`, calls the AI and external MCP proxy, records an `agent_sessions` row, and reads the task's projected progress | AR-01, AR-02, AR-03, AR-19 |
| AR-A03 | A Mode B `kodama agent` encountering an ambiguity requests a pipeline wait; an authorized result resumes the same run and the pipeline selects the next branch | AR-18 |
