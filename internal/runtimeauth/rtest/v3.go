package rtest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/jwt"
)

// ---- contract fixtures ------------------------------------------------------------

type v3Case struct {
	Name    string          `json:"name"`
	Type    string          `json:"type"`
	Valid   bool            `json:"valid"`
	Payload json.RawMessage `json:"payload"`
}

// V3Payload returns the named valid payload of contracts/runtime/v3/fixtures/cases.json:
// the executable fixtures prove the shapes, so every test here builds its
// contexts from them rather than inventing documents.
func V3Payload(t testing.TB, name string) json.RawMessage {
	t.Helper()
	_, file, _, _ := goruntime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "..", "contracts", "runtime", "v3", "fixtures", "cases.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var all []v3Case
	if err := json.Unmarshal(data, &all); err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		if c.Name == name {
			if !c.Valid {
				t.Fatalf("fixture %q is not a valid payload", name)
			}
			return c.Payload
		}
	}
	t.Fatalf("fixture %q not found", name)
	return nil
}

// V3Context decodes a named ContextV3 fixture and applies edits to it. The result
// is validated, so an edit cannot produce a context the contract refuses.
func V3Context(t testing.TB, name string, edits ...func(*runtimev3.ContextV3)) runtimev3.ContextV3 {
	t.Helper()
	c, err := runtimev3.DecodeRequest[runtimev3.ContextV3](V3Payload(t, name))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range edits {
		e(&c)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("the edited context is invalid: %v", err)
	}
	return c
}

// V3Fixtures names the contexts the tests use.
const (
	// WriterContext: a work-item attempt that writes repo_01 and reads repo_02; it
	// advertises the integration grant.
	WriterContext = "context_v3_work_item_writer_two_repositories"
	// ProvisionContext: a provisioning attempt without sources; it advertises the
	// seed grant and holds no static grant.
	ProvisionContext = "context_v3_provision_origin_without_sources"
)

// V3ResourceContext decodes a named ResourceContextV3 fixture.
func V3ResourceContext(t testing.TB, name string, edits ...func(*runtimev3.ResourceContextV3)) runtimev3.ResourceContextV3 {
	t.Helper()
	c, err := runtimev3.DecodeRequest[runtimev3.ResourceContextV3](V3Payload(t, name))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range edits {
		e(&c)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("the edited context is invalid: %v", err)
	}
	return c
}

// PreparationStateV3 is the committed state of a source-set run's preparation,
// from the fixture context with the resource's deadline set to f.DeadlineAt.
func PreparationStateV3(t testing.TB, f ResourceFixture) runtimeauth.ResourceState {
	t.Helper()
	c := V3ResourceContext(t, "resource_context_v3_preparation_of_every_source", func(c *runtimev3.ResourceContextV3) {
		c.TenantID, c.ProjectID, c.RunID = f.Scope.TenantID, f.Scope.ProjectID, f.RunID
		c.Preparation.DeadlineAt = runtimev3.Timestamp(f.DeadlineAt.UTC().Format(time.RFC3339Nano))
	})
	return runtimeauth.ResourceState{
		Scope: f.Scope, RunID: f.RunID, Key: "preparation", Kind: runtimev2.ResourcePreparation, Phase: runtimeauth.ResourceActive,
		Fingerprint: fp(f.Fingerprint), WorkloadName: "kr-0123456789abcdef01234567", WorkloadUID: f.WorkloadUID, DeadlineAt: f.DeadlineAt, ContextV3: &c,
	}
}

// ---- attempts ---------------------------------------------------------------------

// SeedAttemptV3 creates an active source-set attempt with the given pinned
// ContextV3 in the memory authority and returns it as an Attempt whose
// Workload/AdmitRequest helpers install and describe its Job.
func (m *MemStack) SeedAttemptV3(c runtimev3.ContextV3, actions runtimeauth.ActionSet) Attempt {
	m.T.Helper()
	digest, _, err := runtimeauth.PinContextV3(c)
	if err != nil {
		m.T.Fatal(err)
	}
	job := runtimeauth.ExpectedJob{Namespace: Namespace, Name: JobName(c.AttemptID)}
	m.Env.SeedAttempt(m.T, c.Scope(), c.RunID, c.AttemptID, job, digest, actions)
	return Attempt{Scope: c.Scope(), Context: runtimev1.Context{AttemptID: c.AttemptID, RunID: c.RunID}, Job: job, Actions: actions}
}

