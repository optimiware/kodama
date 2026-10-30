package gitgrants_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gitauth"
	"github.com/optimiware/kodama/internal/gitauth/gitauthtest"
	"github.com/optimiware/kodama/internal/gitpush"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/gitgrants"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

// The Git side of the runtime authority (T-150) over the REAL Git authority
// (internal/gitauth, memory adapter): the attempt's credential, its renewal and
// revocation, the liveness answers the Git services read, and the seed and
// integration grants issued during the attempt. Execution's record is a hand-built
// aggregate (the adapters read exported state only); the gates module is a fake of
// its one query.

var bg = context.Background()

// fakeRuns serves one hand-built aggregate and lets a test change it as execution
// would (cancel, finish, deadline).
type fakeRuns struct {
	mu    sync.Mutex
	agg   *execution.Aggregate
	now   func() time.Time
	locks int
}

func (f *fakeRuns) RunOf(_ context.Context, scope domain.Scope, attempt domain.AttemptID) (domain.RunID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if at := f.agg.Attempt(attempt); at != nil && at.Scope == scope {
		return f.agg.Run.ID, nil
	}
	return "", execution.ErrNotFound
}

func (f *fakeRuns) Get(_ context.Context, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.agg.Run.Scope != scope || f.agg.Run.ID != id {
		return nil, execution.ErrNotFound
	}
	return f.agg, nil
}

// Update models the run row lock: the mutation sees the record as it is now, and
// nothing it returns is written.
func (f *fakeRuns) Update(_ context.Context, scope domain.Scope, id domain.RunID, _ execution.UpdateOptions, fn execution.Mutation) (*execution.Aggregate, any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.agg.Run.Scope != scope || f.agg.Run.ID != id {
		return nil, nil, execution.ErrNotFound
	}
	f.locks++
	_, err := fn(context.Background(), fakeEnv{now: f.now}, f.agg)
	return f.agg, nil, err
}

type fakeEnv struct {
	execution.Env
	now func() time.Time
}

func (e fakeEnv) Now() time.Time { return e.now() }

func (f *fakeRuns) edit(fn func(a *execution.Aggregate)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.agg)
}

// fakeGates is gates.Service's one query. answers is consumed in order; the last
// repeats.
type fakeGates struct {
	mu      sync.Mutex
	answers []func() (gates.IntegrationApproval, error)
	calls   int
}

func (g *fakeGates) IntegrationApproval(_ context.Context, _ domain.Scope, run domain.RunID, gate gates.ID, repository string) (gates.IntegrationApproval, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	i := g.calls
	if i >= len(g.answers) {
		i = len(g.answers) - 1
	}
	g.calls++
	if i < 0 {
		return gates.IntegrationApproval{}, gates.ErrNoApproval
	}
	return g.answers[i]()
}

type rig struct {
	t       *testing.T
	m       *rtest.V3MemStack
	runs    *fakeRuns
	repos   *gitauthtest.Repositories
	pushes  *gitauthtest.Pushes
	gates   *fakeGates
	auth    *gitauth.Service
	leases  *gitgrants.Leases
	grants  *gitgrants.Grants
	issuer  *gitgrants.Issuer
	context runtimev3.ContextV3
	scope   domain.Scope
	// afterIssue runs after the Git authority issued something (a cancellation
	// that races the issuance).
	afterIssue func()
}

// raceAuthority wraps the real Git authority so a test can act between its calls.
type raceAuthority struct {
	gitgrants.Authority
	r *rig
}

func (a raceAuthority) IssueAttempt(ctx context.Context, scope domain.Scope, req gitauth.AttemptGrantRequest) (gitauth.GrantHandle, error) {
	h, err := a.Authority.IssueAttempt(ctx, scope, req)
	if err == nil && a.r.afterIssue != nil {
		a.r.afterIssue()
	}
	return h, err
}

func (a raceAuthority) IssueIntegration(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, repo string, c gitv1.IntegrationConstraint) (gitauth.GrantHandle, error) {
	h, err := a.Authority.IssueIntegration(ctx, scope, attempt, repo, c)
	if err == nil && a.r.afterIssue != nil {
		a.r.afterIssue()
	}
	return h, err
}

