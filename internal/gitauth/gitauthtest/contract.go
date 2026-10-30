package gitauthtest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitauth"
	"github.com/optimiware/kodama/internal/identity"
)

var bg = context.Background()

// Scenario is one named behavior every store adapter must show. The names are
// the suffixes of the unit acceptance tests of T-136.
type Scenario struct {
	Name string
	Run  func(t *testing.T, f *Fixture)
}

// Scenarios are the shared scenarios of the grant authority, in order.
func Scenarios() []Scenario {
	return []Scenario{
		{"human_credential_exchange", HumanCredentialExchange},
		{"current_role_on_use", CurrentRoleOnUse},
		{"seed_grant_single_use", SeedGrantSingleUse},
		{"seed_refused_non_empty", SeedRefusedNonEmpty},
		{"attempt_grant_scope", AttemptGrantScope},
		{"integration_grant_binding", IntegrationGrantBinding},
		{"revocation_transactional", RevocationTransactional},
		{"seed_issuance_vs_consumption", SeedIssuanceVersusConsumption},
		{"revoke_all_for_tenant", RevokeAllForTenant},
		{"active_grants_count", ActiveGrantsCount},
		{"tenant_isolation", TenantIsolation},
	}
}

// Run runs one scenario on a fresh environment.
func Run(t *testing.T, open Open, name string) {
	t.Helper()
	for _, s := range Scenarios() {
		if s.Name == name {
			s.Run(t, New(t, open))
			return
		}
	}
	t.Fatalf("no scenario %q", name)
}

// Contract runs every scenario, each on its own environment.
func Contract(t *testing.T, open Open) {
	t.Helper()
	for _, s := range Scenarios() {
		t.Run(s.Name, func(t *testing.T) { s.Run(t, New(t, open)) })
	}
}

// ---- helpers ------------------------------------------------------------------------

func oid(c string) string { return strings.Repeat(c, 40) }

func (f *Fixture) exchange(user identity.UserID, repo string, seed bool) (gitv1.CredentialResponse, error) {
	return f.Svc.Exchange(bg, TenantA, user, gitv1.CredentialRequest{ProjectID: string(ProjectA), RepositoryID: repo, Seed: seed})
}

func (f *Fixture) mustExchange(user identity.UserID, repo string, seed bool) gitv1.CredentialResponse {
	f.T.Helper()
	resp, err := f.exchange(user, repo, seed)
	if err != nil {
		f.T.Fatalf("exchange(%s, %s, seed=%v): %v", user, repo, seed, err)
	}
	if err := resp.Validate(); err != nil {
		f.T.Fatalf("response: %v", err)
	}
	return resp
}

func (f *Fixture) lease(scope domain.Scope, attempt string, live bool, deadline time.Duration) {
	l := gitauth.AttemptLease{Live: live}
	if deadline != 0 {
		l.Deadline = f.Clock.Now().Add(deadline)
	}
	f.Attempts.Set(scope, domain.AttemptID(attempt), l)
}

func (f *Fixture) attemptRequest(kind gitv1.GrantKind, attempt, run string, repos []gitv1.GrantRepository, ttl time.Duration) gitauth.AttemptGrantRequest {
	actor := Workload(attempt, run)
	if kind == gitv1.GrantPreparation {
		actor.AttemptID = nil
		actor.ID = "prep_" + run
	}
	return gitauth.AttemptGrantRequest{Kind: kind, Actor: actor, Repositories: repos, ExpiresAt: f.Clock.Now().Add(ttl)}
}

func agentGrant(repo, workItem, run string, withTask bool) gitv1.GrantRepository {
	pattern, err := gitv1.AgentBranchPattern(workItem, run)
	if err != nil {
		panic(err)
	}
	update := []string{pattern}
	if withTask {
		task, err := gitv1.TaskBranch(workItem)
		if err != nil {
			panic(err)
		}
		update = append(update, task)
	}
	return gitv1.GrantRepository{ID: repo, Read: true, Update: update}
}

func update(ref, old, next string) gitv1.RefUpdate {
	return gitv1.RefUpdate{Ref: ref, Old: old, New: next}
}

// ---- scenarios -------------------------------------------------------------------------

// HumanCredentialExchange: a verified person exchanges the login for a credential
// for exactly one project and repository (GR-05, IT-18).
func HumanCredentialExchange(t *testing.T, f *Fixture) {
	resp := f.mustExchange(Engineer, RepoReady, false)
	if !strings.HasPrefix(resp.Password, "gitc_") || len(resp.Password) != 48 {
		t.Fatalf("credential shape: %d bytes", len(resp.Password))
	}
	if gitauth.SecretBytes < 32 {
		t.Fatal("a credential needs at least 256 bits")
	}
	claims := f.MustVerify(TenantA, resp.Password)
	if err := claims.Validate(); err != nil {
		t.Fatalf("claims: %v", err)
	}
	if claims.Kind != gitv1.GrantHuman || claims.Audience != gitv1.GrantAudience || claims.GrantID != resp.GrantID ||
		claims.TenantID != string(TenantA) || claims.ProjectID != string(ProjectA) ||
		claims.Actor.Type != gitv1.ActorUser || claims.Actor.ID != string(Engineer) || claims.Actor.RunID != nil || claims.Actor.AttemptID != nil {
		t.Fatalf("claims = %+v", claims)
	}
	if len(claims.Repositories) != 1 || claims.Repositories[0].ID != RepoReady || !claims.Repositories[0].Read || claims.Repositories[0].Seed ||
		claims.Repositories[0].Integration != nil || strings.Join(claims.Repositories[0].Update, ",") != strings.Join(gitv1.HumanUpdateCeiling(), ",") {
		t.Fatalf("repositories = %+v", claims.Repositories)
	}
	issued, _ := claims.IssuedAt.Time()
	expires, _ := claims.ExpiresAt.Time()
	if !issued.Equal(f.Clock.Now()) || expires.Sub(issued) != 15*time.Minute || claims.ExpiresAt != resp.ExpiresAt {
		t.Fatalf("lifetime: %s .. %s (%s)", claims.IssuedAt, claims.ExpiresAt, resp.ExpiresAt)
	}
	if err := claims.CheckScope(string(TenantA), string(ProjectA), RepoReady); err != nil {
		t.Fatalf("scope: %v", err)
	}
	for _, other := range [][3]string{{string(TenantB), string(ProjectA), RepoReady}, {string(TenantA), string(ProjectX), RepoReady}, {string(TenantA), string(ProjectA), RepoOther}} {
		WantCode(t, claims.CheckScope(other[0], other[1], other[2]), domain.CodeNotFound)
	}
	if !claims.PermitsUpdate(RepoReady, update("refs/heads/feature", oid("1"), oid("2"))) || !claims.PermitsUpdate(RepoReady, update("refs/tags/v1", oid("0"), oid("2"))) {
		t.Fatal("an engineer's credential must permit ordinary branches and tags within the ceiling")
	}
	if claims.PermitsUpdate(RepoReady, update("refs/kodama/runs/run_1/1", oid("1"), oid("2"))) || claims.PermitsUpdate(RepoOther, update("refs/heads/feature", oid("1"), oid("2"))) {
		t.Fatal("never refs/kodama, never another repository")
	}
	if raw, _ := json.Marshal(claims); strings.Contains(string(raw), resp.Password) {
		t.Fatal("the claims must not contain the secret")
	}
	// Mutating a verified answer never changes the stored grant.
	claims.Repositories[0].Update[0] = "refs/heads/x"
	if again := f.MustVerify(TenantA, resp.Password); again.Repositories[0].Update[0] != "refs/heads/*" {
		t.Fatal("verify returned aliased state")
	}

	// A second exchange is another grant and another secret.
	second := f.mustExchange(Engineer, RepoReady, false)
	if second.Password == resp.Password || second.GrantID == resp.GrantID {
		t.Fatal("each exchange issues a new credential")
	}

	// A person who may only read gets a read-only credential.
	viewer := f.mustExchange(Viewer, RepoReady, false)
	vc := f.MustVerify(TenantA, viewer.Password)
	if len(vc.Repositories[0].Update) != 0 || vc.PermitsUpdate(RepoReady, update("refs/heads/feature", oid("1"), oid("2"))) {
		t.Fatalf("a viewer's credential is read only: %+v", vc.Repositories[0])
	}
	if err := vc.Validate(); err != nil {
		t.Fatalf("read-only claims: %v", err)
	}

	// Refusals: nondisclosing for strangers and foreign objects, explicit for the rest.
	_, err := f.exchange(Outsider, RepoReady, false)
	WantCode(t, err, domain.CodeNotFound)
	_, err = f.exchange(Admin, RepoReady, false)
	WantCode(t, err, domain.CodeForbidden) // admin has no implicit source access
	_, err = f.exchange(Engineer, RepoOther, false)
	WantCode(t, err, domain.CodeNotFound) // another project's repository
	_, err = f.exchange(Engineer, "r136_unknown", false)
	WantCode(t, err, domain.CodeNotFound)
	_, err = f.Svc.Exchange(bg, TenantB, Engineer, gitv1.CredentialRequest{ProjectID: string(ProjectA), RepositoryID: RepoReady})
	WantCode(t, err, domain.CodeNotFound) // the engineer of tenant B has no role in tenant A's project
	_, err = f.Svc.Exchange(bg, TenantA, Engineer, gitv1.CredentialRequest{ProjectID: "bad id", RepositoryID: RepoReady})
	WantCode(t, err, domain.CodeInvalidRequest)
	_, err = f.exchange(Engineer, RepoExternal, false)
	WantCode(t, err, gitv1.CodeHostingMismatch)
	_, err = f.exchange(Engineer, RepoArchived, false)
	WantCode(t, err, gitv1.CodeRepositoryArchived)
	_, err = f.exchange(Engineer, RepoBooting, false)
	WantCode(t, err, gitv1.CodeRepositoryNotReady)
	f.Repos.Fail = errors.New("repository module down")
	_, err = f.exchange(Engineer, RepoReady, false)
	if !errors.Is(err, gitauth.ErrUnavailable) {
		t.Fatalf("a dependency outage is unavailable, not access: %v", err)
	}
	f.Repos.Fail = nil
	if n := f.Env.GrantCount(t, TenantA); n != 3 {
		t.Fatalf("refusals stored grants: %d", n)
	}

	// Another tenant, a missing credential, garbage and tokens are the same answer.
	for _, c := range []string{"", "x", "gitc_", "gitc_" + strings.Repeat("A", 43), strings.Repeat("a", 20) + "." + strings.Repeat("b", 20) + "." + strings.Repeat("c", 20),
		resp.Password + "x", strings.ToUpper(resp.Password)} {
		_, err := f.Svc.Verify(bg, TenantA, c)
		WantInvalid(t, err)
	}
	_, err = f.Svc.Verify(bg, TenantB, resp.Password)
	WantInvalid(t, err)
	_, err = f.Svc.Verify(bg, "bad tenant", resp.Password)
	WantInvalid(t, err)
	if _, err := f.Svc.VerifyScoped(bg, TenantA, resp.Password, string(ProjectX), RepoReady); !gitauth.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("a credential of another project is not_found: %v", err)
	}
	if _, err := f.Svc.VerifyScoped(bg, TenantA, resp.Password, string(ProjectA), RepoReady); err != nil {
		t.Fatalf("scoped: %v", err)
	}

	// Lifetime.
	f.Clock.Advance(15*time.Minute - time.Second)
	f.MustVerify(TenantA, resp.Password)
	f.Clock.Advance(time.Second)
	_, err = f.Svc.Verify(bg, TenantA, resp.Password)
	WantInvalid(t, err)
}

