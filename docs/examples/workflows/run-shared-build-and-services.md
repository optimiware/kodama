# Example: shared workspace, services and builders (`kodama/v1alpha2`)

**Target:** designed `run_shared` model (T-109 contract; implementation T-110–T-117). These are **design fixtures**, not accepted syntax of any installed server and not a qualified build profile: the current executable format is [`kodama/v1alpha1`](../../contracts/pipeline-format.md). Registries, images and digests are placeholders (`registry.example.invalid`, repeated-character digests). The builder commands show intent; the exact flags, storage driver, mapping ranges and BuildKit mode are frozen only by real qualification evidence (T-111/T-117). Normative rules: [`v1alpha2`](../../contracts/pipeline-format-v1alpha2.md), [run resources](../../contracts/run-resources.md).

## 1. One workspace across images, with a gate in the order

```yaml
apiVersion: kodama/v1alpha2
kind: Pipeline
metadata:
  name: generate-test-publish
spec:
  timeoutSeconds: 7200
  steps:
    - id: generate
      kind: container
      image: registry.example.invalid/go-tools@sha256:1111111111111111111111111111111111111111111111111111111111111111
      workspace: {access: write}
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace && go generate ./..."]
      timeoutSeconds: 600
    - id: test
      kind: container
      needs: [generate]
      image: registry.example.invalid/test-tools@sha256:2222222222222222222222222222222222222222222222222222222222222222
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace && go test -json ./... > /kodama/outputs/tests.json"]
      timeoutSeconds: 900
      outputs:
        report: {type: artifact, path: /kodama/outputs/tests.json}
    - id: review
      kind: gate
      needs: [test]
      gate:
        evidence: ["${{ steps.test.outputs.report }}"]
    - id: build
      kind: container
      needs: [review]
      image: registry.example.invalid/ko-tools@sha256:3333333333333333333333333333333333333333333333333333333333333333
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace && ko version"]
      timeoutSeconds: 600
    - id: lint
      kind: container
      image: registry.example.invalid/lint-tools@sha256:4444444444444444444444444444444444444444444444444444444444444444
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace && golangci-lint run ./..."]
      timeoutSeconds: 600
```

Source-backed run, so absent `workspace` means effective `read`; `generate` is the only writer, `test` and `build` see its generated files because they share one scratch, and `lint` has no `needs` yet runs **last**: `executionOrder` is `generate test review build lint` (smallest declaration index among eligible steps, gate in its position), so `lint` waits for the human decision. Move it above `review` to run it earlier. While the gate waits, no Job exists; the scratch and any services stay until `spec.timeoutSeconds` expires. `test` evidence is `clean` only if `generate`'s commit was published and the tree matches it. This example is historical; the current model is in the [pipeline-runtime simplification plan](../../implementation/pipeline-runtime-simplification.md).

## 2. Service-to-service edge

```yaml
apiVersion: kodama/v1alpha2
kind: Pipeline
metadata:
  name: app-with-database
spec:
  timeoutSeconds: 3600
  services:
    db:
      image: registry.example.invalid/postgres-rootless@sha256:5555555555555555555555555555555555555555555555555555555555555555
      env: {POSTGRES_USER: kodama, POSTGRES_DB: app, POSTGRES_PASSWORD_FILE: /kodama/secrets/postgres-password}
      secretFiles: {postgres-password: "secret://project/test-db-password"}
      command: ["/usr/local/bin/docker-entrypoint.sh", "postgres"]
      ports: {sql: {port: 5432}}
      resources: {cpuMillis: 500, memoryMiB: 512, ephemeralMiB: 256}
      storage: [{path: /var/lib/postgresql/data, sizeMiB: 1024}]
      readiness:
        exec: ["pg_isready", "-U", "kodama", "-d", "app"]
        timeoutSeconds: 60
    app:
      image: registry.example.invalid/inventory-app@sha256:6666666666666666666666666666666666666666666666666666666666666666
      env:
        DB_HOST: "${{ services.db.host }}"
        DB_PORT: "${{ services.db.ports.sql }}"
      ports: {http: {port: 8080}}
      resources: {cpuMillis: 500, memoryMiB: 512, ephemeralMiB: 256}
      readiness:
        http: {port: http, path: /healthz}
        timeoutSeconds: 90
  network:
    edges:
      - {from: app, to: db, ports: [sql]}
  steps:
    - id: e2e
      kind: container
      image: registry.example.invalid/e2e-tools@sha256:7777777777777777777777777777777777777777777777777777777777777777
      env:
        APP_HOST: "${{ services.app.host }}"
        APP_PORT: "${{ services.app.ports.http }}"
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace && ./e2e.sh"]
      timeoutSeconds: 900
```

