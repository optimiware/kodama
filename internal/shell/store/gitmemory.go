package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// GitBackend is what the Git hosting routes and the worker's control plane need
// of persistence: the PostgreSQL *Store implements it, and MemoryGit implements
// it for unit tests with the same transaction semantics (one lock stands for the
// repository row lock).
type GitBackend interface {
	TenantByKey(ctx context.Context, key string) (domain.TenantID, error)
	UserIDForPrincipal(ctx context.Context, issuer, subject string) (string, error)
	Roles(ctx context.Context, tenant domain.TenantID, userID, project string) ([]string, error)
	GetProject(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) (Project, error)
	GetProjectByKey(ctx context.Context, tenant domain.TenantID, key string) (Project, error)

	CreateRepository(ctx context.Context, r Repository) (Repository, bool, error)
	GetRepository(ctx context.Context, tenant domain.TenantID, id string) (Repository, error)
	RepositoryByPath(ctx context.Context, tenant domain.TenantID, projectKey, name string) (Repository, error)
	ListRepositories(ctx context.Context, tenant domain.TenantID, project domain.ProjectID) ([]Repository, error)
	UpdateRepository(ctx context.Context, tenant domain.TenantID, id string, expected domain.Version, patch RepositoryPatch) (Repository, error)
	SetRepositoryStatus(ctx context.Context, tenant domain.TenantID, id, to, reason, actorType, actorID string) (Repository, error)
	ConfirmInitialized(ctx context.Context, tenant domain.TenantID, id, defaultBranch string, empty bool) (Repository, error)
	ProvisioningRepositories(ctx context.Context, tenant domain.TenantID, limit int) ([]Repository, error)
	ReconcilableRepositories(ctx context.Context, tenant domain.TenantID, afterID string, limit int) ([]Repository, error)

	IssueCredential(ctx context.Context, c Credential) error
	IssueSeed(ctx context.Context, c Credential) error
	CredentialByDigest(ctx context.Context, tenant domain.TenantID, digest string) (Credential, error)
	GetCredential(ctx context.Context, tenant domain.TenantID, id string) (Credential, error)
	AuthorizeSeed(ctx context.Context, tenant domain.TenantID, credentialID, receiveID string) error
	RevokeRunCredentials(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, runID string) error
	RunGitInfo(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, runID string) (RunGitInfo, error)

	CommitReceive(ctx context.Context, c ReceiveCommit) (ReceiveResult, error)
}

var _ GitBackend = (*Store)(nil)
var _ GitBackend = (*MemoryGit)(nil)

// MemoryEvent is one audit_events row of the memory backend.
type MemoryEvent struct {
	TenantID     domain.TenantID
	ProjectID    domain.ProjectID
	ID           string
	ActorType    string
	ActorID      string
	Type         string
	ResourceType string
	ResourceID   string
	Payload      json.RawMessage

	// Outbox progress (published_at, publish_attempts, next_attempt_at).
	CreatedAt   time.Time
	PublishedAt *time.Time
	Attempts    int
	NextAttempt time.Time
}

type memMembership struct {
	tenant        domain.TenantID
	project, user string
	role          string
}

// MemoryGit is the in-memory GitBackend. It is safe for concurrent use.
type MemoryGit struct {
	mu          sync.Mutex
	now         func() time.Time
	tenants     map[string]domain.TenantID // key -> id
	projects    map[domain.TenantID][]Project
	users       map[string]string // issuer\x00subject -> user id
	memberships []memMembership
	repos       map[domain.TenantID][]*Repository
	creds       map[domain.TenantID][]*Credential
	runs        map[string]RunGitInfo
	events      []MemoryEvent

	dispatch memDispatch
}

// NewMemoryGit returns an empty backend using the system clock.
func NewMemoryGit() *MemoryGit {
	return &MemoryGit{now: time.Now, tenants: map[string]domain.TenantID{}, projects: map[domain.TenantID][]Project{},
		users: map[string]string{}, repos: map[domain.TenantID][]*Repository{}, creds: map[domain.TenantID][]*Credential{},
		runs: map[string]RunGitInfo{}}
}

