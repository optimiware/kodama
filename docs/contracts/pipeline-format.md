# Pipeline format

**Status:** proposed `kodama/v1alpha1`, an API-managed document rather than a CRD.

```yaml
apiVersion: kodama/v1alpha1
kind: Pipeline
metadata:
  name: test
spec:
  inputs: {}
  steps:
    - id: test
      kind: container
      image: registry.example.invalid/go-tools@sha256:1111111111111111111111111111111111111111111111111111111111111111
      workspace: {access: read, from: run}
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace && go test -json ./... > /kodama/outputs/tests.json"]
      timeoutSeconds: 600
      outputs:
        report: {type: artifact, path: /kodama/outputs/tests.json}
```

Images/digests are placeholders. Source repository comes from the work item's workspace; `sourceCommit` is pinned at run creation. Commands execute through [kodama step](../implementation/step-entrypoint.md). Read access means no publication, not a filesystem incapable of holding build-tool scratch changes.

## Fields

| Field | Rule |
| --- | --- |
| `metadata.name` / step `id` | Lowercase letter followed by lowercase letters/digits/hyphens; maximum 63 |
| `spec.inputs` | Map of declarations with `type`, optional `default`, `required`; string/boolean/integer/artifact |
| `kind` | `container`, `agent`, `gate` |
| `needs` | Ancestor IDs; empty by default |
| `when` | Optional `{input: name, equals: scalar}` for executable steps only |
| `workspace` | `access` is `none`, `read`, or `write`; `from` is `run` or an ancestor writer ID for read/write |
| `environment` | Optional project environment ID for a delivery step; admission pins its configuration and grants only its scoped actions/credentials |
| `image`, `command`, `args`, `env` | Container definition; image digest required, command/args are arrays |
| `timeoutSeconds` | Positive execution deadline within configured limits |
| `retry` | `maxAttempts` including first; `backoffSeconds`; `on: [infrastructure, transient]` |
| `outputs` | Named string/boolean/integer/artifact/ociArtifact declarations |

Default attempt count is one. Command failures and unknown external outcomes are not automatically retryable. Runtime resources and startup deadlines come from fixed project/platform settings with bounded CPU/memory requests. File outputs must be inside `/kodama/outputs`; paths and symlinks cannot escape the root.

`env` accepts nonsecret scalars, full-scalar expressions, or `{secretRef: "secret://project/name"}` where explicitly permitted. O-19 explicitly permits declared secret-to-ENV bindings for ordinary build/script steps: pin the authorized version at admission, audit resolution and inject only into the intended child process, never as plaintext in the Kubernetes Job/Pod manifest. Prefer `secretFiles: {name: "secret://project/name"}` mounted under `/kodama/secrets`; secret files never enter commits/outputs. Profiles and run admission pin all declared secret versions. Agent model/tool credentials normally stay in their proxies; an agent does not implicitly inherit a build step's secrets. This specifies intended delivery semantics, not a change to the frozen runtime DTOs without their owner's implementation/qualification.

## Agent and gate forms

```yaml
id: implement
kind: agent
agent: {profile: coding-v1}
workspace: {access: write, from: run}
timeoutSeconds: 1800
```

The immutable profile supplies the image/wrapper and pinned work-item instruction context. No new executor is introduced. `needs_input` fails the attempt with a question; continuation is a new run.

```yaml
id: review
kind: gate
needs: [test]
gate:
  evidence: ["${{ steps.test.outputs.report }}"]
```

Gates are unconditional, require one project reviewer, and have no image, retries, workspace, or Pod. Optional `gate.deploymentId` binds a prior deployment output for application review. The server adds source/content/definition and configuration to the subject. Every successful path through a gate's successors must pass that gate.

## Expressions and source

Only whole-scalar `${{ inputs.name }}` and `${{ steps.id.outputs.name }}` references are supported. Producer references require a dependency path and successful finalized outputs. Artifact references resolve to registered IDs, not raw arbitrary URLs. Use argument arrays/environment variables, not interpolation into shell programs.

`workspace.from: run` selects the original source commit; a writer ID selects its published commit. Writers must be totally ordered in the DAG. A write after approval requires a new applicable gate before protected successors. Each successful writer has an implicit `commit` output. Branch movement does not change existing read inputs.

## Result outputs

The [entrypoint](../implementation/step-entrypoint.md) owns the runtime protocol. A command can write `/kodama/results.json` for scalar/OCI outputs; declared files are collected by the wrapper. Scalars and object metadata must match declarations. OCI registrations verify ownership, registry digest, and source provenance.

```json
{
  "outcome": "completed",
  "outputs": {"image": {"digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "registryRef": "registry.example.invalid/inventory"}}
}
```

This example requires an `image: {type: ociArtifact}` declaration. Failed steps can still retain diagnostics; their success outputs do not become available to descendants.

## Validation

Reject unknown fields, cycles, invalid types, missing ancestors, parallel writers, mutable image tags, undeclared secret access, unsupported profile versions, and conditional gates. Registration validates then stores an immutable canonical definition/digest; repeating identical content returns its existing version. Local CLI validation uses the same structural/semantic rules; no validation-only server resource is needed.
