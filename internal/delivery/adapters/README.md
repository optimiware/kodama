# internal/delivery/adapters

Adapters that satisfy the delivery module's consumer-owned ports and the delivery-facing ports of peer modules.

## Effect reconciliation (T-093)

`Effects` (`NewEffects(records, bindings)`) implements `execution.EffectReconciler` for deployment steps, so an apply whose answer was lost resolves from the deployment record instead of staying blocked. General commands have no reconciler and stay blocked. T-072 binds the adapter into the process; nothing registers it yet.

The verdict is derived only from the scoped deployment record, never from the attempt's own output, and the evidence names its source (`delivery-record`). The lookup is by action key within the query's scope, so another project's or tenant's record is neither read nor revealed (a foreign record is indistinguishable from none).

| Record (apply) | Verdict |
| --- | --- |
| none | `replay_safe` (nothing was applied) |
| `pending` (registered, not started) | `replay_safe` |
| `running`, applying, no recorded object identities | `still_unknown` (nothing to observe) |
| `running`, applying, with identities | `replay_safe` (`apply.sh` observes before it acts) |
| `running`, `outcome_unknown`, with or without identities | `replay_safe` (`apply.sh` never applies again and settles it by observation) |
| `succeeded` / `failed` | `replay_safe` (a settled action replays without effects) |
| record of another run or step | `still_unknown` |

A cleanup run (one `delivery.CleanupBindings` binds to a deployment) takes its evidence from that deployment: `replay_safe` once cleanup was requested, otherwise `still_unknown`.

An unreadable record is `still_unknown`; an outage (including the binding read) is an `execution.ErrUnavailable` error. Neither is ever affirmative, so the attempt stays blocked until a later pass can read the record. The adapter is only trustworthy for steps that register deployments through `apply.sh`; T-072 must bind it only where that holds.

Tests are unit tests over port doubles (including a withheld mode); real PostgreSQL and cluster qualification belong to T-067.

## Production joins (T-097)

The joins that read delivery records for execution and the gate store live beside their consumers because they run inside those modules' PostgreSQL transactions: the transaction-bound environments reader and the runtime secret route in [`internal/execution/adapters`](../../execution/adapters/README.md), and the `gates.Deployments`/`DeploymentBinder` adapter in [`internal/gates/adapters`](../../gates/adapters/README.md). This package keeps its own boundary (no `pgx`, no `identity/postgres`): `Access` here supplies the authority check they join to the transaction. T-101 binds all three.