// SetClock replaces the clock (tests).
func (m *MemoryGit) SetClock(now func() time.Time) { m.mu.Lock(); m.now = now; m.mu.Unlock() }

// AddTenant registers a tenant.
func (m *MemoryGit) AddTenant(id domain.TenantID, key string) {
	m.mu.Lock()
	m.tenants[key] = id
	m.mu.Unlock()
}

// AddProject registers a project.
func (m *MemoryGit) AddProject(tenant domain.TenantID, id domain.ProjectID, key string) {
	m.mu.Lock()
	m.projects[tenant] = append(m.projects[tenant], Project{TenantID: tenant, ID: id, Key: key, Version: 1})
	m.mu.Unlock()
}

// AddUser maps an external principal to a user ID.
func (m *MemoryGit) AddUser(id, issuer, subject string) {
	m.mu.Lock()
	m.users[issuer+"\x00"+subject] = id
	m.mu.Unlock()
}

// Grant adds a role binding; an empty project is tenant-wide.
func (m *MemoryGit) Grant(tenant domain.TenantID, userID, project, role string) {
	m.mu.Lock()
	m.memberships = append(m.memberships, memMembership{tenant, project, userID, role})
	m.mu.Unlock()
}

// Revoke removes every binding of a user's role in a project scope.
func (m *MemoryGit) Revoke(tenant domain.TenantID, userID, project, role string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.memberships[:0]
	for _, x := range m.memberships {
		if !(x.tenant == tenant && x.user == userID && x.project == project && x.role == role) {
			kept = append(kept, x)
		}
	}
	m.memberships = kept
}

// SetRun registers or replaces a run's Git-relevant state.
func (m *MemoryGit) SetRun(r RunGitInfo) {
	m.mu.Lock()
	m.runs[string(r.TenantID)+"/"+string(r.ProjectID)+"/"+r.RunID] = r
	m.mu.Unlock()
}

// Events returns the audit rows committed so far, in order.
func (m *MemoryGit) Events() []MemoryEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]MemoryEvent(nil), m.events...)
}

