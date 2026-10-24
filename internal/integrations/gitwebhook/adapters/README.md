# internal/integrations/gitwebhook/adapters

Production adapters of the native Git trigger's ports (task T-100). **Not wired**: T-101 mounts them in `internal/app`. PostgreSQL persistence is in [`../postgres`](../postgres/README.md); the trigger itself is described in [`../README.md`](../README.md).

| Type | Port | Source of truth |
| --- | --- | --- |
| `Runs` | `gitwebhook.Runs` | The core's run admission plus one admission `Ledger` (StartRun/FindRun/DecideRefusal over one admission store). |
| `ExecutionPlanner` | `Planner` | `execution.Service.Prepare` (authority of the installing principal, definition, work item, exact commit), the work item's content revision and the project's current repository binding. |
| `HookSecrets` | `gitwebhook.Secrets` | The secrets module, by `secret://project/<name>` reference. |
| `Definitions` | `gitwebhook.Definitions` | The pipeline module's repository. |
| `ledgertest` | | Memory `Ledger` and the exported `Contract` that it and the PostgreSQL ledger both run. Test support only. |

## Runs and the refusal ledger

`StartRun` first reads the key's decision: a recorded run is returned (`Created=false`) and a recorded refusal answers `ErrAdmissionClosed`, both without planning again, so a replay never re-evaluates authority and a stalled holder cannot create a run after a terminal refusal. An undecided key is planned outside any transaction and admitted by the ledger, which re-checks the key under its lock. Permanent refusals (`execution.ErrAdmissionRefused`, `ErrDenied`, `ErrHidden`, `ErrNotActionable`, `ErrWorkCancelled`, `ErrPurposeDenied`, `ErrInvalid`, `ErrNotFound`, a trigger without a work item, a conflicting repository identity) wrap `gitwebhook.ErrRunRefused`; conflicts, quota, busy, unconfigured and infrastructure failures are `ErrUnavailable` and decide nothing. `DecideRefusal` records the first refusal reason unless a run exists, in the same ledger. Planning pins the installer as `RequestedBy` and the verified commit as the source commit; the verified `RepositoryID` must equal the project's current binding or the run is refused. Because execution's source check follows the work item's *existing* workspace regardless of the binding, the planner also reads the workspace the prepared run selected (`WorkspaceReader`, satisfied by the workspace store) and refuses permanently, before the ledger admits, unless its repository ID and URL equal the verified `RepositoryID` and the current binding's URL (`PlannerConfig.Workspaces` is required). Run inputs are empty.

A work run needs a work item: a trigger installed without one is refused at admission (`ErrRunRefused`), recorded as `admission_refused`.

## Hook secret

`HookSecrets.Resolve(scope, ref)` finds the project secret named by the reference in the **trigger's own scope** (tenant and project predicates are the secrets store's), requires kind `generic` (a Git or model credential is never a hook secret), the current non-revoked version, unwraps the tenant scope key with the secrets module's `KeyWrapper` and opens the value under its bound ownership context. A missing, deleted, revoked, wrong-kind, malformed or foreign reference is `ErrSecretNotFound` (indistinguishable); a key-provider, store or authentication failure is `ErrUnavailable` with fixed text: no provider error, reference content or value is ever attached, and the adapter logs nothing. The caller owns and clears the returned bytes. Resolution is just in time on every call (rotation and revocation take effect at once).

Not pinned and not audited by the secrets module: the hook secret is not a run's secret, so there is no pin or `secret.resolve` audit; the trigger's own audit records each delivery. The secrets module has no pin-less decrypt API, so this adapter composes its exported read surface (`ListSecrets`, `GetSecretVersion`, `GetScopeKey`, `UnwrapScopeKey`, `envelope.Open`) and needs the installation identity the values were sealed under. A hosted custody adapter (O-19) replaces this type without changing the port. Resolution walks the project's secrets by name (ordered, 100 per page), the same bounded walk the delivery and gateway adapters use.

## Tests

`Test_git_trigger_refusal_ledger_durable` (memory ledger contract, the module's `Service` over `Runs`: stale holder after a terminal refusal, restart, success/refusal exclusivity, lost-finish recovery) and `Test_git_trigger_hook_secret_from_secrets` (secrets memory environment: by-reference resolution, missing/foreign/revoked/deleted/wrong-kind refusals, no value in errors or logs, end-to-end signature verification and revocation) run in the unit layer. `TestExecutionPlanner` and `TestDefinitions` cover the mappings. The PostgreSQL ledger runs the same `ledgertest.Contract` in `../postgres` (`TestPostgresLedgerContract`).
