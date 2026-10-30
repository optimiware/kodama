package gitwebhooktest

import (
	"context"
	"sort"
	"sync"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
)

// NativeStore is an in-memory gitwebhook.NativeStore. It proves policy
// (uniqueness, optimistic versions, write-once outcomes, the lease fence), never
// SQL semantics: the PostgreSQL adapter runs the same NativeStoreContract.
type NativeStore struct {
	mu       sync.Mutex
	triggers map[nativeTriggerKey]gw.PushTrigger
	routes   map[domain.Scope]gw.TaskRoute
	inbox    map[gw.InboxKey]gw.InboxRecord
	audits   []gw.AuditRecord
	// FailFinishes makes that many FinishInbox calls fail with an outage (a lost
	// result) before the next one succeeds.
	FailFinishes int
}

type nativeTriggerKey struct {
	scope domain.Scope
	id    string
}

var _ gw.NativeStore = (*NativeStore)(nil)

// NewNativeStore returns an empty store.
func NewNativeStore() *NativeStore {
	return &NativeStore{triggers: map[nativeTriggerKey]gw.PushTrigger{}, routes: map[domain.Scope]gw.TaskRoute{}, inbox: map[gw.InboxKey]gw.InboxRecord{}}
}

// Audits returns a copy of the audit records written so far.
func (s *NativeStore) Audits() []gw.AuditRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]gw.AuditRecord(nil), s.audits...)
}

// Inbox returns the record of a key, if any.
func (s *NativeStore) Inbox(key gw.InboxKey) (gw.InboxRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.inbox[key]
	return r, ok
}

// InsertPushTrigger implements gitwebhook.NativeStore.
func (s *NativeStore) InsertPushTrigger(_ context.Context, t gw.PushTrigger, audit gw.AuditRecord) error {
	if err := t.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := nativeTriggerKey{t.Scope, t.ID()}
	if _, ok := s.triggers[k]; ok {
		return gw.ErrConflict
	}
	s.triggers[k] = t.Clone()
	s.audits = append(s.audits, audit)
	return nil
}

// PushTrigger implements gitwebhook.NativeStore.
func (s *NativeStore) PushTrigger(_ context.Context, scope domain.Scope, id string) (gw.PushTrigger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.triggers[nativeTriggerKey{scope, id}]
	if !ok {
		return gw.PushTrigger{}, gw.ErrNotFound
	}
	return t.Clone(), nil
}

