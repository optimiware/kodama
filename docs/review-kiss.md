# KISS review and applied corrections

Reviewed baseline: commit `dea8a04`. This pass preserves the product direction while reducing mechanisms that must be built before the first useful factory run.

## Applied simplifications

| Before | Current design | Authority |
| --- | --- | --- |
| Volume checkpoints, manifests, sessions, lease epochs | Git commits, private clones, one publication slot | [Workspaces](specs/workspaces.md) |
| Generic operations/revisions/blockers/findings/connections | Resource state, content history, `blocked_reason`, defect work items, typed secrets | [HTTP](contracts/http-api.md) |
| Waiting/starting/lost/paused states | Small status enums plus reason/timestamps; gate wait is derived | [Execution](specs/pipeline-execution.md) |
| Harness continuation/handoff framework | One verified wrapper; `needs_input` fails with a question | [Agents](specs/agent-runtime.md) |
| Multiple gate taxonomies, quorum/override policy engine | One scoped canonical subject, one human decision | [Delivery](specs/delivery-and-quality-gates.md) |
| Configurable business completion policy | Explicit human completion with current source/content | [Work management](specs/work-management.md) |
| Five-level variable hierarchy and encryption of ordinary variables | Project values + run inputs; encryption for secrets | [Configuration](specs/variables-and-secrets.md) |
| Build/deployment adapter framework | Ordinary scripts; deployment row is the durable intent | [Delivery](specs/delivery-and-quality-gates.md) |
| Required Loki and remote editor compute | K8s live logs/artifact logs; ordinary Git editors | [Architecture](book/05-system-architecture.md) |
| Provider budget settlement framework | Serialized per-run admission, hard request limits, bounded token overshoot | [Gateway](specs/ai-gateway-and-mcp-proxy.md) |

The owner retained the durable NATS event bus. Outbox, inbox deduplication, retries, retained payloads, and replay procedures remain part of the design.

## Corrections to the simplification proposal

1. **Pod deletion is not fencing.** A partitioned process can survive deletion from the API. Private clones and publisher-owned compare-and-swap prevent stale source publication. External effects still need reconciliation.
2. **Use a normal Git branch.** `refs/heads/kodama/work/<id>` works with ordinary hosting conventions; arbitrary `refs/kodama/*` cannot be assumed portable.
3. **Keep source and spec identity together at review.** A commit alone cannot identify which acceptance criteria/configuration were approved. Gate subjects include content revision and deployment configuration.
4. **Keep optimistic concurrency on content edits.** Restricting `If-Match` to transitions would allow concurrent analyst/agent edits to overwrite each other.
5. **Fix idempotency uniqueness.** Store request hash beside the key; including it in the unique key would allow the same key with different content.
6. **Persist deployment intent before apply.** Removing a dedicated intent subsystem does not remove crash ambiguity. The deployment row itself records the action before the script calls Kubernetes.
7. **Bound aggregate gateway overshoot.** One bounded request of overshoot requires one in-flight request per run across replicas. Unrestricted parallel requests would make the bound false.
8. **Restore requires controller fencing.** Rotating a signing key rejects future tokens; it cannot recall delivered credentials or stop an old Git publisher.
9. **Correct task readiness.** Owning a disjoint folder is insufficient if a task needs an unmerged contract or provider decision. Explicit dependencies and integration tasks now control readiness.
10. **Git belongs in M1.** Source retention/publication is foundation work; provider webhooks remain M2.

## Filled implementation gaps

The new [implementation baseline](implementation/README.md) supplies package ownership, schema sketch/transaction boundaries, the in-Pod entrypoint/result protocol, local dependencies, and meaningful test layers. The [task backlog](tasks/README.md) adds production KMS, integration wiring, restore qualification, source retention, shared-file ownership, and later release blockers.

## Retired requirement IDs

AR-07 and AR-08 cover deferred agent continuation; DQ-12 and DQ-A08 cover the removed override engine. Their numbers are reserved. Other retained IDs keep their domain intent while reflecting the selected simpler mechanism. The [traceability table](traceability.md) maps active requirements and tasks.

## Remaining deliberate gaps

At this initial review, provider-specific webhook/KMS choices, public licensing/name, production sizing/retention, untrusted-hosted isolation and the future UI were explicit coordination points. Subsequent owner selections and deferrals are authoritative in the [decision register](decisions.md) and [task graph](tasks/README.md): the current execution scope is trusted-engineer regular-container staging, while stronger isolation is a later extension. A selected direction still requires implementation and qualification evidence.
