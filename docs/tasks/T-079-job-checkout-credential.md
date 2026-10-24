---
id: T-079
milestone: M1
status: done
depends_on: [T-016, T-021, T-063, T-078]
decision_blockers: []
owns: [internal/kube/, internal/entrypoint/]
check: make check-go PKGS="./internal/kube/... ./internal/entrypoint/..."
resources: [go]
---
# T-079 — Job checkout credential and secret-directory delivery

## Goal
Make the real restricted Job carry a scoped, read-only private-checkout credential and Git CA to the common entrypoint, and make declared-secret materialization work under the non-root Pod recipe. This removes the test-only Git credential injector currently needed by T-031's real smoke and one execution-assembly blocker in T-071.

## Contract
Add a consumer-owned checkout-credential provider to the kube executor, supplied per tenant/project/workspace by composition. Bind the credential and Git CA into the attempt's existing immutable bootstrap Secret and pass them through the entrypoint's supported `--checkout-credential-file` and `--git-ca-file` arguments. Preserve Job spec fingerprinting, admission-Secret verification, lost-response recovery and immutable-Secret behavior. The Job document, labels, annotations, logs and events must never contain the credential or CA private material. Publisher credentials stay server-side; the Pod receives only a read-only checkout credential. Keep the entrypoint's non-root 0700/0600 secret intent while tolerating the kubelet fsGroup-owned secrets directory without a chmod failure.

## Deliverables
Kube provider/port and recipe/Secret/Job wiring, exact entrypoint arguments/mounts, secret-directory materialization fix, fake-cluster/unit coverage for fingerprint/recovery/foreign credential, and a handoff for T-031/T-071. Export only the minimal constructor/provider needed by composition.

## Acceptance
`job_checkout_credential`, `job_git_ca`, and `job_secret_dir` cover PX-02/AC-07/VS-11 and the restricted execution baseline. Prove the real Job carries the credential/CA only in the immutable bootstrap Secret, the entrypoint receives the files and can authenticate a private checkout without a test proxy, the Job fingerprint/recovery remains stable, and declared file/env secrets materialize without a non-root chmod failure. Fake-cluster tests must use real Job/Secret serialization; entrypoint tests must use real local processes.

## Out of scope
Production composition resolver and root wiring (T-071), workspace policy changes, publishing credentials in the Pod, Git transport/server implementation, or changing frozen runtime schemas.
