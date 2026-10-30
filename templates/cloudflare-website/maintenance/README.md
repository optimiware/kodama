# Maintenance definitions

These are separate source-free operations against the same `website/stage/`
state binding as the publishing pipeline. They are not part of `pipeline.yaml`
and do not create a Deployment entity.

| Action | Command shape |
| --- | --- |
| Observe | `deploy status --state-binding release --worker website-stage` |
| Reconcile an uncertain operation | `deploy reconcile --state-binding release --worker website-stage --operation-id <id>` |
| Roll back | `deploy rollback --state-binding release --worker website-stage --rollback-release <release-id>` |
| Delete owned resources | `deploy cleanup --state-binding release --worker website-stage --operation-id cleanup-<tick>` |

A periodic maintenance run should use a stable tick idempotency key. Expiry and
deletion policy belong to the plugin's state and require an explicit cleanup
operation; deleting a state key alone never deletes Cloudflare resources.
