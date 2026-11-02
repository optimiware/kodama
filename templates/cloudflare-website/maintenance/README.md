# Maintenance definitions

These are separate source-free operations against the same `website/stage/`
state binding as the publishing pipeline. They are not part of `pipeline.yaml`
and do not create a Deployment entity.

| Definition | File | Access |
| --- | --- | --- |
| Observe | `status.yaml` | state read |
| Reconcile an uncertain operation | `reconcile.yaml` (input `operation_id`) | state read-write |
| Rollback a recorded release | `rollback.yaml` (input `release_id`) | state read-write |
| Delete owned resources | `cleanup.yaml` (input `operation_id`) | state read-write |

A periodic scheduler should admit these with a stable tick idempotency key.
Expiry and deletion policy belong to the plugin's state and require an explicit
cleanup operation; deleting a state key alone never deletes Cloudflare
resources.
