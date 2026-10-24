package entrypoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// pubCase is a writer attempt whose workspace is a real local Git repository
// with commits c1 and c2 (HEAD), driven directly through publish/settle.
type pubCase struct {
	*unit
	dir    string
	c1, c2 string
	a      *attempt
}

func newPubCase(t *testing.T) *pubCase {
	t.Helper()
	requireGit(t)
	u := newUnit(t, `exit 0`)
	dir := u.cfg.WorkspaceDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "init", "-q", "-b", "work")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "one")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "one")
	c1 := gitRun(t, dir, "rev-parse", "HEAD")
	write("a.txt", "two")
	gitRun(t, dir, "commit", "-qam", "two")
	c2 := gitRun(t, dir, "rev-parse", "HEAD")

	branch := "refs/heads/work"
	u.rt.ctx.Workspace = runtimev1.Workspace{Access: runtimev1.AccessWrite, Source: &runtimev1.Source{
		RepositoryURL: "https://git.example.invalid/acme/repo.git", Commit: runtimev1.GitCommit(c2),
		Branch: &branch, ExpectedTip: ptr(runtimev1.GitCommit(c2)),
	}}
	s := u.build()
	a := &attempt{
		s: s, parent: context.Background(), runCtx: context.Background(), c: u.rt.ctx,
		artifacts: map[domain.ArtifactID]runtimev1.ArtifactRef{}, names: map[string]bool{},
	}
	return &pubCase{unit: u, dir: dir, c1: c1, c2: c2, a: a}
}

