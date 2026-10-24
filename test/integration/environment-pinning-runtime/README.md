# test/integration/environment-pinning-runtime — environment and credential pin qualification (T-067)

Resource prefix `t067`. Acceptance checks `environment_pinned_at_admission`,
`environment_edit_preserves_pin`, `environment_pin_rollback` and
`environment_pin_restart_scope` (DQ-15/PX-01/IT-02/VS-02), transferred from T-077, run by the
same-named `Test_…` functions. They need only PostgreSQL (`DEV_STACK=services` is enough); the
deployment path that needs the cluster is in [`../delivery`](../delivery/README.md).

```bash
make check-go-integration PKGS="./test/integration/environment-pinning-runtime/..."
```

The pin tests exercise admission, persistence, dispatch context and the delivery callbacks; the scripts' own
run through the Podman runtime and the real secret route (where a rotated or revoked pinned credential is
shown to be what the container receives) is in [`../delivery`](../delivery/README.md). Where a test registers a
deployment it does so as an attempt that was claimed but never launched (`stack.DirectAttempt`).

The package assembles the stack of [`../delivery/stack`](../delivery/stack/stack.go): real execution
(its store **with the transaction-bound environment binder**), secrets, pipeline, artifact,
runtime-authority and delivery stores read and written by the non-owner runtime role under forced
row-level security, and T-077's real adapters. Unit evidence (the component-contract double of
[`../environment-pinning-contract`](../environment-pinning-contract/pinning_test.go)) is not what satisfies
these checks.

| Test | What it proves |
| --- | --- |
| `Test_environment_pinned_at_admission` | admission resolves the environment under the actor's current authority, retains the immutable snapshot (same digest, namespace, exact credential version and delivery form), registers the run's credential pin in the same transaction, and the run's persisted snapshot and the dispatched context both name that pin; no plaintext is stored; environments of another project or tenant are not referenceable; an actor without authority admits nothing and retains nothing; a run without an environment pins nothing; the application role can neither rewrite nor delete a retained snapshot |
| `Test_environment_edit_preserves_pin` | namespace/configuration edits, a rotation, another credential reference and a revocation after admission leave the persisted pin, its retained snapshot, the credential version pin (v1, unreleased, never v2), the attempt's pinned context and — through a real registration — the recorded deployment's namespace, environment, configuration digest and annotation on the **admitted** values; a later run pins the new snapshot (two snapshots coexist); a restart in between changes nothing |
| `Test_environment_pin_rollback` | a credential revoked, an actor revoked, an unretained or credential-altered pin, a delivery outage inside the admission transaction (unavailable, never access) and a failing event append **after** the run and the credential pins were written each leave `runs`, `step_runs`, `run_history`, `secret_pins`, retained source references, `audit` and `outbox` exactly as they were; the identical admission then commits exactly once |
| `Test_environment_pin_restart_scope` | a plan prepared by a process that is gone commits once in the next and a repeat after a lost acknowledgment returns the stored run; dispatch after a restart (with the environment edited and the credential rotated meanwhile) uses only the persisted pin and a third process reads the same context back; another tenant or project cannot read the run, the retained snapshot or resolve the environment by its identifier; the application role under another tenant's row-level security sees none of the pin's rows, and a cross-project copy of a snapshot is refused by the schema |
