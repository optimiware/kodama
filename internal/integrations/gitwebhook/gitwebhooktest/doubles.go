package gitwebhooktest

import (
	"context"
	"errors"
	"sync"

	"github.com/optimiware/kodama/internal/domain"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/workspace"
)

// Authorizer grants explicit (principal, scope, action) tuples and denies the
// rest. Err, when set, simulates an outage.
type Authorizer struct {
	mu    sync.Mutex
	allow map[authKey]bool
	Err   error
	// Gate, when set, is called with the 1-based count of ActionRun checks
	// before each such check is evaluated, so a test can interleave callers.
	Gate func(call int)
	// After, when set, is called with the same count once that check has been
	// evaluated (its decision is fixed) and before it returns.
	After func(call int)
	calls int
}

type authKey struct {
	principal string
	scope     domain.Scope
	action    gw.Action
}

var _ gw.Authorizer = (*Authorizer)(nil)

// Grant allows the tuple.
func (a *Authorizer) Grant(principal string, scope domain.Scope, action gw.Action) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.allow == nil {
		a.allow = map[authKey]bool{}
	}
	a.allow[authKey{principal, scope, action}] = true
}

// Revoke removes the grant.
func (a *Authorizer) Revoke(principal string, scope domain.Scope, action gw.Action) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.allow, authKey{principal, scope, action})
}

// Authorize implements gitwebhook.Authorizer.
func (a *Authorizer) Authorize(_ context.Context, principal string, scope domain.Scope, action gw.Action) error {
	var n int
	var after func(int)
	if action == gw.ActionRun {
		a.mu.Lock()
		a.calls++
		var gate func(int)
		n, gate, after = a.calls, a.Gate, a.After
		a.mu.Unlock()
		if gate != nil {
			gate(n)
		}
	}
	err := a.decide(principal, scope, action)
	if after != nil {
		after(n)
	}
	return err
}

func (a *Authorizer) decide(principal string, scope domain.Scope, action gw.Action) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Err != nil {
		return a.Err
	}
	if !a.allow[authKey{principal, scope, action}] {
		return gw.ErrDenied
	}
	return nil
}

// Secrets is a scope-keyed secret source.
type Secrets struct {
	mu     sync.Mutex
	values map[secretKey][]byte
	Err    error
	Calls  []string
}

type secretKey struct {
	scope domain.Scope
	ref   string
}

var _ gw.Secrets = (*Secrets)(nil)

// Put stores a value visible only in scope.
func (s *Secrets) Put(scope domain.Scope, ref string, value []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = map[secretKey][]byte{}
	}
	s.values[secretKey{scope, ref}] = append([]byte(nil), value...)
}

// Delete revokes the value.
func (s *Secrets) Delete(scope domain.Scope, ref string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, secretKey{scope, ref})
}

// Resolve implements gitwebhook.Secrets and returns a private copy.
func (s *Secrets) Resolve(_ context.Context, scope domain.Scope, ref string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls = append(s.Calls, ref)
	if s.Err != nil {
		return nil, s.Err
	}
	v, ok := s.values[secretKey{scope, ref}]
	if !ok {
		return nil, gw.ErrSecretNotFound
	}
	return append([]byte(nil), v...), nil
}

// Definitions is a set of registered definitions per scope.
type Definitions struct {
	mu  sync.Mutex
	set map[defKey]bool
	Err error
}

type defKey struct {
	scope domain.Scope
	id    domain.DefinitionID
}

var _ gw.Definitions = (*Definitions)(nil)

// Register adds a definition to scope.
func (d *Definitions) Register(scope domain.Scope, id domain.DefinitionID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.set == nil {
		d.set = map[defKey]bool{}
	}
	d.set[defKey{scope, id}] = true
}

// Registered implements gitwebhook.Definitions.
func (d *Definitions) Registered(_ context.Context, scope domain.Scope, id domain.DefinitionID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Err != nil {
		return d.Err
	}
	if !d.set[defKey{scope, id}] {
		return gw.ErrNotFound
	}
	return nil
}

// Repositories maps a scope to its configured repository.
type Repositories struct {
	mu   sync.Mutex
	cfgs map[domain.Scope]workspace.RepositoryConfig
	Err  error
	// Gate, when set, is called with the 1-based lookup count before each
	// lookup is evaluated, so a test can park a caller.
	Gate  func(call int)
	calls int
}

var _ workspace.Repositories = (*Repositories)(nil)

// Set binds the scope's repository.
func (r *Repositories) Set(scope domain.Scope, cfg workspace.RepositoryConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cfgs == nil {
		r.cfgs = map[domain.Scope]workspace.RepositoryConfig{}
	}
	r.cfgs[scope] = cfg
}