func (p *pubCase) write(name, body string) {
	p.t.Helper()
	path := filepath.Join(p.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

// bundleBytes returns the single uploaded bundle.
func (p *pubCase) bundleBytes() []byte {
	p.t.Helper()
	for _, r := range p.rt.reserved {
		if r.Role == runtimev1.RoleBundle {
			return p.up.object("art_unit001")
		}
	}
	p.t.Fatal("no bundle was reserved")
	return nil
}

func (p *pubCase) wantFailed(v *verdict, ab *abortError, reason string) {
	p.t.Helper()
	if ab != nil || v == nil || v.outcome != runtimev1.OutcomeFailed || v.reason != reason {
		p.t.Fatalf("verdict = %+v, abort = %v; want failed/%s\nnotes: %s", v, ab, reason, p.a.notes.String())
	}
}

func TestPublishCommitsWorkOnTopOfPinnedAndProposesBundle(t *testing.T) {
	p := newPubCase(t)
	p.write("a.txt", "changed")
	p.write("new/dir/b.txt", "new")
	commit, v, ab := p.a.publish()
	if v != nil || ab != nil {
		t.Fatalf("verdict %+v abort %v\n%s", v, ab, p.a.notes.String())
	}
	head := gitRun(t, p.dir, "rev-parse", "HEAD")
	if string(commit) != head || head == p.c2 {
		t.Fatalf("commit %s, HEAD %s", commit, head)
	}
	if parent := gitRun(t, p.dir, "rev-parse", "HEAD^"); parent != p.c2 {
		t.Fatalf("parent %s, want the pinned %s", parent, p.c2)
	}
	if who := gitRun(t, p.dir, "log", "-1", "--format=%an <%ae>|%cn <%ce>|%s"); !strings.HasPrefix(who, "Kodama <step@kodama.invalid>|Kodama <step@kodama.invalid>|kodama: build (attempt att_unit01)") {
		t.Fatalf("commit = %q", who)
	}
	if len(p.rt.proposals) != 1 {
		t.Fatalf("proposals = %+v", p.rt.proposals)
	}
	prop := p.rt.proposals[0]
	if prop.Commit != commit || string(prop.ExpectedTip) != p.c2 || prop.BundleArtifactID != "art_unit001" {
		t.Fatalf("proposal = %+v", prop)
	}
	r := p.rt.reserved[0]
	if r.Role != runtimev1.RoleBundle || r.Name != "source.bundle" || r.Kind != runtimev1.KindFile || *r.MediaType != "application/x-git-bundle" {
		t.Fatalf("reservation = %+v", r)
	}
	// The uploaded object is a verifiable bundle holding the commit.
	bf := filepath.Join(t.TempDir(), "b.bundle")
	if err := os.WriteFile(bf, p.bundleBytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := gitRun(t, p.dir, "bundle", "list-heads", bf); !strings.Contains(out, string(commit)) {
		t.Fatalf("bundle heads = %q", out)
	}
	if r.Digest != digestOfBytes(p.bundleBytes()) || *r.SizeBytes != int64(len(p.bundleBytes())) {
		t.Fatalf("reservation digest/size do not match the uploaded object")
	}
}

func TestPublishNothingChangedProposesNothing(t *testing.T) {
	p := newPubCase(t)
	commit, v, ab := p.a.publish()
	if v != nil || ab != nil || string(commit) != p.c2 {
		t.Fatalf("commit %s verdict %+v abort %v", commit, v, ab)
	}
	if len(p.rt.calls) != 0 {
		t.Fatalf("calls = %v", p.rt.calls)
	}
}

func TestPublishIgnoredFilesAreNotCommitted(t *testing.T) {
	p := newPubCase(t)
	p.write(".gitignore", "build/\n*.log\n")
	p.write("build/out.bin", "artifact")
	p.write("x.log", "noise")
	p.write("keep.txt", "keep")
	if _, v, ab := p.a.publish(); v != nil || ab != nil {
		t.Fatalf("verdict %+v abort %v", v, ab)
	}
	files := gitRun(t, p.dir, "ls-tree", "-r", "--name-only", "HEAD")
	if files != ".gitignore\na.txt\nkeep.txt" {
		t.Fatalf("tree = %q", files)
	}
}

func TestPublishPinnedCommitThatIsNotTheTipCannotReportTheTip(t *testing.T) {
	p := newPubCase(t)
	tip := runtimev1.GitCommit(strings.Repeat("9", 40))
	p.a.c.Workspace.Source.ExpectedTip = &tip
	_, v, ab := p.a.publish()
	p.wantFailed(v, ab, ReasonSourceFailed)
	if len(p.rt.calls) != 0 {
		t.Fatalf("calls = %v", p.rt.calls)
	}
}

func TestPublishRequiresDescendantOfPinnedCommit(t *testing.T) {
	p := newPubCase(t)
	gitRun(t, p.dir, "checkout", "-q", "--orphan", "other")
	p.write("x.txt", "x")
	gitRun(t, p.dir, "add", "x.txt")
	gitRun(t, p.dir, "commit", "-q", "-m", "unrelated")
	_, v, ab := p.a.publish()
	p.wantFailed(v, ab, ReasonSourceFailed)
	if len(p.rt.calls) != 0 {
		t.Fatalf("calls = %v", p.rt.calls)
	}
	// Moving HEAD back to an ancestor (not a descendant of the pin) is refused too.
	q := newPubCase(t)
	q.a.c.Workspace.Source.Commit = runtimev1.GitCommit(q.c2)
	gitRun(t, q.dir, "reset", "-q", "--hard", q.c1)
	_, v, ab = q.a.publish()
	q.wantFailed(v, ab, ReasonSourceFailed)
}

func TestPublishNeverPublishesInjectedSecrets(t *testing.T) {
	const secret = "injected-secret-value-77"
	cases := map[string]func(p *pubCase){
		"file content": func(p *pubCase) { p.write("leak.txt", "key="+secret+"\n") },
		"nested file":  func(p *pubCase) { p.write("deep/er/leak.txt", secret) },
		"issued token": func(p *pubCase) {
			p.a.s.remember("rt_issued_token_aaaaaaaa")
			p.write("t.txt", "rt_issued_token_aaaaaaaa")
		},
		"admission": func(p *pubCase) { p.a.s.remember(unitAdmission); p.write("adm.txt", unitAdmission) },
		"earlier commit": func(p *pubCase) {
			p.write("leak.txt", secret)
			gitRun(t, p.dir, "add", "leak.txt")
			gitRun(t, p.dir, "commit", "-q", "-m", "oops")
			gitRun(t, p.dir, "rm", "-q", "leak.txt")
			gitRun(t, p.dir, "commit", "-q", "-m", "remove")
		},
		"commit message": func(p *pubCase) {
			p.write("ok.txt", "ok")
			gitRun(t, p.dir, "add", "ok.txt")
			gitRun(t, p.dir, "commit", "-q", "-m", "token "+secret)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			p := newPubCase(t)
			p.a.needles = [][]byte{[]byte(secret)}
			setup(p)
			_, v, ab := p.a.publish()
			p.wantFailed(v, ab, ReasonSecretInSource)
			if len(p.rt.calls) != 0 || len(p.up.objects) != 0 {
				t.Fatalf("something left the Pod: calls %v", p.rt.calls)
			}
			if strings.Contains(p.a.notes.String(), secret) {
				t.Fatal("the note repeats the secret")
			}
		})
	}
	t.Run("a short value is not scanned and is a documented limit", func(t *testing.T) {
		p := newPubCase(t)
		p.a.needles = [][]byte{[]byte("short")}
		p.write("f.txt", "this has short in it")
		if _, v, ab := p.a.publish(); v != nil || ab != nil {
			t.Fatalf("verdict %+v abort %v", v, ab)
		}
	})
	t.Run("a secret in an unchanged pinned file is not a change", func(t *testing.T) {
		p := newPubCase(t)
		p.a.needles = [][]byte{[]byte("two")}
		p.write("fresh.txt", "unrelated")
		if _, v, ab := p.a.publish(); v != nil || ab != nil {
			t.Fatalf("verdict %+v abort %v", v, ab)
		}
	})
}

func TestPublishBundleLimitAndTransferFailures(t *testing.T) {
	t.Run("bundle over the size limit", func(t *testing.T) {
		p := newPubCase(t)
		p.a.c.Limits.MaxOutputBytes = 10
		p.write("big.txt", strings.Repeat("0123456789abcdef", 100))
		_, v, ab := p.a.publish()
		p.wantFailed(v, ab, ReasonBundleTooLarge)
		if len(p.rt.calls) != 0 {
			t.Fatalf("calls = %v", p.rt.calls)
		}
	})
	t.Run("upload failure is publication_failed", func(t *testing.T) {
		p := newPubCase(t)
		p.up.err = &TransportError{Op: "upload", Err: errors.New("reset")}
		p.write("b.txt", "b")
		_, v, ab := p.a.publish()
		p.wantFailed(v, ab, ReasonPublicationFailed)
		if len(p.rt.proposals) != 0 {
			t.Fatal("a proposal was sent without an uploaded bundle")
		}
	})
	t.Run("revoked while reserving aborts", func(t *testing.T) {
		p := newPubCase(t)
		p.rt.reserve = func(runtimev1.ArtifactReserveRequest) (runtimev1.ArtifactReserveResponse, error) {
			return runtimev1.ArtifactReserveResponse{}, apiErr("reserveArtifact", 403, runtimev1.CodeAttemptRevoked)
		}
		p.write("b.txt", "b")
		_, v, ab := p.a.publish()
		if v != nil || ab == nil || ab.exit != ExitRevoked {
			t.Fatalf("verdict %+v abort %v", v, ab)
		}
	})
	t.Run("a spent finalization budget aborts with the finalization code", func(t *testing.T) {
		p := newPubCase(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		p.a.fin, p.a.finCancel = ctx, cancel
		p.write("b.txt", "b")
		_, v, ab := p.a.publish()
		if v != nil || ab == nil || ab.exit != ExitFinalization {
			t.Fatalf("verdict %+v abort %v", v, ab)
		}
	})
}

func proposal(commit string) runtimev1.SourceProposalRequest {
	return runtimev1.SourceProposalRequest{Commit: runtimev1.GitCommit(commit), ExpectedTip: runtimev1.GitCommit(strings.Repeat("1", 40)), BundleArtifactID: "art_bundle"}
}

func accepted(commit string) (runtimev1.SourceProposalResponse, error) {
	c := runtimev1.GitCommit(commit)
	return runtimev1.SourceProposalResponse{Status: runtimev1.PublishAccepted, Commit: c, AcceptedCommit: &c}, nil
}

func pending(commit string) (runtimev1.SourceProposalResponse, error) {
	return runtimev1.SourceProposalResponse{Status: runtimev1.PublishPending, Commit: runtimev1.GitCommit(commit)}, nil
}

func TestSettleOutcomes(t *testing.T) {
	commit := strings.Repeat("2", 40)
	req := proposal(commit)
	conflict := &APIError{Op: "proposeSource", Status: 409, Code: runtimev1.CodePublicationConflict, Details: map[string]any{"observedTip": strings.Repeat("3", 40)}}
	transport := &TransportError{Op: "proposeSource", Err: errors.New("EOF")}

	type step struct {
		resp runtimev1.SourceProposalResponse
		err  error
	}
	seq := func(steps ...step) func(int, runtimev1.SourceProposalRequest) (runtimev1.SourceProposalResponse, error) {
		return func(n int, _ runtimev1.SourceProposalRequest) (runtimev1.SourceProposalResponse, error) {
			s := steps[min(n, len(steps))-1]
			return s.resp, s.err
		}
	}
	ok, _ := accepted(commit)
	pend, _ := pending(commit)
	wrongCommit, _ := accepted(strings.Repeat("4", 40))

	cases := []struct {
		name      string
		steps     []step
		wantCalls int
		success   bool
		reason    string
		exit      int
		note      string
	}{
		{name: "accepted", steps: []step{{ok, nil}}, wantCalls: 1, success: true},
		{name: "pending then accepted", steps: []step{{pend, nil}, {pend, nil}, {ok, nil}}, wantCalls: 3, success: true},
		{name: "lost response then accepted", steps: []step{{err: transport}, {err: transport}, {ok, nil}}, wantCalls: 3, success: true},
		{name: "pending then transport then accepted", steps: []step{{pend, nil}, {err: transport}, {ok, nil}}, wantCalls: 3, success: true},
		{name: "conflict is final", steps: []step{{err: conflict}}, wantCalls: 1, reason: ReasonPublicationConflict, note: strings.Repeat("3", 40)},
		{name: "conflict after pending is final", steps: []step{{pend, nil}, {err: conflict}, {ok, nil}}, wantCalls: 2, reason: ReasonPublicationConflict},
		{name: "conflict after a lost response is final", steps: []step{{err: transport}, {err: conflict}}, wantCalls: 2, reason: ReasonPublicationConflict},
		{name: "revoked", steps: []step{{err: apiErr("proposeSource", 403, runtimev1.CodeAttemptRevoked)}}, wantCalls: 1, exit: ExitRevoked},
		{name: "other refusal", steps: []step{{err: apiErr("proposeSource", 404, domain.CodeNotFound)}}, wantCalls: 1, reason: ReasonPublicationFailed},
		{name: "publisher answers for another commit", steps: []step{{wrongCommit, nil}}, wantCalls: 1, reason: ReasonPublicationFailed},
		{name: "contract violation is not retried", steps: []step{{err: &ProtocolError{Op: "proposeSource", Err: errors.New("bad")}}}, wantCalls: 1, reason: ReasonPublicationFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := newUnit(t, `exit 0`)
			u.rt.propose = seq(tc.steps...)
			a := &attempt{s: u.build(), parent: context.Background(), runCtx: context.Background()}
			got, v, ab := a.settle(context.Background(), req)
			if len(u.rt.proposals) != tc.wantCalls {
				t.Fatalf("proposals = %d, want %d", len(u.rt.proposals), tc.wantCalls)
			}
			for _, p := range u.rt.proposals {
				if p != req {
					t.Fatalf("re-proposal differs: %+v", p)
				}
			}
			switch {
			case tc.success:
				if v != nil || ab != nil || string(got) != commit {
					t.Fatalf("got %q verdict %+v abort %v", got, v, ab)
				}
			case tc.exit != 0:
				if ab == nil || ab.exit != tc.exit || v != nil {
					t.Fatalf("abort %+v verdict %+v", ab, v)
				}
			default:
				if ab != nil || v == nil || v.reason != tc.reason || v.outcome != runtimev1.OutcomeFailed || v.source != nil {
					t.Fatalf("verdict %+v abort %v", v, ab)
				}
				if tc.note != "" && !strings.Contains(a.notes.String(), tc.note) {
					t.Fatalf("notes = %q", a.notes.String())
				}
			}
		})
	}

	t.Run("pending that never settles within the bound", func(t *testing.T) {
		u := newUnit(t, `exit 0`)
		u.cfg.SettleTimeout = 60 * time.Millisecond
		u.rt.propose = func(int, runtimev1.SourceProposalRequest) (runtimev1.SourceProposalResponse, error) {
			return pending(commit)
		}
		a := &attempt{s: u.build(), parent: context.Background(), runCtx: context.Background()}
		_, v, ab := a.settle(context.Background(), req)
		if ab != nil || v == nil || v.reason != ReasonPublicationUnsettled || v.source != nil {
			t.Fatalf("verdict %+v abort %v", v, ab)
		}
		if len(u.rt.proposals) < 2 {
			t.Fatalf("proposals = %d, want repeated identical proposals", len(u.rt.proposals))
		}
	})
	t.Run("the finalization budget ending is not a settled failure", func(t *testing.T) {
		u := newUnit(t, `exit 0`)
		u.rt.propose = func(int, runtimev1.SourceProposalRequest) (runtimev1.SourceProposalResponse, error) {
			return pending(commit)
		}
		a := &attempt{s: u.build(), parent: context.Background(), runCtx: context.Background()}
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		_, v, ab := a.settle(ctx, req)
		if v != nil || ab == nil || ab.exit != ExitFinalization {
			t.Fatalf("verdict %+v abort %v", v, ab)
		}
	})
	t.Run("lost authority aborts even on a transport error", func(t *testing.T) {
		u := newUnit(t, `exit 0`)
		u.rt.propose = func(int, runtimev1.SourceProposalRequest) (runtimev1.SourceProposalResponse, error) {
			return runtimev1.SourceProposalResponse{}, transport
		}
		run, cancel := context.WithCancelCause(context.Background())
		cancel(errAuthorityLost)
		a := &attempt{s: u.build(), parent: context.Background(), runCtx: run}
		_, v, ab := a.settle(context.Background(), req)
		if v != nil || ab == nil || ab.exit != ExitRevoked {
			t.Fatalf("verdict %+v abort %v", v, ab)
		}
	})
}
