# Workspaces

**Scope:** Layer 1 shared run `/workspace` with source preparation from the Layer 2 native Git service ([ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md)).

## Model

Under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md), there is **no separate `workspaces` database table** and no S3 Git-bundle publisher. Source continuity lives in the **native Git service** (`repositories` and task branches), and working files during execution live in the **run's shared `/workspace` volume**.

### Internally hosted repositories

- Each task works on a normal Git branch (by convention `refs/heads/kodama/work/<task-id>`) in the project's repository (`<tenant>/<project>/<repo>`).
- Engineers and container steps (Mode A coding harnesses or Mode B `kodama agent`) clone, fetch, commit, and push directly to the native Git endpoint (`/git/<tenant>/<project>/<repo>.git`) using their OAuth credential or the run's scoped Git credential (`KODAMA_TOKEN` and the credential helper).
- Parallel agents or runs can push to their own branches and merge via standard Git steps.

### Run workspace

- When a pipeline run starts, Kodama allocates one disposable `/workspace` volume for the run and prepares the pinned repository commit (plus optional `<tenant>/context` and `<tenant>/<project>/context` Markdown repos under `/workspace/.context/`).
- Every step in the run mounts `/workspace`. Files created or modified by one step are immediately visible to subsequent steps in the same run.
- A durable wait pauses the same run without a workload. The workspace lifetime is bounded and explicit; loss of required state fails the run rather than replaying earlier commands.
- When the run finishes (succeeded, failed, or cancelled), the disposable `/workspace` volume is removed. Durable outputs are the commits pushed to the native Git service and the metadata recorded in `artifacts`.

## Requirements

| ID | Requirement |
| --- | --- |
| WS-01 | Each pipeline run MUST provide a shared `/workspace` mounted across its steps and cleaned up when the run finishes. |
| WS-02 | Initial checkout MUST populate `/workspace` from the native Git service at the run's target branch/commit and optionally mount tenant/project `context` repositories. |
| WS-03 | Steps and agents pushing commits MUST authenticate with their project-space run token against the native Git service; injected secret files under `/kodama/secrets` MUST remain outside `/workspace`. |

**WS-03 limitation.** A step is given a usable Git credential by design: it must be able to fetch and push, so code running in the step can read the credential file and copy it. Kodama does not claim to prevent that. What it guarantees is where the credential is *not*, and when it ends:

- it is not inside `/workspace`: the run's credential directory (`KODAMA_GIT_DIR`, mounted read-only at `/kodama/git` in a container) lies outside the workspace volume;
- it is not in any repository's `.git/config` and not in a remote URL: remotes are plain endpoints, and a generated Git configuration (`GIT_CONFIG_GLOBAL`) holds only a per-repository credential helper that reads the credential file when Git asks, never the secret;
- it is not in the step's environment: the step receives the *path* `KODAMA_GIT_DIR`, not the credential;
- it is removed when the run ends, and removed at once when the run is cancelled or finished (the runner checks the run's liveness on every refresh tick and deletes the files; a failure to learn the answer keeps them and is retried). The server also revokes the credential when the run ends, so a copied credential stops working at that point. It is read-only for the tenant and project `context` repositories and can update only the run's own branches for a source the run may write.

Anything a step copies out of the credential file before the run ends works until the run ends or the credential expires; limiting that exposure is the job of the credential's narrow scope and short lifetime, not of hiding it from the step.

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| WS-A01 | Step 1 generates code in `/workspace`, Step 2 runs tests on those files, and Step 3 commits and pushes to the native Git service | WS-01, WS-02, WS-03 |
| WS-A02 | Mounted secret files in `/kodama/secrets` are never committed into the repository | WS-03 |
