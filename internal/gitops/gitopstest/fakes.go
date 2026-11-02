package gitopstest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitops"
)

// Clock is a settable clock.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock starts at a fixed instant.
func NewClock() *Clock { return &Clock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)} }

// Now implements the clock function.
func (c *Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// Catalog is a settable gitops.Catalog.
type Catalog struct {
	mu    sync.Mutex
	repos map[domain.TenantID][]gitops.Repository
	// Fail makes Tenants or Repositories fail.
	Fail error
}

var _ gitops.Catalog = (*Catalog)(nil)

// Add lists a repository (kept ordered by ID).
func (c *Catalog) Add(r gitops.Repository) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.repos == nil {
		c.repos = map[domain.TenantID][]gitops.Repository{}
	}
	c.repos[r.Scope.TenantID] = append(c.repos[r.Scope.TenantID], r)
	sort.Slice(c.repos[r.Scope.TenantID], func(i, j int) bool { return c.repos[r.Scope.TenantID][i].ID < c.repos[r.Scope.TenantID][j].ID })
}

// Remove stops listing a repository (it was deleted since a backup).
func (c *Catalog) Remove(tenant domain.TenantID, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var kept []gitops.Repository
	for _, r := range c.repos[tenant] {
		if r.ID != id {
			kept = append(kept, r)
		}
	}
	c.repos[tenant] = kept
}

