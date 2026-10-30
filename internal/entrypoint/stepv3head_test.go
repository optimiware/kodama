package entrypoint

// T-162: the entrypoint hands every source checkout over detached at the commit the
// next attempt starts from (README.md, "Source HEADs between attempts"). A reader's
// commit on an agent branch must never become the base of a later writer's push to
// the task branch (AR-A09, GR-07), however the earlier step ended, and a checkout
// whose HEAD is not that commit is refused before the command, unchanged.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	gitsupport "github.com/optimiware/kodama/test/support/git"
)

const (
	headAgentBranch = "kodama/agents/wi_1042/run_01/analysis"
	// readerCommits is what a reader does in the write source: a run-scoped agent
	// branch with one commit of its own.
	readerCommits = `cd backend && git checkout -q -b ` + headAgentBranch + ` && echo findings > a.txt && git add a.txt && ` + gitCommitAs + ` -m findings`
)

// headOf is the commit HEAD designates in a workspace directory.
func headOf(t testing.TB, dir string) string { return tgit(t, dir, "rev-parse", "HEAD") }

// wantDetachedAt requires the workspace HEAD to name no branch and designate commit.
func wantDetachedAt(t testing.TB, dir, commit string) {
	t.Helper()
	if got := headOf(t, dir); got != commit {
		t.Fatalf("%s: HEAD is %s, want %s", dir, got, commit)
	}
	cmd := exec.Command("git", "symbolic-ref", "-q", "HEAD")
	cmd.Dir = dir
	cmd.Env = gitEnv()
	if out, err := cmd.Output(); err == nil {
		t.Fatalf("%s: HEAD is not detached, it names %s", dir, strings.TrimSpace(string(out)))
	}
}

