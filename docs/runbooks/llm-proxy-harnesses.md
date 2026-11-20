# Native LLM proxy and coding-harness recipes

Kodema includes a streaming, credential-injecting LLM proxy (`/llm/{provider}/*`) so external coding harnesses (**OpenCode**, **Claude Code**, **Codex**, **Pi**) run inside workload containers with **zero raw provider API keys mounted into containers**.

## How it works

1. Start Kodema (`kodema up` enables the LLM proxy automatically with `openai`, `anthropic`, and `opencode`).
2. Store provider API keys as project secrets:

   ```sh
   kodema secret set OPENCODE_API_KEY
   kodema secret set ANTHROPIC_API_KEY
   kodema secret set OPENAI_API_KEY
   ```

3. Container workloads **never** list provider keys in `secrets: [...]`. Kodema enforces strict secret isolation: any secret owned by a configured LLM proxy provider is refused during workload secret materialization, even when local `kodema up` allows other project secrets.
4. At container activation, Kodema injects an unshadowable `KODEMA_LLM_<PROVIDER>_BASE_URL` environment variable for every configured provider in the execution's scope:

   | Provider | Injected variable | Proxy URL | Upstream default | Project secret |
   |---|---|---|---|---|
   | `openai` | `KODEMA_LLM_OPENAI_BASE_URL` | `${KODEMA_URL}/llm/openai/v1` | `https://api.openai.com` | `OPENAI_API_KEY` |
   | `anthropic` | `KODEMA_LLM_ANTHROPIC_BASE_URL` | `${KODEMA_URL}/llm/anthropic` | `https://api.anthropic.com` | `ANTHROPIC_API_KEY` |
   | `opencode` | `KODEMA_LLM_OPENCODE_BASE_URL` | `${KODEMA_URL}/llm/opencode/v1` | `https://opencode.ai/inference/openai/v1` | `OPENCODE_API_KEY` |
   | Custom (`<slug>`) | `KODEMA_LLM_<SLUG>_BASE_URL` | `${KODEMA_URL}/llm/<slug>/v1` (or `/llm/<slug>` for `x-api-key`) | Configured `base_url` | Configured `secret_name` |

5. Inside the container, the harness authenticates with `/kodema/secrets/KODEMA_TOKEN` and trusts `$KODEMA_CA_FILE`. Kodema verifies the active execution and action deadline, strips the workload token, injects the provider credential inside the Shell, streams SSE responses chunk-by-chunk, and records per-execution token usage in `model_usage`.

## Custom OpenAI-compatible providers

Add custom providers (OpenRouter, LiteLLM, vLLM, Ollama, corporate gateways) or override a built-in provider directly with `--llm-provider`:

```sh
kodema up \
  --llm-provider openrouter=https://openrouter.ai/api/v1 \
  --llm-provider local-vllm=http://host.containers.internal:8000/v1,secret=VLLM_API_KEY
```

Store the matching project secret (`kodema secret set OPENROUTER_API_KEY` or `kodema secret set VLLM_API_KEY`), and workloads automatically receive `KODEMA_LLM_OPENROUTER_BASE_URL` and `KODEMA_LLM_LOCAL_VLLM_BASE_URL`.

For `kodema serve`, pass `--models-file /path/to/models.json` (or `KODEMA_MODELS_FILE`) or repeatable `--llm-provider <name>=<base_url>[,secret=<NAME>][,auth=<Authorization|x-api-key>][,tenant=<TENANT>][,project=<PROJECT>]`.

## Harness recipes

All four recipes authenticate with `/kodema/secrets/KODEMA_TOKEN`, trust `$KODEMA_CA_FILE`, and declare **no** provider keys in `secrets: [...]`. See also [`examples/pipelines/external-harness.yaml`](../../examples/pipelines/external-harness.yaml).

### OpenCode

```yaml
actions:
  - name: opencode
    image: registry.example.com/kodema/opencode-harness@sha256:0000000000000000000000000000000000000000000000000000000000000000
    timeout: 30m
    commands:
      - |
        if [ -n "${KODEMA_CA_FILE:-}" ]; then
          export NODE_EXTRA_CA_CERTS="$KODEMA_CA_FILE"
          export SSL_CERT_FILE="$KODEMA_CA_FILE"
          export CURL_CA_BUNDLE="$KODEMA_CA_FILE"
          export GIT_SSL_CAINFO="$KODEMA_CA_FILE"
        fi
        export OPENAI_BASE_URL="${KODEMA_LLM_OPENCODE_BASE_URL:-${KODEMA_URL}/llm/opencode/v1}"
        IFS= read -r OPENAI_API_KEY < /kodema/secrets/KODEMA_TOKEN || [ -n "$OPENAI_API_KEY" ]
        export OPENAI_API_KEY
        opencode run "Implement the requested task and verify tests pass"
```

