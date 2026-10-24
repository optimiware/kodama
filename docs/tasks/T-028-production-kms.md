---
id: T-028
milestone: production
status: done
depends_on: [T-012]
decision_blockers: []
owns: [internal/secrets/kms/production/, test/integration/kms-production/, pkg/secretkeys/]
check: make check-go
resources: [go]
review: human
---
# T-028 — Production KMS adapters

## Goal
Wrap scope keys with the KMS of the installation cloud: AWS, GCP, or Scaleway.

## Contract
Consume T-012's scope-key wrap/unwrap port through `pkg/secretkeys`. Do not reimplement wrap/unwrap. C-18/O-05 require AWS KMS, GCP Cloud KMS, and Scaleway Key Manager behind that port, plus the ephemeral and keyfile providers in the same package. Each wraps the 32-byte scope key, not every secret value. The installation selects one provider. Missing configuration or a failed provider fails startup; do not fall back to another cloud or to the development keyfile.

## Deliverables
Three provider adapters, workload credential/config for each, rewrap/key-version behavior, outage classification, and a real-provider test procedure per cloud.

## Acceptance
`kms_context_tamper`, `kms_rewrap`, `kms_outage`, `kms_restored_envelope`, and `kms_provider_required` verify VS-06–VS-10. Those checks are the `pkg/secretkeys` tests (binding rejection, rewrap, classified outage, retained-key unwrap, and a missing provider) plus the owned constructor. A production profile with no selected provider returns a startup error and does not fall back to the keyfile or another cloud. Do not mark acceptance blocked for lack of live cloud credentials. T-029 qualifies a live installation and wires the constructor into process startup; do not edit `internal/app`.

## Out of scope
Treating the development keyfile as production, or adding a fourth KMS provider.
