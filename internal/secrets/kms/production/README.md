# `internal/secrets/kms/production` — production key wrapping (T-028)

Bridges the consumer-owned `secrets.KeyWrapper` port to **one** cloud KMS: AWS
KMS, GCP Cloud KMS or Scaleway Key Manager. The cryptographic calls are the
existing providers of [`pkg/secretkeys`](../../../../pkg/secretkeys) (which also
holds the ephemeral and keyfile providers). This package does **not**
reimplement wrap/unwrap, add a second `ScopeKey`, rewrap service, value codec or
persistence layer, and it does not touch `internal/secrets/keyfile` (the
development adapter) or `internal/app`. Only the 32-byte scope key is wrapped,
never a secret value.

Requirements: VS-06–VS-10, acceptance VS-A04–VS-A08 (VS-08 revocation remains
owned by the resolver/lifecycle). Decisions: C-09, C-15–C-18, O-05 (resolved),
ADR 0006. Current revalidation baseline `7dcb053d0a68cc3ca1e11be01774764d7d000360`.
Historical references (their evidence is not relabelled as current): prior
revalidation baseline `872561caee091367efb36162f7ac56cb700c0105`; original
implementation baseline `673a910af918b6113451c65bdd0454f274ec6545`.
Retention duration and measured recovery (O-11, resolved in
[decisions](../../../../docs/decisions.md)) are qualified by T-029/T-056; this
package selects no retention period and claims no measured recovery.

## Startup integration

T-105 wires this constructor into `internal/app` through the version-2 `secrets` configuration. Public provider `scw` maps to this package's existing `scaleway` identity. App composition owns SDK credential discovery, the overall startup deadline and client cleanup. T-104 runs the live-provider procedure during Kubernetes deployment; T-029 retains complete recovery qualification. Older T-029 startup-ownership references below describe the original T-028 handoff and are superseded by O-26.

## Use

```go
p, err := production.New(ctx, production.Config{
    Provider:  production.AWS,           // "aws" | "gcp" | "scaleway"; explicit, never inferred
    KEKID:     "kodama-prod",            // logical identity, constant across rotation
    Current:   "v2",                     // immutable label used for every new wrap
    Resources: map[string]string{        // label -> cloud key; current + retained
        "v1": "<key id or arn>", "v2": "<key id or arn>",
    },
}, production.Clients{AWS: awsClient})   // exactly the selected cloud's client
if err != nil { /* fatal startup error: no provider, no fallback */ }
```

`New` returns an error and no adapter for: no provider selected, an unknown or
non-production selection (`keyfile`, `ephemeral`, …), invalid key resources,
a missing client/credential (or a client of another cloud), a failed startup
probe, or a probe timeout. Nothing falls back to another cloud or the keyfile.
`Provider` implements `secrets.KeyWrapper` and `CurrentRef()`; it is immutable
and safe for concurrent use. T-029 wires it into process startup.

### Resources per cloud

| Cloud | `Resources` value | Rejected |
| --- | --- | --- |
| aws | key UUID, `mrk-…` id, or `arn:aws:kms:<region>:<account>:key/<id>` | aliases (mutable), anything else |
| gcp | `projects/P/locations/L/keyRings/R/cryptoKeys/K` | key *version* paths (Cloud KMS selects the version itself) |
| scaleway | key UUID, or `fr-par/<uuid>` | other regions |

Two labels naming one resource are rejected as ambiguous. GCP resource names
compare exactly (case-sensitive); Scaleway UUIDs compare case-insensitively.

## Persisted form and assumptions

`WrappedKey{Bytes, KEK, Algorithm}` stores the provider ciphertext in `Bytes`
(≤ 4096 bytes), `KEK = {cloud, KEKID, version label}` and `Algorithm` = the
provider wrap format (`aws-kms-v1`, `gcp-kms-v1`, `scaleway-symmetric-v1`). The
**cloud key resource is not persisted**: it is resolved from the immutable
version label via `Config.Resources`. Because the resource string is not part
of the authenticated binding, nothing cryptographic stops a label being
repointed at a different key that cannot decrypt the wrap; the mapping must
preserve key identity. A label must never be repointed at a different key, and a retained entry must stay configured while any wrap, backup
or retained history refers to it. Cloud-managed key *material* versions (for
example AWS automatic rotation) are tracked by the provider inside the
ciphertext; operators must not disable or schedule deletion of old material
that backups need. This package chooses no retention period (O-11 is outside
this task).

The authenticated binding is a length-delimited, domain-separated frame
(`kodama/scope-key-kms-binding/v1`) of installation ID, tenant ID, scope-key ID,
KEK provider, wrap format, KEK ID, KEK version, then the 8-byte generation
(frozen by the golden vector in `binding_test.go`). It is passed as the cloud's
authenticated data (AWS encryption-context digest, GCP AAD, Scaleway
`associated_data`), so a wrap cannot move to another installation, tenant,
scope key or generation, or be relabelled to another version. Binding fields and
KEK metadata are validated, and a foreign provider, KEK ID, algorithm or unknown
version is refused, **before** any network call.

