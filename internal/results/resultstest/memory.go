// Package resultstest holds the in-memory adapters of the results module's
// consumer-owned ports and the exported Contract suite they share with the
// PostgreSQL-backed composition (SD-13, SD-A11).
//
// Memory is the `development` environment's counterpart of the join the
// production composition makes between four owners: runtime authority (grants
// and sessions), execution (attempts, accepted result hash, acknowledgment),
// the artifact owner (evidence and pins) and the workspace owner (publication
// receipts and Git compare-and-swap). All durable state lives in one memdb
// store, so every use-case call is one all-or-nothing Update, and a Tx reads
// one tenant only.
//
// What it does not model (SD-14): SQL, row-level security, lock order and
// isolation levels, real signed tokens, Git object storage, object bytes and
// restart persistence. The remote branch is an in-memory compare-and-swap
// reference outside the store, because a real Git push is not part of any
// database transaction: that is what makes "Git succeeded, the database did
// not" and "the push outcome is unknown" reachable here. A pass on this
// adapter never certifies atomicity of the real transaction; T-026 runs the
// same Contract against the PostgreSQL composition.
package resultstest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/results"
)

// Clock is a controllable clock.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock starts at a fixed instant.
func NewClock() *Clock {
	return &Clock{t: time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)}
}

// Now returns the current instant.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// Operations a fault can target.
const (
	// OpCommit is the guarded result commit of the Finalizer.
	OpCommit = "commit"
	// OpRecord is the transaction that records a publication intent.
	OpRecord = "record"
	// OpSettle is the transaction that records a publication's outcome after
	// the remote answered.
	OpSettle = "settle"
)

var errInjected = errors.New("resultstest: injected failure")

type faults struct {
	mu      sync.Mutex
	pending map[string]int
	lose    int // acknowledgments to lose after a successful commit
	push    []pushFault
}

// pushFault is one injected outcome of the next remote push.
type pushFault struct{ applied bool }

func (f *faults) arm(op string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pending == nil {
		f.pending = map[string]int{}
	}
	f.pending[op] = n
}

// take consumes one armed failure of op.
func (f *faults) take(op string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pending[op] > 0 {
		f.pending[op]--
		return true
	}
	return false
}

func (f *faults) takeLoss() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lose > 0 {
		f.lose--
		return true
	}
	return false
}

func (f *faults) takePush() (pushFault, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.push) == 0 {
		return pushFault{}, false
	}
	p := f.push[0]
	f.push = f.push[1:]
	return p, true
}

// Rows.
type attemptRow struct {
	Scope      domain.Scope
	Run        domain.RunID
	ID         domain.AttemptID
	Pinned     runtimev1.Context
	StartedAt  time.Time
	Status     string // active, succeeded, failed, cancelled, timed_out
	ResultHash runtimev1.Digest
	Outcome    runtimev1.AttemptStatus
	FinishedAt time.Time
	Late       []runtimev1.Digest // results rejected as evidence
}

type grantRow struct {
	Session string
	Scope   domain.Scope
	Run     domain.RunID
	Attempt domain.AttemptID
	Actions map[results.Action]bool
	Expires time.Time
	Revoked bool
}

type artifactRow struct {
	Evidence results.ArtifactEvidence
	Removed  bool
	Pinned   bool
}

type publicationRow struct {
	Attempt     domain.AttemptID
	Repo        string
	Branch      string
	Request     runtimev1.SourceProposalRequest
	Status      string // pending, accepted, conflict
	ObservedTip string
}

var (
	attempts = memdb.NewTable("results_attempts", func(r attemptRow) string { return string(r.ID) })
	grants   = memdb.NewTable("results_grants", func(r grantRow) string { return r.Session })
	artifact = memdb.NewTable("results_artifacts", func(r artifactRow) string { return string(r.Evidence.ID) })
	pubs     = memdb.NewTable("results_publications", func(r publicationRow) string { return string(r.Attempt) })
)

// Remote is the in-memory branch tip store: expected-tip compare-and-swap.
type Remote struct {
	mu   sync.Mutex
	tips map[string]runtimev1.GitCommit
}

func key(repo, branch string) string { return repo + "#" + branch }

// Tip returns the branch tip ("" when unknown).
func (r *Remote) Tip(repo, branch string) runtimev1.GitCommit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tips[key(repo, branch)]
}

// Set moves the branch to tip without any check, as another writer would.
func (r *Remote) Set(repo, branch string, tip runtimev1.GitCommit) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tips == nil {
		r.tips = map[string]runtimev1.GitCommit{}
	}
	r.tips[key(repo, branch)] = tip
}

