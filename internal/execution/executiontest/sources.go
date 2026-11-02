package executiontest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/memdb"
)

// Commits of the source-set fixtures (no Git repository behind them).
const (
	CommitD = "4444444444444444444444444444444444444444"
	CommitE = "5555555555555555555555555555555555555555"
	CommitF = "6666666666666666666666666666666666666666"
	zeroOID = "0000000000000000000000000000000000000000"
)

// ---- source sets ----

type fakeRepo struct {
	scope domain.Scope
	repo  execution.SourceRepository
	state string // "", "archived", "not_ready", "external"
}

type fakePush struct {
	scope  domain.Scope
	record gitv1.PushRecord
}

// FakeSourceSets is the double of execution.SourceSets: repositories of
// projects, the refs and push records the repository and push modules would hold
// and the retention it is asked for. It makes no network call and never
// touches Git; it is a fake of the consumer-owned seam, not an adapter.
type FakeSourceSets struct {
	mu      sync.Mutex
	baseURL string
	ca      *string
	repos   []fakeRepo
	tips    map[string]string
	pushes  map[string]fakePush
	missing map[string]bool
	seq     int

	retained map[domain.RunID]gitv1.SourceSet
	calls    []string

	// RetainErr fails Retain (a worker outage); nil clears it.
	RetainErr error
	// ResolveErr fails Resolve.
	ResolveErr error
	// OnRepository is called for every repository added (the SQL fixture inserts
	// the repository row the composite foreign key of run_sources points to).
	OnRepository func(scope domain.Scope, repo execution.SourceRepository)
}

// NewFakeSourceSets returns an empty fake with the test Git endpoint.
func NewFakeSourceSets() *FakeSourceSets {
	return &FakeSourceSets{
		baseURL: "https://git.example.invalid/git", tips: map[string]string{}, pushes: map[string]fakePush{}, missing: map[string]bool{},
		retained: map[domain.RunID]gitv1.SourceSet{},
	}
}

// SetCA names the CA file of the endpoint (nil: the system roots).
func (f *FakeSourceSets) SetCA(file *string) { f.mu.Lock(); f.ca = file; f.mu.Unlock() }

// AddRepository adds an internally hosted, ready repository of the scope's
// project whose default branch (main) points at CommitA.
func (f *FakeSourceSets) AddRepository(scope domain.Scope, name string) execution.SourceRepository {
	f.mu.Lock()
	f.seq++
	repo := execution.SourceRepository{
		ID: fmt.Sprintf("repo_%s_%02d", strings.ReplaceAll(name, "-", "_"), f.seq), Name: name, DefaultBranch: "main",
		URL: fmt.Sprintf("%s/%s/%s/%s.git", f.baseURL, scope.TenantID, scope.ProjectID, name),
	}
	f.repos = append(f.repos, fakeRepo{scope: scope, repo: repo})
	f.tips[repo.ID+"|refs/heads/main"] = CommitA
	hook := f.OnRepository
	f.mu.Unlock()
	if hook != nil {
		hook(scope, repo)
	}
	return repo
}

// SetState makes a repository unservable: "archived", "not_ready" or "external"
// (hosting_mismatch); "" restores it.
func (f *FakeSourceSets) SetState(name, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.repos {
		if f.repos[i].repo.Name == name {
			f.repos[i].state = state
		}
	}
}

// SetTip moves a ref of a repository.
func (f *FakeSourceSets) SetTip(repo execution.SourceRepository, ref, commit string) {
	f.mu.Lock()
	f.tips[repo.ID+"|"+ref] = commit
	f.mu.Unlock()
}

// MarkUnreachable makes a commit unreachable: pinning it is refused.
func (f *FakeSourceSets) MarkUnreachable(commit string) {
	f.mu.Lock()
	f.missing[commit] = true
	f.mu.Unlock()
}

