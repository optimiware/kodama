# Unprivileged OCI Container Builds & Workload Resource Limits

Kodema workload containers run unprivileged with **no Docker socket**, **no
Podman socket**, and **no `/dev/fuse`**. That isolation prevents a pipeline
action from escaping to the host container daemon or mutating unrelated
containers on the machine or cluster node.

When a pipeline needs to build and publish an OCI container image (for example
a Node, Hono, Next.js, or Go service), use a userspace OCI builder inside an
ordinary pinned container action and publish the resulting digest-pinned
reference as an action output.

## Recommended unprivileged OCI builders

| Builder | Best for | Requires daemon or `/dev/fuse`? |
|---|---|---|
| **Kaniko (`executor:debug`)** | Standard `Containerfile` / `Dockerfile` builds for Node, Next.js, Python, Rust, or multi-stage images | No |
| **`ko`** | Fast, zero-`Dockerfile` OCI builds for Go binaries | No |
| **Rootless `buildah` (`vfs`)** | Full `Containerfile` compatibility when Kaniko is not an option | No (with `--storage-driver=vfs`) |
| **`crane`** | Inspecting, copying, re-tagging, or assembling OCI tarballs/layouts | No |

### 1. Kaniko (general `Containerfile` / `Dockerfile` builds)

See the verified example pipeline at
[`examples/pipelines/oci-build.yaml`](../../examples/pipelines/oci-build.yaml).

Key points:

- Use a digest-pinned Kaniko `debug` image (which ships `/busybox/sh` alongside
  `/kaniko/executor`, so Kodema's shell entrypoint can run).
- Mount your Docker/OCI registry configuration as a declared project secret
  (e.g. `REGISTRY_AUTH_CONFIG`) and copy `/kodema/secrets/REGISTRY_AUTH_CONFIG`
  to `/kaniko/.docker/config.json` before invoking `/kaniko/executor`. Never
  inline registry credentials in YAML or print them to logs.
- Pass `--digest-file=/workspace/image.digest` and write
  `<destination>@<digest>` to `/kodema/outputs/<action>/image` so downstream
  [test](../../examples/pipelines/test.yaml),
  [promote](../../examples/pipelines/promote.yaml), and
  [rollback](../../examples/pipelines/rollback.yaml) pipelines consume the
  exact immutable digest without rebuilding.

### 2. `ko` (Go services without a `Dockerfile`)

For Go services, `ko` compiles Go packages directly into distroless OCI images
and pushes them to an external registry in pure userspace:

```yaml
actions:
  - name: build-go-image
    image: ghcr.io/ko-build/ko@sha256:<pinned-digest>
    timeout: 10m
    resources:
      memory: 2Gi
      cpu: "2"
    secrets:
      - REGISTRY_AUTH_CONFIG
    outputs:
      - image
    commands:
      - mkdir -p "$HOME/.docker"
      - cp /kodema/secrets/REGISTRY_AUTH_CONFIG "$HOME/.docker/config.json"
      - KO_DOCKER_REPO="$(cat /kodema/inputs/destination)" ko build --bare --image-refs=/workspace/image.ref ./cmd/server
      - cat /workspace/image.ref > /kodema/outputs/build-go-image/image
```

### 3. Rootless `buildah` with VFS storage

When you need full `Containerfile` feature parity in an unprivileged workload
without `/dev/fuse`, select the `vfs` storage driver so `buildah` does not
attempt to mount `fuse-overlayfs`:

```sh
STORAGE_DRIVER=vfs buildah bud --isolation=chroot -f Containerfile -t "$destination" /workspace/src
STORAGE_DRIVER=vfs buildah push --authfile=/kodema/secrets/REGISTRY_AUTH_CONFIG --digestfile=/workspace/image.digest "$destination" "docker://$destination"
```

Note that `vfs` copies layers without copy-on-write, so it uses more temporary
disk space and is slower than Kaniko or `ko`.

### 4. `crane` (OCI layout assembly and registry operations)

For static assets, pre-built binaries, or moving digests between registries,
`crane` (`gcr.io/go-containerregistry/crane`) operates directly over the OCI
HTTP registry API with zero host privileges (`crane append`, `crane copy`,
`crane digest`).

## Workload memory and CPU limits

### Local `kodema up` and `kodema serve --profile test`

Local Podman workloads default to **2 GiB memory** and **2 CPUs** (with a
default per-action ceiling of **4 GiB memory** and **4 CPUs**), so typical
Node, Next.js, and Go builds succeed without manual `NODE_OPTIONS` throttling.

Override the local installation defaults or ceilings on `kodema up` (or via
environment variables on `kodema serve --profile test`):

```sh
kodema up \
  --workload-memory 4Gi \
  --workload-cpus 4 \
  --workload-max-memory 8Gi \
  --workload-max-cpus 8
```

Corresponding environment variables:

- `KODEMA_WORKLOAD_MEMORY_LIMIT` (default `2Gi` on Podman, `1Gi` on Kubernetes)
- `KODEMA_WORKLOAD_CPU_LIMIT` (default `2` on Podman, `1` on Kubernetes)
- `KODEMA_WORKLOAD_MAX_MEMORY_LIMIT` (default `max(4Gi, default)` on Podman; defaults to `KODEMA_WORKLOAD_MEMORY_LIMIT` on Kubernetes)
- `KODEMA_WORKLOAD_MAX_CPU_LIMIT` (default `max(4, default)` on Podman; defaults to `KODEMA_WORKLOAD_CPU_LIMIT` on Kubernetes)

### Per-action `resources` in pipeline YAML

A container action may optionally declare bounded `resources` using standard
Kubernetes quantity syntax (`Ki`, `Mi`, `Gi`, `Ti`, `k`, `M`, `G`, `T` for
memory; cores or millicores such as `"500m"`, `"1.5"`, `"2"` for CPU):

```yaml
actions:
  - name: build
    image: docker.io/library/node:22-alpine@sha256:<pinned-digest>
    resources:
      memory: 2Gi
      cpu: "2"
    commands:
      - npm ci
      - npm run build
```

Installation-level maximum bounds are always enforced at preparation time: an
action requesting more memory or CPU than the installation maximum is refused
with `invalid_input` before any container or Job is started.

## Diagnosing OOM kills

If a workload container exceeds its configured memory limit on either Podman or
Kubernetes, Kodema detects the runtime's `OOMKilled` signal and reports it
explicitly in `execution.inspect` (`kodema inspect <id>`) and `kodema run`:

```text
execution exec-... failed: runtime: oom_killed: OOMKilled: workload exceeded its 2 GiB memory limit (exit 137)
```

When you see `oom_killed`, either raise the action's `resources.memory` within
the installation maximum, or raise the installation limit (`kodema up
--workload-memory 4Gi` locally, or `KODEMA_WORKLOAD_MEMORY_LIMIT` /
`KODEMA_WORKLOAD_MAX_MEMORY_LIMIT` in production).