// cas moves the branch from expected to commit. The same commit already at the
// tip is success (an idempotent repeat). The observed tip is returned on loss.
func (r *Remote) cas(repo, branch string, expected, commit runtimev1.GitCommit) (observed runtimev1.GitCommit, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tips == nil {
		r.tips = map[string]runtimev1.GitCommit{}
	}
	cur := r.tips[key(repo, branch)]
	switch cur {
	case commit:
		return cur, true
	case expected:
		r.tips[key(repo, branch)] = commit
		return commit, true
	}
	return cur, false
}

// Memory is the in-memory results world.
type Memory struct {
	db     *memdb.Store
	Clock  *Clock
	Remote *Remote
	f      *faults
	seq    atomic.Int64
}

// NewMemory returns an empty world.
func NewMemory() *Memory {
	m := &Memory{Clock: NewClock(), Remote: &Remote{}, f: &faults{}}
	m.db = memdb.New()
	return m
}

// update is one all-or-nothing transaction of the named operation. An armed
// failure of the operation discards everything fn wrote, which is what a
// commit whose outcome is lost to the caller, and never applied, looks like.
func (m *Memory) update(ctx context.Context, tenant domain.TenantID, op string, fn func(memdb.Tx) error) error {
	return m.db.Update(ctx, tenant, func(tx memdb.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		if m.f.take(op) {
			return errInjected
		}
		return nil
	})
}

// Dependencies returns the in-memory ports, ready for results.NewService.
func (m *Memory) Dependencies() results.Dependencies {
	return results.Dependencies{
		Authority: (*authority)(m), Contexts: (*execution)(m), Evidence: (*evidence)(m),
		Finalizer: (*execution)(m), Source: (*source)(m),
	}
}

// Service builds the use-case layer over the in-memory ports.
func (m *Memory) Service(tb testing.TB) *results.Service {
	tb.Helper()
	svc, err := results.NewService(m.Dependencies(), results.Options{})
	if err != nil {
		tb.Fatal(err)
	}
	return svc
}

// Subject is the memory adapter as a Contract subject.
func (m *Memory) Subject() Subject {
	return Subject{Deps: m.Dependencies(), World: memWorld{m}, Scopes: Scopes{A: ScopeA, A2: ScopeA2, B: ScopeB}}
}

// memWorld adapts Memory's fixtures to the World the Contract drives.
type memWorld struct{ m *Memory }

func (w memWorld) NewAttempt(tb testing.TB, scope domain.Scope, o AttemptOptions) Attempt {
	tb.Helper()
	return w.m.NewAttempt(tb, scope, o)
}

func (w memWorld) Bearer(tb testing.TB, a Attempt, action results.Action) string {
	return w.m.Bearer(tb, a, action)
}

func (w memWorld) Artifact(tb testing.TB, a Attempt, s ArtifactSpec) domain.ArtifactID {
	return w.m.Artifact(tb, a, s)
}

func (w memWorld) Revoke(tb testing.TB, a Attempt) { w.m.Revoke(tb, a) }

// Bundle returns a new commit on the attempt's expected tip and the bundle
// artifact that carries it.
func (w memWorld) Bundle(tb testing.TB, a Attempt) (runtimev1.GitCommit, domain.ArtifactID) {
	tb.Helper()
	commit := runtimev1.GitCommit(hexOf(w.m.seq.Add(1) + 1<<40))
	return commit, w.m.Artifact(tb, a, ArtifactSpec{Role: runtimev1.RoleBundle})
}

// MoveTip advances the branch as a human push would and returns the new tip.
func (w memWorld) MoveTip(tb testing.TB, a Attempt) runtimev1.GitCommit {
	tb.Helper()
	tip := runtimev1.GitCommit(hexOf(w.m.seq.Add(1) + 2<<40))
	w.m.MoveTip(tb, a, tip)
	return tip
}

func (w memWorld) Tip(tb testing.TB, a Attempt) runtimev1.GitCommit {
	tb.Helper()
	s := a.Pinned.Workspace.Source
	return w.m.Remote.Tip(s.RepositoryURL, *s.Branch)
}

// FailNext makes the next n transactions of op fail at commit: nothing they
// wrote is kept and the caller sees an unclassified error.
func (m *Memory) FailNext(op string, n int) { m.f.arm(op, n) }

// LoseNextAcknowledgments makes the next n result commits succeed durably but
// return an error, as when the response is lost after the transaction.
func (m *Memory) LoseNextAcknowledgments(n int) {
	m.f.mu.Lock()
	m.f.lose = n
	m.f.mu.Unlock()
}

