package rtest_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitauth/gitauthtest"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

// The source-set wire of the runtime authority (T-150) on the memory fixtures:
// admission, renewal and context on the v3 wire, the version fence in both
// directions, and the two runtime Git grants.

// v3Env is one attempt of a source-set run, registered and admitted.
type v3Env struct {
	*rtest.V3MemStack
	t       *testing.T
	scope   domain.Scope
	context runtimev3.ContextV3
	attempt rtest.Attempt
	token   string
	cred    runtimeauth.Credential
}

// newV3Env admits an attempt with the named context fixture. The run's deadline is
// moved to an hour after the stack's clock so the grants the tests issue are
// inside it.
func newV3Env(t *testing.T, fixture string, edits ...func(*runtimev3.ContextV3)) *v3Env {
	t.Helper()
	m := rtest.NewV3MemStack(t)
	deadline := m.Clock.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	started := m.Clock.Now().UTC().Format(time.RFC3339)
	c := rtest.V3Context(t, fixture, append([]func(*runtimev3.ContextV3){func(c *runtimev3.ContextV3) {
		c.Run.StartedAt, c.Run.DeadlineAt = runtimev3.Timestamp(started), runtimev3.Timestamp(deadline)
	}}, edits...)...)
	e := &v3Env{V3MemStack: m, t: t, scope: c.Scope(), context: c}
	e.attempt = m.SeedAttemptV3(c, rtest.AllActions())
	cred, err := runtimeauth.GenerateCredential(nil)
	if err != nil {
		t.Fatal(err)
	}
	e.cred = cred
	if _, err := m.RegisterV3With(e.attempt, c, cred, rtest.AllActions()); err != nil {
		t.Fatalf("register: %v", err)
	}
	resp, err := m.Service.AdmitV3(context.Background(), c.AttemptID, m.AdmitRequest(e.attempt, cred, "nnnnnnnnnnnnnnnnnnnnnn"))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	e.token = resp.Token.Token
	return e
}

func (e *v3Env) authorize() runtimeauth.Authorization {
	e.t.Helper()
	a, err := e.Service.Authorize(context.Background(), runtimeauth.AuthorizeRequest{
		AttemptID: e.context.AttemptID, Bearer: e.token, Action: runtimeauth.GitGrantAction, ContextVersion: runtimeauth.WireV3,
	})
	if err != nil {
		e.t.Fatalf("authorize: %v", err)
	}
	return a
}

func wantRefused(t *testing.T, err error, code domain.ErrorCode) {
	t.Helper()
	var ce *runtimev1.ContractError
	if !errors.Is(err, runtimeauth.ErrRefused) || !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("got %v, want a refusal with code %s", err, code)
	}
}

const (
	hexA = "1111111111111111111111111111111111111111"
	hexC = "3333333333333333333333333333333333333333"
)

