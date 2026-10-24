# Environments and adapters

[ADR 0009](../adr/0009-environments.md) selects every outbound adapter from one required variable, `KODAMA_ENV`. This page owns the interfaces that make that cheap: a three-line environment type, one in-memory transaction kernel and one contract-suite convention. Everything else is an existing consumer-owned port.

| Environment | Answers | Needs | Feedback |
| --- | --- | --- | --- |
| `development` | Does the policy work? | Go | Seconds, zero containers |
| `test` | Do the real services and containers behave? | Podman dependency stack | Minutes, no cluster |
| `production` | Does it hold on Kubernetes? | kind, then the stage cluster | Explicit checkpoints |

## Selecting an environment

```go
type Env string

const (
    Development Env = "development"
    Test        Env = "test"
    Production  Env = "production"
)

func ParseEnv(getenv func(string) string) (Env, error) // KODAMA_ENV is required
```

`internal/app` parses `KODAMA_ENV` once and builds a table of constructors, one per outbound port. The table is not an interface registry: each entry returns the consumer-owned port of its module, and nothing outside `internal/app` takes an `Env`.

| Port (owner) | `development` | `test` | `production` |
| --- | --- | --- | --- |
| Persistence (each module's `Repository`/`Store`) | `memdb` | PostgreSQL | PostgreSQL |
| `events.BrokerPublisher` | In-process | JetStream | JetStream |
| `artifacts.ObjectStore` | In memory | LocalStack S3 | S3-compatible |
| `workspace.RefReader`, `RefPublisher` | Temporary bare repositories | HTTPS Git fixture | HTTPS Git host |
| `secrets.KeyWrapper` | Ephemeral | Local keyfile | Cloud KMS |
| `runtimeauth.TokenVerifier`, OIDC verifier | Static, seeded | OIDC fixture | Standalone or Optimi OAuth |
| `execution.Runtime` | Process | Podman | Kubernetes |

Construction fails closed in three ways: an unset or unknown `KODAMA_ENV` stops startup; `production` rejects any adapter flagged development or test (in-memory stores, the Process runtime, the development KEK, the OIDC fixture); and a missing adapter for a selected port refuses the capability, as today.

## The in-memory kernel

`internal/memdb` is the only in-memory persistence engine. Module adapters are thin tables over it; none implements its own locking or rollback.

```go
type Store struct{ /* … */ }

func New(opts ...Option) *Store
func (s *Store) Update(ctx context.Context, tenant domain.TenantID, fn func(Tx) error) error
func (s *Store) View(ctx context.Context, tenant domain.TenantID, fn func(Tx) error) error

type Table[V any] struct{ /* … */ }

func NewTable[V any](name string, key func(V) string, uniques ...func(V) string) *Table[V]
func (t *Table[V]) Get(tx Tx, key string) (V, bool)
func (t *Table[V]) Put(tx Tx, v V) error // domain.ErrConflict on a unique violation
func (t *Table[V]) Delete(tx Tx, key string)
func (t *Table[V]) Scan(tx Tx, yield func(V) bool)
```

- `Update` runs `fn` on a copy-on-write snapshot. A nil return commits all of it at once; any error discards all of it. State, history, audit and outbox rows written in one `Update` therefore land together or not at all.
- A `Tx` is bound to one tenant. A table has no way to read another tenant's rows, which is the in-memory counterpart of RLS.
- Cross-module joins pass the same `Tx` to the peer, the counterpart of passing `pgx.Tx`. A peer that opens its own `Update` has escaped the caller's transaction, exactly as with another `WithTenant`.
- `WithCommitFault(func() error)` fails a commit for rollback tests.
- `Update` serializes writers. It gives deterministic isolation, not row locks, `SKIP LOCKED` or lock ordering. Concurrency obligations stay with PostgreSQL ([SD-14](../specs/software-design.md)).

State lives only in the `Store`. There is no snapshot file and no restart persistence in `development`.

## One contract suite per port

A port with more than one adapter has one exported suite, in the owning module's `*test` package, that every adapter runs.

```go
func Contract(t *testing.T, open func(testing.TB) Repository)
```

The memory adapter's package calls it unconditionally. The PostgreSQL adapter's package calls it behind `layer.SkipService`, so the unit gate runs the memory side and the `test` environment runs both. `runtimetest.Contract` ([ADR 0008](../adr/0008-runtime-seam.md)) is the same pattern for the runtime. A memory adapter without its contract suite does not merge.

The suite asserts what callers rely on, not how a backend does it: tenant isolation, unique and conflict outcomes, version checks, all-or-nothing updates, replay of the same command, and not-found versus forbidden. It does not assert lock order or isolation levels; those belong to the named PostgreSQL checkpoints.

## Stacks

| Stack | Provisions | Used by |
| --- | --- | --- |
| none | Nothing | `development` |
| `services` | PostgreSQL, NATS, S3, Git, OIDC, registry on one Podman network; the Podman runtime | `test`: M1 and the M2 and M3 persistence checkpoints |
| `cluster` | The `services` stack plus a scoped kind cluster | Kubernetes as the subject: delivery apply, the Kubernetes runtime, the chart; `production` conformance |

Delivery scripts run as steps on Podman and reach a kind namespace as their deployment target, so only tasks whose subject is a Kubernetes target declare `kind`. Until T-091 lands, `make dev-up` still builds the cluster stack.

PostgreSQL for tests is cloned, not migrated per test: a template database migrated once per manifest digest, then `CREATE DATABASE … TEMPLATE` for each test. T-091 measures this against per-test migration and against an embedded PostgreSQL binary, and adopts a change only on a measured gain and a recorded decision.

## Adoption ladder

| Step | Environment | Tasks | Done when |
| --- | --- | --- | --- |
| 1 | `development` | T-086, T-087, T-088, T-080 to T-082 | Core modules unit-test on `memdb` and Process; contracts run on both adapters |
| 2 | `development` | T-071, T-027 | `KODAMA_ENV` guards in place; the assembled slice smoke-tests with no container |
| 3 | `test` | T-091, T-026 | M1 runs on Podman with no cluster |
| 4 | `development`, then `test` | T-083, T-084, T-072, T-039, then T-065, T-073, T-067, T-038 | M2 unit-tested in memory, then run on Podman with kind only as the delivery target |
| 5 | `development`, then `test` | T-075, T-046, then T-044 | M3 business loop unit-tested in memory, then run on Podman |
| 6 | `production` runtime | T-102 → T-089 | Real Kubernetes runtime, chart installation/configuration refusal and restricted baseline pass on kind; component evidence under O-24 |
| 7 | `production` | T-105/T-103 → T-104 | Actual chart readiness, live KMS and assembled M1 roundtrips with supported TLS; explicitly scheduled |
| 8 | `production` | T-090 | Trusted-engineer staging on the stage cluster after T-104 |

Each step is useful alone. Steps 1 to 3 need no cluster; step 6 is the first that does.