// FailNextPush makes the next remote push report an unknown outcome. applied
// says whether the push reached the remote first.
func (m *Memory) FailNextPush(applied bool) {
	m.f.mu.Lock()
	m.f.push = append(m.f.push, pushFault{applied: applied})
	m.f.mu.Unlock()
}

// ---- fixtures (World) ----

// AttemptOptions describe an attempt to create.
type AttemptOptions struct {
	// Writer gives the attempt write access to a source branch.
	Writer bool
	// NoOutputs declares no outputs instead of the single artifact output
	// "report".
	NoOutputs bool
	// Timeout overrides the 60 second execution limit. Subjects that do not
	// control time ignore it; no Contract case depends on it.
	Timeout time.Duration
}

// Attempt is a created attempt.
type Attempt struct {
	Scope  domain.Scope
	Run    domain.RunID
	ID     domain.AttemptID
	Pinned runtimev1.Context
}

// ArtifactSpec describes an artifact of an attempt.
type ArtifactSpec struct {
	// Role defaults to output. The artifact is a file.
	Role runtimev1.ArtifactRole
	// Incomplete leaves the upload unfinished.
	Incomplete bool
}

// Branch and base commit of writer attempts. Every attempt has its own
// repository, so attempts never share a branch tip unless a test moves it.
const (
	Branch = "main"
	// BaseCommit is the pinned input commit and expected tip of writers.
	BaseCommit = runtimev1.GitCommit("1111111111111111111111111111111111111111")
)

// cloneContext deep-copies a context through its wire form.
func cloneContext(tb testing.TB, c runtimev1.Context) runtimev1.Context {
	if tb != nil {
		tb.Helper()
	}
	b, err := json.Marshal(c)
	if err == nil {
		var out runtimev1.Context
		if err = json.Unmarshal(b, &out); err == nil {
			return out
		}
	}
	panic(fmt.Sprintf("resultstest: a pinned context does not survive its wire form: %v", err))
}

func hexOf(n int64) string { return fmt.Sprintf("%040x", n) }

