// Package snapshotdoc is the single authoritative codec of the
// executions.snapshot document and of the deterministic identities derived from
// one execution identity.
//
// Both start paths that create an executions row use this package: the
// task-free execution start in the execution store and the task/trigger start
// in the work store. They therefore write one document shape and derive one set
// of row identities, so any started execution round-trips through the Kernel
// engine's Load regardless of which adapter started it.
//
// # Document shape
//
// The document is UTF-8 JSON. Every admitted byte sequence is carried as a JSON
// byte array, which encoding/json renders as base64, so arbitrary bytes
// round-trip exactly:
//
//	{
//	  "version": 1,
//	  "yaml": "<base64 exact admitted pipeline YAML bytes>",
//	  "snapshot": { ...pipeline.Snapshot public fields... },
//	  "actions": [ ...pipeline.AdmittedAction, in admitted index order... ],
//	  "pipeline_id": "pipe-...",
//	  "pipeline_version": 3
//	}
//
// The nested snapshot carries the SHA-256 digest of the exact YAML bytes
// (yaml_sha256), the admitted input values, the admitted Env defaults, the
// pinned images, secret names, runtime requirements, task id, the admitted task
// conversation bytes, references and metadata. The top-level yaml is required
// because pipeline.Snapshot keeps its exact YAML bytes unexported; Decode
// rebuilds the immutable snapshot through pipeline.NewSnapshot and refuses a
// document whose rebuilt digest differs from the stored digest.
//
// Conversation presence is durable: an absent conversation is JSON null and a
// present empty conversation is JSON "". The same distinction is preserved by
// pipeline.Snapshot and by the Kernel definition snapshot.
//
// # Admitted actions
//
// actions carries the full admitted container or request definition. The
// schema has no action-definition columns: actions rows store only the
// projection returned by ActionRows (identity, execution, index, name, kind,
// state, version, timeout_ms, staged_outputs_set and launch_state), while the
// definitions live in this document. Load pairs each actions row with the
// admitted action at the same index and refuses a row whose name or kind does
// not match.
//
// One admitted fact is additionally projected because the schema has a
// dedicated column for it and every reader expects it: a request action's
// immutable assignment is written to actions.request_assignment by the same
// insert, through RequestAssignment. The Kernel reconciler activates a request
// by committing a flow.Commit through its state port, not through the Shell
// activation port, so an activation-only write would leave an engine-activated
// request row with a NULL assignment and make the approval evidence reader fail
// the whole read. Assignment never changes after admission, so the insert value
// is authoritative; the Kernel activation commit rewrites the same encoding
// beside the resolved subject. A container action has no assignment and keeps
// the column NULL.
//
// # Derived identities
//
// DeriveActionID(executionID, index) is the durable actions.id:
//
//	"act-" + lowercase hex(SHA-256(executionID || 0x00 || "action" || 0x00 || decimal index))[:16]
//
// It is stable across restarts, distinct per index, and independent of any
// caller-supplied action identity (StartUnit carries none).
//
// DeriveWorkspaceID(executionID) is the durable executions.workspace_id:
//
//	"ws-" + lowercase hex(SHA-256(executionID || 0x00 || "workspace"))[:16]
//
// Start persists it for every execution, so Load returns a non-empty stable
// workspace and a container action has its retained workspace identity. A
// planned wait (a request action followed by a later container action) keeps the
// same execution, so it keeps the same workspace. The derivation is a function
// of the execution identity only and cannot collide across executions in
// practice.
package snapshotdoc