// UpdatePushTrigger implements gitwebhook.NativeStore.
func (s *NativeStore) UpdatePushTrigger(_ context.Context, t gw.PushTrigger, expected int64, audit gw.AuditRecord) (gw.PushTrigger, error) {
	if err := t.Validate(); err != nil {
		return gw.PushTrigger{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := nativeTriggerKey{t.Scope, t.ID()}
	cur, ok := s.triggers[k]
	switch {
	case !ok:
		return gw.PushTrigger{}, gw.ErrNotFound
	case cur.Version != expected:
		return gw.PushTrigger{}, gw.ErrConflict
	case cur.Config.RepositoryID != t.Config.RepositoryID:
		return gw.PushTrigger{}, gw.ErrConflict
	}
	next := t.Clone()
	next.Version, next.CreatedAt = expected+1, cur.CreatedAt
	s.triggers[k] = next
	s.audits = append(s.audits, audit)
	return next.Clone(), nil
}

func (s *NativeStore) sorted(scope domain.Scope, keep func(gw.PushTrigger) bool) []gw.PushTrigger {
	var out []gw.PushTrigger
	for k, t := range s.triggers {
		if k.scope == scope && keep(t) {
			out = append(out, t.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// ListPushTriggers implements gitwebhook.NativeStore.
func (s *NativeStore) ListPushTriggers(_ context.Context, scope domain.Scope, after string, limit int) ([]gw.PushTrigger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.sorted(scope, func(t gw.PushTrigger) bool { return t.ID() > after })
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// EnabledPushTriggers implements gitwebhook.NativeStore.
func (s *NativeStore) EnabledPushTriggers(_ context.Context, scope domain.Scope, repositoryID string) ([]gw.PushTrigger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sorted(scope, func(t gw.PushTrigger) bool { return t.Config.Enabled && t.Config.RepositoryID == repositoryID }), nil
}

// PutTaskRoute implements gitwebhook.NativeStore.
func (s *NativeStore) PutTaskRoute(_ context.Context, r gw.TaskRoute, expected int64, audit gw.AuditRecord) (gw.TaskRoute, error) {
	if err := r.Validate(); err != nil {
		return gw.TaskRoute{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.routes[r.Scope]
	switch {
	case !ok && expected != 0, ok && cur.Version != expected:
		return gw.TaskRoute{}, gw.ErrConflict
	case ok:
		r.CreatedAt = cur.CreatedAt
	}
	r.Version = expected + 1
	s.routes[r.Scope] = r
	s.audits = append(s.audits, audit)
	return r, nil
}

// TaskRoute implements gitwebhook.NativeStore.
func (s *NativeStore) TaskRoute(_ context.Context, scope domain.Scope) (gw.TaskRoute, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.routes[scope]
	if !ok {
		return gw.TaskRoute{}, gw.ErrNotFound
	}
	return r, nil
}

// ClaimInbox implements gitwebhook.NativeStore.
func (s *NativeStore) ClaimInbox(_ context.Context, key gw.InboxKey, holder string, now time.Time, ttl time.Duration) (gw.InboxRecord, bool, error) {
	if err := key.Validate(); err != nil {
		return gw.InboxRecord{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.inbox[key]
	if !ok {
		rec = gw.InboxRecord{Key: key, ReceivedAt: now}
	}
	if rec.Done {
		return rec, false, nil
	}
	if rec.LeaseHolder != "" && rec.LeaseHolder != holder && now.Before(rec.LeaseUntil) {
		return rec, false, nil
	}
	rec.LeaseHolder, rec.LeaseUntil = holder, now.Add(ttl)
	s.inbox[key] = rec
	return rec, true, nil
}

// FinishInbox implements gitwebhook.NativeStore.
func (s *NativeStore) FinishInbox(_ context.Context, key gw.InboxKey, fin gw.InboxFinish, now time.Time, audit gw.AuditRecord) (gw.InboxRecord, error) {
	if err := key.Validate(); err != nil {
		return gw.InboxRecord{}, err
	}
	switch {
	case fin.Outcome == gw.OutcomeStarted && fin.RunID == "",
		fin.Outcome != gw.OutcomeStarted && (fin.RunID != "" || fin.Reason == ""),
		fin.Outcome != gw.OutcomeStarted && fin.Outcome != gw.OutcomeIgnored && fin.Outcome != gw.OutcomeRejected:
		return gw.InboxRecord{}, gw.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.FailFinishes > 0 {
		s.FailFinishes--
		return gw.InboxRecord{}, gw.ErrUnavailable
	}
	rec, ok := s.inbox[key]
	switch {
	case !ok:
		return gw.InboxRecord{}, gw.ErrNotFound
	case rec.Done:
		return rec, nil
	case fin.Outcome != gw.OutcomeStarted && (fin.Holder == "" || fin.Holder != rec.LeaseHolder || !now.Before(rec.LeaseUntil)):
		return gw.InboxRecord{}, gw.ErrConflict
	}
	rec.Done, rec.Outcome, rec.Reason, rec.RunID, rec.CompletedAt = true, fin.Outcome, fin.Reason, fin.RunID, now
	rec.LeaseHolder, rec.LeaseUntil = "", time.Time{}
	s.inbox[key] = rec
	s.audits = append(s.audits, audit)
	return rec, nil
}

// LatestInbox implements gitwebhook.NativeStore: the latest completion, ties
// broken by subject and ref in byte order descending.
func (s *NativeStore) LatestInbox(_ context.Context, scope domain.Scope, kind gw.InboxKind, source string, outcome gw.Outcome) (gw.InboxRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best gw.InboxRecord
	found := false
	for k, r := range s.inbox {
		if !r.Done || k.Scope != scope || k.Kind != kind || k.Source != source || (outcome != "" && r.Outcome != outcome) {
			continue
		}
		later := !found || r.CompletedAt.After(best.CompletedAt) ||
			(r.CompletedAt.Equal(best.CompletedAt) && (k.Subject > best.Key.Subject || (k.Subject == best.Key.Subject && k.Ref > best.Key.Ref)))
		if later {
			best, found = r, true
		}
	}
	return best, found, nil
}

// ReleaseInbox implements gitwebhook.NativeStore.
func (s *NativeStore) ReleaseInbox(_ context.Context, key gw.InboxKey, holder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.inbox[key]; ok && !rec.Done && rec.LeaseHolder == holder {
		rec.LeaseHolder, rec.LeaseUntil = "", time.Time{}
		s.inbox[key] = rec
	}
	return nil
}

// ---- port doubles ----

// PushRecords is an in-memory gitwebhook.PushRecords.
type PushRecords struct {
	mu      sync.Mutex
	records map[domain.Scope]map[string]gitv1.PushRecord
	// Reloads counts calls, so a test can prove an excluded push reloads nothing.
	Reloads int
	// Err, when set, is returned by Reload (an outage).
	Err error
}

var _ gw.PushRecords = (*PushRecords)(nil)

// Put stores a record under the scope.
func (p *PushRecords) Put(scope domain.Scope, rec gitv1.PushRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.records == nil {
		p.records = map[domain.Scope]map[string]gitv1.PushRecord{}
	}
	if p.records[scope] == nil {
		p.records[scope] = map[string]gitv1.PushRecord{}
	}
	p.records[scope][rec.ReceiveID] = rec
}

// Reload implements gitwebhook.PushRecords: a record of another scope or
// repository is ErrNotFound.
func (p *PushRecords) Reload(_ context.Context, scope domain.Scope, repositoryID, receiveID string) (gitv1.PushRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Reloads++
	if p.Err != nil {
		return gitv1.PushRecord{}, p.Err
	}
	rec, ok := p.records[scope][receiveID]
	if !ok || rec.RepositoryID != repositoryID {
		return gitv1.PushRecord{}, gw.ErrNotFound
	}
	return rec, nil
}

// NativeRepositories is an in-memory gitwebhook.PushRepositories.
type NativeRepositories struct {
	mu    sync.Mutex
	repos map[domain.Scope]map[string]gw.RepositoryFacts
}

var _ gw.PushRepositories = (*NativeRepositories)(nil)

// Put registers a repository in the scope.
func (r *NativeRepositories) Put(scope domain.Scope, f gw.RepositoryFacts) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.repos == nil {
		r.repos = map[domain.Scope]map[string]gw.RepositoryFacts{}
	}
	if r.repos[scope] == nil {
		r.repos[scope] = map[string]gw.RepositoryFacts{}
	}
	r.repos[scope][f.ID] = f
}

// Repository implements gitwebhook.PushRepositories.
func (r *NativeRepositories) Repository(_ context.Context, scope domain.Scope, id string) (gw.RepositoryFacts, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.repos[scope][id]
	if !ok {
		return gw.RepositoryFacts{}, gw.ErrNotFound
	}
	return f, nil
}

// WorkItems is an in-memory gitwebhook.WorkItems.
type WorkItems struct {
	mu    sync.Mutex
	items map[domain.Scope]map[domain.WorkItemID]gw.WorkFacts
	Err   error
}

var _ gw.WorkItems = (*WorkItems)(nil)

// Set stores the item's facts.
func (w *WorkItems) Set(scope domain.Scope, id domain.WorkItemID, f gw.WorkFacts) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.items == nil {
		w.items = map[domain.Scope]map[domain.WorkItemID]gw.WorkFacts{}
	}
	if w.items[scope] == nil {
		w.items[scope] = map[domain.WorkItemID]gw.WorkFacts{}
	}
	w.items[scope][id] = f
}

// Facts implements gitwebhook.WorkItems.
func (w *WorkItems) Facts(_ context.Context, scope domain.Scope, id domain.WorkItemID) (gw.WorkFacts, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.Err != nil {
		return gw.WorkFacts{}, w.Err
	}
	f, ok := w.items[scope][id]
	if !ok {
		return gw.WorkFacts{}, gw.ErrNotFound
	}
	return f, nil
}

// SourceRuns is an in-memory gitwebhook.SourceRuns that behaves as the real
// admission does where the trigger depends on it: a run ID is created once, the
// same ID with the same request is a replay, and a push run pins the commit of
// the entry the *record* names (here: the request's ExpectedCommit), never a
// branch tip.
type SourceRuns struct {
	mu   sync.Mutex
	runs map[runKey]SourceRun
	// Requests records every Admit call in order.
	Requests []gw.SourceRunRequest
	// Err, when set, is returned by Admit (an outage or a refusal); ErrOnce
	// clears it after one call. LoseResponse makes the next successful Admit
	// create the run but fail the response (a lost acknowledgement).
	Err          error
	ErrOnce      bool
	LoseResponse bool
	// FindErr is returned by Find.
	FindErr error
}

// SourceRun is a run created by the double.
type SourceRun struct {
	Request gw.SourceRunRequest
	// Commit is the commit the run pinned: the push origin's expected commit.
	Commit string
}

type runKey struct {
	scope domain.Scope
	id    domain.RunID
}

var _ gw.SourceRuns = (*SourceRuns)(nil)

// Admit implements gitwebhook.SourceRuns.
func (s *SourceRuns) Admit(_ context.Context, req gw.SourceRunRequest) (gw.RunRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests = append(s.Requests, req)
	if s.Err != nil {
		err := s.Err
		if s.ErrOnce {
			s.Err, s.ErrOnce = nil, false
		}
		return gw.RunRef{}, err
	}
	if s.runs == nil {
		s.runs = map[runKey]SourceRun{}
	}
	k := runKey{req.Scope, req.RunID}
	if _, ok := s.runs[k]; ok {
		return gw.RunRef{ID: req.RunID}, nil
	}
	s.runs[k] = SourceRun{Request: req, Commit: req.ExpectedCommit}
	if s.LoseResponse {
		s.LoseResponse = false
		return gw.RunRef{}, gw.ErrUnavailable
	}
	return gw.RunRef{ID: req.RunID, Created: true}, nil
}

// Find implements gitwebhook.SourceRuns.
func (s *SourceRuns) Find(_ context.Context, scope domain.Scope, id domain.RunID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.FindErr != nil {
		return false, s.FindErr
	}
	_, ok := s.runs[runKey{scope, id}]
	return ok, nil
}

// Runs returns the created runs.
func (s *SourceRuns) Runs() []SourceRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SourceRun, 0, len(s.runs))
	for _, r := range s.runs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Request.RunID < out[j].Request.RunID })
	return out
}

// Created is the number of runs created.
func (s *SourceRuns) Created() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}
