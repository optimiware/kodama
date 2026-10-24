// Package pipelinetest holds test doubles and document builders shared by the
// pipeline module's own tests and by composition tests. It contains no
// production behavior: the in-memory repository mirrors the content-addressed
// semantics of the PostgreSQL adapter, and the authority is a table.
package pipelinetest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

// Digest1 is a placeholder image digest.
const Digest1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// Image is a digest-pinned placeholder image reference.
const Image = "registry.example.invalid/go-tools@" + Digest1

// Container builds one flow-style container step.
func Container(id string, extra ...string) string {
	s := "{id: " + id + ", kind: container, image: " + Image + ", command: [/bin/sh, -ec], timeoutSeconds: 60"
	if len(extra) > 0 {
		s += ", " + strings.Join(extra, ", ")
	}
	return s + "}"
}

// Doc assembles a YAML document named "sample" from step flow mappings.
func Doc(steps ...string) string { return DocNamed("sample", "{}", steps...) }

// DocNamed assembles a YAML document with a name and an inputs mapping.
func DocNamed(name, inputs string, steps ...string) string {
	var b strings.Builder
	b.WriteString("apiVersion: kodama/v1alpha1\nkind: Pipeline\nmetadata: {name: " + name + "}\nspec:\n  inputs: " + inputs + "\n  steps:\n")
	for _, s := range steps {
		b.WriteString("    - " + s + "\n")
	}
	return b.String()
}

// Profiles is a ProfileLookup over a table: name -> immutable.
type Profiles map[string]bool

// LookupProfile implements pipeline.ProfileLookup.
func (p Profiles) LookupProfile(_ context.Context, _ domain.Scope, name string) (pipeline.ProfileInfo, error) {
	immutable, ok := p[name]
	if !ok {
		return pipeline.ProfileInfo{}, pipeline.ErrReferenceNotFound
	}
	return pipeline.ProfileInfo{Immutable: immutable}, nil
}

// References is a ReferencePolicy over tables: secret name -> env use permitted.
type References struct {
	Secrets      map[string]bool
	Environments map[string]bool
}

// CheckSecret implements pipeline.ReferencePolicy.
func (r References) CheckSecret(_ context.Context, _ domain.Scope, _ pipeline.Actor, name string, use pipeline.SecretUse) error {
	envOK, ok := r.Secrets[name]
	if !ok {
		return pipeline.ErrReferenceNotFound
	}
	if use == pipeline.SecretUseEnv && !envOK {
		return pipeline.ErrReferenceDenied
	}
	return nil
}

// CheckEnvironment implements pipeline.ReferencePolicy.
func (r References) CheckEnvironment(_ context.Context, _ domain.Scope, _ pipeline.Actor, id domain.EnvironmentID) error {
	if !r.Environments[string(id)] {
		return pipeline.ErrReferenceNotFound
	}
	return nil
}

// NewValidator builds a Validator enabling the given kinds (container only when
// none is given) with immutable profile "coding-v1" and permissive references.
func NewValidator(t testing.TB, kinds ...pipeline.StepKind) *pipeline.Validator {
	t.Helper()
	if len(kinds) == 0 {
		kinds = []pipeline.StepKind{pipeline.KindContainer}
	}
	v, err := pipeline.NewValidator(pipeline.Config{
		Capabilities: pipeline.EnabledKinds(kinds),
		Profiles:     Profiles{"coding-v1": true},
		References:   References{Secrets: map[string]bool{"token": true}, Environments: map[string]bool{"env_staging": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Authority is a table-driven pipeline.Authority keyed by scope and principal.
type Authority struct {
	mu     sync.Mutex
	actors map[key]pipeline.Actor
	grants map[key]map[pipeline.Permission]bool
	// Err, when set, makes every call fail like an outage.
	Err error
}

type key struct {
	scope     domain.Scope
	principal string
}

// NewAuthority returns an authority that knows no principal.
func NewAuthority() *Authority {
	return &Authority{actors: map[key]pipeline.Actor{}, grants: map[key]map[pipeline.Permission]bool{}}
}

// Grant makes the principal resolvable in the scope with exactly perms.
func (a *Authority) Grant(scope domain.Scope, principal string, typ pipeline.ActorType, perms ...pipeline.Permission) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := key{scope, principal}
	a.actors[k] = pipeline.Actor{ID: principal, Type: typ}
	a.grants[k] = map[pipeline.Permission]bool{}
	for _, p := range perms {
		a.grants[k][p] = true
	}
}

// Revoke removes one permission.
func (a *Authority) Revoke(scope domain.Scope, principal string, perm pipeline.Permission) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.grants[key{scope, principal}], perm)
}

// SetErr makes the authority fail (or recover with nil).
func (a *Authority) SetErr(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Err = err
}

// ResolveActor implements pipeline.Authority.
func (a *Authority) ResolveActor(_ context.Context, scope domain.Scope, principal string) (pipeline.Actor, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Err != nil {
		return pipeline.Actor{}, a.Err
	}
	act, ok := a.actors[key{scope, principal}]
	if !ok {
		return pipeline.Actor{}, pipeline.ErrHidden
	}
	return act, nil
}

// Authorize implements pipeline.Authority.
func (a *Authority) Authorize(_ context.Context, act pipeline.Actor, scope domain.Scope, perm pipeline.Permission) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Err != nil {
		return a.Err
	}
	if !a.grants[key{scope, act.ID}][perm] {
		return pipeline.ErrDenied
	}
	return nil
}

// MemRepo is an in-memory pipeline.Repository with the semantics of the
// PostgreSQL adapter: scoped digest deduplication and per-name versions.
type MemRepo struct {
	mu    sync.Mutex
	defs  []pipeline.Definition
	Calls int
	// Err, when set, is returned by RegisterOrGet.
	Err error
}

// Definitions returns a copy of the stored definitions.
func (m *MemRepo) Definitions() []pipeline.Definition {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]pipeline.Definition(nil), m.defs...)
}

// RegisterOrGet implements pipeline.Repository.
func (m *MemRepo) RegisterOrGet(_ context.Context, c pipeline.Candidate) (pipeline.Definition, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls++
	if m.Err != nil {
		return pipeline.Definition{}, false, m.Err
	}
	for _, d := range m.defs {
		if d.Scope == c.Scope && d.Digest == c.Content.Digest() {
			return d, false, nil
		}
	}
	version := domain.Version(1)
	for _, d := range m.defs {
		if d.Scope == c.Scope && d.Name == c.Content.Name() && d.Version >= version {
			version = d.Version + 1
		}
	}
	d, err := pipeline.NewDefinition(c, version)
	if err != nil {
		return pipeline.Definition{}, false, err
	}
	m.defs = append(m.defs, d)
	return d, true, nil
}

// Get implements pipeline.Repository.
func (m *MemRepo) Get(_ context.Context, s domain.Scope, id domain.DefinitionID) (pipeline.Definition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.defs {
		if d.Scope == s && d.ID == id {
			return d, nil
		}
	}
	return pipeline.Definition{}, fmt.Errorf("%w: the definition was not found", pipeline.ErrNotFound)
}

// List implements pipeline.Repository.
func (m *MemRepo) List(_ context.Context, s domain.Scope, q pipeline.ListQuery) ([]pipeline.Definition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []pipeline.Definition
	for _, d := range m.defs {
		if d.Scope != s || (q.Name != "" && d.Name != q.Name) {
			continue
		}
		if q.After != nil && !(d.CreatedAt.After(q.After.CreatedAt) || (d.CreatedAt.Equal(q.After.CreatedAt) && d.ID > q.After.ID)) {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}