// CurrentRoleOnUse: the person's current project roles decide at every use, so a
// removed or narrowed role takes effect on the next request even while the
// credential is unexpired (IT-18, GR-A04).
func CurrentRoleOnUse(t *testing.T, f *Fixture) {
	eng := f.mustExchange(Engineer, RepoReady, false)
	view := f.mustExchange(Viewer, RepoReady, false)
	canPush := func(cred string) bool {
		t.Helper()
		c, err := f.Svc.Verify(bg, TenantA, cred)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		return c.PermitsUpdate(RepoReady, update("refs/heads/feature", oid("1"), oid("2")))
	}
	if !canPush(eng.Password) || canPush(view.Password) {
		t.Fatal("the engineer pushes, the viewer does not")
	}

	// Demoted to viewer: still reads, no longer pushes, with the same credential.
	f.Roles.Set(TenantA, ProjectA, Engineer, identity.RoleViewer)
	if canPush(eng.Password) {
		t.Fatal("a demoted engineer must not push with an unexpired credential")
	}
	if c := f.MustVerify(TenantA, eng.Password); len(c.Repositories[0].Update) != 0 || c.Validate() != nil {
		t.Fatalf("narrowed claims = %+v", c.Repositories)
	}
	// Restored: the recorded ceiling applies again (it was never rewritten).
	f.Roles.Set(TenantA, ProjectA, Engineer, identity.RoleEngineer)
	if !canPush(eng.Password) {
		t.Fatal("the ceiling is recorded, the roles are current")
	}
	// A credential issued read-only never becomes a push credential.
	f.Roles.Set(TenantA, ProjectA, Viewer, identity.RoleEngineer)
	if canPush(view.Password) {
		t.Fatal("a read-only credential is never widened by a later role")
	}
	f.Roles.Set(TenantA, ProjectA, Viewer, identity.RoleViewer)

	// Role removed, or a role that cannot read source: the credential is refused.
	f.Roles.Set(TenantA, ProjectA, Engineer, identity.RoleAdmin)
	_, err := f.Svc.Verify(bg, TenantA, eng.Password)
	WantInvalid(t, err)
	f.Roles.Remove(TenantA, ProjectA, Engineer)
	_, err = f.Svc.Verify(bg, TenantA, eng.Password)
	WantInvalid(t, err)
	f.Roles.Set(TenantA, ProjectA, Engineer, identity.RoleEngineer)
	if !canPush(eng.Password) {
		t.Fatal("restored")
	}

	// An outage of the authority is never access.
	f.Roles.Fail = errors.New("identity down")
	_, err = f.Svc.Verify(bg, TenantA, eng.Password)
	if !errors.Is(err, gitauth.ErrUnavailable) {
		t.Fatalf("outage: %v", err)
	}
	f.Roles.Fail = nil

	// The binding of the same user ID in another tenant is irrelevant.
	f.Roles.Remove(TenantA, ProjectA, Engineer)
	_, err = f.Svc.Verify(bg, TenantA, eng.Password)
	WantInvalid(t, err)
	f.Roles.Set(TenantA, ProjectA, Engineer, identity.RoleEngineer)

	// A person's seed credential needs a current engineer.
	seed := f.mustExchange(Engineer, RepoEmpty, true)
	f.MustVerify(TenantA, seed.Password)
	f.Roles.Set(TenantA, ProjectA, Engineer, identity.RoleViewer)
	_, err = f.Svc.Verify(bg, TenantA, seed.Password)
	WantInvalid(t, err)
}

