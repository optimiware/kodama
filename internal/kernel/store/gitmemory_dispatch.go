package store

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

var _ OutboxBackend = (*MemoryGit)(nil)
var _ DispatchBackend = (*MemoryGit)(nil)

// memDispatch is the memory backend's pipelines and admitted runs.
type memDispatch struct {
	pipelines []*Pipeline
	disabled  map[string]bool // pipeline ID -> disabled
	runs      []MemoryRun
	triggers  map[string]bool // tenant/project/pipeline/trigger key
}

// MemoryRun is one run admitted through DispatchPush.
type MemoryRun struct {
	TenantID   domain.TenantID
	ProjectID  domain.ProjectID
	ID         string
	PipelineID string
	Pipeline   string
	TriggerKey string
	Definition json.RawMessage
	Sources    json.RawMessage
	Order      []string
	Steps      []Step
}

// Pipelines returns the git-indexed pipelines of a repository, including
// disabled ones, sorted by name.
func (m *MemoryGit) Pipelines(tenant domain.TenantID, repositoryID string) []Pipeline {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Pipeline
	for _, p := range m.dispatch.pipelines {
		if p.TenantID == tenant && p.RepositoryID == repositoryID {
			c := *p
			c.Disabled = m.dispatch.disabled[p.ID]
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// AdmittedRuns returns the runs admitted so far, in order.
func (m *MemoryGit) AdmittedRuns() []MemoryRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]MemoryRun(nil), m.dispatch.runs...)
}

func (m *MemoryGit) PendingEvents(_ context.Context, tenant domain.TenantID, now time.Time, limit int) ([]OutboxEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var due []MemoryEvent
	for _, e := range m.events {
		if e.TenantID == tenant && e.PublishedAt == nil && !e.NextAttempt.After(now) {
			due = append(due, e)
		}
	}
	sort.SliceStable(due, func(i, j int) bool {
		if !due[i].NextAttempt.Equal(due[j].NextAttempt) {
			return due[i].NextAttempt.Before(due[j].NextAttempt)
		}
		return due[i].ID < due[j].ID
	})
	if len(due) > limit {
		due = due[:limit]
	}
	var tenantKey string
	for k, id := range m.tenants {
		if id == tenant {
			tenantKey = k
		}
	}
	out := make([]OutboxEvent, 0, len(due))
	for _, e := range due {
		projectKey := ""
		if e.ProjectID != "" {
			projectKey, _ = m.projectKey(tenant, e.ProjectID)
		}
		out = append(out, OutboxEvent{TenantID: tenant, ProjectID: e.ProjectID, TenantKey: tenantKey, ProjectKey: projectKey,
			ID: e.ID, ActorType: e.ActorType, ActorID: e.ActorID, Type: e.Type, ResourceType: e.ResourceType, ResourceID: e.ResourceID,
			Payload: e.Payload, CreatedAt: e.CreatedAt, Attempts: e.Attempts})
	}
	return out, nil
}

func (m *MemoryGit) MarkPublished(_ context.Context, tenant domain.TenantID, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.events {
		if e := &m.events[i]; e.TenantID == tenant && e.ID == id && e.PublishedAt == nil {
			t := at
			e.PublishedAt = &t
		}
	}
	return nil
}

func (m *MemoryGit) PublishFailed(_ context.Context, tenant domain.TenantID, id string, next time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.events {
		if e := &m.events[i]; e.TenantID == tenant && e.ID == id && e.PublishedAt == nil {
			e.Attempts++
			e.NextAttempt = next
		}
	}
	return nil
}

func (m *MemoryGit) Dispatched(_ context.Context, tenant domain.TenantID, eventID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.events {
		if e.TenantID == tenant && e.ID == DispatchMarkerID(eventID) {
			return true, nil
		}
	}
	return false, nil
}

