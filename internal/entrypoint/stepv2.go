package entrypoint

import (
	"context"
	"errors"
	"fmt"
	"os"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
)

// The run_shared (v2) attempt (docs/implementation/step-entrypoint.md, "run_shared
// (v2) sequence"). The step is the same orchestration as v1 with four differences,
// all on the path before the command and around publication:
//
//  1. admission, context and the new begin fence use /runtime/v2/attempts, and a
//     context of the other version is refused before anything runs;
//  2. there is no checkout: no clone, reset or clean, no checkout credential, and
//     the entrypoint only verifies the controller-prepared /workspace;
//  3. the command starts only after the server acknowledged begin;
//  4. every Git operation over the shared scratch is sealed (sealedgit.go).

// ReasonWorkspaceUnverified is the pre-command verification failure of the shared
// scratch (lost mount, unreadable or unsafe repository, missing prepared commit);
// execution treats it as workspace_lost.
const ReasonWorkspaceUnverified = "workspace_unverified"

// NewStepV2 wires the orchestrator to the run_shared wire: rt carries the shared
// v1 bodies (renewal, secrets, artifacts, source, result), v2 the admission,
// context and begin. cfg selects the v2 wire; it is completed with defaults.
func NewStepV2(cfg Config, rt Runtime, v2 RuntimeV2, up Uploader) *Step {
	cfg.RuntimeAPI = RuntimeAPIv2
	s := NewStep(cfg, rt, up)
	s.v2 = v2
	return s
}

// admission is what a successful exchange returned, on either wire.
type admission struct {
	token runtimev1.TokenInfo
	fetched
}

// fetched is an attempt's context on either wire. c is the v1 shape the shared
// output, secret and source code reads: for a run_shared attempt it is the
// projection of c2 (the pinned input commit of the source is the run's cumulative
// head, which is exactly what the publisher's ancestry and bundle base need).
type fetched struct {
	c  runtimev1.Context
	c2 *runtimev2.ContextV2
	c3 *runtimev3.ContextV3
}

func (f fetched) validate() error {
	if f.c3 != nil {
		// The v1 projection of a source-set context has no source and is not a valid
		// v1 context; the v3 contract's validator covers every shared field.
		return f.c3.Validate()
	}
	if f.c2 == nil {
		return f.c.Validate()
	}
	if err := f.c2.Validate(); err != nil {
		return err
	}
	return f.c.Validate()
}

func (s *Step) admit(ctx context.Context, req runtimev1.AdmitRequest) (admission, error) {
	if s.v3 != nil {
		r, err := s.v3.AdmitV3(ctx, req)
		if err != nil {
			return admission{}, err
		}
		c3 := r.Context
		return admission{token: r.Token, fetched: fetched{c: projectV3(c3), c3: &c3}}, nil
	}
	if s.v2 == nil {
		r, err := s.rt.Admit(ctx, req)
		if err != nil {
			return admission{}, err
		}
		return admission{token: r.Token, fetched: fetched{c: r.Context}}, nil
	}
	r, err := s.v2.AdmitV2(ctx, req)
	if err != nil {
		return admission{}, err
	}
	// A response that is not a ContextV2 never decodes here (the contract's version
	// and run model are validated), so a v1 context on the v2 wire stops the step
	// before any command.
	c2 := r.Context
	return admission{token: r.Token, fetched: fetched{c: projectV2(c2), c2: &c2}}, nil
}

func (s *Step) fetch(ctx context.Context) (fetched, error) {
	if s.v3 != nil {
		c3, err := s.v3.ContextV3(ctx)
		if err != nil {
			return fetched{}, err
		}
		return fetched{c: projectV3(c3), c3: &c3}, nil
	}
	if s.v2 == nil {
		c, err := s.rt.Context(ctx)
		return fetched{c: c}, err
	}
	c2, err := s.v2.ContextV2(ctx)
	if err != nil {
		return fetched{}, err
	}
	return fetched{c: projectV2(c2), c2: &c2}, nil
}

// projectV2 is the v1 shape of a ContextV2 for the code shared by both wires. The
// source's pinned commit is the run's cumulative head (inputCommit before any
// writer published): that is the base a later writer's bundle and ancestry check
// are made against, with the expected tip observed at dispatch.
func projectV2(c runtimev2.ContextV2) runtimev1.Context {
	out := runtimev1.Context{
		TenantID: c.TenantID, ProjectID: c.ProjectID, RunID: c.RunID, StepID: c.StepID, AttemptID: c.AttemptID, ActionKey: c.ActionKey,
		Definition: c.Definition, WorkItem: c.WorkItem, DesignBaseline: c.DesignBaseline, Profile: c.Profile, Command: c.Command,
		Workspace: runtimev1.Workspace{Access: c.Workspace.Access}, Inputs: c.Inputs, Outputs: c.Outputs, Secrets: c.Secrets,
		Environment: c.Environment, Limits: c.Limits,
	}
	if src := c.Workspace.Source; src != nil {
		out.Workspace.Source = &runtimev1.Source{RepositoryURL: src.RepositoryURL, Commit: src.HeadCommit, Branch: src.Branch, ExpectedTip: src.ExpectedTip}
	}
	return out
}

