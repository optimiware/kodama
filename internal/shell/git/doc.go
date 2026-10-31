// Package git is the shell's Git integration authority (C4 of the pipeline-runtime
// simplification plan; GR-21 and GR-22 of docs/specs/git-repositories.md).
//
// The decision to let a run advance a repository's protected default branch is
// made here, from durable run evidence only: the run record the kernel keeps
// (its plan, its step states and its waits) and the repository's configured
// integration pipeline. Nothing else is an input. A task's status or
// resolution, a bare event such as wait.resolved, and a value a container
// writes (approved=true) have no field in Request or in the facts a decision
// reads, so none of them can authorize a push.
//
// Authority is issued only when every condition holds:
//
//   - the run's pipeline is the repository's configured integration pipeline;
//   - the requested step is a protected (authority: integrate) container step
//     that is running now, so an earlier step, a settled step and a step that
//     never became eligible are refused;
//   - every wait the step requires has succeeded with the wait's approving
//     outcome (not a rejection, another declared outcome, expiry or
//     cancellation) and recorded a candidate correlation equal to the commit to
//     be pushed;
//   - the ref is the repository's protected default branch;
//   - the expected old commit is the ref's current commit in the worker and the
//     new candidate is a fast-forward of it.
//
// An Authority names exactly one ref, one expected old commit and one new
// commit, in one run, step, repository and tenant scope. It permits that update
// only: another candidate, another old value or another ref is refused. It is a
// receipt, not a cached decision: Authorizer.Recheck reads the run, the policy
// and the pipeline again at every use, so authority ends when the step settles,
// the run is cancelled or settled, or the ref moves; replaying a push that has
// landed fails the expected-old check.
//
// The native worker keeps its own current-authority and expected-old-value
// checks (internal/gitservice); this package decides what the control plane
// tells it.
package git
