package entrypoint_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/entrypoint"
)

// ran reads the command's run marker (one line per launch).
func (f *fixture) launches() int {
	b, err := os.ReadFile(filepath.Join(f.pids, "runs"))
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "\n")
}

const markRun = `echo run >> "$PIDDIR/runs"` + "\n"

// Test_cancelled_admission covers PX-08 and PX-A06: an attempt cancelled (or
// otherwise refused) before the command starts never launches it, and a
// consumed admission cannot be replayed by a restarted wrapper.
func Test_cancelled_admission(t *testing.T) {
	t.Run("revoked_before_admission", func(t *testing.T) {
		f := newFixture(t, markRun+`echo hi`)
		f.srv.Revoke()
		f.wantExit(f.run(), entrypoint.ExitRevoked)
		if got := f.srv.Ops(); len(got) != 1 || got[0] != "admit" {
			t.Fatalf("calls = %v: nothing may follow a refused admission", got)
		}
		if f.launches() != 0 {
			t.Fatal("the command was launched for a cancelled attempt")
		}
		if _, err := os.Stat(f.cfg.TokenFile); err == nil {
			t.Fatal("a token file exists for a refused admission")
		}
		if _, err := os.Stat(f.cfg.WorkspaceDir); err == nil {
			t.Fatal("the workspace was prepared for a refused admission")
		}
	})

	t.Run("revoked_between_admit_and_context", func(t *testing.T) {
		f := newFixture(t, markRun+`echo hi`)
		f.srv.Intercept(func(op string, n int, body []byte) action {
			if op == "getContext" {
				return action{errCode: runtimev1.CodeAttemptRevoked, pre: func() { f.srv.revoked = true }}
			}
			return action{}
		})
		f.wantExit(f.run(), entrypoint.ExitRevoked)
		if f.launches() != 0 || f.srv.Count("submitResult") != 0 || f.srv.Count("reserveArtifact") != 0 {
			t.Fatalf("calls = %v", f.srv.Ops())
		}
	})

	t.Run("consumed_admission_cannot_be_replayed_by_a_restarted_wrapper", func(t *testing.T) {
		f := newFixture(t, markRun+`echo hi`)
		f.wantExit(f.run(), entrypoint.ExitOK)
		if f.launches() != 1 {
			t.Fatalf("launches = %d", f.launches())
		}
		before := len(f.srv.Calls())
		f.wantExit(f.run(), entrypoint.ExitAdmission) // admission_consumed
		if f.launches() != 1 || len(f.srv.Calls()) != before+1 {
			t.Fatalf("the restarted wrapper acted: launches %d, calls %v", f.launches(), f.srv.Ops()[before:])
		}
	})

	t.Run("wrong_job_or_credential_or_unreachable_server", func(t *testing.T) {
		f := newFixture(t, markRun+`echo hi`)
		f.cfg.Job.PodUID = "9c9c9c9c-9c9c-4c9c-9c9c-9c9c9c9c9c9c"
		f.wantExit(f.run(), entrypoint.ExitAdmission)
		if f.launches() != 0 || len(f.srv.Calls()) != 1 {
			t.Fatalf("calls = %v", f.srv.Ops())
		}

		g := newFixture(t, markRun+`echo hi`)
		if err := os.WriteFile(g.cfg.AdmissionCredentialFile, []byte("adm_not_the_right_credential_000000\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		g.wantExit(g.run(), entrypoint.ExitAdmission)
		if g.launches() != 0 {
			t.Fatal("launched with a bad credential")
		}

		h := newFixture(t, markRun+`echo hi`)
		h.srv.Server.Close()
		h.wantExit(h.run(), entrypoint.ExitAdmission)
		if h.launches() != 0 {
			t.Fatal("launched without a server")
		}

		u := newFixture(t, markRun+`echo hi`)
		if err := os.Remove(u.cfg.AdmissionCredentialFile); err != nil {
			t.Fatal(err)
		}
		u.wantExit(u.run(), entrypoint.ExitUsage)
		if len(u.srv.Calls()) != 0 {
			t.Fatal("contacted the server without an admission credential")
		}
	})
}

// Test_profile_unsupported covers AR-02: a wrapper/profile pairing the server
// refuses (422 profile_unsupported) stops before anything is prepared.
func Test_profile_unsupported(t *testing.T) {
	f := newFixture(t, markRun+`echo hi`)
	f.srv.unsupported = true
	f.cfg.ClientVersion = "0.0.1"
	f.wantExit(f.run(), entrypoint.ExitAdmission)
	if got := f.srv.Ops(); len(got) != 1 || got[0] != "admit" {
		t.Fatalf("calls = %v: a definitive refusal is not retried", got)
	}
	var req runtimev1.AdmitRequest
	body := f.srv.Calls()[0].body
	if err := decodeInto(body, &req); err != nil || req.ClientVersion != "0.0.1" {
		t.Fatalf("admit request = %s (%v)", body, err)
	}
	if f.launches() != 0 {
		t.Fatal("the command was launched")
	}
	if _, err := os.Stat(f.cfg.WorkspaceDir); err == nil {
		t.Fatal("the workspace was prepared")
	}
	if !strings.Contains(f.logs.String(), "profile_unsupported") {
		t.Fatalf("the refusal is not logged:\n%s", f.logs.String())
	}
}

// Test_token_refresh covers PX-08: the runtime token is renewed during a long
// command, a denied renewal or the end of the token's maximum lifetime stops
// the command and finalizes nothing, and a brief outage is survived.
func Test_token_refresh(t *testing.T) {
	t.Run("renewed_tokens_replace_the_old_one", func(t *testing.T) {
		f := newFixture(t, `sleep 1.5`)
		f.fastRenewal()
		f.wantExit(f.run(), entrypoint.ExitOK)
		tokens := f.srv.Tokens()
		if len(tokens) < 3 {
			t.Fatalf("tokens issued = %d, want renewals during the command", len(tokens))
		}
		calls := f.srv.Calls()
		last := calls[len(calls)-1]
		if last.op != "submitResult" || last.token != tokens[len(tokens)-1] {
			t.Fatalf("the result was not sent with the newest token: %+v", last)
		}
		got, err := os.ReadFile(f.cfg.TokenFile)
		if err != nil || string(got) != tokens[len(tokens)-1] {
			t.Fatalf("token file = %q (%v), want the newest token", got, err)
		}
		if fi, err := os.Stat(f.cfg.TokenFile); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("token file mode = %v (%v)", fi, err)
		}
	})

	t.Run("denied_refresh_mid_run_kills_the_command_and_finalizes_nothing", func(t *testing.T) {
		f := newFixture(t, markRun+treeScript(false))
		f.fastRenewal()
		var armed atomic.Bool
		f.srv.Intercept(func(op string, n int, body []byte) action {
			if op == "renewToken" && armed.Load() {
				return action{errCode: runtimev1.CodeAttemptRevoked, pre: func() { f.srv.revoked = true }}
			}
			return action{}
		})
		wait := f.start(context.Background())
		pids := f.waitPIDs(treePIDs...)
		armed.Store(true)
		f.wantExit(wait(), entrypoint.ExitRevoked)
		wantDead(t, pids)
		for _, op := range []string{"submitResult", "reserveArtifact", "proposeSource", "completeArtifact"} {
			if f.srv.Count(op) != 0 {
				t.Fatalf("%s after the denied refresh: %v", op, f.srv.Ops())
			}
		}
	})

	t.Run("maximum_token_lifetime_ends_the_command_before_expiry", func(t *testing.T) {
		f := newFixture(t, treeScript(false))
		f.fastRenewal()
		f.cfg.RenewReserve = 300 * time.Millisecond
		f.srv.with(func() { f.srv.maxExpires = time.Now().Add(2 * time.Second) })
		wait := f.start(context.Background())
		pids := f.waitPIDs(treePIDs...)
		f.wantExit(wait(), entrypoint.ExitRevoked)
		wantDead(t, pids)
		if f.srv.Count("submitResult") != 0 {
			t.Fatalf("a result was submitted past the maximum token lifetime: %v", f.srv.Ops())
		}
	})

	t.Run("brief_renewal_outage_does_not_kill_the_command", func(t *testing.T) {
		f := newFixture(t, `sleep 2`)
		f.srv.with(func() { f.srv.ttl = 1500 * time.Millisecond })
		f.srv.Intercept(func(op string, n int, body []byte) action {
			if op == "renewToken" && n <= 2 { // the client's own two tries
				return action{status: 503}
			}
			return action{}
		})
		f.wantExit(f.run(), entrypoint.ExitOK)
		if f.srv.Count("renewToken") < 3 {
			t.Fatalf("renewals = %d", f.srv.Count("renewToken"))
		}
		if r := f.acceptedResult(); r.Outcome != runtimev1.OutcomeCompleted {
			t.Fatalf("result = %+v", r)
		}
	})

	t.Run("denial_codes_other_than_revoked_also_stop_the_command", func(t *testing.T) {
		for _, code := range []domain.ErrorCode{domain.CodeInvalidCredential, domain.CodeForbidden} {
			f := newFixture(t, markRun+`sleep 30`)
			f.fastRenewal()
			f.srv.Intercept(func(op string, n int, body []byte) action {
				if op == "renewToken" && n >= 2 {
					return action{errCode: code}
				}
				return action{}
			})
			began := time.Now()
			f.wantExit(f.run(), entrypoint.ExitRevoked)
			if time.Since(began) > 15*time.Second {
				t.Fatalf("%s: slow stop", code)
			}
			if f.srv.Count("submitResult") != 0 {
				t.Fatalf("%s: result submitted", code)
			}
		}
	})
}

// Test_exact_commit_checkout covers PX-14 and WS-02: a step runs on exactly
// the pinned commit even after the branch moved, with a private checkout whose
// Git state carries no credential, and a reader never publishes.
func Test_exact_commit_checkout(t *testing.T) {
	t.Run("pinned_commit_survives_branch_movement", func(t *testing.T) {
		r := newRepo(t)
		f := newFixture(t, markRun+`git rev-parse HEAD > "$KODAMA_OUTPUTS_DIR/head.txt"
cat a.txt > "$KODAMA_OUTPUTS_DIR/seen.txt"
git remote get-url origin > "$KODAMA_OUTPUTS_DIR/remote.txt"
git symbolic-ref -q HEAD > "$KODAMA_OUTPUTS_DIR/symref.txt" || echo detached > "$KODAMA_OUTPUTS_DIR/symref.txt"
echo scribble > a.txt; echo more > new.txt`)
		f.withRepo(r, runtimev1.AccessRead, r.c1)
		for _, n := range []string{"head", "seen", "remote", "symref"} {
			f.declareFile(n, "/kodama/outputs/"+n+".txt")
		}
		human := r.humanPush(t, "later.txt", "later work\n")
		if r.tip() != human || human == r.c1 || human == r.c2 {
			t.Fatalf("tip %s", r.tip())
		}
		f.wantExit(f.run(), entrypoint.ExitOK)
		res := f.acceptedResult()
		if res.Outcome != runtimev1.OutcomeCompleted || res.SourceCommit != nil {
			t.Fatalf("result = %+v", res)
		}
		seen := map[string]string{}
		for _, a := range f.srv.Artifacts(runtimev1.RoleOutput) {
			seen[strings.TrimSuffix(a.req.Name, ".txt")] = strings.TrimSpace(string(a.bytes))
		}
		if seen["head"] != r.c1 || seen["seen"] != "one" {
			t.Fatalf("the command ran on %s with a.txt=%q, want pinned %s", seen["head"], seen["seen"], r.c1)
		}
		if seen["remote"] != r.url() || strings.Contains(seen["remote"], "@") || seen["symref"] != "detached" {
			t.Fatalf("remote %q, symref %q", seen["remote"], seen["symref"])
		}
		if p := gitDirContains(t, f.cfg.WorkspaceDir, gitPass, f.srv.admission); p != "" {
			t.Fatalf("credential persisted in %s", p)
		}
		// Reader: no publication of any kind, the remote is untouched.
		if f.srv.Count("proposeSource") != 0 || len(f.srv.Artifacts(runtimev1.RoleBundle)) != 0 {
			t.Fatalf("a reader published: %v", f.srv.Ops())
		}
		if r.tip() != human {
			t.Fatalf("remote tip %s changed", r.tip())
		}
		// The credential reached the Git server as an Authorization header only.
		r.srv.mu.Lock()
		auths := append([]string(nil), r.srv.auths...)
		r.srv.mu.Unlock()
		var sent bool
		for _, a := range auths {
			sent = sent || strings.HasPrefix(a, "Basic ")
		}
		if !sent {
			t.Fatal("the checkout credential was not used")
		}
		f.credentialsAbsent(gitPass)
	})

	t.Run("unknown_commit_fails_checkout_without_launching", func(t *testing.T) {
		r := newRepo(t)
		f := newFixture(t, markRun+`echo hi`)
		f.withRepo(r, runtimev1.AccessRead, strings.Repeat("a", 40))
		f.wantExit(f.run(), entrypoint.ExitOK)
		f.wantFailed(f.acceptedResult(), entrypoint.ReasonCheckoutFailed)
		if f.launches() != 0 {
			t.Fatal("the command ran without its source")
		}
	})

	t.Run("bad_or_missing_credential_fails_checkout_without_leaking_it", func(t *testing.T) {
		const wrong = "wrong-checkout-pass-77aa"
		r := newRepo(t)
		f := newFixture(t, markRun+`echo hi`)
		f.withRepo(r, runtimev1.AccessRead, r.c1)
		if err := os.WriteFile(f.cfg.CheckoutCredentialFile, []byte(gitUser+":"+wrong+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.wantExit(f.run(), entrypoint.ExitOK)
		res := f.acceptedResult()
		f.wantFailed(res, entrypoint.ReasonCheckoutFailed)
		for _, d := range f.diagnostics(res) {
			if strings.Contains(d, wrong) {
				t.Fatal("the credential leaked into a diagnostic")
			}
		}
		f.credentialsAbsent(wrong, gitPass)

		g := newFixture(t, markRun+`echo hi`)
		g.withRepo(r, runtimev1.AccessRead, r.c1)
		if err := os.Remove(g.cfg.CheckoutCredentialFile); err != nil {
			t.Fatal(err)
		}
		g.wantExit(g.run(), entrypoint.ExitOK)
		g.wantFailed(g.acceptedResult(), entrypoint.ReasonCheckoutFailed)
		if g.launches() != 0 {
			t.Fatal("launched without a checkout")
		}

		h := newFixture(t, markRun+`echo hi`)
		h.withRepo(r, runtimev1.AccessRead, r.c1)
		if err := os.WriteFile(h.cfg.CheckoutCredentialFile, []byte("not-a-pair\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		h.wantExit(h.run(), entrypoint.ExitOK)
		h.wantFailed(h.acceptedResult(), entrypoint.ReasonCheckoutFailed)
	})

	t.Run("no_workspace_step_skips_checkout", func(t *testing.T) {
		f := newFixture(t, markRun+`test ! -e .git`)
		f.wantExit(f.run(), entrypoint.ExitOK)
		if r := f.acceptedResult(); r.Outcome != runtimev1.OutcomeCompleted || r.SourceCommit != nil || f.launches() != 1 {
			t.Fatalf("result = %+v", r)
		}
	})
}
