# Pipeline format (`.kodama/<processname>.yaml`)

**Status:** Layer 1 pipeline contract under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md) and [O-38](../decisions.md#pipeline-runtime-and-four-layer-refinement--2026-10-30-o-38). Inspired by Drone CI.

Pipelines live in a repository under `.kodama/<processname>.yaml` (for example `.kodama/sdlc.yaml`, `.kodama/test.yaml`, `.kodama/release.yaml`) or are registered through the API / `kodactl`. The complete parsed document is persisted once in `pipelines.definition` (JSONB). Runs snapshot it; Git retains original YAML comments and formatting.

Pipeline names are unique within a project. The repository is the definition source, not a second naming scope. A conflicting definition source is refused.

This is a proposed authoring contract for the simplification phases, not the legacy validator's accepted syntax. Exact wait and condition grammar is frozen by the contract checkpoint in the [simplification plan](../implementation/pipeline-runtime-simplification.md).

## Example `.kodama/sdlc.yaml`

```yaml
name: sdlc
triggers:
  git:
    branches: [main]

services:
  db:
    scope: pipeline
    image: postgres:16-alpine
    env:
      POSTGRES_USER: kodama
      POSTGRES_DB: app
    secrets: [TEST_DB_PASSWORD]
    ports: [5432]

steps:
  - name: plan
    image: ghcr.io/sst/opencode:latest
    secrets: [ANTHROPIC_API_KEY]
    commands:
      - opencode run "Plan ${KODAMA_TASK_ID} following /workspace/.context"

  - name: implement
    depends_on: [plan]
    image: ghcr.io/sst/opencode:latest
    secrets: [ANTHROPIC_API_KEY]
    commands:
      - opencode run "Implement ${KODAMA_TASK_ID} following /workspace/.context"

  - name: review
    depends_on: [implement]
    wait:
      for: reviewer
      input: approval
      timeout: 72h

  - name: test
    depends_on: [review]
    when: review.outcome == approved
    image: golang:1.27-alpine
    env:
      DATABASE_URL: "postgres://kodama:${TEST_DB_PASSWORD}@db:5432/app?sslmode=disable"
    secrets: [TEST_DB_PASSWORD]
    commands:
      - go test -v ./...

  - name: rework
    depends_on: [review]
    when: review.outcome == rejected
    image: ghcr.io/sst/opencode:latest
    commands:
      - opencode run "Rework ${KODAMA_TASK_ID}"
```

## Fields

| Field | Rule |
| --- | --- |
| `name` | Process name matching the `.kodama/<name>.yaml` filename (`^[a-z][a-z0-9_-]{0,62}$`), unique within the project |
| `triggers.git` | Optional `branches`, `tags`, or `events` (`push`, `tag`, `merge`) that start this pipeline |
| `services` | Optional map of support containers (`scope`: `pipeline` [default], `project`, or `tenant`; `image`, `env`, `secrets`, `ports`) |
| `steps` | Ordered list of execution steps sharing `/workspace` |
| `steps[].name` | Unique step key within the pipeline (`run_steps.step_key`) |
| `steps[].depends_on` | Optional list of prerequisite step names; defaults to sequential execution |
| `steps[].image` | Container image for a container step |
| `steps[].commands` | Shell commands executed inside `/workspace` |
| `steps[].env` | Plaintext environment variables injected into the step container |
| `steps[].secrets` | Names of project or tenant secret references injected into the step container |
| `steps[].timeout` | Optional step timeout (for example `30m`, default `15m`) |
| `steps[].when` | Optional bounded condition over declared inputs, prior step results, and wait outcomes |
| `steps[].wait` | Makes the step a durable wait: `for` (role or external correlation), `input`, and optional `timeout`; no image or commands |

A wait step holds no workload. It pauses the same run and records its identity, expected input, accepted result, deadline, and continuation. A signal is not approval by itself; the recorded outcome selects the pipeline's next branch.

Task progression is not configured in pipeline YAML. A task selects the pipeline and the pipeline defines its process. There is no project `task_workflow` or state-to-pipeline trigger map.

## Built-in runtime environment

Every step automatically mounts `/workspace` (with the prepared source and `<tenant>/context` plus `<tenant>/<project>/context` under `/workspace/.context`) and receives:

- `KODAMA_URL`: internal Kodama API, AI Gateway, and MCP endpoint origin
- `KODAMA_TOKEN`: project-space run authority for Git fetch/push, task reads, wait requests, artifact registration, AI Gateway, and MCP access
- `KODAMA_TENANT`, `KODAMA_PROJECT`, `KODAMA_REPO`, `KODAMA_RUN_ID`, `KODAMA_STEP_KEY`, `KODAMA_TASK_ID`, `KODAMA_COMMIT_SHA`, `KODAMA_BRANCH`

The kernel receives these as declared execution values. It does not know how the shell minted the token or where Git stores the repository.

## Validation

Pipeline YAML is validated on Git indexing and on `kodactl pipeline apply`:

- Reject unknown top-level fields, duplicate step names, dependency cycles in `depends_on`, invalid service scopes, invalid wait/condition shapes, and a step that declares both `wait` and container fields.
- Refuse a name that already belongs to another definition source in the project.
- Persist the validated document directly on the `pipelines` table.

Indexing a definition and admitting a triggered run are idempotent on the event, ref, and commit identity. A failed definition never displaces the previous valid one.
