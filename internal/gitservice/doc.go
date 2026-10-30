// Package gitservice is the tenant Git worker around native Git (ADR 0011,
// docs/specs/git-repositories.md): the package behind `kodama git serve`.
//
// One Service serves one tenant's bare repositories from one data root. It owns
//
//   - the smart-HTTP endpoint (Service.GitHandler): default-deny routing of the
//     three smart endpoints, forwarding-assertion verification, repository paths
//     derived from the repository ID only, a sanitized environment, a bounded
//     on-disk request spool (Git sends large pushes chunked, which net/http/cgi
//     refuses), bounded duration and concurrency and streamed responses around
//     `git http-backend`;
//   - the hooks (RunHook, HookMain, InstallHooks): `pre-receive` authorizes the
//     intended updates, `reference-transaction` is the two-phase fence that
//     appends a receive-attributed, fsynced entry to the repository's ref
//     journal with the ref's observed value while Git holds the ref locks and
//     only then asks the control plane for commit intent, `post-receive`
//     confirms the applied refs. The hooks run as `kodama git-hook <name>` and
//     reach the control plane only through the worker over a loopback listener
//     with a per-receive token;
//   - the ref journal (append-only, per repository, flock-serialized, fsynced);
//   - the per-repository mutation gate shared by receives, server-side ref
//     operations, maintenance and the locked reconciliation read;
//   - the process-group termination of a receive at its deadline;
//   - the worker RPC (Service.RPCHandler) the control plane calls for
//     repository.Hosts: initialize, refs, journal, receive liveness, retained
//     refs, managed branches and maintenance.
//
// The worker re-reports what the control plane may have missed (reconcile.go): on
// the reconcile assignment it re-sends every receive of its journal after the
// acknowledged sequence, and a live outcome carries a journal sequence only when
// nothing earlier is unreported (the kernel keeps no push-record table; see
// docs/specs/git-repositories.md, "Retention, maintenance and recovery").
//
// Signature verification of forwarding assertions and of the control plane's
// identity belongs to gitauth (T-136): this package depends on the small
// AssertionVerifier and ControlCallerVerifier interfaces and runs gitv1's
// claim checks itself. Wiring into the kodama binary (the `git serve` and
// `git-hook` commands) is T-132: ServeCommand and HookMain are the entry points.
package gitservice
