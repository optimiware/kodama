# Example: ERP inventory gap to preview

**Target:** M2. Images, profiles, commands, and scripts below are implementation fixtures, not published software.

The analyst creates `wi_1042` with specification/plan sections and acceptance criteria: application-tenant isolation, audited resolution, idempotent ERP writes, and warehouse usability. The engineer selects the Go/PostgreSQL blueprint, pulls the item into active WIP, and provisions `refs/heads/kodama/work/wi_1042`.

Humans edit through Git. The run pins source commit and work-item content revision. The `coding-v1` profile is an OpenCode wrapper that reads that snapshot and proposes a committed implementation through the controlled publisher.

```yaml
apiVersion: kodama/v1alpha1
kind: Pipeline
metadata:
  name: inventory-preview
spec:
  inputs: {}
  steps:
    - id: implement
      kind: agent
      agent: {profile: coding-v1}
      workspace: {access: write, from: run}
      timeoutSeconds: 1800
    - id: test
      kind: container
      needs: [implement]
      image: registry.example.invalid/go-tools@sha256:1111111111111111111111111111111111111111111111111111111111111111
      workspace: {access: read, from: implement}
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace && ./ci/quality.sh"]
      outputs:
        report: {type: artifact, path: /kodama/outputs/quality.json}
    - id: review
      kind: gate
      needs: [test]
      gate:
        evidence: ["${{ steps.test.outputs.report }}"]
    - id: build
      kind: container
      needs: [review]
      image: registry.example.invalid/builder@sha256:2222222222222222222222222222222222222222222222222222222222222222
      workspace: {access: read, from: implement}
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace && ./ci/build.sh"]
      outputs:
        image: {type: ociArtifact}
    - id: preview
      kind: container
      needs: [build]
      environment: env_preview
      image: registry.example.invalid/delivery@sha256:3333333333333333333333333333333333333333333333333333333333333333
      workspace: {access: read, from: implement}
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace && ./ci/deploy-preview.sh"]
      env:
        IMAGE_ARTIFACT_ID: "${{ steps.build.outputs.image }}"
      outputs:
        deploymentId: {type: string}
        previewUrl: {type: string}
```

## Script contracts

- `quality.sh` writes the declared report and fails on required check failure.
- `build.sh` runs a restricted/rootless builder, pushes/verifies the OCI digest, and writes the image result manifest.
- `deploy-preview.sh` creates a deployment row with the stable action key before applying namespace-scoped resources. It reconciles uncertain outcomes, reports readiness, and writes deployment/URL outputs. Its declared environment supplies scoped credentials; ordinary runtime settings bound to that environment are explicit.

The reviewer sees the source commit, content revision, and test report. The gate waits without a Pod. Subsequent build/source inputs remain pinned; a concurrent human edit invalidates applicability rather than silently modifying the run.

Successful preview execution does not complete the work item. Continue with [staging and acceptance](staging-and-acceptance.md). A `needs_input` agent result records its question and fails; a human revises the specification and starts a new run.
