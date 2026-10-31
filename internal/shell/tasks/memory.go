package tasks

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Memory is the in-process Store. It exists for unit tests and for the
// development environment's executable-contract evidence: it proves the store
// policy (content versions, the one current run, the projection guard, scope
// isolation, the outbox committed with the state) over retained state. It
// persists nothing and says nothing about PostgreSQL row security or locking,
// which the shared contract suite qualifies against the PostgreSQL adapter.
type Memory struct {
	mu     sync.Mutex
	tasks  map[taskKey]Task
	runs   map[runKey]domain.TaskID
	events []Event
	fault  error
}

type taskKey struct {
	scope domain.Scope
	id    domain.TaskID
}

type runKey struct {
	scope domain.Scope
	id    domain.RunID
}

var _ Store = (*Memory)(nil)

// NewMemory returns an empty store.
func NewMemory() *Memory {
	return &Memory{tasks: map[taskKey]Task{}, runs: map[runKey]domain.TaskID{}}
}

// DevelopmentOnly flags the store as a development adapter.
func (*Memory) DevelopmentOnly() bool { return true }

// FailNextWrite makes the next write fail with err after it computed its result
// and before it stored anything, so a test can prove that nothing persists. A
// nil err clears it. It exists for failure-recovery tests.
func (m *Memory) FailNextWrite(err error) {
	m.mu.Lock()
	m.fault = err
	m.mu.Unlock()
}

// Events returns the outbox events committed so far, in commit order.
func (m *Memory) Events() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.events)
}

// Damage overwrites the stored progress of a task without any guard. It exists
// only to prove that Rebuild repairs a damaged projection from the records.
func (m *Memory) Damage(scope domain.Scope, id domain.TaskID, p Progress) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := taskKey{scope, id}
	t := m.tasks[k]
	t.Progress = p
	m.tasks[k] = t
}

// take consumes a pending fault. The caller holds the lock.
func (m *Memory) take() error {
	err := m.fault
	m.fault = nil
	return err
}

func (m *Memory) Create(_ context.Context, t Task, actor string) error {
	if err := t.Scope.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := taskKey{t.Scope, t.ID}
	if _, dup := m.tasks[k]; dup {
		return domain.ErrConflict
	}
	if t.ParentID != "" {
		if _, ok := m.tasks[taskKey{t.Scope, t.ParentID}]; !ok {
			return invalidf("the parent task is not in this project")
		}
	}
	if err := m.take(); err != nil {
		return err
	}
	m.tasks[k] = t.clone()
	m.events = append(m.events, createdEvent(t, actor))
	return nil
}

func (m *Memory) Get(_ context.Context, scope domain.Scope, id domain.TaskID) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[taskKey{scope, id}]
	if !ok {
		return Task{}, ErrNotFound
	}
	return t.clone(), nil
}

func (m *Memory) List(_ context.Context, q ListQuery) ([]Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Task
	for k, t := range m.tasks {
		if k.scope != q.Scope || k.id <= q.After && q.After != "" {
			continue
		}
		if q.Status != "" && t.Progress.Status != q.Status || q.ParentID != "" && t.ParentID != q.ParentID {
			continue
		}
		out = append(out, t.clone())
	}
	return page(out, q.Limit), nil
}

func page(ts []Task, limit int) []Task {
	slices.SortFunc(ts, func(a, b Task) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	if limit > 0 && len(ts) > limit {
		ts = ts[:limit]
	}
	return ts
}

func (m *Memory) UpdateContent(_ context.Context, scope domain.Scope, id domain.TaskID, expected domain.Version, c Content, now time.Time) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := taskKey{scope, id}
	t, ok := m.tasks[k]
	if !ok {
		return Task{}, ErrNotFound
	}
	if t.Version != expected {
		return Task{}, ErrContentConflict
	}
	if err := m.take(); err != nil {
		return Task{}, err
	}
	t.Content, t.Version, t.Updated = c, t.Version+1, now.UTC()
	m.tasks[k] = t.clone()
	return t.clone(), nil
}

func (m *Memory) Select(_ context.Context, sel RunSelection) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := taskKey{sel.Scope, sel.TaskID}
	t, ok := m.tasks[k]
	if !ok {
		return Task{}, ErrNotFound
	}
	switch {
	case t.Progress.RunID == sel.Run:
		return t.clone(), nil // already current: a replay
	case t.Progress.RunID != sel.Expected:
		return Task{}, ErrStaleRun
	}
	if err := m.take(); err != nil {
		return Task{}, err
	}
	p := sel.Progress
	p.Version = t.Progress.Version + 1
	t.Progress = p
	m.tasks[k] = t
	m.runs[runKey{sel.Scope, sel.Run}] = sel.TaskID
	m.events = append(m.events, progressEvent(t))
	return t.clone(), nil
}

func (m *Memory) Advance(_ context.Context, scope domain.Scope, id domain.TaskID, obs Progress, force bool) (Task, Verdict, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := taskKey{scope, id}
	t, ok := m.tasks[k]
	if !ok {
		return Task{}, VerdictStale, ErrNotFound
	}
	before := t.Progress.Version
	next, v := advance(t.Progress, obs, force)
	if v != VerdictApplied {
		return t.clone(), v, nil
	}
	if err := m.take(); err != nil {
		return Task{}, v, err
	}
	t.Progress = next
	m.tasks[k] = t
	if next.Version != before {
		m.events = append(m.events, progressEvent(t)) // the view changed, not only its version
	}
	return t.clone(), v, nil
}

func (m *Memory) TaskOfRun(_ context.Context, scope domain.Scope, run domain.RunID) (domain.TaskID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.runs[runKey{scope, run}]
	if !ok {
		return "", ErrNotFound
	}
	return id, nil
}

func (m *Memory) Waiting(_ context.Context, scope domain.Scope, roles []string, after domain.TaskID, limit int) ([]Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Task
	for k, t := range m.tasks {
		if k.scope != scope || (after != "" && k.id <= after) {
			continue
		}
		if t.Progress.Status == StatusWaiting && t.Progress.WaitingFor != "" && slices.Contains(roles, t.Progress.WaitingFor) {
			out = append(out, t.clone())
		}
	}
	return page(out, limit), nil
}
