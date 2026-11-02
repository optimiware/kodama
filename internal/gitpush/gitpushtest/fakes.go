package gitpushtest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitauth"
	"github.com/optimiware/kodama/internal/gitpush"
)

// Clock is a settable clock.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock starts at the current instant, truncated to the second.
//
// The instant is the wall clock's: the event bus refuses an event from the
// future beyond its permitted skew, and settlements stamp their events with the
// service clock.
func NewClock() *Clock { return &Clock{t: time.Now().UTC().Truncate(time.Second)} }

// Now returns the current fixture time.
func (c *Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func repoKey(tenant domain.TenantID, id string) string { return string(tenant) + "|" + id }

// Repository is one repository of the Repositories fake.
type Repository struct {
	Facts gitpush.RepositoryFacts
	Empty bool
	// observed is the time of the last emptiness report.
	observed time.Time
}

// Repositories is the repository module stand-in. It serves gitpush (as
// gitpush.Repositories) and gitauth (through GitauthView).
type Repositories struct {
	mu    sync.Mutex
	repos map[string]*Repository
	// Fail, when set, fails every call (an outage).
	Fail error
	// ReportFail fails ReportEmpty only, until cleared.
	ReportFail error
	// Reports counts the successful ReportEmpty calls.
	Reports int
	// AfterRead runs after Repository has answered, before the caller continues:
	// tests change the repository between the read and the authorization's
	// transaction deterministically.
	AfterRead func()
}

var _ gitpush.Repositories = (*Repositories)(nil)

// Put stores a repository.
func (r *Repositories) Put(repo Repository) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.repos == nil {
		r.repos = map[string]*Repository{}
	}
	c := repo
	r.repos[repoKey(repo.Facts.Scope.TenantID, repo.Facts.ID)] = &c
}

// Update changes a stored repository.
func (r *Repositories) Update(tenant domain.TenantID, id string, fn func(*Repository)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(r.repos[repoKey(tenant, id)])
}

// IsEmpty reports the stored emptiness.
func (r *Repositories) IsEmpty(tenant domain.TenantID, id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.repos[repoKey(tenant, id)].Empty
}

// Repository implements gitpush.Repositories.
func (r *Repositories) Repository(ctx context.Context, tenant domain.TenantID, id string) (gitpush.RepositoryFacts, error) {
	facts, err := r.read(ctx, tenant, id)
	r.mu.Lock()
	hook := r.AfterRead
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	return facts, err
}

// GuardView is the same facts without the hook: what the memory store's
// Tx.RepositoryState reads inside the transaction.
func (r *Repositories) GuardView() gitpush.Repositories { return guardView{r} }

type guardView struct{ r *Repositories }

func (g guardView) Repository(ctx context.Context, tenant domain.TenantID, id string) (gitpush.RepositoryFacts, error) {
	return g.r.read(ctx, tenant, id)
}

func (g guardView) ReportEmpty(ctx context.Context, tenant domain.TenantID, id string, empty bool, at time.Time) error {
	return g.r.ReportEmpty(ctx, tenant, id, empty, at)
}

func (r *Repositories) read(_ context.Context, tenant domain.TenantID, id string) (gitpush.RepositoryFacts, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Fail != nil {
		return gitpush.RepositoryFacts{}, r.Fail
	}
	repo, ok := r.repos[repoKey(tenant, id)]
	if !ok {
		return gitpush.RepositoryFacts{}, gitpush.ErrNotFound
	}
	return repo.Facts, nil
}

// ReportEmpty implements gitpush.Repositories with the repository module's rule
// that an older observation never wins.
func (r *Repositories) ReportEmpty(_ context.Context, tenant domain.TenantID, id string, empty bool, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Fail != nil {
		return r.Fail
	}
	if r.ReportFail != nil {
		return r.ReportFail
	}
	repo, ok := r.repos[repoKey(tenant, id)]
	if !ok {
		return gitpush.ErrNotFound
	}
	r.Reports++
	if at.Before(repo.observed) {
		return nil
	}
	repo.Empty, repo.observed = empty, at
	return nil
}