// AddPush records an applied push by a person that created ref at commit.
func (f *FakeSourceSets) AddPush(scope domain.Scope, repo execution.SourceRepository, ref, commit string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := fmt.Sprintf("push_%03d", f.seq)
	f.pushes[id] = fakePush{scope: scope, record: gitv1.PushRecord{
		PushID: id, RepositoryID: repo.ID, ReceiveID: fmt.Sprintf("rcv_%03d", f.seq), Actor: gitv1.Actor{Type: gitv1.ActorUser, ID: Engineer},
		GrantID: fmt.Sprintf("gg_%03d", f.seq), Status: gitv1.PushApplied, CreatedAt: "2026-10-30T10:00:00Z",
		Entries: []gitv1.PushEntry{{Ref: ref, Old: zeroOID, New: commit, Kind: gitv1.KindCreate, State: gitv1.EntryApplied}},
	}}
	return id
}

// AddRefusedPush records a push whose only entry was refused.
func (f *FakeSourceSets) AddRefusedPush(scope domain.Scope, repo execution.SourceRepository, ref, commit string) string {
	id := f.AddPush(scope, repo, ref, commit)
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pushes[id]
	reason := string(gitv1.ReasonRefProtected)
	p.record.Entries = []gitv1.PushEntry{{Ref: ref, Old: zeroOID, New: commit, Kind: gitv1.KindCreate, State: gitv1.EntryRefused, Reason: &reason}}
	p.record.Status = gitv1.PushRefused
	f.pushes[id] = p
	return id
}

// Calls lists the operations asked of the fake in order.
func (f *FakeSourceSets) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Count counts the calls of one operation.
func (f *FakeSourceSets) Count(op string) int {
	n := 0
	for _, c := range f.Calls() {
		if c == op {
			n++
		}
	}
	return n
}

// Retained returns the set the run asked to retain.
func (f *FakeSourceSets) Retained(run domain.RunID) (gitv1.SourceSet, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.retained[run]
	return s, ok
}

// Resolve implements execution.SourceSets.
func (f *FakeSourceSets) Resolve(_ context.Context, scope domain.Scope, refs []execution.SourceRef) (execution.ResolvedSources, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "resolve")
	if f.ResolveErr != nil {
		return execution.ResolvedSources{}, f.ResolveErr
	}
	out := execution.ResolvedSources{BaseURL: f.baseURL, CAFile: f.ca}
	for _, ref := range refs {
		var found *fakeRepo
		for i := range f.repos {
			r := &f.repos[i]
			if r.scope == scope && ((ref.Name != "" && r.repo.Name == ref.Name) || (ref.ID != "" && r.repo.ID == ref.ID)) {
				found = r
			}
		}
		if found == nil {
			return execution.ResolvedSources{}, execution.ErrNotFound
		}
		switch found.state {
		case "archived":
			return execution.ResolvedSources{}, execution.Refusedf("repository_archived")
		case "not_ready":
			return execution.ResolvedSources{}, execution.Refusedf("repository_not_ready")
		case "external":
			return execution.ResolvedSources{}, execution.Refusedf("hosting_mismatch")
		}
		out.Repositories = append(out.Repositories, found.repo)
	}
	return out, nil
}

// Pin implements execution.SourceSets.
func (f *FakeSourceSets) Pin(_ context.Context, scope domain.Scope, reqs []execution.PinRequest) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "pin")
	out := make([]string, len(reqs))
	for i, r := range reqs {
		var commit string
		switch {
		case r.Push != nil:
			p, ok := f.pushes[r.Push.PushID]
			if !ok || p.scope != scope || p.record.RepositoryID != r.Repository.ID {
				return nil, execution.Refusedf("push_record_unavailable")
			}
			c, err := gitv1.PinnedCommit(p.record, r.Push.Ref)
			if err != nil {
				return nil, execution.Refusedf("push_not_applied")
			}
			commit = c
		case r.Commit != nil:
			commit = *r.Commit
		case r.Ref != nil:
			c, ok := f.tips[r.Repository.ID+"|"+*r.Ref]
			if !ok {
				return nil, execution.Refusedf("source_ref_unknown")
			}
			commit = c
		default:
			commit = f.tips[r.Repository.ID+"|refs/heads/"+r.Repository.DefaultBranch]
		}
		if f.missing[commit] || commit == "" {
			return nil, execution.Refusedf("source_commit_unreachable")
		}
		out[i] = commit
	}
	return out, nil
}

// Retain implements execution.SourceSets.
func (f *FakeSourceSets) Retain(_ context.Context, _ domain.Scope, run domain.RunID, set gitv1.SourceSet) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "retain")
	if f.RetainErr != nil {
		return f.RetainErr
	}
	f.retained[run] = set
	return nil
}

