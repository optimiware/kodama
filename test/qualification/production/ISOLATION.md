# Q02 runtime and cross-tenant isolation probes

These probes are qualification checks, not component tests. Their evidence
counts only when they run against a deployed stage revision through
`run-stage.sh` (or an equivalent verified environment). With no stage inputs the
stage tests skip and name every missing input; `Q02_REQUIRE_STAGE=1` turns a
missing input into a failure.

## Probes

| Probe | Invariants |
|---|---|
| `TestStageRuntimeIsolation` | Each tenant namespace is `kodama-tenant-<tenant>`, carries the tenant label and the baseline/restricted pod-security profile; the `kodama-tenant-limits` LimitRange publishes positive CPU/memory defaults; the `kodama-tenant-default-deny` NetworkPolicy denies both directions and selects no foreign or all-namespace peer, while admitting cluster DNS and Kodama; a live workload Pod has positive requests/limits within the namespace defaults, no host namespace, no host path and no privileged container; a live tenant-A workload cannot reach a tenant-B listener, can reach Kodama, and cannot reach the Kubernetes API. |
| `TestStageCrossTenantIsolation` | Two onboarded tenants cannot read or mutate each other's pipelines, tasks, executions, inbox, events, secret metadata, memberships or logs; foreign admin tokens and foreign service and execution credentials refuse with `404 not_found`, except two cases that refuse with `403 forbidden` before any store read: an execution credential inspecting another execution, and a human calling `execution.action.outputs`; a tenant's own admin, service and execution credentials still work; the other tenant's namespace contains no pod, workspace claim or execution credential Secret for the execution; neither tenant's `/workspace` contains the other's marker; the per-tenant Git service serves only its own service credential (when the Git fixture is configured). |

## Stage prerequisites

1. The stage values list both tenants (`tenants: [tenant-a, tenant-b]`) so
   the chart renders both tenant namespaces, LimitRanges and NetworkPolicies
   and the server binds both Kubernetes runtime adapters. The chart derives the
   namespace as `kodama-tenant-<tenant>`.
2. Each tenant has an onboarded project and an admin credential. Supply the
   second tenant through `Q02_SECOND_TENANT_ID`, `Q02_SECOND_PROJECT_ID`,
   `Q02_SECOND_TENANT_NAMESPACE`, and either a second OAuthSonas persona
   (`Q02_SECOND_ADMIN_PERSONA`) or an explicit `Q02_SECOND_ADMIN_TOKEN`.
   `run-stage.sh` captures a caller-supplied `Q02_SECOND_ADMIN_TOKEN` before it
   unsets every caller-exported qualification token, and exports it only to the
   final test command; it never reaches the kubectl, curl, token-script or
   filesystem children.
3. The digest-pinned `Q02_IMAGE` workload image provides `sh`, `timeout`,
   `kill` and `sleep`; the network listener/client pair additionally needs
   `curl` and either `nc` or `python3`. `Q02_NETWORK_IMAGE` may pin a separate tool image for that pair
   (the stage uses the pinned `curlimages/curl` digest) and defaults to
   `Q02_IMAGE`. With neither `nc` nor `python3` the runtime probe fails with
   `q02-nc-missing`; it can never turn the cross-tenant assertion into a pass.
4. Git isolation needs both per-tenant Git services. Deploy
   `gitService` for both tenants, run `scripts/stage-git --provision` for each,
   and set `Q02_GIT_REPOSITORY_A`, `Q02_GIT_REPOSITORY_B`,
   `Q02_GIT_SERVICE_A` and `Q02_GIT_SERVICE_B`. The probe reads the service
   credentials from the `kodama-git-<tenant>` Secrets in `kodama-system`; it
   never logs their values. A missing Git fixture fails the probe by default.
   `Q02_ALLOW_GIT_SKIP=1` deliberately runs the rest of the probe without Git
   and writes an explicit `skipped` evidence record, so the skip is never
   indistinguishable from a pass.

## Notes

- The network probe uses a tenant-B workload as the target, so it proves the
  deployed policy's effect rather than only its declaration. The listener loops
  for five minutes, far longer than a slow image pull or the client's curl
  window, and the probe requires tenant B's execution to still be non-terminal
  once tenant A's client is terminal. A listener that is missing or exits early
  fails instead of turning every refused connection into a vacuous pass. The
  client also reaches Kodama and fails to reach the Kubernetes API, which proves
  the policy is not a blanket deny or a blanket allow.
- The cross-tenant API assertions require `404 not_found`, not `403`, so a
  foreign resource's existence is not disclosed. The two `403` cases above are
  refused on the caller's kind before any resource is looked up.
- No token, Secret value, log body or workspace marker is written to evidence.
