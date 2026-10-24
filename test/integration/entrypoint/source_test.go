package entrypoint_test

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/entrypoint"
)

// newWriter is a writer step on the work branch of a fresh repository, pinned
// to (and expecting) the tip c2.
func newWriter(t *testing.T, script string) (*fixture, *repo) {
	t.Helper()
	r := newRepo(t)
	f := newFixture(t, script)
	f.withRepo(r, runtimev1.AccessWrite, r.c2)
	return f, r
}

// bundleCommit is the commit a proposed bundle carries, as read back by an
// independent clone of the remote.
type bundleCommit struct {
	id      string
	files   []string // tree of the commit
	changed []string // files changed relative to the expected tip
	authors string   // "name <email>" of author and committer
	patch   string   // full `git log -p` of the commits the bundle adds
}

// inspectBundle fetches the bundle into a fresh clone of the remote (which
// holds the base commit) and reads the proposed commit back.
func inspectBundle(t *testing.T, r *repo, data []byte) bundleCommit {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "inspect")
	gitRun(t, ".", "clone", "-q", "--bare", r.bare, dir)
	file := filepath.Join(t.TempDir(), "b.bundle")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "bundle", "verify", file)
	gitRun(t, dir, "fetch", "-q", file, "refs/kodama/candidate:refs/test/candidate")
	b := bundleCommit{id: gitRun(t, dir, "rev-parse", "refs/test/candidate")}
	b.files = lines(gitRun(t, dir, "ls-tree", "-r", "--name-only", b.id))
	b.changed = lines(gitRun(t, dir, "diff", "--name-only", r.c2, b.id))
	b.authors = gitRun(t, dir, "log", "--format=%an <%ae> / %cn <%ce>", r.c2+".."+b.id)
	b.patch = gitRun(t, dir, "log", "-p", "--format=fuller", r.c2+".."+b.id)
	return b
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

// gitDirContains reports whether any file below the workspace's .git holds the needle.
func gitDirContains(t *testing.T, workspace string, needles ...string) (found string) {
	t.Helper()
	_ = filepath.Walk(filepath.Join(workspace, ".git"), func(p string, fi os.FileInfo, err error) error {
		if err != nil || !fi.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		for _, n := range needles {
			if bytes.Contains(data, []byte(n)) {
				found = p
			}
		}
		return nil
	})
	return found
}