// RegisterV3With registers an existing credential for a source-set attempt.
func (s *Stack) RegisterV3With(a Attempt, c runtimev3.ContextV3, cred runtimeauth.Credential, actions runtimeauth.ActionSet) (runtimeauth.Registration, error) {
	return s.Service.RegisterV3(context.Background(), runtimeauth.RegisterCommandV3{Scope: a.Scope, AttemptID: c.AttemptID, Credential: cred, Actions: actions, Context: c})
}

// V3MemStack is a MemStack with the resource subjects, the runtime Git grants and
// their fakes: the whole source-set wire in memory.
type V3MemStack struct {
	*ResourceMemStack
	Git    *FakeGit
	Issued *MemIssuedGrants
}

// NewV3MemStack builds the service with the memory authority, resources, the
// Git fakes and the issued-grant records.
func NewV3MemStack(t testing.TB, opts ...memdb.Option) *V3MemStack {
	t.Helper()
	m := &MemStack{Env: NewMemEnv(t, opts...)}
	s := &Stack{T: t, Clock: NewClock(), Faults: &Faults{}, Logs: &SafeBuffer{}, Jobs: &Jobs{}}
	key, err := jwt.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if s.Ring, err = jwt.NewKeyRing(1, key); err != nil {
		t.Fatal(err)
	}
	if s.Signer, err = jwt.NewSigner(s.Ring, Issuer); err != nil {
		t.Fatal(err)
	}
	if s.Verifier, err = jwt.NewVerifier(jwt.VerifierConfig{Ring: s.Ring, Issuer: Issuer, MaxLifetime: TestPolicy.MaxSession, Clock: s.Clock.Now}); err != nil {
		t.Fatal(err)
	}
	res := NewMemResources()
	s.ResourceConfig = &runtimeauth.ResourceConfig{Store: res, State: res, Workloads: res, Tokens: s.Signer, Verifier: s.Verifier}
	git, issued := NewFakeGit(s.Clock.Now), NewMemIssuedGrants()
	s.GitConfig = &runtimeauth.GitConfig{Issuer: git, Repositories: git, Approvals: git, Issued: issued}
	s.Service = s.NewService(TestPolicy, m.Env.Grants, m.Env.Attempts, s.Jobs, s.Signer)
	m.Stack = s
	return &V3MemStack{ResourceMemStack: &ResourceMemStack{MemStack: m, Resources: res}, Git: git, Issued: issued}
}

// ---- issued grant records ------------------------------------------------------------

// MemIssuedGrants is runtimeauth.IssuedGrants in memory. It is development-only.
type MemIssuedGrants struct {
	mu   sync.Mutex
	rows map[string]runtimeauth.IssuedGrant // tenant + "/" + grant ID
}

var _ runtimeauth.IssuedGrants = (*MemIssuedGrants)(nil)

// NewMemIssuedGrants returns an empty store.
func NewMemIssuedGrants() *MemIssuedGrants {
	return &MemIssuedGrants{rows: map[string]runtimeauth.IssuedGrant{}}
}

// DevelopmentOnly marks the adapter for the composition root.
func (*MemIssuedGrants) DevelopmentOnly() bool { return true }

// RecordIssued implements runtimeauth.IssuedGrants.
func (m *MemIssuedGrants) RecordIssued(_ context.Context, g runtimeauth.IssuedGrant) error {
	if err := g.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(g)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := string(g.Scope.TenantID) + "/" + g.Claims.GrantID
	if old, ok := m.rows[k]; ok {
		oldRaw, _ := json.Marshal(old)
		if string(oldRaw) != string(raw) {
			return runtimeauth.ErrConflict
		}
		return nil
	}
	m.rows[k] = g
	return nil
}

