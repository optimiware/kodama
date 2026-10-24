---
name: inventory-delivery
description: Change the ci/*.sh scripts, the delivery helper, manifests or pipelines.
---

Use for `ci/`, `internal/delivery/`, `cmd/delivery/`, `pipelines/` and `deploy/`.

1. Read [operations](../../../docs/operations.md) "Delivery contract". The scripts use only the documented
   variables; the runtime origin and token file are explicit inputs and a token is never read from the environment.
2. The deployment record is registered before any external change; the apply intent is reported before the first
   apply; readiness is structured (observed UID and ready flag), never exit status or output text.
3. After a lost response observe the cluster by the receipt's annotations. While the outcome is unknown, observe
   and settle; never apply again. A revoked, refused or mismatched pin stops all effects.
4. Rendered objects stay in the environment namespace, pull images by digest and follow the restricted baseline;
   `manifests_test.go` enforces it.
5. Pipelines use whole-scalar references and digest-pinned images; keep installation values in
   `@@PLACEHOLDERS@@` and validate rendered output with the factory validator before registering.
6. Credentials: each pod mounts only the files `manifests.go` projects for its component (the API never mounts the ERP
   token); credential files are readable by uid/gid 65532 through `fsGroup`, never world-readable.
7. Snapshot bindings: every binding of the environment snapshot must exist identically in the pinned context; the
   context may declare more. Build evidence names the digest-pinned tools image that ran and the platform set.
8. Evidence: `go test ./internal/delivery/...` (protocol and recovery tests with doubles: development feedback only)
   and the real-component delivery results from the acceptance run, listed separately. Name the candidate commit
   and record any unresolved deviation in the conformance report instead of describing it in prose.
