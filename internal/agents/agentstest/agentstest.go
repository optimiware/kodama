// Package agentstest holds test doubles and builders shared by the agents
// module's own tests and by composition tests. It contains no production
// behavior: the catalogue entries it builds are fixtures that never establish
// production support, the authority and approvals are tables, and the in-memory
// repository mirrors the content-addressed semantics of the PostgreSQL adapter.
//
// Resource prefix t030.
package agentstest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/domain"
)

// Prefix is the resource prefix of this task's test resources.
const Prefix = "t030"

// Fixture identifiers. The image is a placeholder digest, not a real build.
const (
	Digest1  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	Image    = "registry.example.invalid/t030-agent@" + Digest1
	Commit   = "3030303030303030303030303030303030303030"
	Endpoint = "https://gateway.example.invalid/v1"
)

// Command is the fixture wrapper entrypoint.
var Command = []string{"/usr/local/bin/t030-wrapper"}

// Baseline builds a valid, canonical design baseline.
func Baseline(t testing.TB) runtimev1.DesignBaseline {
	t.Helper()
	b, err := runtimev1.NewDesignBaseline(
		[]string{"AR-14", "AR-01"}, []string{"P-16", "C-16"},
		[]runtimev1.SourceRef{{Repository: "https://git.example.invalid/kodama.git", Revision: Commit, Path: "docs/specs/agent-runtime.md"}},
		[]string{"tenant-authority", "module-boundaries"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Draft builds a valid draft named name that claims every control.
func Draft(t testing.TB, name string) agents.Draft {
	t.Helper()
	req, tok, calls := int64(100), int64(200000), int64(50)
	return agents.Draft{
		Name:  name,
		Image: Image,
		Wrapper: agents.Wrapper{ID: "t030-wrapper", Version: "1.4.2", Command: slices.Clone(Command),
			Harness: agents.Harness{ID: "opencode", Version: "0.9.1"}},
		Instructions:   "# Role\nImplement the work item. Report conflicts instead of inventing architecture.",
		DesignBaseline: Baseline(t),
		Model:          agents.Model{Name: "t030-model", Endpoint: Endpoint},
		Tools:          []string{"repo.read", "repo.search"},
		Limits:         agents.Limits{WallSeconds: 1800, MaxRequests: &req, MaxTokens: &tok, MaxToolCalls: &calls},
		Transcript:     agents.Transcript{Record: true},
	}
}

// Content builds validated content or fails the test.
func Content(t testing.TB, d agents.Draft) agents.Content {
	t.Helper()
	c, err := agents.NewContent(d)
	if err != nil {
		t.Fatalf("content: %v", err)
	}
	return c
}

// Entry is the catalogue fixture that qualifies Draft's tuple with every
// control. It is test evidence only.
func Entry(d agents.Draft) agents.CatalogueEntry {
	return agents.CatalogueEntry{
		Tuple: agents.Tuple{Image: d.Image, WrapperID: d.Wrapper.ID, WrapperVersion: d.Wrapper.Version, Command: slices.Clone(d.Wrapper.Command),
			HarnessID: d.Wrapper.Harness.ID, HarnessVersion: d.Wrapper.Harness.Version},
		Controls: agents.Controls(),
		Evidence: "t030-fixture-smoke",
	}
}

// Catalogue qualifies the given drafts' tuples (none: an empty catalogue).
func Catalogue(t testing.TB, drafts ...agents.Draft) *agents.Catalogue {
	t.Helper()
	var es []agents.CatalogueEntry
	for _, d := range drafts {
		es = append(es, Entry(d))
	}
	c, err := agents.NewCatalogue(es...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Approvals is a table-driven agents.Approvals: everything is refused unless
// allowed.
type Approvals struct {
	mu     sync.Mutex
	models map[string]bool
	tools  map[string]bool
	// scoped holds tool approvals that apply only inside one project.
	scoped map[domain.Scope]map[string]bool
	// Err, when set, makes every call fail like an outage.
	Err error
}

// NewApprovals approves the fixture model and tools.
func NewApprovals() *Approvals {
	return &Approvals{models: map[string]bool{"t030-model": true}, tools: map[string]bool{"repo.read": true, "repo.search": true}}
}

// AllowToolsIn approves tools for exactly one project, in addition to the
// globally approved ones. Another project's identical request is refused.
func (a *Approvals) AllowToolsIn(scope domain.Scope, tools ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.scoped == nil {
		a.scoped = map[domain.Scope]map[string]bool{}
	}
	if a.scoped[scope] == nil {
		a.scoped[scope] = map[string]bool{}
	}
	for _, t := range tools {
		a.scoped[scope][t] = true
	}
}

// AllowModel approves one more model name.
func (a *Approvals) AllowModel(name string) {
	a.mu.Lock()
	a.models[name] = true
	a.mu.Unlock()
}

// WithdrawModel removes a model approval.
func (a *Approvals) WithdrawModel(name string) {
	a.mu.Lock()
	delete(a.models, name)
	a.mu.Unlock()
}

// ApproveModel implements agents.Approvals.
func (a *Approvals) ApproveModel(_ context.Context, _ domain.Scope, m agents.Model) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Err != nil {
		return a.Err
	}
	if !a.models[m.Name] || m.Endpoint != Endpoint {
		return agents.ErrDenied
	}
	return nil
}

// ApproveTools implements agents.Approvals.
func (a *Approvals) ApproveTools(_ context.Context, scope domain.Scope, tools []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Err != nil {
		return a.Err
	}
	for _, t := range tools {
		if !a.tools[t] && !a.scoped[scope][t] {
			return agents.ErrDenied
		}
	}
	return nil
}

// Authority is a table-driven agents.Authority keyed by scope and principal.
type Authority struct {
	mu     sync.Mutex
	actors map[key]agents.Actor
	grants map[key]map[agents.Permission]bool
	// Err, when set, makes every call fail like an outage.
	Err error
}

type key struct {
	scope     domain.Scope
	principal string
}

// NewAuthority returns an authority that knows no principal.
func NewAuthority() *Authority {
	return &Authority{actors: map[key]agents.Actor{}, grants: map[key]map[agents.Permission]bool{}}
}

// Grant makes the principal resolvable in the scope with exactly perms.
func (a *Authority) Grant(scope domain.Scope, principal string, typ agents.ActorType, perms ...agents.Permission) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := key{scope, principal}
	a.actors[k] = agents.Actor{ID: principal, Type: typ}
	a.grants[k] = map[agents.Permission]bool{}
	for _, p := range perms {
		a.grants[k][p] = true
	}
}

// ResolveActor implements agents.Authority.
func (a *Authority) ResolveActor(_ context.Context, scope domain.Scope, principal string) (agents.Actor, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Err != nil {
		return agents.Actor{}, a.Err
	}
	act, ok := a.actors[key{scope, principal}]
	if !ok {
		return agents.Actor{}, agents.ErrHidden
	}
	return act, nil
}

// Authorize implements agents.Authority.
func (a *Authority) Authorize(_ context.Context, act agents.Actor, scope domain.Scope, perm agents.Permission) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Err != nil {
		return a.Err
	}
	if !a.grants[key{scope, act.ID}][perm] {
		return agents.ErrDenied
	}
	return nil
}

// MemRepo is an in-memory agents.Repository with the semantics of the PostgreSQL
// adapter: scoped digest deduplication and per-name versions.
type MemRepo struct {
	mu       sync.Mutex
	versions []agents.ProfileVersion
	Calls    int
	// Reads counts Get and List calls.
	Reads int
	// Err, when set, is returned by RegisterOrGet.
	Err error
}

// Versions returns a copy of the stored versions.
func (m *MemRepo) Versions() []agents.ProfileVersion {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.versions)
}

// RegisterOrGet implements agents.Repository.
func (m *MemRepo) RegisterOrGet(_ context.Context, c agents.Candidate) (agents.ProfileVersion, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls++
	if m.Err != nil {
		return agents.ProfileVersion{}, false, m.Err
	}
	next := int64(1)
	for _, p := range m.versions {
		if p.Scope != c.Scope {
			continue
		}
		if p.Digest == c.Content.Digest() {
			return p, false, nil
		}
		if p.Name == c.Content.Name() && int64(p.Version) >= next {
			next = int64(p.Version) + 1
		}
	}
	p, err := agents.NewProfileVersion(c, domain.Version(next))
	if err != nil {
		return agents.ProfileVersion{}, false, err
	}
	m.versions = append(m.versions, p)
	return p, true, nil
}

// Get implements agents.Repository.
func (m *MemRepo) Get(_ context.Context, scope domain.Scope, name string, version domain.Version) (agents.ProfileVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Reads++
	for _, p := range m.versions {
		if p.Scope == scope && p.Name == name && p.Version == version {
			return p, nil
		}
	}
	return agents.ProfileVersion{}, fmt.Errorf("%w: the profile was not found", agents.ErrNotFound)
}

// List implements agents.Repository with (created_at, id) keyset paging.
func (m *MemRepo) List(_ context.Context, scope domain.Scope, q agents.ListQuery) ([]agents.ProfileVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Reads++
	var rows []agents.ProfileVersion
	for _, p := range m.versions {
		if p.Scope != scope || (q.Name != "" && p.Name != q.Name) {
			continue
		}
		if q.After != nil && !(p.CreatedAt.After(q.After.CreatedAt) || (p.CreatedAt.Equal(q.After.CreatedAt) && p.ID > q.After.ID)) {
			continue
		}
		rows = append(rows, p)
	}
	slices.SortFunc(rows, func(a, b agents.ProfileVersion) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})
	if len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	return rows, nil
}
