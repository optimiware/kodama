# Project status

Kodama is **early and not production-ready**. The core platform works end to end on a developer machine. Production deployment, key recovery and hosted use are still ahead. This page says plainly what is which.

## Works today

Verified by acceptance suites that run the real server against PostgreSQL, NATS JetStream, S3, Git and OIDC, with pipeline steps run as Podman containers.

| Area | State |
| --- | --- |
| Work items, projects, memberships, RBAC | Working |
| Git workspaces with controlled publication | Working |
| Container pipelines: runs, retries, cancellation, restart recovery, logs, artifacts | Working ([M1 report](acceptance/m1.md)) |
| Variables and encrypted secrets | Working |
| CLI and generated Go client for the HTTP API | Working |
| Agent steps, human gates, environments and deployments | Working with deterministic test agents ([M2 report](acceptance/m2.md)) |
| Native Git push trigger | Working |
| Dependencies between work items, flow metrics, ChatOps notifications | Working ([M3 report](acceptance/m3.md)); a human cannot yet move an item from review to done through the assembled server |
| Kubernetes Job runtime and Helm chart | Component conformance on a local kind cluster ([report](acceptance/kube.md)); installed-server readiness on the stage cluster is under "Not yet" until reviewed |

## Not yet

| Area | Where it stands |
| --- | --- |
| Production profile | `KODAMA_ENV=production` runs on the stage installation with live Scaleway KMS custody (T-104). A supported production profile is not claimed: TLS hardening (O-27), recovery and PITR, and hosted authority remain |
| Installed server on Kubernetes | Shown on the Scaleway stage cluster by the T-104 readiness gate (readiness and staging gates passed at `0a905ef5989d`; [readiness report](acceptance/kube-full.md), [staging record](operations/staging.md#qualification-record)). Plaintext NATS and a fixture issuer (O-27), scheduled-only backups, and direct secret delivery with the limits in [T-107](tasks/T-107-direct-secret-delivery.md) remain. Not a supported production profile |
| Source, data and key recovery drills | Planned |
| Live model and coding-agent qualification | Agents are tested with scripted responses only |
| Optimi Platform (hosted) integration | Design target; standalone identity works today |
| Other agent harnesses, untrusted multi-tenant isolation, UI, Supabase blueprint, browser QA | Deferred |
| External MCP server | `kodama mcp serve` implements the public API operations as MCP tools (secret-value writes are opt-in); the Helm chart installs it as a separate Deployment/Service with an optional dedicated-host Ingress. OAuth resource-server, SDK client, executor and chart tests pass with the in-process issuer. A real authorization server that accepts the MCP `resource` parameter and supports the chosen client registration is still required before claiming a zero-configuration OAuth connection; production must also replace the current token pass-through with an MCP-specific audience and separate upstream credential ([spec](specs/mcp-server.md)) |

The full list of remaining work is the [task backlog](tasks/README.md). The technical record of how the server is composed, with every route, worker and unwired capability, is [assembly status](implementation/assembly-status.md).

## Try it

```sh
make test-m1     # the whole M1 slice in memory: no containers, no services, about 30 seconds
make build       # builds dev/.state/bin/kodama
dev/.state/bin/kodama --help
```

`kodama server` starts a health-only scaffold unless started with `--assembly core`, which needs PostgreSQL, JetStream, S3, Git and an OIDC issuer. The [development runbook](../dev/README.md) brings those up locally with Podman.
