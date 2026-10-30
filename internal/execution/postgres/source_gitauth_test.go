package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/gitauth"
	"github.com/optimiware/kodama/internal/gitauth/gitauthtest"
	gitauthpg "github.com/optimiware/kodama/internal/gitauth/postgres"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/storage"
)

// gitGrants is the execution.GitGrants of this test: the real gitauth service over
// the real git_grants table, the adapter T-150 will write (type conversion only).
type gitGrants struct{ svc *gitauth.Service }

func (g *gitGrants) IssueAttempt(ctx context.Context, scope domain.Scope, req execution.AttemptGrantRequest) (execution.GitGrantHandle, error) {
	h, err := g.svc.IssueAttempt(ctx, scope, gitauth.AttemptGrantRequest{Kind: req.Kind, Actor: req.Actor, Repositories: req.Repositories, ExpiresAt: req.ExpiresAt})
	if err != nil {
		return execution.GitGrantHandle{}, err
	}
	return execution.GitGrantHandle{GrantID: h.GrantID, Credential: execution.NewGitSecret(h.Credential.Reveal()), ExpiresAt: h.ExpiresAt}, nil
}

func (g *gitGrants) RevokeAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) error {
	return g.svc.RevokeAttempt(ctx, scope, attempt)
}

// leases is gitauth's view of execution: an attempt is live while it is running
// and its authority is not revoked.
type leases struct{ store *execpg.Store }

