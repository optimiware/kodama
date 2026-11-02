package entrypoint

// T-164: an integration step whose approved candidate is already on the default
// branch completes without a push; any other 409 stays a classified failure.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitsupport "github.com/optimiware/kodama/test/support/git"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
)

func publicationConflictErr(reason, tip string) error {
	return &APIError{Op: "integrationGitGrant", Status: http.StatusConflict, Code: runtimev1.CodePublicationConflict, Message: "publication_conflict",
		Details: map[string]any{"reason": reason, "observedTip": tip}}
}

func (f *v3Fixture) integrationConflict(reason, tip string) {
	f.v3.integration = func(int, runtimev3.GitIntegrationRequest) (runtimev3.GitGrantResponse, error) {
		return runtimev3.GitGrantResponse{}, publicationConflictErr(reason, tip)
	}
}

func (f *v3Fixture) receives() int {
	return f.gs.CountRequests(func(r gitsupport.Request) bool { return r.Service == "git-receive-pack" })
}

func (f *v3Fixture) wantNoIntegrationPush(t *testing.T) {
	t.Helper()
	if n := f.receives(); n != 0 {
		t.Fatalf("%d receive requests", n)
	}
	if tipOf(t, f.backend, "refs/heads/main") != f.backend.pinned || tipOf(t, f.backend, v3Task) != f.backend.pinned {
		t.Fatal("a ref moved")
	}
	if _, err := os.Stat(filepath.Join(f.cfg.GitDir, "integration-credential")); !os.IsNotExist(err) {
		t.Fatalf("a credential file exists: %v", err)
	}
}

func Test_unit_entrypoint_integration_already_integrated(t *testing.T) {
	t.Run("the server's answer is the authority", func(t *testing.T) {
		// The workspace HEAD (the pinned commit) is not the observed tip: the control
		// plane says the default branch is at the approved candidate, and that is enough.
		f := newV3Fixture(t, true, "")
		tip := strings.Repeat("a", 40)
		f.integrationConflict("already_integrated", tip)
		f.withTestBin(`"$TEST_BIN" step-git integration --repository repo_01 --gate gate_1 > "$KODAMA_OUTPUTS_DIR/out.txt"`)
		r := f.wantOK()
		if r.Outcome != runtimev3.OutcomeCompleted || len(r.Commits) != 0 {
			t.Fatalf("result %+v\n%s", r, f.logs.String())
		}
		f.wantNoIntegrationPush(t)
		if out := mustRead(t, filepath.Join(f.cfg.OutputsDir, "out.txt")); !strings.Contains(out, "already-integrated "+tip) || !strings.Contains(out, "nothing to push") {
			t.Fatalf("step-git printed %q", out)
		}
	})

	t.Run("the tip equal to the workspace HEAD", func(t *testing.T) {
		f := newV3Fixture(t, true, "")
		f.integrationConflict("already_integrated", f.backend.pinned)
		f.withTestBin(`"$TEST_BIN" step-git integration --repository repo_01 --gate gate_1`)
		r := f.wantOK()
		if r.Outcome != runtimev3.OutcomeCompleted || len(r.Commits) != 0 {
			t.Fatalf("result %+v\n%s", r, f.logs.String())
		}
		f.wantNoIntegrationPush(t)
	})

	t.Run("replays are idempotent", func(t *testing.T) {
		f := newV3Fixture(t, true, "")
		f.integrationConflict("already_integrated", strings.Repeat("b", 40))
		f.withTestBin(`set -e
"$TEST_BIN" step-git integration --repository repo_01 --gate gate_1
"$TEST_BIN" step-git integration --repository repo_01 --gate gate_1`)
		r := f.wantOK()
		if r.Outcome != runtimev3.OutcomeCompleted || len(r.Commits) != 0 || f.v3.count("integrationGitGrant") != 2 {
			t.Fatalf("result %+v calls %v", r, f.v3.calls)
		}
		f.wantNoIntegrationPush(t)
	})

	t.Run("an earlier grant of the attempt is superseded", func(t *testing.T) {
		f := newV3Fixture(t, true, "")
		candidate, work := f.backend.repo.Commit(t, f.backend.pinned, "approved candidate", gitsupport.File{Path: "candidate.txt", Content: "approved"})
		tgit(t, f.backend.dir(f), "fetch", "--quiet", work, candidate)
		f.integrationGrant(candidate, func(int) string { return f.gs.WriteToken })
		grant := f.v3.integration
		f.v3.integration = func(n int, q runtimev3.GitIntegrationRequest) (runtimev3.GitGrantResponse, error) {
			if n == 1 {
				return grant(n, q)
			}
			return runtimev3.GitGrantResponse{}, publicationConflictErr("already_integrated", candidate)
		}
		f.withTestBin(`set -e
"$TEST_BIN" step-git integration --repository repo_01 --gate gate_1
"$TEST_BIN" step-git integration --repository repo_01 --gate gate_1`)
		r := f.wantOK()
		if r.Outcome != runtimev3.OutcomeCompleted || len(r.Commits) != 0 || f.receives() != 0 {
			t.Fatalf("result %+v receives %d\n%s", r, f.receives(), f.logs.String())
		}
	})
}