// SeedGrantSingleUse: the single-use seed grant of an empty repository, engineer
// only, consumed by the one receive that authorizes it (GR-18, S8).
func SeedGrantSingleUse(t *testing.T, f *Fixture) {
	resp := f.mustExchange(Engineer, RepoEmpty, true)
	claims := f.MustVerify(TenantA, resp.Password)
	if claims.Kind != gitv1.GrantSeed || !claims.Repositories[0].Seed || len(claims.Repositories[0].Update) != 1 || claims.Repositories[0].Update[0] != "refs/heads/main" {
		t.Fatalf("seed claims = %+v", claims)
	}
	if err := claims.CheckSeed("main"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	issued, _ := claims.IssuedAt.Time()
	expires, _ := claims.ExpiresAt.Time()
	if expires.Sub(issued) > gitv1.MaxSeedLifetime {
		t.Fatalf("seed lifetime %s", expires.Sub(issued))
	}
	if !claims.PermitsUpdate(RepoEmpty, update("refs/heads/main", oid("0"), oid("2"))) || claims.PermitsUpdate(RepoEmpty, update("refs/heads/other", oid("0"), oid("2"))) {
		t.Fatal("a seed permits creating the default branch only")
	}

	// The seed is consumed when its receive is authorized.
	if err := f.Svc.ConsumeSeed(bg, TenantA, resp.GrantID, "rcv_1"); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if err := f.Svc.ConsumeSeed(bg, TenantA, resp.GrantID, "rcv_1"); err != nil {
		t.Fatalf("a replayed authorization of the same receive: %v", err)
	}
	if err := f.Svc.ConsumeSeed(bg, TenantA, resp.GrantID, "rcv_2"); !errors.Is(err, gitauth.ErrSeedConsumed) {
		t.Fatalf("another receive: %v", err)
	}
	// Still resolvable (the push authorizer refuses it as seed_consumed), and
	// still the same replay after it expired.
	f.MustVerify(TenantA, resp.Password)
	f.Clock.Advance(time.Hour)
	if err := f.Svc.ConsumeSeed(bg, TenantA, resp.GrantID, "rcv_1"); err != nil {
		t.Fatalf("an authorized receive replays after the grant expired: %v", err)
	}
	if err := f.Svc.ConsumeSeed(bg, TenantA, resp.GrantID, "rcv_2"); !errors.Is(err, gitauth.ErrSeedConsumed) {
		t.Fatalf("after expiry: %v", err)
	}
	f.Clock.Advance(-time.Hour)

	// The seed settled refused and the repository is still empty: a new seed.
	again := f.mustExchange(Engineer, RepoEmpty, true)
	if again.Password == resp.Password {
		t.Fatal("a new seed is a new credential")
	}
	// A seed asked again before use replaces the earlier unused one.
	third := f.mustExchange(Engineer, RepoEmpty, true)
	_, err := f.Svc.Verify(bg, TenantA, again.Password)
	WantInvalid(t, err)
	f.MustVerify(TenantA, third.Password)
	if err := f.Svc.ConsumeSeed(bg, TenantA, again.GrantID, "rcv_3"); !errors.Is(err, gitauth.ErrGrantInactive) {
		t.Fatalf("a revoked seed: %v", err)
	}
	if err := f.Svc.ConsumeSeed(bg, TenantA, third.GrantID, "rcv_3"); err != nil {
		t.Fatal(err)
	}

	// Who may not: a viewer, a stranger; only a seed is consumed; unknown grants.
	_, err = f.exchange(Viewer, RepoEmpty, true)
	WantCode(t, err, domain.CodeForbidden)
	_, err = f.exchange(Outsider, RepoEmpty, true)
	WantCode(t, err, domain.CodeNotFound)
	human := f.mustExchange(Engineer, RepoReady, false)
	WantCode(t, f.Svc.ConsumeSeed(bg, TenantA, human.GrantID, "rcv_9"), domain.CodeInvalidRequest)
	WantCode(t, f.Svc.ConsumeSeed(bg, TenantA, "gg_unknown", "rcv_9"), domain.CodeNotFound)
	WantCode(t, f.Svc.ConsumeSeed(bg, TenantA, third.GrantID, "bad receive"), domain.CodeInvalidRequest)

	// Expiry: an unused seed that expired authorizes nothing.
	old := f.mustExchange(Engineer, RepoEmpty, true)
	f.Clock.Advance(gitv1.MaxSeedLifetime)
	_, err = f.Svc.Verify(bg, TenantA, old.Password)
	WantInvalid(t, err)
	if err := f.Svc.ConsumeSeed(bg, TenantA, old.GrantID, "rcv_10"); !errors.Is(err, gitauth.ErrGrantInactive) {
		t.Fatalf("an expired seed: %v", err)
	}
	f.Clock.Advance(-gitv1.MaxSeedLifetime)

	// A provisioning run's attempt is issued a seed under the same rules.
	f.lease(ScopeA(), "att_prov", true, time.Hour)
	req := f.attemptRequest(gitv1.GrantSeed, "att_prov", "run_prov",
		[]gitv1.GrantRepository{{ID: RepoEmpty, Read: true, Update: []string{"refs/heads/main"}, Seed: true}}, 10*time.Minute)
	h, err := f.Svc.IssueAttempt(bg, ScopeA(), req)
	if err != nil {
		t.Fatalf("provisioning seed: %v", err)
	}
	pc := f.MustVerify(TenantA, h.Credential.Reveal())
	if pc.Kind != gitv1.GrantSeed || pc.Actor.Type != gitv1.ActorWorkload || pc.Actor.ID != "att_prov" || pc.CheckSeed("main") != nil {
		t.Fatalf("provisioning seed claims = %+v", pc)
	}
	bad := req
	bad.Actor = Workload("att_prov2", "run_prov")
	bad.Repositories = []gitv1.GrantRepository{{ID: RepoEmpty, Read: true, Update: []string{"refs/heads/trunk"}, Seed: true}}
	f.lease(ScopeA(), "att_prov2", true, time.Hour)
	_, err = f.Svc.IssueAttempt(bg, ScopeA(), bad)
	WantCode(t, err, domain.CodeInvalidRequest) // not the repository's default branch
	f.lease(ScopeA(), "att_prov", false, time.Hour)
	_, err = f.Svc.Verify(bg, TenantA, h.Credential.Reveal())
	WantInvalid(t, err) // the attempt ended

	concurrentSeed(t, f)
}

// concurrentSeed: concurrent requests are decided one after the other. At most
// one receive consumes a seed, and of concurrent issuances only the last
// credential remains usable.
func concurrentSeed(t *testing.T, f *Fixture) {
	t.Helper()
	const n = 12
	f.Repos.Put(ScopeA(), NewRepository("r136_race", func(r *gitv1.Repository) { r.Empty = true }))
	grant := f.mustExchangeRepo(Engineer, "r136_race", true)
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = f.Svc.ConsumeSeed(bg, TenantA, grant.GrantID, "rcv_race_"+string(rune('a'+i)))
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, gitauth.ErrSeedConsumed):
		default:
			t.Fatalf("concurrent consume: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d receives consumed one seed", won)
	}

	f.Repos.Put(ScopeA(), NewRepository("r136_race2", func(r *gitv1.Repository) { r.Empty = true }))
	creds := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := f.exchange(Engineer, "r136_race2", true)
			errs[i], creds[i] = err, resp.Password
		}()
	}
	wg.Wait()
	usable := 0
	for i := range creds {
		if errs[i] != nil {
			t.Fatalf("concurrent issue: %v", errs[i])
		}
		if _, err := f.Svc.Verify(bg, TenantA, creds[i]); err == nil {
			usable++
		}
	}
	if usable != 1 {
		t.Fatalf("%d concurrently issued seed credentials are usable, want 1", usable)
	}
}

func (f *Fixture) mustExchangeRepo(user identity.UserID, repo string, seed bool) gitv1.CredentialResponse {
	return f.mustExchange(user, repo, seed)
}

// SeedRefusedNonEmpty: a seed is refused unless the repository is internal,
// ready, empty and has no unsettled push of any kind; the refusal stores nothing.
func SeedRefusedNonEmpty(t *testing.T, f *Fixture) {
	before := f.Env.GrantCount(t, TenantA)
	for _, c := range []struct {
		name string
		repo string
		code domain.ErrorCode
	}{
		{"refs exist", RepoReady, gitv1.CodeRepositoryNotEmpty},
		{"external", RepoExternal, gitv1.CodeHostingMismatch},
		{"archived", RepoArchived, gitv1.CodeRepositoryArchived},
		{"provisioning", RepoBooting, gitv1.CodeRepositoryNotReady},
		{"another project", RepoOther, domain.CodeNotFound},
		{"another tenant", RepoB, domain.CodeNotFound},
		{"unknown", "r136_unknown", domain.CodeNotFound},
	} {
		_, err := f.exchange(Engineer, c.repo, true)
		if !gitauth.IsCode(err, c.code) {
			t.Fatalf("%s: %v, want %s", c.name, err, c.code)
		}
	}
	if status, _ := gitv1.Status(gitv1.CodeRepositoryNotEmpty); status != 409 {
		t.Fatalf("repository_not_empty is a 409, not %d", status)
	}
	// Any unsettled push of any kind blocks: authorized (in flight) or unknown.
	f.Pushes.SetUnsettled(ScopeA(), RepoEmpty, true)
	_, err := f.exchange(Engineer, RepoEmpty, true)
	WantCode(t, err, gitv1.CodeSeedPending)
	// ... and the role is checked first: a viewer learns nothing about the state.
	_, err = f.exchange(Viewer, RepoEmpty, true)
	WantCode(t, err, domain.CodeForbidden)
	_, err = f.exchange(Viewer, RepoReady, true)
	WantCode(t, err, domain.CodeForbidden)
	// Provisioning runs follow the same rules.
	f.lease(ScopeA(), "att_p", true, time.Hour)
	seedReq := f.attemptRequest(gitv1.GrantSeed, "att_p", "run_p",
		[]gitv1.GrantRepository{{ID: RepoEmpty, Read: true, Update: []string{"refs/heads/main"}, Seed: true}}, 10*time.Minute)
	_, err = f.Svc.IssueAttempt(bg, ScopeA(), seedReq)
	WantCode(t, err, gitv1.CodeSeedPending)
	f.Pushes.SetUnsettled(ScopeA(), RepoEmpty, false)
	// Dependency outages are never an issued seed.
	f.Pushes.Fail = errors.New("push records unavailable")
	_, err = f.exchange(Engineer, RepoEmpty, true)
	if !errors.Is(err, gitauth.ErrUnavailable) {
		t.Fatalf("push records outage: %v", err)
	}
	f.Pushes.Fail = nil
	nonEmpty := seedReq
	nonEmpty.Actor = Workload("att_q", "run_q")
	nonEmpty.Repositories = []gitv1.GrantRepository{{ID: RepoReady, Read: true, Update: []string{"refs/heads/main"}, Seed: true}}
	f.lease(ScopeA(), "att_q", true, time.Hour)
	_, err = f.Svc.IssueAttempt(bg, ScopeA(), nonEmpty)
	WantCode(t, err, gitv1.CodeRepositoryNotEmpty)
	if after := f.Env.GrantCount(t, TenantA); after != before {
		t.Fatalf("refused seeds stored %d grants", after-before)
	}
	// With the facts in order the seed is issued.
	f.mustExchange(Engineer, RepoEmpty, true)
	// The repository's default branch decides the seed's ref.
	f.Repos.Put(ScopeA(), NewRepository(RepoEmpty, func(r *gitv1.Repository) { r.Empty = true; r.DefaultBranch = "trunk" }))
	resp := f.mustExchange(Engineer, RepoEmpty, true)
	if c := f.MustVerify(TenantA, resp.Password); c.CheckSeed("trunk") != nil {
		t.Fatalf("seed ref = %v", c.Repositories[0].Update)
	}
}

