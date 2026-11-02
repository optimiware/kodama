package workspacetest

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // object names of the in-memory commit graph, not security
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"testing"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/workspace"
)

// This file is the explicit peer fakes of the internally hosted model (T-147):
// the tenant Git worker (workspace.Hosts), the repository module, the push
// records of gitpush, its writer gate and the server-side push inspector. They
// hold state the tests can read and fault hooks for the unknown outcomes.

// ---- Hosts ----

type hostKey struct {
	tenant domain.TenantID
	repo   string
}

type hostRepo struct {
	refs    map[string]string
	journal []gitv1.JournalEntry
	head    int64
}

// FakeHosts implements workspace.Hosts over in-memory repositories.
type FakeHosts struct {
	mu    sync.Mutex
	repos map[hostKey]*hostRepo

	// CreateOps and PinOps list the operation identifiers of every call, in order.
	CreateOps []string
	PinOps    []string
	// ReadErr, when set, makes ReadRefValues and ReadJournal fail (worker outage).
	ReadErr error
	// BeforeRead, when set, runs before the next ReadRefValues is answered
	// (outside the host's lock), once: an interleaving point for tests.
	BeforeRead func(repo, ref string)

	createFault, pinFault fault
}

type fault struct {
	err     error
	applied bool // the request is applied and its response lost
	times   int  // remaining; <0: always
}

var _ workspace.Hosts = (*FakeHosts)(nil)

func NewFakeHosts() *FakeHosts { return &FakeHosts{repos: map[hostKey]*hostRepo{}} }

func (h *FakeHosts) repo(tenant domain.TenantID, id string) *hostRepo {
	k := hostKey{tenant, id}
	r := h.repos[k]
	if r == nil {
		r = &hostRepo{refs: map[string]string{}}
		h.repos[k] = r
	}
	return r
}

// SetRef sets a ref directly (a push, a seed or a human update).
func (h *FakeHosts) SetRef(tenant domain.TenantID, repo, ref string, commit workspace.Commit) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.repo(tenant, repo).refs[ref] = string(commit)
}

// DeleteRef removes a ref.
func (h *FakeHosts) DeleteRef(tenant domain.TenantID, repo, ref string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.repo(tenant, repo).refs, ref)
}

// Ref reads a ref ("" when absent).
func (h *FakeHosts) Ref(tenant domain.TenantID, repo, ref string) workspace.Commit {
	h.mu.Lock()
	defer h.mu.Unlock()
	return workspace.Commit(h.repo(tenant, repo).refs[ref])
}

// FailCreate makes the next n CreateBranch calls fail with err. With applied the
// branch is created first and only the response is lost (an unknown outcome);
// n < 0 repeats forever.
func (h *FakeHosts) FailCreate(err error, applied bool, n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.createFault = fault{err: err, applied: applied, times: n}
}

// FailPin is FailCreate for SetRetainedRef.
func (h *FakeHosts) FailPin(err error, applied bool, n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pinFault = fault{err: err, applied: applied, times: n}
}

func (f *fault) take() (fault, bool) {
	if f.err == nil || f.times == 0 {
		return fault{}, false
	}
	cur := *f
	if f.times > 0 {
		f.times--
	}
	return cur, true
}

func contractErr(code domain.ErrorCode, msg string) error {
	return &gitv1.ContractError{Code: code, Message: msg}
}