`app` may reach `db:sql` (declared edge, so its `DB_HOST` expression is valid); `db` accepts nobody else, and `e2e` reaches `app:http` and `db:sql` but not another run's services with the same aliases. Removing the edge makes the `DB_*` expressions invalid (`edge_undeclared`). Endpoint addresses are fixed at admission, so the edge carries no start order; commands begin only after both services are ready.

## 3. ko, Buildah and BuildKit

**O-30 scope:** ko and Buildah run as ordinary steps under the policy's `step` role and are the delivered target. The BuildKit `role: builder` service shown last is deferred to T-123: until then admission refuses it (`capability_unsupported`). Under the stage network model (tenant isolation, public HTTP/HTTPS egress) a public registry needs no profile; the `registry` profile below stands for a private one.

These run as ordinary steps (ko, Buildah) or a run-exclusive service (BuildKit) under the execution policy; the document has no privilege, device, mapping or storage field. Registry access is the named `registry` egress profile; the push credential is a pinned secret file delivered just in time, outside `/workspace`.

```yaml
apiVersion: kodama/v1alpha2
kind: Pipeline
metadata:
  name: ko-publish
spec:
  timeoutSeconds: 1800
  steps:
    - id: build
      kind: container
      image: registry.example.invalid/ko-tools@sha256:8888888888888888888888888888888888888888888888888888888888888888
      workspace: {access: read}
      env:
        KO_DOCKER_REPO: registry.example.invalid/inventory
        DOCKER_CONFIG: /kodama/secrets
      secretFiles: {config.json: "secret://project/registry-push"}
      egress: [registry]
      command: ["/bin/sh", "-ec"]
      args:
        - |
          cd /workspace
          ref=$(ko build --bare ./cmd/inventory)
          printf '{"outcome":"completed","outputs":{"image":{"digest":"%s","registryRef":"registry.example.invalid/inventory"}}}' "${ref#*@}" > /kodama/results.json
      timeoutSeconds: 900
      outputs:
        image: {type: ociArtifact}
```

A Buildah step is the same shape with a Containerfile build (`RUN` instructions, multiple ownership IDs), for example `buildah build --storage-driver=vfs` followed by `buildah push`, using its qualified image and `egress: [registry]`. It needs the policy's mapping helpers and storage, which exist only if the installation advertises `builder_rootless` after qualification; otherwise admission fails `capability_unsupported` rather than running unprivileged and failing mid-build.

```yaml
apiVersion: kodama/v1alpha2
kind: Pipeline
metadata:
  name: buildkit-publish
spec:
  timeoutSeconds: 3600
  services:
    buildkit:
      role: builder
      image: registry.example.invalid/buildkitd-rootless@sha256:9999999999999999999999999999999999999999999999999999999999999999
      ports: {grpc: {port: 1234}}
      resources: {cpuMillis: 2000, memoryMiB: 4096, ephemeralMiB: 8192}
      storage: [{path: /var/lib/buildkit, sizeMiB: 16384}]
      egress: [registry]
      readiness: {tcp: grpc, timeoutSeconds: 120}
  steps:
    - id: build
      kind: container
      image: registry.example.invalid/buildctl-tools@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      workspace: {access: read}
      env:
        BUILDKIT_SERVER: "${{ services.buildkit.host }}"
        BUILDKIT_PORT: "${{ services.buildkit.ports.grpc }}"
      secretFiles: {config.json: "secret://project/registry-push"}
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace && buildctl --addr tcp://$BUILDKIT_SERVER:$BUILDKIT_PORT build --frontend dockerfile.v0 --local context=. --local dockerfile=."]
      timeoutSeconds: 1800
```