func newRig(t *testing.T, fixture string) *rig {
	t.Helper()
	m := rtest.NewV3MemStack(t)
	r := &rig{t: t, m: m, repos: &gitauthtest.Repositories{}, pushes: &gitauthtest.Pushes{}, gates: &fakeGates{}}
	deadline := m.Clock.Now().Add(time.Hour).UTC()
	r.context = rtest.V3Context(t, fixture, func(c *runtimev3.ContextV3) {
		c.Run.StartedAt = runtimev3.Timestamp(m.Clock.Now().UTC().Format(time.RFC3339))
		c.Run.DeadlineAt = runtimev3.Timestamp(deadline.Format(time.RFC3339))
	})
	r.scope = r.context.Scope()
	_, doc, err := runtimeauth.PinContextV3(r.context)
	if err != nil {
		t.Fatal(err)
	}
	r.runs = &fakeRuns{now: m.Clock.Now, agg: &execution.Aggregate{
		Run:      &execution.Run{ID: r.context.RunID, Scope: r.scope, Status: execution.RunRunning, DeadlineAt: &deadline},
		Attempts: []*execution.Attempt{{Scope: r.scope, ID: r.context.AttemptID, RunID: r.context.RunID, StepID: r.context.StepID, Status: execution.AttemptRunning, Context: doc}},
		Controls: map[domain.AttemptID]*execution.Control{r.context.AttemptID: execution.NewControl(r.context.AttemptID)},
	}}
	if r.leases, err = gitgrants.NewLeases(r.runs, m.Clock.Now); err != nil {
		t.Fatal(err)
	}
	approvals, err := gitgrants.NewApprovals(r.gates)
	if err != nil {
		t.Fatal(err)
	}
	decisions, err := gitgrants.NewGateDecisions(approvals)
	if err != nil {
		t.Fatal(err)
	}
	roles := &gitauthtest.Roles{}
	authority, err := identity.NewStandaloneAuthority(roles)
	if err != nil {
		t.Fatal(err)
	}
	r.auth, err = gitauth.NewService(gitauth.Config{
		Store: gitauthtest.NewMemEnv(t).Store(), Repositories: r.repos, Pushes: r.pushes, Attempts: r.leases, Gates: decisions,
		Authority: authority, Clock: m.Clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := raceAuthority{Authority: r.auth, r: r}
	if r.grants, err = gitgrants.NewGrants(wrapped, r.runs, m.Clock.Now); err != nil {
		t.Fatal(err)
	}
	if r.issuer, err = gitgrants.NewIssuer(wrapped, m.Clock.Now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"repo_01", "repo_02"} {
		r.repos.Put(r.scope, gitauthtest.NewRepository(id, nil))
	}
	return r
}

func (r *rig) cancel() {
	r.runs.edit(func(a *execution.Aggregate) {
		now := r.m.Clock.Now()
		a.Control(r.context.AttemptID).RevokedAt = &now
		a.Attempts[0].Status = execution.AttemptCancelled
	})
}

func (r *rig) request() execution.AttemptGrantRequest {
	r.t.Helper()
	req, needed, err := execution.AttemptGrantFor(r.context, r.m.Clock.Now())
	if err != nil || !needed {
		r.t.Fatalf("grant request: %v %v", err, needed)
	}
	return req
}

func (r *rig) verify(credential string) (gitv1.GrantClaims, error) {
	return r.auth.Verify(bg, r.scope.TenantID, credential)
}

func Test_unit_attempt_git_credential_delivered_and_renewed(t *testing.T) {
	r := newRig(t, rtest.WriterContext)
	deadline, _ := r.context.Run.DeadlineAt.Time()

	// Delivered: execution's own request over the Git authority, one credential.
	h, err := r.grants.IssueAttempt(bg, r.scope, r.request())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	secret := h.Credential.Reveal()
	if h.GrantID == "" || !strings.HasPrefix(secret, "gitc_") || h.ExpiresAt.After(deadline) {
		t.Fatalf("handle: %+v (deadline %s)", h, deadline)
	}
	for _, rendered := range []string{fmt.Sprint(h), fmt.Sprintf("%+v", h), fmt.Sprintf("%#v", h), fmt.Sprintf("%v", h.Credential)} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("the credential prints itself: %s", rendered)
		}
	}
	if raw, _ := json.Marshal(h); strings.Contains(string(raw), secret) {
		t.Fatal("the credential is encodable")
	}
	claims, err := r.verify(secret)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Kind != gitv1.GrantAttempt || claims.GrantID != h.GrantID || *claims.Actor.AttemptID != "att_01" || *claims.Actor.RunID != "run_01" {
		t.Fatalf("claims: %+v", claims)
	}
	if claims.ProjectID != string(r.scope.ProjectID) || claims.TenantID != string(r.scope.TenantID) {
		t.Fatalf("the grant is not the run's tenant and project: %+v", claims)
	}
	// Exactly what the context allows: its agent namespace and, as the writer, its
	// task branch; never the default branch.
	upd := func(repo, ref string) bool {
		return claims.PermitsUpdate(repo, gitv1.RefUpdate{Ref: ref, Old: strings.Repeat("1", 40), New: strings.Repeat("2", 40)})
	}
	if !upd("repo_01", "refs/heads/kodama/agents/wi_1042/run_01/x") || !upd("repo_01", "refs/heads/kodama/work/wi_1042") ||
		upd("repo_01", "refs/heads/main") || upd("repo_02", "refs/heads/kodama/work/wi_1042") || upd("repo_03", "refs/heads/kodama/agents/wi_1042/run_01/x") {
		t.Fatalf("the grant is not the context's: %+v", claims.Repositories)
	}

	// Renewed: the same grant with a fresh secret that replaces the earlier one;
	// the expiry is the first issuance's and never later.
	r.m.Clock.Advance(10 * time.Minute)
	renewed, err := r.grants.RenewAttempt(bg, r.scope, "att_01")
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if renewed.GrantID != h.GrantID || renewed.Credential.Reveal() == secret || !renewed.ExpiresAt.Equal(h.ExpiresAt) || renewed.ExpiresAt.After(deadline) {
		t.Fatalf("renewal: %+v vs %+v", renewed, h)
	}
	if _, err := r.verify(secret); err == nil {
		t.Fatal("the replaced credential still works")
	}
	if _, err := r.verify(renewed.Credential.Reveal()); err != nil {
		t.Fatalf("the renewed credential does not work: %v", err)
	}

	// Never past the run: a credential expires with the grant, at the latest the
	// run's deadline, and a renewal after it is refused.
	r.m.Clock.Advance(time.Hour)
	if _, err := r.verify(renewed.Credential.Reveal()); err == nil {
		t.Fatal("a credential outlived the run's deadline")
	}
	_, err = r.grants.RenewAttempt(bg, r.scope, "att_01")
	if !errors.Is(err, execution.ErrAttemptRevoked) {
		t.Fatalf("a renewal after the run's deadline: %v", err)
	}
}

