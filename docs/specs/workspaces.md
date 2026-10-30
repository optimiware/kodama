# Workspaces

**Scope:** M1, Git-backed source continuity. Human editor access is ordinary Git clone/commit/push. The durable **source workspace** below retains the existing `workspace` API names; it is distinct from the **run workspace** (`attempt_private` scratch per attempt today, `run_shared` shared filesystem per run under [ADR 0010](../adr/0010-run-workspace-services-builders.md)). [ADR 0012](../adr/0012-container-sdlc-and-artifact-releases.md) clarifies the product vocabulary and output retention; it does not rename persisted IDs or change Git hosting authority.

## Model

A workspace stores ownership, work-item ID, repository URL, separate checkout/publisher secret references, work branch, accepted commit, `writer_attempt_id`, publication fields, state, and `last_error`. Checkout authority is read-only; publisher authority stays in the control plane. States are `provisioning`, `available`, and `archived`; errors are reasons on the resource. Archive/restore change availability without deleting source history.

The default branch is `refs/heads/kodama/work/<work-item-id>`. A normal branch is portable across common Git hosts, unlike assuming support for arbitrary `refs/kodama/*`. A checkpoint is a commit reachable through retained Git refs. No PVC snapshots, workspace sessions, or blob checkpoint manifests are required.

`[attempt_private]` (current) Each attempt clones its pinned commit into private scratch storage. `read` means the attempt cannot publish source; its disposable checkout may still be modified by build tools. `write` grants the sole publication slot. Uncommitted/ignored files are ephemeral; recovery starts from the last published commit. Users must commit source they want retained.

## Publication protocol

1. Reserve `writer_attempt_id` in PostgreSQL. Remember the expected branch tip and input commit.
2. Run the attempt against its own checkout. The runtime has read-only Git access; it does not receive canonical-branch write credentials.
3. The entrypoint submits a bounded Git bundle through the artifact transport and proposes its commit to the control-plane publisher. This temporary transport object is not a durable checkpoint format.
4. The publisher validates ownership, the live writer/attempt, commit ancestry, and expected tip. It records `publish_expected_commit` and `publish_commit` on the workspace before pushing.
5. Publish with an explicit expected-old-ref compare-and-swap and fast-forward ancestry check. Keep the publication slot until the Git result is known. A concurrent human push causes a conflict, not an overwrite.
6. Record the accepted commit, audit/event, and release the writer after result/workload settlement. Retain per-run Git refs so later branch movement cannot make pinned commits unreachable.

Only the publisher holds branch-write credentials. There is one in-flight publisher per workspace. A failed/unknown push is resolved by reading the remote ref before admitting another writer. Reassignment after a node failure revokes the old attempt's publication authority; in `attempt_private` private scratch copies prevent filesystem overlap, while `run_shared` prevents it with a verified single holder ([below](#run-scratch-run_shared)).

A publication authorized before cancellation may still finish remotely. Reconciliation records that effect without making a cancelled attempt successful. An unchanged ref after a timeout alone does not prove an in-flight request can never complete: retain uncertain publication ownership until resolved, or reconcile explicitly before retry/reassignment. New accepted source always uses the observed expected tip and validated provenance.

Kubernetes deletion is a cleanup observation, not proof that a partitioned process stopped. CAS publication protects accepted source; external tool/deployment side effects still need their own uncertainty handling. During restore, fence old control-plane publishers and rotate their Git credentials before resuming publication.

## Run scratch (`run_shared`)

Normative design of the `kodama/v1alpha2` model; implementation T-113–T-117; not delivered or qualified. Resource lifecycle, identity and ordering are in the [run-resources contract](../contracts/run-resources.md); the entrypoint/wire is [runtime v2](../contracts/runtime-v2.md).

