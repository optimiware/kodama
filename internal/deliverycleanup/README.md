# internal/deliverycleanup

Deployment expiry and cleanup-run admission (task T-066). A small scheduler over
two consumer-owned ports: `Records` (T-035's deployment records, adapter
`NewRecords(*delivery.Service)`) and `Runs` (execution, `*execution.Service`).
It deploys and deletes nothing itself and touches no work item, workspace or WIP.

* **Expiry.** `Scheduler.Sweep(scope)` / `Tick` / `Loop` request cleanup for settled deployments past
  their `expires_at` (controlled `Clock`), and admit a run for every requested, undeleted one.
  A deployment still applying waits (`not_settled`); a stage with no expiry is cleaned only on request.
* **Admission.** `execution.Service.AdmitCleanupRound` is `AdmitCleanup` with a run identity derived from
  (tenant, project, deployment, round) (`execution.CleanupRunID`), so duplicate claims converge on one run;
  the record owner's current authorization (`DeploymentCleanup`) and the retained-configuration binding
  (T-077) are checked for every new run. Public admission cannot create cleanup runs.
* **Deployment input (T-092).** One project cleanup definition cleans every due deployment: the scheduler admits
  every run against that single definition, and admission (`execution.PrepareCleanup`) hands the run its own
  deployment as the run input `deployment_id` (`execution.CleanupDeploymentInput`), set from
  `CleanupCommand.DeploymentID` after the record owner authorized it, never from caller text. The convention for
  the project cleanup definition: declare `spec.inputs.deployment_id: {type: string, required: true}`; the cleanup
  step then sees it as `KODAMA_INPUT_DEPLOYMENT_ID`, which `delete.sh` reads when `DEPLOY_DEPLOYMENT_ID` is not
  set (T-065). A definition that does not declare the input keeps working and receives nothing (it must name the
  deployment some other way). The value is pinned in the run's snapshot, so retry rounds, a restarted scheduler
  and `Reconcile` rounds each carry the deployment of their own run; it wins over a project variable of the same
  name. A caller-supplied `deployment_id` that is not the admitted deployment is refused (`ErrAdmissionRefused`),
  as is a definition that declares it with a type other than `string`.
* **Retry.** A round that failed with its effects accounted for is retried after `n*RetryBackoff`, at most
  `MaxRounds` rounds (PX-07). An unknown outcome (ambiguous attempt, an unreported success, or a record
  `unknown`) is never retried by a pass; `Scheduler.Reconcile` is the explicit step that admits the next
  round, whose script observes the recorded objects first. A run held open by an unaccounted effect is
  settled by execution's own effect reconciliation first (`ErrInFlight`).
* **Wiring for T-072.** `NewRecords`, `NewBindings(execution.NewCleanupBindings(store))` (the
  `delivery.CleanupBindings` reader), `New(Config)` and `Scheduler.Loop`. The `Scopes` and `Definitions`
  ports are supplied by the composition root. Registration and activation belong to T-072; SQL behaviour,
  non-owner roles and restart to T-067.

Discovery: `Records.Scan` pages every undeleted deployment of a scope by identity; a sweep reads `ScanPages`
pages of `Batch` records and the cursor resumes (and wraps) on the next sweep, so records that stay blocked
(applying, unknown outcome, retries exhausted) cannot starve later expired ones. A retry decision rereads the
record and fails closed on a lookup error or an unknown cleanup state; a round is classified from its
attempts (an ambiguous attempt without replay-safe evidence is an unknown outcome whatever the run's reason).