func (h *FakeHosts) ReadRefValues(_ context.Context, tenant domain.TenantID, req gitv1.ReadRefValuesRequest) (gitv1.ReadRefValuesResponse, error) {
	h.mu.Lock()
	hook := h.BeforeRead
	h.BeforeRead = nil
	h.mu.Unlock()
	if hook != nil && len(req.Refs) > 0 {
		hook(req.RepositoryID, req.Refs[0])
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ReadErr != nil {
		return gitv1.ReadRefValuesResponse{}, h.ReadErr
	}
	r := h.repo(tenant, req.RepositoryID)
	resp := gitv1.ReadRefValuesResponse{Quiescent: true, JournalHead: r.head, WorkerInstance: "worker_fake"}
	for _, ref := range req.Refs {
		v := gitv1.RefValue{Ref: ref}
		if c, ok := r.refs[ref]; ok {
			c := c
			v.Commit = &c
		}
		resp.Values = append(resp.Values, v)
	}
	return resp, nil
}

func (h *FakeHosts) ReadJournal(_ context.Context, tenant domain.TenantID, req gitv1.ReadJournalRequest) (gitv1.ReadJournalResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ReadErr != nil {
		return gitv1.ReadJournalResponse{}, h.ReadErr
	}
	r := h.repo(tenant, req.RepositoryID)
	resp := gitv1.ReadJournalResponse{HeadSequence: r.head, WorkerInstance: "worker_fake"}
	for _, e := range r.journal {
		if e.Sequence <= req.SinceSequence || req.Ref == nil || e.Ref != *req.Ref {
			continue
		}
		if int64(len(resp.Entries)) >= req.Limit {
			resp.HasMore = true
			break
		}
		resp.Entries = append(resp.Entries, e)
	}
	return resp, nil
}

func (h *FakeHosts) CreateBranch(_ context.Context, tenant domain.TenantID, req gitv1.CreateBranchRequest) (gitv1.CreateBranchResponse, error) {
	if err := req.Validate(); err != nil {
		return gitv1.CreateBranchResponse{}, contractErr(domain.CodeInvalidRequest, err.Error())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.CreateOps = append(h.CreateOps, req.OperationID)
	f, faulty := h.createFault.take()
	if faulty && !f.applied {
		return gitv1.CreateBranchResponse{}, f.err
	}
	r := h.repo(tenant, req.RepositoryID)
	resp := gitv1.CreateBranchResponse{Ref: req.Ref, Commit: req.Commit}
	switch cur, ok := r.refs[req.Ref]; {
	case ok && cur == req.Commit:
	case ok:
		return gitv1.CreateBranchResponse{}, contractErr(gitv1.CodeBranchExists, "the branch exists at another commit")
	default:
		r.refs[req.Ref] = req.Commit
		resp.Created = true
		r.head++
		op := req.OperationID
		r.journal = append(r.journal, gitv1.JournalEntry{OperationID: &op, Ref: req.Ref, Old: zeroOID(len(req.Commit)), New: req.Commit,
			State: gitv1.JournalCommitted, Sequence: r.head})
	}
	if faulty {
		return gitv1.CreateBranchResponse{}, f.err
	}
	return resp, nil
}

func (h *FakeHosts) SetRetainedRef(_ context.Context, tenant domain.TenantID, req gitv1.SetRetainedRefRequest) (gitv1.SetRetainedRefResponse, error) {
	if err := req.Validate(); err != nil {
		return gitv1.SetRetainedRefResponse{}, contractErr(domain.CodeInvalidRequest, err.Error())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.PinOps = append(h.PinOps, req.OperationID)
	f, faulty := h.pinFault.take()
	if faulty && !f.applied {
		return gitv1.SetRetainedRefResponse{}, f.err
	}
	r := h.repo(tenant, req.RepositoryID)
	resp := gitv1.SetRetainedRefResponse{Ref: req.Ref, Commit: req.Commit}
	switch cur, ok := r.refs[req.Ref]; {
	case ok && cur == req.Commit:
	case ok:
		return gitv1.SetRetainedRefResponse{}, contractErr(gitv1.CodeRetainedRefExists, "the retained ref exists at another commit")
	default:
		r.refs[req.Ref] = req.Commit
		resp.Created = true
	}
	if faulty {
		return gitv1.SetRetainedRefResponse{}, f.err
	}
	return resp, nil
}

func zeroOID(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '0'
	}
	return string(b)
}

// ---- repositories ----

type repoKey struct {
	scope domain.Scope
	id    workspace.RepositoryID
}

// FakeInternalRepos implements workspace.InternalRepositories.
type FakeInternalRepos struct {
	mu sync.Mutex
	m  map[repoKey]workspace.InternalRepository
}

var _ workspace.InternalRepositories = (*FakeInternalRepos)(nil)

func NewFakeInternalRepos() *FakeInternalRepos {
	return &FakeInternalRepos{m: map[repoKey]workspace.InternalRepository{}}
}

// Set registers a repository of the scope.
func (r *FakeInternalRepos) Set(scope domain.Scope, repo workspace.InternalRepository) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[repoKey{scope, repo.ID}] = repo
}

func (r *FakeInternalRepos) InternalRepository(_ context.Context, scope domain.Scope, id workspace.RepositoryID) (workspace.InternalRepository, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	repo, ok := r.m[repoKey{scope, id}]
	if !ok {
		return workspace.InternalRepository{}, workspace.ErrNotFound
	}
	return repo, nil
}

// ---- push records and the writer gate ----

// FakePushes implements workspace.PushRecords and workspace.RefPushes over
// scripted records shaped like gitpush's: the by-ref questions are answered from
// the records' entries exactly as the push store answers them (in flight:
// authorized, committing or unknown; applied: newest first).
type FakePushes struct {
	mu    sync.Mutex
	recs  map[string]gitv1.PushRecord
	order []string // keys in the order records were first stored
	scope map[string]domain.Scope
	// Err, when set, fails every Reload (push store outage).
	Err error
	// RefErr, when set, fails every by-ref question.
	RefErr error
	// AfterUnsettled, when set, runs after each UnsettledOnRef answer has been
	// computed and before it is returned: a test uses it to interleave another
	// actor between a question and the decision made on its answer. It clears
	// itself when it must run once.
	AfterUnsettled func(repositoryID, ref string)
}

var (
	_ workspace.PushRecords = (*FakePushes)(nil)
	_ workspace.RefPushes   = (*FakePushes)(nil)
)

func NewFakePushes() *FakePushes {
	return &FakePushes{recs: map[string]gitv1.PushRecord{}, scope: map[string]domain.Scope{}}
}

func pushKey(scope domain.Scope, repo, push string) string {
	return string(scope.TenantID) + "|" + string(scope.ProjectID) + "|" + repo + "|" + push
}

// Put stores a record under its scope.
func (p *FakePushes) Put(scope domain.Scope, rec gitv1.PushRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := pushKey(scope, rec.RepositoryID, rec.PushID)
	if _, ok := p.recs[k]; !ok {
		p.order = append(p.order, k)
	}
	p.recs[k] = rec
	p.scope[k] = scope
}

// SetEntryState moves the entry of the record for ref to state, as gitpush's
// settlement or an operator's resolution does, and re-derives its status.
func (p *FakePushes) SetEntryState(scope domain.Scope, repositoryID, pushID, ref string, state gitv1.EntryState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := pushKey(scope, repositoryID, pushID)
	rec := p.recs[k]
	entries := append([]gitv1.PushEntry(nil), rec.Entries...)
	for i := range entries {
		if entries[i].Ref != ref {
			continue
		}
		entries[i].State = state
		entries[i].Reason = nil
		if state == gitv1.EntryRefused || state == gitv1.EntryUnknown {
			reason := "resolved_not_applied"
			entries[i].Reason = &reason
		}
	}
	rec.Entries = entries
	rec.Status = gitv1.DeriveStatus(entries)
	p.recs[k] = rec
}

// UnsettledOnRef implements workspace.RefPushes.
func (p *FakePushes) UnsettledOnRef(_ context.Context, scope domain.Scope, repositoryID, ref string) (bool, error) {
	p.mu.Lock()
	if p.RefErr != nil {
		p.mu.Unlock()
		return false, p.RefErr
	}
	pending := false
	for _, k := range p.order {
		rec := p.recs[k]
		if p.scope[k] != scope || rec.RepositoryID != repositoryID {
			continue
		}
		for _, e := range rec.Entries {
			if e.Ref == ref && (e.State == gitv1.EntryAuthorized || e.State == gitv1.EntryCommitting || e.State == gitv1.EntryUnknown) {
				pending = true
			}
		}
	}
	hook := p.AfterUnsettled
	p.mu.Unlock()
	if hook != nil {
		hook(repositoryID, ref)
	}
	return pending, nil
}

// unknownOnRef reports an unknown entry for the ref (the writer gate's question).
func (p *FakePushes) unknownOnRef(scope domain.Scope, repositoryID, ref string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, k := range p.order {
		rec := p.recs[k]
		if p.scope[k] != scope || rec.RepositoryID != repositoryID {
			continue
		}
		for _, e := range rec.Entries {
			if e.Ref == ref && e.State == gitv1.EntryUnknown {
				return true
			}
		}
	}
	return false
}

// AppliedOnRef implements workspace.RefPushes: the records with an applied entry
// for the ref, newest first.
func (p *FakePushes) AppliedOnRef(_ context.Context, scope domain.Scope, repositoryID, ref string, limit int) ([]gitv1.PushRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.RefErr != nil {
		return nil, p.RefErr
	}
	var out []gitv1.PushRecord
	for i := len(p.order) - 1; i >= 0 && len(out) < limit; i-- {
		k := p.order[i]
		rec := p.recs[k]
		if p.scope[k] != scope || rec.RepositoryID != repositoryID {
			continue
		}
		for _, e := range rec.Entries {
			if e.Ref == ref && e.State == gitv1.EntryApplied {
				out = append(out, rec)
				break
			}
		}
	}
	return out, nil
}

func (p *FakePushes) Reload(_ context.Context, scope domain.Scope, repositoryID, pushID string) (gitv1.PushRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Err != nil {
		return gitv1.PushRecord{}, p.Err
	}
	rec, ok := p.recs[pushKey(scope, repositoryID, pushID)]
	if !ok {
		return gitv1.PushRecord{}, contractErr(domain.CodeNotFound, "not found")
	}
	return rec, nil
}

// FakeGate implements workspace.WriterGate.
type FakeGate struct {
	mu      sync.Mutex
	blocked map[string]bool
	// Err, when set, fails every question (gitpush outage).
	Err error
	// Asked lists "repo|ref" of every question.
	Asked []string
	// AfterAsk, when set, runs after each question has been answered and before
	// the answer is returned (an interleaving point for tests; it clears itself
	// when it must run once).
	AfterAsk func(repositoryID, ref string)
	pushes   *FakePushes
}

var _ workspace.WriterGate = (*FakeGate)(nil)

func NewFakeGate() *FakeGate { return &FakeGate{blocked: map[string]bool{}} }

// Block makes an unknown entry exist on the branch of the repository.
func (g *FakeGate) Block(scope domain.Scope, repo, ref string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.blocked[pushKey(scope, repo, ref)] = true
}

// Resolve clears it.
func (g *FakeGate) Resolve(scope domain.Scope, repo, ref string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.blocked, pushKey(scope, repo, ref))
}

