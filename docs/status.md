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
| Kubernetes Job runtime and Helm chart | Component conformance on a local kind cluster ([report](acceptance/kube.md)) |

## Not yet

| Area | Where it stands |
| --- | --- |
| Production profile | Startup is deliberately refused until production key custody is qualified |
| Installed server on Kubernetes | Not yet shown ready end to end |
| Source, data and key recovery drills | Planned |
| Live model and coding-agent qualification | Agents are tested with scripted responses only |
| Optimi Platform (hosted) integration | Design target; standalone identity works today |
| Other agent harnesses, untrusted multi-tenant isolation, UI, Supabase blueprint, browser QA | Deferred |

The full list of remaining work is the [task backlog](tasks/README.md). The technical record of how the server is composed, with every route, worker and unwired capability, is [assembly status](implementation/assembly-status.md).

## Try it

```sh
make test-m1     # the whole M1 slice in memory: no containers, no services, about 30 seconds
make build       # builds dev/.state/bin/kodama
dev/.state/bin/kodama --help
```

`kodama server` starts a health-only scaffold unless started with `--assembly core`, which needs PostgreSQL, JetStream, S3, Git and an OIDC issuer. The [development runbook](../dev/README.md) brings those up locally with Podman.
