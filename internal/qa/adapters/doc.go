// Package adapters implements the consumer-owned ports of internal/qa over the
// execution, delivery, artifact and work modules.
//
//   - Attempts reads an attempt's trusted facts (owner, verified pinned context,
//     accepted result) from the execution store.
//   - Deployments reads the deployment module's recorded facts.
//   - Artifacts reads artifact metadata and verified bytes and pins evidence.
//   - Work files defects through an ordinary work.Service acting as one
//     configured mapping principal under the work module's own authority
//     (injected, identity-backed), narrowed to read, create and evidence-link
//     edits: no transition, completion or assignment is reachable through it. The
//     authority is checked before any read or write. WorkGuards binds the same
//     narrowing, the owner's transaction-bound permission check and the
//     artifact-backed evidence check to the work store's mutating transactions.
//     It also compares a defect's immutable creation record with the finding
//     payload before reusing the identity.
//
// Nothing here opens a transport or grants a permission beyond that narrowing;
// the composition root (T-072) wires it.
package adapters

import "time"

func nowUTC() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }
