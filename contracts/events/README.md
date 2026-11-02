# Event contract v1

The frozen wire contract of Kodama domain events, delivered by T-005 for the [event catalog](../../docs/contracts/event-catalog.md). The catalog owns the semantics (publication, ordering, retention); this directory owns the exact shapes.

| Path | Contents |
| --- | --- |
| [`v1/schema.json`](v1/schema.json) | JSON Schema 2020-12: envelope plus one `data.<type>` definition per event type |
| `v1/*.go` | `eventsv1` package: `Envelope`, the typed payloads (the catalog's 16, `run_resource.state_changed` and the three Git service events), `Decode`/`Encode`, `Validate`, `Canonical`/`Digest`, `DecodeData`, `Types`/`Spec` |
| [`v1/fixtures/cases.json`](v1/fixtures/cases.json) | Positive and negative cases with the expected schema and Go verdicts |
| [`v1/fixtures/examples.json`](v1/fixtures/examples.json) | One valid envelope per event type |

## Rules consumers rely on

- **Envelope.** `id`, `schemaVersion` (1), `type`, `occurredAt` (UTC, literal `Z`), `tenantId`, `resource {type,id,version}`, `actor {type,id}`, `correlationId`, `causationId` (always present; `null` when none) and `data`. `projectId` is **omitted** for tenant-wide events and never empty. Only `membership.changed` and `configuration.changed` may be tenant-wide.
- **Actors.** `user`, `service`, `workload`, `operator`.
- **Aggregate binding.** `resource.type` is fixed per event type: `work_item`, `workspace`, `run`, `step_attempt`, `gate`, `deployment`, `membership`, `project`, `configuration`, `usage_record`, `run_resource` and `repository`. Several event types may describe one resource version.
- **Strict parsing, tolerant additions.** `Decode` rejects input over 64 KiB (`data` over 32 KiB), invalid UTF-8, duplicate keys at any depth, nesting over 16, trailing data and non-integer or out-of-range (beyond ±(2^53−1)) numbers. Unknown additional members are compatible optional additions and are ignored. Member names match exactly and case-sensitively in the envelope, its nested objects and every typed payload (as in the schema): `ID`, `Kind` or `TenantID` is an ignored addition that can neither override `id`/`kind`/`tenantId` nor satisfy a required member.
- **Unsupported is not success.** An unknown `schemaVersion` yields `ErrUnsupportedVersion`, an unknown `type` yields `ErrUnsupportedType`; both are reported before any other field, because a newer contract may change everything else. Consumers quarantine them.
- **Required catalog fields.** Every field the [catalog](../../docs/contracts/event-catalog.md) lists as required is required and non-null in `data`, including the classified `reason` of `run.state_changed` and `step_attempt.state_changed` and the `deadline` of `gate.requested`; omission or `null` is invalid in both the Go validation and the schema. Only additions beyond the catalog are optional.
- **Optional members are non-null.** An optional member (`contentRevision`, `supersedingCommit`, `supersedingRevision`, `sourceCommit`, `artifactIds`, `imageDigest`, `inputTokens`, `outputTokens`, `publishingAttemptId`, `humanSynchronization`) is either omitted or carries a value of its type; an explicit `null` is invalid. Only `previousCommit` of `workspace.checkpoint_created` is nullable. A checkpoint names exactly one origin: `publishingAttemptId`, or `humanSynchronization` as the literal `true`; both members together, or `humanSynchronization` as `false`/`null`, are invalid. Go validation checks member presence on the raw payload, and `TestMemberPresenceAgreesWithSchema` mutates every member of every example (null, false, empty, removed) to keep the two verdicts equal.
- **References only.** Payloads carry IDs, digests, commits and codes, never secret values, source files or transcripts. `configuration.changed` carries versions, not values.
- **Git service events** (additive, T-126): `repository.created` (`name`, `defaultBranch`, `hosting`; committed when the repository becomes ready after initialization, not at reservation), `repository.state_changed` (`from`, `to`, `reason`; only provisioning→ready, ready→archived and archived→ready) and `git.refs_updated` bind to resource types `repository` and `git_push`, require the project, and share the repository name, branch name and ref syntax of [`contracts/git/v1`](../git/v1/). `git.refs_updated` (T-168) carries the applied updates **inline**: `receiveId`, `repositoryId`, `actor` of type `user` or `workload`, `originRunId` (an explicit `null` for a person's push, the run for a workload's) and `updates` and an optional `journalPosition` (the receive's worker-journal position, which orders pushes as applied), `updates` being 1 to 1024 entries of `ref`, `old`, `new` and `kind` (`create`, `fast_forward`, `rewrite` or `delete`). A kind agrees with its values (the all-zero object ID is a ref that did not exist or no longer does), a ref appears once, `old` differs from `new`, and a server ref (`refs/kodama/…`) is never carried; the schema states the kind rules and the Go contract also checks uniqueness. The event carries references only. The envelope `resource.id` equals `receiveId` (checked by the Go contract; the schema cannot compute it): the push is its receive. A very large push exceeds the envelope's data bound, so a producer that keeps the payload outside an envelope (the kernel's outbox rows) validates it with `DecodeGitRefsUpdated`.
- **Content identity.** `Digest` is SHA-256 over the canonical encoding (sorted keys, no whitespace). Appending the same event ID with a different digest is a conflict.

The wire spelling of every payload field is proposed here, because the catalog lists semantic requirements, not member names. Producer modules confirm their payloads when they adopt the contract; additions inside version 1 must be optional. A breaking meaning or type change requires `schemaVersion` 2 and a documented migration.

## Checks

```sh
go test -race -count=1 ./contracts/events/...
```

No external services are required.
