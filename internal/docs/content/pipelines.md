# Kodema Pipelines

Pipelines are ordered sequences of actions executed one at a time inside a single execution.

## Execution rules

- Actions execute strictly in declaration order, with at most one active action per execution.
- There are **no** `when` conditions, loops, backward jumps, parallel branches, DAGs, joins, or YAML anchors/aliases. Put branching logic inside container scripts or choose a different pipeline.
- Every action has a finite timeout (Go duration from `1m` to `720h` / 30 days, defaulting to the installation default).
- Admission snapshots exact YAML bytes and resolves container images to immutable digests; later edits to the pipeline do not change an admitted execution.
- Only succeeded actions produce downstream outputs. Action failure stops the execution and cancels remaining actions with `upstream_failed`; explicit cancellation cancels remaining actions with `requested`.

## Schema

```yaml
inputs: [repo_path, resource_id, target_ref, expected_base]
env:
  LOG_LEVEL: info

actions:
  - name: implement
    image: registry.example.com/coding-agent:1
    timeout: 30m
    outputs: [candidate]
    secrets: [BUILD_REGISTRY_AUTH]
    env:
      AGENT_MODE: batch
    resources:
      memory: 4Gi
      cpu: "2"
    commands:
      - git clone "$KODEMA_GIT_URL/$(cat /kodema/inputs/repo_path)" /workspace/repo
      - cd /workspace/repo && run-agent
      - git -C /workspace/repo rev-parse HEAD > /kodema/outputs/implement/candidate

  - name: review
    request: role/reviewer
    timeout: 72h
    outputs: [notes]
    subject:
      operation: git.ref.update
      resource: {from: inputs.resource_id}
      ref: {from: inputs.target_ref}
      candidate: {from: implement.outputs.candidate}
      expected_base: {from: inputs.expected_base}

  - name: integrate
    image: registry.example.com/git-tools:1
    timeout: 5m
    commands:
      - |
        git -C /workspace/repo push origin \
          "$(cat /kodema/outputs/implement/candidate):$(cat /kodema/inputs/target_ref)"
```

## Container actions

- Required: `name`, `image`, `commands`.
- Optional: `env` (string literals only; action values override pipeline-level `env`), `secrets` (declared secret names resolved at activation), `outputs` (declared UTF-8 keys; up to 8 keys, 4 KiB max each at submission), `timeout`, `resources` (`memory`, `cpu`).
- Exit code `0` succeeds; nonzero fails. Commands run in `/workspace` with `/kodema/bin` appended to `PATH`.
- `KODEMA_URL`, `KODEMA_EXECUTION`, `KODEMA_ACTION`, `KODEMA_TASK` and secret name `KODEMA_TOKEN` are reserved and refused at apply. Runtime-supplied `KODEMA_CA_FILE`, `KODEMA_GIT_URL`, `KODEMA_LLM_<PROVIDER>_BASE_URL` (such as `KODEMA_LLM_OPENAI_BASE_URL`, `KODEMA_LLM_ANTHROPIC_BASE_URL`, `KODEMA_LLM_OPENCODE_BASE_URL`, and custom providers), and `GIT_CONFIG_COUNT` / `GIT_CONFIG_KEY_*` / `GIT_CONFIG_VALUE_*` cannot be shadowed when configured by the installation. Secrets owned by configured LLM proxy providers cannot be mounted into containers via `secrets: [...]`; workloads authenticate to `/llm/<provider>/*` with `/kodema/secrets/KODEMA_TOKEN`.

## Request actions

- Required: `name`, `request` (`role/<role>` or `service/<id>`), `subject`.
- Optional: `timeout` (defaults to the installation default), `outputs` (declared response keys provided by the responder).
- Runs **no container** and revokes the execution credential while waiting, while retaining `/workspace` for subsequent actions.
- `subject` values must be literals or direct lookups only: `{from: inputs.<key>}` or `{from: <earlier-action>.outputs.<key>}`. No string interpolation, self-references, or forward references.
- For approval-guarded effects (such as `git.ref.update`), include `operation`, `resource` (the registered resource ID, e.g. `git-shop-web`), and the exact effect parameters (`ref`, `candidate`, `expected_base`) in `subject`. Extra subject fields act as immutable display context for the reviewer and are ignored during verification matching.

## Push triggers (`on.push`)

- Declaring `on.push` requires `repository` (a registered resource ID) and `service` (a service account ID; binding requires Admin authority).
- Refs match **exact** full ref names (for example `refs/heads/main`) or **trailing-slash prefixes** (for example `refs/heads/work/`), with no globs or regexes.
- Push pipelines may only declare inputs from `repository` (the registered resource ID), `ref`, `commit`, and `previous_commit`; no other inputs (even optional) may be declared, and only declared keys are populated.
- Ref deletions and execution-originated pushes never trigger `on.push` (preventing push loops).

```yaml
repository: git-shop-web
service: svc-ci
on:
  push: [refs/heads/main, refs/heads/work/]
inputs: [repository, ref, commit, previous_commit]
actions:
  - name: verify
    image: registry.example.com/ci:1
    commands:
      - git clone "$KODEMA_GIT_URL/shop/web" /workspace/repo
      - git -C /workspace/repo checkout --detach "$(cat /kodema/inputs/commit)"
```

## Applying and running pipelines

- Store/update named pipelines: `kodema pipeline apply <file-or-dir>` (calls `pipeline.apply`).
- Run a local YAML file inline without storing it: `kodema run <file> [--input k=v] [--detach]` (calls `pipeline.start`).
- Create and start a task on a stored pipeline: `kodema task run --title "..." --pipeline <name|id> [--input k=v]`.
