package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
)

// Git grants of the attempts of a source-set run (GR-06, seam S5).
//
// Execution asks the Git authority for the grant of an attempt after the attempt
// is committed (a grant is issued only to a live attempt) and before its workload
// is started, and revokes it in the transaction that ends the attempt's
// authority (cancellation, failure, timeout, completion, supersession). The
// request is a network call and happens outside every transaction; the
// revocation is a statement of the caller's own transaction (see the stores'
// GitGrants peer), so a cancelled attempt can never push after the commit. A
// receive that already obtained commit intent completes, any later one is
// refused: the push authorizer re-checks the attempt's liveness at every push.

// GitSecret is the opaque Git credential of a grant. It is delivered to the
// runtime adapter once, never logged, formatted or stored: every formatter and
// every encoder prints a redaction.
type GitSecret struct{ value string }

// NewGitSecret wraps a credential.
func NewGitSecret(v string) GitSecret { return GitSecret{value: v} }

// Reveal returns the credential for delivery.
func (s GitSecret) Reveal() string { return s.value }

// IsZero reports an absent credential.
func (s GitSecret) IsZero() bool { return s.value == "" }

func (GitSecret) String() string               { return "[redacted]" }
func (GitSecret) GoString() string             { return "[redacted]" }
func (GitSecret) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }
func (GitSecret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func (s GitSecret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte("[redacted]")) }
func (GitSecret) LogValue() slog.Value         { return slog.StringValue("[redacted]") }

// AttemptGrantRequest asks the Git authority for the grant of one attempt (or the
// read-only grant of a preparation workload, or the seed grant of a provisioning
// attempt): the actor, the repositories and the update patterns the run's access
// allows, and an expiry bounded by the attempt's own deadline.
type AttemptGrantRequest struct {
	Kind         gitv1.GrantKind // preparation, attempt or seed
	Actor        gitv1.Actor
	Repositories []gitv1.GrantRepository
	ExpiresAt    time.Time
}

// GitGrantHandle is an issued grant with its credential, for delivery only.
type GitGrantHandle struct {
	GrantID    string
	Credential GitSecret
	ExpiresAt  time.Time
}

// GitGrants is what execution asks of the Git authority for a run's workloads
// (provider gitauth, T-136). Revocation is not a method here: it joins the
// transaction that ends the attempt (the stores' GitGrants peer, gitauth's
// RevokeAttemptIn).
type GitGrants interface {
	// IssueAttempt issues the grant. It is idempotent on (kind, attempt or
	// workload): a retry returns the same grant with a fresh credential that
	// REPLACES the earlier one, so execution never calls it for an attempt whose
	// workload already exists (see requestAttemptGrant). An attempt that is no
	// longer live is refused (attempt_inactive). The credential is never logged
	// or stored.
	//
	// The adapter reads the attempt's lease outside the grant's own transaction
	// (gitauth.Attempts.Lease), so a cancellation that commits in between can
	// leave a live grant for an ended attempt; execution closes that window by
	// re-reading the attempt after issuing and calling RevokeAttempt. The lease
	// the adapter reports must therefore last at least as long as the grant:
	// AttemptGrantFor caps the grant's expiry at the run's deadline, so the
	// lease deadline must not be earlier than the run's.
	IssueAttempt(ctx context.Context, scope domain.Scope, req AttemptGrantRequest) (GitGrantHandle, error)
	// RevokeAttempt revokes every grant of the attempt in its own transaction. It
	// is idempotent. Execution uses it only to close the issuance race above;
	// the revocation that belongs to a transition joins the transition's
	// transaction through the stores' GitGrants peer.
	RevokeAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) error
}

// AttemptGit is the Git grant delivered with an attempt's start: the reference
// the pinned context carries, the issued grant and its credential. The runtime
// adapter writes the credential to the context's credential file with the
// workload's permissions only, outside /workspace, and never into the context,
// an environment variable or a log.
type AttemptGit struct {
	GrantRef   string
	GrantID    string
	Credential GitSecret
	ExpiresAt  time.Time
}

// AttemptGrantFor derives the grant request of an attempt from its pinned
// context: exactly the repositories and ref patterns the context records (the
// run's agent namespace and, for the writer, the write branches), read access
// everywhere, bounded by the run's deadline and the contract's workload lifetime.
// A context without a static grant (a source-free provisioning run) has none.
func AttemptGrantFor(c runtimev3.ContextV3, now time.Time) (AttemptGrantRequest, bool, error) {
	if c.Git == nil || c.Git.GrantID == nil {
		return AttemptGrantRequest{}, false, nil
	}
	deadline, err := c.Run.DeadlineAt.Time()
	if err != nil {
		return AttemptGrantRequest{}, false, invalidf("the run deadline is unreadable")
	}
	expires := now.UTC().Add(gitv1.MaxWorkloadGrantLifetime)
	if deadline.Before(expires) {
		expires = deadline
	}
	if !expires.After(now) {
		return AttemptGrantRequest{}, false, fmt.Errorf("%w: the run deadline has passed", ErrAttemptRevoked)
	}
	run, attempt := string(c.RunID), string(c.AttemptID)
	return AttemptGrantRequest{
		Kind:         gitv1.GrantAttempt,
		Actor:        gitv1.Actor{Type: gitv1.ActorWorkload, ID: attempt, RunID: &run, AttemptID: &attempt},
		Repositories: c.Git.GrantRepositories(),
		ExpiresAt:    expires.UTC().Truncate(time.Second),
	}, true, nil
}