// NewAttempt creates an active attempt with a pinned context and a grant that
// allows the actions its access permits.
func (m *Memory) NewAttempt(tb testing.TB, scope domain.Scope, o AttemptOptions) Attempt {
	tb.Helper()
	n := m.seq.Add(1)
	id, run := domain.AttemptID(fmt.Sprintf("att_%d", n)), domain.RunID(fmt.Sprintf("run_%d", n))
	var outputs []runtimev1.OutputDecl
	if o.NoOutputs {
		outputs = []runtimev1.OutputDecl{}
	} else {
		path := "/kodama/outputs/report"
		outputs = []runtimev1.OutputDecl{{Name: "report", Type: runtimev1.OutputArtifact, Path: &path}}
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	repo := fmt.Sprintf("https://git.example.invalid/acme/%s.git", run)
	c := runtimev1.Context{
		TenantID: scope.TenantID, ProjectID: scope.ProjectID, RunID: run, StepID: "step", AttemptID: id, ActionKey: "ak_" + string(run),
		Definition: runtimev1.Definition{ID: "pd_results", Digest: runtimev1.Digest("sha256:" + strings.Repeat("2", 64))},
		Command:    runtimev1.Command{Image: "registry.invalid/kodama/step@sha256:" + strings.Repeat("0", 64), Command: []string{"/bin/sh"}, Args: []string{"-c", "true"}, Env: map[string]string{}},
		Workspace:  runtimev1.Workspace{Access: runtimev1.AccessRead, Source: &runtimev1.Source{RepositoryURL: repo, Commit: BaseCommit}},
		Inputs:     map[string]runtimev1.Value{}, Outputs: outputs, Secrets: []runtimev1.SecretBinding{},
		Limits: runtimev1.Limits{TimeoutSeconds: int64(timeout / time.Second), MaxOutputBytes: 1 << 20, MaxOutputFiles: 16, MaxDiagnosticBytes: 1 << 20},
	}
	actions := map[results.Action]bool{results.ActionResultSubmit: true}
	if o.Writer {
		branch, tip := Branch, BaseCommit
		c.Workspace = runtimev1.Workspace{Access: runtimev1.AccessWrite, Source: &runtimev1.Source{RepositoryURL: repo, Commit: BaseCommit, Branch: &branch, ExpectedTip: &tip}}
		actions[results.ActionSourcePropose] = true
		m.Remote.Set(repo, Branch, BaseCommit)
	}
	now := m.Clock.Now()
	// memdb stores values as given, and a context holds maps, slices and
	// pointers: the stored document is a private deep copy, and the caller's
	// copy is another one.
	a := Attempt{Scope: scope, Run: run, ID: id, Pinned: cloneContext(tb, c)}
	err := m.db.Update(context.Background(), scope.TenantID, func(tx memdb.Tx) error {
		if err := attempts.Put(tx, attemptRow{Scope: scope, Run: run, ID: id, Pinned: cloneContext(tb, c), StartedAt: now, Status: "active"}); err != nil {
			return err
		}
		return grants.Put(tx, grantRow{Session: "ses_" + string(id), Scope: scope, Run: run, Attempt: id, Actions: actions, Expires: now.Add(time.Hour)})
	})
	if err != nil {
		tb.Fatal(err)
	}
	return a
}

// Bearer returns the credential of the attempt's grant. The action is part of
// the request, not of the token: a grant that does not cover it refuses it.
func (m *Memory) Bearer(tb testing.TB, a Attempt, _ results.Action) string {
	tb.Helper()
	return "rtm." + string(a.Scope.TenantID) + ".ses_" + string(a.ID)
}

// Artifact registers an artifact of the attempt and returns its ID.
func (m *Memory) Artifact(tb testing.TB, a Attempt, s ArtifactSpec) domain.ArtifactID {
	tb.Helper()
	if s.Role == "" {
		s.Role = runtimev1.RoleOutput
	}
	id := domain.ArtifactID(fmt.Sprintf("art_%d", m.seq.Add(1)))
	ev := results.ArtifactEvidence{ID: id, Scope: a.Scope, Attempt: a.ID, Kind: runtimev1.KindFile, Role: s.Role, Usable: !s.Incomplete,
		Digest: runtimev1.Digest("sha256:" + strings.Repeat("0", 64))}
	if err := m.db.Update(context.Background(), a.Scope.TenantID, func(tx memdb.Tx) error { return artifact.Put(tx, artifactRow{Evidence: ev}) }); err != nil {
		tb.Fatal(err)
	}
	return id
}

// Revoke cancels the attempt and revokes its grant in one transaction, as the
// execution owner's cancellation does.
func (m *Memory) Revoke(tb testing.TB, a Attempt) {
	tb.Helper()
	if err := m.Cancel(context.Background(), a.Scope, a.ID); err != nil {
		tb.Fatal(err)
	}
}

// Cancel is the cancellation transaction.
func (m *Memory) Cancel(ctx context.Context, scope domain.Scope, id domain.AttemptID) error {
	return m.db.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		at, ok := attempts.Get(tx, string(id))
		if !ok || at.Scope != scope {
			return results.ErrNotFound
		}
		if at.Status == "active" {
			at.Status, at.FinishedAt = "cancelled", m.Clock.Now()
			if err := attempts.Put(tx, at); err != nil {
				return err
			}
		}
		return m.revokeGrants(tx, id)
	})
}

func (m *Memory) revokeGrants(tx memdb.Tx, id domain.AttemptID) error {
	var rows []grantRow
	grants.Scan(tx, func(g grantRow) bool {
		if g.Attempt == id {
			rows = append(rows, g)
		}
		return true
	})
	for _, g := range rows {
		g.Revoked = true
		if err := grants.Put(tx, g); err != nil {
			return err
		}
	}
	return nil
}

// MoveTip advances the attempt's branch as another writer would.
func (m *Memory) MoveTip(tb testing.TB, a Attempt, tip runtimev1.GitCommit) {
	tb.Helper()
	s := a.Pinned.Workspace.Source
	if s == nil || s.Branch == nil {
		tb.Fatal("the attempt has no branch")
	}
	m.Remote.Set(s.RepositoryURL, *s.Branch, tip)
}

// ---- inspection ----

// State is an attempt's durable result state.
type State struct {
	Status     string
	ResultHash runtimev1.Digest
	Outcome    runtimev1.AttemptStatus
	FinishedAt time.Time
	// Late lists the hashes of results rejected as evidence.
	Late []runtimev1.Digest
}