// runUntilReadyThenCancel interrupts the entrypoint once the command signalled
// readiness (a file named ready in the workspace) and returns its exit code.
func (f *v3Fixture) runUntilReadyThenCancel() int {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.step = NewStepV3(f.cfg, f.rt, f.v3, f.up)
	done := make(chan int, 1)
	go func() { done <- f.step.Run(ctx) }()
	ready := filepath.Join(f.cfg.WorkspaceDir, "ready")
	deadline := time.Now().Add(30 * time.Second)
	for !fileExists(ready) {
		if time.Now().After(deadline) {
			f.t.Fatalf("the command never became ready\n%s", f.logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		return code
	case <-time.After(30 * time.Second):
		f.t.Fatal("Run did not finish after the interrupt")
	}
	return -1
}

func Test_unit_entrypoint_restores_head_after_non_writer_publish(t *testing.T) {
	t.Run("a reader's published agent-branch commit is not left as HEAD", func(t *testing.T) {
		f := newV3Fixture(t, false, readerCommits)
		r := f.wantOK()
		if len(r.Commits) != 1 || r.Commits[0].Ref != "refs/heads/"+headAgentBranch {
			t.Fatalf("result %+v\n%s", r, f.logs.String())
		}
		dir := f.backend.dir(f)
		published := string(r.Commits[0].Commit)
		if published == f.backend.pinned {
			t.Fatal("the reader published nothing of its own")
		}
		wantDetachedAt(t, dir, f.backend.pinned)
		wantDetachedAt(t, f.frontend.dir(f), f.frontend.pinned)
		// The published commit stays where it was published, on the agent branch only.
		if got := tgit(t, dir, "rev-parse", "refs/heads/"+headAgentBranch); got != published {
			t.Fatalf("the agent branch is %s, want %s", got, published)
		}
		if got := tipOf(t, f.backend, "refs/heads/"+headAgentBranch); got != published {
			t.Fatalf("the service's agent branch is %s, want %s", got, published)
		}
		// The checkout's own index follows HEAD: the reader's file is an untracked
		// leftover of the tree, not staged work of a base nobody asked for.
		if st := tgit(t, dir, "status", "--porcelain"); st != "?? a.txt" {
			t.Fatalf("status after the restore:\n%s", st)
		}
		if !fileExists(filepath.Join(dir, "a.txt")) {
			t.Fatal("the work tree was reset: the scratch is cumulative")
		}
	})

	t.Run("the next attempt begins at the commit its context states and is refused at none other", func(t *testing.T) {
		f := newV3Fixture(t, false, readerCommits)
		f.wantOK()
		f.script = "true"
		f.v3.ctx = f.context()
		f.wantOK()
		if len(f.v3.begun) != 2 {
			t.Fatalf("%d begin calls, want one per attempt", len(f.v3.begun))
		}
		second := f.v3.begun[1].Sources
		if second[0].Head != runtimev3.GitCommit(f.backend.pinned) || second[1].Head != runtimev3.GitCommit(f.frontend.pinned) {
			t.Fatalf("the second attempt observed heads %+v", second)
		}
		// The restored HEAD is what the contract derives clean from: the tracked files
		// are those of head (the reader's file is untracked, so modified, truthfully).
		if !second[0].TrackedClean || !second[0].UntrackedPresent {
			t.Fatalf("observation %+v", second[0])
		}
	})

	t.Run("a step that declines to publish restores HEAD as well", func(t *testing.T) {
		for name, script := range map[string]string{
			"a commit on a detached HEAD":            `cd backend && git checkout -q --detach && echo x > n.txt && git add n.txt && ` + gitCommitAs + ` -m x`,
			"a commit on a branch outside the grant": `cd frontend && git checkout -q -b feature/x && echo x > n.txt && git add n.txt && ` + gitCommitAs + ` -m x`,
			"a branch checked out and nothing else":  `cd backend && git checkout -q -b ` + headAgentBranch,
			"a commit made and abandoned":            `cd backend && echo x > n.txt && git add n.txt && ` + gitCommitAs + ` -m x && git checkout -q --detach HEAD~1`,
		} {
			t.Run(name, func(t *testing.T) {
				f := newV3Fixture(t, false, script)
				r := f.wantOK()
				if len(r.Commits) != 0 {
					t.Fatalf("result %+v", r)
				}
				wantDetachedAt(t, f.backend.dir(f), f.backend.pinned)
				wantDetachedAt(t, f.frontend.dir(f), f.frontend.pinned)
			})
		}
	})

	t.Run("a failed command restores HEAD and publishes nothing", func(t *testing.T) {
		f := newV3Fixture(t, false, readerCommits+" && exit 3")
		f.wantFailed(f.v3Result(t), ReasonExitNonzero)
		wantDetachedAt(t, f.backend.dir(f), f.backend.pinned)
		if tip := tipOf(t, f.backend, "refs/heads/"+headAgentBranch); tip != "" {
			t.Fatalf("a failed step pushed the agent branch: %s", tip)
		}
	})

	t.Run("a timed-out command restores HEAD", func(t *testing.T) {
		f := newV3Fixture(t, false, readerCommits+" && sleep 60")
		f.v3.ctx.Limits.TimeoutSeconds = 1
		f.wantFailed(f.v3Result(t), ReasonTimeout)
		wantDetachedAt(t, f.backend.dir(f), f.backend.pinned)
	})

	t.Run("a cancelled step restores HEAD before it ends", func(t *testing.T) {
		f := newV3Fixture(t, false, readerCommits+` && : > "$KODAMA_WORKSPACE/ready" && sleep 60`)
		f.runUntilReadyThenCancel()
		wantDetachedAt(t, f.backend.dir(f), f.backend.pinned)
		if tip := tipOf(t, f.backend, "refs/heads/"+headAgentBranch); tip != "" {
			t.Fatalf("a cancelled step pushed the agent branch: %s", tip)
		}
	})

	t.Run("a writer restores HEAD to the commit it published, detached, and to its start commit when it published nothing", func(t *testing.T) {
		f := newV3Fixture(t, true, "echo changed > backend/README.md")
		r := f.wantOK()
		if len(r.Commits) != 1 {
			t.Fatalf("result %+v\n%s", r, f.logs.String())
		}
		wantDetachedAt(t, f.backend.dir(f), string(r.Commits[0].Commit))

		// Failed after editing: the tree stays modified (the scratch is cumulative), HEAD is the start commit.
		g := newV3Fixture(t, true, "echo changed > backend/README.md && exit 1")
		g.wantFailed(g.v3Result(t), ReasonExitNonzero)
		wantDetachedAt(t, g.backend.dir(g), g.backend.pinned)

		// On an agent branch with its own commit, publishing both: HEAD follows the task branch commit.
		h := newV3Fixture(t, true, `cd backend && git checkout -q -b `+headAgentBranch+` && echo one > one.txt && git add one.txt && `+gitCommitAs+` -m own && echo two > two.txt`)
		hr := h.wantOK()
		if len(hr.Commits) != 2 {
			t.Fatalf("commits: %s", describe(hr.Commits))
		}
		var task string
		for _, c := range hr.Commits {
			if c.Ref == v3Task {
				task = string(c.Commit)
			}
		}
		wantDetachedAt(t, h.backend.dir(h), task)
	})

	t.Run("a refused checkout is left exactly as found", func(t *testing.T) {
		f := newV3Fixture(t, false, "touch ran")
		dir := f.backend.dir(f)
		tgit(t, dir, "checkout", "-q", "-b", headAgentBranch)
		writeFile(t, filepath.Join(dir, "leak.txt"), "x\n")
		tgit(t, dir, "add", "leak.txt")
		tgit(t, dir, "-c", "user.name=agent", "-c", "user.email=a@example.invalid", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "leak")
		leaked := headOf(t, dir)
		f.wantFailed(f.v3Result(t), ReasonWorkspaceUnverified)
		if fileExists(filepath.Join(f.cfg.WorkspaceDir, "ran")) || f.v3.count("begin") != 0 {
			t.Fatal("the command or the begin fence ran over a checkout whose HEAD is not the context's")
		}
		if got := headOf(t, dir); got != leaked {
			t.Fatalf("a refused checkout was changed: HEAD %s, was %s", got, leaked)
		}
		if ref := tgit(t, dir, "symbolic-ref", "HEAD"); ref != "refs/heads/"+headAgentBranch {
			t.Fatalf("the refused checkout's HEAD names %s", ref)
		}
	})

	t.Run("the restore never reads the workspace's configuration or runs its hooks", func(t *testing.T) {
		f := newV3Fixture(t, false, "")
		marker := filepath.Join(f.root, "executed")
		hooks := filepath.Join(f.backend.dir(f), ".git", "hooks")
		f.script = readerCommits + ` && cd .git && printf '[core]\n\tfsmonitor = touch ` + marker + `\n\thooksPath = ` + hooks + `\n[filter "evil"]\n\tclean = touch ` + marker + `\n' >> config` +
			` && mkdir -p hooks && for h in post-index-change reference-transaction post-checkout; do printf '#!/bin/sh\ntouch ` + marker + `\n' > hooks/$h; chmod +x hooks/$h; done`
		f.v3.ctx = f.context()
		f.wantOK()
		if fileExists(marker) {
			t.Fatal("something the workspace configured was executed while restoring HEAD")
		}
		wantDetachedAt(t, f.backend.dir(f), f.backend.pinned)
	})
}

// v3Result runs the attempt and returns its result, whatever the exit code.
func (f *v3Fixture) v3Result(t testing.TB) runtimev3.ResultV3 {
	t.Helper()
	f.run()
	return f.result()
}

func Test_unit_entrypoint_reader_commit_never_reaches_task_branch(t *testing.T) {
	// refs of a bare repository reachable from nothing but the named commit.
	reachable := func(t *testing.T, s *v3Source, tip, commit string) bool {
		t.Helper()
		cmd := exec.Command("git", "merge-base", "--is-ancestor", commit, tip)
		cmd.Dir = s.repo.Path
		cmd.Env = gitEnv()
		return cmd.Run() == nil
	}

	for name, readerScript := range map[string]string{
		"the reader commits on an agent branch of the write source": readerCommits,
		"the reader makes two commits": `cd backend && git checkout -q -b ` + headAgentBranch +
			` && echo findings > a.txt && git add a.txt && ` + gitCommitAs + ` -m findings && echo more > b.txt && git add b.txt && ` + gitCommitAs + ` -m more`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newV3Fixture(t, false, readerScript)
			rr := f.wantOK()
			if len(rr.Commits) != 1 || rr.Commits[0].RepositoryID != "repo_01" {
				t.Fatalf("the reader's result %+v\n%s", rr, f.logs.String())
			}
			readerTip := string(rr.Commits[0].Commit)
			agentRef := rr.Commits[0].Ref
			if tipOf(t, f.backend, agentRef) != readerTip {
				t.Fatalf("the reader's branch is %s, want %s", tipOf(t, f.backend, agentRef), readerTip)
			}
			readerCommitsN := f.backend.repo.Git(t, "rev-list", "--count", f.backend.pinned+".."+readerTip)

			// The same run's writer, in the same shared workspace, commits on top of the HEAD it finds.
			f.writer = true
			// The writer's commit is the whole non-ignored tree, so it also carries the
			// reader's untracked a.txt: intentional (the scratch is cumulative, a writer
			// commits what earlier steps left; workspaces.md). What it must not carry is
			// the reader's *commit*, asserted below.
			f.script = "echo writer > backend/writer.txt"
			f.v3.ctx = f.context()
			wr := f.wantOK()
			if wr.Outcome != runtimev3.OutcomeCompleted || len(wr.Commits) != 1 || wr.Commits[0].Ref != v3Task {
				t.Fatalf("the writer's result %+v\n%s", wr, f.logs.String())
			}
			taskTip := tipOf(t, f.backend, v3Task)
			if taskTip != string(wr.Commits[0].Commit) {
				t.Fatalf("the task branch is %s, the writer pushed %s", taskTip, wr.Commits[0].Commit)
			}
			// Only the writer's commit and its own base: one commit above the expected tip.
			if parent := f.backend.repo.Git(t, "rev-parse", taskTip+"^"); parent != f.backend.pinned {
				t.Fatalf("the task branch's commit is based on %s, want the pinned commit %s (the reader's tip is %s)", parent, f.backend.pinned, readerTip)
			}
			if n := f.backend.repo.Git(t, "rev-list", "--count", f.backend.pinned+".."+taskTip); n != "1" {
				t.Fatalf("%s commits reached the task branch, want the writer's one", n)
			}
			if reachable(t, f.backend, taskTip, readerTip) {
				t.Fatalf("the reader's commit %s is reachable from the task branch", readerTip)
			}
			if got := f.backend.repo.Git(t, "log", "--format=%an", f.backend.pinned+".."+taskTip); got != "Kodama" {
				t.Fatalf("authors above the base: %q", got)
			}
			// The reader's work is still only on its agent branch.
			if tipOf(t, f.backend, agentRef) != readerTip {
				t.Fatalf("the agent branch moved to %s", tipOf(t, f.backend, agentRef))
			}
			if n := f.backend.repo.Git(t, "rev-list", "--count", f.backend.pinned+".."+tipOf(t, f.backend, agentRef)); n != readerCommitsN {
				t.Fatalf("the agent branch has %s commits above the base, had %s", n, readerCommitsN)
			}
			wantDetachedAt(t, f.backend.dir(f), taskTip)
			if tipOf(t, f.backend, "refs/heads/main") != f.backend.pinned {
				t.Fatal("the default branch moved")
			}
		})
	}

	t.Run("a leaked HEAD is refused for the writer, so it can never become a base", func(t *testing.T) {
		f := newV3Fixture(t, true, "echo writer > backend/writer.txt")
		dir := f.backend.dir(f)
		// A step that did not restore (a killed entrypoint, a failed restore): HEAD
		// stays on the agent branch's commit.
		tgit(t, dir, "checkout", "-q", "-b", headAgentBranch)
		writeFile(t, filepath.Join(dir, "a.txt"), "findings\n")
		tgit(t, dir, "add", "a.txt")
		tgit(t, dir, "-c", "user.name=agent", "-c", "user.email=a@example.invalid", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "findings")
		f.wantFailed(f.v3Result(t), ReasonWorkspaceUnverified)
		if tip := tipOf(t, f.backend, v3Task); tip != f.backend.pinned {
			t.Fatalf("the task branch moved to %s", tip)
		}
		if n := f.gs.CountRequests(func(r gitsupport.Request) bool { return r.Service == "git-receive-pack" }); n != 0 {
			t.Fatalf("%d receive requests after a refused checkout", n)
		}
	})

	t.Run("a head the context advanced is the start commit of the next writer", func(t *testing.T) {
		f := newV3Fixture(t, true, "echo one > backend/one.txt")
		first := f.wantOK()
		head := string(first.Commits[0].Commit)
		// The next context states the published commit as head and tip.
		f.script = "echo two > backend/two.txt"
		f.v3.ctx = f.context()
		c := runtimev3.GitCommit(head)
		f.v3.ctx.Workspace.Sources[0].HeadCommit, f.v3.ctx.Workspace.Sources[0].ExpectedTip = c, &c
		f.v3.ctx.Workspace.Lineage = []runtimev3.LineageEntryV3{{
			StepID: "first", AttemptID: "att_first", Access: runtimev3.AccessWrite, BeganAt: timestamp(-30 * time.Second),
			WorkspaceState: runtimev3.StateClean, Published: first.Commits,
		}}
		second := f.wantOK()
		if len(second.Commits) != 1 {
			t.Fatalf("second result %+v\n%s", second, f.logs.String())
		}
		if parent := f.backend.repo.Git(t, "rev-parse", string(second.Commits[0].Commit)+"^"); parent != head {
			t.Fatalf("the second writer's commit is based on %s, want the published head %s", parent, head)
		}
		if o := f.v3.begun[1].Sources[0]; o.Head != c || !o.TrackedClean || o.UntrackedPresent {
			t.Fatalf("the second begin observed %+v, want a clean source at the published head", o)
		}
	})
}