// requestAttemptGrant issues the Git grant of a context v3 attempt just before
// its workload is created (first launch only). A composition without the Git
// authority refuses to start the attempt: it is never started without the
// credential its context promises.
//
// Issuing again replaces the credential of the earlier issue, so it is never
// done for an attempt whose workload already exists: when a Start succeeded but
// its confirmation was lost, the redrive observes the workload first and, finding
// it, adopts it without issuing (spec.Git stays nil: adopting never needs a new
// credential, see Runtime.Start). Only when the runtime affirms that it has no
// workload for the attempt (ErrNotFound) is a grant issued.
//
// A cancellation can commit between the authority's lease read and its grant
// insert, leaving a live grant for an ended attempt. The attempt is therefore
// read again after issuing; if its authority ended, the grant is revoked through
// the port and nothing is started (ErrAttemptRevoked). The push authorizer still
// refuses every push of an ended attempt; this removes the dangling grant.
func (s *Service) requestAttemptGrant(ctx context.Context, rt Runtime, spec AttemptSpec) (*AttemptGit, error) {
	if !isContextV3(spec.Context) {
		return nil, nil
	}
	c, err := spec.DecodeContextV3()
	if err != nil {
		return nil, err
	}
	req, needed, err := AttemptGrantFor(c, s.now())
	if err != nil || !needed {
		return nil, err
	}
	if s.d.GitGrants == nil {
		return nil, fmt.Errorf("%w: the Git authority is required for a source-set run", ErrNotConfigured)
	}
	switch _, oerr := rt.Observe(ctx, spec.Workload("")); {
	case oerr == nil:
		return nil, nil // the workload exists: adopt it with the credential it holds
	case errors.Is(oerr, ErrNotFound):
	default:
		return nil, Unavailablef(oerr, "the runtime could not say whether the workload exists")
	}
	h, err := s.d.GitGrants.IssueAttempt(ctx, spec.Scope, req)
	if err != nil {
		return nil, err
	}
	if h.GrantID == "" || h.Credential.IsZero() {
		return nil, Unavailablef(nil, "the Git authority returned no grant")
	}
	view, err := s.d.Store.Get(ctx, spec.Scope, spec.RunID)
	if err != nil {
		return nil, Unavailablef(err, "the attempt could not be read after its grant was issued")
	}
	if at := view.Attempt(spec.AttemptID); at == nil || at.Status != AttemptRunning || !view.Control(spec.AttemptID).Live() {
		if rerr := s.d.GitGrants.RevokeAttempt(ctx, spec.Scope, spec.AttemptID); rerr != nil {
			return nil, Unavailablef(rerr, "the grant of an ended attempt could not be revoked")
		}
		return nil, fmt.Errorf("%w: the attempt ended while its grant was issued", ErrAttemptRevoked)
	}
	return &AttemptGit{GrantRef: *c.Git.GrantID, GrantID: h.GrantID, Credential: h.Credential, ExpiresAt: h.ExpiresAt}, nil
}

// PreparationGrantFor derives the read-only grant request of a run's one
// preparation workload: every repository of the source set, no update pattern
// anywhere. The grant is keyed by the preparation's workload identity (the
// resource key of the run), expires with the preparation's deadline and is
// revoked when the preparation ends.
func PreparationGrantFor(run *Run, workload string, deadline, now time.Time) (AttemptGrantRequest, error) {
	if run.Sources == nil || run.Sources.Empty() {
		return AttemptGrantRequest{}, invalidf("a source-free run has no preparation")
	}
	expires := now.UTC().Add(gitv1.MaxWorkloadGrantLifetime)
	if deadline.Before(expires) {
		expires = deadline
	}
	if !expires.After(now) {
		return AttemptGrantRequest{}, fmt.Errorf("%w: the preparation deadline has passed", ErrResourceRevoked)
	}
	id := string(run.ID)
	repos := make([]gitv1.GrantRepository, 0, len(run.Sources.Entries))
	for _, repo := range run.Sources.RepositoryIDs() {
		repos = append(repos, gitv1.GrantRepository{ID: repo, Read: true, Update: []string{}})
	}
	return AttemptGrantRequest{
		Kind: gitv1.GrantPreparation, Actor: gitv1.Actor{Type: gitv1.ActorWorkload, ID: workload, RunID: &id},
		Repositories: repos, ExpiresAt: expires.UTC().Truncate(time.Second),
	}, nil
}

func isContextV3(raw []byte) bool {
	var v struct {
		ContextVersion int64 `json:"contextVersion"`
	}
	return json.Unmarshal(raw, &v) == nil && v.ContextVersion == SourceContextVersion
}
