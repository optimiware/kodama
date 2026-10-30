# AI gateway and MCP gateway

**Scope:** Layer 2 OpenAI-compatible AI Gateway, MCP Gateway, `agent_sessions`, and `mcp_servers` ([ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md)).

## AI Gateway (OpenAI-compatible)

Kodama includes a built-in OpenAI-compatible HTTP proxy (`/api/v1/ai/v1/chat/completions`) used by the native `kodama agent` runtime (Mode B) and optionally by container harnesses (Mode A):
- **Server-side credential injection:** Resolves the project's or tenant's configured model secret (`secrets`) on the server so upstream provider API keys do not need to be exposed inside container environments.
- **Session & token tracking:** Records model name, prompt tokens, completion tokens, and message transcripts on `agent_sessions` (linked to `tenant_id`, `project_id`, `run_id`, `step_id`, and `task_id`).
- **Guardrails & filtering:** Enforces project-configured model allowlists and token/request budgets per run.

## MCP Gateway

Kodama provides a unified Model Context Protocol (MCP) surface (`/api/v1/mcp`) for both **external users/operators** (authenticated via OAuth RBAC) and **in-container agents** (authenticated via project-space `KODAMA_TOKEN`):

1. **Native Kodama MCP tools (Core Kernel & Knowledge Base):**
   - `tasks.*`: List, create, inspect, transition tasks, park tasks, and query/resolve the role inbox.
   - `git.*`: List repositories, read files/diffs from `<tenant>/context` and `<tenant>/<project>/*`, create branches, and inspect commits.
   - `pipelines.*` & `runs.*`: Trigger pipelines, inspect run/step status and logs, and manage scoped `services`.
   - `artifacts.*`: Register and query external artifact metadata.
2. **External MCP server federation (`mcp_servers`):**
   - Projects and tenants can register external MCP servers in the `mcp_servers` table (`name`, `url`, `secret_id`, `allowed_tools`).
   - The MCP Gateway proxies allowed tool calls to those servers while injecting `secret_id` credentials server-side.

## Requirements

| ID | Requirement |
| --- | --- |
| GM-01 | The AI Gateway MUST expose an OpenAI-compatible endpoint authenticated by project-space run tokens, inject upstream provider secrets server-side, and record token usage on `agent_sessions`. |
| GM-02 | The MCP Gateway MUST expose native Kodama Layer 1 tools and registered `mcp_servers` tools according to caller authority (user RBAC or project-space workload token). |
| GM-03 | External `mcp_servers` credentials (`secret_id`) MUST remain server-side and enforce `allowed_tools` filtering. |

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| GM-A01 | `kodama agent` calls the OpenAI-compatible AI Gateway using its run token without holding the raw provider key; usage is recorded in `agent_sessions` | GM-01 |
| GM-A02 | Both `kodactl` users and in-run agents invoke native task/Git/knowledge-base MCP tools and allowed external `mcp_servers` tools within their authorized scope | GM-02, GM-03 |
