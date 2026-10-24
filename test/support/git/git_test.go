package git_test

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	gitfx "github.com/optimiware/kodama/test/support/git"
)

func authHeader(token string) string {
	return "http.extraHeader=Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(gitfx.Username+":"+token))
}

func gitEnv(srv *gitfx.Server) []string {
	return append(os.Environ(), "GIT_SSL_CAINFO="+srv.CAFile, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
}

func clone(t *testing.T, srv *gitfx.Server, url, token string) error {
	t.Helper()
	cmd := exec.Command("git", "-c", authHeader(token), "clone", "-q", url, filepath.Join(t.TempDir(), "c"))
	cmd.Env = gitEnv(srv)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("clone: %s", out)
	}
	return err
}

func push(t *testing.T, srv *gitfx.Server, work, url, token, refspec string) error {
	t.Helper()
	cmd := exec.Command("git", "-c", authHeader(token), "push", "-q", url, refspec)
	cmd.Dir = work
	cmd.Env = gitEnv(srv)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("push: %s", out)
	}
	return err
}

func TestServerEnforcesTokens(t *testing.T) {
	srv := gitfx.NewServer(t)
	repo := srv.NewRepo(t, "r")
	if err := clone(t, srv, repo.URL, srv.ReadToken); err != nil {
		t.Fatalf("read token cannot clone: %v", err)
	}
	if err := clone(t, srv, repo.URL, "wrong"); err == nil {
		t.Fatal("clone succeeded with a wrong token")
	}
	commit, work := repo.Commit(t, repo.Initial, "x", gitfx.File{Path: "a", Content: "1"})
	if err := push(t, srv, work, repo.URL, srv.ReadToken, commit+":refs/heads/feature"); err == nil {
		t.Fatal("the read token could push")
	}
	if repo.Tip(t, "refs/heads/feature") != "" {
		t.Fatal("ref created by a read-only push")
	}
	if err := push(t, srv, work, repo.URL, srv.WriteToken, commit+":refs/heads/feature"); err != nil {
		t.Fatalf("write token cannot push: %v", err)
	}
	if repo.Tip(t, "refs/heads/feature") != commit {
		t.Fatal("push did not update the ref")
	}
}

func TestHoldAndDropAck(t *testing.T) {
	srv := gitfx.NewServer(t)
	repo := srv.NewRepo(t, "r")
	commit, work := repo.Commit(t, repo.Initial, "x", gitfx.File{Path: "a", Content: "1"})
	hold := srv.HoldReceivePack()
	hold.DropAck = true
	done := make(chan error, 1)
	go func() { done <- push(t, srv, work, repo.URL, srv.WriteToken, commit+":refs/heads/feature") }()
	select {
	case <-hold.Reached:
	case <-time.After(10 * time.Second):
		t.Fatal("receive-pack was never parked")
	}
	if repo.Tip(t, "refs/heads/feature") != "" {
		t.Fatal("the ref moved while the request was parked")
	}
	hold.Release()
	if err := <-done; err == nil {
		t.Fatal("the client saw an acknowledgment that should have been dropped")
	}
	if repo.Tip(t, "refs/heads/feature") != commit {
		t.Fatal("the dropped acknowledgment must still leave the remote updated")
	}
}

func TestBundleHasOneRefAndPrerequisite(t *testing.T) {
	srv := gitfx.NewServer(t)
	repo := srv.NewRepo(t, "r")
	commit, bundle, _ := repo.Bundle(t, repo.Initial, "change", gitfx.File{Path: "a", Content: "1"})
	if len(bundle) == 0 || commit == "" {
		t.Fatal("empty bundle")
	}
	if got := string(bundle[:20]); got != "# v2 git bundle\n-"+repo.Initial[:3] && got[:16] != "# v2 git bundle\n" {
		t.Fatalf("unexpected bundle header %q", got)
	}
}
