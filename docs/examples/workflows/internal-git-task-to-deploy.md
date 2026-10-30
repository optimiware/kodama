# Example: internal Git, from task to deployment

**Target:** the designed internal Git service ([ADR 0011](../../adr/0011-internal-tenant-git-service.md), T-125–T-133). Nothing here is served yet. CLI commands marked *planned* are the intended interface of T-132; hosts, IDs, commits and images are placeholders (`example.invalid`, repeated-character commits). Normative rules: [Git repositories](../../specs/git-repositories.md), [Git service contract](../../contracts/git-service.md).

## 1. A new project, provisioned by an agent

An engineer creates the project and describes the work; everything after that is automated.

```sh
# planned CLI
kodama work create --project inventory --kind story \
  --title "Inventory exception review" --spec-file spec.md
kodama work transition wi_1042 --to ready
```

A provisioning pipeline starts for the ready item (`origin: provision`, no repository yet). Its agent calls the `repository.create` MCP tool:

```json
{"tool": "repository.create", "arguments": {"name": "backend", "defaultBranch": "main", "policy": {"directPushToDefault": false}}}
```

Kodama reserves `repo_01`, the tenant's Git worker initializes the bare repository, and `repository.created` is emitted. The provisioning attempt holds a seed grant, so its first push to `main` is accepted; any other ref or repository is refused.

## 2. Two agents implement the task

Steps inside one run still execute one at a time. To work in parallel, Kodama starts two implementation runs for `wi_1042`, `run_01` (API) and `run_02` (UI). Each pins the same source set (`repo_01` at the seed commit, checked out at `/workspace/backend`), has its own workspace and clone, and receives an attempt grant for its own agent-branch namespace only. Neither needs the writer reservation:

```sh
# inside run_01's agent step; the grant arrives through the credential helper
git -C /workspace/backend switch -c kodama/agents/wi_1042/run_01/api
git -C /workspace/backend commit -am "Add exception review API"
git -C /workspace/backend push origin kodama/agents/wi_1042/run_01/api
```

A push to `kodama/agents/wi_1042/run_02/ui` or to `kodama/work/wi_1042` from `run_01` is refused (`grant_scope`). Each accepted push gets a push record and emits `git.refs_updated` with `originRunId` set, so the validation trigger ignores the run's own pushes.

When both runs succeed, a merge run starts. Its writer step holds the work item's writer reservation, merges both agent branches into `kodama/work/wi_1042` and pushes the task branch. If an engineer pushed that branch meanwhile, the push is rejected and the step fetches and merges again.

## 3. Review, integration and deployment

1. An integration run's writer step merges the task branch with the current `main` tip `2222…` and pushes candidate `3333…` to the task branch.
2. Tests and an agent review run against `3333…`; their evidence names `repo_01@3333…`.
3. A human reviewer approves the gate whose subject names candidate `3333…` and base `2222…`.
4. The integration step receives a grant for exactly `refs/heads/main: 2222… → 3333…` and pushes it. If `main` had moved, the push fails and the pipeline recomputes the candidate.
5. The applied push emits `git.refs_updated` for `main`; the project's CI/CD trigger starts a run pinned to `3333…`, whose last step deploys with Helm using a mounted kubeconfig secret.

A revert is another task: a pipeline creates a revert commit and goes through the same path, or redeploys a previously built image by digest. Nobody resets `main`.

## 4. An existing project, pushed by a developer

```sh
# planned CLI: create the repository and configure Git credentials
kodama repo create --project billing --direct-push-to-default=true billing-api
git config --global credential.https://git.example.invalid.helper "kodama git credential"

# ordinary Git from here on
git remote add kodama https://git.example.invalid/git/acme/billing/billing-api.git
git push kodama main
```

Because the repository is empty, the developer's first push to `main` counts as the seed. Kodama records the push and the CI/CD trigger starts one run for exactly the pushed commit. The repository was created with `--direct-push-to-default=true`, so later pushes to `main` by an engineer also start CI/CD directly. Created with `false` (the recommended default under O-33), only the seed push may target `main`: the developer pushes a task branch and the integration pipeline advances `main`.

## What to check

- Tenant and project scope on every repository, grant, push record and event.
- Each run's source set, and that evidence and gate subjects name the same commits.
- That no step ever receives a grant for `main` outside integration or seeding.
- That a lost push confirmation leaves the commit output pending until reconciled, never assumed.