// IssuedFor implements runtimeauth.IssuedGrants.
func (m *MemIssuedGrants) IssuedFor(_ context.Context, scope domain.Scope, attempt domain.AttemptID) ([]gitv1.GrantClaims, error) {
	if err := scope.Validate(); err != nil {
		return nil, runtimeauth.ErrInvalidRequest
	}
	if err := attempt.Validate(); err != nil {
		return nil, runtimeauth.ErrInvalidRequest
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []gitv1.GrantClaims{}
	for _, g := range m.rows {
		if g.Scope == scope && g.AttemptID == attempt {
			out = append(out, g.Claims)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GrantID < out[j].GrantID })
	return out, nil
}

// ---- fake Git ports ---------------------------------------------------------------------

// FakeGit stands in for the Git authority, the repository module and the gates
// module behind runtimeauth's Git ports. It issues claims exactly shaped like
// gitauth's and records what it was asked, so the service's own rules (what it
// asks for, in which order, and what it refuses before asking) are what a test
// observes. The real gitauth is exercised in package gitgrants.
type FakeGit struct {
	mu  sync.Mutex
	now func() time.Time

	repos     map[string]gitv1.Repository // scope + "/" + id
	created   map[string]bool             // scope + "/" + run + "/" + id
	unsettled map[string]bool
	approvals map[string]approvalAnswer // scope + "/" + run + "/" + gate + "/" + repo
	seq       int

	// IssueErr, when set, fails every issuance with it.
	IssueErr error
	// OnIssue, when set, runs after an issuance succeeded and before it returns:
	// where a test cancels the attempt "while the grant is issued".
	OnIssue func()
	// Calls, in order: "seed", "integration", "revoke", "approval", "repository".
	Calls []string
	// Revoked lists the attempts RevokeAttempt was called for.
	Revoked []domain.AttemptID
	// Issued lists every grant issued.
	Issued []gitv1.GrantClaims
}

type approvalAnswer struct {
	approval runtimev3.IntegrationApproval
	err      error
}

var (
	_ runtimeauth.GitIssuer            = (*FakeGit)(nil)
	_ runtimeauth.GitRepositories      = (*FakeGit)(nil)
	_ runtimeauth.IntegrationApprovals = (*FakeGit)(nil)
)

// NewFakeGit returns an empty fake reading time from now.
func NewFakeGit(now func() time.Time) *FakeGit {
	return &FakeGit{now: now, repos: map[string]gitv1.Repository{}, created: map[string]bool{}, unsettled: map[string]bool{}, approvals: map[string]approvalAnswer{}}
}

func repoKey(scope domain.Scope, id string) string {
	return string(scope.TenantID) + "/" + string(scope.ProjectID) + "/" + id
}

// PutRepository stores a repository of the scope's project.
func (f *FakeGit) PutRepository(scope domain.Scope, r gitv1.Repository) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos[repoKey(scope, r.ID)] = r
}

// Create records that the run created the repository.
func (f *FakeGit) Create(scope domain.Scope, run domain.RunID, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created[repoKey(scope, id)+"/"+string(run)] = true
}

// SetUnsettled sets whether the repository has an unsettled push.
func (f *FakeGit) SetUnsettled(scope domain.Scope, id string, v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unsettled[repoKey(scope, id)] = v
}

// Approve sets the answer of IntegrationApproval for a gate: an approval, or an
// error (ErrNoApproval, *ApprovalStale, *ApprovalApplied, an outage).
func (f *FakeGit) Approve(scope domain.Scope, run domain.RunID, gate, repo string, a runtimev3.IntegrationApproval, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approvals[repoKey(scope, gate)+"/"+string(run)+"/"+repo] = approvalAnswer{a, err}
}

// Repository implements runtimeauth.GitRepositories.
func (f *FakeGit) Repository(_ context.Context, scope domain.Scope, id string) (gitv1.Repository, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "repository")
	r, ok := f.repos[repoKey(scope, id)]
	if !ok {
		return gitv1.Repository{}, runtimeauth.ErrNotFound
	}
	return r, nil
}

// UnsettledPush implements runtimeauth.GitRepositories.
func (f *FakeGit) UnsettledPush(_ context.Context, scope domain.Scope, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unsettled[repoKey(scope, id)], nil
}

