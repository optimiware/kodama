package entrypoint

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
)

// The v3 attempt (docs/contracts/runtime-v3.md, docs/implementation/step-entrypoint.md):
// a run_shared attempt over a source set of internally hosted repositories.
//
//  1. admission, context, begin and result use /runtime/v3/attempts, and a context
//     of another version is refused before anything runs;
//  2. no checkout: every source at its path under /workspace is verified in place
//     (present, a usable repository, the pinned commits) with sealed Git, and the
//     begin fence observes all of them;
//  3. after a successful command the entrypoint pushes explicitly with the
//     attempt's Git grant through a scoped credential helper (stepv3push.go) and
//     reports the commits with the push record each push was recorded under. There
//     is no source proposal, bundle or bundle artifact.

// NewStepV3 wires the orchestrator to the v3 wire: rt carries the shared v1
// bodies (renewal, secrets, artifacts), v3 the admission, context, begin and
// result. cfg selects the v3 wire; it is completed with defaults.
func NewStepV3(cfg Config, rt Runtime, v3 RuntimeV3, up Uploader) *Step {
	cfg.RuntimeAPI = RuntimeAPIv3
	s := NewStep(cfg, rt, up)
	s.v3 = v3
	if g, ok := v3.(GitGrants); ok {
		s.grants = g
	}
	return s
}

// projectV3 is the v1 shape of a ContextV3 for the code shared by every wire
// (outputs, secrets, command environment, artifacts). It carries no source: a
// source-set attempt never reads one from it.
func projectV3(c runtimev3.ContextV3) runtimev1.Context {
	return runtimev1.Context{
		TenantID: c.TenantID, ProjectID: c.ProjectID, RunID: c.RunID, StepID: c.StepID, AttemptID: c.AttemptID, ActionKey: c.ActionKey,
		Definition: c.Definition, WorkItem: c.WorkItem, DesignBaseline: c.DesignBaseline, Profile: c.Profile, Command: c.Command,
		Workspace: runtimev1.Workspace{Access: c.Workspace.Access}, Inputs: c.Inputs, Outputs: c.Outputs, Secrets: c.Secrets,
		Environment: c.Environment, Limits: c.Limits,
	}
}

// sourceRepo is one verified source: the context's entry, its directory under the
// run workspace and the sealed Git that reads it (never the repository's own
// configuration, hooks, filters or helpers).
type sourceRepo struct {
	src runtimev3.SourceV3
	dir string
	sg  *sealedGit
	// next is the commit the checkout is left detached at when the attempt ends
	// (restoreHeads): the source's headCommit as the context states it, or the
	// commit the attempt itself published to the source's write branch. Never a
	// commit published on an agent branch. Empty for a directory that is not a
	// source of the set (a seed directory).
	next string
}

// sourceDir is the directory of a source path under the run workspace; the path
// "." is the workspace itself.
func (c Config) sourceDir(path string) string {
	if path == "." {
		return c.WorkspaceDir
	}
	return filepath.Join(c.WorkspaceDir, filepath.FromSlash(path))
}

// checkSourceDir requires every directory from the workspace to the source to be a
// real directory: a symlink an earlier step planted would send Git elsewhere.
func checkSourceDir(root, rel string) error {
	dir := root
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return errors.New("is not a directory")
	}
	if rel == "." {
		return nil
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "" {
			continue
		}
		dir = filepath.Join(dir, part)
		fi, err := os.Lstat(dir)
		switch {
		case err != nil:
			return errors.New("is not present")
		case fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir():
			return errors.New("is not a plain directory")
		}
	}
	return nil
}

