// Package recovery implements the thin installation FencePort, BackupPort and
// RestorePort drivers.
//
// Every effect belongs to an injected seam: the Libpod client for fencing, the
// Executor for pg_dump/pg_restore and the Files boundary for host artifacts.
// The database dump keeps object ownership and table/column ACLs, so a restore
// requires the target's application roles to already exist: `kodama migrate`
// creates them, and a missing role is a typed refusal before pg_restore. The
// drivers persist backup intent as a manifest written last, never collapse an
// unknown effect into "never started" or "stopped", never replay an uncertain
// effect, and never capture disposable workspaces. Restore binds exactly one
// preflighted identity, consumes that binding before any effect and never
// implements dispatch.
package recovery
