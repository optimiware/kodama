# Example: ordinary containers from build to rollback

**Target:** [ADR 0012](../../adr/0012-container-sdlc-and-artifact-releases.md), implementation pending. Existing container/secret syntax below illustrates the primitive; release/fetch/publication helpers and generic delivery use the [planned contract](../../contracts/artifacts-and-releases.md) and are not available merely because the YAML parses. Image digests, identifiers and domains are placeholders; scripts live in the project's pinned source.

## Shared files and explicitly retained outputs

```yaml
apiVersion: kodama/v1alpha2
kind: Pipeline
metadata:
  name: inventory-build
spec:
  timeoutSeconds: 1800
  steps:
    - id: build
      kind: container
      image: registry.example.invalid/build-tools@sha256:1111111111111111111111111111111111111111111111111111111111111111
      command: ["/bin/sh", "-ec"]
      args:
        - "cd /workspace && ./ci/build.sh && tar -cf /kodama/outputs/build.tar -C dist ."
      timeoutSeconds: 600
      outputs:
        build: {type: artifact, path: /kodama/outputs/build.tar}
    - id: test
      kind: container
      needs: [build]
      image: registry.example.invalid/test-tools@sha256:2222222222222222222222222222222222222222222222222222222222222222
      command: ["/bin/sh", "-ec"]
      args:
        - "cd /workspace && ./ci/test-built-assets.sh > /kodama/outputs/coverage.json"
      timeoutSeconds: 600
      outputs:
        coverage: {type: artifact, path: /kodama/outputs/coverage.json}
```

`build.sh` writes `/workspace/dist`; the test sees those same files. Neither step publishes source. The archive and coverage JSON survive the run through explicit output collection; other scratch files do not. A chart `.tgz`, HTML report archive or binary is retained the same way. An image-producing step instead pushes to a configured registry and declares its existing `ociArtifact` output.

## A harness is also a command

Step excerpt using the ordinary declared-secret form:

```yaml
id: implement
kind: container
image: registry.example.invalid/claude-tools@sha256:3333333333333333333333333333333333333333333333333333333333333333
workspace: {access: write}
command: ["/bin/sh", "-ec"]
args: ["cd /workspace && ./ci/run-agent.sh"]
env:
  ANTHROPIC_API_KEY: {secretRef: "secret://project/agent-api-key"}
timeoutSeconds: 1200
```

The pinned script contains the harness-specific invocation; the image must contain that harness and required Git tooling. No managed profile/model catalogue is needed for direct execution. Source changes still follow the selected Git hosting/publication authority. Known secret values are excluded from retained source/outputs through the existing disclosure path. The run must not claim gateway-enforced budgets for this direct provider call. A managed native Go or OpenCode image can use the same step primitive with its selected profile conveniences.

## Release, publication and deployment

After producer outputs finalize, a subsequent step or authorized API caller creates release `inventory/application/1.4.2` with exact entries for the image, chart, build archive and coverage report. The server derives/validates producer and source references. Creating the same version with changed content conflicts.

| Ordinary step | Script responsibility | Kodama record |
| --- | --- | --- |
| Publish image/chart | Copy with crane/skopeo/oras/Helm to `registry.prod.example.invalid`; verify the full digest-addressed content | Publication receipts for exact destination copies |
| Deploy Kubernetes | Fetch the retained chart; select the production image digest; run Helm with the explicitly bound target kubeconfig | Optional generic pre-apply deployment and script-observed outcome |
| Deploy static assets | Fetch/verify the retained archive; extract explicitly; upload with Wrangler or a storage CLI using its secret | Same generic deployment shape, no kubeconfig or image requirement |
| Apply infrastructure | Run Terraform/OpenTofu scripts from the pinned source; use their normal locking state backend | Generic configuration/source-bound deployment; artifact set may be empty; plans/reports can be artifacts |

These are recipe responsibilities, not new pipeline step kinds. Target destinations and credentials must be explicitly authorized by the existing runtime policy. Production pulls only from its own reachable registry/bucket; it cannot reach Kodama's internal object storage in the acceptance scenario. A lost apply response leaves an unknown outcome until the script/operator observes the target. An absent receipt never authorizes blind replay.

## Promote and roll back

Promote `1.4.2` by selecting its existing manifest/digests and the target configuration. Later, a rollback pipeline selects `1.4.1`, retains its prior artifacts, explicitly chooses the recorded configuration and pins currently authorized credentials for that new run. It performs the same ordinary publication/deployment steps if a destination copy is needed; it does not rebuild source or move a `latest` tag as a substitute for exact identity.

The previous deployment's history remains unchanged. Removed registry layers or file objects cause a visible failure; a metadata pin is not a replacement for bytes. Database migrations/infrastructure changes require the application's explicit recovery procedure. Generated build files need no Git commit: review can bind the exact release and disclosed input lineage, while source-candidate review still rejects mismatched source.
