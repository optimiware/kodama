# Changelog

## Unreleased

Added:

- MCP over Streamable HTTP that standard clients accept: protocol version
  negotiation, `notifications/initialized`, tool descriptions, `content` and
  `structuredContent` results, and a 401 with resource metadata on an
  unauthenticated tool call.
- Discovery operations: `principal.whoami` and list operations for projects,
  pipelines, tasks, bindings and services. A read accepts an empty body.
- `kodactl`: help, `whoami`, `inbox`, `approve`, `reject`, `logs`, `task`,
  `member`, `--ca-file`, and an MCP stdio bridge with `mcp install`.
- A built-in OIDC provider and social sign-in broker (`kodama auth serve`),
  and `kodama auth principal-id`.
- `kodama dev`: an in-memory server with a built-in sign-in on one HTTPS port.
- Git: a new repository is served without restarting `git serve`; a push
  trigger may name a ref prefix ending in `/`.
- A streaming model proxy for workloads, and the installation CA mounted into
  Podman workloads.
- An unreadable image reference is refused as `invalid_input`; an image that is
  missing from the local Podman store fails the execution at start.
- The release workflow builds the image for `linux/amd64` and `linux/arm64`
  and moves the `latest` tag on a release that is not a pre-release.

Known limitations:

- `kodama dev` cannot run executions: the process runtime has no workspace
  provisioning, so an execution fails with `workspace_missing`.
- `kodama dev` writes two credentials. The default one is scoped to the project
  (tasks, pipelines, the inbox). Membership administration needs the
  tenant-scoped one in `KODAMA_TENANT_ADMIN_CREDENTIAL_FILE`.
- `kodama up` is experimental. On rootless Podman it starts only with
  `--listen 0.0.0.0:<port>` and an explicit `--image`, and its administrator
  holds no project role, so tasks and pipelines are refused until roles are
  granted.
- `kodactl pipeline apply` does not print the pipeline id that
  `kodactl task run --pipeline` needs; read it with `kodactl call pipeline.list`.
- An unauthenticated MCP `tools/list` answers 200; only tool calls answer 401.
- No release has run the multi-arch image build or the `latest` tag yet.

## 0.1.0

The first public release.

Deployment targets:

- Single host on rootless Podman, and Kubernetes through the Helm charts.
- Scaleway is the only cloud target that has run live. AWS and GCP are
  implemented and not validated against a real cloud.

Known limitations:

- Git ref deletes are refused, also where `RW+` allows them.
- There is no public query for referenced commits; retained commits are
  protected by the control plane's keep-refs.
- Human and browser NATS subscriptions are refused; humans read events and
  logs over HTTP or MCP.
- The `webhook` and `email` service kinds deliver nothing outbound;
  email-to-task intake works.
- A workload start the runtime refuses leaves the execution `running` until
  its action deadline, and the cause is not shown in the execution view.
- The reference Scaleway configuration backs up every 6 hours, which misses
  the recommended RPO of 15 minutes.
- No service has been resumed from a restored database on a cloud deployment,
  so there is no measured RTO.
- The image is linux/amd64 only. Artifacts are not signed.
