package git

// The independent Git service implementation is split across a few files:
//
//   - service.go: the tenant-bound Service, read authorization, the serialized
//     receive sequence (Prepare, prepareBatch, Apply, Report, Update, Recover),
//     canonical `git.ref.update` verification, approval enforcement and journal
//     intent.
//   - storage.go: tenant-scoped repository path validation and provisioning.
//   - journal.go: the durable, atomically replaced FileJournal.
//   - retention.go: authorized retained-commit discovery and the backup
//     manifest.
//   - smarthttp.go: the smart-HTTP front handler (actor authentication, local
//     read/write-at-all rules, bounded streaming through the injected
//     GitBackend, sanitized refusals and header allow-lists). The handler mints
//     a unique per-request push identity and places it in the backend
//     environment as KODAMA_SOURCE_EVENT, so an identical repeated push never
//     reuses an earlier journal entry.
//   - hook.go: the native `update`/`pre-receive`/`post-receive` authority
//     adapter (strict object-id/ref parsing, exact Service.Update verification
//     for one ref, sanitized exit codes).
//
// The contract declarations live in types.go, ports.go,
// contract_rules.go and contract_journal.go. This package starts no process
// itself: native Git commands go through the injected CommandExecutor and the
// smart-HTTP backend, and no credential, factory database or tenant key is ever
// persisted. The HTTP handler gives the backend only an opaque actor handle;
// the hook resolves it through the injected ActorSource.
//
// Batch atomicity: the `pre-receive` hook verifies each ref update and
// journals a durable intent for every ref it accepts, but it takes no ref
// effect and sends no report. A refusal of any ref rejects the whole batch
// before the native transaction; the native receive-pack ref transaction is
// the atomic all-or-nothing boundary, and the hook's exit 0 only lets that
// transaction proceed. Acceptance and `git.ref.updated` reporting follow from
// the `post-receive` adapter, which reconciles each intent from observed ref
// state through Service.Recover. A refused batch therefore moves no ref, sends
// no report and leaves no `reported` entry. A redelivery of the same push
// (same KODAMA_SOURCE_EVENT) settles its existing entry from observation only
// and never applies the stored compare-and-swap from stale verification.
//
// Ref deletes are refused. The journal has no durable candidate
// representation for a delete, so the hook refuses it before any effect even
// though `RW+` includes delete permission in the rule evaluator. Accepting
// deletes needs a journal format change; it is not silently approximated with
// a zero object ID.