// workGit is the Git the shared source code uses: the plain checkout's Git on the
// v1 wire, the sealed Git on the v2 wire.
type workGit interface {
	head(ctx context.Context, dir string) (string, error)
	dirty(ctx context.Context, dir string) (bool, error)
	commitAll(ctx context.Context, dir, message string) (string, error)
	isAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error)
	bundle(ctx context.Context, dir, base, commit, out string) error
	scanForSecrets(ctx context.Context, dir, base, commit string, needles [][]byte) (string, error)
}

func (a *attempt) work() workGit {
	if a.sealed != nil {
		return a.sealed
	}
	return a.s.git
}

// verifyWorkspace is the only thing a run_shared attempt does to /workspace before
// its command: look. A source-free scratch must be present and writable (it is
// empty only at the run's first attempt, and is never cleared or recreated here);
// a source-backed one must be a usable repository holding the pinned commits, and
// its head and cleanliness are what the begin fence reports. Which volume it is,
// is the controller's UID binding; nothing found inside the volume is identity.
func (a *attempt) verifyWorkspace() (*runtimev2.BeginRequest, *verdict) {
	cfg, c2 := a.s.cfg, a.c2
	unverified := func(format string, args ...any) (*runtimev2.BeginRequest, *verdict) {
		a.note(format, args...)
		v := failed(ReasonWorkspaceUnverified, nil)
		return nil, &v
	}
	fi, err := os.Stat(cfg.WorkspaceDir)
	switch {
	case err != nil:
		return unverified("the run workspace %s is not available: %v", cfg.WorkspaceDir, scrub(err))
	case !fi.IsDir():
		return unverified("the run workspace %s is not a directory", cfg.WorkspaceDir)
	}
	src := c2.Workspace.Source
	if src == nil {
		// Source-free: present and writable. A probe file proves the mount accepts
		// writes; it is removed at once and the scratch's contents are never read.
		probe, err := os.CreateTemp(cfg.WorkspaceDir, ".kodama-probe-*")
		if err != nil {
			return unverified("the run workspace is not writable: %v", scrub(err))
		}
		name := probe.Name()
		_ = probe.Close()
		if err := os.Remove(name); err != nil {
			return unverified("the run workspace probe could not be removed: %v", scrub(err))
		}
		return &runtimev2.BeginRequest{Workspace: nil}, nil
	}
	ctx := a.runCtx
	sg, err := newSealedGit(ctx, a.s.git, cfg.WorkspaceDir, cfg.ScratchDir, nil)
	if err != nil {
		return unverified("the run workspace repository cannot be used: %v", err)
	}
	a.sealed = sg
	for _, c := range []runtimev2.GitCommit{src.InputCommit, src.HeadCommit} {
		if !sg.hasCommit(ctx, string(c)) {
			return unverified("the run's commit %s is not in the workspace", c)
		}
	}
	st, err := sg.state(ctx)
	if err != nil {
		return unverified("the run workspace cannot be inspected: %v", err)
	}
	return &runtimev2.BeginRequest{Workspace: &runtimev2.BeginWorkspace{
		Head: runtimev2.GitCommit(st.Head), TrackedClean: st.TrackedClean, UntrackedPresent: st.UntrackedPresent, IgnoredPresent: st.IgnoredPresent,
	}}, nil
}

// recordBegin makes the begin call and checks the acknowledgment is the derivation
// of the request. It returns nil only for a durable acknowledgment. A revoked
// attempt aborts as revoked; any other failure, including an unacknowledged call,
// aborts without a result: the server may have recorded the begin, which the
// controller then treats as begun (never replayed), and the command never started.
func (a *attempt) recordBegin(req runtimev2.BeginRequest) *abortError {
	ack, err := a.s.v2.Begin(a.runCtx, req)
	if err != nil {
		if isRevoked(err) || a.authorityLost() {
			return &abortError{ExitRevoked, err}
		}
		return &abortError{ExitAdmission, fmt.Errorf("the command-begin was not acknowledged: %w", err)}
	}
	if err := runtimev2.CheckBeginResponse(*a.c2, req, ack); err != nil {
		return &abortError{ExitAdmission, fmt.Errorf("the begin acknowledgment is not the derivation of the request: %w", err)}
	}
	if a.authorityLost() {
		return &abortError{ExitRevoked, errors.New("authority lost while the begin was acknowledged")}
	}
	a.s.log.Info("command begin acknowledged", "workspaceState", string(ack.WorkspaceState), "ignoredPresent", ack.IgnoredPresent)
	return nil
}