func (g *FakeGate) UnknownBlocksWriter(_ context.Context, scope domain.Scope, repositoryID, branchRef string) (bool, error) {
	g.mu.Lock()
	g.Asked = append(g.Asked, repositoryID+"|"+branchRef)
	if g.Err != nil {
		g.mu.Unlock()
		return false, g.Err
	}
	blocked := g.blocked[pushKey(scope, repositoryID, branchRef)]
	hook := g.AfterAsk
	g.mu.Unlock()
	if !blocked && g.pushes != nil {
		blocked = g.pushes.unknownOnRef(scope, repositoryID, branchRef) // as gitpush derives it from its records
	}
	if hook != nil {
		hook(repositoryID, branchRef)
	}
	return blocked, nil
}

// ---- the push inspector ----

type commitNode struct {
	parents []workspace.Commit
	content string
}

// FakeInspector implements workspace.PushInspector over an in-memory commit
// graph: ancestry and the secret scan are computed from the graph exactly as the
// Git adapter computes them from objects, so the scan covers every commit
// reachable from the pushed commit and not from the accepted one.
type FakeInspector struct {
	mu    sync.Mutex
	nodes map[workspace.Commit]commitNode
	// Err, when set, fails every inspection (an unreadable host).
	Err error
	// Requests records every inspection request.
	Requests []workspace.InspectRequest
}