// Tenants implements gitops.Catalog.
func (c *Catalog) Tenants(context.Context) ([]domain.TenantID, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Fail != nil {
		return nil, c.Fail
	}
	var out []domain.TenantID
	for t := range c.repos {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// Repositories implements gitops.Catalog.
func (c *Catalog) Repositories(_ context.Context, tenant domain.TenantID, after string, limit int) ([]gitops.Repository, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []gitops.Repository
	for _, r := range c.repos[tenant] {
		if r.ID > after && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

// Holds is a settable gitops.Holds keyed by repository.
type Holds struct {
	mu   sync.Mutex
	held map[string]gitops.Held
	Fail error
}

var _ gitops.Holds = (*Holds)(nil)

// Set replaces what a repository's records hold.
func (h *Holds) Set(repositoryID string, held gitops.Held) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.held == nil {
		h.held = map[string]gitops.Held{}
	}
	h.held[repositoryID] = held
}

// Held implements gitops.Holds.
func (h *Holds) Held(_ context.Context, _ domain.Scope, repositoryID string) (gitops.Held, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.held[repositoryID], h.Fail
}

// Gate is a gitops.ReleaseGate answering a fixed value.
type Gate struct{ Allow bool }

// MayRelease implements gitops.ReleaseGate.
func (g *Gate) MayRelease(context.Context, domain.Scope, domain.RunID) (bool, error) {
	return g.Allow, nil
}

// Store is an in-memory gitops.BackupStore.
type Store struct {
	mu   sync.Mutex
	objs map[string][]byte
	// FailPut makes Put of a key containing the substring fail.
	FailPut string
	// OnPut runs before a Put stores its object (a test pauses an upload there).
	OnPut func(key string)
	// OnList runs before a List answers; OnDelete before a Delete removes its key
	// (a test begins a recovery in those windows).
	OnList   func(prefix string)
	OnDelete func(key string)
}

var _ gitops.BackupStore = (*Store)(nil)

// Put implements gitops.BackupStore.
func (s *Store) Put(_ context.Context, key string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	hook := s.OnPut
	s.mu.Unlock()
	if hook != nil {
		hook(key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.FailPut != "" && strings.Contains(key, s.FailPut) {
		return errors.New("store: put refused")
	}
	if s.objs == nil {
		s.objs = map[string][]byte{}
	}
	s.objs[key] = b
	return nil
}

// Open implements gitops.BackupStore.
func (s *Store) Open(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objs[key]
	if !ok {
		return nil, gitops.ErrBackupNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// List implements gitops.BackupStore.
func (s *Store) List(_ context.Context, prefix string) ([]string, error) {
	s.mu.Lock()
	hook := s.OnList
	s.mu.Unlock()
	if hook != nil {
		hook(prefix)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Delete implements gitops.BackupStore.
func (s *Store) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	hook := s.OnDelete
	s.mu.Unlock()
	if hook != nil {
		hook(key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objs, key)
	return nil
}

// Bytes returns an object's content (nil when absent), for assertions.
func (s *Store) Bytes(key string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objs[key]
}

// Corrupt flips a byte of the object.
func (s *Store) Corrupt(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.objs[key]
	b[len(b)/2] ^= 0xff
}

// Snapshots is a gitops.Snapshots recording its calls.
type Snapshots struct {
	mu       sync.Mutex
	Taken    []string
	Restored []string
	Log      *[]string
	Fail     error
	// OnRestore runs when a volume is restored (a test swaps the worker's state).
	OnRestore func()
}

var _ gitops.Snapshots = (*Snapshots)(nil)

func (s *Snapshots) log(e string) {
	if s.Log != nil {
		*s.Log = append(*s.Log, e)
	}
}

// Snapshot implements gitops.Snapshots.
func (s *Snapshots) Snapshot(_ context.Context, tenant domain.TenantID) (gitops.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Fail != nil {
		return gitops.Snapshot{}, s.Fail
	}
	id := "snap-" + string(tenant) + "-" + string(rune('a'+len(s.Taken)))
	s.Taken = append(s.Taken, id)
	s.log("snapshot")
	return gitops.Snapshot{ID: id, TakenAt: time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC)}, nil
}

// RestoreVolume implements gitops.Snapshots.
func (s *Snapshots) RestoreVolume(_ context.Context, _ domain.TenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Fail != nil {
		return s.Fail
	}
	s.Restored = append(s.Restored, id)
	s.log("restoreVolume")
	if s.OnRestore != nil {
		s.OnRestore()
	}
	return nil
}

// Fence is a gitops.AccessFence recording its transitions.
type Fence struct {
	mu     sync.Mutex
	closed map[domain.TenantID]gitops.FenceReason
	// Log, when set, receives "close"/"open" entries in order.
	Log       *[]string
	FailClose error
	FailOpen  error
	// FailRead makes ClosedFor fail.
	FailRead error
}

var _ gitops.AccessFence = (*Fence)(nil)

// Close implements gitops.AccessFence.
func (f *Fence) Close(_ context.Context, tenant domain.TenantID, reason gitops.FenceReason) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailClose != nil {
		return f.FailClose
	}
	if f.closed == nil {
		f.closed = map[domain.TenantID]gitops.FenceReason{}
	}
	if _, ok := f.closed[tenant]; !ok {
		f.closed[tenant] = reason
	}
	if f.Log != nil {
		*f.Log = append(*f.Log, "close")
	}
	return nil
}

// Open implements gitops.AccessFence.
func (f *Fence) Open(_ context.Context, tenant domain.TenantID, reason gitops.FenceReason) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailOpen != nil {
		return f.FailOpen
	}
	if f.closed[tenant] == reason {
		delete(f.closed, tenant)
		if f.Log != nil {
			*f.Log = append(*f.Log, "open")
		}
	}
	return nil
}

// ClosedFor implements gitops.AccessFence.
func (f *Fence) ClosedFor(_ context.Context, tenant domain.TenantID, reason gitops.FenceReason) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailRead != nil {
		return false, f.FailRead
	}
	return f.closed[tenant] == reason, nil
}

// IsClosed reports whether the tenant's access is closed for any reason.
func (f *Fence) IsClosed(tenant domain.TenantID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.closed[tenant]
	return ok
}

// Pushes is a scripted gitops.Pushes.
type Pushes struct {
	mu          sync.Mutex
	InFlight    map[string]int
	Unknown     map[string]int
	Known       map[string]int64
	Acknowledge []string
	ReconcileFn func(repo string)
	Fail        error
}

var _ gitops.Pushes = (*Pushes)(nil)

// Reconcile implements gitops.Pushes.
func (p *Pushes) Reconcile(_ context.Context, _ domain.TenantID, repo string) error {
	if p.ReconcileFn != nil {
		p.ReconcileFn(repo)
	}
	return p.Fail
}

// Settlement implements gitops.Pushes.
func (p *Pushes) Settlement(_ context.Context, _ domain.TenantID, repo string) (gitops.PushSettlement, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return gitops.PushSettlement{InFlight: p.InFlight[repo], Unknown: p.Unknown[repo], KnownHead: p.Known[repo]}, nil
}

// AcknowledgeRestore implements gitops.Pushes.
func (p *Pushes) AcknowledgeRestore(_ context.Context, _ domain.Scope, repo string, head int64, _, _ string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.InFlight[repo] > 0 {
		return false, errors.New("in flight")
	}
	p.Acknowledge = append(p.Acknowledge, repo)
	p.Known[repo] = head
	return true, nil
}

// Leases is the in-memory gitops.BackupLeases.
type Leases struct {
	Now func() time.Time

	mu   sync.Mutex
	runs map[domain.TenantID]map[string]*leaseRun
}

type leaseRun struct {
	state   gitops.BackupState
	expires time.Time
}

var _ gitops.BackupLeases = (*Leases)(nil)

func (l *Leases) tenant(t domain.TenantID) map[string]*leaseRun {
	if l.runs == nil {
		l.runs = map[domain.TenantID]map[string]*leaseRun{}
	}
	if l.runs[t] == nil {
		l.runs[t] = map[string]*leaseRun{}
	}
	return l.runs[t]
}

// Acquire implements gitops.BackupLeases.
func (l *Leases) Acquire(_ context.Context, tenant domain.TenantID, id string, ttl time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	runs := l.tenant(tenant)
	for _, r := range runs {
		if r.state == gitops.BackupActive {
			if l.Now().Before(r.expires) {
				return gitops.ErrBackupInProgress
			}
			r.state = gitops.BackupFailed // abandoned
		}
	}
	runs[id] = &leaseRun{state: gitops.BackupActive, expires: l.Now().Add(ttl)}
	return nil
}

// Renew implements gitops.BackupLeases.
func (l *Leases) Renew(_ context.Context, tenant domain.TenantID, id string, ttl time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.tenant(tenant)[id]
	if r == nil || r.state != gitops.BackupActive {
		return gitops.ErrLeaseLost
	}
	r.expires = l.Now().Add(ttl)
	return nil
}

// Complete implements gitops.BackupLeases.
func (l *Leases) Complete(_ context.Context, tenant domain.TenantID, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.tenant(tenant)[id]
	if r == nil || r.state != gitops.BackupActive {
		return gitops.ErrLeaseLost
	}
	r.state = gitops.BackupComplete
	return nil
}

// Fail implements gitops.BackupLeases.
func (l *Leases) Fail(_ context.Context, tenant domain.TenantID, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r := l.tenant(tenant)[id]; r != nil && r.state == gitops.BackupActive {
		r.state = gitops.BackupFailed
	}
	return nil
}

// State implements gitops.BackupLeases.
func (l *Leases) State(_ context.Context, tenant domain.TenantID, id string) (gitops.BackupState, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.tenant(tenant)[id]
	if r == nil {
		return "", false, nil
	}
	return r.state, true, nil
}

// Intents is the in-memory gitops.PruneIntents.
type Intents struct {
	Now func() time.Time

	mu       sync.Mutex
	intents  map[string]intent
	projects map[string]domain.ProjectID // the project a repository was first declared under
	// OnDeclare runs after an intent was recorded (a test acts as a concurrent
	// module there).
	OnDeclare func(ref string)
}

type intent struct {
	tip     string
	owner   string
	expires time.Time
}

var _ gitops.PruneIntents = (*Intents)(nil)

func intentKey(tenant domain.TenantID, repo, ref string) string {
	return string(tenant) + "|" + repo + "|" + ref
}

// Declare implements gitops.PruneIntents.
func (i *Intents) Declare(_ context.Context, scope domain.Scope, repo, ref, tip, owner string, ttl time.Duration) error {
	i.mu.Lock()
	if i.intents == nil {
		i.intents = map[string]intent{}
	}
	key := intentKey(scope.TenantID, repo, ref)
	if i.projects == nil {
		i.projects = map[string]domain.ProjectID{}
	}
	if p, ok := i.projects[intentKey(scope.TenantID, repo, "")]; ok && p != scope.ProjectID {
		i.mu.Unlock()
		return errors.New("intents: the repository belongs to another project")
	}
	i.projects[intentKey(scope.TenantID, repo, "")] = scope.ProjectID
	if cur, ok := i.intents[key]; ok && cur.owner != owner && i.Now().Before(cur.expires) {
		i.mu.Unlock()
		return gitops.ErrPruneBusy
	}
	i.intents[key] = intent{tip: tip, owner: owner, expires: i.Now().Add(ttl)}
	hook := i.OnDeclare
	i.mu.Unlock()
	if hook != nil {
		hook(ref)
	}
	return nil
}

// Release implements gitops.PruneIntents: only the owner's reservation.
func (i *Intents) Release(_ context.Context, scope domain.Scope, repo, ref, owner string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	key := intentKey(scope.TenantID, repo, ref)
	if cur, ok := i.intents[key]; ok && cur.owner == owner {
		delete(i.intents, key)
	}
	return nil
}

// Conflicts implements gitops.PruneIntents.
func (i *Intents) Conflicts(_ context.Context, scope domain.Scope, repo, ref, commit string) (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for k, v := range i.intents {
		if !i.Now().Before(v.expires) || !strings.HasPrefix(k, intentKey(scope.TenantID, repo, "")) {
			continue
		}
		if k == intentKey(scope.TenantID, repo, ref) || v.tip == commit {
			return true, nil
		}
	}
	return false, nil
}

// Count is the number of live reservations (for assertions).
func (i *Intents) Count() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.intents)
}

// Recoveries is the in-memory gitops.RecoveryStore.
type Recoveries struct {
	mu   sync.Mutex
	last map[domain.TenantID]*gitops.RecoveryRecord
	// FailRead makes Current fail.
	FailRead error
}

var _ gitops.RecoveryStore = (*Recoveries)(nil)

// rec returns the tenant's recovery; steps are recorded only while it is active
// (the database's rule).
func (s *Recoveries) rec(t domain.TenantID, id string) (*gitops.RecoveryRecord, error) {
	r := s.last[t]
	if r == nil || r.ID != id {
		return nil, errors.New("recoveries: no such recovery")
	}
	if r.Status != gitops.RecoveryActive {
		return nil, errors.New("recoveries: the recovery is not active")
	}
	return r, nil
}

func clone(r *gitops.RecoveryRecord) gitops.RecoveryRecord {
	c := *r
	c.Replays = map[string]gitops.ReplayResult{}
	for k, v := range r.Replays {
		c.Replays[k] = v
	}
	c.Resolutions = append([]gitops.DiscrepancyResolution(nil), r.Resolutions...)
	return c
}

// Begin implements gitops.RecoveryStore.
func (s *Recoveries) Begin(_ context.Context, tenant domain.TenantID, id string, src gitops.RestoreSource, _ time.Time) (gitops.RecoveryRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = map[domain.TenantID]*gitops.RecoveryRecord{}
	}
	if cur := s.last[tenant]; cur != nil && cur.Status == gitops.RecoveryActive {
		if cur.SnapshotID != src.SnapshotID || cur.BackupID != src.BackupID {
			return gitops.RecoveryRecord{}, gitops.ErrRecoveryActive
		}
		return clone(cur), nil
	}
	r := &gitops.RecoveryRecord{ID: id, Tenant: tenant, SnapshotID: src.SnapshotID, BackupID: src.BackupID, Status: gitops.RecoveryActive,
		VolumeRestored: src.SnapshotID == "", Replays: map[string]gitops.ReplayResult{}}
	s.last[tenant] = r
	return clone(r), nil
}

// Current implements gitops.RecoveryStore.
func (s *Recoveries) Current(_ context.Context, tenant domain.TenantID) (gitops.RecoveryRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.FailRead != nil {
		return gitops.RecoveryRecord{}, false, s.FailRead
	}
	r := s.last[tenant]
	if r == nil {
		return gitops.RecoveryRecord{}, false, nil
	}
	return clone(r), true, nil
}

// MarkVolumeRestored implements gitops.RecoveryStore.
func (s *Recoveries) MarkVolumeRestored(_ context.Context, tenant domain.TenantID, id string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.rec(tenant, id)
	if err != nil {
		return err
	}
	r.VolumeRestored = true
	return nil
}

// MarkReplayed implements gitops.RecoveryStore.
func (s *Recoveries) MarkReplayed(_ context.Context, tenant domain.TenantID, id, repo string, res gitops.ReplayResult, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.rec(tenant, id)
	if err != nil {
		return err
	}
	if _, done := r.Replays[repo]; !done { // the first record stands
		r.Replays[repo] = res
	}
	return nil
}

// Resolve implements gitops.RecoveryStore.
func (s *Recoveries) Resolve(_ context.Context, tenant domain.TenantID, id string, res gitops.DiscrepancyResolution) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.rec(tenant, id)
	if err != nil {
		return err
	}
	for _, x := range r.Resolutions {
		if x.Repository == res.Repository && x.Ref == res.Ref && x.Kind == res.Kind && x.Actual == res.Actual {
			return nil // the first record stands
		}
	}
	r.Resolutions = append(r.Resolutions, res)
	return nil
}

// Complete implements gitops.RecoveryStore.
func (s *Recoveries) Complete(_ context.Context, tenant domain.TenantID, id string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.last[tenant]; r != nil && r.ID == id && r.Status == gitops.RecoveryComplete {
		return nil // idempotent
	}
	r, err := s.rec(tenant, id)
	if err != nil {
		return err
	}
	if !r.VolumeRestored {
		return errors.New("recoveries: a recovery completes only after its volume restore")
	}
	r.Status = gitops.RecoveryComplete
	return nil
}
