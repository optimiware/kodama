# Git service contract

**Status:** Native Git HTTP and Gerrit-style hierarchy contract under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md); behavior is specified in [Git repositories and knowledge base](../specs/git-repositories.md).

## Git endpoint

Kodama serves Git smart HTTP (`git-upload-pack` and `git-receive-pack`) at a Gerrit-style hierarchical path:

```text
https://<host>/git/<tenant-key>/context.git
https://<host>/git/<tenant-key>/<project-key>/context.git
https://<host>/git/<tenant-key>/<project-key>/<repo-name>.git
```

- **Automatic project association:** Any repository path matching `<tenant-key>/<project-key>/<repo-name>` is automatically associated with `<project-key>` (`repositories.project_id`).
- **Tenant context:** `<tenant-key>/context` is stored with `project_id = NULL` and is readable across all projects in `<tenant-key>`.
- **Authentication:** Accepts either a user's OAuth/Git credential (governed by RBAC `memberships`) or an in-run container's `KODAMA_TOKEN` (governed by project-space permissions).

## Repository resource

| Route | Purpose |
| --- | --- |
| `GET, POST /api/v1/tenants/{tenantKey}/repositories` | List or create tenant-level repositories (such as `<tenant>/context`) |
| `GET, POST /api/v1/tenants/{tenantKey}/projects/{projectKey}/repositories` | List or create project repositories (`<tenant>/<project>/<name>`) |

Example repository record:

```json
{
  "id": "repo_01",
  "tenantId": "t_acme",
  "projectId": "p_erp",
  "path": "acme/erp/backend",
  "defaultBranch": "main",
  "status": "ready",
  "cloneUrl": "https://kodama.example.invalid/git/acme/erp/backend.git",
  "createdAt": "2026-10-30T10:00:00Z"
}
```

## Worker interface

The Kodama server forwards authenticated Git requests to the tenant's `kodama git serve` worker and receives `post-receive` push notifications over the internal worker interface.

## Push records and events

When a push succeeds on the native Git worker:
1. If the push updates `.kodama/*.yaml` on the repository's default branch, each `.kodama/<processname>.yaml` file is validated and upserted into `pipelines`.
2. A `git.push` (or `git.tag` / `git.merge`) signal is published to NATS JetStream (`kodama.v1.<tenant>.<project>.git.push`), triggering any matching pipelines.

## Source set

When a pipeline run starts, Kodama checks out the target repository into `/workspace` and makes the tenant/project Knowledge Base (`<tenant>/context` and `<tenant>/<project>/context`) available under `/workspace/.context/`.