func Test_unit_runtimeauth_v3_wire_admission_and_fence(t *testing.T) {
	e := newV3Env(t, rtest.WriterContext)
	ctx := context.Background()

	got, err := e.Service.ContextV3(ctx, e.context.AttemptID, e.token)
	if err != nil {
		t.Fatal(err)
	}
	want, _, _ := runtimeauth.PinContextV3(e.context)
	have, _, _ := runtimeauth.PinContextV3(got)
	if want != have {
		t.Fatal("the v3 context is not the pinned one")
	}
	if _, err := e.Service.RenewV3(ctx, e.context.AttemptID, e.token); err != nil {
		t.Fatalf("renew: %v", err)
	}

	// The fence in both directions: a v3 attempt on the v1 and v2 service methods is
	// a version mismatch with nothing issued; so is the reverse.
	_, err = e.Service.Context(ctx, e.context.AttemptID, e.token)
	is(t, err, runtimeauth.ErrContextVersionMismatch)
	_, err = e.Service.ContextV2(ctx, e.context.AttemptID, e.token)
	is(t, err, runtimeauth.ErrContextVersionMismatch)
	_, err = e.Service.Renew(ctx, e.context.AttemptID, e.token)
	is(t, err, runtimeauth.ErrContextVersionMismatch)
	_, err = e.Service.RenewV2(ctx, e.context.AttemptID, e.token)
	is(t, err, runtimeauth.ErrContextVersionMismatch)
	for _, wire := range []int64{runtimeauth.WireV1, runtimeauth.WireV2} {
		_, err = e.Service.Authorize(ctx, runtimeauth.AuthorizeRequest{AttemptID: e.context.AttemptID, Bearer: e.token, Action: runtimeauth.ActionContextRead, ContextVersion: wire})
		is(t, err, runtimeauth.ErrContextVersionMismatch)
	}

	// A v1 attempt on the v3 admission: mismatch, nothing consumed.
	o := rtest.NewV3MemStack(t)
	v1, req := o.HappyContext()
	other := o.SeedAttempt(v1, rtest.AllActions())
	cred, _, err := o.Register(other, rtest.AllActions())
	if err != nil {
		t.Fatal(err)
	}
	req.AdmissionCredential = cred.Reveal()
	_, err = o.Service.AdmitV3(ctx, v1.AttemptID, req)
	is(t, err, runtimeauth.ErrContextVersionMismatch)
	if _, err := o.Service.Admit(ctx, v1.AttemptID, req); err != nil {
		t.Fatalf("the refused v3 admission consumed the credential: %v", err)
	}
	if runtimeauth.WireOf([]byte(`{"contextVersion":3}`)) != runtimeauth.WireV3 || runtimeauth.WireOf([]byte(`{"contextVersion":2}`)) != runtimeauth.WireV2 || runtimeauth.WireOf([]byte(`{}`)) != runtimeauth.WireV1 {
		t.Fatal("WireOf")
	}

	// A cancelled attempt has no authority on the wire.
	if err := e.Env.Cancel(ctx, e.scope, e.context.AttemptID, true, e.Clock.Now()); err != nil {
		t.Fatal(err)
	}
	_, err = e.Service.ContextV3(ctx, e.context.AttemptID, e.token)
	is(t, err, runtimeauth.ErrAttemptRevoked)
	_, err = e.Service.RenewV3(ctx, e.context.AttemptID, e.token)
	is(t, err, runtimeauth.ErrAttemptRevoked)
}