func (l leases) Lease(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (gitauth.AttemptLease, error) {
	snap, err := l.store.ObserveAttempt(ctx, scope, attempt)
	if errors.Is(err, execution.ErrNotFound) {
		return gitauth.AttemptLease{}, gitauth.ErrNotFound
	}
	return gitauth.AttemptLease{Live: err == nil && snap.Live}, err
}

// Test_source_git_grants_revoked_with_the_attempt: the revocation joined to the
// REAL gitauth grant store (migration 008, gitauth/postgres.RevokeAttempt as the
// store's GitGrants peer): a cancellation and a failed result revoke the stored
// grant in their own transaction, and a revoker that fails rolls the cancellation
// back (GR-06, PX-08).
func Test_source_git_grants_revoked_with_the_attempt(t *testing.T) {
	ctx := context.Background()
	issuer := &gitGrants{}
	var e *executiontest.Env
	var s executiontest.SourceSubject
	e, s = executiontest.NewSourcePG(t, executiontest.SourceOptions{
		GitGrants: issuer, GitRevoker: gitauthpg.RevokeAttempt, Contributions: []storage.Contribution{gitauthpg.Migrations()},
	})
	repos := &gitauthtest.Repositories{}
	svc, err := gitauth.NewService(gitauth.Config{
		Store: mustGitStore(t, e), Repositories: repos, Pushes: &gitauthtest.Pushes{}, Attempts: leases{store: e.Store},
		Authority: mustAuthority(t), Clock: e.Clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	issuer.svc = svc
	s.Repos.OnRepository = func(scope domain.Scope, r execution.SourceRepository) {
		repos.Put(scope, gitauthtest.NewRepository(r.ID, nil))
		e.OwnerExec(`INSERT INTO repositories (tenant_id, project_id, id, name, default_branch, hosting, status, created_by_type, created_by_id, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,'internal','ready','user','u',now(),now())`, string(scope.TenantID), string(scope.ProjectID), r.ID, r.Name, r.DefaultBranch)
	}

	start := func(t *testing.T) (*executiontest.SourceRun, execution.AttemptSpec) {
		t.Helper()
		r := executiontest.NewSourceRun(t, s, "write")
		if ok, err := s.Svc.DispatchNext(ctx, executiontest.SharedScope.TenantID); err != nil || !ok {
			t.Fatalf("dispatch: %v %v", ok, err)
		}
		starts := s.Starts()
		spec := starts[len(starts)-1]
		if spec.Git == nil {
			t.Fatalf("the attempt started without its grant: %+v", spec.Git)
		}
		return r, spec
	}
	active := func(attempt domain.AttemptID) (live, revoked int) {
		live = e.Count(`SELECT count(*) FROM git_grants WHERE attempt_id = $1 AND revoked_at IS NULL`, string(attempt))
		revoked = e.Count(`SELECT count(*) FROM git_grants WHERE attempt_id = $1 AND revoked_at IS NOT NULL`, string(attempt))
		return
	}
	caller := execution.Caller{PrincipalID: executiontest.Engineer}

	t.Run("cancellation revokes the stored grant, and a failing revoker rolls it back", func(t *testing.T) {
		r, spec := start(t)
		if live, revoked := active(spec.AttemptID); live != 1 || revoked != 0 {
			t.Fatalf("the issued grant: live %d revoked %d", live, revoked)
		}
		// A revoker that fails after the cancellation was written rolls it all back.
		failing := execpg.AttemptGrantRevoker(func(ctx context.Context, tx pgx.Tx, scope domain.Scope, attempt domain.AttemptID, now time.Time) (int, error) {
			if n, err := gitauthpg.RevokeAttempt(ctx, tx, scope, attempt, now); err != nil || n == 0 {
				return n, err
			}
			return 0, errors.New("revocation outage after the revoke statement")
		})
		peers := fakePeers(e)
		peers.GitGrants = failing
		bad, err := execpg.NewStore(e.Runtime, e.Events, peers, execpg.Options{Clock: e.Clock.Now, RunSources: true})
		if err != nil {
			t.Fatal(err)
		}
		badSvc, err := e.BuildService(executiontest.Options{Store: bad, SourceSets: s.Repos, GitGrants: issuer,
			Shared: &execution.SharedConfig{Resources: s.Resources, Policies: s.Policies}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := badSvc.Cancel(ctx, caller, executiontest.ScopeA, r.Run.ID); !errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("a failing revoker: %v", err)
		}
		agg, err := e.Store.Get(ctx, executiontest.ScopeA, r.Run.ID)
		if err != nil || agg.Run.CancelRequestedAt != nil || agg.Attempt(spec.AttemptID).Status != execution.AttemptRunning {
			t.Fatalf("the cancellation survived its failed revocation: %v", err)
		}
		if live, revoked := active(spec.AttemptID); live != 1 || revoked != 0 {
			t.Fatalf("the grant was revoked by a rolled back transaction: live %d revoked %d", live, revoked)
		}
		// The real store cancels and revokes in one transaction.
		if _, err := s.Svc.Cancel(ctx, caller, executiontest.ScopeA, r.Run.ID); err != nil {
			t.Fatal(err)
		}
		if live, revoked := active(spec.AttemptID); live != 0 || revoked != 1 {
			t.Fatalf("the cancellation left the stored grant live: live %d revoked %d", live, revoked)
		}
	})

	t.Run("a failed result revokes the stored grant", func(t *testing.T) {
		r, spec := start(t)
		code, reason := int64(1), "command_failed"
		res := runtimev3.ResultV3{Outcome: runtimev1.OutcomeFailed, ExitCode: &code, Reason: &reason, Commits: []runtimev3.CommitOutput{},
			Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
		h, err := runtimev3.ResultHash(res)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Svc.CommitResultV3(ctx, execution.ResultCommitV3{Scope: executiontest.ScopeA, AttemptID: spec.AttemptID, Request: runtimev3.ResultRequestV3{ResultHash: h, Result: res}}); err != nil {
			t.Fatal(err)
		}
		if live, revoked := active(spec.AttemptID); live != 0 || revoked != 1 {
			t.Fatalf("a failed attempt keeps its stored grant: live %d revoked %d", live, revoked)
		}
		if got, err := e.Store.Get(ctx, executiontest.ScopeA, r.Run.ID); err != nil || got.Run.Status == execution.RunSucceeded {
			t.Fatalf("a failed attempt: %v", err)
		}
	})
}

func mustGitStore(t *testing.T, e *executiontest.Env) gitauth.Store {
	t.Helper()
	st, err := gitauthpg.NewStore(e.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func mustAuthority(t *testing.T) identity.Authority {
	t.Helper()
	a, err := identity.NewStandaloneAuthority(&gitauthtest.Roles{})
	if err != nil {
		t.Fatal(err)
	}
	return a
}
