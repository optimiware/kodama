# internal/gates/adapters

Bridges from the gate module's consumer-owned ports to identity, artifacts, execution and workspace. T-097 adds the deployment join; `internal/app` binds it in T-101 and nothing registers it yet.

## Deployment adapter (`deployments.go`)

`NewDeployments(reader)` implements `gates.Deployments` over committed deployment records (preparation of a gate). `BindDeployments(tx)` is the `gatespg.DeploymentBinder` the gate store takes as `Config.Deployments`: it implements the same port over the very transaction that decides or consumes the gate, with one `SELECT` scoped by tenant and project, no network call and no row lock (so it cannot precede the execution locks). The digest, scope and candidate never change after registration and cleanup only moves forward; a cleanup committed after the read is seen by the next check, and the read commits or rolls back with the decision.

Both answer through one rule:

| Record | Answer |
| --- | --- |
| live deployment of the asked scope | `{ID, ConfigDigest, Scope, Candidate}` from the record (the pinned configuration digest and the recorded source commit) |
| absent, of another project or tenant, or a row that is not the asked one | `gates.ErrNotFound` |
| cleanup requested (any cleanup state) or deleted | `gates.ErrNotFound`, so the gate becomes stale (deployment changed) |
| read failure, or a record without digest or commit | `gates.ErrUnavailable` (denies progression; no invalidation is invented) |
| binder that cannot be joined to a transaction | closed: every read is `ErrUnavailable` |

The gate service and store keep their own checks (`verifyDeployment`, applicability); this adapter never decides a gate.

## Tests

Unit tests over a committed-reader double and a scripted transaction: `Test_gate_deployment_binder` (answers and scoping) and `Test_gate_deployment_binder_atomicity`, which drives the real gate store's `Decide` with `BindDeployments` as its `DeploymentBinder` over a scripted database that separates committed state from each transaction's staged writes. It asserts the deployment is read in the deciding transaction (before the decision is written), a failed read, a cleaned-up, deleted, foreign or changed deployment prevents the decision, a failure after the read (audit, event append, commit) rolls back the decision with its audit row and event, a store without the binder cannot decide a deployment gate, and recovery commits exactly once. It parses no SQL: the joined PostgreSQL path with real rows and locks is T-073/T-038's.
