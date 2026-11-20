package clientcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientcmdDocsAndContextOfflineAndRealGitPull(t *testing.T) {
	isolateUserConfig(t)
	t.Setenv("KODEMA_SERVER", "")
	t.Setenv("KODEMA_SERVER_URL", "")
	t.Setenv("KODEMA_URL", "")
	t.Setenv("KODEMA_CA_FILE", "")
	t.Setenv("KODEMA_CREDENTIAL_FILE", "")
	t.Setenv("KODEMA_GIT_URL", "")

	// 1. `kodema docs` and `kodema docs install` run offline with no server/config.
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"docs", "workload"}, &out, &errOut, defaultCLIDeps())
	if code != 0 {
		t.Fatalf("kodema docs workload exit = %d; stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "# Container Workload Contract") {
		t.Fatalf("kodema docs workload output unexpected:\n%s", out.String())
	}

	dest := t.TempDir()
	out.Reset()
	errOut.Reset()
	code = run(context.Background(), []string{"docs", "install", "--target", "opencode", "--dest", dest}, &out, &errOut, defaultCLIDeps())
	if code != 0 {
		t.Fatalf("kodema docs install exit = %d; stderr=%q", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dest, ".opencode", "skills", "kodema", "SKILL.md")); err != nil {
		t.Fatalf("installed kodema SKILL.md missing: %v", err)
	}

	// 2. `kodema context list` and `kodema context show` run offline against a local dir.
	ctxRepoWork := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ctxRepoWork, "skills"), 0o750); err != nil {
		t.Fatalf("mkdir skills: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ctxRepoWork, "conventions.md"), []byte("# Org Rules\n\nUse Go.\n"), 0o600); err != nil {
		t.Fatalf("write conventions: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ctxRepoWork, "skills", "code-review.md"), []byte("# Code Review\n\nCheck race conditions.\n"), 0o600); err != nil {
		t.Fatalf("write skill: %v", err)
	}

	out.Reset()
	errOut.Reset()
	code = run(context.Background(), []string{"context", "list", "--dir", ctxRepoWork}, &out, &errOut, defaultCLIDeps())
	if code != 0 || !strings.Contains(out.String(), "code-review") {
		t.Fatalf("kodema context list = (%d, %q, %q)", code, out.String(), errOut.String())
	}

	out.Reset()
	errOut.Reset()
	code = run(context.Background(), []string{"context", "show", "code-review", "--dir", ctxRepoWork}, &out, &errOut, defaultCLIDeps())
	if code != 0 || !strings.Contains(out.String(), "Check race conditions.") {
		t.Fatalf("kodema context show = (%d, %q, %q)", code, out.String(), errOut.String())
	}

	// 3. `kodema context pull` executes real git via runSystemGit and writes to disk.
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=Kodema Test",
		"GIT_AUTHOR_EMAIL=test@kodema.example",
		"GIT_COMMITTER_NAME=Kodema Test",
		"GIT_COMMITTER_EMAIL=test@kodema.example",
		"GIT_TERMINAL_PROMPT=0",
	)
	for _, args := range [][]string{
		{"init", "--initial-branch=main"},
		{"add", "-A"},
		{"commit", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = ctxRepoWork
		cmd.Env = gitEnv
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(output))
		}
	}
	bareRoot := t.TempDir()
	cloneCmd := exec.Command("git", "clone", "--bare", "--quiet", ctxRepoWork, filepath.Join(bareRoot, "context"))
	cloneCmd.Env = gitEnv
	if output, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare: %v\n%s", err, string(output))
	}

	t.Setenv("KODEMA_GIT_URL", "file://"+filepath.ToSlash(bareRoot))
	pullDest := t.TempDir()
	out.Reset()
	errOut.Reset()
	code = run(context.Background(), []string{
		"context", "pull", "--repo", "context", "--dest", pullDest, "--target", "agents", "--agents-md", "AGENTS.md",
	}, &out, &errOut, defaultCLIDeps())
	if code != 0 {
		t.Fatalf("kodema context pull exit = %d; stderr=%q", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(pullDest, ".agents", "skills", "code-review", "SKILL.md")); err != nil {
		t.Fatalf("pulled skill missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pullDest, ".kodema", "context", "context", "conventions.md")); err != nil {
		t.Fatalf("pulled conventions doc missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pullDest, "AGENTS.md")); err != nil {
		t.Fatalf("AGENTS.md missing: %v", err)
	}

	// 4. Ambiguous tag named HEAD emits a git stderr warning that must not
	//    pollute the recorded commit SHA, and symlinked AGENTS.md -> CLAUDE.md
	//    updates the target without breaking the symlink.
	tagCmd := exec.Command("git", "update-ref", "refs/tags/HEAD", "HEAD")
	tagCmd.Dir = ctxRepoWork
	tagCmd.Env = gitEnv
	if output, err := tagCmd.CombinedOutput(); err != nil {
		t.Fatalf("git update-ref refs/tags/HEAD: %v\n%s", err, string(output))
	}
	bareAmbig := filepath.Join(bareRoot, "ambig")
	ambigClone := exec.Command("git", "clone", "--bare", "--quiet", ctxRepoWork, bareAmbig)
	ambigClone.Env = gitEnv
	if output, err := ambigClone.CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare ambig: %v\n%s", err, string(output))
	}
	symlinkDest := t.TempDir()
	claudeMD := filepath.Join(symlinkDest, "CLAUDE.md")
	agentsLink := filepath.Join(symlinkDest, "AGENTS.md")
	if err := os.WriteFile(claudeMD, []byte("# Shared Agent Rules\n"), 0o600); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}
	if err := os.Symlink("CLAUDE.md", agentsLink); err != nil {
		t.Fatalf("symlink AGENTS.md: %v", err)
	}
	out.Reset()
	errOut.Reset()
	code = run(context.Background(), []string{
		"context", "pull", "--repo", "ambig", "--dest", symlinkDest, "--agents-md", "AGENTS.md", "--output", "json",
	}, &out, &errOut, defaultCLIDeps())
	if code != 0 {
		t.Fatalf("context pull ambig exit = %d; stderr=%q", code, errOut.String())
	}
	var pullPayload struct {
		Repos []struct {
			Commit string `json:"commit"`
		} `json:"repos"`
	}
	if err := json.Unmarshal(out.Bytes(), &pullPayload); err != nil {
		t.Fatalf("decode ambig pull json: %v\n%s", err, out.String())
	}
	if len(pullPayload.Repos) != 1 || strings.Contains(pullPayload.Repos[0].Commit, "warning") || len(pullPayload.Repos[0].Commit) != 40 {
		t.Fatalf("ambig pull commit = %q; want clean 40-char SHA", pullPayload.Repos[0].Commit)
	}
	linkInfo, err := os.Lstat(agentsLink)
	if err != nil || linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("AGENTS.md symlink was broken (err=%v, mode=%v)", err, linkInfo)
	}
	claudeBytes, err := os.ReadFile(claudeMD)
	if err != nil || !strings.Contains(string(claudeBytes), "<!-- kodema:context:start -->") {
		t.Fatalf("CLAUDE.md target was not updated via symlink: %q (err=%v)", string(claudeBytes), err)
	}

	// 5. Offline `kodema docs` respects default config file's output format.
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgFile, []byte(`{"server_url":"https://kodema.example.com","output":"json"}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("KODEMA_CONFIG", cfgFile)
	out.Reset()
	errOut.Reset()
	code = run(context.Background(), []string{"docs", "--list"}, &out, &errOut, defaultCLIDeps())
	if code != 0 {
		t.Fatalf("kodema docs --list with config output=json exit = %d; stderr=%q", code, errOut.String())
	}
	var docsJSON struct {
		Topics []struct {
			Slug string `json:"slug"`
		} `json:"topics"`
	}
	if err := json.Unmarshal(out.Bytes(), &docsJSON); err != nil || len(docsJSON.Topics) == 0 {
		t.Fatalf("kodema docs --list did not honor config output=json: %q (err=%v)", out.String(), err)
	}
}