// State reads the attempt's durable state.
func (m *Memory) State(tb testing.TB, a Attempt) State {
	tb.Helper()
	var s State
	err := m.db.View(context.Background(), a.Scope.TenantID, func(tx memdb.Tx) error {
		at, ok := attempts.Get(tx, string(a.ID))
		if !ok {
			return results.ErrNotFound
		}
		s = State{Status: at.Status, ResultHash: at.ResultHash, Outcome: at.Outcome, FinishedAt: at.FinishedAt, Late: append([]runtimev1.Digest(nil), at.Late...)}
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
	return s
}

// Publication is an attempt's durable publication receipt.
type Publication struct {
	Status      string
	Commit      runtimev1.GitCommit
	ObservedTip runtimev1.GitCommit
}

// Publication reads the receipt; ok is false when no intent was recorded.
func (m *Memory) Publication(tb testing.TB, a Attempt) (p Publication, ok bool) {
	tb.Helper()
	_ = m.db.View(context.Background(), a.Scope.TenantID, func(tx memdb.Tx) error {
		row, found := pubs.Get(tx, string(a.ID))
		p, ok = Publication{Status: row.Status, Commit: row.Request.Commit, ObservedTip: runtimev1.GitCommit(row.ObservedTip)}, found
		return nil
	})
	return p, ok
}

// Pinned reports whether the artifact is pinned against retention.
func (m *Memory) Pinned(tb testing.TB, a Attempt, id domain.ArtifactID) bool {
	tb.Helper()
	var pinned bool
	_ = m.db.View(context.Background(), a.Scope.TenantID, func(tx memdb.Tx) error {
		row, _ := artifact.Get(tx, string(id))
		pinned = row.Pinned
		return nil
	})
	return pinned
}

// Retain applies retention to the artifact: a pinned artifact is kept and
// false is returned.
func (m *Memory) Retain(tb testing.TB, a Attempt, id domain.ArtifactID) (removed bool) {
	tb.Helper()
	err := m.db.Update(context.Background(), a.Scope.TenantID, func(tx memdb.Tx) error {
		row, ok := artifact.Get(tx, string(id))
		if !ok || row.Pinned || row.Evidence.Scope != a.Scope {
			return nil
		}
		row.Removed, removed = true, true
		return artifact.Put(tx, row)
	})
	if err != nil {
		tb.Fatal(err)
	}
	return removed
}

// ---- authority ----

type session struct {
	m       *Memory
	tenant  domain.TenantID
	id      string
	attempt domain.AttemptID
}

type sessionKey struct{}

type authority Memory

var _ results.Authority = (*authority)(nil)

func (a *authority) Authenticate(ctx context.Context, req results.AuthRequest) (results.Principal, error) {
	m := (*Memory)(a)
	parts := strings.Split(req.Bearer, ".")
	if len(parts) != 3 || parts[0] != "rtm" || domain.TenantID(parts[1]).Validate() != nil {
		return results.Principal{}, results.Fail(results.ErrUnauthenticated, nil, "the credential is not valid")
	}
	tenant := domain.TenantID(parts[1])
	var p results.Principal
	err := m.db.View(ctx, tenant, func(tx memdb.Tx) error {
		g, ok := grants.Get(tx, parts[2])
		if !ok || g.Attempt != req.Attempt || g.Scope.TenantID != tenant {
			return results.Fail(results.ErrUnauthenticated, nil, "the token is not valid for this attempt")
		}
		if err := m.check(tx, g, req.Action); err != nil {
			return err
		}
		p = results.Principal{Scope: g.Scope, RunID: g.Run, AttemptID: g.Attempt, Action: req.Action, Proof: session{m: m, tenant: tenant, id: g.Session, attempt: g.Attempt}}
		return nil
	})
	return p, err
}

// Bind implements results.Authority: a principal this authority did not issue
// is refused, because an unbound context would skip the in-transaction check.
func (a *authority) Bind(ctx context.Context, p results.Principal) (context.Context, error) {
	s, ok := p.Proof.(session)
	if !ok || s.m != (*Memory)(a) || s.attempt != p.AttemptID {
		return ctx, results.Fail(results.ErrUnauthenticated, nil, "the principal was not issued by this authority")
	}
	return context.WithValue(ctx, sessionKey{}, s), nil
}

// check is the session check: the first, cheap one in Authenticate and the
// authoritative one repeated inside the protected mutation's transaction.
// result.submit keeps the receipt exception: after the attempt leaves active
// an unrevoked grant still reaches execution, which answers the original
// acknowledgment, the conflict or the late-result rejection.
func (m *Memory) check(tx memdb.Tx, g grantRow, action results.Action) error {
	at, ok := attempts.Get(tx, string(g.Attempt))
	if !ok || at.Scope != g.Scope {
		return results.Fail(results.ErrUnauthenticated, nil, "the token is not valid for this attempt")
	}
	receipt := action == results.ActionResultSubmit && !g.Revoked
	if !receipt && (g.Revoked || at.Status != "active") {
		return results.Fail(results.ErrAttemptRevoked, nil, "the attempt is no longer authorized")
	}
	if g.Revoked {
		return results.Fail(results.ErrAttemptRevoked, nil, "the attempt is no longer authorized")
	}
	if !g.Actions[action] {
		return results.Fail(results.ErrForbidden, nil, "the action is not granted")
	}
	if !m.Clock.Now().Before(g.Expires) {
		return results.Fail(results.ErrUnauthenticated, nil, "the credential is not valid")
	}
	return nil
}

// guard repeats the session check inside the mutation's transaction. An
// unbound context fails closed.
func (m *Memory) guard(ctx context.Context, tx memdb.Tx, scope domain.Scope, attempt domain.AttemptID, action results.Action) error {
	s, ok := ctx.Value(sessionKey{}).(session)
	if !ok || s.m != m || s.tenant != scope.TenantID || s.attempt != attempt {
		return results.Fail(results.ErrUnauthenticated, nil, "the request is not bound to a session")
	}
	g, ok := grants.Get(tx, s.id)
	if !ok || g.Attempt != attempt || g.Scope != scope {
		return results.Fail(results.ErrUnauthenticated, nil, "the token is not valid for this attempt")
	}
	return m.check(tx, g, action)
}

// ---- execution ----

type execution Memory

var (
	_ results.Finalizer = (*execution)(nil)
	_ results.Contexts  = (*execution)(nil)
)

func (e *execution) Pinned(ctx context.Context, scope domain.Scope, id domain.AttemptID) (runtimev1.Context, error) {
	var c runtimev1.Context
	err := e.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		at, ok := attempts.Get(tx, string(id))
		if !ok || at.Scope != scope {
			return results.Fail(results.ErrNotFound, nil, "the attempt was not found")
		}
		c = cloneContext(nil, at.Pinned)
		return nil
	})
	return c, err
}