`role: builder` asks for the installation's pinned builder-service mode (rootless, `--oci-worker-no-process-sandbox` in its own process namespace, or a host-privileged mode on an exclusive disposable worker); the document selects none of them. The service is run-exclusive, never shares daemon state or cache with another run, and is stopped whole with the run, including leftover build subprocesses. Client authentication material is per-run and generated by execution, delivered to the service and to steps through the protected secret path as defined by the qualified builder-service profile; it is not a document field.

## 4. Source-free cleanup run

```yaml
apiVersion: kodama/v1alpha2
kind: Pipeline
metadata:
  name: preview-cleanup
spec:
  timeoutSeconds: 900
  steps:
    - id: cleanup
      kind: container
      image: registry.example.invalid/cleanup-tools@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
      command: ["/usr/local/bin/cleanup-preview"]
      timeoutSeconds: 600
```

Run as an internal `purpose=cleanup` run, `workspace` resolves to `none`: empty scratch, no checkout credential, no preparation workload. The same definition submitted as a work run resolves to `read` and gets a prepared checkout; explicit `workspace: {access: none}` would be refused for a work run (`workspace_none_source_backed`).

### Source-free, two steps sharing scratch

```yaml
apiVersion: kodama/v1alpha2
kind: Pipeline
metadata:
  name: cleanup-two-step
spec:
  timeoutSeconds: 900
  steps:
    - id: collect
      kind: container
      image: registry.example.invalid/cleanup-tools@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
      command: ["/bin/sh", "-ec"]
      args: ["/usr/local/bin/list-owned-objects > /workspace/objects.txt"]
      timeoutSeconds: 300
    - id: delete
      kind: container
      needs: [collect]
      image: registry.example.invalid/cleanup-tools@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
      command: ["/usr/local/bin/delete-listed", "/workspace/objects.txt"]
      timeoutSeconds: 300
```

The scratch is empty only when `collect` starts; `delete` verifies the same bound writable mount without clearing it and reads the file `collect` wrote. Both attempts begin with `workspace: null` and record `workspaceState: none`; `delete`'s context lineage holds one explicit source-free entry for `collect`.

## 5. Execution-policy excerpt: registry and preview egress

The installation, not the pipeline, authorizes destinations. Only workloads naming a profile receive it; the database in example 2 gets none.

```yaml
egressProfiles:
  registry: {destinations: [{host: registry.example.invalid, ports: [443]}]}
  preview:  {destinations: [{host: preview.example.invalid, ports: [443]}]}
```

A service with `egress: [preview]` reaches that private destination after name resolution, DNS goes to the installation resolver, and Kodama callbacks/Git/artifact endpoints are granted per workload role. Under the run model with `publicEgress: false` nothing else is reachable; under the O-30 stage model public HTTP/HTTPS is reachable as well.

## Acceptance mapping

| Example | Observable evidence | Acceptance |
| --- | --- | --- |
| 1 | Same files across different images, one holder at a time, order with the gate, clean/modified provenance | WS-A09, WS-A10, WS-A12, DQ-A12, PX-A16 |
| 2 | Run-only endpoints, readiness barrier, declared edge, default deny elsewhere | PX-A13, IT-A10 |
| 3 | Real ko/Buildah builds and registry push with explicit credentials/egress, refusal of the deferred builder service (BuildKit evidence: T-123) | IT-A11, VS-A11 |
| 4 | Source-free defaults and absence of checkout authority | WS-A09, PX-A17 |
| 5 | Named egress profiles only | IT-A10 |