// bothWrite makes the frontend a write source of the same task branch too.
func (f *v3Fixture) bothWrite() {
	f.t.Helper()
	c := &f.v3.ctx
	branch, tip := v3Task, runtimev3.GitCommit(f.frontend.pinned)
	c.Workspace.Sources[1].Access, c.Workspace.Sources[1].Branch, c.Workspace.Sources[1].ExpectedTip = "write", &branch, &tip
	set, err := c.Workspace.SourceSet()
	if err != nil {
		f.t.Fatal(err)
	}
	digest, err := set.Digest()
	if err != nil {
		f.t.Fatal(err)
	}
	d := runtimev3.Digest(digest)
	c.Workspace.SourceSetDigest = &d
	access, err := c.GitAccess()
	if err != nil {
		f.t.Fatal(err)
	}
	c.Git.Repositories = access
	if err := c.Validate(); err != nil {
		f.t.Fatalf("the two-write-source context is invalid: %v", err)
	}
}

func Test_unit_entrypoint_partial_push_failure_ends_the_workspace(t *testing.T) {
	// A is pushed and B is rejected: the attempt fails, so the next context still
	// states A's old headCommit while the checkout is at A's pushed commit. The
	// successor's verification refuses it (fail closed; no rewind, no second publish).
	f := newV3Fixture(t, true, "echo a > backend/a.txt && echo b > frontend/b.txt")
	f.bothWrite()
	f.v3.onBegin = func() {
		f.frontend.repo.HumanPush(t, v3Task, "someone else", gitsupport.File{Path: "theirs.txt", Content: "theirs"})
	}
	r := f.wantOK()
	f.wantFailed(r, ReasonPublicationConflict)
	pushedA := tipOf(t, f.backend, v3Task)
	if pushedA == f.backend.pinned {
		t.Fatal("the first source's push did not apply")
	}
	wantDetachedAt(t, f.backend.dir(f), pushedA)
	wantDetachedAt(t, f.frontend.dir(f), f.frontend.pinned)

	receives := f.gs.CountRequests(func(r gitsupport.Request) bool { return r.Service == "git-receive-pack" && r.Method == "POST" })
	f.v3.mu.Lock()
	f.v3.results, f.v3.begun = nil, nil
	f.v3.mu.Unlock()
	f.script = "echo again > backend/again.txt"
	f.v3.ctx.Command.Args = []string{f.script}
	f.wantFailed(f.v3Result(t), ReasonWorkspaceUnverified)
	if len(f.v3.begun) != 0 {
		t.Fatal("the begin fence ran over a workspace the context does not state")
	}
	if got := f.gs.CountRequests(func(r gitsupport.Request) bool { return r.Service == "git-receive-pack" && r.Method == "POST" }); got != receives {
		t.Fatalf("%d receives after the refused attempt, want none beyond %d", got-receives, receives)
	}
	if tip := tipOf(t, f.backend, v3Task); tip != pushedA {
		t.Fatalf("the task branch moved to %s after the refusal", tip)
	}
	wantDetachedAt(t, f.backend.dir(f), pushedA) // refused as found
}

