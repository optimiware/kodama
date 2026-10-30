# Pipeline format (`.kodama/<processname>.yaml`)

**Status:** Core Layer 1 pipeline contract under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md), inspired by Drone CI.

Pipelines live in a repository under `.kodama/<processname>.yaml` (for example `.kodama/plan.yaml`, `.kodama/implement.yaml`, `.kodama/review.yaml`, `.kodama/test.yaml`, `.kodama/preview.yaml`, `.kodama/promote.yaml`) or are registered via API / `kodactl`. The complete parsed document is persisted once in `pipelines.definition` (JSONB). Runs snapshot it; Git retains original YAML comments/formatting. This is a proposed authoring contract for Phase 3, not the legacy validator's accepted syntax.

## Example `.kodama/implement.yaml`

```yaml
name: implement
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
  - name: code
    image: ghcr.io/sst/opencode:latest
    secrets: [ANTHROPIC_API_KEY]
    commands:
      - opencode run "Implement task ${KODAMA_TASK_ID} following /workspace/.context"

  - name: test
    depends_on: [code]
    image: golang:1.27-alpine
    env:
      DATABASE_URL: "postgres://kodama:${TEST_DB_PASSWORD}@db:5432/app?sslmode=disable"
    secrets: [TEST_DB_PASSWORD]
    commands:
      - go test -v ./...

  - name: review-and-transition
    depends_on: [test]
    image: ghcr.io/optimiware/kodama:latest
    commands:
      - git add -A && git commit -m "Implement ${KODAMA_TASK_ID}" && git push origin HEAD
      - kodama agent run --skill review --on-pass review --on-fail implement
```

## Fields

[ADR 0012](../adr/0012-container-sdlc-and-artifact-releases.md) builds on the container primitive, with optional agent profiles and native artifact/release consumption. Its [planned contract](artifacts-and-releases.md) does not add publish/deploy/rollback step kinds or silently change this frozen format. Existing `artifact` references are IDs, not automatically downloaded files.

| Field | Rule |
| --- | --- |
| `name` | Process name matching the `.kodama/<name>.yaml` filename (`^[a-z][a-z0-9_-]{0,62}$`) |
| Task triggers | Stored once in `projects.task_workflow.triggers`, keyed by state; apply on creation, state entry and resumption |
| `triggers.git` | Optional `branches`, `tags`, or `events` (`push`, `tag`, `merge`) that trigger this pipeline |
| `services` | Optional map of support containers (`scope`: `pipeline` [default], `project`, or `tenant`; `image`, `env`, `secrets`, `ports`) |
| `steps` | Ordered list of container execution steps sharing `/workspace` |
| `steps[].name` | Unique step key within the pipeline (`run_steps.step_key`) |
| `steps[].depends_on` | Optional list of prerequisite step names; defaults to sequential execution |
| `steps[].image` | Container image to run (`opencode`, `claude-code`, `golang`, `kodama`, `ko`, `buildah`, etc.) |
| `steps[].commands` | Shell commands executed inside `/workspace` |
| `steps[].env` | Plaintext environment variables injected into the step container |
| `steps[].secrets` | Names of project or tenant `secrets` injected into the step container |
| `steps[].timeout` | Optional step timeout (for example `30m`, default `15m`) |

## Built-in runtime environment

Every step automatically mounts `/workspace` (with the repository checked out and `<tenant>/context` + `<tenant>/<project>/context` available in `/workspace/.context`) and receives:
- `KODAMA_URL`: Internal Kodama API / AI Gateway / MCP Gateway URL
- `KODAMA_TOKEN`: Project-space run token authorizing Git push/fetch, task state transitions, artifact registration, AI Gateway, and MCP Gateway access
- `KODAMA_TENANT`, `KODAMA_PROJECT`, `KODAMA_REPO`, `KODAMA_RUN_ID`, `KODAMA_STEP_KEY`, `KODAMA_TASK_ID`, `KODAMA_COMMIT_SHA`, `KODAMA_BRANCH`

## Validation

Pipeline YAML is validated on `git push` (for `.kodama/*.yaml`) and on `kodactl pipeline apply`:
- Rejects unknown top-level fields, duplicate step names, dependency cycles in `depends_on`, and invalid service scopes.
- Persists the validated YAML/JSONB directly on the `pipelines` table.
