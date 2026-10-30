# Kodama kernel chart

**Status:** stage/test installation profile for the ADR 0013 kernel. It is not a
supported production identity or isolation profile.

This chart runs `kodama server --assembly kernel` over the nine-section kernel:
projects, pipelines, sequential container steps with a shared workspace, native
Git hosting, task/role-inbox tracking, envelope-encrypted secrets, metadata-only
artifacts and the PostgreSQL outbox/NATS bus.

## What it installs

| Resource | Purpose |
| --- | --- |
| Deployment | One kernel server, `--assembly kernel`, `KODAMA_ENV=production` |
| Migration Job | `kodama migrate`, owner role only, Helm pre-install/pre-upgrade hook |
| Service/Ingress | Public HTTPS origin for the API and `/git/` |
| Runtime Secret | Database runtime URL, NATS token, Git control token and two P-256 Git keys |
| KMS Secret | Provider credential file (Scaleway `secret-key`) |
| PersistentVolumeClaim | Retained logs and embedded Git worker roots |
| Tenant namespaces | Restricted Pod Security, execution service account, quota, default-deny ingress and public-only egress |
| Namespaced Role/Binding | Jobs, Pods, logs, Secrets and PVCs in each tenant namespace |
| ClusterRole/Binding | Read-only namespace discovery only |

## Values

`values.yaml` is a placeholder profile. Every credential is an existing Secret
reference. The strict schema rejects unknown fields and unpinned images.

Required inputs:

- `installation.id`
- `image.repository/tag/digest`
- `secrets.runtime.existingSecret`, `secrets.migration.existingSecret`,
  `secrets.kms.existingSecret`
- `kms.provider/current/resources`
- `execution.storageClass`, `execution.tenants[]`
- `publicURL`, `nats.url`, `oidc.issuer`, `oidc.clientId`

The stage stack passes `execution.checkoutImage` as the same release image. That
image checks out a run's pinned source in a Job inside its workspace claim.

## Stage-only Git hosting

`git.hosting=embedded` runs one in-process Git worker per tenant under the
server's persistent state directory. It is accepted outside development only
with `git.embeddedAck=stage-only` and is recorded as a stage deviation. A
supported production installation uses per-tenant workers and separate volumes,
which is not implemented by this chart.

## Checks

```sh
helm lint --strict deploy/charts/kodama-kernel
go test ./deploy/charts/kodama-kernel/
```

The Go test renders the chart and asserts the kernel assembly, readiness probe,
embedded hosting acknowledgement, tenant isolation and pinned image digest.
