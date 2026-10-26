# Example: staging and acceptance

**Target:** M3, continuing [preview](erp-gap-to-preview.md). The pipelines here are `kodama/v1alpha1` (current executable format); see the [run-shared example](run-shared-build-and-services.md) for the `kodama/v1alpha2` form.

Start a delivery run using the exact successful source commit, current content revision, and verified image artifact. Admission checks artifact provenance. The source is the candidate; no additional candidate/checkpoint IDs are needed.

```yaml
apiVersion: kodama/v1alpha1
kind: Pipeline
metadata:
  name: inventory-stage
spec:
  inputs:
    image: {type: artifact, required: true}
  steps:
    - id: stage
      kind: container
      environment: env_stage
      image: registry.example.invalid/delivery@sha256:3333333333333333333333333333333333333333333333333333333333333333
      workspace: {access: read, from: run}
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace && ./ci/deploy-stage.sh"]
      env:
        IMAGE_ARTIFACT_ID: "${{ inputs.image }}"
      outputs:
        deploymentId: {type: string}
    - id: e2e
      kind: container
      needs: [stage]
      image: registry.example.invalid/test-tools@sha256:4444444444444444444444444444444444444444444444444444444444444444
      workspace: {access: read, from: run}
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace && ./ci/e2e.sh"]
      env:
        DEPLOYMENT_ID: "${{ steps.stage.outputs.deploymentId }}"
      outputs:
        report: {type: artifact, path: /kodama/outputs/e2e.json}
    - id: exploratory-qa
      kind: agent
      needs: [e2e]
      agent: {profile: exploratory-qa-v1}
      workspace: {access: none}
      outputs:
        report: {type: artifact, path: /kodama/outputs/qa.json}
    - id: business-review
      kind: gate
      needs: [exploratory-qa]
      gate:
        deploymentId: "${{ steps.stage.outputs.deploymentId }}"
        evidence:
          - "${{ steps.e2e.outputs.report }}"
          - "${{ steps.exploratory-qa.outputs.report }}"
```

All images/profiles are placeholders. The QA profile reads authorized upstream deployment outputs and pinned acceptance context. It uses permitted browser tools, retains screenshots/reports, and files ordinary defect work items with evidence links.

The stage script registers the durable deployment row before apply. The image digest matches preview; runtime configuration is explicit and hashed. Tests/QA identify the deployment configuration actually inspected. Changing that deployment invalidates old evidence for a new review subject.

The analyst with reviewer authority inspects the application and decides the business-review gate. Then an explicit `review → done` transition records human acceptance of current content/source. A rejection preserves evidence, returns work to active with WIP admission, and starts rework from the accepted commit.

ChatOps reports durable deployment/review/completion events and links to authenticated current records. Notification failure cannot erase the pending review. Preview TTL cleanup is independent of retained source and acceptance evidence.