// GitauthView adapts the fake to gitauth.Repositories.
func (r *Repositories) GitauthView() gitauth.Repositories { return gitauthRepos{r} }

type gitauthRepos struct{ r *Repositories }

func (g gitauthRepos) Repository(_ context.Context, scope domain.Scope, id string) (gitv1.Repository, error) {
	g.r.mu.Lock()
	defer g.r.mu.Unlock()
	repo, ok := g.r.repos[repoKey(scope.TenantID, id)]
	if !ok || repo.Facts.Scope != scope {
		return gitv1.Repository{}, gitauth.ErrNotFound
	}
	f := repo.Facts
	return gitv1.Repository{
		ID: f.ID, Name: "backend", DefaultBranch: f.DefaultBranch, Hosting: f.Hosting, Status: f.Status, Empty: repo.Empty,
		Policy: gitv1.RepositoryPolicy{DirectPushToDefault: f.DirectPushToDefault}, CloneURL: "https://git.example.invalid/git/acme/inventory/backend.git",
		Version: 1, CreatedAt: "2026-10-30T09:00:00Z",
	}, nil
}

// AttemptsView adapts a gitauth.Attempts stand-in (the same lease table gitauth
// reads) to gitpush.Attempts.
type AttemptsView struct {
	Leases gitauth.Attempts
	// After runs after the lease was read, before Live returns.
	After func()
}

// Live implements gitpush.Attempts.
func (a AttemptsView) Live(ctx context.Context, scope domain.Scope, attemptID string) (bool, error) {
	l, err := a.Leases.Lease(ctx, scope, domain.AttemptID(attemptID))
	if a.After != nil {
		defer a.After()
	}
	switch {
	case errors.Is(err, gitauth.ErrNotFound):
		return false, gitpush.ErrNotFound
	case err != nil:
		return false, err
	}
	return l.Live, nil
}

// Reservations is the writer-reservation stand-in.
type Reservations struct {
	mu   sync.Mutex
	held map[string]bool
	Fail error
	// AfterRead runs after a read decided its answer and before it is returned,
	// with the 1-based number of the read: a test changes the reservation or
	// observes the store in the window between a read and what follows it.
	AfterRead func(n int)
	reads     int
}

var _ gitpush.Reservations = (*Reservations)(nil)

func resKey(scope domain.Scope, workItem, attempt string) string {
	return string(scope.TenantID) + "|" + string(scope.ProjectID) + "|" + workItem + "|" + attempt
}

// Hold makes the attempt the writer of the work item.
func (r *Reservations) Hold(scope domain.Scope, workItem, attempt string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.held == nil {
		r.held = map[string]bool{}
	}
	r.held[resKey(scope, workItem, attempt)] = true
}

// Release removes the reservation.
func (r *Reservations) Release(scope domain.Scope, workItem, attempt string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.held, resKey(scope, workItem, attempt))
}

// HoldsWriter implements gitpush.Reservations.
func (r *Reservations) HoldsWriter(_ context.Context, scope domain.Scope, workItem, attempt string) (bool, error) {
	r.mu.Lock()
	r.reads++
	n, hook := r.reads, r.AfterRead
	fail, held := r.Fail, r.held[resKey(scope, workItem, attempt)]
	r.mu.Unlock()
	if fail != nil {
		return false, fail
	}
	if hook != nil {
		hook(n)
	}
	return held, nil
}

// Reads is how many times the reservation was read.
func (r *Reservations) Reads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

// ReadAuth is the read-authority stand-in: the users allowed to read a scope.
type ReadAuth struct {
	mu      sync.Mutex
	allowed map[string]bool
}

var _ gitpush.ReadAuthorizer = (*ReadAuth)(nil)

// Allow lets the principal read the push records of the project.
func (a *ReadAuth) Allow(principal string, scope domain.Scope) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.allowed == nil {
		a.allowed = map[string]bool{}
	}
	a.allowed[principal+"|"+string(scope.TenantID)+"|"+string(scope.ProjectID)] = true
}