// AttemptGrantScope: preparation and attempt grants issued by execution carry
// exactly the admitted repositories and patterns, are bound to the attempt's
// liveness and lease, and are idempotent per attempt (GR-06, S5).
func AttemptGrantScope(t *testing.T, f *Fixture) {
	f.lease(ScopeA(), "att_1", true, time.Hour)
	repos := []gitv1.GrantRepository{agentGrant(RepoReady, "wi_1042", "run_1", true)}
	req := f.attemptRequest(gitv1.GrantAttempt, "att_1", "run_1", repos, 30*time.Minute)
	h, err := f.Svc.IssueAttempt(bg, ScopeA(), req)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if h.GrantID == "" || h.Credential.IsZero() || !h.ExpiresAt.Equal(req.ExpiresAt) || strings.Contains(h.Credential.String()+h.Credential.GoString(), "gitc_") {
		t.Fatalf("handle = %+v", h)
	}
	claims := f.MustVerify(TenantA, h.Credential.Reveal())
	if err := claims.Validate(); err != nil {
		t.Fatal(err)
	}
	if claims.Kind != gitv1.GrantAttempt || claims.Actor.ID != "att_1" || *claims.Actor.AttemptID != "att_1" || *claims.Actor.RunID != "run_1" ||
		claims.ProjectID != string(ProjectA) || len(claims.Repositories) != 1 {
		t.Fatalf("claims = %+v", claims)
	}
	agentRef, _ := gitv1.AgentBranch("wi_1042", "run_1", "impl")
	taskRef, _ := gitv1.TaskBranch("wi_1042")
	for ref, want := range map[string]bool{
		agentRef: true, taskRef: true, "refs/heads/main": false, "refs/tags/v1": false, "refs/kodama/runs/run_1/1": false,
		"refs/heads/kodama/agents/wi_1042/run_2/impl": false, "refs/heads/kodama/work/wi_9": false,
	} {
		if got := claims.PermitsUpdate(RepoReady, update(ref, oid("1"), oid("2"))); got != want {
			t.Errorf("PermitsUpdate(%s) = %v, want %v", ref, got, want)
		}
	}
	for _, other := range [][3]string{{string(TenantB), string(ProjectA), RepoReady}, {string(TenantA), string(ProjectX), RepoReady}, {string(TenantA), string(ProjectA), RepoEmpty}} {
		WantCode(t, claims.CheckScope(other[0], other[1], other[2]), domain.CodeNotFound)
	}
	if got, err := f.Svc.Claims(bg, TenantA, h.GrantID); err != nil || got.GrantID != h.GrantID {
		t.Fatalf("claims by ID: %+v %v", got, err)
	}

	// Idempotent per attempt: the same grant with a fresh secret; the first
	// secret was replaced (only digests are stored).
	again, err := f.Svc.IssueAttempt(bg, ScopeA(), req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if again.GrantID != h.GrantID || again.Credential.Reveal() == h.Credential.Reveal() || !again.ExpiresAt.Equal(h.ExpiresAt) {
		t.Fatalf("replay = %+v", again)
	}
	_, err = f.Svc.Verify(bg, TenantA, h.Credential.Reveal())
	WantInvalid(t, err)
	f.MustVerify(TenantA, again.Credential.Reveal())
	changed := req
	changed.Repositories = []gitv1.GrantRepository{agentGrant(RepoReady, "wi_1042", "run_1", false)}
	_, err = f.Svc.IssueAttempt(bg, ScopeA(), changed)
	WantCode(t, err, domain.CodeIdempotencyConflict)

	// Tenant B's attempt with the same ID is another attempt.
	f.lease(ScopeB(), "att_1", true, time.Hour)
	bReq := f.attemptRequest(gitv1.GrantAttempt, "att_1", "run_1", []gitv1.GrantRepository{agentGrant(RepoB, "wi_1", "run_1", true)}, 30*time.Minute)
	bh, err := f.Svc.IssueAttempt(bg, ScopeB(), bReq)
	if err != nil || bh.GrantID == h.GrantID {
		t.Fatalf("tenant B: %+v %v", bh, err)
	}
	_, err = f.Svc.Verify(bg, TenantA, bh.Credential.Reveal())
	WantInvalid(t, err)
	_, err = f.Svc.Verify(bg, TenantB, again.Credential.Reveal())
	WantInvalid(t, err)
	f.MustVerify(TenantB, bh.Credential.Reveal())

	// Liveness at every use, revoked or not.
	f.lease(ScopeA(), "att_1", false, time.Hour) // cancelled, grants not revoked yet
	_, err = f.Svc.Verify(bg, TenantA, again.Credential.Reveal())
	WantInvalid(t, err)
	_, err = f.Svc.Claims(bg, TenantA, h.GrantID)
	WantInvalid(t, err)
	f.lease(ScopeA(), "att_1", true, 10*time.Minute)
	f.MustVerify(TenantA, again.Credential.Reveal())
	f.Clock.Advance(10 * time.Minute) // the attempt's lease ended
	_, err = f.Svc.Verify(bg, TenantA, again.Credential.Reveal())
	WantInvalid(t, err)
	f.Clock.Advance(-10 * time.Minute)
	f.Attempts.Fail = errors.New("execution down")
	_, err = f.Svc.Verify(bg, TenantA, again.Credential.Reveal())
	if !errors.Is(err, gitauth.ErrUnavailable) {
		t.Fatalf("an execution outage is never access: %v", err)
	}
	f.Attempts.Fail = nil

	// Issuance is bounded by the attempt and the contract.
	f.lease(ScopeA(), "att_2", true, 20*time.Minute)
	long := f.attemptRequest(gitv1.GrantAttempt, "att_2", "run_2", repos, 30*time.Minute)
	_, err = f.Svc.IssueAttempt(bg, ScopeA(), long)
	WantCode(t, err, domain.CodeInvalidRequest) // beyond the attempt's lease
	f.lease(ScopeA(), "att_3", true, 0)
	huge := f.attemptRequest(gitv1.GrantAttempt, "att_3", "run_3", repos, 25*time.Hour)
	_, err = f.Svc.IssueAttempt(bg, ScopeA(), huge)
	WantCode(t, err, domain.CodeInvalidRequest) // beyond the contract's workload lifetime
	f.lease(ScopeA(), "att_4", false, time.Hour)
	_, err = f.Svc.IssueAttempt(bg, ScopeA(), f.attemptRequest(gitv1.GrantAttempt, "att_4", "run_4", repos, time.Minute))
	WantCode(t, err, gitv1.CodeAttemptInactive)
	_, err = f.Svc.IssueAttempt(bg, ScopeA(), f.attemptRequest(gitv1.GrantAttempt, "att_unknown", "run_4", repos, time.Minute))
	WantCode(t, err, domain.CodeNotFound)

	// Repositories must belong to the project and be servable.
	f.lease(ScopeA(), "att_5", true, time.Hour)
	for _, c := range []struct {
		repo string
		code domain.ErrorCode
	}{{RepoOther, domain.CodeNotFound}, {RepoB, domain.CodeNotFound}, {RepoExternal, gitv1.CodeHostingMismatch},
		{RepoArchived, gitv1.CodeRepositoryArchived}, {RepoBooting, gitv1.CodeRepositoryNotReady}} {
		_, err = f.Svc.IssueAttempt(bg, ScopeA(), f.attemptRequest(gitv1.GrantAttempt, "att_5", "run_5", []gitv1.GrantRepository{agentGrant(c.repo, "wi_1", "run_5", false)}, time.Minute))
		WantCode(t, err, c.code)
	}
	// The contract's shapes are enforced: an attempt grant never names a bare
	// wildcard, refs/kodama or the default branch pattern syntax.
	for _, bad := range []string{"refs/heads/*", "refs/kodama/runs/*", "refs/heads/a/*/b", "main"} {
		_, err = f.Svc.IssueAttempt(bg, ScopeA(), f.attemptRequest(gitv1.GrantAttempt, "att_5", "run_5", []gitv1.GrantRepository{{ID: RepoReady, Read: true, Update: []string{bad}}}, time.Minute))
		WantCode(t, err, domain.CodeInvalidRequest)
	}
	_, err = f.Svc.IssueAttempt(bg, ScopeA(), gitauth.AttemptGrantRequest{Kind: gitv1.GrantHuman, Actor: Workload("att_5", "run_5"), Repositories: repos, ExpiresAt: f.Clock.Now().Add(time.Minute)})
	WantCode(t, err, domain.CodeInvalidRequest) // execution never issues human or integration grants
	_, err = f.Svc.IssueAttempt(bg, ScopeA(), gitauth.AttemptGrantRequest{Kind: gitv1.GrantAttempt, Actor: gitv1.Actor{Type: gitv1.ActorUser, ID: "u"}, Repositories: repos, ExpiresAt: f.Clock.Now().Add(time.Minute)})
	WantCode(t, err, domain.CodeInvalidRequest)

	// A preparation grant is read only and belongs to no attempt.
	prep := f.attemptRequest(gitv1.GrantPreparation, "", "run_p", []gitv1.GrantRepository{{ID: RepoReady, Read: true, Update: []string{}}}, time.Hour)
	ph, err := f.Svc.IssueAttempt(bg, ScopeA(), prep)
	if err != nil {
		t.Fatalf("preparation: %v", err)
	}
	pc := f.MustVerify(TenantA, ph.Credential.Reveal())
	if pc.Kind != gitv1.GrantPreparation || pc.Actor.AttemptID != nil || pc.PermitsUpdate(RepoReady, update("refs/heads/x", oid("1"), oid("2"))) {
		t.Fatalf("preparation claims = %+v", pc)
	}
	writing := prep
	writing.Actor.ID = "prep_other"
	writing.Repositories = []gitv1.GrantRepository{{ID: RepoReady, Read: true, Update: []string{"refs/heads/x"}}}
	_, err = f.Svc.IssueAttempt(bg, ScopeA(), writing)
	WantCode(t, err, domain.CodeInvalidRequest)
	got, err := f.Svc.IssueAttempt(bg, ScopeA(), prep)
	if err != nil || got.GrantID != ph.GrantID {
		t.Fatalf("preparation replay: %+v %v", got, err)
	}
	f.MustVerify(TenantA, got.Credential.Reveal())
	f.Clock.Advance(time.Hour)
	_, err = f.Svc.Verify(bg, TenantA, got.Credential.Reveal())
	WantInvalid(t, err)
	if _, err := f.Svc.IssueAttempt(bg, ScopeA(), prep); err == nil {
		t.Fatal("an expired grant is not revived by a retry")
	}
}

// IntegrationGrantBinding: an integration grant is bound to one approved gate
// decision and permits exactly one update of the default branch (GR-12).
func IntegrationGrantBinding(t *testing.T, f *Fixture) {
	c := gitv1.IntegrationConstraint{Ref: "refs/heads/main", From: oid("2"), To: oid("3"), GateID: "gate_07"}
	approve := func(mutate func(*gitauth.GateDecision)) {
		d := gitauth.GateDecision{Approved: true, Current: true, RunID: "run_i", AttemptID: "att_i", RepositoryID: RepoReady, Base: c.From, Candidate: c.To}
		if mutate != nil {
			mutate(&d)
		}
		f.Gates.Set(ScopeA(), c.GateID, d)
	}
	issue := func() (gitauth.GrantHandle, error) {
		return f.Svc.IssueIntegration(bg, ScopeA(), "att_i", RepoReady, c)
	}
	f.lease(ScopeA(), "att_i", true, 0)
	approve(nil)
	h, err := issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	claims := f.MustVerify(TenantA, h.Credential.Reveal())
	if err := claims.Validate(); err != nil {
		t.Fatal(err)
	}
	r := claims.Repositories[0]
	if claims.Kind != gitv1.GrantIntegration || len(claims.Repositories) != 1 || len(r.Update) != 1 || r.Update[0] != c.Ref || r.Integration == nil || *r.Integration != c ||
		*claims.Actor.AttemptID != "att_i" || *claims.Actor.RunID != "run_i" {
		t.Fatalf("claims = %+v", claims)
	}
	expires, _ := claims.ExpiresAt.Time()
	if want := f.Clock.Now().Add(30 * time.Minute); !expires.Equal(want) {
		t.Fatalf("expires %s, want %s", expires, want)
	}
	for name, u := range map[string]gitv1.RefUpdate{
		"the approved update": update(c.Ref, c.From, c.To),
	} {
		if !claims.PermitsUpdate(RepoReady, u) {
			t.Errorf("%s must be permitted", name)
		}
	}
	for name, u := range map[string]gitv1.RefUpdate{
		"another commit":  update(c.Ref, c.From, oid("4")),
		"another base":    update(c.Ref, oid("5"), c.To),
		"another ref":     update("refs/heads/release", c.From, c.To),
		"a task branch":   update("refs/heads/kodama/work/wi_1", oid("0"), c.To),
		"a delete":        update(c.Ref, c.From, oid("0")),
		"the seed create": update(c.Ref, oid("0"), c.To),
	} {
		if claims.PermitsUpdate(RepoReady, u) {
			t.Errorf("%s must not be permitted", name)
		}
	}
	if claims.PermitsUpdate(RepoEmpty, update(c.Ref, c.From, c.To)) {
		t.Error("another repository")
	}

	// Replay: the same grant, a fresh secret.
	again, err := issue()
	if err != nil || again.GrantID != h.GrantID || again.Credential.Reveal() == h.Credential.Reveal() {
		t.Fatalf("replay = %+v %v", again, err)
	}
	_, err = f.Svc.Verify(bg, TenantA, h.Credential.Reveal())
	WantInvalid(t, err)

	// A moved target is a new constraint, never an edit of the issued grant.
	moved := c
	moved.To = oid("4")
	approve(func(d *gitauth.GateDecision) { d.Candidate = moved.To })
	_, err = f.Svc.IssueIntegration(bg, ScopeA(), "att_i", RepoReady, moved)
	WantCode(t, err, domain.CodeIdempotencyConflict)
	f.MustVerify(TenantA, again.Credential.Reveal())

	// Refusals: the decision must be approved, current and name this update.
	for name, mutate := range map[string]func(*gitauth.GateDecision){
		"not approved":       func(d *gitauth.GateDecision) { d.Approved = false },
		"superseded":         func(d *gitauth.GateDecision) { d.Current = false },
		"another candidate":  func(d *gitauth.GateDecision) { d.Candidate = oid("9") },
		"another base":       func(d *gitauth.GateDecision) { d.Base = oid("8") },
		"another repository": func(d *gitauth.GateDecision) { d.RepositoryID = RepoEmpty },
		"another attempt":    func(d *gitauth.GateDecision) { d.AttemptID = "att_j" },
	} {
		approve(mutate)
		_, err := f.Svc.IssueIntegration(bg, ScopeA(), "att_i", RepoReady, gitv1.IntegrationConstraint{Ref: "refs/heads/main", From: oid("2"), To: oid("3"), GateID: "gate_07"})
		if !gitauth.IsCode(err, domain.CodeForbidden) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	approve(nil)
	_, err = f.Svc.IssueIntegration(bg, ScopeA(), "att_i", RepoReady, gitv1.IntegrationConstraint{Ref: "refs/heads/main", From: oid("2"), To: oid("3"), GateID: "gate_unknown"})
	WantCode(t, err, domain.CodeNotFound)
	f.Gates.Set(ScopeX(), "gate_foreign", gitauth.GateDecision{Approved: true, Current: true, RunID: "r", AttemptID: "att_i", RepositoryID: RepoReady, Base: c.From, Candidate: c.To})
	_, err = f.Svc.IssueIntegration(bg, ScopeA(), "att_i", RepoReady, gitv1.IntegrationConstraint{Ref: "refs/heads/main", From: c.From, To: c.To, GateID: "gate_foreign"})
	WantCode(t, err, domain.CodeNotFound) // a gate of another project
	notDefault := c
	notDefault.Ref = "refs/heads/release"
	notDefault.GateID = "gate_08"
	f.Gates.Set(ScopeA(), "gate_08", gitauth.GateDecision{Approved: true, Current: true, RunID: "run_i", AttemptID: "att_i", RepositoryID: RepoReady, Base: c.From, Candidate: c.To})
	_, err = f.Svc.IssueIntegration(bg, ScopeA(), "att_i", RepoReady, notDefault)
	WantCode(t, err, domain.CodeForbidden) // only the default branch
	badShape := c
	badShape.From = c.To
	_, err = f.Svc.IssueIntegration(bg, ScopeA(), "att_i", RepoReady, badShape)
	WantCode(t, err, domain.CodeInvalidRequest)
	_, err = f.Svc.IssueIntegration(bg, ScopeA(), "att_i", RepoArchived, c)
	WantCode(t, err, domain.CodeForbidden) // the gate's subject names another repository
	f.Gates.Fail = errors.New("gates down")
	_, err = f.Svc.IssueIntegration(bg, ScopeA(), "att_i", RepoReady, c)
	if !errors.Is(err, gitauth.ErrUnavailable) {
		t.Fatalf("gates outage: %v", err)
	}
	f.Gates.Fail = nil

	// The attempt must be live; its lease bounds the grant.
	f.lease(ScopeA(), "att_i", false, 0)
	g2 := c
	g2.GateID = "gate_09"
	f.Gates.Set(ScopeA(), "gate_09", gitauth.GateDecision{Approved: true, Current: true, RunID: "run_i", AttemptID: "att_i", RepositoryID: RepoReady, Base: c.From, Candidate: c.To})
	_, err = f.Svc.IssueIntegration(bg, ScopeA(), "att_i", RepoReady, g2)
	WantCode(t, err, gitv1.CodeAttemptInactive)
	_, err = f.Svc.Verify(bg, TenantA, again.Credential.Reveal())
	WantInvalid(t, err) // the attempt ended: the issued grant stops working
	f.lease(ScopeA(), "att_i", true, 5*time.Minute)
	short, err := f.Svc.IssueIntegration(bg, ScopeA(), "att_i", RepoReady, g2)
	if err != nil || !short.ExpiresAt.Equal(f.Clock.Now().Add(5*time.Minute)) {
		t.Fatalf("bounded by the lease: %+v %v", short, err)
	}
}

// RevocationTransactional: revoking an attempt's grants is a step of the
// caller's transaction: it commits with it and disappears with its rollback, so a
// cancelled attempt can never push after the cancellation commits (GR-06).
func RevocationTransactional(t *testing.T, f *Fixture) {
	f.lease(ScopeA(), "att_r", true, time.Hour)
	f.lease(ScopeA(), "att_s", true, time.Hour)
	issue := func(attempt, run string) gitauth.GrantHandle {
		t.Helper()
		h, err := f.Svc.IssueAttempt(bg, ScopeA(), f.attemptRequest(gitv1.GrantAttempt, attempt, run, []gitv1.GrantRepository{agentGrant(RepoReady, "wi_1", run, true)}, time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	r, s := issue("att_r", "run_r"), issue("att_s", "run_s")
	prep, err := f.Svc.IssueAttempt(bg, ScopeA(), f.attemptRequest(gitv1.GrantPreparation, "", "run_r", []gitv1.GrantRepository{{ID: RepoReady, Read: true, Update: []string{}}}, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f.lease(ScopeB(), "att_r", true, time.Hour)
	br, err := f.Svc.IssueAttempt(bg, ScopeB(), f.attemptRequest(gitv1.GrantAttempt, "att_r", "run_r", []gitv1.GrantRepository{agentGrant(RepoB, "wi_1", "run_r", true)}, time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// The caller's transaction fails after revoking: nothing is revoked.
	boom := errors.New("the cancellation failed")
	err = f.Env.Store().Do(bg, TenantA, func(ctx context.Context, tx gitauth.GrantTx) error {
		n, err := gitauth.RevokeAttemptIn(ctx, tx, ScopeA(), "att_r", f.Clock.Now())
		if err != nil || n != 1 {
			t.Fatalf("revoked %d: %v", n, err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("the transaction's own error: %v", err)
	}
	f.MustVerify(TenantA, r.Credential.Reveal())

	// It commits with the caller's transaction.
	err = f.Env.Store().Do(bg, TenantA, func(ctx context.Context, tx gitauth.GrantTx) error {
		n, err := gitauth.RevokeAttemptIn(ctx, tx, ScopeA(), "att_r", f.Clock.Now())
		if err != nil || n != 1 {
			t.Fatalf("revoked %d: %v", n, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Svc.Verify(bg, TenantA, r.Credential.Reveal())
	WantInvalid(t, err)
	// Other attempts, the preparation grant and the other tenant's attempt of the
	// same ID are untouched; a wrong project in the scope revokes nothing.
	f.MustVerify(TenantA, s.Credential.Reveal())
	f.MustVerify(TenantA, prep.Credential.Reveal())
	f.MustVerify(TenantB, br.Credential.Reveal())
	if err := f.Svc.RevokeAttempt(bg, domain.Scope{TenantID: TenantA, ProjectID: ProjectX}, "att_s"); err != nil {
		t.Fatal(err)
	}
	f.MustVerify(TenantA, s.Credential.Reveal())
	// Idempotent.
	if err := f.Svc.RevokeAttempt(bg, ScopeA(), "att_r"); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if err := f.Svc.RevokeAttempt(bg, ScopeA(), "att_never_seen"); err != nil {
		t.Fatalf("unknown attempt: %v", err)
	}
	// A replay of the revoked issuance does not revive it.
	_, err = f.Svc.IssueAttempt(bg, ScopeA(), f.attemptRequest(gitv1.GrantAttempt, "att_r", "run_r", []gitv1.GrantRepository{agentGrant(RepoReady, "wi_1", "run_r", true)}, time.Hour))
	WantCode(t, err, domain.CodeForbidden)
	WantCode(t, f.Svc.RevokeAttempt(bg, domain.Scope{TenantID: "bad tenant", ProjectID: ProjectA}, "att_r"), domain.CodeInvalidRequest)

	// The service's own revocation, with its own transaction.
	if err := f.Svc.RevokeAttempt(bg, ScopeA(), "att_s"); err != nil {
		t.Fatal(err)
	}
	_, err = f.Svc.Verify(bg, TenantA, s.Credential.Reveal())
	WantInvalid(t, err)
	f.MustVerify(TenantA, prep.Credential.Reveal())
	if err := f.Svc.Revoke(bg, TenantA, prep.GrantID); err != nil {
		t.Fatal(err)
	}
	if err := f.Svc.Revoke(bg, TenantA, prep.GrantID); err != nil {
		t.Fatalf("revoking twice: %v", err)
	}
	_, err = f.Svc.Verify(bg, TenantA, prep.Credential.Reveal())
	WantInvalid(t, err)
	WantCode(t, f.Svc.Revoke(bg, TenantA, "gg_unknown"), domain.CodeNotFound)
	WantCode(t, f.Svc.Revoke(bg, TenantB, r.GrantID), domain.CodeNotFound)

	// Consuming a seed joins the caller's transaction the same way.
	seed := f.mustExchange(Engineer, RepoEmpty, true)
	err = f.Env.Store().Do(bg, TenantA, func(ctx context.Context, tx gitauth.GrantTx) error {
		if err := gitauth.ConsumeSeedIn(ctx, tx, seed.GrantID, "rcv_tx", f.Clock.Now()); err != nil {
			t.Fatal(err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if err := f.Svc.ConsumeSeed(bg, TenantA, seed.GrantID, "rcv_other"); err != nil {
		t.Fatalf("a rolled-back consumption leaves the seed unused: %v", err)
	}
	if err := f.Svc.ConsumeSeed(bg, TenantA, seed.GrantID, "rcv_tx"); !errors.Is(err, gitauth.ErrSeedConsumed) {
		t.Fatalf("committed consumption: %v", err)
	}
}

// TenantIsolation: a grant of one tenant is unreachable from another by
// credential, ID, revocation, consumption or attempt.
func TenantIsolation(t *testing.T, f *Fixture) {
	resp := f.mustExchange(Engineer, RepoEmpty, true)
	human := f.mustExchange(Engineer, RepoReady, false)
	WantCode(t, f.Svc.Revoke(bg, TenantB, human.GrantID), domain.CodeNotFound)
	WantCode(t, f.Svc.ConsumeSeed(bg, TenantB, resp.GrantID, "rcv_1"), domain.CodeNotFound)
	_, err := f.Svc.Claims(bg, TenantB, human.GrantID)
	WantCode(t, err, domain.CodeNotFound)
	_, err = f.Svc.Claims(bg, "bad tenant", human.GrantID)
	WantCode(t, err, domain.CodeNotFound)
	_, err = f.Svc.Verify(bg, TenantB, human.Password)
	WantInvalid(t, err)
	f.MustVerify(TenantA, human.Password)
	if err := f.Svc.ConsumeSeed(bg, TenantA, resp.GrantID, "rcv_1"); err != nil {
		t.Fatalf("the owner's seed is unaffected: %v", err)
	}
	if n := f.Env.GrantCount(t, TenantB); n != 0 {
		t.Fatalf("tenant B holds %d grants", n)
	}
	// Tenant B issues for itself; nothing crosses.
	bResp, err := f.Svc.Exchange(bg, TenantB, Engineer, gitv1.CredentialRequest{ProjectID: string(ProjectB), RepositoryID: RepoB})
	if err != nil {
		t.Fatal(err)
	}
	bc, err := f.Svc.Verify(bg, TenantB, bResp.Password)
	if err != nil || bc.TenantID != string(TenantB) || bc.ProjectID != string(ProjectB) {
		t.Fatalf("tenant B claims = %+v %v", bc, err)
	}
	_, err = f.Svc.Verify(bg, TenantA, bResp.Password)
	WantInvalid(t, err)
}

// HumanCredential exchanges the person's login for a human credential of the
// repository in ProjectA and returns the secret.
func (f *Fixture) HumanCredential(user identity.UserID, repo string) string {
	f.T.Helper()
	return f.mustExchange(user, repo, false).Password
}

// raceWindow is how long a transaction that must be blocked by the seed lock is
// given to prove it is not: the other side is held open by a channel (never a
// sleep), and a regression is detected the instant the blocked side reaches its
// next step, so the window only bounds how long a correct implementation waits.
// It is generous against scheduling delay; every other wait is bounded by a
// context so an early failure cannot hang the test.
const raceWindow = 500 * time.Millisecond

// SeedIssuanceVersusConsumption: seed issuance and the receive-time consumption
// of a seed are serialized by the repository's seed lock, so a new seed is never
// issued while the consumed one's receive is unsettled, and a receive never
// consumes a seed that a newer issuance revoked. The push-record insert of the
// push authorizer is simulated through the Pushes port inside the consuming
// transaction, as T-130 will do it. Both orders are driven by channels: the
// holder of the lock is parked until the test releases it, and the other side
// must not make progress while it is parked.
func SeedIssuanceVersusConsumption(t *testing.T, f *Fixture) {
	const repo = RepoEmpty
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	wait := func(ch <-chan error, what string) error {
		t.Helper()
		select {
		case err := <-ch:
			return err
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
			return nil
		}
	}
	defer func() { f.Pushes.OnCheck = nil }()

	// Consumption (with its push record) first: the issuance that starts while
	// that transaction is open must wait, and see the unsettled push once it commits.
	first := f.mustExchange(Engineer, repo, true)
	grants := f.Env.GrantCount(t, TenantA)
	reachedCheck := make(chan struct{}, 1) // the issuance read the push records
	f.Pushes.OnCheck = func() {
		select {
		case reachedCheck <- struct{}{}:
		default:
		}
	}
	consumed, proceed := make(chan struct{}), make(chan struct{})
	authorized := make(chan error, 1)
	go func() {
		authorized <- f.Env.Store().Do(ctx, TenantA, func(txctx context.Context, tx gitauth.GrantTx) error {
			if err := gitauth.ConsumeSeedIn(txctx, tx, first.GrantID, "rcv_first", f.Clock.Now()); err != nil {
				return err
			}
			close(consumed) // the seed lock is held from here until the commit
			select {
			case <-proceed:
			case <-txctx.Done():
				return txctx.Err()
			}
			f.Pushes.SetUnsettled(ScopeA(), repo, true) // the push record, in the same transaction
			return nil
		})
	}()
	select {
	case <-consumed:
	case err := <-authorized:
		t.Fatalf("authorization ended before it held the lock: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for the consuming transaction")
	}
	issued := make(chan error, 1)
	go func() {
		_, err := f.exchange(Engineer, repo, true)
		issued <- err
	}()
	select {
	case <-reachedCheck:
		close(proceed)
		t.Fatal("the issuance checked the push records while the consuming transaction held the seed lock")
	case err := <-issued:
		close(proceed)
		t.Fatalf("the issuance finished while the consuming transaction held the seed lock: %v", err)
	case <-time.After(raceWindow): // blocked, as it must be
	}
	close(proceed)
	WantCode(t, wait(issued, "the issuance"), gitv1.CodeSeedPending)
	if err := wait(authorized, "the authorization"); err != nil {
		t.Fatalf("authorization: %v", err)
	}
	if n := f.Env.GrantCount(t, TenantA); n != grants {
		t.Fatalf("a seed was issued while the consumed seed's receive was unsettled (%d grants, was %d)", n, grants)
	}
	// Once the receive settles refused the repository is seedable again.
	f.Pushes.SetUnsettled(ScopeA(), repo, false)
	second := f.mustExchange(Engineer, repo, true)

	// Issuance first: it is parked inside its transaction (after the lock, at the
	// push-record check); a receive that tries to consume the old seed meanwhile
	// must wait, and then finds the seed revoked and authorizes nothing.
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.Pushes.OnCheck = func() {
		once.Do(func() {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
	}
	var third gitv1.CredentialResponse
	issuing := make(chan error, 1)
	go func() {
		var err error
		third, err = f.exchange(Engineer, repo, true)
		issuing <- err
	}()
	select {
	case <-entered:
	case err := <-issuing:
		t.Fatalf("issuance ended before it held the lock: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for the issuance")
	}
	consuming := make(chan error, 1)
	go func() {
		consuming <- f.Env.Store().Do(ctx, TenantA, func(txctx context.Context, tx gitauth.GrantTx) error {
			return gitauth.ConsumeSeedIn(txctx, tx, second.GrantID, "rcv_second", f.Clock.Now())
		})
	}()
	select {
	case err := <-consuming:
		close(release)
		t.Fatalf("the receive finished while the issuance held the seed lock: %v", err)
	case <-time.After(raceWindow): // blocked, as it must be
	}
	close(release)
	if err := wait(issuing, "the issuance"); err != nil {
		t.Fatalf("issuance: %v", err)
	}
	if err := wait(consuming, "the receive"); !errors.Is(err, gitauth.ErrGrantInactive) {
		t.Fatalf("a receive consuming a seed that a newer issuance revoked: %v", err)
	}
	f.MustVerify(TenantA, third.Password)
	if err := f.Svc.ConsumeSeed(bg, TenantA, third.GrantID, "rcv_third"); err != nil {
		t.Fatalf("the newest seed: %v", err)
	}
}

// issueEveryKind issues one grant of every kind in TenantA (the seed in the
// repository that is still empty, an attempt grant in another project of the
// tenant too) and returns the credentials by name.
func (f *Fixture) issueEveryKind(t *testing.T) map[string]string {
	t.Helper()
	creds := map[string]string{
		"human": f.mustExchange(Engineer, RepoReady, false).Password,
		"seed":  f.mustExchange(Engineer, RepoEmpty, true).Password,
	}
	f.lease(ScopeA(), "att_all", true, time.Hour)
	f.lease(ScopeX(), "att_x", true, time.Hour)
	for name, req := range map[string]gitauth.AttemptGrantRequest{
		"preparation": f.attemptRequest(gitv1.GrantPreparation, "", "run_all", []gitv1.GrantRepository{{ID: RepoReady, Read: true, Update: []string{}}}, time.Hour),
		"attempt":     f.attemptRequest(gitv1.GrantAttempt, "att_all", "run_all", []gitv1.GrantRepository{agentGrant(RepoReady, "wi_1", "run_all", true)}, time.Hour),
	} {
		h, err := f.Svc.IssueAttempt(bg, ScopeA(), req)
		if err != nil {
			t.Fatalf("issue %s: %v", name, err)
		}
		creds[name] = h.Credential.Reveal()
	}
	h, err := f.Svc.IssueAttempt(bg, ScopeX(), f.attemptRequest(gitv1.GrantAttempt, "att_x", "run_x", []gitv1.GrantRepository{agentGrant(RepoOther, "wi_2", "run_x", true)}, time.Hour))
	if err != nil {
		t.Fatalf("issue in the other project: %v", err)
	}
	creds["attempt_other_project"] = h.Credential.Reveal()
	c := gitv1.IntegrationConstraint{Ref: "refs/heads/main", From: oid("2"), To: oid("3"), GateID: "gate_all"}
	f.Gates.Set(ScopeA(), c.GateID, gitauth.GateDecision{Approved: true, Current: true, RunID: "run_all", AttemptID: "att_all", RepositoryID: RepoReady, Base: c.From, Candidate: c.To})
	ih, err := f.Svc.IssueIntegration(bg, ScopeA(), "att_all", RepoReady, c)
	if err != nil {
		t.Fatalf("issue integration: %v", err)
	}
	creds["integration"] = ih.Credential.Reveal()
	return creds
}

var offboarder = gitauth.AuditActor{Type: gitauth.AuditActorSystem, ID: "gitops-offboarding"}

// RevokeAllForTenant: a tenant-wide revocation revokes every unrevoked grant of
// every kind and project of that tenant and no other tenant's, is atomic with
// the caller's transaction, is audited once, is idempotent and takes effect at
// the next verification (IT-12).
func RevokeAllForTenant(t *testing.T, f *Fixture) {
	creds := f.issueEveryKind(t)
	const issued = 6 // human, seed, preparation, attempt, integration, attempt of another project
	if len(creds) != issued {
		t.Fatalf("issued %d kinds", len(creds))
	}
	// A consumed seed and an already revoked grant are part of the tenant's
	// grants too; the revocation must neither fail on nor recount them.
	if err := f.Svc.ConsumeSeed(bg, TenantA, f.claimsOf(t, creds["seed"]).GrantID, "rcv_all"); err != nil {
		t.Fatal(err)
	}
	f.lease(ScopeA(), "att_spent", true, time.Hour)
	extra, err := f.Svc.IssueAttempt(bg, ScopeA(), f.attemptRequest(gitv1.GrantAttempt, "att_spent", "run_all", []gitv1.GrantRepository{agentGrant(RepoReady, "wi_1", "run_all", true)}, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Svc.Revoke(bg, TenantA, extra.GrantID); err != nil {
		t.Fatal(err)
	}
	// Another tenant's grants, the same user ID and identical shape.
	bHuman, err := f.Svc.Exchange(bg, TenantB, Engineer, gitv1.CredentialRequest{ProjectID: string(ProjectB), RepositoryID: RepoB})
	if err != nil {
		t.Fatal(err)
	}
	f.lease(ScopeB(), "att_all", true, time.Hour)
	bAttempt, err := f.Svc.IssueAttempt(bg, ScopeB(), f.attemptRequest(gitv1.GrantAttempt, "att_all", "run_all", []gitv1.GrantRepository{agentGrant(RepoB, "wi_1", "run_all", true)}, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n := f.mustActive(TenantA); n != issued {
		t.Fatalf("active before = %d", n)
	}
	if n := f.mustActive(TenantB); n != 2 {
		t.Fatalf("tenant B active = %d", n)
	}

	// Refusals change nothing.
	_, err = f.Svc.RevokeAll(bg, "bad tenant", "offboarding", offboarder)
	WantCode(t, err, domain.CodeInvalidRequest)
	_, err = f.Svc.RevokeAll(bg, TenantA, "  ", offboarder)
	WantCode(t, err, domain.CodeInvalidRequest)
	_, err = f.Svc.RevokeAll(bg, TenantA, strings.Repeat("x", gitauth.MaxAuditReason+1), offboarder)
	WantCode(t, err, domain.CodeInvalidRequest)
	_, err = f.Svc.RevokeAll(bg, TenantA, "offboarding", gitauth.AuditActor{Type: "robot", ID: "x"})
	WantCode(t, err, domain.CodeInvalidRequest)
	_, err = f.Svc.RevokeAll(bg, TenantA, "offboarding", gitauth.AuditActor{Type: gitauth.AuditActorSystem})
	WantCode(t, err, domain.CodeInvalidRequest)
	if n := f.mustActive(TenantA); n != issued {
		t.Fatalf("a refused revocation changed %d active grants", issued-n)
	}
	if got := f.Env.RevocationAudits(t, TenantA); len(got) != 0 {
		t.Fatalf("a refused revocation was audited: %+v", got)
	}

	// The caller's transaction fails after revoking: nothing is revoked and
	// nothing is audited, inside the transaction the revocation was visible.
	boom := errors.New("the offboarding step failed")
	err = f.Env.Store().Do(bg, TenantA, func(ctx context.Context, tx gitauth.GrantTx) error {
		n, err := gitauth.RevokeAllIn(ctx, tx, "offboarding", offboarder, f.Clock.Now(), "aud_rolled_back")
		if err != nil || n != issued {
			t.Fatalf("revoked %d: %v", n, err)
		}
		if active, err := tx.ActiveGrants(ctx, f.Clock.Now()); err != nil || active != 0 {
			t.Fatalf("the transaction sees %d active grants: %v", active, err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("the transaction's own error: %v", err)
	}
	for name, c := range creds {
		if _, err := f.Svc.Verify(bg, TenantA, c); err != nil {
			t.Fatalf("%s after a rolled back revocation: %v", name, err)
		}
	}
	if n := f.mustActive(TenantA); n != issued {
		t.Fatalf("active after rollback = %d", n)
	}
	if got := f.Env.RevocationAudits(t, TenantA); len(got) != 0 {
		t.Fatalf("a rolled back revocation was audited: %+v", got)
	}

	// The service's own revocation.
	n, err := f.Svc.RevokeAll(bg, TenantA, "tenant offboarding", offboarder)
	if err != nil || n != issued {
		t.Fatalf("RevokeAll = %d, %v", n, err)
	}
	for _, c := range creds {
		_, err := f.Svc.Verify(bg, TenantA, c)
		WantInvalid(t, err)
	}
	if n := f.mustActive(TenantA); n != 0 {
		t.Fatalf("active after = %d", n)
	}
	audits := f.Env.RevocationAudits(t, TenantA)
	if len(audits) != 1 || audits[0] != (AuditRow{Action: gitauth.AuditActionRevokeAll, ActorType: "system", ActorID: "gitops-offboarding", Reason: "tenant offboarding", Revoked: issued}) {
		t.Fatalf("audit = %+v", audits)
	}
	// The other tenant is untouched.
	f.MustVerify(TenantB, bHuman.Password)
	f.MustVerify(TenantB, bAttempt.Credential.Reveal())
	if n := f.mustActive(TenantB); n != 2 {
		t.Fatalf("tenant B active = %d", n)
	}
	if got := f.Env.RevocationAudits(t, TenantB); len(got) != 0 {
		t.Fatalf("tenant B was audited: %+v", got)
	}
	// A replay of a revoked issuance does not revive it.
	_, err = f.Svc.IssueAttempt(bg, ScopeA(), f.attemptRequest(gitv1.GrantAttempt, "att_all", "run_all", []gitv1.GrantRepository{agentGrant(RepoReady, "wi_1", "run_all", true)}, time.Hour))
	WantCode(t, err, domain.CodeForbidden)

	// Idempotent: a repeat revokes and records nothing.
	for range 2 {
		if n, err := f.Svc.RevokeAll(bg, TenantA, "tenant offboarding", offboarder); err != nil || n != 0 {
			t.Fatalf("repeat = %d, %v", n, err)
		}
	}
	if got := f.Env.RevocationAudits(t, TenantA); len(got) != 1 {
		t.Fatalf("a repeat was audited: %+v", got)
	}

	// Joined into the caller's transaction, which commits with it.
	f.Clock.Advance(time.Minute)
	err = f.Env.Store().Do(bg, TenantB, func(ctx context.Context, tx gitauth.GrantTx) error {
		n, err := gitauth.RevokeAllIn(ctx, tx, "operator request", gitauth.AuditActor{Type: gitauth.AuditActorOperator, ID: "op_1"}, f.Clock.Now(), "aud_joined")
		if err != nil || n != 2 {
			t.Fatalf("joined revoke = %d, %v", n, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Svc.Verify(bg, TenantB, bHuman.Password)
	WantInvalid(t, err)
	_, err = f.Svc.Verify(bg, TenantB, bAttempt.Credential.Reveal())
	WantInvalid(t, err)
	if n := f.mustActive(TenantB); n != 0 {
		t.Fatalf("tenant B active after = %d", n)
	}
	if got := f.Env.RevocationAudits(t, TenantB); len(got) != 1 || got[0].Revoked != 2 || got[0].ActorType != "operator" || got[0].Reason != "operator request" {
		t.Fatalf("tenant B audit = %+v", got)
	}
	// Tenant A's trail is unchanged, and a reused audit ID is a conflict, not a
	// second record.
	if got := f.Env.RevocationAudits(t, TenantA); len(got) != 1 {
		t.Fatalf("tenant A audit = %+v", got)
	}
	err = f.Env.Store().Do(bg, TenantB, func(ctx context.Context, tx gitauth.GrantTx) error {
		return tx.RecordAudit(ctx, gitauth.AuditRecord{ID: "aud_joined", Action: gitauth.AuditActionRevokeAll, Actor: offboarder, Reason: "again", OccurredAt: f.Clock.Now(), Revoked: 1})
	})
	if !errors.Is(err, gitauth.ErrConflict) {
		t.Fatalf("a reused audit ID: %v", err)
	}
	if got := f.Env.RevocationAudits(t, TenantB); len(got) != 1 {
		t.Fatalf("tenant B audit after the conflict = %+v", got)
	}
}

func (f *Fixture) claimsOf(t *testing.T, credential string) gitv1.GrantClaims {
	t.Helper()
	return f.MustVerify(TenantA, credential)
}

func (f *Fixture) mustActive(tenant domain.TenantID) int {
	f.T.Helper()
	n, err := f.Svc.Active(bg, tenant)
	if err != nil {
		f.T.Fatalf("active: %v", err)
	}
	return n
}

// ActiveGrantsCount: Active counts exactly the tenant's grants that are neither
// revoked nor expired, per tenant, at the service's clock.
func ActiveGrantsCount(t *testing.T, f *Fixture) {
	if n := f.mustActive(TenantA); n != 0 {
		t.Fatalf("a tenant without grants has %d active", n)
	}
	_, err := f.Svc.Active(bg, "bad tenant")
	WantCode(t, err, domain.CodeInvalidRequest)

	human := f.mustExchange(Engineer, RepoReady, false) // 15 minutes
	seed := f.mustExchange(Engineer, RepoEmpty, true)   // 15 minutes
	f.lease(ScopeA(), "att_c", true, 3*time.Hour)
	long, err := f.Svc.IssueAttempt(bg, ScopeA(), f.attemptRequest(gitv1.GrantAttempt, "att_c", "run_c", []gitv1.GrantRepository{agentGrant(RepoReady, "wi_1", "run_c", true)}, 2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f.lease(ScopeA(), "att_d", true, 3*time.Hour)
	if _, err := f.Svc.IssueAttempt(bg, ScopeA(), f.attemptRequest(gitv1.GrantAttempt, "att_d", "run_d", []gitv1.GrantRepository{agentGrant(RepoReady, "wi_1", "run_d", true)}, time.Hour)); err != nil {
		t.Fatal(err)
	}
	bResp, err := f.Svc.Exchange(bg, TenantB, Engineer, gitv1.CredentialRequest{ProjectID: string(ProjectB), RepositoryID: RepoB})
	if err != nil {
		t.Fatal(err)
	}
	if a, b := f.mustActive(TenantA), f.mustActive(TenantB); a != 4 || b != 1 {
		t.Fatalf("active = %d / %d, want 4 / 1", a, b)
	}

	// A revoked grant is not active.
	if err := f.Svc.Revoke(bg, TenantA, human.GrantID); err != nil {
		t.Fatal(err)
	}
	if n := f.mustActive(TenantA); n != 3 {
		t.Fatalf("active after one revocation = %d", n)
	}
	// A consumed seed counts until it is revoked or expires.
	if err := f.Svc.ConsumeSeed(bg, TenantA, seed.GrantID, "rcv_c"); err != nil {
		t.Fatal(err)
	}
	if n := f.mustActive(TenantA); n != 3 {
		t.Fatalf("active with a consumed seed = %d", n)
	}
	// Attempt revocation is counted too.
	if err := f.Svc.RevokeAttempt(bg, ScopeA(), "att_d"); err != nil {
		t.Fatal(err)
	}
	if n := f.mustActive(TenantA); n != 2 {
		t.Fatalf("active after the attempt revocation = %d", n)
	}
	// Expiry: the seed expires (15 minutes), the long attempt grant does not.
	f.Clock.Advance(16 * time.Minute)
	if n := f.mustActive(TenantA); n != 1 {
		t.Fatalf("active after the seed's expiry = %d", n)
	}
	f.MustVerify(TenantA, long.Credential.Reveal())
	// The boundary is the contract's: usable strictly before the expiry.
	expires := long.ExpiresAt
	f.Clock.Advance(expires.Sub(f.Clock.Now()) - time.Microsecond)
	if n := f.mustActive(TenantA); n != 1 {
		t.Fatalf("active just before the expiry = %d", n)
	}
	f.Clock.Advance(time.Microsecond)
	if n := f.mustActive(TenantA); n != 0 {
		t.Fatalf("active at the expiry = %d", n)
	}
	if n := f.mustActive(TenantB); n != 0 {
		t.Fatalf("tenant B's expired grant is active: %d", n)
	}
	_, err = f.Svc.Verify(bg, TenantB, bResp.Password)
	WantInvalid(t, err)
}
