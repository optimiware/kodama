# Container Workload Contract

Every container action receives the same filesystem layout, environment variables, optional Git authentication (when `KODEMA_GIT_URL` is configured), and `/kodema/bin/kodema` toolbox—regardless of whether it runs Claude Code, OpenCode, Codex, Pi, or a shell script.

## Filesystem layout

| Path | Mode | Contents |
|---|---|---|
| `/workspace` | Writable | Working directory shared sequentially by all container actions of one execution and preserved across planned `request` waits. Disposable when the execution ends or is cancelled by `task.ask`. |
| `/kodema/inputs/<key>` | Read-only | Admitted execution input values. |
| `/kodema/outputs/<action>/<key>` | Read-only | Settled outputs of earlier succeeded actions in this execution. |
| `/kodema/outputs/<current-action>/` | Writable | Present when the current action declares `outputs`. Write one file per declared key; Kodema automatically runs `kodema output push` after the last command succeeds (trimming one trailing newline per file, max 8 keys, max 4 KiB each). |
| `/kodema/task.json` | Read-only | Task conversation snapshot at admission: a JSON array of messages (`author`, `at`, `body`, optional `references`). Absent for task-free executions. |
| `/kodema/secrets/<NAME>` | Read-only | Declared project secrets resolved at container activation. |
| `/kodema/secrets/KODEMA_TOKEN` | Read-only | Live execution credential (`kodex.*`), automatically renewed by the controller during long actions and revoked during request waits or terminal transitions. |
| `/kodema/bin/kodema` | Read-only | Control-plane-matched `kodema` CLI toolbox, automatically added to `PATH`. |

## Standard environment variables

Always supplied (unshadowable):

- `KODEMA_URL` — HTTPS base URL of Kodema's API reachable from the workload.
- `KODEMA_EXECUTION` — current execution ID.
- `KODEMA_ACTION` — current action name.
- `KODEMA_TASK` — associated task ID (unset for task-free executions).

Supplied when configured by the installation:

- `KODEMA_CA_FILE` — path to the installation's custom CA PEM bundle. Export it for runtimes that need custom TLS roots:
  ```sh
  if [ -n "${KODEMA_CA_FILE:-}" ]; then
    export NODE_EXTRA_CA_CERTS="$KODEMA_CA_FILE"
    export SSL_CERT_FILE="$KODEMA_CA_FILE"
    export CURL_CA_BUNDLE="$KODEMA_CA_FILE"
    export GIT_SSL_CAINFO="$KODEMA_CA_FILE"
  fi
  ```
- `KODEMA_GIT_URL` — base URL of the installation's Git service (`$KODEMA_GIT_URL/<repository>`). Kodema injects `GIT_CONFIG_*` so Git authenticates to that origin alone via `/kodema/bin/kodema git credential` with no pipeline setup.
- `KODEMA_LLM_<PROVIDER>_BASE_URL` — unshadowable streaming LLM proxy endpoint for every provider enabled in the execution's scope (provider name normalized to uppercase `[A-Z0-9_]`). In `kodema up`, Kodema enables three built-in providers out of the box plus any custom `--llm-provider` entries:
  - `KODEMA_LLM_OPENAI_BASE_URL` -> `${KODEMA_URL}/llm/openai/v1` (backed by project secret `OPENAI_API_KEY`)
  - `KODEMA_LLM_ANTHROPIC_BASE_URL` -> `${KODEMA_URL}/llm/anthropic` (backed by project secret `ANTHROPIC_API_KEY`)
  - `KODEMA_LLM_OPENCODE_BASE_URL` -> `${KODEMA_URL}/llm/opencode/v1` (OpenCode Zen, backed by project secret `OPENCODE_API_KEY`)
  - Custom providers (e.g. `--llm-provider openrouter=https://openrouter.ai/api/v1`) -> `KODEMA_LLM_OPENROUTER_BASE_URL=${KODEMA_URL}/llm/openrouter/v1`

## Using Kodema's native LLM proxy from coding harnesses