func (e *execution) Accepted(ctx context.Context, scope domain.Scope, id domain.AttemptID) (hash runtimev1.Digest, found bool, err error) {
	err = e.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		at, ok := attempts.Get(tx, string(id))
		if !ok || at.Scope != scope {
			return results.Fail(results.ErrNotFound, nil, "the attempt was not found")
		}
		hash, found = at.ResultHash, at.ResultHash != ""
		return nil
	})
	return hash, found, err
}

// Commit records the result idempotently under attempt ID and hash, inside one
// transaction that repeats the session check first.
func (e *execution) Commit(ctx context.Context, c results.Commit) (results.Acknowledgment, error) {
	m := (*Memory)(e)
	var ack results.Acknowledgment
	var rejected error
	err := m.update(ctx, c.Scope.TenantID, OpCommit, func(tx memdb.Tx) error {
		if err := m.guard(ctx, tx, c.Scope, c.AttemptID, results.ActionResultSubmit); err != nil {
			return err
		}
		at, ok := attempts.Get(tx, string(c.AttemptID))
		if !ok || at.Scope != c.Scope {
			return results.Fail(results.ErrNotFound, nil, "the attempt was not found")
		}
		hash := c.Request.ResultHash
		if at.ResultHash != "" {
			if at.ResultHash != hash {
				return results.Fail(results.ErrResultConflict, nil, "a different result was already accepted")
			}
			ack = acknowledgment(at)
			return nil
		}
		now := m.Clock.Now()
		late := func(status string) error {
			// A late result is kept as evidence and cannot progress the attempt.
			at.Late = append(append([]runtimev1.Digest(nil), at.Late...), hash)
			if status != "" {
				at.Status, at.FinishedAt = status, now
			}
			rejected = results.Fail(results.ErrAttemptRevoked, nil, "the attempt is no longer authorized")
			if status != "" {
				if err := m.revokeGrants(tx, at.ID); err != nil {
					return err
				}
			}
			return attempts.Put(tx, at)
		}
		if at.Status != "active" {
			return late("")
		}
		if now.After(at.StartedAt.Add(time.Duration(at.Pinned.Limits.TimeoutSeconds) * time.Second)) {
			return late("timed_out")
		}
		res := c.Request.Result
		if cerr := runtimev1.CheckResult(res, at.Pinned, c.Artifacts); cerr != nil {
			var ce *runtimev1.ContractError
			if !errors.As(cerr, &ce) || ce.Code != runtimev1.CodeOutputInvalid || res.Outcome != runtimev1.OutcomeCompleted {
				return results.Fail(results.ErrInvalidRequest, cerr, "the result does not satisfy the attempt's context")
			}
			// Missing or wrong outputs fail the attempt durably; no success
			// output is exposed and nothing is retried.
			at.Status, at.Outcome = "failed", runtimev1.AttemptFailed
		} else {
			if res.SourceCommit != nil {
				if err := e.settled(tx, at, *res.SourceCommit); err != nil {
					return err
				}
			}
			at.Status, at.Outcome = "succeeded", runtimev1.AttemptSucceeded
			if res.Outcome != runtimev1.OutcomeCompleted {
				at.Status, at.Outcome = "failed", runtimev1.AttemptFailed
			}
		}
		at.ResultHash, at.FinishedAt = hash, now
		if err := attempts.Put(tx, at); err != nil {
			return err
		}
		ack = acknowledgment(at)
		return nil
	})
	if err != nil {
		return results.Acknowledgment{}, err
	}
	if rejected != nil {
		return results.Acknowledgment{}, rejected
	}
	if m.f.takeLoss() {
		return results.Acknowledgment{}, errors.New("resultstest: response lost after commit")
	}
	return ack, nil
}

