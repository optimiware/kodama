package pipelines

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Memory is the in-process Store adapter. It exists for tests and the
// `development` environment; it persists nothing and is never a production
// fallback. It applies the same decide policy and the same scope rules as the
// PostgreSQL adapter, and the shared contract suite holds both to them.
//
// Projects and repositories are not owned by the registry: the memory adapter is
// told which exist (AddProject, AddRepository), as the database adapter reads
// them from its tables.
type Memory struct {
	mu       sync.Mutex
	now      func() time.Time
	projects map[domain.Scope]*memProject
}

type memProject struct {
	repos     map[string]bool
	byName    map[string]*Definition
	byID      map[string]*Definition
	attempts  map[string]memAttempt
	conflicts []Conflict
}

type memAttempt struct {
	pipelineID string
	conflict   *Conflict
}

var (
	_ Store = (*Memory)(nil)
)

// NewMemory returns an empty adapter; now defaults to time.Now.
func NewMemory(now func() time.Time) *Memory {
	if now == nil {
		now = time.Now
	}
	return &Memory{now: now, projects: map[domain.Scope]*memProject{}}
}

// AddProject makes a project known. It is idempotent.
func (m *Memory) AddProject(scope domain.Scope) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.project(scope, true)
}

// AddRepository makes a repository of an existing project known.
func (m *Memory) AddRepository(scope domain.Scope, repositoryID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.project(scope, true); p != nil {
		p.repos[repositoryID] = true
	}
}

func (m *Memory) project(scope domain.Scope, create bool) *memProject {
	p := m.projects[scope]
	if p == nil && create {
		p = &memProject{repos: map[string]bool{}, byName: map[string]*Definition{}, byID: map[string]*Definition{}, attempts: map[string]memAttempt{}}
		m.projects[scope] = p
	}
	return p
}

func (m *Memory) Resolve(_ context.Context, scope domain.Scope, name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.project(scope, false)
	if p == nil || p.byName[name] == nil {
		return "", ErrNotFound
	}
	return p.byName[name].ID, nil
}

func (m *Memory) Get(_ context.Context, scope domain.Scope, id string) (Definition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.project(scope, false)
	if p == nil || p.byID[id] == nil {
		return Definition{}, ErrNotFound
	}
	return clone(p.byID[id]), nil
}

func (m *Memory) Conflicts(_ context.Context, scope domain.Scope) ([]Conflict, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.project(scope, false)
	if p == nil {
		return nil, ErrNotFound
	}
	out := append([]Conflict(nil), p.conflicts...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Recorded.Before(out[j].Recorded) })
	return out, nil
}

func (m *Memory) Submit(_ context.Context, sub Submission) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.project(sub.Scope, false)
	if p == nil {
		return Result{}, ErrNotFound
	}
	if sub.Source.Kind == SourceGit && !p.repos[sub.Source.RepositoryID] {
		return Result{}, ErrNotFound
	}

	// A recorded attempt is answered from the record, whatever has happened to
	// the name since.
	if a, ok := p.attempts[sub.AttemptKey]; ok {
		if a.conflict != nil {
			return Result{Replayed: true}, &ConflictError{Name: sub.Name, Existing: a.conflict.Existing, Attempted: a.conflict.Attempted}
		}
		return Result{Definition: clone(p.byID[a.pipelineID]), Replayed: true}, nil
	}

	existing := p.byName[sub.Name]
	same := existing != nil && bytes.Equal(existing.Document, sub.Document)
	now := m.now()
	switch decide(existing, sub, same) {
	case ActionCreate:
		def := &Definition{Scope: sub.Scope, ID: m.newID(), Name: sub.Name, Source: sub.Source, Commit: sub.Commit,
			Document: append([]byte(nil), sub.Document...), Version: 1, Created: now, Updated: now}
		p.byName[def.Name], p.byID[def.ID] = def, def
		m.record(p, sub, def.ID)
		return Result{Definition: clone(def), Created: true}, nil
	case ActionUnchanged:
		m.record(p, sub, existing.ID)
		return Result{Definition: clone(existing)}, nil
	case ActionUpdate:
		existing.Document = append([]byte(nil), sub.Document...)
		existing.Commit, existing.Disabled = sub.Commit, false
		existing.Version++
		existing.Updated = now
		m.record(p, sub, existing.ID)
		return Result{Definition: clone(existing), Updated: true}, nil
	case ActionDuplicate:
		return Result{}, ErrDuplicateName
	default: // ActionConflict
		c := Conflict{ID: "pa_" + sub.AttemptKey, Scope: sub.Scope, Name: sub.Name, PipelineID: existing.ID,
			Existing: existing.Source, Attempted: sub.Source, EventID: sub.EventID, Ref: sub.Ref, Commit: sub.Commit, Recorded: now}
		p.conflicts = append(p.conflicts, c)
		p.attempts[sub.AttemptKey] = memAttempt{conflict: &c}
		return Result{}, &ConflictError{Name: sub.Name, Existing: existing.Source, Attempted: sub.Source}
	}
}

// record remembers an accepted Git attempt. An API registration is not an
// indexing attempt and replaying it is a duplicate, so it is not recorded.
func (m *Memory) record(p *memProject, sub Submission, id string) {
	if sub.Source.Kind == SourceGit {
		p.attempts[sub.AttemptKey] = memAttempt{pipelineID: id}
	}
}

func (m *Memory) newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("pipelines: entropy unavailable: " + err.Error())
	}
	return "pl_" + hex.EncodeToString(b[:])
}

func clone(d *Definition) Definition {
	c := *d
	c.Document = append([]byte(nil), d.Document...)
	return c
}
