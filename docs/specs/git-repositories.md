# Git repositories and knowledge base

**Scope:** Layer 1 native Git service, Gerrit-style hierarchical path conventions, automatic project association, `.kodama/<processname>.yaml` indexing, and the Git-native Markdown Knowledge Base ([ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md)).

## Model

Kodama embeds a native Git HTTP service (`kodama git serve` wrapping `git http-backend`, `git receive-pack`, `git upload-pack`, and server-side hooks) isolated per tenant. PostgreSQL stores lightweight metadata in `repositories` (`id`, `tenant_id`, `project_id`, `path`, `default_branch`, `status`), while Git stores the commit graph, branches, and tags on the tenant's durable Git volume.

### Gerrit-style path hierarchy and automatic project association

Repositories are addressed by a hierarchical path under the tenant:

| Path pattern | Scope & association | Typical usage |
| --- | --- | --- |
| `<tenant>/context` | Tenant scope (`project_id = NULL`) | Tenant-wide engineering guidelines, security policies, global skills, and blueprints |
| `<tenant>/<project>/context` | Auto-associated with `<project>` | Project architecture, domain glossary, component map, and conventions |
| `<tenant>/<project>/<component>` | Auto-associated with `<project>` | Application/service source code and `.kodama/<processname>.yaml` pipelines |
| `<tenant>/<project>/deploy` | Auto-associated with `<project>` | Deployment manifests (Helm, Terraform, Wrangler) and promotion pipelines |

### Provisioning

When a repository is created or pushed at `/git/<tenant>/<project>/<name>.git`, Kodama automatically resolves `<project>` within `<tenant>`, sets `repositories.project_id` to that project, and initializes the bare repository on the tenant's Git volume.

### Push journal and events

When commits are pushed to a repository's default branch, the Git `post-receive` hook inspects `.kodama/*.yaml`:
- Each `.kodama/<processname>.yaml` file (for example `.kodama/plan.yaml`, `.kodama/implement.yaml`, `.kodama/review.yaml`, `.kodama/test.yaml`, `.kodama/preview.yaml`, `.kodama/promote.yaml`) is parsed, validated, and upserted into the `pipelines` table (`name = <processname>`, `definition` JSONB/YAML).
- Pipelines can also be registered or updated directly through the API or `kodactl`.

## Integration

### Push events, merges, and releases

Every accepted push, branch merge, or tag creation in the native Git service publishes a signal onto the NATS JetStream bus (`git.push`, `git.merge`, `git.tag`) with the repository ID, path, ref, old/new commit SHA, and actor (user or run ID).
- Pushes made by a pipeline step carry the originating `run_id` so trigger rules can avoid recursive self-triggering.
- Merging a task branch into the default branch or tagging a release is performed either by an ordinary pipeline step or via `kodactl` / Git commands using the project's credentials.

## Requirements

| ID | Requirement |
| --- | --- |
| GR-01 | Repositories under `<tenant>/<project>/*` MUST be automatically associated with `<project>`; `<tenant>/context` MUST be tenant-scoped and readable by all projects in that tenant. |
| GR-02 | Each tenant's repositories MUST be isolated on that tenant's Git storage and authenticated via OAuth user credentials or project-space run tokens. |
| GR-03 | Accepted pushes to a repository MUST index `.kodama/<processname>.yaml` definitions into `pipelines` and emit a `git.push` / `git.tag` signal on NATS. |
| GR-04 | Server-owned Git hooks and configuration MUST NOT be bypassable by repository content or client push options. |

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| GR-A01 | Creating or pushing `acme/erp/backend` auto-associates the repository with project `erp`; `acme/context` is accessible across tenant `acme` projects | GR-01, GR-02 |
| GR-A02 | Pushing `.kodama/implement.yaml` updates the project's `pipelines` row and publishes a NATS `git.push` event | GR-03, GR-04 |
| GR-A03 | Cross-tenant clone or push attempts fail with `404 not_found` | GR-02 |
