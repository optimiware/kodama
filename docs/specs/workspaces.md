# Workspaces

**Scope:** Layer 1 shared run `/workspace` and direct native Git integration ([ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md)).

## Model

Under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md), there is **no separate `workspaces` database table** and no S3 Git-bundle publisher. Source continuity lives in the **native Git service** (`repositories` and task branches), and working files during execution live in the **run's shared `/workspace` volume**.

### Internally hosted repositories

- Each task works on a normal Git branch (by convention `task/<task-id>` or `main`) in the project's repository (`<tenant>/<project>/<repo>`).
- Engineers and container steps (Mode A coding harnesses or Mode B `kodama agent`) clone, fetch, commit, and push directly to the native Git endpoint (`/git/<tenant>/<project>/<repo>.git`) using their OAuth credential or project-space run token (`KODAMA_TOKEN`).
- Parallel agents or runs can push to their own branches and merge via standard Git steps.

### Run scratch (`run_shared`)

- When a pipeline run starts, Kodama allocates one disposable `/workspace` volume for the run and checks out the pinned repository commit (plus optional `<tenant>/context` and `<tenant>/<project>/context` Markdown repos under `/workspace/.context/`).
- Every step in the run mounts `/workspace`. Files created or modified by one step are immediately visible to subsequent steps in the same run.
- When the run finishes (succeeded, failed, or cancelled), the disposable `/workspace` volume is removed. Durable outputs are the commits pushed to the native Git service and the metadata recorded in `artifacts`.

## Requirements

| ID | Requirement |
| --- | --- |
| WS-01 | Each pipeline run MUST provide a shared `/workspace` mounted across its steps and cleaned up when the run finishes. |
| WS-02 | Initial checkout MUST populate `/workspace` from the native Git service at the run's target branch/commit and optionally mount tenant/project `context` repositories. |
| WS-03 | Steps and agents pushing commits MUST authenticate with their project-space run token against the native Git service; injected secret files under `/kodama/secrets` MUST remain outside `/workspace`. |

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| WS-A01 | Step 1 generates code in `/workspace`, Step 2 runs tests on those files, and Step 3 commits and pushes to the native Git service | WS-01, WS-02, WS-03 |
| WS-A02 | Mounted secret files in `/kodama/secrets` are never committed into the repository | WS-03 |