`Config` and the wrap record are metadata only; nothing here is a credential.

## Credentials and client ownership

`production.Clients` takes already-constructed clients: an AWS KMS client, a
Cloud KMS client (`NewKeyManagementClient` or REST) or the Scaleway API secret
key. This package reads no environment variable, credential chain, metadata
service or file, and invents no deployment settings. The caller:

* builds each client with the **workload** credential and the intended
  region/endpoint (AWS client region must match the key; Scaleway is `fr-par`);
* owns client lifetime — notably it must `Close()` a GCP client after the
  `Provider` is no longer used;
* owns SDK retry policy. Retries lengthen `Wrap`/`Unwrap`; callers bound them
  with the request context. `ScalewayToken` is never logged (`Clients` prints
  redacted).

Least privilege: encrypt and decrypt on exactly the configured keys (no key
administration, no wildcards). AWS `kms:Encrypt`/`kms:Decrypt`; GCP
`cloudkms.cryptoKeys.useToEncrypt`/`useToDecrypt` (or `roles/cloudkms.cryptoKeyEncrypterDecrypter`);
Scaleway Key Manager encrypt/decrypt on those keys.

**Scaleway location:** the existing provider speaks only to `fr-par` and
records `fr-par/<uuid>`. That is an adapter limitation, not a residency default
or decision; T-029 must record it for installations on Scaleway.

## Startup probe

`New` performs one disposable wrap/unwrap of a random 32-byte key under the
current resource, with a reserved probe context, bounded by
`Config.ProbeTimeout` (default 10 s, max 2 min), and compares the result in
constant time. This proves *current* encrypt/decrypt capability. It does **not**
prove that every retained key still decrypts every backup; that is checked by
the restored-envelope procedure (see `test/integration/kms-production/README.md`)
and T-029.

## Error mapping

| `pkg/secretkeys` | `secrets.KeyErrorClass` | Retryable |
| --- | --- | --- |
| `ErrUnavailable` (transport, throttling, 5xx, timeout), cancelled/expired context, unclassified | `KeyUnavailable` | yes |
| `ErrDenied` (authn/authz, disabled or invalid-state key) | `KeyDenied` | no |
| `ErrNotFound` (key missing, unconfigured retained version) | `KeyNotFound` | no |
| `ErrCiphertext`, `ErrInvalid`, wrong plaintext length, wrong binding | `KeyCorrupt` | no |

`*secrets.KeyError.Err` is a static sanitized cause (or the context error); it
never carries a provider message, request ID, credential, binding or key
material. Plaintext never accompanies an error, and adapter-owned temporary
buffers are cleared.

`pkg/secretkeys` classifies AWS by SDK error code and HTTP status, GCP by gRPC
status (or HTTP status for REST clients), Scaleway by HTTP status.

## Rotation and recovery

* New wraps use only `Current`; retained versions only unwrap.
* `Service.RewrapScopeKey` unwraps (retained or current) and wraps under the
  current key **before** its transactional CAS. Success changes the wrapping
  revision and reference only — not scope identity, generation, value
  ciphertext or pinned versions.
* Any KMS failure returns no usable result and changes no stored state. A
  retry may produce different ciphertext without changing scope-key material.
* A concurrent rewrap loser discards its prepared wrap and reloads through the
  existing service contract; this adapter has no persistence or reconciliation
  loop.
* To rotate: create the new key, add it as a new label, move `Current`, keep
  every prior label until no wrap, backup or history needs it.

## Evidence

Offline, deterministic, no cloud credentials: `pkg/secretkeys` acceptance tests
(`Test_kms_context_tamper`, `Test_kms_rewrap`, `Test_kms_outage`,
`Test_kms_restored_envelope`) run the real AWS and Cloud KMS SDK clients and the
Scaleway HTTP client against local wire-compatible endpoints;
`Test_kms_provider_required` here; companions in this package and
`test/integration/kms-production`. Live-provider qualification and full
restore are T-029 and the per-cloud procedures in
`test/integration/kms-production/README.md`.

### Status of evidence

* **Implemented component behavior:** the adapters, constructor, binding,
  error classification and tests described above.
* **Historical reported results:** earlier revisions reported passing focused
  tests; a receipt for candidate `872561ca…` failed on an unrelated
  `internal/kube` `missing_pinned_fixture` infrastructure check and is
  nonqualifying. It does not establish qualification of the current baseline.
* **Pending current qualification:** complete `LAYER=all` qualification and
  affected-consumer verification of the current candidate belong to the
  coordinator, with fresh review. Focused owned-package race tests are
  developer evidence only.
* **T-029:** process-startup wiring, live-cloud qualification, SDK/credential
  configuration, retention (O-11) and full restore qualification.