var _ execution.SourceSets = (*FakeSourceSets)(nil)

// ---- Git authority ----

// FakeGitAuthority is the double of execution.GitGrants together with the
// revocation seam of the stores. Issuance is idempotent per attempt and refused
// for an attempt that was revoked, as gitauth refuses a grant for an attempt
// that is not live; every revocation is recorded in the transaction that made it.
type FakeGitAuthority struct {
	mu       sync.Mutex
	n        int
	issued   []execution.AttemptGrantRequest
	handles  map[domain.AttemptID]execution.GitGrantHandle
	revoked  map[domain.AttemptID]bool
	IssueErr error
	// RevokeErr fails the in-transaction revocation (a store outage).
	RevokeErr error
	// Revoked reports whether the committed state revoked the grants of the
	// attempt (what gitauth reads from execution's attempt lease when it issues).
	Revoked func(attempt domain.AttemptID) bool
	// Revokes counts the revocation calls, committed or not.
	revokes int
	// AfterIssue runs after a grant was issued and before IssueAttempt returns
	// (a cancellation committing in the authority's lease-to-insert window).
	AfterIssue func(attempt domain.AttemptID)
	// direct counts RevokeAttempt calls (the port's own-transaction revocation).
	direct []domain.AttemptID
}

// NewFakeGitAuthority returns an empty authority.
func NewFakeGitAuthority() *FakeGitAuthority {
	return &FakeGitAuthority{handles: map[domain.AttemptID]execution.GitGrantHandle{}, revoked: map[domain.AttemptID]bool{}}
}

// IssueAttempt implements execution.GitGrants.
func (g *FakeGitAuthority) IssueAttempt(_ context.Context, _ domain.Scope, req execution.AttemptGrantRequest) (execution.GitGrantHandle, error) {
	if req.Actor.AttemptID == nil {
		return execution.GitGrantHandle{}, execution.Invalidf("an attempt grant names its attempt")
	}
	attempt := domain.AttemptID(*req.Actor.AttemptID)
	committed := g.Revoked != nil && g.Revoked(attempt)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.IssueErr != nil {
		return execution.GitGrantHandle{}, g.IssueErr
	}
	if g.revoked[attempt] || committed {
		return execution.GitGrantHandle{}, fmt.Errorf("%w: attempt_inactive", execution.ErrAttemptRevoked)
	}
	g.issued = append(g.issued, req)
	g.n++
	h, ok := g.handles[attempt]
	if !ok {
		h = execution.GitGrantHandle{GrantID: fmt.Sprintf("gg_issued_%03d", g.n), ExpiresAt: req.ExpiresAt}
	}
	h.Credential = execution.NewGitSecret(fmt.Sprintf("gitc_secret_%03d", g.n))
	g.handles[attempt] = h
	hook := g.AfterIssue
	if hook != nil {
		g.mu.Unlock()
		hook(attempt)
		g.mu.Lock()
	}
	return h, nil
}

// RevokeAttempt implements execution.GitGrants: the revocation in its own
// transaction that closes the issuance race.
func (g *FakeGitAuthority) RevokeAttempt(_ context.Context, _ domain.Scope, attempt domain.AttemptID) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.revoked[attempt] = true
	g.direct = append(g.direct, attempt)
	return nil
}

// DirectRevocations lists the attempts revoked through the port.
func (g *FakeGitAuthority) DirectRevocations() []domain.AttemptID {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]domain.AttemptID(nil), g.direct...)
}

// Issued lists the requests the authority received.
func (g *FakeGitAuthority) Issued() []execution.AttemptGrantRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]execution.AttemptGrantRequest(nil), g.issued...)
}

// Revokes counts the revocation calls.
func (g *FakeGitAuthority) Revokes() int { g.mu.Lock(); defer g.mu.Unlock(); return g.revokes }

var gitRevokedT = memdb.NewTable("test_git_revoked", func(r gitRevokedRow) string { return string(r.Scope.ProjectID) + "/" + string(r.Attempt) })

type gitRevokedRow struct {
	Scope   domain.Scope
	Attempt domain.AttemptID
}

