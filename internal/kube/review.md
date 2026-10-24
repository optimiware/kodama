# T-021 review notes

Candidate-specific evidence lives in the harness handoff (`.harness/handoff.md`, local); this file
records what a reviewer should check and the assumptions the integration owner confirms.

## Baseline

Subject: T-021 — native Job executor and reconciliation; standalone trusted-engineer
regular-container profile. Amendment baseline commit `303e1311e568f9063031ece27ffe27427d92222d`;
the candidate commit is recorded in the harness handoff. Earlier review evidence is not current
evidence. Requirements PX-03, PX-04, PX-08, PX-09, IT-07, IT-08 (supporting SD-01–SD-11); acceptance
PX-A02, PX-A06, IT-A06. Decisions: confirmed C-01, C-02, C-03, C-12, C-16, C-17, C-19; ADRs 0001,
0002, 0003, 0005, 0006, 0007. O-07 selects trusted-engineer regular-container execution and is not a
release blocker for this profile. O-18/O-19 set hosted authority/custody direction; hosted
qualification remains T-061. No stronger sandbox, hosted-authority implementation, Tekton executor
or Pod-deletion source fence is introduced.

## What to check

* Imports and boundaries: Kubernetes wire types stay in `kubernetes/` and the recipe/verification
  code that must speak them; planning (`recipe.go`, `template.go`) does no I/O; no domain registry;
  only `internal/kube/` and `test/integration/kube/` change.
* `workload.go` owns the external-effect transitions; execution statuses are never set here.
* `verify.go` compares actual controlled spec fields, not annotations.
* `adapters/runtimeauth.go` uses `execpg.Store.ObserveAttempt` / `execpg.LockAttempt`; the
  transaction-bound translation makes no Kubernetes call.
* `reconcile.go` scans managed cluster resources as well as durable attempts, so late creates of
  terminal runs are found; lookup outages are errors, never orphan evidence.
* `executor.go` termination: UID preconditions, foreground deletion; committed launch evidence
  (execution's and the record's) is never discarded, and a missing Secret never yields `not_started`.
* `reconcile.go` orphan cleanup: Pods and Jobs of an absent or terminal attempt are deleted only with
  corroboration from committed data (see README); candidates without it stay untouched.
* `preflight.go` is deliberately conservative (see "Namespace preflight" in the README): port
  ranges are refused, only unscoped ResourceQuotas count, and a policy selecting Pods through the Job
  controller's labels (current or legacy name/UID labels, whose values are per-Job) is inspected as a
  workload policy. `kubernetes/types.go` keeps `endPort` and quota scopes so the check sees them.
* `reconcile.go` `scanJob` uses execution's observed UID as well as the record's (`committedUIDs`), so a
  same-name replacement is never deleted after the recovery Secret is lost; `scanSecret` deletes only a
  Secret that validates as the attempt's own record, never on a validation failure.
* `kubetest.Require` skips through `layer.SkipService` before inspecting any kubeconfig, so the unit
  layer (`-tags unit`) never reaches kubectl, a container runtime or an image build.
* `workload.go` `NoteLaunch` merges like termination does: `started` outranks `not_started`, which
  outranks `unknown`, and the stronger value is persisted before a Pod is deleted.
* `test/integration/kube/run.sh` requires both a cluster and PostgreSQL; fixture images and build
  directories are per invocation.

## Assumptions

0. **Conservative preflight subset.** Port ranges are rejected rather than supported, and only
   unscoped ResourceQuotas count toward the required Pod/CPU/memory/ephemeral-storage bounds.
   These restrictions are isolated in `preflight.go`; deployment owners supply explicit ports and
   an unscoped quota. The T-021 reviewer confirms the subset. The service-account check is the
   explicit list of forbidden permissions in `forbiddenAccess`, not proof of every RBAC permission.
1. **Execution policy.** T-020 pins context and Job identity but supplies no namespace, service
   account, resource quantities or injector image. `ExecutionPolicy` is an immutable value injected
   through constructors (`policy.go`); a missing tenant denies dispatch; a changed policy does not
   silently alter an existing attempt (the committed fingerprint no longer matches, so the attempt
   is not re-planned or adopted). T-021 review and the T-071/T-027 integration owners confirm deployment values.
2. **Credential recovery.** T-019 stores only the credential digest. An attempt-scoped Secret keeps
   the generated credential and minimal create-recovery metadata, protected by resource-version
   checks, so a lost registration or create response never regenerates a registered credential.
3. **Transport.** A narrow standard-library HTTP adapter with an injected authenticated
   `http.Client`; no Kubernetes SDK and no kubeconfig loader. T-071/T-027 supply production
   authentication and scheduling. The review confirms the bounded API surface (`kubernetes/client.go`).
4. **Cluster fixtures.** The merged environment pins kind/Kubernetes but not a NetworkPolicy
   enforcing implementation. The fixtures use kind's default CNI (kindnetd) and enforcement is
   evidence-dependent, not assumed from its name: `Test_restricted_job` probes real connectivity and fails otherwise. The fixture
   pins are in `test/integration/kube/fixtures/pins.json`; this is replaceable test infrastructure,
   not the O-07 decision.
5. **Timing.** T-020's startup/execution deadlines and T-016's command timeout are the only
   deadlines. Primary-container launch is conservative evidence, not proof of external effects.
   `Options.CreateQuiescence` is a mechanical bound on how long an unknown create is assumed able
   to land, not a deadline.
6. **Retention.** `Options.CompletedRetention` (default 0: delete after settlement) is an operator
   choice; the integration owners set the production value and qualify `CreateQuiescence` against API request timeouts.
7. **Recovery bookkeeping.** The attempt-scoped Secret is Kubernetes-local bookkeeping, not authority: runtimeauth
   persists only credential digests. Orphan cleanup trusts it only after validating it as the attempt's own
   and matching the controlling UID; it grants no execution authority.