### Claude Code

```yaml
actions:
  - name: claude
    image: registry.example.com/kodema/claude-harness@sha256:0000000000000000000000000000000000000000000000000000000000000000
    timeout: 30m
    commands:
      - |
        if [ -n "${KODEMA_CA_FILE:-}" ]; then
          export NODE_EXTRA_CA_CERTS="$KODEMA_CA_FILE"
          export SSL_CERT_FILE="$KODEMA_CA_FILE"
          export CURL_CA_BUNDLE="$KODEMA_CA_FILE"
          export GIT_SSL_CAINFO="$KODEMA_CA_FILE"
        fi
        export ANTHROPIC_BASE_URL="${KODEMA_LLM_ANTHROPIC_BASE_URL:-${KODEMA_URL}/llm/anthropic}"
        IFS= read -r ANTHROPIC_API_KEY < /kodema/secrets/KODEMA_TOKEN || [ -n "$ANTHROPIC_API_KEY" ]
        export ANTHROPIC_API_KEY
        claude -p "Implement the requested task and verify tests pass" --dangerously-skip-permissions
```

### Codex

```yaml
actions:
  - name: codex
    image: registry.example.com/kodema/codex-harness@sha256:0000000000000000000000000000000000000000000000000000000000000000
    timeout: 30m
    commands:
      - |
        if [ -n "${KODEMA_CA_FILE:-}" ]; then
          export NODE_EXTRA_CA_CERTS="$KODEMA_CA_FILE"
          export SSL_CERT_FILE="$KODEMA_CA_FILE"
          export CURL_CA_BUNDLE="$KODEMA_CA_FILE"
          export GIT_SSL_CAINFO="$KODEMA_CA_FILE"
        fi
        export OPENAI_BASE_URL="${KODEMA_LLM_OPENAI_BASE_URL:-${KODEMA_URL}/llm/openai/v1}"
        IFS= read -r OPENAI_API_KEY < /kodema/secrets/KODEMA_TOKEN || [ -n "$OPENAI_API_KEY" ]
        export OPENAI_API_KEY
        codex exec "Implement the requested task and verify tests pass"
```

### Pi

```yaml
actions:
  - name: pi
    image: registry.example.com/kodema/pi-harness@sha256:0000000000000000000000000000000000000000000000000000000000000000
    timeout: 30m
    commands:
      - |
        if [ -n "${KODEMA_CA_FILE:-}" ]; then
          export NODE_EXTRA_CA_CERTS="$KODEMA_CA_FILE"
          export SSL_CERT_FILE="$KODEMA_CA_FILE"
          export CURL_CA_BUNDLE="$KODEMA_CA_FILE"
          export GIT_SSL_CAINFO="$KODEMA_CA_FILE"
        fi
        export ANTHROPIC_BASE_URL="${KODEMA_LLM_ANTHROPIC_BASE_URL:-${KODEMA_URL}/llm/anthropic}"
        export OPENAI_BASE_URL="${KODEMA_LLM_OPENAI_BASE_URL:-${KODEMA_URL}/llm/openai/v1}"
        IFS= read -r ANTHROPIC_API_KEY < /kodema/secrets/KODEMA_TOKEN || [ -n "$ANTHROPIC_API_KEY" ]
        IFS= read -r OPENAI_API_KEY < /kodema/secrets/KODEMA_TOKEN || [ -n "$OPENAI_API_KEY" ]
        export ANTHROPIC_API_KEY OPENAI_API_KEY
        pi -p "Implement the requested task and verify tests pass"
```

## Minimal coding-harness base image

Use this `Containerfile` to build a small, reproducible coding-harness base image with `node`, `git`, `jq`, `curl`, `ca-certificates`, OpenCode, and a native-proxy environment helper:

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

Build and pin the digest before referencing it in a pipeline:

```sh
podman build -t localhost/kodema-opencode-harness:latest -f Containerfile .
```

## Inspecting per-execution token usage

Every proxied LLM call is attributed to the execution ID in `model_usage` and returned by `execution.inspect` (REST and MCP):

```sh
kodema inspect <execution-id>
kodema inspect <execution-id> --output json
```

When an execution has made model calls, `kodema inspect` prints a `model_usage` summary line (`requests`, `input_tokens`, `output_tokens`, `total_tokens`) and a per-provider/model breakdown table.
