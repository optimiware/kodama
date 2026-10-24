---
status: accepted
---

# Three environments: development in memory, test on Podman, production on Kubernetes

Kodama's tests and local runs grew around one heavy fixture: PostgreSQL, NATS JetStream, LocalStack S3, HTTPS Git, OIDC, an OCI registry and kind. Ten test-support packages (work, execution, events, artifacts, runtime authority, workspaces, gates, identity stack, ChatOps, work dependencies) need a real PostgreSQL, and the unit layer skips them. Only identity, pipelines, agents and observation have in-memory repositories. Fast feedback therefore stops at those four modules, and the first usable end-to-end run costs a cluster.

Select every outbound adapter from one environment, `KODAMA_ENV`: `development`, `test` or `production`. The variable is required; a missing or unknown value refuses to start. Only `internal/app` reads it. Domain and use-case packages never see it and depend only on consumer-owned ports ([ADR 0006](0006-hexagonal-domain-design.md)).

| Port | `development` | `test` | `production` |
| --- | --- | --- | --- |
| State (`Store`, repositories) | In memory, one `memdb` transaction | PostgreSQL, RLS, non-owner role | PostgreSQL, RLS, point-in-time recovery |
| Events (`events.BrokerPublisher`) | In-process broker | NATS JetStream | NATS JetStream |
| Objects (`artifacts.ObjectStore`) | In memory | LocalStack S3 | S3-compatible storage |
| Git (`workspace.RefReader`, `RefPublisher`) | Bare repositories in a temporary directory | HTTPS Git with CA and token | Controlled HTTPS Git host |
| Keys (`secrets.KeyWrapper`) | Ephemeral in-memory wrapper | Local keyfile | AWS, GCP or Scaleway KMS |
| Identity | Static verifier and seeded principals | OIDC fixture | Standalone OIDC or Optimi OAuth |
| Runtime (`execution.Runtime`) | Process | Podman | Kubernetes |
| Model and tools | Scripted responses | Deterministic fixtures over HTTP | Configured upstreams |

`development` is purely in memory and ephemeral: no file, no network dependency, nothing survives the process. It is the composition that unit and slice tests build and the loopback developer server may use. It is not an installable environment. `test` runs the dependency stack and the Podman runtime without a cluster; kind is a separate stack that exists only where a Kubernetes target is the subject (delivery, the Kubernetes runtime, the chart). `production` is Kubernetes: kind for conformance, then Platform's stage cluster ([C-19](../decisions.md#confirmed-direction)).

`production` refuses every `development` and `test` adapter, so a fixture can never become a fallback. The development KEK, Process runtime, in-memory stores and OIDC fixture are rejected at construction, not by convention.

## Rejected: SQLite for development

SQLite looked like a middle path between maps and PostgreSQL. It is a second SQL dialect for about eleven thousand lines of adapters: the schema carries 170 row-level-security policies, 36 triggers and 99 functions, and the queries use row locks, `SKIP LOCKED`, advisory locks and `jsonb`. SQLite has none of them and a single writer. A green SQLite run would resemble SQL evidence while proving neither tenant isolation nor concurrency, which is the false confidence SD-11 forbids. Cross-module bridges also take `pgx.Tx` today, so they could not use it without a new transaction abstraction.

## Consequences

One small `memdb` kernel gives every in-memory adapter the same atomic update, tenant scoping and uniqueness. Every port with more than one adapter has one exported contract suite that all of them run, so a memory adapter cannot drift from its PostgreSQL twin. In-memory adapters prove policy; PostgreSQL semantics stay named checkpoint obligations ([SD-11](../specs/software-design.md)).

The M1 and M2 checkpoints run on `test`, so most integration runs no longer pay for a cluster. Kubernetes qualification moves to its own task and staging follows it. An embedded or cloned PostgreSQL for the middle tier is measured before adoption, not assumed.
