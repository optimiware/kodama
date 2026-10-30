# Pipeline format (`.kodama/<processname>.yaml`)

**Status:** Layer 1 pipeline contract under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md) and [O-38](../decisions.md#pipeline-runtime-and-four-layer-refinement--2026-10-30-o-38). Inspired by Drone CI.

Pipelines live in a repository under `.kodama/<processname>.yaml` (for example `.kodama/sdlc.yaml`, `.kodama/test.yaml`, `.kodama/release.yaml`) or are registered through the API / `kodactl`. The complete parsed document is persisted once in `pipelines.definition` (JSONB). Runs snapshot it; Git retains original YAML comments and formatting.

Pipeline names are unique within a project. The repository is the definition source, not a second naming scope. A conflicting definition source is refused.

This is a proposed authoring contract for the simplification phases, not the legacy validator's accepted syntax; no parser, validator or engine implements it yet. The wait, condition, output and protected-step syntax below is the frozen baseline from the contract checkpoint in the [simplification plan](../implementation/pipeline-runtime-simplification.md#contract-decisions-to-freeze-before-fan-out). Runtime behavior for each construct (lifecycle, joins, decisions, deadlines) is owned by [pipeline execution](../specs/pipeline-execution.md); this contract owns only syntax, grammar, bounds and validation.

## Example `.kodama/sdlc.yaml`

An illustrative definition; image names, secrets and commands are placeholders.

```yaml
name: sdlc
inputs:
  base_branch: {type: string, default: main}

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
    outputs: [commit, question]
    commands:
      - opencode run "Implement ${KODAMA_TASK_ID}; write the pushed commit to $KODAMA_OUTPUT_DIR/commit and an optional clarifying question to $KODAMA_OUTPUT_DIR/question"

  - name: clarify
    depends_on: [implement]
    when: implement.outputs.question != ""
    wait:
      assign: engineer
      request: implement.outputs.question
      input: answer
      outcomes: [answered]
      timeout: 24h

  - name: review
    depends_on: [implement, clarify]
    join: any
    wait:
      assign: reviewer
      correlate:
        candidate: implement.outputs.commit
      input: approval
      outcomes: [approved, rejected]
      approving: approved
      timeout: 72h
      on_expiry: fail

  - name: test
    depends_on: [review]
    when: review.outcome == "approved"
    image: golang:1.27-alpine
    env:
      DATABASE_URL: "postgres://kodama:${TEST_DB_PASSWORD}@db:5432/app?sslmode=disable"
    secrets: [TEST_DB_PASSWORD]
    commands:
      - go test -v ./...

  - name: integrate
    depends_on: [test]
    authority: integrate
    requires:
      - {wait: review, outcome: approved}
    image: ghcr.io/example/integrator:latest
    commands:
      - integrate-candidate

  - name: rework
    depends_on: [review]
    when: review.outcome == "rejected"
    image: ghcr.io/sst/opencode:latest
    bind:
      REVIEW_NOTES: review.answer
    commands:
      - opencode run "Rework ${KODAMA_TASK_ID} addressing $REVIEW_NOTES"
```

The `rework` step is a declared forward step. The revised candidate is reviewed by a further declared wait step or by a new run that continues from it; neither reopens `review`.

## Fields

| Field | Rule |
| --- | --- |
| `name` | Process name matching the `.kodama/<name>.yaml` filename (`^[a-z][a-z0-9_-]{0,62}$`), unique within the project |
| `inputs` | Optional map of run inputs: name (`^[a-z][a-z0-9_]{0,62}$`) to `{type: string\|integer\|boolean, default, required}`; at most 16; a string value is at most 256 bytes. Admission snapshots the values; an undeclared or mistyped input is refused |
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
| `steps[].join` | `all` (default) or `any`; how skipped prerequisites combine, see [Joins](../specs/pipeline-execution.md#eligibility-skipping-and-joins). `any` needs at least two prerequisites |
| `steps[].when` | Optional [condition](#conditions) over declared inputs, prerequisite step results, and wait outcomes |
| `steps[].outputs` | Container steps only. Up to 8 output names (`^[a-z][a-z0-9_]{0,31}$`) the step may record; see [Outputs and bindings](#outputs-and-bindings) |
| `steps[].bind` | Container steps only. Up to 8 `ENV_NAME: <reference>` entries injecting a visible value into the step environment |
| `steps[].wait` | Makes the step a durable wait; see [Waits](#waits). No image, commands, `env`, `secrets`, `outputs` or `bind` |
| `steps[].requires` | Protected steps only: up to 4 `{wait: <step>, outcome: <name>}` entries, each naming a prerequisite wait and that wait's `approving` outcome; see [Protected steps](#protected-steps) |
| `steps[].authority` | Optional protected-step marker. The only value is `integrate`; it requires a non-empty `requires` |

## Waits

```yaml
wait:
  assign: reviewer          # who may decide: an opaque role label
  correlate:                # what is being decided: immutable key values
    candidate: implement.outputs.commit
  request: implement.outputs.question   # optional bounded text shown to the resolver
  input: approval           # kind of input expected
  outcomes: [approved, rejected]
  approving: approved       # the one outcome that can authorize a protected step
  timeout: 72h
  on_expiry: fail           # fail (default) | continue
```

| Field | Rule |
| --- | --- |
| `assign` | Optional opaque label naming the role that may resolve the wait through the inbox. The shell validates it against known roles when the definition is indexed or applied; the kernel never interprets it |
| `correlate` | Optional map of up to 4 keys (`^[a-z][a-z0-9_]{0,31}$`) to [references](#references). The values are read when the wait opens and are immutable thereafter. An external signal must present every key and value |
| `request` | Optional reference to a declared output of a prerequisite container step. Its value (at most 4 KiB) is recorded with the wait when it opens |
| `input` | Required kind label (`^[a-z][a-z0-9_-]{0,31}$`) describing the input the resolver supplies. The accepted answer is optional UTF-8 text of at most 4 KiB; a structured answer schema is not part of this contract |
| `outcomes` | Required closed set of 1–8 outcome names (`^[a-z][a-z0-9_]{0,31}$`). A resolver may choose only one of them. `expired` and `cancelled` are reserved and cannot be declared |
| `approving` | Optional member of `outcomes`: the single outcome that counts as approval. It is required when a protected step names the wait in `requires`, and `expired` cannot be approving. No other outcome, including one named `rejected`, can ever authorize a protected step |
| `timeout` | Required duration from 1 minute to 30 days, measured from the commit that opens the wait |
| `on_expiry` | `fail` (default) fails the step with reason `wait_expired`. `continue` records the reserved outcome `expired` and settles the step as succeeded, so `when` can branch on `<wait>.outcome == "expired"`; `requires` cannot name `expired` |

At least one of `assign` and `correlate` is required; assignment and correlation are separate fields because they answer different questions and change under different rules ([assignment and correlation](../specs/pipeline-execution.md#assignment-and-correlation)). A wait step holds no workload. `outcome` is always one of the declared outcomes or `expired`; Only `approving` carries meaning to the engine; every other outcome, including `rejected`, is ordinary data that the author branches on with `when`.

## References

A reference reads one declared value:

```text
reference := "inputs." name
           | step "." ( "status" | "outcome" | "answer" | "outputs." name )
```

| Reference | Value |
| --- | --- |
| `inputs.<name>` | A declared run input |
| `<step>.status` | `succeeded` or `skipped` for a settled prerequisite; always defined, see [missing values](#conditions) |
| `<step>.outcome` | The recorded outcome of a wait step |
| `<step>.answer` | The recorded answer text of a wait step |
| `<step>.outputs.<name>` | A declared output of a succeeded container step |

**Visibility.** `when`, `bind`, `request`, `correlate` and `requires` may reference only declared inputs and steps that are transitive prerequisites through `depends_on`. A reference to an undeclared input, output, or non-prerequisite step is a validation error. `outcome` and `answer` apply to wait steps only; `outputs` applies to container steps only. A `failed` or `cancelled` step is never a visible prerequisite because it ends the run before a dependent is evaluated.

## Conditions

`when` is a predicate over references and literals. It is the only conditional construct: **there is no `condition` step kind**, no expression language beyond this grammar, and no way to execute code, call a function, read the environment, time, the shell, or the task. A condition step is added only by amending this contract with a requirement that `when` cannot express.

```text
expr    := and ( "||" and )*
and     := not ( "&&" not )*
not     := "!" not | "(" expr ")" | cmp | operand
cmp     := operand ( "==" | "!=" | "<" | "<=" | ">" | ">=" ) operand
         | operand "in" "[" literal ( "," literal )* "]"
operand := reference | literal
literal := '"' string '"' | integer | "true" | "false"
```

- **Types.** Outputs, outcomes, answers and `status` are strings. Inputs have their declared type. Comparison operands must have the same type; there is no coercion. `<`, `<=`, `>` and `>=` apply only to integer inputs and integer literals. A bare operand is valid only when boolean.
- **Missing values.** `<step>.status` is never missing: a prerequisite is terminal when `when` is evaluated, so it is `succeeded` or `skipped`. Every other reference is *missing* when its step was skipped, the output was not recorded, or the answer is absent. A skipped step therefore has `status == "skipped"` while its `outcome`, `answer` and `outputs.*` are missing. A comparison (including `in`) with a missing operand is `false`, including `!=`; a bare missing boolean is `false`; `!` then negates that result. Validation rejects what it can prove invalid; a missing value is a runtime outcome, never an error.
- **Literal checks.** A literal compared with `<wait>.outcome` must be a declared outcome of that wait, or `expired` when `on_expiry` is `continue`; a literal compared with `<step>.status` must be `succeeded` or `skipped`. Timeout branching uses `when` only.
- **Bounds.** At most 256 bytes of expression text, 8 comparisons, nesting depth 4, string literals of at most 64 bytes, `in` lists of at most 8 literals, integers within signed 64 bits. A value longer than a literal compares unequal; it is never truncated.
- **Evaluation** is deterministic and side-effect free. When it happens and what is recorded is owned by [pipeline execution](../specs/pipeline-execution.md#eligibility-skipping-and-joins).

## Outputs and bindings

A container step records a result by writing at most one regular file per declared output name under the directory named by `KODAMA_OUTPUT_DIR`, an ephemeral per-step directory outside `/workspace`. Each file is UTF-8 text of at most 4 KiB and is recorded only if the step settles as succeeded; see [agent-requested waits](../specs/pipeline-execution.md#agent-requested-waits). An undeclared name, oversize value or non-text file is discarded and recorded as a bounded diagnostic. `bind` injects the current value of a visible reference as an environment variable of at most 4 KiB; a missing value leaves the variable unset. A binding never targets a name that `env`, `secrets` or the built-in environment already defines.

## Protected steps

`authority: integrate` marks a step that may request integration authority from the shell; it carries no other meaning to the kernel. The same rules apply to any authority value added later. `requires` is valid only on a protected step, which must declare a non-empty `requires`. Each entry names a wait step that is a transitive prerequisite and that wait's `approving` outcome. Naming any other outcome, including a rejection or `expired`, is a validation error, and so is naming a wait that declares no `approving` outcome. `requires` combines conjunctively with `when` and applies even when `when` is absent or `join: any` is used, so a protected step is never reachable unconditionally and only an approving decision on the required waits makes it eligible. Every required wait of an `integrate` step must declare `correlate.candidate`; the shell compares it with the pushed commit ([integration](../specs/git-repositories.md#integration)).

A refused definition, because `requires` names a rejection:

```yaml
name: refused
steps:
  - name: implement
    image: example/agent:latest
    outputs: [commit]
    commands: ["agent run"]
  - name: review
    depends_on: [implement]
    wait:
      assign: reviewer
      correlate: {candidate: implement.outputs.commit}
      input: approval
      outcomes: [approved, rejected]
      approving: approved
      timeout: 72h
  - name: integrate
    depends_on: [review]
    authority: integrate
    requires:
      - {wait: review, outcome: rejected}
    image: example/integrator:latest
    commands: ["integrate-candidate"]
```

## Built-in runtime environment

Every step automatically mounts `/workspace` (with the prepared source and `<tenant>/context` plus `<tenant>/<project>/context` under `/workspace/.context`) and receives:

- `KODAMA_URL`: internal Kodama API, AI Gateway, and MCP endpoint origin
- `KODAMA_TOKEN`: project-space run authority for Git fetch/push, task reads, artifact registration, AI Gateway, and MCP access. It cannot open, resolve, or answer a wait
- `KODAMA_OUTPUT_DIR`: the per-step [output directory](#outputs-and-bindings)
- `KODAMA_TENANT`, `KODAMA_PROJECT`, `KODAMA_REPO`, `KODAMA_RUN_ID`, `KODAMA_STEP_KEY`, `KODAMA_TASK_ID`, `KODAMA_COMMIT_SHA`, `KODAMA_BRANCH`

The kernel receives these as declared execution values. It does not know how the shell minted the token or where Git stores the repository.

## Validation

Pipeline YAML is validated on Git indexing and on `kodactl pipeline apply`:

- Reject unknown top-level fields, duplicate step names, dependency cycles in `depends_on`, invalid service scopes, and a step that declares both `wait` and container fields. There is no loop, `goto`, retry-from or backward dependency construct: a definition describes an acyclic plan.
- Reject a wait without `assign` or `correlate`, without `outcomes` or `timeout`, with a timeout outside its bounds, or declaring a reserved outcome.
- Reject a `when`, `bind`, `request`, `correlate` or `requires` that violates the [reference visibility, type, and bound rules](#conditions).
- Reject `requires` on a step without `authority`, `authority` without `requires`, a `requires` entry whose outcome is not the named wait's `approving` outcome (including `expired` and any rejection), a required wait with no `approving` outcome, and an `integrate` step whose required wait lacks `correlate.candidate`.
- Reject a literal compared with `outcome` or `status` that the [literal checks](#conditions) do not permit, an `approving` value that is not a declared outcome, and an unknown field in a `requires` entry.
- Reject more than 8 outputs or bindings on a step, or a binding that collides with an existing variable.
- Refuse a name that already belongs to another definition source in the project.
- Persist the validated document directly on the `pipelines` table.

Indexing a definition and admitting a triggered run are idempotent on the event, ref, and commit identity. A failed definition never displaces the previous valid one.

## Requirements

| ID | Requirement |
| --- | --- |
| PF-01 | A definition MUST be an acyclic plan: `depends_on` MUST NOT form a cycle and the format MUST NOT define any loop, retry-from, or backward-reference construct; rework MUST be expressed as declared forward steps or a new run. |
| PF-02 | A wait MUST declare a closed non-empty outcome set, a bounded timeout, and at least one of `assign` and `correlate`; `expired` and `cancelled` MUST be reserved. |
| PF-03 | `when`, `bind`, `request`, `correlate` and `requires` MUST use only the fixed references, operators, types and bounds above; a reference MUST be visible; the format MUST NOT contain a `condition` step kind or executable expressions. |
| PF-04 | Outputs and bindings MUST be declared, bounded, and limited to container steps; a container MUST NOT be able to declare a wait or suspend itself. |
| PF-05 | A step with `authority` MUST declare `requires` naming prerequisite waits and each wait's `approving` outcome, `requires` MUST NOT appear on an unprotected step, and an `integrate` step's required waits MUST declare `correlate.candidate`. |
| PF-06 | Only a wait's single declared `approving` outcome MAY authorize a protected step; a rejection, any other outcome and `expired` MUST be a validation error in `requires`, and timeout branching MUST use `when` only. |

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| PF-A01 | The example above validates; a dependency cycle, a loop-like field, a wait that also declares an image, a wait without `assign`/`correlate`, an empty or reserved outcome, and an out-of-range timeout are each refused with a field-specific error | PF-01, PF-02 |
| PF-A02 | Fixtures cover every operator, a missing operand on `==`, `!=`, `in` and under `!`, a skipped step whose `status == "skipped"` is true while its `outcome`, `answer` and `outputs.*` comparisons are false, a reference to a non-prerequisite step, a literal that is not a declared outcome or status, a type mismatch, ordering on a string, and each bound at and beyond its limit | PF-03 |
| PF-A03 | A container with 9 outputs, an oversize binding, or a binding colliding with a built-in variable is refused; a definition cannot declare a wait inside a container step | PF-04 |
| PF-A04 | `authority: integrate` without `requires`, `requires` on an unprotected step, `requires` naming a non-prerequisite, a wait with no `approving` outcome, or a wait lacking `correlate.candidate` is refused | PF-05 |
| PF-A05 | The refused definition above, whose protected step requires `review` with the `rejected` outcome, is refused with an error naming the step and outcome; the same document with `outcome: approved` validates | PF-06 |
| PF-A06 | A definition with `on_expiry: continue` and a step `when: review.outcome == "expired"` validates; a protected step `requires` naming `expired`, and a `when` comparing `outcome` to an undeclared literal, are refused | PF-03, PF-06 |