func Test_unit_attempt_git_credential_renewal_refusals(t *testing.T) {
	// A renewal is rebuilt from the committed context of a live attempt only.
	t.Run("not running, terminal, draining", func(t *testing.T) {
		for name, end := range map[string]func(*rig){
			"revoked": func(r *rig) { r.cancel() },
			"finished": func(r *rig) {
				r.runs.edit(func(a *execution.Aggregate) { a.Attempts[0].Status = execution.AttemptSucceeded })
			},
			"terminal": func(r *rig) { r.runs.edit(func(a *execution.Aggregate) { a.Run.Status = execution.RunSucceeded }) },
			"draining": func(r *rig) {
				now := r.m.Clock.Now()
				r.runs.edit(func(a *execution.Aggregate) { a.Run.CancelRequestedAt = &now })
			},
		} {
			r := newRig(t, rtest.WriterContext)
			if _, err := r.grants.IssueAttempt(bg, r.scope, r.request()); err != nil {
				t.Fatal(err)
			}
			end(r)
			if _, err := r.grants.RenewAttempt(bg, r.scope, "att_01"); !errors.Is(err, execution.ErrAttemptRevoked) {
				t.Errorf("%s: %v", name, err)
			}
			if live, err := r.leases.Live(bg, r.scope, "att_01"); err != nil || live {
				t.Errorf("%s: live = %v %v", name, live, err)
			}
		}
	})
	t.Run("no static grant, another version, unknown attempt, another tenant", func(t *testing.T) {
		r := newRig(t, rtest.ProvisionContext)
		if _, err := r.grants.RenewAttempt(bg, r.scope, "att_01"); !errors.Is(err, execution.ErrNotFound) {
			t.Fatalf("a provisioning attempt holds no static grant: %v", err)
		}
		r = newRig(t, rtest.WriterContext)
		r.runs.edit(func(a *execution.Aggregate) { a.Attempts[0].Context = []byte(`{"contextVersion":2}`) })
		if _, err := r.grants.RenewAttempt(bg, r.scope, "att_01"); !errors.Is(err, execution.ErrContextVersionMismatch) {
			t.Fatalf("a v2 attempt: %v", err)
		}
		r = newRig(t, rtest.WriterContext)
		if _, err := r.grants.RenewAttempt(bg, r.scope, "att_nowhere"); !errors.Is(err, execution.ErrNotFound) {
			t.Fatalf("unknown attempt: %v", err)
		}
		other := domain.Scope{TenantID: "t_globex", ProjectID: r.scope.ProjectID}
		if _, err := r.grants.RenewAttempt(bg, other, "att_01"); !errors.Is(err, execution.ErrNotFound) {
			t.Fatalf("another tenant's attempt: %v", err)
		}
	})
	t.Run("a revoked grant is not renewed", func(t *testing.T) {
		r := newRig(t, rtest.WriterContext)
		if _, err := r.grants.IssueAttempt(bg, r.scope, r.request()); err != nil {
			t.Fatal(err)
		}
		if err := r.grants.RevokeAttempt(bg, r.scope, "att_01"); err != nil {
			t.Fatal(err)
		}
		if _, err := r.grants.RenewAttempt(bg, r.scope, "att_01"); err == nil {
			t.Fatal("a revoked grant was renewed")
		}
	})
	t.Run("an attempt that ends while its credential is rotated gets nothing", func(t *testing.T) {
		r := newRig(t, rtest.WriterContext)
		first, err := r.grants.IssueAttempt(bg, r.scope, r.request())
		if err != nil {
			t.Fatal(err)
		}
		r.afterIssue = r.cancel
		_, err = r.grants.RenewAttempt(bg, r.scope, "att_01")
		if !errors.Is(err, execution.ErrAttemptRevoked) {
			t.Fatalf("renewal racing a cancellation: %v", err)
		}
		r.afterIssue = nil
		// Revoked, not merely not live: bring the attempt back to prove the grant is gone.
		r.runs.edit(func(a *execution.Aggregate) {
			a.Control("att_01").RevokedAt = nil
			a.Attempts[0].Status = execution.AttemptRunning
		})
		if _, err := r.verify(first.Credential.Reveal()); err == nil {
			t.Fatal("the grant of an ended attempt survived")
		}
	})
}

