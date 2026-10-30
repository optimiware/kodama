// Package gitpush is the control-plane half of the internal Git service's push
// journal (docs/specs/git-repositories.md, "Access and ref policy" and "Push
// journal and events"; GR-07, GR-09 to GR-11, GR-18, GR-19; seams S4 and S8 of
// docs/implementation/internal-git-service.md): every ref update is authorized
// against the grant, the ref policy matrix and current authority before Git
// applies it, every receive is journaled as a push record with one entry per
// ref, and an applied record publishes exactly one reference-only
// git.refs_updated event.
//
//   - Authorize checks each update (Evaluate, the pure ref policy matrix),
//     consumes the seed grant of a seed receive in the same transaction as the
//     record (gitauth.ConsumeSeedIn through Tx.Grants) and creates the record
//     before the response; the worker's request carries the Git facts, so no Git
//     or worker call is made with a SQL lock held.
//   - CommitIntent moves entries from authorized to committing only while the
//     record is unsettled, the attempt is live and the deadline has not passed.
//   - Outcome marks the reported updates applied, refuses the entries that never
//     reached committing and commits audit and the one event with the entries.
//   - Reconcile settles what remains with gitv1.Settle over the worker's
//     evidence (receive liveness, the ref journal, the value under the worker's
//     lock, the highest journal head seen): never refs, reflog or Pod deletion
//     alone; unknown stays unknown and blocks writer reassignment.
//
// The package holds the Record aggregate and its transitions, the policy
// matrix, the use cases (Service) and the small consumer-owned ports they
// need (Store, Repositories, Grants, Attempts, Reservations, Worker,
// ReadAuthorizer). It has no pgx, HTTP or process dependency: PostgreSQL
// (postgres/), memory and test support (gitpushtest/) and HTTP (http/) are
// adapters wired by the composition root (T-132). Wire shapes, validation and
// codes are contracts/git/v1's and are never redefined here.
package gitpush