func Test_unit_runtimeauth_v3_resources(t *testing.T) {
	m := rtest.NewV3MemStack(t)
	ctx := context.Background()
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	run := domain.RunID("run_01")
	deadline := m.Clock.Now().Add(time.Hour)
	f := rtest.ResourceFixture{Scope: scope, RunID: run, WorkloadUID: "6f1c1d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f", DeadlineAt: deadline}
	st3 := rtest.PreparationStateV3(t, f)
	ref := runtimeauth.ResourceRef{Scope: scope, RunID: run, Key: "preparation"}
	m.Resources.Put(st3)
	// A v2 resource of another run on the same fixture.
	v2Ref := runtimeauth.ResourceRef{Scope: scope, RunID: "run_02", Key: "preparation"}
	m.Resources.Put(rtest.PreparationState(rtest.ResourceFixture{Scope: scope, RunID: "run_02", WorkloadUID: "7a2d2e3f-4b5c-4d6e-9fa0-1b2c3d4e5f60", DeadlineAt: deadline}))

	register := func(r runtimeauth.ResourceRef) runtimeauth.Credential {
		st, err := m.Resources.ObserveResource(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		cred, _ := runtimeauth.GenerateCredential(nil)
		if err := m.Service.RegisterResource(ctx, runtimeauth.ResourceRegisterCommand{Scope: r.Scope, RunID: r.RunID, Key: r.Key, Credential: cred, SpecFingerprint: st.Fingerprint, ExpiresAt: st.DeadlineAt}); err != nil {
			t.Fatal(err)
		}
		return cred
	}
	admit := func(r runtimeauth.ResourceRef, cred runtimeauth.Credential, v3 bool) (any, error) {
		st, _ := m.Resources.ObserveResource(ctx, r)
		req := runtimev2.ResourceAdmitRequest{AdmissionCredential: cred.Reveal(), Nonce: "nnnnnnnnnnnnnnnnnnnnnn", ClientVersion: "1.4.0",
			Identity: runtimev2.ResourceIdentity{WorkloadName: st.WorkloadName, WorkloadUID: st.WorkloadUID}}
		if v3 {
			return m.Service.AdmitResourceV3(ctx, r, req)
		}
		return m.Service.AdmitResource(ctx, r, req)
	}

	cred := register(ref)
	// The v2 wire refuses the v3 resource and consumes nothing.
	_, err := admit(ref, cred, false)
	is(t, err, runtimeauth.ErrContextVersionMismatch)
	out, err := admit(ref, cred, true)
	if err != nil {
		t.Fatalf("v3 admission after a refused v2 one: %v", err)
	}
	resp := out.(runtimev3.ResourceAdmitResponseV3)
	if resp.Context.Preparation == nil || len(resp.Context.Preparation.Sources) != 2 || resp.Context.Preparation.Git.GrantID == "" {
		t.Fatalf("the context is not the committed preparation: %+v", resp.Context)
	}
	tok := resp.Token.Token

	// Every v3 operation; the v2 operations of the same token are refused.
	if _, err := m.Service.RenewResourceV3(ctx, run, "preparation", tok); err != nil {
		t.Fatalf("renew: %v", err)
	}
	c, err := m.Service.ResourceContextV3(ctx, run, "preparation", tok)
	if err != nil || c.Preparation == nil {
		t.Fatalf("context: %v", err)
	}
	_, err = m.Service.RenewResource(ctx, run, "preparation", tok)
	is(t, err, runtimeauth.ErrContextVersionMismatch)
	_, err = m.Service.ResourceContext(ctx, run, "preparation", tok)
	is(t, err, runtimeauth.ErrContextVersionMismatch)
	_, err = m.Service.AuthorizeResource(ctx, runtimeauth.ResourceAuthorizeRequest{RunID: run, Key: "preparation", Bearer: tok, Op: runtimeauth.OpResourceComplete})
	is(t, err, runtimeauth.ErrContextVersionMismatch)
	a, err := m.Service.AuthorizeResource(ctx, runtimeauth.ResourceAuthorizeRequest{RunID: run, Key: "preparation", Bearer: tok, Op: runtimeauth.OpResourceComplete, ContextVersion: runtimeauth.WireV3})
	if err != nil || a.ContextV3 == nil || a.ContextVersion != runtimeauth.WireV3 {
		t.Fatalf("authorize: %+v %v", a, err)
	}
	if err := m.Service.ReauthorizeResource(ctx, a); err != nil {
		t.Fatalf("reauthorize: %v", err)
	}

	// And the reverse: a v2 resource on the v3 wire.
	cred2 := register(v2Ref)
	_, err = admit(v2Ref, cred2, true)
	is(t, err, runtimeauth.ErrContextVersionMismatch)
	if _, err := admit(v2Ref, cred2, false); err != nil {
		t.Fatalf("the refused v3 admission consumed the v2 credential: %v", err)
	}

	// A stop ends the authority with nothing revoked.
	m.Resources.Stop(ref, false, m.Clock.Now())
	_, err = m.Service.ResourceContextV3(ctx, run, "preparation", tok)
	is(t, err, runtimeauth.ErrResourceRevoked)
}

func provisionRepo(id string, mut func(*gitv1.Repository)) gitv1.Repository {
	return gitauthtest.NewRepository(id, func(r *gitv1.Repository) {
		r.Empty = true
		if mut != nil {
			mut(r)
		}
	})
}

func Test_unit_runtimeauth_git_seed_grant(t *testing.T) {
	e := newV3Env(t, rtest.ProvisionContext)
	ctx := context.Background()
	seed := func(repo string) (runtimev3.GitGrantResponse, error) {
		return e.Service.SeedGitGrant(ctx, e.authorize(), runtimev3.GitSeedRequest{RepositoryID: repo})
	}

	// A repository the run did not create is not found, indistinguishable from an
	// unknown one, and the repository module is not even asked for it.
	e.Git.PutRepository(e.scope, provisionRepo("repo_new", nil))
	e.Git.PutRepository(e.scope, provisionRepo("repo_foreign", nil))
	e.Git.Create(e.scope, "run_other", "repo_foreign")
	for _, id := range []string{"repo_new", "repo_foreign", "repo_unknown"} {
		_, err := seed(id)
		wantRefused(t, err, domain.CodeNotFound)
	}
	if len(e.Git.Issued) != 0 {
		t.Fatal("a grant was issued for a repository the run did not create")
	}

	// The creating run's repository: the eligibility order of the Git contract.
	e.Git.Create(e.scope, "run_01", "repo_new")
	for _, c := range []struct {
		name string
		mut  func(*gitv1.Repository)
		code domain.ErrorCode
	}{
		{"external", func(r *gitv1.Repository) { r.Hosting = gitv1.HostingExternal }, gitv1.CodeHostingMismatch},
		{"archived", func(r *gitv1.Repository) { r.Status = gitv1.StatusArchived }, gitv1.CodeRepositoryArchived},
		{"provisioning", func(r *gitv1.Repository) { r.Status = gitv1.StatusProvisioning }, gitv1.CodeRepositoryNotReady},
		{"not empty", func(r *gitv1.Repository) { r.Empty = false }, gitv1.CodeRepositoryNotEmpty},
	} {
		e.Git.PutRepository(e.scope, provisionRepo("repo_new", c.mut))
		_, err := seed("repo_new")
		wantRefused(t, err, c.code)
	}
	e.Git.PutRepository(e.scope, provisionRepo("repo_new", nil))
	e.Git.SetUnsettled(e.scope, "repo_new", true)
	_, err := seed("repo_new")
	wantRefused(t, err, gitv1.CodeSeedPending)
	e.Git.SetUnsettled(e.scope, "repo_new", false)
	if len(e.Git.Issued) != 0 {
		t.Fatal("an ineligible repository was issued a grant")
	}

	resp, err := seed("repo_new")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	g := resp.Grant
	if g.Kind != gitv1.GrantSeed || len(g.Repositories) != 1 || g.Repositories[0].ID != "repo_new" || !g.Repositories[0].Seed ||
		len(g.Repositories[0].Update) != 1 || g.Repositories[0].Update[0] != "refs/heads/main" || g.Actor.AttemptID == nil || *g.Actor.AttemptID != "att_01" {
		t.Fatalf("grant: %+v", g)
	}
	if resp.CredentialFile != "/kodama/git/seed-credential" || resp.Credential == "" {
		t.Fatalf("delivery: %+v", resp)
	}
	if err := resp.Validate(); err != nil {
		t.Fatalf("the response violates the contract: %v", err)
	}
	// The grant is recorded as the attempt's runtime grant: that is what lets its
	// commit output be recognized later, from the control plane's own records.
	issued, err := e.Service.IssuedGitGrants(ctx, e.scope, "att_01")
	if err != nil || len(issued) != 1 || issued[0].GrantID != g.GrantID {
		t.Fatalf("issued: %+v %v", issued, err)
	}
	if other, _ := e.Service.IssuedGitGrants(ctx, e.scope, "att_other"); len(other) != 0 {
		t.Fatalf("another attempt's records: %+v", other)
	}
	// A repeat before consumption is the same grant.
	again, err := seed("repo_new")
	if err != nil || again.Grant.GrantID != g.GrantID {
		t.Fatalf("repeat: %+v %v", again, err)
	}
	if issued, _ := e.Service.IssuedGitGrants(ctx, e.scope, "att_01"); len(issued) != 1 {
		t.Fatalf("a repeat recorded another grant: %d", len(issued))
	}

	// The credential type prints a redaction in every formatter and encoder.
	cred := runtimeauth.NewGitCredential("gitc_secretsecretsecretsecretsecretsecretsecr")
	for _, rendered := range []string{cred.String(), cred.GoString(), fmt.Sprintf("%v %+v %#v %s", cred, cred, cred, cred)} {
		if strings.Contains(rendered, "secret") {
			t.Fatalf("the credential prints itself: %s", rendered)
		}
	}
	if raw, _ := json.Marshal(struct{ C runtimeauth.GitCredential }{cred}); strings.Contains(string(raw), "secret") {
		t.Fatalf("the credential encodes itself: %s", raw)
	}
}

func Test_unit_runtimeauth_git_seed_needs_the_capability(t *testing.T) {
	// A work-item attempt has no seed capability: forbidden, whatever the request.
	e := newV3Env(t, rtest.WriterContext)
	e.Git.PutRepository(e.scope, provisionRepo("repo_new", nil))
	e.Git.Create(e.scope, "run_01", "repo_new")
	_, err := e.Service.SeedGitGrant(context.Background(), e.authorize(), runtimev3.GitSeedRequest{RepositoryID: "repo_new"})
	wantRefused(t, err, domain.CodeForbidden)
	if len(e.Git.Calls) != 0 {
		t.Fatalf("the repository module was asked: %v", e.Git.Calls)
	}
	// An authorization that is not the attempt's session or wire is refused before
	// anything is read.
	bad := e.authorize()
	bad.ContextVersion = runtimeauth.WireV2
	_, err = e.Service.SeedGitGrant(context.Background(), bad, runtimev3.GitSeedRequest{RepositoryID: "repo_new"})
	is(t, err, runtimeauth.ErrInvalidCredential)
	bad = e.authorize()
	bad.AttemptID = "att_other"
	_, err = e.Service.SeedGitGrant(context.Background(), bad, runtimev3.GitSeedRequest{RepositoryID: "repo_new"})
	if err == nil {
		t.Fatal("an authorization for another attempt was accepted")
	}
}

func Test_unit_runtimeauth_git_integration_grant(t *testing.T) {
	e := newV3Env(t, rtest.WriterContext)
	ctx := context.Background()
	ask := func(repo, gate string) (runtimev3.GitGrantResponse, error) {
		return e.Service.IntegrationGitGrant(ctx, e.authorize(), runtimev3.GitIntegrationRequest{RepositoryID: repo, GateID: gate})
	}
	approval := runtimev3.IntegrationApproval{GateID: "gate_07", RepositoryID: "repo_01", Ref: "refs/heads/main", Base: hexA, Candidate: hexC, CurrentTip: hexA}

	// No approval: forbidden, and no grant exists. A repository without a write
	// source (repo_02 is read-only) is forbidden before the gates are asked.
	_, err := ask("repo_01", "gate_07")
	wantRefused(t, err, domain.CodeForbidden)
	before := len(e.Git.Calls)
	_, err = ask("repo_02", "gate_07")
	wantRefused(t, err, domain.CodeForbidden)
	_, err = ask("repo_unknown", "gate_07")
	wantRefused(t, err, domain.CodeForbidden)
	if len(e.Git.Calls) != before {
		t.Fatalf("the gates were asked for a repository the attempt cannot integrate: %v", e.Git.Calls[before:])
	}
	// An approval for another gate or repository than the request names.
	e.Git.Approve(e.scope, "run_01", "gate_08", "repo_01", approval, nil) // facts name gate_07
	_, err = ask("repo_01", "gate_08")
	wantRefused(t, err, domain.CodeForbidden)

	// A moved base is a publication conflict carrying the superseding tip; an
	// already applied candidate is the same conflict with its reason, and no grant.
	e.Git.Approve(e.scope, "run_01", "gate_07", "repo_01", runtimev3.IntegrationApproval{}, &runtimeauth.ApprovalStale{SupersedingCommit: hexC})
	_, err = ask("repo_01", "gate_07")
	var pc *runtimeauth.PublicationConflict
	if !errors.Is(err, runtimeauth.ErrPublicationConflict) || !errors.As(err, &pc) || pc.Reason != "base_moved" || pc.ObservedTip != hexC {
		t.Fatalf("a moved base: %v", err)
	}
	e.Git.Approve(e.scope, "run_01", "gate_07", "repo_01", runtimev3.IntegrationApproval{}, &runtimeauth.ApprovalApplied{Approval: approval})
	_, err = ask("repo_01", "gate_07")
	if !errors.Is(err, runtimeauth.ErrAlreadyIntegrated) || !errors.As(err, &pc) || pc.ObservedTip != hexC {
		t.Fatalf("already integrated: %v", err)
	}
	e.Git.Approve(e.scope, "run_01", "gate_07", "repo_01", runtimev3.IntegrationApproval{}, errors.New("gates down"))
	_, err = ask("repo_01", "gate_07")
	is(t, err, runtimeauth.ErrUnavailable)
	moved := approval
	moved.CurrentTip = hexC // the tip no longer is the base
	e.Git.Approve(e.scope, "run_01", "gate_07", "repo_01", moved, nil)
	_, err = ask("repo_01", "gate_07")
	is(t, err, runtimeauth.ErrPublicationConflict)
	if len(e.Git.Issued) != 0 {
		t.Fatalf("a grant was issued without a current approval: %+v", e.Git.Issued)
	}

	e.Git.Approve(e.scope, "run_01", "gate_07", "repo_01", approval, nil)
	resp, err := ask("repo_01", "gate_07")
	if err != nil {
		t.Fatalf("integration: %v", err)
	}
	r := resp.Grant.Repositories[0]
	if resp.Grant.Kind != gitv1.GrantIntegration || r.Integration == nil || *r.Integration != (gitv1.IntegrationConstraint{Ref: "refs/heads/main", From: hexA, To: hexC, GateID: "gate_07"}) ||
		len(r.Update) != 1 || r.Update[0] != "refs/heads/main" || resp.CredentialFile != "/kodama/git/integration-credential" {
		t.Fatalf("grant: %+v", resp)
	}
	if err := resp.Validate(); err != nil {
		t.Fatal(err)
	}
	issued, err := e.Service.IssuedGitGrants(ctx, e.scope, "att_01")
	if err != nil || len(issued) != 1 || issued[0].GrantID != resp.Grant.GrantID {
		t.Fatalf("issued: %+v %v", issued, err)
	}
	// The recorded claims cover exactly the one output the contract allows.
	res := runtimev3.ResultV3{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), Outputs: map[string]runtimev1.Value{"report": runtimev1.StringValue("art_0001")}, DiagnosticArtifactIDs: []domain.ArtifactID{},
		Commits: []runtimev3.CommitOutput{{RepositoryID: "repo_01", Ref: "refs/heads/main", Commit: hexC, PushID: "pu_07"}}}
	arts := map[domain.ArtifactID]runtimev1.ArtifactRef{"art_0001": {ID: "art_0001", Scope: e.scope, Attempt: "att_01", Kind: runtimev1.KindFile, Usable: true}}
	if err := runtimev3.CheckResult(res, e.context, arts, issued...); err != nil {
		t.Fatalf("the integration output is not covered: %v", err)
	}
	res.Commits[0].Commit = hexA
	if err := runtimev3.CheckResult(res, e.context, arts, issued...); err == nil {
		t.Fatal("a commit that is not the approved candidate was covered")
	}
}

