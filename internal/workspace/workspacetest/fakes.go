// Package workspacetest holds the shared fixtures of the workspace module's
// tests: an isolated migrated PostgreSQL database, a real HTTPS Git server with
// distinct read and write tokens, the real adapters wired into a Service, and
// explicit in-memory implementations of the peer-module ports (work items,
// repository configuration, attempt authority, bundle artifacts, secrets).
// Missing peer adapters fail closed in production; here each is explicit so a
// test states exactly which authority it grants.
package workspacetest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/workspace"
)

// Authz grants actions to listed actors inside listed scopes and denies
// everything else (deny by default). A scope the actor has no grant for is
// reported as not found, as the hosted authorizers do for hidden references.
type Authz struct {
	mu     sync.Mutex
	grants map[string]map[domain.Scope]bool
	Calls  int
}

func NewAuthz() *Authz { return &Authz{grants: map[string]map[domain.Scope]bool{}} }

// Grant lets actor perform every action in scope.
func (a *Authz) Grant(actor string, scope domain.Scope) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.grants[actor] == nil {
		a.grants[actor] = map[domain.Scope]bool{}
	}
	a.grants[actor][scope] = true
}

// Revoke removes the actor's grants.
func (a *Authz) Revoke(actor string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.grants, actor)
}

func (a *Authz) Authorize(_ context.Context, actor workspace.Actor, _ workspace.Action, scope domain.Scope, _ workspace.Resource) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Calls++
	if a.grants[actor.ID][scope] {
		return nil
	}
	if len(a.grants[actor.ID]) > 0 {
		return workspace.ErrNotFound
	}
	return workspace.ErrDenied
}

// Items resolves work items per scope.
type Items struct {
	mu    sync.Mutex
	items map[domain.Scope]map[domain.WorkItemID]string
}

func NewItems() *Items { return &Items{items: map[domain.Scope]map[domain.WorkItemID]string{}} }

// Add registers a work item of the given kind ("story", "task", "defect", "epic").
func (i *Items) Add(scope domain.Scope, id domain.WorkItemID, kind string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.items[scope] == nil {
		i.items[scope] = map[domain.WorkItemID]string{}
	}
	i.items[scope][id] = kind
}

func (i *Items) Actionable(_ context.Context, scope domain.Scope, id domain.WorkItemID) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	kind, ok := i.items[scope][id]
	switch {
	case !ok:
		return workspace.ErrNotFound
	case kind == "epic":
		return workspace.ErrNotActionable
	}
	return nil
}

// Repos returns one repository configuration per scope.
type Repos struct {
	mu   sync.Mutex
	cfgs map[domain.Scope]workspace.RepositoryConfig
	Err  error
}

func NewRepos() *Repos { return &Repos{cfgs: map[domain.Scope]workspace.RepositoryConfig{}} }

func (r *Repos) Set(scope domain.Scope, cfg workspace.RepositoryConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfgs[scope] = cfg
}

func (r *Repos) Repository(_ context.Context, scope domain.Scope) (workspace.RepositoryConfig, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Err != nil {
		return workspace.RepositoryConfig{}, r.Err
	}
	cfg, ok := r.cfgs[scope]
	if !ok {
		return workspace.RepositoryConfig{}, workspace.ErrNotFound
	}
	return cfg, nil
}

// Attempts is the attempt-authority port.
type Attempts struct {
	mu sync.Mutex
	m  map[domain.AttemptID]attempt
}

type attempt struct {
	scope domain.Scope
	auth  workspace.WriterAuthority
}

func NewAttempts() *Attempts { return &Attempts{m: map[domain.AttemptID]attempt{}} }

// Register creates a live attempt with the given access.
func (a *Attempts) Register(scope domain.Scope, id domain.AttemptID, ws workspace.ID, access workspace.Access) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.m[id] = attempt{scope: scope, auth: workspace.WriterAuthority{WorkspaceID: ws, Access: access, Live: true}}
}

// Revoke ends the attempt's authority (cancelled, superseded or partitioned).
func (a *Attempts) Revoke(id domain.AttemptID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	x := a.m[id]
	x.auth.Live = false
	a.m[id] = x
}

func (a *Attempts) Authority(_ context.Context, scope domain.Scope, id domain.AttemptID) (workspace.WriterAuthority, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	x, ok := a.m[id]
	if !ok || x.scope != scope {
		return workspace.WriterAuthority{}, workspace.ErrNotFound
	}
	return x.auth, nil
}

// Bundles holds bundle artifacts.
type Bundles struct {
	mu         sync.Mutex
	m          map[domain.ArtifactID]stored
	SettledIDs []domain.ArtifactID
	// OpenErr, when set, makes Open fail (artifact store outage).
	OpenErr error
}

type stored struct {
	b    workspace.Bundle
	data []byte
}

func NewBundles() *Bundles { return &Bundles{m: map[domain.ArtifactID]stored{}} }

// Digest returns the artifact digest of data.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Add stores a completed bundle artifact produced by attempt.
func (b *Bundles) Add(scope domain.Scope, id domain.ArtifactID, att domain.AttemptID, data []byte) workspace.Bundle {
	return b.AddWith(workspace.Bundle{ID: id, Scope: scope, AttemptID: att, Role: "bundle", Complete: true, Digest: Digest(data), Size: int64(len(data))}, data)
}

// AddWith stores an artifact with explicit (possibly wrong) metadata.
func (b *Bundles) AddWith(meta workspace.Bundle, data []byte) workspace.Bundle {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.m[meta.ID] = stored{b: meta, data: data}
	return meta
}

func (b *Bundles) Open(_ context.Context, scope domain.Scope, id domain.ArtifactID) (workspace.Bundle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.OpenErr != nil {
		return workspace.Bundle{}, b.OpenErr
	}
	s, ok := b.m[id]
	if !ok || s.b.Scope != scope {
		return workspace.Bundle{}, workspace.ErrNotFound
	}
	out := s.b
	out.Body = io.NopCloser(bytes.NewReader(s.data))
	return out, nil
}

func (b *Bundles) Settled(_ context.Context, _ domain.Scope, id domain.ArtifactID) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.SettledIDs = append(b.SettledIDs, id)
	return nil
}

// Secrets returns injected secret values per attempt.
type Secrets struct {
	mu sync.Mutex
	m  map[domain.AttemptID][][]byte
}

func NewSecrets() *Secrets { return &Secrets{m: map[domain.AttemptID][][]byte{}} }

func (s *Secrets) Set(id domain.AttemptID, needles ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range needles {
		s.m[id] = append(s.m[id], []byte(n))
	}
}

func (s *Secrets) Needles(_ context.Context, _ domain.Scope, id domain.AttemptID) ([][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[id], nil
}

// Policy is the deletion policy; the zero value denies.
type Policy struct {
	mu    sync.Mutex
	Allow bool
	Err   error
}

func (p *Policy) AllowDeletion(context.Context, domain.Scope, *workspace.RetainedRef) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Allow, p.Err
}

// Rotation verifies publisher credential rotation evidence.
type Rotation struct {
	mu   sync.Mutex
	Err  error
	Seen []string
}

func (r *Rotation) VerifyRotated(_ context.Context, _ domain.Scope, e workspace.RotationEvidence) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Seen = append(r.Seen, e.RotationID)
	return r.Err
}

// Clock is a settable clock.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

func NewClock() *Clock { return &Clock{now: time.Now().UTC()} }

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

var errInjected = errors.New("injected failure")

// ErrInjected is returned by fault-injecting wrappers.
func ErrInjected() error { return errInjected }