func acknowledgment(at attemptRow) results.Acknowledgment {
	return results.Acknowledgment{AttemptID: at.ID, ResultHash: at.ResultHash, Status: at.Outcome, AcceptedAt: at.FinishedAt}
}

// settled enforces writer settlement: an unsettled or different publication
// blocks success. /result has no pending response, so both are "retry".
func (e *execution) settled(tx memdb.Tx, at attemptRow, commit runtimev1.GitCommit) error {
	p, ok := pubs.Get(tx, string(at.ID))
	if !ok {
		return results.Fail(results.ErrUnavailable, nil, "the writer's publication is unsettled; repeat the result")
	}
	if p.Status != "accepted" || p.Request.Commit != commit {
		return results.Fail(results.ErrUnavailable, nil, "the writer's publication is unsettled; repeat the result")
	}
	return nil
}

// ---- evidence ----

type evidence Memory

var _ results.Evidence = (*evidence)(nil)

func (e *evidence) References(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, ids []domain.ArtifactID) (map[domain.ArtifactID]results.ArtifactEvidence, error) {
	out := map[domain.ArtifactID]results.ArtifactEvidence{}
	err := e.db.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		for _, id := range ids {
			row, ok := artifact.Get(tx, string(id))
			if ok && !row.Removed && row.Evidence.Scope == scope && row.Evidence.Attempt == attempt {
				out[id] = row.Evidence
			}
		}
		return nil
	})
	return out, err
}