// Test_source_conflict covers WS-04, WS-06 and PX-10 for writers: publication
// goes through the control plane's expected-tip compare-and-swap; a moved
// branch fails the step without a retry or overwrite, an unknown outcome is
// re-proposed identically until it settles, and injected credentials never
// enter the bundle or the commit.
func Test_source_conflict(t *testing.T) {
	t.Run("human_push_moves_tip_publication_conflict_is_final", func(t *testing.T) {
		f, r := newWriter(t, `echo agent > agent.txt`)
		var human string
		f.srv.Intercept(func(op string, n int, body []byte) action {
			if op == "proposeSource" && n == 1 {
				return action{pre: func() {
					var err error
					if human, err = r.humanPushSafe("human.txt", "human work\n"); err != nil {
						t.Errorf("human push: %v", err)
					}
				}}
			}
			return action{}
		})
		f.wantExit(f.run(), entrypoint.ExitOK)

		res := f.acceptedResult()
		f.wantFailed(res, entrypoint.ReasonPublicationConflict)
		if res.ExitCode == nil || *res.ExitCode != 0 {
			t.Fatalf("exit code = %v, want the observed 0", res.ExitCode)
		}
		if human == "" || r.tip() != human {
			t.Fatalf("the human's tip was overwritten: tip %s, human %s", r.tip(), human)
		}
		if got := f.srv.Count("proposeSource"); got != 1 {
			t.Fatalf("proposeSource calls = %d: a publication conflict must not be retried", got)
		}
		if _, err := gitOut(r.bare, "show", human+":agent.txt"); err == nil {
			t.Fatal("the agent's change reached the human's commit")
		}
		diag := f.diagnostics(res)
		if note := diag["entrypoint.log"]; !strings.Contains(note, human) || !strings.Contains(note, r.c2) {
			t.Fatalf("the conflict note lacks expected/observed tips: %q", note)
		}
		// The proposal that lost stays a recoverable artifact; it is not a result output.
		if len(f.srv.Artifacts(runtimev1.RoleBundle)) != 1 {
			t.Fatalf("bundle artifacts = %d", len(f.srv.Artifacts(runtimev1.RoleBundle)))
		}
		f.credentialsAbsent(gitPass)
	})

	t.Run("conflict_details_come_from_the_server_not_a_retry", func(t *testing.T) {
		f, r := newWriter(t, `echo agent > agent.txt`)
		f.srv.Intercept(func(op string, n int, body []byte) action {
			if op == "proposeSource" {
				return action{errCode: runtimev1.CodePublicationConflict, details: map[string]any{"observedTip": strings.Repeat("3", 40)}}
			}
			return action{}
		})
		f.wantExit(f.run(), entrypoint.ExitOK)
		f.wantFailed(f.acceptedResult(), entrypoint.ReasonPublicationConflict)
		if f.srv.Count("proposeSource") != 1 || r.tip() != r.c2 {
			t.Fatalf("calls %v, tip %s", f.srv.Ops(), r.tip())
		}
		for _, o := range f.srv.Order() {
			if o == "source_accepted" {
				t.Fatal("source was accepted")
			}
		}
	})

	t.Run("pending_settles_by_identical_reproposal", func(t *testing.T) {
		f, r := newWriter(t, `echo agent > agent.txt`)
		f.srv.pendingLeft = 3
		f.wantExit(f.run(), entrypoint.ExitOK)

		res := f.acceptedResult()
		if res.Outcome != runtimev1.OutcomeCompleted || res.SourceCommit == nil || string(*res.SourceCommit) != r.tip() {
			t.Fatalf("result = %+v, remote tip %s", res, r.tip())
		}
		bodies := f.srv.ProposalBodies()
		if len(bodies) != 4 {
			t.Fatalf("proposals = %d, want 3 pending + 1 accepted", len(bodies))
		}
		for _, b := range bodies {
			if string(b) != string(bodies[0]) {
				t.Fatal("re-proposals are not identical")
			}
		}
		if n := len(f.srv.Artifacts(runtimev1.RoleBundle)); n != 1 {
			t.Fatalf("bundle artifacts = %d: the bundle must be uploaded once", n)
		}
		order := f.srv.Order()
		if len(order) != 2 || order[0] != "source_accepted" || order[1] != "result" {
			t.Fatalf("order = %v: the result must follow source acceptance", order)
		}
		if f.srv.Count("proposeSource") != 4 {
			t.Fatalf("calls = %v", f.srv.Ops())
		}
	})

	t.Run("result_is_never_submitted_while_publication_is_pending", func(t *testing.T) {
		f, r := newWriter(t, `echo agent > agent.txt`)
		f.cfg.SettleTimeout = 400 * time.Millisecond
		f.srv.pendingForever = true
		f.wantExit(f.run(), entrypoint.ExitOK)
		res := f.acceptedResult()
		f.wantFailed(res, entrypoint.ReasonPublicationUnsettled)
		if r.tip() != r.c2 {
			t.Fatalf("the branch moved to %s", r.tip())
		}
		if f.srv.Count("proposeSource") < 2 {
			t.Fatalf("the proposal was not repeated: %v", f.srv.Ops())
		}
	})

	t.Run("lost_response_is_replayed_and_observes_the_same_publication", func(t *testing.T) {
		for _, lost := range []int{1, 3} { // within the client's retries, and beyond them
			f, r := newWriter(t, `echo agent > agent.txt`)
			f.srv.Intercept(func(op string, n int, body []byte) action {
				if op == "proposeSource" && n <= lost {
					return action{lose: true}
				}
				return action{}
			})
			f.wantExit(f.run(), entrypoint.ExitOK)
			res := f.acceptedResult()
			if res.Outcome != runtimev1.OutcomeCompleted || res.SourceCommit == nil || string(*res.SourceCommit) != r.tip() {
				t.Fatalf("lost=%d: result = %+v, tip %s", lost, res, r.tip())
			}
			bodies := f.srv.ProposalBodies()
			if len(bodies) != lost+1 {
				t.Fatalf("lost=%d: proposals = %d", lost, len(bodies))
			}
			for _, b := range bodies {
				if string(b) != string(bodies[0]) {
					t.Fatalf("lost=%d: replay differs", lost)
				}
			}
			if n := len(f.srv.Artifacts(runtimev1.RoleBundle)); n != 1 {
				t.Fatalf("lost=%d: bundles = %d", lost, n)
			}
			// Exactly one commit was published on top of the expected tip.
			if got := gitRun(t, r.bare, "rev-list", "--count", r.c2+".."+branch); got != "1" {
				t.Fatalf("lost=%d: %s commits published", lost, got)
			}
		}
	})

	t.Run("bundle_has_the_commit_and_no_credentials_or_foreign_files", func(t *testing.T) {
		script := `echo tracked-change > a.txt
echo new > src.txt
mkdir -p sub && echo nested > sub/n.txt
printf '{"outcome":"completed","outputs":{}}' > "$KODAMA_RESULTS_FILE"
echo '{"ok":true}' > "$KODAMA_OUTPUTS_DIR/report.json"
`
		f, r := newWriter(t, script)
		f.secret("erp-token", "secret://project/erp-token", secretValue, runtimev1.DeliveryFile)
		f.secret("db-pass", "secret://project/db-pass", envSecret, runtimev1.DeliveryEnv)
		f.declareFile("report", "/kodama/outputs/report.json")
		f.wantExit(f.run(), entrypoint.ExitOK)

		res := f.acceptedResult()
		if res.Outcome != runtimev1.OutcomeCompleted || res.SourceCommit == nil {
			t.Fatalf("result = %+v\nlogs:\n%s", res, f.logs.String())
		}
		bundles := f.srv.Artifacts(runtimev1.RoleBundle)
		if len(bundles) != 1 || !bundles[0].usable {
			t.Fatalf("bundles = %+v", bundles)
		}
		props := f.srv.Proposals()
		if len(props) != 1 || props[0].BundleArtifactID != bundles[0].id || string(props[0].ExpectedTip) != r.c2 {
			t.Fatalf("proposals = %+v", props)
		}
		b := inspectBundle(t, r, bundles[0].bytes)
		if b.id != string(*res.SourceCommit) || b.id != string(props[0].Commit) || b.id != r.tip() {
			t.Fatalf("commit mismatch: bundle %s, result %s, proposal %s, tip %s", b.id, *res.SourceCommit, props[0].Commit, r.tip())
		}
		wantFiles := []string{"a.txt", "src.txt", "sub/n.txt"}
		if strings.Join(b.files, ",") != strings.Join(wantFiles, ",") {
			t.Fatalf("committed tree = %v, want %v (no secrets, identity or outputs)", b.files, wantFiles)
		}
		if strings.Join(b.changed, ",") != strings.Join(wantFiles, ",") {
			t.Fatalf("changed files = %v", b.changed)
		}
		if b.authors != "Kodama <step@kodama.invalid> / Kodama <step@kodama.invalid>" {
			t.Fatalf("commit identity = %q", b.authors)
		}
		token, _ := os.ReadFile(f.cfg.TokenFile)
		needles := append([]string{gitPass, secretValue, envSecret, f.srv.admission, string(token), gitUser + ":" + gitPass}, f.srv.Tokens()...)
		for _, n := range needles {
			if n == "" {
				continue
			}
			if bytes.Contains(bundles[0].bytes, []byte(n)) || strings.Contains(b.patch, n) {
				t.Fatalf("credential %q... is in the bundle or commit", n[:4])
			}
		}
		if p := gitDirContains(t, f.cfg.WorkspaceDir, needles...); p != "" {
			t.Fatalf("credential persisted in the workspace's .git: %s", p)
		}
		cfgData, _ := os.ReadFile(filepath.Join(f.cfg.WorkspaceDir, ".git", "config"))
		if !strings.Contains(string(cfgData), r.url()) || strings.Contains(strings.ToLower(string(cfgData)), "extraheader") {
			t.Fatalf(".git/config = %s", cfgData)
		}
		// The admission credential is legitimately sent to /admit once; it must
		// be nowhere else (uploads, logs, other request bodies).
		f.credentialsAbsent(append(needles[:0:0], gitPass, secretValue, envSecret, string(token))...)
		for name, data := range f.srv.UploadedBytes() {
			if bytes.Contains(data, []byte(f.srv.admission)) {
				t.Fatalf("admission credential in uploaded %s", name)
			}
		}
		if strings.Contains(f.logs.String(), f.srv.admission) {
			t.Fatal("admission credential in the logs")
		}
		// The declared output was uploaded as an output, not committed.
		if outs := f.srv.Artifacts(runtimev1.RoleOutput); len(outs) != 1 {
			t.Fatalf("outputs = %d", len(outs))
		}
	})

	t.Run("injected_secret_in_source_is_never_published", func(t *testing.T) {
		cases := map[string]string{
			"secret file copied":      `cp "$KODAMA_SECRETS_DIR/erp-token" leak.txt`,
			"env secret written":      `echo "pass=$DB_PASS" > leak.txt`,
			"runtime token written":   `cat "$IDENTITY/token" > leak.txt`,
			"admission written":       `cat "$IDENTITY/admission" > leak.txt`,
			"checkout cred committed": `cat "$IDENTITY/checkout-credential" > leak.txt`,
			"leak in earlier commit": `cp "$KODAMA_SECRETS_DIR/erp-token" leak.txt
git -c user.name=a -c user.email=a@example.invalid add leak.txt
git -c user.name=a -c user.email=a@example.invalid commit -q -m "oops"
git -c user.name=a -c user.email=a@example.invalid rm -q leak.txt
git -c user.name=a -c user.email=a@example.invalid commit -q -m "remove"
echo fine > ok.txt`,
			"leak in commit message": `echo ok > ok.txt
git add ok.txt
git -c user.name=a -c user.email=a@example.invalid commit -q -m "token $DB_PASS"`,
		}
		for name, script := range cases {
			t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
				f, r := newWriter(t, script)
				f.secret("erp-token", "secret://project/erp-token", secretValue, runtimev1.DeliveryFile)
				f.secret("db-pass", "secret://project/db-pass", envSecret, runtimev1.DeliveryEnv)
				f.srv.ctx.Command.Env["IDENTITY"] = f.path("identity")
				f.wantExit(f.run(), entrypoint.ExitOK)
				f.wantFailed(f.acceptedResult(), entrypoint.ReasonSecretInSource)
				if len(f.srv.Artifacts(runtimev1.RoleBundle)) != 0 || f.srv.Count("proposeSource") != 0 {
					t.Fatalf("a bundle was uploaded or proposed: %v", f.srv.Ops())
				}
				if r.tip() != r.c2 {
					t.Fatalf("branch moved to %s", r.tip())
				}
				f.credentialsAbsent(secretValue, envSecret)
			})
		}
	})

	t.Run("revoked_during_proposal_publishes_nothing_and_submits_nothing", func(t *testing.T) {
		f, r := newWriter(t, `echo agent > agent.txt`)
		f.srv.Intercept(func(op string, n int, body []byte) action {
			if op == "proposeSource" {
				return action{errCode: runtimev1.CodeAttemptRevoked, pre: func() { f.srv.revoked = true }}
			}
			return action{}
		})
		f.wantExit(f.run(), entrypoint.ExitRevoked)
		if f.srv.Count("submitResult") != 0 || r.tip() != r.c2 {
			t.Fatalf("calls %v, tip %s", f.srv.Ops(), r.tip())
		}
	})

	t.Run("unchanged_writer_reports_the_unchanged_tip_without_a_proposal", func(t *testing.T) {
		f, r := newWriter(t, `cat a.txt >/dev/null`)
		f.wantExit(f.run(), entrypoint.ExitOK)
		res := f.acceptedResult()
		if res.Outcome != runtimev1.OutcomeCompleted || res.SourceCommit == nil || string(*res.SourceCommit) != r.c2 {
			t.Fatalf("result = %+v", res)
		}
		if f.srv.Count("proposeSource") != 0 || len(f.srv.Artifacts(runtimev1.RoleBundle)) != 0 {
			t.Fatalf("calls = %v", f.srv.Ops())
		}
	})

	t.Run("failed_writer_never_publishes_or_reports_a_source_commit", func(t *testing.T) {
		f, r := newWriter(t, `echo agent > agent.txt; exit 4`)
		f.wantExit(f.run(), entrypoint.ExitOK)
		f.wantFailed(f.acceptedResult(), entrypoint.ReasonExitNonzero)
		if f.srv.Count("proposeSource") != 0 || r.tip() != r.c2 {
			t.Fatalf("calls %v, tip %s", f.srv.Ops(), r.tip())
		}
	})

	t.Run("missing_output_blocks_publication", func(t *testing.T) {
		f, r := newWriter(t, `echo agent > agent.txt`)
		f.declareFile("report", "/kodama/outputs/report.json")
		f.wantExit(f.run(), entrypoint.ExitOK)
		f.wantFailed(f.acceptedResult(), "output_invalid")
		if f.srv.Count("proposeSource") != 0 || r.tip() != r.c2 {
			t.Fatalf("calls %v, tip %s", f.srv.Ops(), r.tip())
		}
	})

	t.Run("command_commits_are_kept_and_must_descend_from_the_pinned_commit", func(t *testing.T) {
		f, r := newWriter(t, `echo own > own.txt
git add own.txt
git -c user.name=cmd -c user.email=cmd@example.invalid commit -q -m "by the command"
echo extra > extra.txt`)
		f.wantExit(f.run(), entrypoint.ExitOK)
		res := f.acceptedResult()
		if res.SourceCommit == nil || string(*res.SourceCommit) != r.tip() {
			t.Fatalf("result = %+v", res)
		}
		if n := gitRun(t, r.bare, "rev-list", "--count", r.c2+".."+branch); n != "2" {
			t.Fatalf("published %s commits, want the command's and the uncommitted work", n)
		}
		files := lines(gitRun(t, r.bare, "ls-tree", "-r", "--name-only", branch))
		if strings.Join(files, ",") != "a.txt,extra.txt,own.txt" {
			t.Fatalf("tree = %v", files)
		}

		g, r2 := newWriter(t, `git checkout -q --orphan other && echo x > x.txt && git add x.txt &&
git -c user.name=cmd -c user.email=cmd@example.invalid commit -q -m "unrelated history"`)
		g.wantExit(g.run(), entrypoint.ExitOK)
		g.wantFailed(g.acceptedResult(), entrypoint.ReasonSourceFailed)
		if g.srv.Count("proposeSource") != 0 || r2.tip() != r2.c2 {
			t.Fatalf("calls %v", g.srv.Ops())
		}
	})
}
