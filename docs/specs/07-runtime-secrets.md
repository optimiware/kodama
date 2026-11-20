# S07 — Runtime adapters, materialization, secrets, and logs

Source: [container actions](../../KODEMA.md#container-actions), [files](../../KODEMA.md#environment-and-materialized-files), [secrets](../../KODEMA.md#secret-custody), [environments](../../KODEMA.md#execution-environments). Owners: K03, P09, P14–P17. AC: 1, 6, 7, 9, 11.

## 1. Preparation

One `Prepare(action)` materializes an activation snapshot and attaches the execution's existing writable `/workspace`. Immutable files live outside the workspace, are read-only to the workload, and readable by its configured user:

| Path | Data |
|---|---|
| `/kodema/inputs/<key>` | Admitted input string bytes |
| `/kodema/outputs/<action>/<key>` | Earlier succeeded outputs only |
| `/kodema/task.json` | Admitted task conversation (JSON array of messages); absent when task-free |
| `/kodema/task/title` | Admitted task title (UTF-8 text); absent when task-free |
| `/kodema/task/description` | Admitted task description (UTF-8 text); absent when task-free |
| `/kodema/task/metadata.json` | Admitted task metadata (deterministic JSON object, `{}` when empty); absent when task-free |
| `/kodema/task/spec.json` | Admitted task specification JSON (`{"id","title","description","metadata"}`); absent when task-free |
| `/kodema/secrets/<NAME>` | Declared activation-time values after policy check |
| `/kodema/secrets/KODEMA_TOKEN` | Current execution credential; only mutable mounted file |
| `/kodema/bin/kodema` | The toolbox: the control plane's own static `kodema` binary, read-only and executable (Podman and Kubernetes) |
| `/kodema/outputs/<this action>/` | Writable and initially empty, only when the action declares outputs; never read by the Shell |
| `$KODEMA_CA_FILE` | Optional installation CA bundle: `/kodema/ca.crt` when `WorkloadCAFile` is configured on the Podman runtime, `/etc/kodema/ca/ca.crt` when a CA Secret is configured on the Kubernetes runtime |

Do not append newlines or reinterpret strings. Apply explicit file modes/ownership and path confinement; defend traversal, symlinks, name collisions, and partial preparation. Preparation failure prevents start. Secret material and mounts are not copied into the shared workspace or log output. Workspace disappearance is a failure, not permission to create a replacement.

Reserved environment: `KODEMA_URL`, `KODEMA_EXECUTION`, `KODEMA_ACTION`, and optional `KODEMA_TASK`. When configured models or LLM proxy providers are enabled for the execution's scope, activation also injects unshadowable `KODEMA_LLM_OPENAI_BASE_URL=<KODEMA_URL>/llm/openai/v1` and `KODEMA_LLM_ANTHROPIC_BASE_URL=<KODEMA_URL>/llm/anthropic`; when a CA bundle is configured, `KODEMA_CA_FILE` names its path on either container runtime so `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, `CURL_CA_BUNDLE`, and `GIT_SSL_CAINFO` can trust private/local TLS directly. When the installation configures a workload Git URL (`KODEMA_WORKLOAD_GIT_URL`; in production `{tenant}` stands for the execution's tenant), the adapter exposes it as `KODEMA_GIT_URL` and adds `GIT_CONFIG_COUNT` with `GIT_CONFIG_KEY_<n>`/`GIT_CONFIG_VALUE_<n>` pairs that set, for that origin only, `credential.<origin>.helper` first to the empty string, which resets the helper list so no helper of the image is asked for or keeps the execution credential, then to `/kodema/bin/kodema git credential` restricted to that origin, and, with a CA bundle, `http.<origin>.sslCAInfo` to its path. A pipeline's own `GIT_CONFIG_COUNT`, `GIT_CONFIG_KEY_<n>` and `GIT_CONFIG_VALUE_<n>` entries are dropped when this configuration is present. Merge pipeline/action literal environment with action precedence, then reserved context. Task-free runs omit task context. Context values never replace credential validation.

## 2. Script and process behavior

Replace the container entrypoint with a fail-fast script using the image's shell/tools. No tracing by default and no Kodema process that supervises the commands: the Kernel renders the script unchanged and knows nothing of the toolbox. Nonzero command exits stop subsequent commands under the documented shell semantics; do not assume a nonportable shell feature exists. Preserve command bytes/order and test quoting/multiline behavior.

The container runtimes start the script through one launcher, owned by `internal/shell/workspace` and identical on Podman and Kubernetes. The image's shell is the container's first process and runs, in order:

1. the Kernel's script, in `/workspace`, with `/kodema/bin` appended to the image's own `PATH` (never replacing it, and never leading with an empty element);
2. only when the action declares outputs and the script exited zero, `/kodema/bin/kodema output push`, replacing the shell.

The exit status is the script's when it failed and the push's otherwise, so a refused submission fails the action through the ordinary exit-status rule. An action that declares no outputs runs step 1 alone. The Process adapter mounts no toolbox and runs the script as before.

`kodema output push` reads regular files directly inside `/kodema/outputs/$KODEMA_ACTION`, in name order. The file name is the output key and the content the value, with exactly one trailing line feed removed; an empty file is the empty string. It refuses more than eight entries, links, special files, subdirectories, invalid UTF-8 and oversized values before any request. With no file it exits zero and sends nothing. Otherwise it reads the execution for its action ID and version, submits one map with an idempotency key derived from the execution, the action and the values, and on `version_conflict` reads again and retries with the same key, a bounded number of times. Validation of keys and values stays with the operation (S02 §4).

The toolbox is the binary the control plane itself runs, so its version is the installation's. Podman bind-mounts one installation copy read-only; Kubernetes copies it from the control plane's image, pinned by digest, with an init container into a volume the workload mounts read-only. That init container runs no pipeline command, holds no execution credential and mounts neither the workspace nor the activation files, and its termination is never stop evidence for the workload.

On Kubernetes the chart supplies `KODEMA_TOOLBOX_IMAGE=<image.repository>@<image.digest>`. Use a multi-architecture index if tenant nodes have different architectures. The installer mounts only its pod-local toolbox volume. The workload mounts it read-only, with a separate memory-backed volume limited to 1 MiB for its own outputs. The chart derives the Git endpoint from the per-tenant Git Service when enabled; `config.workloadGitURL` overrides it.

Runtime exit zero is success, nonzero failure. Outputs/custom observations are optional. Images may contain an external agent and ordinary build tools; they need no Kodema software. The Shell does not clone Git or inspect container filesystems for outputs.

## 3. Adapter matrix

| Adapter | Environment | Required implementation |
|---|---|---|
| Process | Development | Explicit process executor/observer, host temp resources, process-group stop, log streams; no isolation claim |
| Podman | Test | Pinned containers, stable labels/IDs, retained workspace volume, mount/update behavior, exit/stop evidence |
| Kubernetes | Test, Production | Native Jobs, restart policy preventing replay, no automatic command retries, tenant namespace/limits/network isolation, retained workspace volume |

All implement one workload contract: capabilities, Prepare, start, observe, logs, stop, remove, credential refresh. Requests never invoke a special runtime adapter. Each adapter documents evidence needed to confirm stop under transport loss, object deletion, node disconnect, and controller restart. Kubernetes Job deletion alone cannot prove the process stopped; uncertainty remains visible and blocks handoff.

Persist workload/resource identity before effect, observe after crash, and retry launch only on affirmative nonexecution evidence. Idempotent deletion must not collapse absent-object evidence into confirmed stop. Credential refresh updates the mounted file without restarting the command and is recoverable after a lost update response.

## 4. Secret custody and key providers

Public secret management (`secret.write`, `secret.metadata.read`, `secret.metadata.list`, `secret.delete`) is project-scoped (`ScopeProject`, requiring `project_id`); tenant-scoped secret management is not exposed on the public API. The `secrets` table retains `(id, tenant_id, nullable project_id, name, ciphertext, wrapped_key, key_ref, created_at, updated_at)`, unique per scope including any internally stored tenant-wide rows. Writes replace current values; deletion removes. Management reads return metadata, not plaintext.

Envelope encryption authenticates installation and scoped identity as associated data. C02 freezes the exact byte encoding, cryptographic suite, key-wrapping contract, and key-version handling with tamper tests. Tenant derivation keys use Shell-only encrypted storage separate from workload-declared secrets.

At activation, authorize each declared name against the server's workload secret-name policy (`Config.SecretRules` / `KODEMA_SECRET_RULES_FILE`) and resolve the current project value (with internal tenant-wide fallback only when no project row exists). Workload name policy is deny-by-default when unconfigured and `production` accepts exact secret names only (refusing wildcard `allow_all` rules); local `kodema up` stacks configure a project-scoped `allow_all` rule by default (overridable via `--allow-project-secrets` / `--allow-secret`) while continuing to block `KODEMA_TOKEN`, nonexistent secrets, and other scopes. Missing/denied/unreadable values fail preparation, and policy denials name the specific unauthorized secret without revealing allowed rule names or secret material. Mounted values stay fixed for the workload; later writes affect later activations only. Requests resolve none. Gateway/proxy use upstream secrets inside the Shell on demand.