var _ workspace.PushInspector = (*FakeInspector)(nil)

func NewFakeInspector() *FakeInspector {
	return &FakeInspector{nodes: map[workspace.Commit]commitNode{}}
}

// Add registers a commit with its parents and the text of its tree and message,
// and returns its object name (derived from both, so equal input is equal).
func (i *FakeInspector) Add(content string, parents ...workspace.Commit) workspace.Commit {
	i.mu.Lock()
	defer i.mu.Unlock()
	h := sha1.New() //nolint:gosec
	_, _ = fmt.Fprintf(h, "%s|%v|%d", content, parents, len(i.nodes))
	c := workspace.Commit(hex.EncodeToString(h.Sum(nil)))
	i.nodes[c] = commitNode{parents: parents, content: content}
	return c
}

// Seed registers a root commit for a default branch tip.
func (i *FakeInspector) Seed(content string) workspace.Commit { return i.Add(content) }

func (i *FakeInspector) ancestors(c workspace.Commit) map[workspace.Commit]bool {
	out := map[workspace.Commit]bool{}
	stack := []workspace.Commit{c}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[cur] {
			continue
		}
		out[cur] = true
		stack = append(stack, i.nodes[cur].parents...)
	}
	return out
}

func (i *FakeInspector) Inspect(_ context.Context, req workspace.InspectRequest) (workspace.Relation, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.Requests = append(i.Requests, req)
	if i.Err != nil {
		return "", i.Err
	}
	if _, ok := i.nodes[req.Commit]; !ok {
		return "", fmt.Errorf("%w: the pushed commit is not readable on the host", workspace.ErrUnavailable)
	}
	if _, ok := i.nodes[req.Since]; !ok {
		return workspace.RelationDiverged, nil
	}
	fromCommit, fromSince := i.ancestors(req.Commit), i.ancestors(req.Since)
	switch {
	case fromSince[req.Commit]:
		return workspace.RelationContained, nil
	case !fromCommit[req.Since]:
		return workspace.RelationDiverged, nil
	}
	var introduced []workspace.Commit
	for c := range fromCommit {
		if !fromSince[c] {
			introduced = append(introduced, c)
		}
	}
	sort.Slice(introduced, func(a, b int) bool { return introduced[a] < introduced[b] })
	for _, c := range introduced {
		for _, n := range req.Needles {
			if len(n) >= 8 && bytes.Contains([]byte(i.nodes[c].content), n) {
				return "", workspace.ErrSecretInSource
			}
		}
	}
	return workspace.RelationDescends, nil
}