func Test_unit_attempt_git_credential_revoked_on_cancel(t *testing.T) {
	r := newRig(t, rtest.WriterContext)
	h, err := r.grants.IssueAttempt(bg, r.scope, r.request())
	if err != nil {
		t.Fatal(err)
	}
	cred := h.Credential.Reveal()
	if live, err := r.leases.Live(bg, r.scope, "att_01"); err != nil || !live {
		t.Fatalf("live: %v %v", live, err)
	}
	lease, err := r.leases.Lease(bg, r.scope, "att_01")
	deadline, _ := r.context.Run.DeadlineAt.Time()
	if err != nil || !lease.Live || !lease.Deadline.Equal(deadline) {
		t.Fatalf("lease: %+v %v (the lease must last as long as the run: %s)", lease, err, deadline)
	}

	// The cancellation commits in execution (its store revokes the grants in the same
	// transaction, which is the cleanup): even before any revocation the Git services
	// read the attempt's own state at every use, so the credential stops working and
	// the claims stop resolving at once.
	r.cancel()
	if live, err := r.leases.Live(bg, r.scope, "att_01"); err != nil || live {
		t.Fatalf("a cancelled attempt is live: %v %v", live, err)
	}
	if _, err := r.verify(cred); err == nil {
		t.Fatal("a cancelled attempt's credential still verifies")
	}
	if _, err := r.auth.Claims(bg, r.scope.TenantID, h.GrantID); err == nil {
		t.Fatal("a cancelled attempt's grant still resolves (the push authorizer would allow a push)")
	}
	// A new credential is refused for the ended attempt.
	if _, err := r.grants.IssueAttempt(bg, r.scope, r.request()); err == nil {
		t.Fatal("a cancelled attempt was issued a credential")
	}
	// The revocation is idempotent and leaves nothing active.
	for i := 0; i < 2; i++ {
		if err := r.grants.RevokeAttempt(bg, r.scope, "att_01"); err != nil {
			t.Fatalf("revoke %d: %v", i, err)
		}
	}
	if n, err := r.auth.Active(bg, r.scope.TenantID); err != nil || n != 0 {
		t.Fatalf("active grants after revocation: %d %v", n, err)
	}
	// A cancellation that lands between the Git authority's lease read and its
	// insert is closed by the locking read after the grant is committed: nothing is
	// delivered and the grant is revoked (execution's own re-read only narrows it).
	race := newRig(t, rtest.WriterContext)
	race.afterIssue = race.cancel
	if _, err := race.grants.IssueAttempt(bg, race.scope, race.request()); !errors.Is(err, execution.ErrAttemptRevoked) {
		t.Fatalf("issuance racing a cancellation: %v", err)
	}
	if race.runs.locks != 1 {
		t.Fatalf("the liveness was not read under the run lock: %d", race.runs.locks)
	}
	if n, err := race.auth.Active(bg, race.scope.TenantID); err != nil || n != 0 {
		t.Fatalf("the grant of the ended attempt survived: %d %v", n, err)
	}
	// A live attempt reads its liveness under the lock too and keeps its grant.
	live := newRig(t, rtest.WriterContext)
	if _, err := live.grants.IssueAttempt(bg, live.scope, live.request()); err != nil || live.runs.locks != 1 {
		t.Fatalf("a live attempt: %v (locks %d)", err, live.runs.locks)
	}
	// An unknown attempt is not live and not found; another tenant's is unknown too.
	if _, err := r.leases.Live(bg, r.scope, "att_nowhere"); !errors.Is(err, gitpush.ErrNotFound) {
		t.Fatalf("unknown attempt: %v", err)
	}
	if _, err := r.leases.Lease(bg, domain.Scope{TenantID: "t_globex", ProjectID: r.scope.ProjectID}, "att_01"); !errors.Is(err, gitauth.ErrNotFound) {
		t.Fatalf("another tenant: %v", err)
	}
}