External coding harnesses (`Claude Code`, `Codex`, `OpenCode`, `Pi`) stream native provider protocols through `/llm/{provider}/{upstream_path...}` (`POST /llm/anthropic/v1/messages`, `POST /llm/openai/v1/responses`, `POST /llm/openai/v1/chat/completions`) without ever mounting raw `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` secrets into the workload container. The workload presents `/kodema/secrets/KODEMA_TOKEN` via `Authorization: Bearer <token>` or `x-api-key: <token>` (`ANTHROPIC_BASE_URL="${KODEMA_LLM_ANTHROPIC_BASE_URL:-${KODEMA_URL}/llm/anthropic}" ANTHROPIC_API_KEY="$(cat /kodema/secrets/KODEMA_TOKEN)"` for Claude Code; `OPENAI_BASE_URL="${KODEMA_LLM_OPENAI_BASE_URL:-${KODEMA_URL}/llm/openai/v1}" OPENAI_API_KEY="$(cat /kodema/secrets/KODEMA_TOKEN)"` for Codex/OpenCode/Pi). Before opening any upstream connection, the Shell verifies a running container workload principal holding `configured_models` within its active action deadline, strips both `Authorization` and `x-api-key`, resolves the configured provider secret through the gateway secret policy, refuses HTTP redirects, and flushes SSE `text/event-stream` chunks immediately; execution cancellation or action deadline expiry terminates in-flight streams.

Key adapters: ephemeral for development, keyfile for test, and one explicitly selected AWS/GCP/Scaleway cloud KMS in production. Preflight refuses a key reference outside the declared key adapter family, including an explicit development reference, and from C-ADAPTER r17 it validates the AWS and GCP provider grammars: an AWS reference is `aws-kms:arn:<partition>:kms:<region>:<account>:key/<key-id>`, `aws-kms:arn:<partition>:kms:<region>:<account>:alias/<alias-name>` or `aws-kms:alias/<alias-name>` (the alias form needs an explicit configured region, and an ARN whose region disagrees with that configuration fails before credential acquisition), and a GCP reference is `gcp-kms:projects/<project>/locations/<location>/keyRings/<ring>/cryptoKeys/<key>`. Bare AWS ids, incomplete GCP key rings, `cryptoKeyVersions` references, traversal, escapes, query strings and fragments are refused. References are not normalized or rewritten into a different persisted identity, and an alias reassignment is not KEK rotation. The `scw-kms` structural grammar stays reserved as r16 and is unchanged here. Production requires TLS on the broker (`tls://` without userinfo or a fragment) and `sslmode=verify-full` on all four PostgreSQL DSNs (runtime, controller, publisher, auth). Bootstrap database/KMS access is installation configuration, not recursive use of the secret store. Fake provider clients prove adapter behavior before G2; actual provider compatibility is qualified later.

## 5. Logs and cleanup

Runtime exposes stdout/stderr chunks with stable stream identity and offsets/sequence information. Shell retains bounded logs and publishes live chunks; broker failure cannot block workload observation/progression. Readers have scope checks, bounded pagination/ranges, and explicit truncation/retention behavior.

No automatic command tracing or credential logging. Test configured redaction of known credential/secret values, but do not claim arbitrary workload output can always be sanitized; workload logs are authorized sensitive data. Raw model/tool content is separately opt-in.

Cleanup tracks owned resource IDs, attempts, last error, and next retry/blocked reason. Terminal outcome is immutable even if cleanup is pending. Stop confirmation precedes safe workspace release; request waits keep workspace resources, while task handoffs clean up the cancelled execution.

## 6. Unit and delayed real proof

Before G2, use temporary files and fake process/Podman/Kubernetes/KMS clients. Test generated scripts/manifests/API requests, failure classification, mount snapshots, renewal recovery, path confinement, declared-secret resolution, logs/backpressure, and stop/cleanup schedules. Unit tests do not start `sh`, Git, containers, or cloud services.

After G2, run the same behavior fixtures through real Process/Podman/Kubernetes adapters. Confirm file permissions, shared workspace through request/controller restart, no replay, actual termination, token replacement, tenant network separation, and lost-resource behavior. Real keyfile/cloud envelope round trips and provider errors also require later evidence.