// MemRevoker is the revocation seam of the memory store: it records the attempt
// in the caller's transaction, so a rolled back transition revokes nothing.
func (g *FakeGitAuthority) MemRevoker() func(tx memdb.Tx, scope domain.Scope, attempt domain.AttemptID, now time.Time) error {
	return func(tx memdb.Tx, scope domain.Scope, attempt domain.AttemptID, _ time.Time) error {
		g.mu.Lock()
		g.revokes++
		err := g.RevokeErr
		g.mu.Unlock()
		if err != nil {
			return err
		}
		return gitRevokedT.Put(tx, gitRevokedRow{Scope: scope, Attempt: attempt})
	}
}

// MemRevoked reports whether the attempt's grants were revoked in a committed
// transaction of the memory kernel.
func (g *FakeGitAuthority) MemRevoked(db *memdb.Store, scope domain.Scope, attempt domain.AttemptID) bool {
	var ok bool
	_ = db.View(context.Background(), scope.TenantID, func(tx memdb.Tx) error {
		_, ok = gitRevokedT.Get(tx, string(scope.ProjectID)+"/"+string(attempt))
		return nil
	})
	if ok {
		g.mu.Lock()
		g.revoked[attempt] = true
		g.mu.Unlock()
	}
	return ok
}

// PGRevoker is the revocation seam of the PostgreSQL store over a test table the
// fixture created: the row is written in the caller's open transaction.
func (g *FakeGitAuthority) PGRevoker() func(ctx context.Context, tx pgx.Tx, scope domain.Scope, attempt domain.AttemptID, now time.Time) (int, error) {
	return func(ctx context.Context, tx pgx.Tx, scope domain.Scope, attempt domain.AttemptID, now time.Time) (int, error) {
		g.mu.Lock()
		g.revokes++
		err := g.RevokeErr
		g.mu.Unlock()
		if err != nil {
			return 0, err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO t148_git_revocations (tenant_id, project_id, attempt_id) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`,
			string(scope.TenantID), string(scope.ProjectID), string(attempt))
		return int(tag.RowsAffected()), err
	}
}

// MarkRevoked tells the authority the attempt is no longer live (what gitauth
// learns from execution's attempt lease).
func (g *FakeGitAuthority) MarkRevoked(attempt domain.AttemptID) {
	g.mu.Lock()
	g.revoked[attempt] = true
	g.mu.Unlock()
}

var _ execution.GitGrants = (*FakeGitAuthority)(nil)

// ---- internal workspaces ----

// FakeInternalWorkspaces is the double of execution.InternalWorkspaces and of the
// memory store's internal writer peer: one workspace per work item, a writer
// gate that can be blocked, accepted tips per repository and a scripted
// checkpoint acceptance.
type FakeInternalWorkspaces struct {
	mu          sync.Mutex
	WorkspaceID string
	Version     domain.Version
	Accepted    map[string]string
	GateErr     error
	// CheckpointErr is returned by AcceptCheckpoint (ErrBusy: unsettled push;
	// ErrAdmissionRefused: a refused output).
	CheckpointErr error
	provisioned   [][]string
	checkpoints   []execution.CommitOutput
	holder        domain.AttemptID
	reserved      []domain.Version
	staleNext     bool
}

// NewFakeInternalWorkspaces returns a workspace at version 1.
func NewFakeInternalWorkspaces(workspaceID string) *FakeInternalWorkspaces {
	return &FakeInternalWorkspaces{WorkspaceID: workspaceID, Version: 1, Accepted: map[string]string{}}
}

// Provision implements execution.InternalWorkspaces.
func (w *FakeInternalWorkspaces) Provision(_ context.Context, _ domain.Scope, _ domain.WorkItemID, repositories []string) (execution.InternalWorkspace, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.provisioned = append(w.provisioned, append([]string(nil), repositories...))
	return execution.InternalWorkspace{ID: w.WorkspaceID}, nil
}

// AdmitWriter implements execution.InternalWorkspaces.
func (w *FakeInternalWorkspaces) AdmitWriter(_ context.Context, _ domain.Scope, workspaceID string) (execution.WriterAdmission, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.GateErr != nil {
		return execution.WriterAdmission{}, w.GateErr
	}
	if workspaceID != w.WorkspaceID {
		return execution.WriterAdmission{}, execution.ErrNotFound
	}
	acc := make(map[string]string, len(w.Accepted))
	for k, v := range w.Accepted {
		acc[k] = v
	}
	out := execution.WriterAdmission{Version: w.Version, Accepted: acc}
	if w.staleNext {
		// The workspace changes right after the gate was checked.
		w.staleNext = false
		w.Version++
	}
	return out, nil
}

// StaleGate makes the next gate answer stale: the workspace version moves right
// after it was read.
func (w *FakeInternalWorkspaces) StaleGate() { w.mu.Lock(); w.staleNext = true; w.mu.Unlock() }

// AcceptCheckpoint implements execution.InternalWorkspaces.
func (w *FakeInternalWorkspaces) AcceptCheckpoint(_ context.Context, _ domain.Scope, _ domain.AttemptID, out execution.CommitOutput) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.CheckpointErr != nil {
		return w.CheckpointErr
	}
	w.checkpoints = append(w.checkpoints, out)
	return nil
}

// ReserveInternalWriter is the memory store's peer: the slot is reserved only at
// the version the gate was checked at.
func (w *FakeInternalWorkspaces) ReserveInternalWriter(_ memdb.Tx, at *execution.Attempt, observed domain.Version) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reserved = append(w.reserved, observed)
	switch {
	case w.holder != "" && w.holder != at.ID:
		return fmt.Errorf("%w: the writer slot is held", execution.ErrBusy)
	case observed != w.Version:
		return fmt.Errorf("%w: the workspace changed since the gate", execution.ErrStale)
	}
	w.holder = at.ID
	return nil
}

// Provisioned lists the repositories each Provision call named.
func (w *FakeInternalWorkspaces) Provisioned() [][]string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([][]string(nil), w.provisioned...)
}

// Checkpoints lists the accepted outputs in order.
func (w *FakeInternalWorkspaces) Checkpoints() []execution.CommitOutput {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]execution.CommitOutput(nil), w.checkpoints...)
}

// Holder is the attempt holding the writer slot.
func (w *FakeInternalWorkspaces) Holder() domain.AttemptID {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.holder
}

// Bump advances the workspace version (a change since the gate).
func (w *FakeInternalWorkspaces) Bump() { w.mu.Lock(); w.Version++; w.mu.Unlock() }

var _ execution.InternalWorkspaces = (*FakeInternalWorkspaces)(nil)

// ---- tracking runtime ----

// TrackingRuntime is a runtime double that knows which workloads it created, as a
// real backend does: Observe affirms a workload absent (ErrNotFound) until Start
// created it. It delegates everything else to Inner (the fixture's Manual).
type TrackingRuntime struct {
	execution.Runtime // Runtime is the inner runtime (the fixture's Manual)
	mu                sync.Mutex
	started           map[domain.AttemptID]bool
	lose              int
}

// NewTrackingRuntime returns a tracker over no runtime yet: set Inner before use.
func NewTrackingRuntime() *TrackingRuntime {
	return &TrackingRuntime{started: map[domain.AttemptID]bool{}}
}

// LoseStartResponses makes the next n Start calls create or adopt the workload and
// then fail with an unknown outcome, as a lost response does.
func (r *TrackingRuntime) LoseStartResponses(n int) { r.mu.Lock(); r.lose = n; r.mu.Unlock() }

// Start implements execution.Runtime.
func (r *TrackingRuntime) Start(ctx context.Context, spec execution.AttemptSpec) (execution.Workload, error) {
	w, err := r.Runtime.Start(ctx, spec)
	if err != nil {
		return w, err
	}
	r.mu.Lock()
	r.started[spec.AttemptID] = true
	lose := r.lose > 0
	if lose {
		r.lose--
	}
	r.mu.Unlock()
	if lose {
		return execution.Workload{}, fmt.Errorf("%w: the response was lost", execution.ErrUnavailable)
	}
	return w, nil
}

// Observe implements execution.Runtime.
func (r *TrackingRuntime) Observe(ctx context.Context, w execution.Workload) (execution.Observation, error) {
	r.mu.Lock()
	ok := r.started[w.AttemptID]
	r.mu.Unlock()
	if !ok {
		return execution.Observation{}, execution.ErrNotFound
	}
	return r.Runtime.Observe(ctx, w)
}