// DispatchPush is the memory form of Store.DispatchPush: one lock stands for the
// repository row lock and the transaction, and a failure leaves nothing applied.
func (m *MemoryGit) DispatchPush(_ context.Context, d PushDispatch) (PushDispatchResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := PushDispatchResult{}
	repo := m.repo(d.TenantID, d.RepositoryID)
	if repo == nil {
		return res, ErrNotFound
	}
	if repo.ProjectID == "" {
		res.Skipped = true
		return res, nil
	}
	if repo.Status == RepoArchived {
		res.Archived = true
		return res, nil
	}
	proj := repo.ProjectID
	for _, e := range m.events {
		if e.TenantID == d.TenantID && e.ID == DispatchMarkerID(d.EventID) {
			res.Replayed = true
			return res, nil
		}
	}
	// Work on copies so that a planner failure applies nothing.
	pipelines := make([]*Pipeline, len(m.dispatch.pipelines))
	for i, p := range m.dispatch.pipelines {
		c := *p
		pipelines[i] = &c
	}
	disabled := map[string]bool{}
	for k, v := range m.dispatch.disabled {
		disabled[k] = v
	}
	var refusals []MemoryEvent
	marker := dispatchMarker{RepositoryID: d.RepositoryID, ReceiveID: d.ReceiveID, EventID: d.EventID,
		JournalPosition: d.JournalPosition, Pipelines: []string{}, Disabled: []string{}, Runs: []string{}}
	now := m.now()

	if ix := d.Index; ix != nil {
		marker.Commit = ix.Commit
		if d.JournalPosition > 0 {
			for _, e := range m.events {
				if e.TenantID != d.TenantID || e.Type != EventPipelinesIndexed || e.ResourceID != d.RepositoryID {
					continue
				}
				var other dispatchMarker
				if json.Unmarshal(e.Payload, &other) == nil && other.Indexed && other.JournalPosition > d.JournalPosition {
					res.Stale = true
				}
			}
		}
		marker.Stale = res.Stale
		if !res.Stale {
			marker.Indexed = true
			for _, def := range ix.Definitions {
				var found *Pipeline
				for _, p := range pipelines {
					if p.TenantID == d.TenantID && p.RepositoryID == d.RepositoryID && p.Name == def.Name {
						found = p
					}
				}
				if found == nil {
					id, err := newID("pl")
					if err != nil {
						return PushDispatchResult{}, err
					}
					found = &Pipeline{TenantID: d.TenantID, ProjectID: proj, ID: id, Name: def.Name, Version: 0, CreatedAt: now, RepositoryID: d.RepositoryID}
					pipelines = append(pipelines, found)
				}
				found.Definition, found.Source, found.SourceCommit = def.Definition, "git", ix.Commit
				found.Version++
				found.UpdatedAt = now
				disabled[found.ID] = false
				res.Indexed = append(res.Indexed, def.Name)
			}
			if !ix.Unreadable {
				present := map[string]bool{}
				for _, n := range ix.Present {
					present[n] = true
				}
				for _, p := range pipelines {
					if p.TenantID == d.TenantID && p.RepositoryID == d.RepositoryID && !disabled[p.ID] && !present[p.Name] {
						disabled[p.ID] = true
						p.Version++
						p.UpdatedAt = now
						res.Disabled = append(res.Disabled, p.Name)
					}
				}
			}
			for _, r := range ix.Refusals {
				payload, _ := json.Marshal(map[string]any{"repositoryId": d.RepositoryID, "ref": ix.Ref, "commit": ix.Commit,
					"path": r.Path, "code": r.Code, "documentPath": nullIfEmpty(r.DocumentPath)})
				refusals = append(refusals, MemoryEvent{TenantID: d.TenantID, ProjectID: proj, ID: refusalID(d.EventID, r.Path), ActorType: "system",
					ActorID: "kernel-dispatch", Type: EventPipelineRefused, ResourceType: "repository", ResourceID: d.RepositoryID,
					Payload: payload, CreatedAt: now, NextAttempt: now})
				res.Refused++
			}
			marker.Pipelines, marker.Disabled, marker.Refused = nonNil(res.Indexed), nonNil(res.Disabled), res.Refused
		}
	}

	var admitted []MemoryRun
	triggers := map[string]bool{}
	for k := range m.dispatch.triggers {
		triggers[k] = true
	}
	if d.Plan != nil {
		var current []Pipeline
		for _, p := range pipelines {
			if p.TenantID == d.TenantID && p.RepositoryID == d.RepositoryID && !disabled[p.ID] {
				current = append(current, *p)
			}
		}
		sort.Slice(current, func(i, j int) bool { return current[i].Name < current[j].Name })
		plan, err := d.Plan(current)
		if err != nil {
			return PushDispatchResult{}, err
		}
		for _, a := range plan {
			key := string(d.TenantID) + "/" + string(proj) + "/" + a.PipelineID + "/" + a.TriggerKey
			if a.TriggerKey == "" {
				return PushDispatchResult{}, domain.ErrInvalidValue
			}
			if triggers[key] {
				continue
			}
			triggers[key] = true
			name := ""
			for _, p := range pipelines {
				if p.ID == a.PipelineID {
					name = p.Name
				}
			}
			admitted = append(admitted, MemoryRun{TenantID: d.TenantID, ProjectID: proj, ID: a.Run.ID, PipelineID: a.PipelineID, Pipeline: name,
				TriggerKey: a.TriggerKey, Definition: a.Run.Definition, Sources: a.Sources, Order: a.Run.ExecutionOrder, Steps: a.Steps})
			res.Runs = append(res.Runs, a.Run.ID)
		}
		marker.Runs = nonNil(res.Runs)
	}

	payload, err := json.Marshal(marker)
	if err != nil {
		return PushDispatchResult{}, err
	}
	// Commit.
	m.dispatch.pipelines = pipelines
	m.dispatch.disabled = disabled
	m.dispatch.triggers = triggers
	m.dispatch.runs = append(m.dispatch.runs, admitted...)
	m.events = append(m.events, refusals...)
	m.event(d.TenantID, proj, DispatchMarkerID(d.EventID), "system", "kernel-dispatch", EventPipelinesIndexed, "repository", d.RepositoryID, payload)
	return res, nil
}

// TenantIDs lists the registered tenants, ordered by ID.
func (m *MemoryGit) TenantIDs(context.Context) ([]domain.TenantID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]domain.TenantID, 0, len(m.tenants))
	for _, id := range m.tenants {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