// AuthorizeRead implements gitpush.ReadAuthorizer.
func (a *ReadAuth) AuthorizeRead(_ context.Context, principal string, scope domain.Scope, _ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.allowed[principal+"|"+string(scope.TenantID)+"|"+string(scope.ProjectID)] {
		return gitpush.ErrDenied
	}
	return nil
}

// AdminAuth is the administrative-authority stand-in: the users who may resolve
// unknown entries of a project.
type AdminAuth struct {
	mu      sync.Mutex
	allowed map[string]bool
}

var _ gitpush.ResolveAuthorizer = (*AdminAuth)(nil)

// Allow makes the principal an administrator of the project.
func (a *AdminAuth) Allow(principal string, scope domain.Scope) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.allowed == nil {
		a.allowed = map[string]bool{}
	}
	a.allowed[principal+"|"+string(scope.TenantID)+"|"+string(scope.ProjectID)] = true
}

// AuthorizeResolve implements gitpush.ResolveAuthorizer.
func (a *AdminAuth) AuthorizeResolve(_ context.Context, principal string, scope domain.Scope, _ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.allowed[principal+"|"+string(scope.TenantID)+"|"+string(scope.ProjectID)] {
		return gitpush.ErrDenied
	}
	return nil
}

// Worker is a scripted tenant Git worker: a per-repository ref journal, ref
// values, the set of running receives and switchable failures. Tests append
// the entries the hooks would have written; the worker never invents evidence.
type Worker struct {
	mu      sync.Mutex
	repos   map[string]*workerRepo
	Calls   int
	Fail    error // fails every call
	Journal error // fails ReadJournal only
	// Hook runs before a call is answered, with the operation name ("liveness",
	// "journal" or "values") and how many times that operation was called
	// (1 for the first): tests change the worker between the reads of one
	// settlement deterministically.
	Hook  func(op string, n int)
	calls map[string]int
	// PageLimit, when positive, caps the page size of ReadJournal below the
	// request's limit, so a read spans several pages.
	PageLimit int64
}

func (w *Worker) hook(op string) {
	w.mu.Lock()
	if w.calls == nil {
		w.calls = map[string]int{}
	}
	w.calls[op]++
	n, hook := w.calls[op], w.Hook
	w.mu.Unlock()
	if hook != nil {
		hook(op, n)
	}
}

var _ gitpush.Worker = (*Worker)(nil)

type workerRepo struct {
	journal   []gitv1.JournalEntry
	refs      map[string]string
	running   map[string]bool
	quiescent bool
	instance  string
}

func (w *Worker) repo(tenant domain.TenantID, id string) *workerRepo {
	if w.repos == nil {
		w.repos = map[string]*workerRepo{}
	}
	k := repoKey(tenant, id)
	r, ok := w.repos[k]
	if !ok {
		r = &workerRepo{refs: map[string]string{}, running: map[string]bool{}, quiescent: true, instance: "worker-1"}
		w.repos[k] = r
	}
	return r
}

// Prepared appends the prepared entry a hook would write: observed is the ref's
// value under the lock.
func (w *Worker) Prepared(tenant domain.TenantID, repo, receive, ref, old, new, observed string) int64 {
	return w.append(tenant, repo, gitv1.JournalEntry{ReceiveID: &receive, Ref: ref, Old: old, New: new, Observed: &observed, State: gitv1.JournalPrepared})
}

// Committed appends the committed outcome of a receive's transaction.
func (w *Worker) Committed(tenant domain.TenantID, repo, receive, ref, old, new string) int64 {
	return w.append(tenant, repo, gitv1.JournalEntry{ReceiveID: &receive, Ref: ref, Old: old, New: new, State: gitv1.JournalCommitted})
}

// Aborted appends the aborted outcome of a receive's transaction.
func (w *Worker) Aborted(tenant domain.TenantID, repo, receive, ref, old, new string) int64 {
	return w.append(tenant, repo, gitv1.JournalEntry{ReceiveID: &receive, Ref: ref, Old: old, New: new, State: gitv1.JournalAborted})
}

func (w *Worker) append(tenant domain.TenantID, repo string, e gitv1.JournalEntry) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := w.repo(tenant, repo)
	e.Sequence = int64(len(r.journal)) + 1
	r.journal = append(r.journal, e)
	return e.Sequence
}