// ---- run disclosures ----

// Disclosures implements workspace.RunDisclosures: the values a run disclosed to
// its other steps and services.
type Disclosures struct {
	mu sync.Mutex
	m  map[domain.AttemptID][][]byte
	// Err, when set, fails every lookup.
	Err error
}

var _ workspace.RunDisclosures = (*Disclosures)(nil)

func NewDisclosures() *Disclosures { return &Disclosures{m: map[domain.AttemptID][][]byte{}} }

// Set records the prior disclosures an attempt must also scan for.
func (d *Disclosures) Set(id domain.AttemptID, values ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.m[id] = nil
	for _, v := range values {
		d.m[id] = append(d.m[id], []byte(v))
	}
}

func (d *Disclosures) Prior(_ context.Context, _ domain.Scope, id domain.AttemptID) ([][]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.m[id], d.Err
}

// ---- event recording ----

// EventRecorder wraps the memory event bus's transaction-local append and keeps
// what was appended, so a test can read an event's type and payload. A
// transaction that rolls back may have appended; Committed filters by the bus's
// committed event identifiers.
type EventRecorder struct {
	inner *eventstest.Mem
	mu    sync.Mutex
	seen  []eventsv1.Envelope
}

var _ EventAppender = (*EventRecorder)(nil)

func (r *EventRecorder) Append(tx memdb.Tx, env eventsv1.Envelope) (eventstest.AppendResult, error) {
	res, err := r.inner.Append(tx, env)
	if err == nil {
		r.mu.Lock()
		r.seen = append(r.seen, env)
		r.mu.Unlock()
	}
	return res, err
}