func (e *evidence) Protect(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, ids []domain.ArtifactID) error {
	return e.db.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		rows := make([]artifactRow, 0, len(ids))
		for _, id := range ids {
			// Only usable artifacts the attempt itself produced are pinned, so
			// one attempt can never hold another's content; anything else is
			// skipped and rejected by the consumer's own checks.
			row, ok := artifact.Get(tx, string(id))
			if !ok || row.Removed || !row.Evidence.Usable || row.Evidence.Scope != scope || row.Evidence.Attempt != attempt {
				continue
			}
			row.Pinned = true
			rows = append(rows, row)
		}
		for _, row := range rows {
			if err := artifact.Put(tx, row); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---- source ----

type source Memory

var _ results.Source = (*source)(nil)

// Propose persists the publication intent before the remote is touched,
// performs the compare-and-swap outside any transaction, and settles the
// outcome in a second transaction. An unknown push outcome, or a settlement
// that did not commit, leaves the intent pending (202): success is never
// inferred, and a repeat of the identical proposal observes the same receipt.
func (s *source) Propose(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, req runtimev1.SourceProposalRequest) (results.SourceOutcome, error) {
	m := (*Memory)(s)
	var pub publicationRow
	err := m.update(ctx, scope.TenantID, OpRecord, func(tx memdb.Tx) error {
		if err := m.guard(ctx, tx, scope, attempt, results.ActionSourcePropose); err != nil {
			return err
		}
		at, ok := attempts.Get(tx, string(attempt))
		if !ok || at.Scope != scope || at.Pinned.Workspace.Source == nil || at.Pinned.Workspace.Source.Branch == nil {
			return results.Fail(results.ErrNotFound, nil, "the attempt was not found")
		}
		src := at.Pinned.Workspace.Source
		// The recorded receipt answers first: an identical repeat observes it,
		// a changed proposal is refused.
		if cur, ok := pubs.Get(tx, string(attempt)); ok {
			pub = cur
			if cur.Request != req {
				return &results.PublicationConflict{Reason: "changed_proposal", ObservedTip: string(m.Remote.Tip(cur.Repo, cur.Branch))}
			}
			return nil
		}
		// A new publication is validated before its intent is recorded and
		// before the remote is touched: the proposal must name the writer's
		// reserved expected tip (a moved tip is a conflict at the remote, never
		// a reason to rewrite the reservation), and the artifact must be this
		// attempt's complete bundle.
		if src.ExpectedTip == nil || req.ExpectedTip != *src.ExpectedTip {
			return results.Fail(results.ErrInvalidRequest, nil, "the expected tip does not match the reservation")
		}
		bundle, ok := artifact.Get(tx, string(req.BundleArtifactID))
		if !ok || bundle.Removed || bundle.Evidence.Scope != scope || bundle.Evidence.Attempt != attempt {
			return results.Fail(results.ErrNotFound, nil, "the resource was not found")
		}
		if !bundle.Evidence.Usable || bundle.Evidence.Role != runtimev1.RoleBundle {
			return results.Fail(results.ErrArtifactIncomplete, nil, "the bundle artifact is not complete")
		}
		pub = publicationRow{Attempt: attempt, Repo: src.RepositoryURL, Branch: *src.Branch, Request: req, Status: "pending"}
		return pubs.Put(tx, pub)
	})
	if err != nil {
		return results.SourceOutcome{}, err
	}
	switch pub.Status {
	case "accepted":
		return outcome(pub), nil
	case "conflict":
		return results.SourceOutcome{}, &results.PublicationConflict{Reason: "tip_moved", ObservedTip: pub.ObservedTip}
	}

	// The remote effect: not part of any database transaction.
	if f, ok := m.f.takePush(); ok {
		if f.applied {
			m.Remote.cas(pub.Repo, pub.Branch, pub.Request.ExpectedTip, pub.Request.Commit)
		}
		return pendingOutcome(pub), nil
	}
	observed, won := m.Remote.cas(pub.Repo, pub.Branch, pub.Request.ExpectedTip, pub.Request.Commit)
	status := "accepted"
	if !won {
		status = "conflict"
	}
	settleErr := m.update(ctx, scope.TenantID, OpSettle, func(tx memdb.Tx) error {
		cur, ok := pubs.Get(tx, string(attempt))
		if !ok {
			return results.Fail(results.ErrNotFound, nil, "the attempt was not found")
		}
		if cur.Status == "pending" {
			cur.Status, cur.ObservedTip = status, string(observed)
			if err := pubs.Put(tx, cur); err != nil {
				return err
			}
		}
		pub = cur
		return nil
	})
	if settleErr != nil {
		// The remote answered but the receipt could not be written: the
		// publication stays pending for the reconciler.
		return pendingOutcome(pub), nil
	}
	if pub.Status == "conflict" {
		return results.SourceOutcome{}, &results.PublicationConflict{Reason: "tip_moved", ObservedTip: pub.ObservedTip}
	}
	return outcome(pub), nil
}

func outcome(p publicationRow) results.SourceOutcome {
	c := p.Request.Commit
	return results.SourceOutcome{HTTPStatus: 200, Response: runtimev1.SourceProposalResponse{Status: runtimev1.PublishAccepted, Commit: c, AcceptedCommit: &c}}
}

func pendingOutcome(p publicationRow) results.SourceOutcome {
	return results.SourceOutcome{HTTPStatus: 202, Response: runtimev1.SourceProposalResponse{Status: runtimev1.PublishPending, Commit: p.Request.Commit}}
}

// Reconcile settles the tenant's pending publications from the remote, as the
// workspace reconciler does: the commit at the tip is accepted, a tip that
// moved elsewhere is a conflict, and an unchanged tip stays pending. It never
// changes an attempt's result. It returns the number settled.
func (m *Memory) Reconcile(ctx context.Context, tenant domain.TenantID) (int, error) {
	settled := 0
	err := m.db.Update(ctx, tenant, func(tx memdb.Tx) error {
		var pending []publicationRow
		pubs.Scan(tx, func(p publicationRow) bool {
			if p.Status == "pending" {
				pending = append(pending, p)
			}
			return true
		})
		for _, p := range pending {
			tip := m.Remote.Tip(p.Repo, p.Branch)
			switch {
			case tip == p.Request.Commit:
				p.Status = "accepted"
			case tip != p.Request.ExpectedTip:
				p.Status, p.ObservedTip = "conflict", string(tip)
			default:
				continue
			}
			if err := pubs.Put(tx, p); err != nil {
				return err
			}
			settled++
		}
		return nil
	})
	return settled, err
}
