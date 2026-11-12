// Package workspace_test pins the activation materialization API.
//
// Package kodama/internal/shell/workspace provides:
//
//	// MaterializationConfig is the activation-time configuration for one
//	// snapshot. Scope must be a valid ownership scope. Secrets may be nil only
//	// when the request declares no secret names.
//	type MaterializationConfig struct {
//		Scope   tenancy.Scope
//		Secrets secret.Resolver
//	}
//
//	// BuildMaterialization resolves the request's declared secrets at
//	// activation and returns one complete Materialization. Files carries the
//	// admitted inputs, the earlier succeeded outputs and the optional task
//	// snapshot; Secrets carries the resolved read-only secret values;
//	// Credential carries the sole mutable mounted credential. Any failure
//	// returns the zero Materialization and an error, never a partial snapshot.
//	func BuildMaterialization(context.Context, engine.PrepareRequest, MaterializationConfig) (Materialization, error)
//
//	// Environment merges the already action-precedence-merged literal
//	// container environment with the reserved execution context in the order
//	// KODAMA_URL, KODAMA_EXECUTION, KODAMA_ACTION, then KODAMA_TASK only when
//	// the request has a task id. It never emits the credential or a secret
//	// value and refuses a literal environment name that shadows reserved
//	// context.
//	func Environment(engine.PrepareRequest, string) ([]string, error)
//
// Behavior these tests rely on:
//
//   - File bytes are exact: no newline is appended and no string is
//     reinterpreted. Inputs materialize at /kodama/inputs/<key>, earlier
//     succeeded outputs at /kodama/outputs/<action>/<key>, and the task
//     snapshot at /kodama/task.json only when a task snapshot exists.
//   - Every File has an explicit regular, read-only mode. Secret values are
//     returned in Materialization.Secrets (never as ordinary files) and the
//     credential is returned in Materialization.Credential, so
//     /kodama/secrets/KODAMA_TOKEN is never an immutable File.
//   - The returned snapshot is deep-copied: mutating the request, the
//     resolver's backing store or a later BuildMaterialization call cannot
//     alter an already built Materialization.
//   - Declared names are resolved once, in declared order, through the
//     configured scope. Missing, extra, duplicate, oversized, reserved or
//     shadowing names and a resolver failure return the zero Materialization
//     and an error.
//   - A task snapshot and a task id must be present together; a task-free
//     request has neither.
package workspace_test

import (
	"github.com/optimiware/kodama/internal/shell/workspace"
)

// Compile-time pins for the declarations the behavior tests consume.
var (
	_ = workspace.BuildMaterialization
	_ = workspace.Environment
	_ workspace.MaterializationConfig
)
