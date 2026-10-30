# internal/app

The composition root: it selects every outbound adapter from the environment (`KODAMA_ENV`), builds the core assembly (`NewCore`) and the development composition's memory adapters, and records what it deliberately does not wire (`Routes.Disabled`). Domain and use-case packages never see an environment; they depend on consumer-owned ports and this package binds them.

## M2 production joins (T-101)

T-101 binds the prerequisites T-096 to T-100 into `NewCore`. Every binding is pure composition over injected adapters (unit-tested without a service), fails closed when its configuration or input is absent, and records the capability it leaves off in `Routes.Disabled` with a reason in `Routes.Reasons`. Nothing here widens authority.

| Join | Bound from | Enables | Disabled (reason in `Routes.Reasons`) when |
| --- | --- | --- | --- |
| Action ceiling (T-096) | `action-ceiling.json` | `model.invoke`, `tool.invoke`, `deployment.call` for attempts whose pinned facts allow them | the file is absent: attempts hold only the default actions (`action-ceiling`) |
| Delivery environments reader (T-097) | `execadapters.BindDeliveryEnvironments` as `execpg.Peers.Environments` | environment-declaring steps, the environment resolver, the cleanup environment | the store reports it does not bind them (`step-environments`) |
| Deployment binder (T-097) | `gatesadapters.BindDeployments` as the gate store's `DeploymentBinder`, the committed `gatesadapters.NewDeployments` as the gate service's reader | decidable deployment-bearing gates | either half is absent (`deployment-gates`) |
| Runtime secret route (T-097) | `execadapters.SecretRoute` behind `POST /runtime/v1/attempts/{attemptId}/secrets/resolve` | `runtime-secret-resolution` | no route is supplied (`runtime-secret-resolution`) |
| Podman checkout (T-098) | the existing `checkoutProvider` (project repository, installation credential, Git CA) through `podmanCheckout` | source-consuming Podman attempts receive the read-only material of their pinned repository only | no provider: the attempt is refused (`bootstrap.ErrNoCheckout`); no Git CA, no repository or another repository: refused (`ErrCheckoutDenied`) |
| Registry verifier (T-067) | `registry.json`: `oci.Verifier` over the configured registries, provenance from the producing attempt's pinned source | `ociArtifact` outputs | the file is absent: every OCI registration is refused (`oci-registry`) |
| Git trigger (T-100) | `git-triggers.json`: the PostgreSQL store and admission ledger, the secrets-backed hook secret, the core's run planner (agent steps frozen like the ordinary admission routes), mounted behind `AssembleTriggers` | `git-webhooks` | the file is absent: the hook route is not registered (`git-webhooks`) |
| Conformance report (T-031) | `ConformanceReportOutput = "conformance"` (the OpenCode wrapper's declared output; a test pins the two together) | gates of agent-produced candidates | no name is bound (`agent-candidate-gates`) |

### Configuration files

Optional, strict JSON (unknown members are errors), in the secrets directory or at the path named by the variable:

| File (variable) | Content |
| --- | --- |
| `action-ceiling.json` (`KODAMA_ACTION_CEILING_FILE`) | `{"kinds": {"agent": {"grants": ["model.invoke", "tool.invoke"]}, "container": {"grants": ["deployment.call"]}}}`. A grant is an upper bound: the attempt's own pinned profile (model, tools) or declared environment must still allow it. Only those three actions can be granted; `model.invoke` and `tool.invoke` apply to `agent` only. Applies to the Podman runtime; **production refuses a configured ceiling** until the Kubernetes forms (T-089) register per-kind ceilings, because that runtime registers under its execution policy's action lists. |
| `registry.json` (`KODAMA_REGISTRY_FILE`) | `{"endpoints": [{"host": "registry.example.com", "url": "https://registry.example.com", "authorizationFile": "registry-auth", "tenants": ["t_acme"], "repositories": ["team/app"]}], "caFile": "/path/ca.pem"}`. `host` is the repository reference's host (lowercase `host[:port]`), `url` defaults to `https://<host>`, `authorizationFile` (in the secrets directory) holds the complete `Authorization` header value, sent to that origin only. `tenants` (`"*"` for all) and `repositories` (path prefixes: `team/app` matches `team/app` and `team/app/x`) are required. Plain http needs `allowInsecureHttp` and is refused in production and with an authorization. |
| `git-triggers.json` (`KODAMA_GIT_TRIGGERS_FILE`) | `{"rateLimitPerMinute": 120}`: selects the native Git push trigger and bounds unauthenticated hook traffic per remote host. There is no unlimited default; remove the file to disable the trigger. Trigger rows, delivery claims and the admission ledger need the `gitwebhook` migration (appended last to `Contributions()`). |

### Disabled capabilities

`Routes.Disabled` lists capabilities that are deliberately not wired; `Routes.Reasons` names the missing configuration for those the T-101 joins own. An operator reads it to see why, for example, an agent step cannot reach the model gateway (`action-ceiling`) or why an image registration is refused (`oci-registry`).

### Not here

Real service and Podman qualification (T-038), Kubernetes forms of the ceiling and checkout (T-089) and production KMS wiring (T-029).

## run_shared composition (T-116)

`runshared*.go` compose the run_shared model for Kubernetes when the settings document carries a `runShared` section: the policy list (`PolicyList`), the resource authority and `/runtime/v2` routes, the Kubernetes run-resource adapter, prior disclosures, the additive run view and resource logs (`SharedPublic`). `AssembleExecution` binds the resource port of the runtime's own backend and refuses a half-composed model. See the [assembly status](../../docs/implementation/assembly-status.md#run_shared-assembly-t-116) and the [runbook](../../docs/operations/run-shared.md). Evidence: `Test_unit_app_run_shared_*`, `Test_unit_app_execution_policy_settings`, `Test_unit_app_runtime_v2_routes_served`, `Test_unit_app_prepare_and_service_run_commands`, `Test_unit_chart_run_shared_render`.

## Tests

`make check-go PKGS="./internal/app/..."`. The T-101 acceptance tests are `Test_app_action_ceiling_bound`, `Test_app_delivery_joins_bound`, `Test_app_podman_checkout_bound`, `Test_app_registry_verifier_bound` and `Test_app_git_trigger_mounted`; they use injected adapters, the modules' memory doubles and `httptest`, and prove no SQL, transaction or container behavior (T-073/T-038).
