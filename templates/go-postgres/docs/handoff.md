# T-043 handoff: Go/PostgreSQL blueprint

## Baseline

- Subject: executable inventory-gap template, version `1.0.0`, amended to run its delivery scripts against real delivery dependencies.
- Immutable source baseline of this amendment: `6344fb637d233c1a3dcee4bce3f5fd6bea2aa650` on `task/T-043-go-blueprint`. Candidate: the commit containing this file; a generated application records its own commit (`generate.sh --git`) and `GENERATED_FROM.json` (`templateVersion`, `templateLockSha256`, `treeSha256`).
- Requirements: T-043, DQ-07/DQ-10, DQ-A04, the Go blueprint checklist; delivery scripts keep DQ-08/DQ-15 pre-apply and pinning semantics; application `INV-01`..`INV-18`.
- Disposition: conforms as component evidence. Assembled factory admission and authorization, gates, exploratory QA and human completion are T-046/T-044 and are not claimed.

## Evidence of the documentation-correction amendment

Base commit `d76922c5b0b47eb523a0f179305d3e66b96bd2a8`; amendment commit `cf575dceb93cf1cc32c011cb8b2cff01616e78e2` (this handoff's own update follows it and sits outside `app/`, so the generated tree is unchanged). Changes: `app/docs/operations.md` (snapshot binding rule: every snapshot binding must appear identically in the pinned context, unrelated context bindings are permitted and not consumed, matching `Snapshot.Verify` and `TestSnapshotBindingContract`; worker startup: starts when membership becomes current, exits non-zero only if provisioning never appears; deployment uncertainty is the factory's `outcomeUnknown`/`outcome_unknown` indication on the existing status, distinct from the application's exception state `unknown`; outer `resources.py` wrapper and explicit timeouts), `README.md`, this file and the blueprint document. No code, migration, pin or factory contract changed.

Generated with `--module acme.example/inventory-gap`: commit `958692cc2bb0b5fcbc8771eb27279c4f38e1d4a4`, tree `5e4f1e26d25fad9bc961679736e47fc9f4140248958878736fdbe4cabbe59190`, template lock `04b9f8fe30ae9e741e95555df5df0f98557122545cda678c75fb5269c4825b68`. The delivery run recorded this commit as `sourceCommit`; tools image `localhost:35811/t043/tools@sha256:bd1dc75351c92f10394c15e0bce53ffc0249b0c09bea2145310ebf14b05bf29c`; application image `sha256:254ce05185fdfa9750a25436eb173a61c84ab223f4636824ff412c033bb245fa` (index over `linux/amd64`, `linux/arm64`, Go `1.27.2`). Provider unchanged: Podman on macOS arm64, kind (Kubernetes v1.37.0), scope `t043`; Docker and Linux CI are not qualified.

| Command | Result |
| --- | --- |
| `make docs-check` | passed |
| `make check-task TASK=T-043` (unit layer, not merge evidence) | passed |
| `python3 scripts/harness/resources.py --resources go,services,kind --execution-timeout 3000 -- go test -race -count=1 -timeout=40m -json ./templates/go-postgres/acceptance/...` | passed, no skips of tests: `Test_blueprint_go_tenant`, `Test_erp_duplicate_resolution`, `Test_blueprint_go_delivery` (243 s), `Test_runtime_config_promotion` (140 s), `TestRealDeliveryRecovery` (147 s), `TestDeliveryScriptContracts`, `TestRuntimeConfigPromotionContracts`, `TestDeliveryRecovery`, `TestFactoryContracts`, `TestSnapshotBindingContract`, `TestConformanceEvidenceDeclaration`, `TestGeneratedAppIndependent`; package times as reported by `go test` under the wrapper's `-p=2`; root package 174 s |
| Same wrapper, `./templates/go-postgres/acceptance/delivery/` with `INVENTORY_REAL_EVIDENCE_DIR` (to read the digests above) | passed, 198 s |

This is component evidence run by the implementer; complete-layer coordinator qualification and fresh review remain required.

## Evidence of the previous candidate (historical)

Generated with `--module acme.example/inventory-gap`: commit `5cd0ae5d74283b5a45c94d47271ae08866af5416` (tree `ff01b6be5dfc95f115d3e8ff62d6c28c89e83610237d35735836f038cfd24427`; regenerating from the candidate reproduces both).

| Item | Value |
| --- | --- |
| Tools image executed by every script | `localhost:35811/t043/tools@sha256:97ae07b59566f7185e24e4267033eea46f8f7a011161f713e81fe15790b6e057` (built from `deploy/tools/Containerfile`, pushed to the scope's registry; recorded in `build-metadata.json` and `deployment.json`) |
| Application image (two concurrent builds of the commit) | `sha256:8b627efe16abdb63f77a48cbf8935c80f803ff36f457c3044438b3cfaa8fde52`, index over `linux/amd64` and `linux/arm64`, Go `1.27.2` |
| Provider and versions | Podman 6.1.4 (AppleHV, macOS arm64), kind v0.33.0 (Kubernetes v1.37.0), kubectl v1.37.1, PostgreSQL 17.6, oauthsonas 1.4.0, registry 3.0.0, scope `t043` |

The Podman results do not qualify Docker or Linux CI; only the macOS arm64 Podman path ran.

## Commands and results of the previous candidate (historical)

| Command | Result |
| --- | --- |
| `go test -race -count=1 -json ./templates/go-postgres/acceptance/...` under `scripts/harness/resources.py --resources go,services,kind` with a provisioned scope | passed, no skips: `Test_blueprint_go_tenant`, `Test_erp_duplicate_resolution`, `Test_blueprint_go_delivery` (398 s), `Test_runtime_config_promotion` (224 s), `TestRealDeliveryRecovery` (164 s), plus `TestDeliveryScriptContracts`, `TestRuntimeConfigPromotionContracts`, `TestDeliveryRecovery`, `TestFactoryContracts`, `TestSnapshotBindingContract`, `TestConformanceEvidenceDeclaration`, `TestGeneratedAppIndependent`; delivery, promotion and recovery packages ran concurrently, the whole run took about 9.5 minutes |
| `go test -tags unit -race -count=1 ./...` in `app/` and `./templates/...` | passed (services skip) |
| `make docs-check` | passed |
| `make check-task TASK=T-043` (unit layer) | one run failed in `kodama/internal/kube` `Test_dispatch_create_response_loss/concurrent_redrive_converges` (a package outside this task; its log shows an unrelated concurrency assertion); every package of this task passed. Not repeated unchanged. |

## What the real suites prove

Named in `app/test/realdelivery` and described in [`app/docs/operations.md`](../app/docs/operations.md#real-delivery-tests). All five scripts run in the restricted tools image (non-root, read-only root filesystem, no capabilities, no socket). Quality reaches a real PostgreSQL; builds are reproducible (one digest) and the image is pulled and run by digest; preview and stage deploy the same digest through real `kubectl`, real migration Jobs and real API/worker Deployments in separate namespaces and databases, each with a namespace-scoped credential; e2e runs against the deployed API with tokens from the development OIDC provider and reaches the ERP's acknowledgment through the deployed worker. Recovery uses real `kubectl` effects: a lost apply response is settled by observation without a second apply, an unknown outcome is never blindly reapplied and keeps object UIDs, and revoked authority, mismatched pins, RBAC denial, foreign objects, migration failure and bounded readiness stop with a recorded outcome.

## Application change found by the real run

A worker deployed with the application starts before its service identity is provisioned in a fresh environment, so it crash-looped and the deployment could not become ready. `app.AwaitWorker` (used by `inventory worker`) now waits, logged, for at most 15 minutes while the membership or schema is not yet there; `NewWorker` stays strict and unprovisioned identities still never run. Regression: `TestWorkerAwaitsProvisioning`.

## Interface changes for the integration owner

None to the factory (no routes, DTOs, settings or events). Application behavior: the worker command waits for provisioning as above. Test layout: the delivery, promotion and recovery drivers moved to their own packages (`acceptance/delivery`, `acceptance/promotion`, `acceptance/recovery`) to keep each inside Go's default 10-minute test timeout; `acceptance/driver` is their shared support.

## Assumptions for the integration owner to confirm

1. Application identity: configurable OIDC verification and application-owned memberships; factory roles and tenant claims grant no application access. Membership administration is provisioning-only; failed exceptions are terminal (`internal/identity/`, `internal/tenancy/`, `docs/specification.md`).
2. Legacy protocol: no vendor ERP protocol is selected. The replaceable HTTP contract (tenant-scoped credentials, stable operation keys, acknowledgment, outcome lookup) is a sample with a controlled fake; no vendor support or production credentials are claimed.
3. Configuration transport gap: the runtime context carries the environment ID, configuration digest and secret bindings but not the environment snapshot. Runs supply a nonsecret snapshot as a declared input; the deploy helper verifies it with the factory's exact serialization and compares ownership, environment, namespace and bindings before applying. T-046 confirms this bridge.
4. Installation inputs: registry endpoints, issuer/audience, environment IDs, agent profile references and the tools image digest (recorded by the installation that builds it) are supplied by T-046.
5. Test environment: the real suites use a provisioned scope of the repository's development environment through `INVENTORY_DEV_ENV` (default `dev/.state/environments/<DEV_SCOPE>`), host-networked containers reaching the host as `host.containers.internal`, and the controlled factory callback served over `https` with a throwaway CA (`SSL_CERT_FILE`), because the helper accepts plain `http` only on loopback. T-043's reviewer confirms this component evidence.

## Limits

- The factory's runtime callbacks and the ERP peer are controlled fixtures; assembled factory admission, authorization, gates, exploratory QA and human completion are T-046/T-044.
- Complete-layer prerequisites: `KODAMA_TEST_POSTGRES_ADMIN_URL`, `KODAMA_REGISTRY_URL`, a provisioned development scope (`make dev-up DEV_SCOPE=t043`), a container runtime and network access. `make test`/`make check-task LAYER=all` reserve `go,services` leases only, and a scope provisioned earlier does not keep its `kind` lease during later tests, so run the real suites under the outer `bun scripts/openforge/src/cli.ts resources --resources go,services,kind --execution-timeout 3000 -- go test -race -count=1 -timeout=40m ./templates/go-postgres/acceptance/...`. Service-free `make check-task TASK=T-043` is never qualifying evidence. The historical command receipts above name the Python wrapper used when those checks ran.
- Measured with warm caches (packages concurrent): delivery about 398 s, promotion 224 s, recovery 164 s, whole run about 9.5 minutes. Cold caches or slower hosts may exceed Go's default 10-minute package timeout; the application suites allow 25 minutes themselves, so pass an explicit `-timeout`.