func ptr[T any](v T) *T { return &v }

// An attempt that ends while its grant is issued gets nothing: the credential is
// not delivered and the attempt's grants are revoked (the issuer reads liveness
// outside the grant's own transaction).
func Test_unit_runtimeauth_git_grant_issuance_race(t *testing.T) {
	e := newV3Env(t, rtest.ProvisionContext)
	ctx := context.Background()
	e.Git.PutRepository(e.scope, provisionRepo("repo_new", nil))
	e.Git.Create(e.scope, "run_01", "repo_new")
	auth := e.authorize()
	e.Git.OnIssue = func() {
		if err := e.Env.Cancel(ctx, e.scope, "att_01", false, e.Clock.Now()); err != nil {
			t.Error(err)
		}
	}
	resp, err := e.Service.SeedGitGrant(ctx, auth, runtimev3.GitSeedRequest{RepositoryID: "repo_new"})
	is(t, err, runtimeauth.ErrAttemptRevoked)
	if resp.Credential != "" {
		t.Fatal("the credential of an ended attempt was delivered")
	}
	if len(e.Git.Revoked) != 1 || e.Git.Revoked[0] != "att_01" {
		t.Fatalf("the grant of the ended attempt was not revoked: %v", e.Git.Revoked)
	}
}

// After the attempt's authority ended (state changed, grants revoked or not) the
// route layer refuses a runtime grant: the session check reads the attempt's own
// state, so revocation having run is not what the refusal depends on.
func Test_unit_runtimeauth_git_grants_refused_after_authority_ended(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		e := newV3Env(t, rtest.ProvisionContext)
		ctx := context.Background()
		e.Git.PutRepository(e.scope, provisionRepo("repo_new", nil))
		e.Git.Create(e.scope, "run_01", "repo_new")
		auth := e.authorize()
		if err := e.Env.Cancel(ctx, e.scope, "att_01", revoke, e.Clock.Now()); err != nil {
			t.Fatal(err)
		}
		_, err := e.Service.SeedGitGrant(ctx, auth, runtimev3.GitSeedRequest{RepositoryID: "repo_new"})
		is(t, err, runtimeauth.ErrAttemptRevoked)
		_, err = e.Service.IntegrationGitGrant(ctx, auth, runtimev3.GitIntegrationRequest{RepositoryID: "repo_new", GateID: "gate_07"})
		is(t, err, runtimeauth.ErrAttemptRevoked)
		if len(e.Git.Issued) != 0 || len(e.Git.Calls) != 0 {
			t.Fatalf("revoke=%v: the Git ports were used after cancellation: %v", revoke, e.Git.Calls)
		}
		_, err = e.Service.Authorize(ctx, runtimeauth.AuthorizeRequest{AttemptID: "att_01", Bearer: e.token, Action: runtimeauth.GitGrantAction, ContextVersion: runtimeauth.WireV3})
		is(t, err, runtimeauth.ErrAttemptRevoked)
	}
}