func Test_unit_attempt_preparation_grant_is_read_only(t *testing.T) {
	r := newRig(t, rtest.WriterContext)
	run := &execution.Run{ID: "run_01", Scope: r.scope, Sources: &execution.RunSources{Entries: []execution.RunSource{
		{RepositoryID: "repo_01", Commit: strings.Repeat("1", 40), Path: "backend", Access: gitv1.AccessWrite},
		{RepositoryID: "repo_02", Commit: strings.Repeat("2", 40), Path: "frontend", Access: gitv1.AccessRead},
	}}}
	// The preparation workload's grant is keyed by the run; a run keeps its
	// attempt-bound lease check only for attempts, so the preparation grant needs
	// no attempt, expires with the preparation and updates nothing.
	h, err := r.grants.IssuePreparation(bg, run, "kr-prep-run_01", r.m.Clock.Now().Add(30*time.Minute))
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	c, err := r.verify(h.Credential.Reveal())
	if err != nil || c.Kind != gitv1.GrantPreparation || len(c.Repositories) != 2 {
		t.Fatalf("claims: %+v %v", c, err)
	}
	for _, repo := range c.Repositories {
		if len(repo.Update) != 0 || !repo.Read {
			t.Fatalf("a preparation grant must be read-only: %+v", repo)
		}
	}
	if _, err := r.grants.IssuePreparation(bg, &execution.Run{ID: "run_01", Scope: r.scope}, "kr-prep", r.m.Clock.Now().Add(time.Hour)); err == nil {
		t.Fatal("a source-free run has no preparation grant")
	}
}