Store provider API keys once as project secrets (`kodema secret set OPENCODE_API_KEY`, `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, etc.). **Never declare provider keys in container `secrets: [...]`**—Kodema keeps raw provider keys inside the control plane and strictly refuses to mount LLM-proxy-managed secrets into workload containers (even when local `kodema up` allows other project secrets).

Every harness authenticates with `/kodema/secrets/KODEMA_TOKEN` and trusts `$KODEMA_CA_FILE`. Token usage for every proxied call is attributed automatically to the execution and surfaced by `kodema inspect <execution-id>` (`model_usage`).

Shared TLS prelude:

```sh
if [ -n "${KODEMA_CA_FILE:-}" ]; then
  export NODE_EXTRA_CA_CERTS="$KODEMA_CA_FILE"
  export SSL_CERT_FILE="$KODEMA_CA_FILE"
  export CURL_CA_BUNDLE="$KODEMA_CA_FILE"
  export GIT_SSL_CAINFO="$KODEMA_CA_FILE"
fi
```

### OpenCode

Store `OPENCODE_API_KEY` (or `ANTHROPIC_API_KEY` / `OPENAI_API_KEY`) via `kodema secret set`:

```sh
export OPENAI_BASE_URL="${KODEMA_LLM_OPENCODE_BASE_URL:-${KODEMA_URL}/llm/opencode/v1}"
IFS= read -r OPENAI_API_KEY < /kodema/secrets/KODEMA_TOKEN || [ -n "$OPENAI_API_KEY" ]
export OPENAI_API_KEY
opencode run "Implement the requested change and run tests"
```

### Claude Code

Store `ANTHROPIC_API_KEY` via `kodema secret set ANTHROPIC_API_KEY`:

```sh
export ANTHROPIC_BASE_URL="${KODEMA_LLM_ANTHROPIC_BASE_URL:-${KODEMA_URL}/llm/anthropic}"
IFS= read -r ANTHROPIC_API_KEY < /kodema/secrets/KODEMA_TOKEN || [ -n "$ANTHROPIC_API_KEY" ]
export ANTHROPIC_API_KEY
claude -p "Implement the requested change and run tests" --dangerously-skip-permissions
```

### Codex

Store `OPENAI_API_KEY` via `kodema secret set OPENAI_API_KEY`:

```sh
export OPENAI_BASE_URL="${KODEMA_LLM_OPENAI_BASE_URL:-${KODEMA_URL}/llm/openai/v1}"
IFS= read -r OPENAI_API_KEY < /kodema/secrets/KODEMA_TOKEN || [ -n "$OPENAI_API_KEY" ]
export OPENAI_API_KEY
codex exec "Implement the requested change and run tests"
```

### Pi

Store `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` (or a custom provider key via `--llm-provider`):

```sh
export ANTHROPIC_BASE_URL="${KODEMA_LLM_ANTHROPIC_BASE_URL:-${KODEMA_URL}/llm/anthropic}"
export OPENAI_BASE_URL="${KODEMA_LLM_OPENAI_BASE_URL:-${KODEMA_URL}/llm/openai/v1}"
IFS= read -r ANTHROPIC_API_KEY < /kodema/secrets/KODEMA_TOKEN || [ -n "$ANTHROPIC_API_KEY" ]
IFS= read -r OPENAI_API_KEY < /kodema/secrets/KODEMA_TOKEN || [ -n "$OPENAI_API_KEY" ]
export ANTHROPIC_API_KEY OPENAI_API_KEY
pi -p "Implement the requested change and run tests"
```

### Minimal coding-harness base image (`Containerfile`)

Build a lean, repeatable image with `git`, `jq`, `curl`, `node`, and OpenCode preconfigured to wire `$KODEMA_CA_FILE` and `/kodema/secrets/KODEMA_TOKEN` to Kodema's native LLM proxy:

```dockerfile
FROM docker.io/library/node:22-bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git jq \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g opencode-ai

RUN cat <<'EOF' > /usr/local/bin/kodema-proxy-env.sh
#!/bin/sh
if [ -r /kodema/secrets/KODEMA_TOKEN ]; then
  IFS= read -r ANTHROPIC_API_KEY < /kodema/secrets/KODEMA_TOKEN || [ -n "$ANTHROPIC_API_KEY" ]
  IFS= read -r OPENAI_API_KEY < /kodema/secrets/KODEMA_TOKEN || [ -n "$OPENAI_API_KEY" ]
  IFS= read -r OPENCODE_API_KEY < /kodema/secrets/KODEMA_TOKEN || [ -n "$OPENCODE_API_KEY" ]
  export ANTHROPIC_API_KEY OPENAI_API_KEY OPENCODE_API_KEY
fi
if [ -n "${KODEMA_LLM_ANTHROPIC_BASE_URL:-}" ]; then
  export ANTHROPIC_BASE_URL="$KODEMA_LLM_ANTHROPIC_BASE_URL"
fi
if [ -n "${KODEMA_LLM_OPENCODE_BASE_URL:-}" ]; then
  export OPENAI_BASE_URL="$KODEMA_LLM_OPENCODE_BASE_URL"
elif [ -n "${KODEMA_LLM_OPENAI_BASE_URL:-}" ]; then
  export OPENAI_BASE_URL="$KODEMA_LLM_OPENAI_BASE_URL"
fi
if [ -n "${KODEMA_CA_FILE:-}" ]; then
  export NODE_EXTRA_CA_CERTS="$KODEMA_CA_FILE"
  export SSL_CERT_FILE="$KODEMA_CA_FILE"
  export CURL_CA_BUNDLE="$KODEMA_CA_FILE"
  export GIT_SSL_CAINFO="$KODEMA_CA_FILE"
fi
EOF
RUN chmod 0755 /usr/local/bin/kodema-proxy-env.sh
```

Then source `/usr/local/bin/kodema-proxy-env.sh` in your pipeline command (for example `. /usr/local/bin/kodema-proxy-env.sh && opencode run "..."`).

Read any other declared non-proxy secret file with `IFS= read -r ... < /kodema/secrets/<NAME>`.

## Toolbox commands inside containers

`kodema` automatically detects `/kodema/secrets/KODEMA_TOKEN` and `$KODEMA_URL` inside a container action:

- **Read Kodema docs:** `kodema docs` / `kodema docs workload` / `kodema docs install --target agents`
- **Pull shared company/project context & skills:**
  ```sh
  kodema context pull --repo context --repo myproject/context --include-kodema-docs
  ```
- **Ask a human or service for unplanned input** (save durable commits/digests first—this cancels the current execution!):
  ```sh
  kodema task ask --question "Which target branch should this land on?" --to role/engineer --ref "commit=$(git rev-parse HEAD)"
  ```
- **Inspect or comment on the task:** `kodema task show "$KODEMA_TASK"`, `kodema call task.comment ...`
- **Connect an in-container harness over MCP:** `kodema mcp stdio` bridges stdin/stdout to `$KODEMA_URL/mcp` using `/kodema/secrets/KODEMA_TOKEN`.
- **Submit outputs early (optional):** `kodema output push` (also run automatically after the final command).

## Execution permissions boundary

An execution credential (`kodex.*`) can read/edit its assigned task's content and metadata, comment on or ask questions on its task, create project tasks, submit its own action outputs, publish custom events, and call configured models/tools. It **cannot** resolve requests, reply to waiting tasks, read plaintext secrets, or manage pipelines/memberships/bindings.