- **One preparation.** A run owns one bounded scratch filesystem. A source-backed run checks out the admitted commit once, before any user command, in a controller-owned workload with read-only repository credentials (hooks and implicit submodule recursion off, credentials gone afterwards) that must be verified stopped. The control plane records a receipt bound to the run, scratch identity and commit; a marker file in the volume is never evidence because tenant code can write it. A source-free run gets scratch that is empty at initial provisioning only (earlier steps' files persist for later steps exactly as in a source-backed run) and no checkout credential. After preparation the scratch is never reset, recloned or rebuilt between steps, on retry or after restart. Moving the work branch does not refresh it.
- **Shared and cumulative.** Every executable step, each with its own pinned image, mounts the same `/workspace`. Untracked, ignored and generated files survive across steps. The run's input `sourceCommit` stays immutable; context separately records the latest commit successfully published *in this run* (`headCommit`) and the lineage of attempts that began commands. `read` means no publication authority, not an immutable filesystem. Edits by non-publishing steps are not a checkpoint; a later writer may commit them, and the publication records that lineage.
- **Exclusive holder.** At most one executable attempt holds the scratch at a time. The next attempt is claimed only after the prior workload is verified stopped (not merely deleted or its SQL claim expired), its result/publication settled and the volume observed released; uncertain termination blocks handoff. A partitioned or late process therefore cannot overlap a successor on the same files, and retry/redrive cannot give a possibly live holder's volume to another attempt. This run-scratch ownership is separate from the logical workspace's writer reservation.
- **Cumulative publication.** Writers use the unchanged protocol: writer reservation, expected tip, temporary bundle, publisher-owned expected-old-ref CAS with ancestry validation, unknown-publication reconciliation, retained per-run refs. The commit selection of the existing entrypoint (all non-ignored changes) applies to the whole shared tree, so a writer can commit files an earlier step generated. A human push between writers makes the later proposal conflict (`publication_conflict`); there is no automatic merge or rewind. The publisher validates against Git, not against any claim made from scratch.
- **Disclosure.** Mount exclusion and known-value scanning keep injected secrets out of source and outputs; they cannot stop a command that was explicitly given a secret from copying it into shared files. Source/output filtering therefore also considers values delivered to *earlier* attempts of the run, and a retained disclosure limit is documented rather than promised away ([secrets](variables-and-secrets.md)).
- **Git hardening.** Git runs with explicit sanitized configuration; repository- or tenant-written config, hooks, filters, credential helpers, attributes, symlinks and changed ownership are never trusted. The entrypoint's pre-command verification (the prepared commit's objects are present and `/workspace` is a usable repository) fails the attempt before any command as `workspace_unverified`.
- **Loss and recovery.** Scratch is disposable. If it is lost or cannot be verified after any command began, the run fails with an explicit reason; execution never reclones and resumes later steps as though files survived. A rerun is a new run with new scratch from a pinned commit; durable recovery is the last published commit plus retained artifacts. Scratch never creates or acknowledges a checkpoint (WS-06).
- **Provenance.** The entrypoint reports workspace state at command begin; the server records it with produced artifacts so evidence is never labeled as built from an unchanged candidate when scratch differed ([DQ-16](delivery-and-quality-gates.md#requirements)).

### Retained outputs (ADR 0012)

The run workspace is shared working state, not an artifact version store. Generated assets, binaries and reports may flow between steps without commits or intermediate transfers. Declare selected files as artifacts for retention; package directories explicitly. A subsequent run fetches exact retained inputs through the [artifact contract](../contracts/artifacts-and-releases.md), rather than mounting an old run's volume. Source checkpoints still require confirmed Git publication. DQ-17/DQ-18 refine approval of generated outputs after the new contract cutover; old DQ-16 contexts keep their recorded meaning.

## Human work

An engineer clones the work branch, commits, and pushes a normal fast-forward update. Before admitting a writer or consuming a review, Kodama refreshes the remote tip and records authorized changes. A human push during an agent attempt wins the ref race; the agent's result remains an artifact/commit proposal for explicit reconciliation. No automatic merge or force-rewind is performed.

## Requirements

| ID | Requirement |
| --- | --- |
| WS-01 | Provisioning MUST reserve one workspace per actionable item before creating the branch. |
| WS-02 | Runs/readers MUST pin exact Git commits and retain their reachability. |
| WS-03 | Writer admission MUST atomically reserve `writer_attempt_id`. |
| WS-04 | Publication MUST use active-attempt validation and expected-tip CAS; stale attempts MUST NOT write the accepted branch. |
| WS-05 | `[attempt_private]` Attempts MUST use private checkouts. `[both models]` Read attempts MUST lack publication authority. `[run_shared]` private checkouts are replaced by WS-13/WS-14. |
| WS-06 | A checkpoint MUST be acknowledged only after remote Git publication is confirmed. Injected credentials MUST remain outside source commits. |
| WS-07 | Archive MUST reject new publication and settle current publication before returning; restore explicitly reopens the existing workspace. |
| WS-08 | Failure MUST report the last published commit and that uncommitted work may be lost. |
| WS-09 | Human synchronization and source/artifact access MUST enforce tenant/project authorization. |
| WS-10 | Scratch size, execution time, and artifact transfer MUST be bounded. |
| WS-11 | Unknown publication outcomes MUST be reconciled before writer reassignment; downstream readers MUST bind a specific successful writer commit (`run_shared`: the context's `headCommit` and lineage). |
| WS-12 | Retention/deletion MUST preserve referenced commits and audit requested removal. |
| WS-13 | `[run_shared]` A source-backed run MUST prepare its exact pinned commit once, before any user command, evidenced by a control-plane receipt; every executable step MUST see the same `/workspace`; branch movement MUST NOT refresh it. A source-free run MUST receive initially empty scratch (later steps see earlier steps' files; the scratch is never cleared between steps) and no checkout authority. |
| WS-14 | `[run_shared]` At most one executable attempt MAY hold a run's scratch; handoff MUST wait for verified stop and release of the prior holder. Retry, redrive, an expired claim or a partition MUST NOT give a possibly live holder's volume to another attempt. Logical-workspace writer admission stays separate. |
| WS-15 | `[run_shared]` Lost or unverifiable scratch MUST fail the run with an explicit reason; a rerun MUST get fresh resources, retained Git commits and artifacts MUST survive, and scratch MUST NOT create or acknowledge a checkpoint. |
| WS-16 | `[run_shared]` Cumulative writers MUST publish through expected-tip CAS with ancestry validation; a human push MUST conflict rather than be overwritten; earlier steps' edits and secret disclosures MUST be accounted for in lineage and publication/output checks. |

## Interface and acceptance

Expose provision/read/archive/restore and authorized source synchronization through [HTTP](../contracts/http-api.md). Publication is an internal attempt interface in the [entrypoint contract](../implementation/step-entrypoint.md).

| ID | Scenario | Requirements |
| --- | --- | --- |
| WS-A01 | Duplicate provisioning creates one branch/workspace at a recorded commit | WS-01, WS-02 |
| WS-A02 | Competing writers yield one reservation | WS-03 |
| WS-A03 | Revoked/partitioned attempt cannot publish after reassignment | WS-04, WS-05 |
| WS-A04 | Readers keep their source commit while the work branch advances | WS-02, WS-05, WS-11 |
| WS-A05 | An uncertain Git push is reconciled and credentials never enter its bundle | WS-06, WS-11 |
| WS-A06 | Recovery starts from the published commit and reports ephemeral loss | WS-08 |
| WS-A07 | Human push races cause CAS conflict; cross-tenant and oversized access fail | WS-04, WS-09, WS-10 |
| WS-A08 | Archive/restore/deletion preserve retained source and audit | WS-07, WS-12 |
| WS-A09 | One pinned checkout is prepared once; steps with different images see generated, untracked and ignored files; branch movement does not refresh scratch; a source-free run has initially empty scratch that later steps still see written by earlier ones (never cleared between steps) and no checkout credential | WS-13 |
| WS-A10 | Eligible steps cannot overlap scratch ownership; retry, redrive, expired claims and partition cannot hand a possibly live holder's volume to another attempt | WS-14 |
| WS-A11 | Lost scratch fails the run; a rerun gets fresh resources; retained Git commits and artifacts survive; scratch writes never create a checkpoint | WS-08, WS-15 |
| WS-A12 | Cumulative writers use expected-tip CAS; a human push conflicts; prior-step edits and secret disclosures are recorded in lineage and caught by publication/output checks | WS-04, WS-11, WS-16 |
