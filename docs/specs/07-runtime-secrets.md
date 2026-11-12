# S07 — Runtime adapters, materialization, secrets, and logs

Source: [container actions](../../KODAMA.md#container-actions), [files](../../KODAMA.md#environment-and-materialized-files), [secrets](../../KODAMA.md#secret-custody), [environments](../../KODAMA.md#execution-environments). Owners: K03, P09, P14–P17. AC: 1, 6, 7, 9, 11.

## 1. Preparation

One `Prepare(action)` materializes an activation snapshot and attaches the execution's existing writable `/workspace`. Immutable files live outside the workspace, are read-only to the workload, and readable by its configured user:

| Path | Data |
|---|---|
| `/kodama/inputs/<key>` | Admitted input string bytes |
| `/kodama/outputs/<action>/<key>` | Earlier succeeded outputs only |
| `/kodama/task.json` | Admitted task conversation (JSON array of messages); no task metadata; absent when task-free |
| `/kodama/secrets/<NAME>` | Declared activation-time values after policy check |
| `/kodama/secrets/KODAMA_TOKEN` | Current execution credential; only mutable mounted file |

Do not append newlines or reinterpret strings. Apply explicit file modes/ownership and path confinement; defend traversal, symlinks, name collisions, and partial preparation. Preparation failure prevents start. Secret material and mounts are not copied into the shared workspace or log output. Workspace disappearance is a failure, not permission to create a replacement.

Reserved environment: `KODAMA_URL`, `KODAMA_EXECUTION`, `KODAMA_ACTION`, and optional `KODAMA_TASK`. Merge pipeline/action literal environment with action precedence, then reserved context. Task-free runs omit task context. Context values never replace credential validation.

## 2. Script and process behavior

Replace the container entrypoint with a fail-fast script using the image's shell/tools. No tracing by default and no injected Kodama runner. Nonzero command exits stop subsequent commands under the documented shell semantics; do not assume a nonportable shell feature exists. Preserve command bytes/order and test quoting/multiline behavior.

Runtime exit zero is success, nonzero failure. Outputs/custom observations are optional. Images may contain an external agent, ordinary build tools, or a pinned checksum-verified `kodactl`. The Shell does not clone Git or inspect container filesystems for outputs.

## 3. Adapter matrix

| Adapter | Environment | Required implementation |
|---|---|---|
| Process | Development | Explicit process executor/observer, host temp resources, process-group stop, log streams; no isolation claim |
| Podman | Test | Pinned containers, stable labels/IDs, retained workspace volume, mount/update behavior, exit/stop evidence |
| Kubernetes | Test, Production | Native Jobs, restart policy preventing replay, no automatic command retries, tenant namespace/limits/network isolation, retained workspace volume |

All implement one workload contract: capabilities, Prepare, start, observe, logs, stop, remove, credential refresh. Requests never invoke a special runtime adapter. Each adapter documents evidence needed to confirm stop under transport loss, object deletion, node disconnect, and controller restart. Kubernetes Job deletion alone cannot prove the process stopped; uncertainty remains visible and blocks handoff.

Persist workload/resource identity before effect, observe after crash, and retry launch only on affirmative nonexecution evidence. Idempotent deletion must not collapse absent-object evidence into confirmed stop. Credential refresh updates the mounted file without restarting the command and is recoverable after a lost update response.

## 4. Secret custody and key providers

Secrets use `(id, tenant_id, nullable project_id, name, ciphertext, wrapped_key, key_ref, created_at, updated_at)`, unique per scope including tenant-wide rows. Writes replace current values; deletion removes. Management reads return metadata, not plaintext.

Envelope encryption authenticates installation and scoped identity as associated data. C02 freezes the exact byte encoding, cryptographic suite, key-wrapping contract, and key-version handling with tamper tests. Tenant derivation keys use Shell-only encrypted storage separate from workload-declared secrets.

At activation, authorize each declared name and resolve current project value before tenant fallback. Missing/denied/unreadable values fail preparation. Mounted values stay fixed for the workload; later writes affect later activations only. Requests resolve none. Gateway/proxy use upstream secrets inside the Shell on demand.

Key adapters: ephemeral for development, keyfile for test, and one explicitly selected AWS/GCP/Scaleway cloud KMS in production. Preflight refuses a key reference outside the declared key adapter family, including an explicit development reference, and from C-ADAPTER r17 it validates the AWS and GCP provider grammars: an AWS reference is `aws-kms:arn:<partition>:kms:<region>:<account>:key/<key-id>`, `aws-kms:arn:<partition>:kms:<region>:<account>:alias/<alias-name>` or `aws-kms:alias/<alias-name>` (the alias form needs an explicit configured region, and an ARN whose region disagrees with that configuration fails before credential acquisition), and a GCP reference is `gcp-kms:projects/<project>/locations/<location>/keyRings/<ring>/cryptoKeys/<key>`. Bare AWS ids, incomplete GCP key rings, `cryptoKeyVersions` references, traversal, escapes, query strings and fragments are refused. References are not normalized or rewritten into a different persisted identity, and an alias reassignment is not KEK rotation. The `scw-kms` structural grammar stays reserved as r16 and is unchanged here. Production requires TLS on the broker (`tls://` without userinfo or a fragment) and `sslmode=verify-full` on all four PostgreSQL DSNs (runtime, controller, publisher, auth). Bootstrap database/KMS access is installation configuration, not recursive use of the secret store. Fake provider clients prove adapter behavior before G2; actual provider compatibility is qualified later.

## 5. Logs and cleanup

Runtime exposes stdout/stderr chunks with stable stream identity and offsets/sequence information. Shell retains bounded logs and publishes live chunks; broker failure cannot block workload observation/progression. Readers have scope checks, bounded pagination/ranges, and explicit truncation/retention behavior.

No automatic command tracing or credential logging. Test configured redaction of known credential/secret values, but do not claim arbitrary workload output can always be sanitized; workload logs are authorized sensitive data. Raw model/tool content is separately opt-in.

Cleanup tracks owned resource IDs, attempts, last error, and next retry/blocked reason. Terminal outcome is immutable even if cleanup is pending. Stop confirmation precedes safe workspace release; request waits keep workspace resources, while task handoffs clean up the cancelled execution.

## 6. Unit and delayed real proof

Before G2, use temporary files and fake process/Podman/Kubernetes/KMS clients. Test generated scripts/manifests/API requests, failure classification, mount snapshots, renewal recovery, path confinement, declared-secret resolution, logs/backpressure, and stop/cleanup schedules. Unit tests do not start `sh`, Git, containers, or cloud services.

After G2, run the same behavior fixtures through real Process/Podman/Kubernetes adapters. Confirm file permissions, shared workspace through request/controller restart, no replay, actual termination, token replacement, tenant network separation, and lost-resource behavior. Real keyfile/cloud envelope round trips and provider errors also require later evidence.
