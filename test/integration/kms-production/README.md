# KMS production tests and real-provider procedure (T-028)

## Offline tests (mandatory, no cloud credentials)

`go test -race -count=1 ./test/integration/kms-production/ ./internal/secrets/kms/production/ ./pkg/secretkeys/`
(or `make test-task TASK=T-028`).

Here, `rewrap_test.go` and `restore_test.go` drive `production.New` with real
AWS and Cloud KMS SDK clients against a local, wire-compatible KMS endpoint
(AES-256-GCM per key resource, authenticated data enforced), the existing
`secrets.ScopeKey` entity and the existing `envelope` codec. They prove: rewrap
keeps scope identity, generation, value ciphertext and value version; a stale
revision is rejected; a failed preparation or startup during an outage leaves
the stored wrap usable; and a backup restored after two rotations opens with a
fresh adapter that retains the old version, and fails closed without it.
Scaleway's endpoint is fixed to `api.scaleway.com`, so its wire behavior is
covered inside `pkg/secretkeys` (which may override the endpoint) instead.

These are **not** a live qualification and use no PostgreSQL. T-012's real
PostgreSQL CAS/history tests remain the database regression evidence. T-029
qualifies a live installation and wires `production.New` into process startup.

## Real-provider procedure (per cloud)

Run it once per installation cloud, from a throwaway project/account/namespace,
with the **workload** identity the installation will really use. Record only
safe metadata (key identifiers, provider, versions, pass/fail, timings). Never
print or store plaintext keys, credentials, tokens, wrapped bytes or provider
response bodies. Never destroy, disable or schedule deletion of a retained key;
only delete the throwaway keys created by step 1 of this procedure after the
final step, and only if they never wrapped real data.

Common steps (adapt the commands to the cloud below):

1. **Create two symmetric encrypt/decrypt keys** `A` and `B` (resource prefix
   `t028` in names), same location as the installation. No aliases for AWS.
2. **Grant least privilege** to the workload identity: encrypt and decrypt on
   exactly `A` and `B`. Confirm it has no administrative key permission.
3. **Authenticate as the workload** and construct the client in the deployment
   process shape (not an operator's personal credential).
4. **Round trip:** `production.New` with `Current: v1 → A`. It must start (the
   startup probe is a real round trip). Wrap and unwrap a random 32-byte key
   with a real `secrets.WrapContext`.
5. **Tamper rejection** (after both `A` and `B` are configured as `v1` and
   `v2`, so a relabel reaches the cloud): unwrap with a changed installation,
   tenant, scope-key ID or generation, with one flipped ciphertext byte, and
   with the stored `v1` wrap relabelled as the other configured version `v2`.
   Each must fail with `KeyCorrupt` (authentication failure, no plaintext). A
   version label that is not configured at all (for example `v9`), a foreign
   provider, KEK ID or algorithm is `KeyNotFound` and is rejected before any
   network access; it is not evidence of cloud-side tamper rejection.
6. **Rotation:** `Current: v2 → B`, retaining `v1 → A`. Unwrap the step-4 wrap
   (retained), wrap again (must record `v2`), confirm identical key material and
   that new wraps never record `v1`.
7. **Restored old envelope:** with a fresh process/adapter and the persisted
   step-4 metadata, unwrap succeeds with `v1` retained and fails with
   `KeyNotFound` when `v1` is removed from `Resources`.
8. **Denied:** run as an identity without decrypt on `A`; startup or unwrap must
   fail as `KeyDenied`, not retried, not falling back.
9. **Outage/unreachable:** break network egress to the KMS endpoint (firewall or
   endpoint override); startup must fail within `ProbeTimeout`, and runtime
   calls must be `KeyUnavailable` and retryable. Restore access and confirm
   recovery with no restart-time state change.
10. **Missing or unusable key (isolated failure injection only):** never
    repoint a retained label of a real installation. In a throwaway
    configuration, either remove a label from `Resources` (`KeyNotFound`,
    rejected before network access) or point a throwaway label at a key
    resource that never existed or was deleted in the throwaway project (the
    cloud reports absence; `KeyNotFound`). A key that exists but is disabled,
    pending deletion or in an invalid state is a different case and is
    classified `KeyDenied`. Label-to-key mappings of real wraps are immutable.
11. **Cleanup** the throwaway keys per the rule above and the test identity.

### AWS KMS

* Keys: `aws kms create-key --key-spec SYMMETRIC_DEFAULT --key-usage ENCRYPT_DECRYPT`. Use the key ID or ARN, never `alias/...`.
* Policy: `kms:Encrypt`, `kms:Decrypt` on the two key ARNs for the workload role (IAM role for service account, instance profile, or equivalent).
* Client: `kms.NewFromConfig(awsCfg)` with the role's credentials and the key's region.
* Denied: remove the workload's decrypt grant, or use a role without it (`AccessDeniedException`). Outage: block the regional `kms.<region>.amazonaws.com` endpoint. Missing: a key pending deletion or disabled maps to a denied/invalid-state classification (`KeyDenied`); a nonexistent key ID is `KeyNotFound`.
* Do not schedule deletion of a key that wrapped real data (7-30 day pending window is not a retention policy).

### GCP Cloud KMS

* Keys: a symmetric CryptoKey (`purpose ENCRYPT_DECRYPT`) in a key ring in the installation location. Use the CryptoKey resource name, not a version path.
* Role: `roles/cloudkms.cryptoKeyEncrypterDecrypter` on each key (not project-wide).
* Client: `kms.NewKeyManagementClient(ctx)` using the workload identity (Workload Identity, service account); the caller `Close()`s it.
* Denied: remove the role binding (`PERMISSION_DENIED`). Outage: block `cloudkms.googleapis.com`. Disabled/destroyed key versions are classified as denied (`FAILED_PRECONDITION`); keep prior key versions enabled while backups need them.

### Scaleway Key Manager

* Keys: symmetric Key Manager keys in `fr-par` (the only region the adapter supports). Use the key UUID.
* Credential: an API secret key (IAM application) whose policy allows Key Manager encrypt/decrypt on the project of those keys only. Pass it as `Clients.ScalewayToken`.
* Denied: a key without the Key Manager permission set (HTTP 403). Outage: block `api.scaleway.com`. Missing: delete only a throwaway key (HTTP 404).
* A key in another region is rejected at configuration time.

## Handoff to deployment and recovery

Under O-26, T-105 owns process wiring: configuration, workload credentials,
`production.New`, fatal startup errors, service injection and client cleanup.
T-104 runs the real-provider procedure during Kubernetes deployment with the
actual workload identity. T-029 retains full restore qualification with the
key versions required by every supported backup. The public configuration name
for Scaleway is `scw`; the persisted provider identity remains `scaleway`.