// verifySources is the only thing a v3 attempt does to /workspace before its
// command: look. A source-free workspace must be present and writable; every source
// must be present at its path, a usable repository holding its pinned commit and
// its head, and the begin fence reports each head and its cleanliness. No clone,
// checkout, reset or clean; which volume it is, is the controller's UID binding.
func (a *attempt) verifySources() (*runtimev3.BeginRequestV3, *verdict) {
	cfg, c3 := a.s.cfg, a.c3
	unverified := func(format string, args ...any) (*runtimev3.BeginRequestV3, *verdict) {
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
	if len(c3.Workspace.Sources) == 0 {
		probe, err := os.CreateTemp(cfg.WorkspaceDir, ".kodama-probe-*")
		if err != nil {
			return unverified("the run workspace is not writable: %v", scrub(err))
		}
		name := probe.Name()
		_ = probe.Close()
		if err := os.Remove(name); err != nil {
			return unverified("the run workspace probe could not be removed: %v", scrub(err))
		}
		return &runtimev3.BeginRequestV3{Sources: []runtimev3.BeginSource{}}, nil
	}
	ctx := a.runCtx
	req := &runtimev3.BeginRequestV3{Sources: make([]runtimev3.BeginSource, 0, len(c3.Workspace.Sources))}
	for _, src := range c3.Workspace.Sources {
		if err := checkSourceDir(cfg.WorkspaceDir, src.Path); err != nil {
			return unverified("the source %s %v", src.Path, err)
		}
		dir := cfg.sourceDir(src.Path)
		sg, err := newSealedGit(ctx, a.s.git, dir, cfg.ScratchDir, nil)
		if err != nil {
			return unverified("the repository of source %s cannot be used: %v", src.Path, err)
		}
		a.srcs = append(a.srcs, &sourceRepo{src: src, dir: dir, sg: sg, next: string(src.HeadCommit)})
		for _, c := range []string{src.Commit, string(src.HeadCommit)} {
			if !sg.hasCommit(ctx, c) {
				return unverified("the commit %s of source %s is not in the workspace", c, src.Path)
			}
		}
		st, err := sg.state(ctx)
		if err != nil {
			return unverified("the repository of source %s cannot be inspected: %v", src.Path, err)
		}
		if st.Head != string(src.HeadCommit) {
			// A checkout is handed over detached at the commit the context states
			// (restoreHeads); any other HEAD is a leak of an earlier attempt, never a
			// base. Nothing was changed: the refusal is classified and the command
			// does not start.
			return unverified("the HEAD %s of source %s is not the commit %s the context starts it from", st.Head, src.Path, src.HeadCommit)
		}
		req.Sources = append(req.Sources, runtimev3.BeginSource{
			Path: src.Path, Head: runtimev3.GitCommit(st.Head), TrackedClean: st.TrackedClean, UntrackedPresent: st.UntrackedPresent, IgnoredPresent: st.IgnoredPresent,
		})
	}
	return req, nil
}

// restoreHeads leaves every source checkout detached at the commit the next attempt
// starts from, whatever this attempt did (a published, declined, failed, cancelled
// or timed-out step alike): the source's headCommit as the context states it, or
// the commit this attempt itself published to the source's write branch. A command
// (or the agent wrapper) that committed on a run-scoped agent branch leaves HEAD
// beyond that; the next step, a writer in particular, would commit on top of it
// and publish the agent branch's commits to the task branch. The work tree is never
// reset (a run's scratch is cumulative): only HEAD and the checkout's own index
// move, so what the command left in the tree is reported by the next begin as the
// modified state it is. A failure is noted; the next attempt then refuses the
// workspace in verifySources instead of building on it.
//
// It does nothing before the command started: no source has been touched by the
// attempt then, and a refused checkout is left exactly as found.
func (a *attempt) restoreHeads() {
	if !a.launched || len(a.srcs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(a.parent), max(a.s.cfg.InterruptTimeout, a.s.cfg.FinalizeTimeout))
	defer cancel()
	for _, sr := range a.srcs {
		if sr.next == "" {
			continue
		}
		if err := sr.sg.detach(ctx, sr.next); err != nil {
			a.note("the HEAD of source %s could not be restored to %s: %v", sr.src.Path, sr.next, scrub(err))
			a.s.log.Warn("source head not restored", "source", sr.src.Path, "error", redactErr(err))
		}
	}
}

// recordBeginV3 makes the begin call over every source and checks the
// acknowledgment is the derivation of the request; see recordBegin.
func (a *attempt) recordBeginV3(req runtimev3.BeginRequestV3) *abortError {
	ack, err := a.s.v3.BeginV3(a.runCtx, req)
	if err != nil {
		if isRevoked(err) || a.authorityLost() {
			return &abortError{ExitRevoked, err}
		}
		return &abortError{ExitAdmission, fmt.Errorf("the command-begin was not acknowledged: %w", err)}
	}
	if err := runtimev3.CheckBeginResponse(*a.c3, req, ack); err != nil {
		return &abortError{ExitAdmission, fmt.Errorf("the begin acknowledgment is not the derivation of the request: %w", err)}
	}
	if a.authorityLost() {
		return &abortError{ExitRevoked, errors.New("authority lost while the begin was acknowledged")}
	}
	a.s.log.Info("command begin acknowledged", "workspaceState", string(ack.WorkspaceState), "ignoredPresent", ack.IgnoredPresent, "sources", len(req.Sources))
	return nil
}

// gitCredentialFiles lists the credential files the context advertises (the
// attempt's own grant first), mapped onto the configured Git directory.
func (a *attempt) gitCredentialFiles() []string {
	g := a.c3.Git
	if g == nil {
		return nil
	}
	var declared []string
	if g.CredentialFile != nil {
		declared = append(declared, *g.CredentialFile)
	}
	for _, rg := range []*runtimev3.GitRuntimeGrant{g.Runtime.Seed, g.Runtime.Integration} {
		if rg != nil {
			declared = append(declared, rg.CredentialFile)
		}
	}
	var out []string
	for _, d := range declared {
		if p, err := a.s.cfg.gitFile(d); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// registerGitCredential adds a Git credential value, and every representation of
// it that could reach a log or a header, to the disclosure set: the redaction of
// logs, notes and the live container-log mirror, and the scan of everything
// pushed. It goes through the step's issued-value registry (which bumps the
// generation the mirror follows), so it is safe to call from the grant broker's
// goroutine and takes effect before the next byte of output. Values are only ever
// added: an earlier credential stays registered when a later one replaces it.
func (a *attempt) registerGitCredential(v string) {
	if v == "" {
		return
	}
	a.credMu.Lock()
	seen := a.gitCreds[v]
	if !seen {
		if a.gitCreds == nil {
			a.gitCreds = map[string]bool{}
		}
		a.gitCreds[v] = true
	}
	a.credMu.Unlock()
	if seen {
		return
	}
	pair := GitUsername + ":" + v
	basic := base64.StdEncoding.EncodeToString([]byte(pair))
	for _, form := range []string{v, pair, basic, "Authorization: Basic " + basic} {
		a.s.remember(form)
	}
	a.credMu.Lock()
	a.gitForms = append(a.gitForms, v, pair, basic, "Authorization: Basic "+basic)
	a.credMu.Unlock()
}

// loadGitCredentials registers every Git credential currently on disk (the
// attempt's own grant). Runtime-issued credentials are registered by the grant
// broker before they are written. An absent or unreadable file is not an error
// here: a push that needs it fails explicitly.
func (a *attempt) loadGitCredentials() {
	if a.c3 == nil {
		return
	}
	for _, p := range a.gitCredentialFiles() {
		if v, err := readCredentialFile(p); err == nil {
			a.registerGitCredential(v)
		}
	}
}

// gitRedactors lists every representation of the loaded credentials, for the
// error text of a Git invocation.
func (a *attempt) gitRedactors() []string {
	a.credMu.Lock()
	defer a.credMu.Unlock()
	return append([]string(nil), a.gitForms...)
}
