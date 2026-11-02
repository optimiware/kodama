package gitgrants_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gitauth/gitauthtest"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/gitgrants"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

// repositories is runtimeauth.GitRepositories over the Git authority's fixtures
// and a record of which run created which repository.
type repositories struct {
	repos   *gitauthtest.Repositories
	pushes  *gitauthtest.Pushes
	created map[string]domain.RunID // repository id -> creating run
}

func (r *repositories) Repository(ctx context.Context, scope domain.Scope, id string) (gitv1.Repository, error) {
	repo, err := r.repos.Repository(ctx, scope, id)
	if err != nil {
		return gitv1.Repository{}, runtimeauth.ErrNotFound
	}
	return repo, nil
}

func (r *repositories) UnsettledPush(ctx context.Context, scope domain.Scope, id string) (bool, error) {
	return r.pushes.UnsettledPush(ctx, scope, id)
}

func (r *repositories) CreatedByRun(_ context.Context, _ domain.Scope, run domain.RunID, id string) (bool, error) {
	return r.created[id] == run, nil
}

// service builds a runtimeauth.Service whose Git half is the real Git authority
// through this package's adapters, admits the rig's attempt and returns the
// authorization of its token.
func (r *rig) service(t *testing.T) (*runtimeauth.Service, func() runtimeauth.Authorization, *repositories) {
	t.Helper()
	repos := &repositories{repos: r.repos, pushes: r.pushes, created: map[string]domain.RunID{}}
	approvals, err := gitHubApprovals(r)
	if err != nil {
		t.Fatal(err)
	}
	r.m.GitConfig = &runtimeauth.GitConfig{Issuer: r.issuer, Repositories: repos, Approvals: approvals, Issued: r.m.Issued}
	svc := r.m.NewService(rtest.TestPolicy, r.m.Env.Grants, r.m.Env.Attempts, r.m.Jobs, r.m.Signer)
	attempt := r.m.SeedAttemptV3(r.context, rtest.AllActions())
	cred, err := runtimeauth.GenerateCredential(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.RegisterV3With(attempt, r.context, cred, rtest.AllActions()); err != nil {
		t.Fatal(err)
	}
	resp, err := svc.AdmitV3(bg, r.context.AttemptID, r.m.AdmitRequest(attempt, cred, "nnnnnnnnnnnnnnnnnnnnnn"))
	if err != nil {
		t.Fatal(err)
	}
	return svc, func() runtimeauth.Authorization {
		a, err := svc.Authorize(bg, runtimeauth.AuthorizeRequest{AttemptID: r.context.AttemptID, Bearer: resp.Token.Token, Action: runtimeauth.GitGrantAction, ContextVersion: runtimeauth.WireV3})
		if err != nil {
			t.Fatalf("authorize: %v", err)
		}
		return a
	}, repos
}

func gitHubApprovals(r *rig) (runtimeauth.IntegrationApprovals, error) {
	return gitgrants.NewApprovals(r.gates)
}

func approved(gate string, repo, base, candidate, tip string) func() (gates.IntegrationApproval, error) {
	return func() (gates.IntegrationApproval, error) {
		return gates.IntegrationApproval{GateID: gates.ID(gate), RunID: "run_01", RepositoryID: repo, Ref: "refs/heads/main", Base: base, Candidate: candidate, CurrentTip: tip}, nil
	}
}

func failing(err error) func() (gates.IntegrationApproval, error) {
	return func() (gates.IntegrationApproval, error) { return gates.IntegrationApproval{}, err }
}

func oid(c string) string { return strings.Repeat(c, 40) }

func refused(t *testing.T, err error, code domain.ErrorCode) {
	t.Helper()
	var ce *runtimev1.ContractError
	if !errors.Is(err, runtimeauth.ErrRefused) || !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("got %v, want a refusal %s", err, code)
	}
}

