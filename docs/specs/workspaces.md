# Workspaces

**Scope:** M1, Git-backed source continuity. Human editor access is ordinary Git clone/commit/push.

## Model

A workspace stores ownership, work-item ID, repository URL, separate checkout/publisher secret references, work branch, accepted commit, `writer_attempt_id`, publication fields, state, and `last_error`. Checkout authority is read-only; publisher authority stays in the control plane. States are `provisioning`, `available`, and `archived`; errors are reasons on the resource. Archive/restore change availability without deleting source history.

The default branch is `refs/heads/kodama/work/<work-item-id>`. A normal branch is portable across common Git hosts, unlike assuming support for arbitrary `refs/kodama/*`. A checkpoint is a commit reachable through retained Git refs. No PVC snapshots, workspace sessions, or blob checkpoint manifests are required.

Each attempt clones its pinned commit into private scratch storage. `read` means the attempt cannot publish source; its disposable checkout may still be modified by build tools. `write` grants the sole publication slot. Uncommitted/ignored files are ephemeral; recovery starts from the last published commit. Users must commit source they want retained.

## Publication protocol

1. Reserve `writer_attempt_id` in PostgreSQL. Remember the expected branch tip and input commit.
2. Run the attempt against its own checkout. The runtime has read-only Git access; it does not receive canonical-branch write credentials.
3. The entrypoint submits a bounded Git bundle through the artifact transport and proposes its commit to the control-plane publisher. This temporary transport object is not a durable checkpoint format.
4. The publisher validates ownership, the live writer/attempt, commit ancestry, and expected tip. It records `publish_expected_commit` and `publish_commit` on the workspace before pushing.
5. Publish with an explicit expected-old-ref compare-and-swap and fast-forward ancestry check. Keep the publication slot until the Git result is known. A concurrent human push causes a conflict, not an overwrite.
6. Record the accepted commit, audit/event, and release the writer after result/workload settlement. Retain per-run Git refs so later branch movement cannot make pinned commits unreachable.

Only the publisher holds branch-write credentials. There is one in-flight publisher per workspace. A failed/unknown push is resolved by reading the remote ref before admitting another writer. Reassignment after a node failure revokes the old attempt's publication authority; private scratch copies prevent filesystem overlap.

A publication authorized before cancellation may still finish remotely. Reconciliation records that effect without making a cancelled attempt successful. An unchanged ref after a timeout alone does not prove an in-flight request can never complete: retain uncertain publication ownership until resolved, or reconcile explicitly before retry/reassignment. New accepted source always uses the observed expected tip and validated provenance.

Kubernetes deletion is a cleanup observation, not proof that a partitioned process stopped. CAS publication protects accepted source; external tool/deployment side effects still need their own uncertainty handling. During restore, fence old control-plane publishers and rotate their Git credentials before resuming publication.

## Human work

An engineer clones the work branch, commits, and pushes a normal fast-forward update. Before admitting a writer or consuming a review, Kodama refreshes the remote tip and records authorized changes. A human push during an agent attempt wins the ref race; the agent's result remains an artifact/commit proposal for explicit reconciliation. No automatic merge or force-rewind is performed.

## Requirements

| ID | Requirement |
| --- | --- |
| WS-01 | Provisioning MUST reserve one workspace per actionable item before creating the branch. |
| WS-02 | Runs/readers MUST pin exact Git commits and retain their reachability. |
| WS-03 | Writer admission MUST atomically reserve `writer_attempt_id`. |
| WS-04 | Publication MUST use active-attempt validation and expected-tip CAS; stale attempts MUST NOT write the accepted branch. |
| WS-05 | Attempts MUST use private checkouts; read attempts MUST lack publication authority. |
| WS-06 | A checkpoint MUST be acknowledged only after remote Git publication is confirmed. Injected credentials MUST remain outside source commits. |
| WS-07 | Archive MUST reject new publication and settle current publication before returning; restore explicitly reopens the existing workspace. |
| WS-08 | Failure MUST report the last published commit and that uncommitted work may be lost. |
| WS-09 | Human synchronization and source/artifact access MUST enforce tenant/project authorization. |
| WS-10 | Scratch size, execution time, and artifact transfer MUST be bounded. |
| WS-11 | Unknown publication outcomes MUST be reconciled before writer reassignment; downstream readers MUST bind a specific successful writer commit. |
| WS-12 | Retention/deletion MUST preserve referenced commits and audit requested removal. |

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