func Test_unit_entrypoint_detach_fails_closed(t *testing.T) {
	// What the command leaves behind in the checkout's .git after its commit.
	for name, obstruct := range map[string]string{
		"a leftover index.lock": `: > .git/index.lock`,
		"a symlinked index":     `rm -f .git/index && echo decoy > "$KODAMA_WORKSPACE/index-target" && ln -s "$KODAMA_WORKSPACE/index-target" .git/index`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newV3Fixture(t, false, readerCommits+" && "+obstruct)
			f.run()
			dir := f.backend.dir(f)
			// The restore failed before HEAD: still on the agent branch, past the pinned commit.
			if ref := tgit(t, dir, "symbolic-ref", "HEAD"); ref != "refs/heads/"+headAgentBranch {
				t.Fatalf("HEAD names %s: it was moved although its index could not be rebuilt", ref)
			}
			if got := headOf(t, dir); got == f.backend.pinned {
				t.Fatal("HEAD was moved to the pinned commit")
			}
			if target := filepath.Join(f.cfg.WorkspaceDir, "index-target"); fileExists(target) {
				if b := mustRead(t, target); b != "decoy\n" {
					t.Fatalf("the symlink target was written: %q", b)
				}
			}
			// The next attempt refuses the leaked HEAD (the obstruction is gone, HEAD is not right).
			_ = os.Remove(filepath.Join(dir, ".git", "index.lock"))
			f.v3.mu.Lock()
			f.v3.results, f.v3.begun = nil, nil
			f.v3.mu.Unlock()
			f.script = "touch ran"
			f.v3.ctx = f.context()
			f.wantFailed(f.v3Result(t), ReasonWorkspaceUnverified)
			if fileExists(filepath.Join(f.cfg.WorkspaceDir, "ran")) || len(f.v3.begun) != 0 {
				t.Fatal("the command ran over a leaked HEAD")
			}
		})
	}
}