// CreatedByRun implements runtimeauth.GitRepositories.
func (f *FakeGit) CreatedByRun(_ context.Context, scope domain.Scope, run domain.RunID, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created[repoKey(scope, id)+"/"+string(run)], nil
}

// IntegrationApproval implements runtimeauth.IntegrationApprovals.
func (f *FakeGit) IntegrationApproval(_ context.Context, scope domain.Scope, run domain.RunID, gate, repo string) (runtimev3.IntegrationApproval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "approval")
	if a, ok := f.approvals[repoKey(scope, gate)+"/"+string(run)+"/"+repo]; ok {
		return a.approval, a.err
	}
	return runtimev3.IntegrationApproval{}, runtimeauth.ErrNoApproval
}

func (f *FakeGit) secret() runtimeauth.GitCredential {
	f.seq++
	n := strconv.Itoa(f.seq)
	return runtimeauth.NewGitCredential("gitc_" + strings.Repeat("a", 43-len(n)) + n)
}

func (f *FakeGit) claims(kind gitv1.GrantKind, scope domain.Scope, run domain.RunID, attempt domain.AttemptID, repo gitv1.GrantRepository, ttl time.Duration, notAfter time.Time) gitv1.GrantClaims {
	now := f.now().UTC().Truncate(time.Second)
	exp := now.Add(ttl)
	if !notAfter.IsZero() && notAfter.Before(exp) {
		exp = notAfter
	}
	r, a := string(run), string(attempt)
	return gitv1.GrantClaims{
		GrantID: "gg_" + string(kind) + "_" + string(attempt), Audience: gitv1.GrantAudience, TenantID: string(scope.TenantID), ProjectID: string(scope.ProjectID),
		Actor: gitv1.Actor{Type: gitv1.ActorWorkload, ID: a, RunID: &r, AttemptID: &a}, Kind: kind, Repositories: []gitv1.GrantRepository{repo},
		IssuedAt: gitv1.Timestamp(now.Format(time.RFC3339)), ExpiresAt: gitv1.Timestamp(exp.Format(time.RFC3339)),
	}
}

// IssueSeed implements runtimeauth.GitIssuer.
func (f *FakeGit) IssueSeed(_ context.Context, scope domain.Scope, run domain.RunID, attempt domain.AttemptID, repo gitv1.Repository, notAfter time.Time) (runtimeauth.IssuedGitGrant, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, "seed")
	if f.IssueErr != nil {
		err := f.IssueErr
		f.mu.Unlock()
		return runtimeauth.IssuedGitGrant{}, err
	}
	c := f.claims(gitv1.GrantSeed, scope, run, attempt, gitv1.GrantRepository{ID: repo.ID, Read: true, Update: []string{gitv1.HeadsPrefix + repo.DefaultBranch}, Seed: true}, gitv1.MaxSeedLifetime, notAfter)
	f.Issued = append(f.Issued, c)
	out := runtimeauth.IssuedGitGrant{Claims: c, Credential: f.secret()}
	hook := f.OnIssue
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return out, nil
}

// IssueIntegration implements runtimeauth.GitIssuer.
func (f *FakeGit) IssueIntegration(_ context.Context, scope domain.Scope, run domain.RunID, attempt domain.AttemptID, repositoryID string, c gitv1.IntegrationConstraint) (runtimeauth.IssuedGitGrant, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, "integration")
	if f.IssueErr != nil {
		err := f.IssueErr
		f.mu.Unlock()
		return runtimeauth.IssuedGitGrant{}, err
	}
	cc := c
	claims := f.claims(gitv1.GrantIntegration, scope, run, attempt, gitv1.GrantRepository{ID: repositoryID, Read: true, Update: []string{c.Ref}, Integration: &cc}, 30*time.Minute, time.Time{})
	f.Issued = append(f.Issued, claims)
	out := runtimeauth.IssuedGitGrant{Claims: claims, Credential: f.secret()}
	hook := f.OnIssue
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return out, nil
}

// RevokeAttempt implements runtimeauth.GitIssuer.
func (f *FakeGit) RevokeAttempt(_ context.Context, _ domain.Scope, attempt domain.AttemptID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "revoke")
	f.Revoked = append(f.Revoked, attempt)
	return nil
}