func Test_unit_runtime_seed_grant_over_gitauth(t *testing.T) {
	r := newRig(t, rtest.ProvisionContext)
	svc, authorize, repos := r.service(t)
	r.repos.Put(r.scope, gitauthtest.NewRepository("repo_new", func(g *gitv1.Repository) { g.Empty = true }))
	r.repos.Put(r.scope, gitauthtest.NewRepository("repo_two", func(g *gitv1.Repository) { g.Empty = true }))
	repos.created["repo_new"], repos.created["repo_two"] = "run_01", "run_01"
	auth := authorize()
	seed := func(id string) (runtimev3.GitGrantResponse, error) {
		return svc.SeedGitGrant(bg, auth, runtimev3.GitSeedRequest{RepositoryID: id})
	}

	resp, err := seed("repo_new")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := resp.Validate(); err != nil {
		t.Fatalf("the response violates the contract: %v", err)
	}
	claims, err := r.verify(resp.Credential)
	if err != nil {
		t.Fatalf("the Git authority does not know the credential: %v", err)
	}
	if claims.GrantID != resp.Grant.GrantID || claims.Kind != gitv1.GrantSeed || *claims.Actor.AttemptID != "att_01" {
		t.Fatalf("claims: %+v", claims)
	}
	create := func(repo, ref string) bool {
		return claims.PermitsUpdate(repo, gitv1.RefUpdate{Ref: ref, Old: strings.Repeat("0", 40), New: oid("2")})
	}
	if !create("repo_new", "refs/heads/main") || create("repo_new", "refs/heads/other") || create("repo_two", "refs/heads/main") || create("repo_new", "refs/tags/v1") {
		t.Fatal("a seed grant creates exactly the default branch of its one repository")
	}

	// A repeat before consumption: the same grant, a fresh credential that replaces
	// the first, and still one record.
	again, err := seed("repo_new")
	if err != nil || again.Grant.GrantID != resp.Grant.GrantID || again.Credential == resp.Credential {
		t.Fatalf("repeat: %+v %v", again, err)
	}
	if _, err := r.verify(resp.Credential); err == nil {
		t.Fatal("the replaced credential still works")
	}
	if issued, err := svc.IssuedGitGrants(bg, r.scope, "att_01"); err != nil || len(issued) != 1 {
		t.Fatalf("issued: %+v %v", issued, err)
	}

	// The Git authority keys a seed by its attempt: a second repository is refused,
	// not mixed into the first grant.
	_, err = seed("repo_two")
	refused(t, err, domain.CodeForbidden)

	// A non-empty or archived repository is the Git contract's own refusal.
	r2 := newRig(t, rtest.ProvisionContext)
	svc2, authorize2, repos2 := r2.service(t)
	r2.repos.Put(r2.scope, gitauthtest.NewRepository("repo_full", nil))
	repos2.created["repo_full"] = "run_01"
	_, err = svc2.SeedGitGrant(bg, authorize2(), runtimev3.GitSeedRequest{RepositoryID: "repo_full"})
	refused(t, err, gitv1.CodeRepositoryNotEmpty)

	// Cancellation ends it all: no seed, and the earlier credential is dead.
	r.cancel()
	if err := r.m.Env.Cancel(bg, r.scope, "att_01", false, r.m.Clock.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := seed("repo_new"); !errors.Is(err, runtimeauth.ErrAttemptRevoked) {
		t.Fatalf("seed after cancellation: %v", err)
	}
	if _, err := svc.Authorize(bg, runtimeauth.AuthorizeRequest{AttemptID: "att_01", Bearer: "x", Action: runtimeauth.GitGrantAction, ContextVersion: runtimeauth.WireV3}); err == nil {
		t.Fatal("authorize accepted a bad token")
	}
	if _, err := r.verify(again.Credential); err == nil {
		t.Fatal("the seed credential of a cancelled attempt works")
	}
}

func Test_unit_runtime_integration_grant_over_gitauth(t *testing.T) {
	r := newRig(t, rtest.WriterContext)
	svc, authorize, _ := r.service(t)
	auth := authorize()
	ask := func() (runtimev3.GitGrantResponse, error) {
		return svc.IntegrationGitGrant(bg, auth, runtimev3.GitIntegrationRequest{RepositoryID: "repo_01", GateID: "gate_07"})
	}
	good := approved("gate_07", "repo_01", oid("1"), oid("3"), oid("1"))

	// The approval is read by the route and read again by the Git authority when it
	// issues: a reviewer who loses the authority in between gets no grant.
	r.gates.answers = []func() (gates.IntegrationApproval, error){good, failing(gates.ErrNoApproval)}
	_, err := ask()
	refused(t, err, domain.CodeForbidden)
	if issued, _ := svc.IssuedGitGrants(bg, r.scope, "att_01"); len(issued) != 0 {
		t.Fatalf("a refused issuance was recorded: %+v", issued)
	}

	for name, c := range map[string]struct {
		answer func() (gates.IntegrationApproval, error)
		check  func(error)
	}{
		"no approval":    {failing(gates.ErrNoApproval), func(err error) { refused(t, err, domain.CodeForbidden) }},
		"an absent gate": {failing(gates.ErrNotFound), func(err error) { refused(t, err, domain.CodeForbidden) }},
		"a moved base": {failing(&gates.StaleError{Definitive: true, Reason: "base_moved", SupersedingCommit: oid("4")}), func(err error) {
			var pc *runtimeauth.PublicationConflict
			if !errors.As(err, &pc) || pc.Reason != "base_moved" || pc.ObservedTip != oid("4") {
				t.Errorf("got %v", err)
			}
		}},
		"already integrated": {failing(&gates.AlreadyIntegratedError{Approval: gates.IntegrationApproval{GateID: "gate_07", RepositoryID: "repo_01", Candidate: oid("3")}}), func(err error) {
			var pc *runtimeauth.PublicationConflict
			if !errors.Is(err, runtimeauth.ErrAlreadyIntegrated) || !errors.As(err, &pc) || pc.ObservedTip != oid("3") {
				t.Errorf("got %v", err)
			}
		}},
		"an outage": {failing(errors.New("gates down")), func(err error) {
			if !errors.Is(err, runtimeauth.ErrUnavailable) {
				t.Errorf("got %v", err)
			}
		}},
	} {
		r.gates.mu.Lock()
		r.gates.answers, r.gates.calls = []func() (gates.IntegrationApproval, error){c.answer}, 0
		r.gates.mu.Unlock()
		_, err := ask()
		c.check(err)
		if issued, _ := svc.IssuedGitGrants(bg, r.scope, "att_01"); len(issued) != 0 {
			t.Fatalf("%s: a grant was recorded: %+v", name, issued)
		}
	}

	r.gates.mu.Lock()
	r.gates.answers, r.gates.calls = []func() (gates.IntegrationApproval, error){good}, 0
	r.gates.mu.Unlock()
	resp, err := ask()
	if err != nil {
		t.Fatalf("integration: %v", err)
	}
	claims, err := r.verify(resp.Credential)
	if err != nil {
		t.Fatalf("the Git authority does not know the credential: %v", err)
	}
	in := claims.Repositories[0].Integration
	if claims.Kind != gitv1.GrantIntegration || in == nil || *in != (gitv1.IntegrationConstraint{Ref: "refs/heads/main", From: oid("1"), To: oid("3"), GateID: "gate_07"}) {
		t.Fatalf("claims: %+v", claims)
	}
	push := func(repo, ref, old, next string) bool {
		return claims.PermitsUpdate(repo, gitv1.RefUpdate{Ref: ref, Old: old, New: next})
	}
	if !push("repo_01", "refs/heads/main", oid("1"), oid("3")) || push("repo_01", "refs/heads/main", oid("1"), oid("5")) ||
		push("repo_01", "refs/heads/main", oid("2"), oid("3")) || push("repo_01", "refs/heads/kodama/work/wi_1042", oid("1"), oid("3")) || push("repo_02", "refs/heads/main", oid("1"), oid("3")) {
		t.Fatal("an integration grant permits exactly its one update")
	}
	// A repeat is the same grant with a fresh credential; one record.
	again, err := ask()
	if err != nil || again.Grant.GrantID != resp.Grant.GrantID || again.Credential == resp.Credential {
		t.Fatalf("repeat: %+v %v", again, err)
	}
	if issued, _ := svc.IssuedGitGrants(bg, r.scope, "att_01"); len(issued) != 1 {
		t.Fatalf("issued: %+v", issued)
	}

	// After cancellation: refused by the route layer, and the credential is dead.
	r.cancel()
	if err := r.m.Env.Cancel(bg, r.scope, "att_01", false, r.m.Clock.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := ask(); !errors.Is(err, runtimeauth.ErrAttemptRevoked) {
		t.Fatalf("after cancellation: %v", err)
	}
	if _, err := r.verify(again.Credential); err == nil {
		t.Fatal("the integration credential of a cancelled attempt works")
	}
}

// The Git authority reads the attempt's lease outside its grant transaction, so a
// cancellation can land between its liveness read and the insert. The runtime
// authority reads the attempt again under its lock and withdraws the grant.
func Test_unit_runtime_grant_issuance_race_over_gitauth(t *testing.T) {
	r := newRig(t, rtest.ProvisionContext)
	svc, authorize, repos := r.service(t)
	r.repos.Put(r.scope, gitauthtest.NewRepository("repo_new", func(g *gitv1.Repository) { g.Empty = true }))
	repos.created["repo_new"] = "run_01"
	auth := authorize()
	r.afterIssue = func() {
		r.cancel()
		if err := r.m.Env.Cancel(bg, r.scope, "att_01", false, r.m.Clock.Now()); err != nil {
			t.Error(err)
		}
	}
	resp, err := svc.SeedGitGrant(bg, auth, runtimev3.GitSeedRequest{RepositoryID: "repo_new"})
	if !errors.Is(err, runtimeauth.ErrAttemptRevoked) || resp.Credential != "" {
		t.Fatalf("seed racing a cancellation: %+v %v", resp, err)
	}
	if n, err := r.auth.Active(bg, r.scope.TenantID); err != nil || n != 0 {
		t.Fatalf("the grant of the ended attempt was not revoked: %d %v", n, err)
	}
}