// Committed returns the committed events of a type for the tenant, in order.
func (r *EventRecorder) Committed(t testing.TB, tenant domain.TenantID, typ string) []eventsv1.Envelope {
	t.Helper()
	ids, err := r.inner.EventIDs(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	committed := map[string]bool{}
	for _, id := range ids {
		committed[id] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []eventsv1.Envelope
	for _, e := range r.seen {
		if e.Type == typ && e.TenantID == tenant && committed[e.ID] {
			out = append(out, e)
		}
	}
	return out
}

// ---- the environment ----

// Internal fixture identifiers.
const (
	RepoApp  workspace.RepositoryID = "repo_app"
	RepoDocs workspace.RepositoryID = "repo_docs"
)

// ExecutionService is the service principal that accepts checkpoints, as the
// execution module will.
var ExecutionService = workspace.Actor{Type: workspace.ActorService, ID: "svc_execution"}

// InternalEnv is the whole workspace module with the internally hosted model
// wired over the memory store and the fakes above, beside the external model of
// MemServiceEnv, which it embeds unchanged.
type InternalEnv struct {
	*MemServiceEnv

	Hosts      *FakeHosts
	IRepos     *FakeInternalRepos
	Pushes     *FakePushes
	Gate       *FakeGate
	Inspector  *FakeInspector
	Disclosed  *Disclosures
	Recorder   *EventRecorder
	scopeSeeds map[domain.Scope]map[workspace.RepositoryID]workspace.Commit
	pushSeq    int
}

// NewInternalEnv wires the service with the internal ports and the prior
// disclosure composition of the secret scan.
func NewInternalEnv(t testing.TB) *InternalEnv {
	t.Helper()
	m := NewMemServiceEnv(t)
	e := &InternalEnv{MemServiceEnv: m, Hosts: NewFakeHosts(), IRepos: NewFakeInternalRepos(), Pushes: NewFakePushes(), Gate: NewFakeGate(),
		Inspector: NewFakeInspector(), Disclosed: NewDisclosures(), scopeSeeds: map[domain.Scope]map[workspace.RepositoryID]workspace.Commit{}}
	secrets, err := workspace.WithPriorDisclosures(m.Secrets, e.Disclosed)
	if err != nil {
		t.Fatal(err)
	}
	e.Gate.pushes = e.Pushes
	e.Recorder = &EventRecorder{inner: m.Events}
	if m.Store, err = NewMemStore(m.DB, e.Recorder, MemOptions{Clock: m.Clock.Now}); err != nil {
		t.Fatal(err)
	}
	e.Authz.Grant(ExecutionService.ID, ScopeA)
	e.Authz.Grant(ExecutionService.ID, ScopeB)
	e.Svc, err = workspace.NewService(workspace.Dependencies{
		Store: m.Store, Tenants: m.Tenants, Authorizer: m.Authz, WorkItems: m.Items, Repositories: m.Repos, Attempts: m.Attempts,
		Bundles: m.Bundles, Secrets: secrets, Refs: m.Git.Reader(), Source: m.Git.Preparer(), Publisher: m.Git.Publisher(), Clock: m.Clock.Now,
		Hosts: e.Hosts, InternalRepos: e.IRepos, Pushes: e.Pushes, Gate: e.Gate, RefPushes: e.Pushes, Inspector: e.Inspector,
	}, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// Seed registers an internal, ready repository of the scope whose default
// branch main is at a new root commit, and returns that commit.
func (e *InternalEnv) Seed(scope domain.Scope, repo workspace.RepositoryID) workspace.Commit {
	e.T.Helper()
	tip := e.Inspector.Seed("seed of " + string(repo) + " in " + string(scope.ProjectID))
	e.IRepos.Set(scope, workspace.InternalRepository{ID: repo, URL: "https://git.example.test/git/" + string(scope.TenantID) + "/" + string(repo) + ".git",
		CheckoutSecretRef: "kodama-git-read:" + string(repo), DefaultBranch: "main", Hosting: gitv1.HostingInternal, Status: gitv1.StatusReady})
	e.Hosts.SetRef(scope.TenantID, string(repo), "refs/heads/main", tip)
	if e.scopeSeeds[scope] == nil {
		e.scopeSeeds[scope] = map[workspace.RepositoryID]workspace.Commit{}
	}
	e.scopeSeeds[scope][repo] = tip
	return tip
}

// Provision provisions the internal workspace of a fresh story.
func (e *InternalEnv) Provision(scope domain.Scope, item domain.WorkItemID, repos ...workspace.RepositoryID) (*workspace.Workspace, []*workspace.RepositoryAssociation) {
	e.T.Helper()
	e.Items.Add(scope, item, "story")
	w, assocs, _, err := e.Svc.ProvisionInternal(context.Background(), Engineer, scope, item, repos)
	if err != nil {
		e.T.Fatalf("provision internal: %v", err)
	}
	return w, assocs
}

// Writer registers a live write attempt and reserves the slot for it.
func (e *InternalEnv) InternalWriter(w *workspace.Workspace, attempt domain.AttemptID) *workspace.Workspace {
	e.T.Helper()
	e.Attempts.Register(w.Scope, attempt, w.ID, workspace.AccessWrite)
	out, err := e.Svc.ReserveInternalWriter(context.Background(), ExecutionService, workspace.ReserveWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: attempt})
	if err != nil {
		e.T.Fatalf("reserve internal writer: %v", err)
	}
	return out
}

// PushEntry describes one entry of a scripted push record.
type PushEntry struct {
	Ref      string
	Old, New workspace.Commit
	State    gitv1.EntryState
	Kind     gitv1.UpdateKind
}

// Push stores a push record made by the attempt of the run in the repository
// with the given entries (its status is derived from them, as the push store
// derives it), and returns its push ID.
func (e *InternalEnv) Push(scope domain.Scope, repo workspace.RepositoryID, attempt domain.AttemptID, run domain.RunID, entries ...PushEntry) string {
	e.T.Helper()
	e.pushSeq++
	id := fmt.Sprintf("push_t147_%d", e.pushSeq)
	attemptID, runID := string(attempt), string(run)
	rec := gitv1.PushRecord{PushID: id, RepositoryID: string(repo), ReceiveID: fmt.Sprintf("rcv_t147_%d", e.pushSeq), GrantID: "grt_t147",
		OriginRunID: &runID, Actor: gitv1.Actor{Type: gitv1.ActorWorkload, ID: attemptID, RunID: &runID, AttemptID: &attemptID},
		CreatedAt: gitv1.Timestamp("2026-10-19T10:00:00Z")}
	for _, en := range entries {
		pe := gitv1.PushEntry{Ref: en.Ref, Old: string(en.Old), New: string(en.New), Kind: en.Kind, State: en.State}
		if en.State == gitv1.EntryRefused || en.State == gitv1.EntryUnknown {
			reason := "no_commit_intent"
			if en.State == gitv1.EntryUnknown {
				reason = "journal_unreadable"
			}
			pe.Reason = &reason
		}
		rec.Entries = append(rec.Entries, pe)
	}
	rec.Status = gitv1.DeriveStatus(rec.Entries)
	e.Pushes.Put(scope, rec)
	return id
}

// AppliedPush is a push record whose one fast-forward of ref is applied.
func (e *InternalEnv) AppliedPush(scope domain.Scope, repo workspace.RepositoryID, attempt domain.AttemptID, run domain.RunID, ref string, old, new workspace.Commit) string {
	e.T.Helper()
	return e.Push(scope, repo, attempt, run, PushEntry{Ref: ref, Old: old, New: new, State: gitv1.EntryApplied, Kind: gitv1.KindFastForward})
}

// HumanPush stores an applied push record made by an engineer (a user grant)
// that moved the ref from old to new, moves the host's ref and returns the push
// ID.
func (e *InternalEnv) HumanPush(scope domain.Scope, repo workspace.RepositoryID, ref string, old, new workspace.Commit) string {
	e.T.Helper()
	e.pushSeq++
	id := fmt.Sprintf("push_t147_%d", e.pushSeq)
	rec := gitv1.PushRecord{PushID: id, RepositoryID: string(repo), ReceiveID: fmt.Sprintf("rcv_t147_%d", e.pushSeq), GrantID: "grt_t147_human",
		Actor: gitv1.Actor{Type: gitv1.ActorUser, ID: string(Engineer.ID)}, CreatedAt: gitv1.Timestamp("2026-10-19T10:00:00Z"),
		Entries: []gitv1.PushEntry{{Ref: ref, Old: string(old), New: string(new), Kind: gitv1.KindFastForward, State: gitv1.EntryApplied}}}
	rec.Status = gitv1.DeriveStatus(rec.Entries)
	e.Hosts.SetRef(scope.TenantID, string(repo), ref, new)
	e.Pushes.Put(scope, rec)
	return id
}
