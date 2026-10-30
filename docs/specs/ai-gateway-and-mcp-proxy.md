# AI gateway and external MCP proxy

**Scope:** Layer 3 OpenAI-compatible AI Gateway, external MCP proxy, `agent_sessions`, and `mcp_servers` ([ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md)). The native Kodama MCP server belongs to the Layer 2 shell and is described in the [API and CLI specification](api-cli-and-integrations.md).

## AI Gateway (OpenAI-compatible)

Kodama includes a built-in OpenAI-compatible HTTP proxy (`/api/v1/ai/v1/chat/completions`) used by the native `kodama agent` runtime (Mode B) and optionally by container harnesses (Mode A):
- **Server-side credential injection:** Resolves the project's or tenant's configured model secret (`secrets`) on the server so upstream provider API keys do not need to be exposed inside container environments.
- **Session & token tracking:** Records model name, prompt tokens, completion tokens, and message transcripts on `agent_sessions` (linked to `tenant_id`, `project_id`, `run_id`, `step_id`, and `task_id`).
- **Guardrails & filtering:** Enforces project-configured model allowlists and token/request budgets per run.

## Native MCP server and external MCP proxy

The Layer 2 shell provides the native Kodama MCP surface (`/api/v1/mcp`) for external users and operators. Layer 3 provides the external MCP proxy used by agent work. Both authenticate the caller and apply current authority.

1. **Native Kodama MCP tools (shell):**
   - `tasks.*`: List, create, inspect, start, rerun, and query/resolve the task's current wait.
   - `git.*`: List repositories, read files and diffs from `<tenant>/context` and `<tenant>/<project>/*`, create branches, and inspect commits.
   - `pipelines.*` and `runs.*`: Trigger pipelines, inspect run/step status and logs, and manage scoped `services`.
   - `artifacts.*`: Register and query external artifact metadata.
2. **External MCP server federation (`mcp_servers`):**
   - Projects and tenants can register external MCP servers in the `mcp_servers` table (`name`, `url`, `secret_id`, `allowed_tools`).
   - The Layer 3 proxy forwards allowed tool calls to those servers while injecting `secret_id` credentials server-side.
   - The proxy does not add execution semantics or access private shell state.

## Requirements

| ID | Requirement |
| --- | --- |
| GM-01 | The AI Gateway MUST expose an OpenAI-compatible endpoint authenticated by project-space run tokens, inject upstream provider secrets server-side, and record token usage on `agent_sessions`. |
| GM-02 | The external MCP proxy MUST expose registered `mcp_servers` tools according to caller authority; the native Kodama MCP server remains a Layer 2 shell surface. |
| GM-03 | External `mcp_servers` credentials (`secret_id`) MUST remain server-side and enforce `allowed_tools` filtering. |

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| GM-A01 | `kodama agent` calls the OpenAI-compatible AI Gateway using its run token without holding the raw provider key; usage is recorded in `agent_sessions` | GM-01 |
| GM-A02 | Both `kodactl` users and in-run agents invoke native task/Git/knowledge-base MCP tools and allowed external `mcp_servers` tools within their authorized scope | GM-02, GM-03 |