// Repository implements workspace.Repositories.
func (r *Repositories) Repository(_ context.Context, scope domain.Scope) (workspace.RepositoryConfig, error) {
	r.mu.Lock()
	r.calls++
	n, gate := r.calls, r.Gate
	r.mu.Unlock()
	if gate != nil {
		gate(n)
	}
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

// Git is a controllable workspace.RefReader double. It records every call so
// tests can prove which credential references were offered.
type Git struct {
	mu       sync.Mutex
	branches map[string]workspace.BranchInfo
	Err      error
	Calls    []GitCall
}

// GitCall is one recorded BranchContains call.
type GitCall struct {
	Repo   workspace.RepoRef
	Branch string
	Commit workspace.Commit
}

var _ workspace.RefReader = (*Git)(nil)

// SetBranch fixes what BranchContains reports for ref.
func (g *Git) SetBranch(ref string, info workspace.BranchInfo) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.branches == nil {
		g.branches = map[string]workspace.BranchInfo{}
	}
	g.branches[ref] = info
}

// SetError makes every call fail (nil restores service).
func (g *Git) SetError(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Err = err
}

// CallCount returns the number of BranchContains calls.
func (g *Git) CallCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.Calls)
}

// ResolveRef implements workspace.RefReader; the trigger never needs it, so a
// call is a test failure signal returned as an error.
func (g *Git) ResolveRef(context.Context, workspace.RepoRef, string) (workspace.Commit, bool, error) {
	return "", false, errors.New("gitwebhooktest: ResolveRef must not be used: a branch tip is never a trigger commit")
}

// BranchContains implements workspace.RefReader and reports exactly what
// SetBranch configured for the branch (the zero BranchInfo when unset: the
// branch does not exist).
func (g *Git) BranchContains(_ context.Context, repo workspace.RepoRef, branch string, commit workspace.Commit) (workspace.BranchInfo, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Calls = append(g.Calls, GitCall{Repo: repo, Branch: branch, Commit: commit})
	if g.Err != nil {
		return workspace.BranchInfo{}, g.Err
	}
	return g.branches[branch], nil
}

// Runs is an idempotent run-admission double keyed by IdempotencyKey.
type Runs struct {
	mu       sync.Mutex
	byKey    map[string]gw.RunRef
	refused  map[string]string
	Requests []gw.RunRequest
	Err      error
	FindErr  error
	// DecideErr makes DecideRefusal fail (an outage).
	DecideErr error
	// LoseResponses makes that many StartRun calls create the run and then
	// report an outage (the response was lost).
	LoseResponses int
	// Gate, when set, is called with the 1-based StartRun count before the call
	// takes effect, so a test can interleave callers.
	Gate  func(call int)
	calls int
	next  int
}

var _ gw.Runs = (*Runs)(nil)

// StartRun implements gitwebhook.Runs.
func (r *Runs) StartRun(_ context.Context, req gw.RunRequest) (gw.RunRef, error) {
	r.mu.Lock()
	r.calls++
	n, gate := r.calls, r.Gate
	r.mu.Unlock()
	if gate != nil {
		gate(n)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Requests = append(r.Requests, req)
	if _, closed := r.refused[req.IdempotencyKey]; closed {
		return gw.RunRef{}, gw.ErrAdmissionClosed
	}
	if r.Err != nil {
		return gw.RunRef{}, r.Err
	}
	if r.byKey == nil {
		r.byKey = map[string]gw.RunRef{}
	}
	ref, ok := r.byKey[req.IdempotencyKey]
	if ok {
		ref.Created = false
	} else {
		r.next++
		ref = gw.RunRef{ID: domain.RunID("run-" + string(rune('a'+r.next-1))), Created: true}
		r.byKey[req.IdempotencyKey] = ref
	}
	if r.LoseResponses > 0 {
		r.LoseResponses--
		return gw.RunRef{}, errors.New("gitwebhooktest: response lost")
	}
	return ref, nil
}

// FindRun implements gitwebhook.Runs.
func (r *Runs) FindRun(_ context.Context, _ domain.Scope, key string) (gw.RunRef, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.FindErr != nil {
		return gw.RunRef{}, false, r.FindErr
	}
	ref, ok := r.byKey[key]
	ref.Created = false
	return ref, ok, nil
}

// DecideRefusal implements gitwebhook.Runs: atomic with StartRun.
func (r *Runs) DecideRefusal(_ context.Context, _ domain.Scope, key, reason string) (gw.Decision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.DecideErr != nil {
		return gw.Decision{}, r.DecideErr
	}
	if ref, ok := r.byKey[key]; ok {
		ref.Created = false
		return gw.Decision{Found: true, Run: ref}, nil
	}
	if r.refused == nil {
		r.refused = map[string]string{}
	}
	if prior, ok := r.refused[key]; ok {
		return gw.Decision{Reason: prior}, nil
	}
	r.refused[key] = reason
	return gw.Decision{Reason: reason}, nil
}

// Created returns the number of distinct runs created.
func (r *Runs) Created() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byKey)
}

// Started returns the number of StartRun calls that reached admission.
func (r *Runs) Started() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.Requests)
}

// SetError makes StartRun fail (nil restores service).
func (r *Runs) SetError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Err = err
}