func Test_unit_entrypoint_integration_base_moved_fails(t *testing.T) {
	other := strings.Repeat("e", 40)
	cases := []struct {
		name, reason, tip string
	}{
		{"base moved", "base_moved", ""},
		{"base moved with any tip", "base_moved", other},
		{"base moved with the workspace HEAD as tip", "base_moved", "PINNED"},
		{"already integrated without a tip", "already_integrated", ""},
		{"already integrated with a malformed tip", "already_integrated", "not-a-commit"},
		{"already integrated with an upper-case tip", "already_integrated", strings.ToUpper(other)},
		{"another reason", "something_else", other},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, ignores := range []bool{false, true} {
				f := newV3Fixture(t, true, "")
				tip := strings.ReplaceAll(tc.tip, "PINNED", f.backend.pinned)
				f.integrationConflict(tc.reason, tip)
				script := `"$TEST_BIN" step-git integration --repository repo_01 --gate gate_1`
				if ignores { // a command that goes on after the refusal still fails the step
					script += " || true"
				}
				f.withTestBin(script)
				r := f.wantOK()
				f.wantFailed(r, ReasonPublicationConflict)
				f.wantNoIntegrationPush(t)
			}
		})
	}

	t.Run("an unrelated 409 or refusal is not already integrated", func(t *testing.T) {
		f := newV3Fixture(t, true, "")
		f.v3.integration = func(int, runtimev3.GitIntegrationRequest) (runtimev3.GitGrantResponse, error) {
			return runtimev3.GitGrantResponse{}, &APIError{Op: "integrationGitGrant", Status: 403, Code: "forbidden",
				Details: map[string]any{"reason": "already_integrated", "observedTip": f.backend.pinned}}
		}
		f.withTestBin(`"$TEST_BIN" step-git integration --repository repo_01 --gate gate_1`)
		f.wantFailed(f.wantOK(), ReasonExitNonzero)
		f.wantNoIntegrationPush(t)
	})

	t.Run("a later grant supersedes the refusal", func(t *testing.T) {
		f := newV3Fixture(t, true, "")
		candidate, work := f.backend.repo.Commit(t, f.backend.pinned, "approved candidate", gitsupport.File{Path: "candidate.txt", Content: "approved"})
		tgit(t, f.backend.dir(f), "fetch", "--quiet", work, candidate)
		f.integrationGrant(candidate, func(int) string { return f.gs.WriteToken })
		grant := f.v3.integration
		f.v3.integration = func(n int, q runtimev3.GitIntegrationRequest) (runtimev3.GitGrantResponse, error) {
			if n == 1 {
				return runtimev3.GitGrantResponse{}, publicationConflictErr("base_moved", f.backend.pinned)
			}
			return grant(n, q)
		}
		f.withTestBin(`"$TEST_BIN" step-git integration --repository repo_01 --gate gate_1 || true
"$TEST_BIN" step-git integration --repository repo_01 --gate gate_1`)
		r := f.wantOK()
		if r.Outcome != runtimev3.OutcomeCompleted || len(r.Commits) != 1 || string(r.Commits[0].Commit) != candidate {
			t.Fatalf("result %+v\n%s", r, f.logs.String())
		}
	})

	t.Run("a refusal is sticky across a transient failure", func(t *testing.T) {
		f := newV3Fixture(t, true, "")
		f.v3.integration = func(n int, _ runtimev3.GitIntegrationRequest) (runtimev3.GitGrantResponse, error) {
			if n == 1 {
				return runtimev3.GitGrantResponse{}, publicationConflictErr("base_moved", strings.Repeat("c", 40))
			}
			return runtimev3.GitGrantResponse{}, &APIError{Op: "integrationGitGrant", Status: 503, Code: "unavailable"}
		}
		f.withTestBin(`"$TEST_BIN" step-git integration --repository repo_01 --gate gate_1 || true
"$TEST_BIN" step-git integration --repository repo_01 --gate gate_1 || true
exit 0`)
		f.wantFailed(f.wantOK(), ReasonPublicationConflict)
		f.wantNoIntegrationPush(t)
		if f.v3.count("integrationGitGrant") != 2 {
			t.Fatalf("calls %v", f.v3.calls)
		}
	})
}

// The adapter keeps the two fields of the v3 error envelope the entrypoint reads.
func Test_unit_entrypoint_integration_conflict_wire_decoding(t *testing.T) {
	const tip = "0123456789abcdef0123456789abcdef01234567"
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/git/integration") {
			failV3(rw, http.StatusNotFound, "not_found")
			return
		}
		replyJSON(rw, http.StatusConflict, runtimev3.ErrorResponse{Error: runtimev3.ErrorBody{Code: runtimev1.CodePublicationConflict, Message: "publication_conflict",
			RequestID: "req_wire", Details: map[string]any{"reason": "already_integrated", "observedTip": tip}}})
	}))
	defer srv.Close()
	c, err := NewClientV3(srv.URL, "att_unit01", "", RetryPolicy{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.setToken("attempt-token")
	_, err = c.IntegrationGrant(context.Background(), runtimev3.GitIntegrationRequest{RepositoryID: "repo_01", GateID: "gate_1"})
	if !publicationConflict(err) {
		t.Fatalf("error %v", err)
	}
	if got, ok := alreadyIntegratedTip(err); !ok || got != tip {
		t.Fatalf("already integrated: %q %v", got, ok)
	}
}