// SetRef sets the current value of a ref ("" removes it).
func (w *Worker) SetRef(tenant domain.TenantID, repo, ref, commit string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := w.repo(tenant, repo)
	if commit == "" {
		delete(r.refs, ref)
		return
	}
	r.refs[ref] = commit
}

// SetRunning marks a receive as running (or not) on the worker.
func (w *Worker) SetRunning(tenant domain.TenantID, repo, receive string, running bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.repo(tenant, repo).running[receive] = running
}

// SetQuiescent sets whether ref reads are quiescent.
func (w *Worker) SetQuiescent(tenant domain.TenantID, repo string, q bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.repo(tenant, repo).quiescent = q
}

// Restore simulates a volume restored from an older snapshot: the journal keeps
// only its first n entries.
func (w *Worker) Restore(tenant domain.TenantID, repo string, n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := w.repo(tenant, repo)
	if n < len(r.journal) {
		r.journal = slices.Clone(r.journal[:n])
	}
}

// Wipe simulates a replaced worker with an empty journal.
func (w *Worker) Wipe(tenant domain.TenantID, repo string) { w.Restore(tenant, repo, 0) }

// Head is the journal's highest sequence.
func (w *Worker) Head(tenant domain.TenantID, repo string) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return int64(len(w.repo(tenant, repo).journal))
}

func (w *Worker) enter() error {
	w.Calls++
	return w.Fail
}

// ReadRefValues implements gitpush.Worker.
func (w *Worker) ReadRefValues(_ context.Context, tenant domain.TenantID, req gitv1.ReadRefValuesRequest) (gitv1.ReadRefValuesResponse, error) {
	w.hook("values")
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.enter(); err != nil {
		return gitv1.ReadRefValuesResponse{}, err
	}
	r := w.repo(tenant, req.RepositoryID)
	out := gitv1.ReadRefValuesResponse{Quiescent: r.quiescent, JournalHead: int64(len(r.journal)), WorkerInstance: r.instance}
	for _, ref := range req.Refs {
		v := gitv1.RefValue{Ref: ref}
		if c, ok := r.refs[ref]; ok {
			c := c
			v.Commit = &c
		}
		out.Values = append(out.Values, v)
	}
	return out, nil
}

// ReadJournal implements gitpush.Worker.
func (w *Worker) ReadJournal(_ context.Context, tenant domain.TenantID, req gitv1.ReadJournalRequest) (gitv1.ReadJournalResponse, error) {
	w.hook("journal")
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.enter(); err != nil {
		return gitv1.ReadJournalResponse{}, err
	}
	if w.Journal != nil {
		return gitv1.ReadJournalResponse{}, w.Journal
	}
	r := w.repo(tenant, req.RepositoryID)
	out := gitv1.ReadJournalResponse{Entries: []gitv1.JournalEntry{}, HeadSequence: int64(len(r.journal)), WorkerInstance: r.instance}
	for _, e := range r.journal {
		if e.Sequence <= req.SinceSequence {
			continue
		}
		if req.ReceiveID != nil && (e.ReceiveID == nil || *e.ReceiveID != *req.ReceiveID) {
			continue
		}
		if req.Ref != nil && e.Ref != *req.Ref {
			continue
		}
		limit := req.Limit
		if w.PageLimit > 0 && w.PageLimit < limit {
			limit = w.PageLimit
		}
		if int64(len(out.Entries)) >= limit {
			out.HasMore = true
			break
		}
		out.Entries = append(out.Entries, e)
	}
	return out, nil
}

// ReceiveLiveness implements gitpush.Worker.
func (w *Worker) ReceiveLiveness(_ context.Context, tenant domain.TenantID, repo, receive string) (gitv1.ReceiveLivenessResponse, error) {
	w.hook("liveness")
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.enter(); err != nil {
		return gitv1.ReceiveLivenessResponse{}, err
	}
	r := w.repo(tenant, repo)
	return gitv1.ReceiveLivenessResponse{ReceiveID: receive, Running: r.running[receive], WorkerInstance: r.instance}, nil
}