func Test_unit_runtimeauth_git_not_configured_fails_closed(t *testing.T) {
	e := newV3Env(t, rtest.ProvisionContext)
	// A service without the Git half refuses both operations and the record read.
	svc, err := runtimeauth.NewService(runtimeauth.Config{
		Store: e.Env.Grants, Attempts: e.Env.Attempts, Jobs: e.Jobs, Tokens: e.Signer, Verifier: e.Verifier,
		Wrapper: runtimeauth.ExactWrapperPolicy{Supported: []string{"1.4.0"}}, Policy: rtest.TestPolicy, Clock: e.Clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = svc.SeedGitGrant(ctx, e.authorize(), runtimev3.GitSeedRequest{RepositoryID: "repo_new"})
	is(t, err, runtimeauth.ErrUnavailable)
	_, err = svc.IntegrationGitGrant(ctx, e.authorize(), runtimev3.GitIntegrationRequest{RepositoryID: "repo_new", GateID: "gate_07"})
	is(t, err, runtimeauth.ErrUnavailable)
	_, err = svc.IssuedGitGrants(ctx, e.scope, "att_01")
	is(t, err, runtimeauth.ErrUnavailable)
	if _, err := runtimeauth.NewService(runtimeauth.Config{
		Store: e.Env.Grants, Attempts: e.Env.Attempts, Jobs: e.Jobs, Tokens: e.Signer, Verifier: e.Verifier,
		Wrapper: runtimeauth.ExactWrapperPolicy{Supported: []string{"1.4.0"}}, Policy: rtest.TestPolicy, Git: &runtimeauth.GitConfig{Issuer: e.Git},
	}); err == nil {
		t.Fatal("a Git configuration missing ports was accepted")
	}
}

func Test_unit_runtimeauth_issued_grants_memory_contract(t *testing.T) {
	rtest.IssuedGrantsContract(t, func(t testing.TB) (runtimeauth.IssuedGrants, func(domain.Scope)) {
		return rtest.NewMemIssuedGrants(), func(domain.Scope) {}
	})
}