// EventsOfType filters Events.
func (m *MemoryGit) EventsOfType(t string) []MemoryEvent {
	var out []MemoryEvent
	for _, e := range m.Events() {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

func (m *MemoryGit) TenantByKey(_ context.Context, key string) (domain.TenantID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.tenants[key]; ok {
		return id, nil
	}
	return "", ErrNotFound
}

func (m *MemoryGit) UserIDForPrincipal(_ context.Context, issuer, subject string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.users[issuer+"\x00"+subject]; ok {
		return id, nil
	}
	return "", ErrNotFound
}

func (m *MemoryGit) Roles(_ context.Context, tenant domain.TenantID, userID, project string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var roles []string
	for _, x := range m.memberships {
		if x.tenant == tenant && x.user == userID && (x.project == "" || x.project == project) {
			roles = append(roles, x.role)
		}
	}
	sort.Strings(roles)
	return roles, nil
}

func (m *MemoryGit) GetProject(_ context.Context, tenant domain.TenantID, project domain.ProjectID) (Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.projects[tenant] {
		if p.ID == project {
			return p, nil
		}
	}
	return Project{}, ErrNotFound
}

func (m *MemoryGit) GetProjectByKey(_ context.Context, tenant domain.TenantID, key string) (Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.projects[tenant] {
		if p.Key == key {
			return p, nil
		}
	}
	return Project{}, ErrNotFound
}

func (m *MemoryGit) projectKey(tenant domain.TenantID, id domain.ProjectID) (string, bool) {
	for _, p := range m.projects[tenant] {
		if p.ID == id {
			return p.Key, true
		}
	}
	return "", false
}

func (m *MemoryGit) repo(tenant domain.TenantID, id string) *Repository {
	for _, r := range m.repos[tenant] {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (m *MemoryGit) CreateRepository(_ context.Context, r Repository) (Repository, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.ProjectID != "" {
		if _, ok := m.projectKey(r.TenantID, r.ProjectID); !ok {
			return Repository{}, false, ErrNotFound
		}
	} else if r.Name != "context" {
		return Repository{}, false, domain.ErrInvalidValue
	}
	for _, x := range m.repos[r.TenantID] {
		if x.ProjectID == r.ProjectID && x.Name == r.Name {
			return *x, false, nil
		}
	}
	now := m.now()
	r.Status, r.Reason, r.Empty, r.JournalAck, r.Version, r.CreatedAt, r.UpdatedAt = RepoProvisioning, "", true, 0, 1, now, now
	cp := r
	m.repos[r.TenantID] = append(m.repos[r.TenantID], &cp)
	return cp, true, nil
}

func (m *MemoryGit) GetRepository(_ context.Context, tenant domain.TenantID, id string) (Repository, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.repo(tenant, id); r != nil {
		return *r, nil
	}
	return Repository{}, ErrNotFound
}

func (m *MemoryGit) RepositoryByPath(_ context.Context, tenant domain.TenantID, projectKey, name string) (Repository, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var project domain.ProjectID
	if projectKey != "" {
		found := false
		for _, p := range m.projects[tenant] {
			if p.Key == projectKey {
				project, found = p.ID, true
			}
		}
		if !found {
			return Repository{}, ErrNotFound
		}
	}
	for _, r := range m.repos[tenant] {
		if r.ProjectID == project && r.Name == name {
			return *r, nil
		}
	}
	return Repository{}, ErrNotFound
}

func (m *MemoryGit) ListRepositories(_ context.Context, tenant domain.TenantID, project domain.ProjectID) ([]Repository, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Repository
	for _, r := range m.repos[tenant] {
		if r.ProjectID == project {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *MemoryGit) UpdateRepository(_ context.Context, tenant domain.TenantID, id string, expected domain.Version, patch RepositoryPatch) (Repository, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.repo(tenant, id)
	if r == nil {
		return Repository{}, ErrNotFound
	}
	if r.Version != expected {
		return Repository{}, ErrVersionConflict
	}
	if patch.DefaultBranch != nil && *patch.DefaultBranch != r.DefaultBranch {
		switch {
		case r.Status == RepoArchived:
			return Repository{}, ErrRepositoryArchived
		case r.Status != RepoReady:
			return Repository{}, ErrRepositoryNotReady
		case !r.Empty:
			return Repository{}, ErrRepositoryNotEmpty
		}
		now := m.now()
		for _, x := range m.creds[tenant] {
			if x.RepoID == id && x.Kind == CredSeed && x.ConsumedAt != nil && x.SettledAt == nil && x.ConsumedAt.After(now.Add(-seedPendingWindow)) {
				return Repository{}, ErrSeedPending
			}
		}
		for _, x := range m.creds[tenant] {
			if x.RepoID == id && x.Kind == CredSeed && x.ConsumedAt == nil && x.RevokedAt == nil {
				t := now
				x.RevokedAt = &t
			}
		}
		r.DefaultBranch = *patch.DefaultBranch
		r.Status, r.Reason = RepoProvisioning, ReasonReconfiguration
	}
	if patch.Policy != nil {
		r.Policy = *patch.Policy
	}
	r.Version++
	r.UpdatedAt = m.now()
	return *r, nil
}

func (m *MemoryGit) event(tenant domain.TenantID, project domain.ProjectID, id, actorType, actorID, typ, resType, resID string, payload []byte) bool {
	if id == "" {
		id = fmt.Sprintf("evt_mem_%d", len(m.events)+1)
	}
	for _, e := range m.events {
		if e.TenantID == tenant && e.ID == id {
			return false
		}
	}
	now := m.now()
	m.events = append(m.events, MemoryEvent{TenantID: tenant, ProjectID: project, ID: id, ActorType: actorType, ActorID: actorID,
		Type: typ, ResourceType: resType, ResourceID: resID, Payload: payload, CreatedAt: now, NextAttempt: now})
	return true
}

func (m *MemoryGit) SetRepositoryStatus(_ context.Context, tenant domain.TenantID, id, to, reason, actorType, actorID string) (Repository, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.repo(tenant, id)
	if r == nil {
		return Repository{}, ErrNotFound
	}
	if r.Status == to {
		return *r, nil
	}
	switch {
	case to == RepoArchived && r.Status == RepoReady, to == RepoReady && r.Status == RepoArchived:
	case r.Status == RepoProvisioning:
		return Repository{}, ErrRepositoryNotReady
	default:
		return Repository{}, ErrConflict
	}
	from := r.Status
	r.Status, r.Reason = to, reason
	r.Version++
	payload, _ := json.Marshal(map[string]any{"repositoryId": id, "from": from, "to": to, "reason": nullIfEmpty(reason)})
	m.event(tenant, r.ProjectID, "", actorType, actorID, "repository.state_changed", "repository", id, payload)
	return *r, nil
}

func (m *MemoryGit) ConfirmInitialized(_ context.Context, tenant domain.TenantID, id, defaultBranch string, empty bool) (Repository, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.repo(tenant, id)
	if r == nil {
		return Repository{}, ErrNotFound
	}
	if r.DefaultBranch != defaultBranch {
		return Repository{}, ErrConflict
	}
	if r.Status != RepoProvisioning {
		return *r, nil
	}
	reconfigured := r.Reason == ReasonReconfiguration
	r.Status, r.Reason, r.Empty = RepoReady, "", empty
	r.Version++
	if reconfigured {
		payload, _ := json.Marshal(map[string]any{"repositoryId": id, "from": RepoProvisioning, "to": RepoReady, "reason": ReasonReconfiguration})
		m.event(tenant, r.ProjectID, "", "system", "git-worker", "repository.state_changed", "repository", id, payload)
		return *r, nil
	}
	payload, _ := json.Marshal(map[string]any{"repositoryId": id, "name": r.Name, "defaultBranch": r.DefaultBranch, "hosting": "internal"})
	m.event(tenant, r.ProjectID, "", "system", "git-worker", "repository.created", "repository", id, payload)
	return *r, nil
}

func (m *MemoryGit) ProvisioningRepositories(_ context.Context, tenant domain.TenantID, limit int) ([]Repository, error) {
	return m.page(tenant, func(r *Repository) bool { return r.Status == RepoProvisioning }, "", limit), nil
}

func (m *MemoryGit) ReconcilableRepositories(_ context.Context, tenant domain.TenantID, afterID string, limit int) ([]Repository, error) {
	return m.page(tenant, func(r *Repository) bool { return r.Status == RepoReady || r.Status == RepoArchived }, afterID, limit), nil
}

func (m *MemoryGit) page(tenant domain.TenantID, keep func(*Repository) bool, afterID string, limit int) []Repository {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Repository
	for _, r := range m.repos[tenant] {
		if keep(r) && r.ID > afterID {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (m *MemoryGit) cred(tenant domain.TenantID, id string) *Credential {
	for _, c := range m.creds[tenant] {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func (m *MemoryGit) IssueCredential(_ context.Context, c Credential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.issueLocked(c)
}

func (m *MemoryGit) issueLocked(c Credential) error {
	if m.repo(c.TenantID, c.RepoID) == nil {
		return ErrNotFound
	}
	for _, x := range m.creds[c.TenantID] {
		if x.ID == c.ID || x.Digest == c.Digest {
			return ErrConflict
		}
	}
	cp := c
	cp.Version = 1
	m.creds[c.TenantID] = append(m.creds[c.TenantID], &cp)
	return nil
}

func (m *MemoryGit) IssueSeed(_ context.Context, c Credential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.repo(c.TenantID, c.RepoID)
	if r == nil {
		return ErrNotFound
	}
	switch {
	case r.Status == RepoArchived:
		return ErrRepositoryArchived
	case r.Status != RepoReady:
		return ErrRepositoryNotReady
	case !r.Empty:
		return ErrRepositoryNotEmpty
	}
	now := m.now()
	for _, x := range m.creds[c.TenantID] {
		if x.RepoID == c.RepoID && x.Kind == CredSeed {
			if x.ConsumedAt != nil && x.SettledAt == nil && x.ConsumedAt.After(now.Add(-seedPendingWindow)) {
				return ErrSeedPending
			}
			if x.ConsumedAt == nil && x.RevokedAt == nil {
				t := now
				x.RevokedAt = &t
			}
		}
	}
	return m.issueLocked(c)
}

func (m *MemoryGit) CredentialByDigest(_ context.Context, tenant domain.TenantID, digest string) (Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.creds[tenant] {
		if c.Digest == digest {
			return *c, nil
		}
	}
	return Credential{}, ErrNotFound
}

func (m *MemoryGit) GetCredential(_ context.Context, tenant domain.TenantID, id string) (Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.cred(tenant, id); c != nil {
		return *c, nil
	}
	return Credential{}, ErrNotFound
}

func (m *MemoryGit) AuthorizeSeed(_ context.Context, tenant domain.TenantID, credentialID, receiveID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.cred(tenant, credentialID)
	if c == nil || c.Kind != CredSeed {
		return ErrNotFound
	}
	if c.ConsumedAt != nil {
		if c.ConsumedReceive == receiveID {
			return nil
		}
		return ErrSeedConsumed
	}
	if c.RevokedAt != nil {
		return ErrSeedConsumed
	}
	now := m.now()
	for _, x := range m.creds[tenant] {
		if x.RepoID == c.RepoID && x.Kind == CredSeed && x.ID != c.ID && x.ConsumedAt != nil && x.SettledAt == nil &&
			x.ConsumedAt.After(now.Add(-seedPendingWindow)) {
			return ErrSeedConsumed
		}
	}
	c.ConsumedAt, c.ConsumedReceive = &now, receiveID
	c.Version++
	return nil
}

func (m *MemoryGit) RevokeRunCredentials(_ context.Context, tenant domain.TenantID, project domain.ProjectID, runID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for _, c := range m.creds[tenant] {
		if c.RunID == runID && c.ProjectID == project && c.RevokedAt == nil {
			t := now
			c.RevokedAt = &t
		}
	}
	return nil
}

func (m *MemoryGit) RunGitInfo(_ context.Context, tenant domain.TenantID, project domain.ProjectID, runID string) (RunGitInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.runs[string(tenant)+"/"+string(project)+"/"+runID]; ok {
		return r, nil
	}
	return RunGitInfo{}, ErrNotFound
}

func (m *MemoryGit) CommitReceive(_ context.Context, c ReceiveCommit) (ReceiveResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.repo(c.TenantID, c.RepositoryID)
	if r == nil {
		return ReceiveResult{}, ErrNotFound
	}
	res := ReceiveResult{}
	if len(eventUpdates(c.Updates)) > 0 {
		payload, err := GitRefsUpdatedPayload(c)
		if err != nil {
			return ReceiveResult{}, err
		}
		res.EventID = GitReceiveEventID(c.RepositoryID, c.ReceiveID)
		res.Created = m.event(c.TenantID, r.ProjectID, res.EventID, c.ActorType, c.ActorID, "git.refs_updated", "git_push", c.ReceiveID, payload)
	}
	applied := false
	for _, u := range c.Updates {
		if !isZeroOID(u.New) {
			applied = true
		}
	}
	if c.JournalSequence > r.JournalAck {
		r.JournalAck = c.JournalSequence
	}
	if applied {
		r.Empty = false
	}
	res.JournalAck = r.JournalAck
	now := m.now()
	for _, x := range m.creds[c.TenantID] {
		if x.RepoID == c.RepositoryID && x.ConsumedReceive == c.ReceiveID && x.SettledAt == nil {
			t := now
			x.SettledAt = &t
		}
	}
	return res, nil
}
