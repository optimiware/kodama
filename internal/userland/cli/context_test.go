package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/optimiware/kodema/internal/docs"
	"github.com/optimiware/kodema/internal/userland/cli"
)

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Kodema Test",
		"GIT_AUTHOR_EMAIL=test@kodema.example",
		"GIT_COMMITTER_NAME=Kodema Test",
		"GIT_COMMITTER_EMAIL=test@kodema.example",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func makeTestContextRepo(t *testing.T, barePath string, files map[string]string) string {
	t.Helper()
	work := t.TempDir()
	runTestGit(t, work, "init", "--initial-branch=main")
	for rel, content := range files {
		full := filepath.Join(work, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	runTestGit(t, work, "add", "-A")
	runTestGit(t, work, "commit", "-m", "initial context")
	commit := runTestGit(t, work, "rev-parse", "HEAD")

	if err := os.MkdirAll(filepath.Dir(barePath), 0o750); err != nil {
		t.Fatalf("mkdir bare parent: %v", err)
	}
	runTestGit(t, work, "clone", "--bare", "--quiet", work, barePath)
	return commit
}

func realGitRunner(ctx context.Context, dir string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%s", msg)
		}
		return "", fmt.Errorf("%s", strings.TrimSpace(stdout.String()))
	}
	return stdout.String(), nil
}

func TestContextPullMultiRepoOverrideFrontmatterAgentsMDAndKodemaDocs(t *testing.T) {
	gitRoot := t.TempDir()
	companyCommit := makeTestContextRepo(t, filepath.Join(gitRoot, "context"), map[string]string{
		"README.md":                 "# Company Context\n\nShared standards.\n",
		"conventions.md":            "# Conventions\n\nAlways run go test -race.\n",
		"docs/architecture.md":      "# Architecture\n\nLayered Go modules.\n",
		"skills/build-and-test.md":  "# Skill: build and test\n\nCompany default build and test guide.\n",
		"skills/security-review.md": "# Skill: security review\n\nCheck authorization boundaries before merge.\n",
	})
	projectCommit := makeTestContextRepo(t, filepath.Join(gitRoot, "shop", "context"), map[string]string{
		"conventions.md":                             "# Shop Conventions\n\nShop-specific rules.\n",
		"skills/build-and-test/SKILL.md":             "---\nname: build-and-test\ndescription: Shop-specific digest build and test workflow\n---\n\n# Shop build and test\n\nOverride company default.\n",
		"skills/build-and-test/references/checks.md": "# Checks\n\nRun smoke tests.\n",
	})

	fs := newFakeFS()
	fs.files["/work/AGENTS.md"] = []byte("# Repo Instructions\n\nKeep commits small.\n")
	var out, errOut bytes.Buffer
	ports := cli.Ports{Files: fs, Out: &out, Err: &errOut}
	deps := cli.CommandDeps{
		GitRun: realGitRunner,
		Env: func(key string) string {
			if key == "KODEMA_GIT_URL" {
				return "file://" + filepath.ToSlash(gitRoot)
			}
			return ""
		},
	}

	code := cli.RunWithDeps(context.Background(), cli.Config{}, []string{
		"context", "pull",
		"--repo", "context",
		"--repo", "shop/context@" + projectCommit,
		"--dest", "/work",
		"--target", "all",
		"--agents-md", "AGENTS.md",
		"--include-kodema-docs",
		"--output", "json",
	}, ports, deps)
	if code != cli.ExitSuccess {
		t.Fatalf("context pull exit = %d; stderr=%q", code, errOut.String())
	}

	var summary struct {
		Target   string `json:"target"`
		Dest     string `json:"dest"`
		AgentsMD string `json:"agents_md"`
		Repos    []struct {
			Repo   string `json:"repo"`
			Commit string `json:"commit"`
			Skills int    `json:"skills"`
			Docs   int    `json:"docs"`
		} `json:"repos"`
		Skills []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"skills"`
		Docs []struct {
			Repo string `json:"repo"`
			Path string `json:"path"`
			Dest string `json:"dest"`
		} `json:"docs"`
	}
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatalf("decode context pull json: %v\n%s", err, out.String())
	}
	if len(summary.Repos) != 2 {
		t.Fatalf("repos count = %d; want 2", len(summary.Repos))
	}
	if summary.Repos[0].Repo != "context" || summary.Repos[0].Commit != companyCommit {
		t.Fatalf("repo[0] = %+v; want context@%s", summary.Repos[0], companyCommit)
	}
	if summary.Repos[1].Repo != "shop/context" || summary.Repos[1].Commit != projectCommit {
		t.Fatalf("repo[1] = %+v; want shop/context@%s", summary.Repos[1], projectCommit)
	}

	// Verify skill override + synthesized frontmatter + supporting files + embedded kodema skill
	// across all three target directories (.agents, .claude, .opencode).
	for _, rootDir := range []string{"/work/.agents/skills", "/work/.claude/skills", "/work/.opencode/skills"} {
		// Overridden directory skill preserves explicit frontmatter & copies references/checks.md.
		bt, err := fs.ReadFile(rootDir + "/build-and-test/SKILL.md")
		if err != nil {
			t.Fatalf("missing %s/build-and-test/SKILL.md: %v", rootDir, err)
		}
		if !strings.Contains(string(bt), "Shop-specific digest build and test workflow") || !strings.Contains(string(bt), "Override company default.") {
			t.Fatalf("build-and-test was not overridden by project repo:\n%s", string(bt))
		}
		ref, err := fs.ReadFile(rootDir + "/build-and-test/references/checks.md")
		if err != nil || !strings.Contains(string(ref), "Run smoke tests.") {
			t.Fatalf("supporting reference file missing or wrong: %q, %v", string(ref), err)
		}

		// Plain flat Markdown skill receives portable YAML frontmatter automatically.
		sr, err := fs.ReadFile(rootDir + "/security-review/SKILL.md")
		if err != nil {
			t.Fatalf("missing %s/security-review/SKILL.md: %v", rootDir, err)
		}
		if !strings.HasPrefix(string(sr), "---\nname: security-review\ndescription: \"Check authorization boundaries before merge.\"\n---\n\n# Skill: security review") {
			t.Fatalf("synthesized frontmatter mismatch:\n%s", string(sr))
		}

		// Embedded Kodema docs skill was installed alongside company skills.
		kd, err := fs.ReadFile(rootDir + "/kodema/SKILL.md")
		if err != nil || string(kd) != docs.SkillDocument() {
			t.Fatalf("embedded kodema skill missing in %s: %v", rootDir, err)
		}
	}

	// Context docs copied under /work/.kodema/context/<repo>/...
	for _, wantPath := range []string{
		"/work/.kodema/context/context/README.md",
		"/work/.kodema/context/context/conventions.md",
		"/work/.kodema/context/context/docs/architecture.md",
		"/work/.kodema/context/shop/context/conventions.md",
	} {
		if _, err := fs.ReadFile(wantPath); err != nil {
			t.Fatalf("expected context doc %s: %v", wantPath, err)
		}
	}

	// AGENTS.md preserved existing content and appended managed block with relative paths.
	agentsMD, err := fs.ReadFile("/work/AGENTS.md")
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	agentsText := string(agentsMD)
	if !strings.HasPrefix(agentsText, "# Repo Instructions\n\nKeep commits small.\n") {
		t.Fatalf("existing AGENTS.md content was not preserved:\n%s", agentsText)
	}
	for _, want := range []string{
		"<!-- kodema:context:start -->",
		"<!-- kodema:context:end -->",
		"`.kodema/context/context/conventions.md`",
		"`.agents/skills`",
		"`build-and-test`",
		"`security-review`",
		"`kodema`",
	} {
		if !strings.Contains(agentsText, want) {
			t.Errorf("AGENTS.md missing %q:\n%s", want, agentsText)
		}
	}

	// Re-running updates the managed block idempotently without duplicating it.
	out.Reset()
	errOut.Reset()
	code = cli.RunWithDeps(context.Background(), cli.Config{}, []string{
		"context", "pull", "--repo", "context", "--dest", "/work", "--agents-md", "AGENTS.md",
	}, ports, deps)
	if code != cli.ExitSuccess {
		t.Fatalf("second context pull exit = %d; stderr=%q", code, errOut.String())
	}
	agentsMD2, _ := fs.ReadFile("/work/AGENTS.md")
	if strings.Count(string(agentsMD2), "<!-- kodema:context:start -->") != 1 {
		t.Fatalf("AGENTS.md duplicated managed block:\n%s", string(agentsMD2))
	}
	if !strings.HasPrefix(string(agentsMD2), "# Repo Instructions\n\nKeep commits small.\n") {
		t.Fatalf("second run lost user AGENTS.md content:\n%s", string(agentsMD2))
	}
}

func TestContextPullRefusesSecretBeforeWritingDestination(t *testing.T) {
	gitRoot := t.TempDir()
	makeTestContextRepo(t, filepath.Join(gitRoot, "clean"), map[string]string{
		"skills/safe-skill.md": "# Safe\n\nNo secrets here.\n",
	})
	makeTestContextRepo(t, filepath.Join(gitRoot, "leaky"), map[string]string{
		"conventions.md": "# Leaky\n\nGITHUB_TOKEN=ghp_supersecretvalue123\n",
	})

	fs := newFakeFS()
	var out, errOut bytes.Buffer
	ports := cli.Ports{Files: fs, Out: &out, Err: &errOut}
	deps := cli.CommandDeps{
		GitRun: realGitRunner,
		Env: func(key string) string {
			if key == "KODEMA_GIT_URL" {
				return "file://" + filepath.ToSlash(gitRoot)
			}
			return ""
		},
	}

	code := cli.RunWithDeps(context.Background(), cli.Config{}, []string{
		"context", "pull", "--repo", "clean", "--repo", "leaky", "--dest", "/work",
	}, ports, deps)
	if code != cli.ExitFailure {
		t.Fatalf("leaky context pull exit = %d; want %d", code, cli.ExitFailure)
	}
	if strings.Contains(errOut.String(), "ghp_supersecretvalue123") {
		t.Fatalf("stderr leaked secret value: %q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "credential material") {
		t.Fatalf("stderr = %q; want credential material diagnostic", errOut.String())
	}
	// Phase 1 validation must abort before writing anything from the clean repo too.
	if len(fs.files) != 0 {
		t.Fatalf("destination was modified despite validation failure: %v", fs.files)
	}
}

func TestContextPullRefusesInvalidInputsAndSymlinks(t *testing.T) {
	for _, args := range [][]string{
		{"context", "pull"},
		{"context", "pull", "@"},
		{"context", "pull", "--repo", "@", "--git-url", "https://git.example.com"},
		{"context", "pull", "--repo", "@main", "--git-url", "https://git.example.com"},
		{"context", "pull", "--repo", "context@", "--git-url", "https://git.example.com"},
		{"context", "pull", "--repo", "../escape", "--git-url", "https://git.example.com"},
		{"context", "pull", "--repo", "-flag-injection", "--git-url", "https://git.example.com"},
		{"context", "pull", "--repo", "context", "--ref", "--upload-pack=evil", "--git-url", "https://git.example.com"},
		{"context", "pull", "--repo", "http://insecure.example/context"},
		{"context", "pull", "--repo", "https://user:pass@git.example.com/context"},
		{"context", "pull", "--repo", "context", "--repo", "context", "--git-url", "https://git.example.com"},
		{"context", "pull", "--repo", "context"}, // no --git-url, no KODEMA_GIT_URL, no ServerURL
	} {
		ports, _, _ := testPorts(nil)
		code := cli.RunWithDeps(context.Background(), cli.Config{}, args, ports, cli.CommandDeps{
			Env: func(string) string { return "" },
		})
		if code != cli.ExitUsage {
			t.Errorf("args %v exit = %d; want %d", args, code, cli.ExitUsage)
		}
	}

	// Workstation pull against matching Kodema HTTPS origin injects origin-scoped
	// credential helper without tokens in argv, and skips injection when
	// GIT_CONFIG_COUNT is already set (in-container runtime configuration).
	var recorded [][]string
	fakeGit := func(_ context.Context, _ string, args []string) (string, error) {
		recorded = append(recorded, append([]string(nil), args...))
		return "", fmt.Errorf("stop after capture")
	}
	cfg := cli.Config{
		ServerURL: "https://kodema.example.com",
		CAFile:    "/etc/kodema/ca.pem",
	}
	ports, _, _ := testPorts(nil)
	ports.Files = newFakeFS()
	_ = cli.RunWithDeps(context.Background(), cfg, []string{"context", "pull", "--repo", "context"}, ports, cli.CommandDeps{
		GitRun:      fakeGit,
		CommandPath: "/usr/local/bin/kodema",
		Env:         func(string) string { return "" },
	})
	if len(recorded) == 0 {
		t.Fatal("fakeGit was not called")
	}
	joined := strings.Join(recorded[0], " ")
	if !strings.Contains(joined, "credential.https://kodema.example.com.helper=") ||
		!strings.Contains(joined, "git credential") ||
		!strings.Contains(joined, "http.https://kodema.example.com.sslCAInfo=/etc/kodema/ca.pem") {
		t.Fatalf("workstation git args missing origin-scoped config: %v", recorded[0])
	}

	// Inside a container (GIT_CONFIG_COUNT set), no extra -c overrides are added.
	recorded = nil
	_ = cli.RunWithDeps(context.Background(), cfg, []string{"context", "pull", "--repo", "context"}, ports, cli.CommandDeps{
		GitRun: fakeGit,
		Env: func(key string) string {
			switch key {
			case "KODEMA_GIT_URL":
				return "https://kodema.example.com/git"
			case "GIT_CONFIG_COUNT":
				return "2"
			default:
				return ""
			}
		},
	})
	if len(recorded) == 0 || recorded[0][0] != "clone" {
		t.Fatalf("container git args = %v; want direct clone without -c override", recorded)
	}
}

func TestContextListAndShowValidateLocalDirAndShippedExamples(t *testing.T) {
	// Validate the repository's own examples/knowledge directory.
	examplesKnowledge := filepath.Join("..", "..", "..", "examples", "knowledge")
	ports, out, errOut := testPorts(nil)
	if code := cli.RunWithDeps(context.Background(), cli.Config{}, []string{"context", "list", "--dir", examplesKnowledge, "--output", "json"}, ports, cli.CommandDeps{}); code != cli.ExitSuccess {
		t.Fatalf("context list examples/knowledge exit = %d; stderr=%q", code, errOut.String())
	}
	var listed struct {
		Skills []struct {
			Name string `json:"name"`
		} `json:"skills"`
		Docs []struct {
			Path string `json:"path"`
		} `json:"docs"`
	}
	if err := json.Unmarshal(out.Bytes(), &listed); err != nil {
		t.Fatalf("decode context list json: %v\n%s", err, out.String())
	}
	if len(listed.Skills) != 2 || len(listed.Docs) != 2 {
		t.Fatalf("examples/knowledge = %d skills, %d docs; want 2 skills, 2 docs", len(listed.Skills), len(listed.Docs))
	}

	ports, out, errOut = testPorts(nil)
	if code := cli.RunWithDeps(context.Background(), cli.Config{}, []string{"context", "show", "build-and-test", "--dir", examplesKnowledge}, ports, cli.CommandDeps{}); code != cli.ExitSuccess {
		t.Fatalf("context show build-and-test exit = %d; stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "# Skill: build and test") {
		t.Fatalf("context show output unexpected:\n%s", out.String())
	}

	// Local directory containing a symlinked skill or duplicate skill slug refuses.
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "escape.md")
	_ = os.WriteFile(outside, []byte("# escape\n"), 0o600)
	_ = os.MkdirAll(filepath.Join(dir, "skills"), 0o750)
	if err := os.Symlink(outside, filepath.Join(dir, "skills", "bad-link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	ports, _, errOut = testPorts(nil)
	if code := cli.RunWithDeps(context.Background(), cli.Config{}, []string{"context", "list", "--dir", dir}, ports, cli.CommandDeps{}); code != cli.ExitFailure {
		t.Fatalf("symlinked skill list exit = %d; want %d", code, cli.ExitFailure)
	}
	if !strings.Contains(errOut.String(), "symlink") {
		t.Fatalf("symlink error = %q; want symlink diagnostic", errOut.String())
	}

	// Duplicate skill (both skills/dup.md and skills/dup/SKILL.md) refuses.
	dupDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dupDir, "skills", "dup"), 0o750)
	_ = os.WriteFile(filepath.Join(dupDir, "skills", "dup.md"), []byte("# Dup flat\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dupDir, "skills", "dup", "SKILL.md"), []byte("# Dup dir\n"), 0o600)
	ports, _, errOut = testPorts(nil)
	if code := cli.RunWithDeps(context.Background(), cli.Config{}, []string{"context", "list", "--dir", dupDir}, ports, cli.CommandDeps{}); code != cli.ExitFailure {
		t.Fatalf("duplicate skill list exit = %d; want %d", code, cli.ExitFailure)
	}
	if !strings.Contains(errOut.String(), "duplicate skill") {
		t.Fatalf("duplicate skill error = %q; want duplicate skill diagnostic", errOut.String())
	}
}

func TestContextPullEdgeCasesCleanupYAMLUTF8ModesOverrideAndEmptyJSON(t *testing.T) {
	gitRoot := t.TempDir()

	// Build a context repo exercising:
	// - folded YAML block description (`description: >`)
	// - multi-byte UTF-8 paragraph > 200 runes
	// - frontmatter without description (synthesized description must be injected)
	// - repo-provided `kodema` skill overriding --include-kodema-docs cleanly
	// - executable supporting script (`scripts/run.sh`)
	work := t.TempDir()
	runTestGit(t, work, "init", "--initial-branch=main")
	writeFile := func(rel, content string, mode os.FileMode) {
		full := filepath.Join(work, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	longUTF8 := strings.Repeat("a", 196) + strings.Repeat("é", 10) + " tail words"
	writeFile("skills/yaml-block/SKILL.md", "---\nname: yaml-block\ndescription: >\n  Folded description of the skill\n  spanning two lines.\n---\n\n# YAML block\n\nBody.\n", 0o600)
	writeFile("skills/long-desc.md", "# Long\n\n"+longUTF8+"\n", 0o600)
	writeFile("skills/no-desc/SKILL.md", "---\nname: no-desc\n---\n\n# No Desc\n\nSynthesize this summary.\n", 0o600)
	writeFile("skills/kodema/SKILL.md", "---\nname: kodema\ndescription: Company override of kodema skill\n---\n\n# Company Kodema\n", 0o600)
	writeFile("skills/tooling/SKILL.md", "# Tooling\n\nRun scripts/run.sh.\n", 0o600)
	writeFile("skills/tooling/scripts/run.sh", "#!/bin/sh\necho hi\n", 0o755)
	writeFile("docs/old.md", "# Old doc\n", 0o600)
	runTestGit(t, work, "add", "-A")
	runTestGit(t, work, "commit", "-m", "initial")
	bare := filepath.Join(gitRoot, "context")
	runTestGit(t, work, "clone", "--bare", "--quiet", work, bare)

	fs := newFakeFS()
	// Hand-authored skill outside the pull manifest must be preserved on re-pull.
	fs.files["/work/.agents/skills/hand-authored/SKILL.md"] = []byte("---\nname: hand-authored\ndescription: Keep me\n---\n")

	deps := cli.CommandDeps{
		GitRun: realGitRunner,
		Env: func(key string) string {
			if key == "KODEMA_GIT_URL" {
				return "file://" + filepath.ToSlash(gitRoot)
			}
			return ""
		},
	}

	var out, errOut bytes.Buffer
	ports := cli.Ports{Files: fs, Out: &out, Err: &errOut}
	code := cli.RunWithDeps(context.Background(), cli.Config{}, []string{
		"context", "pull", "--repo", "context", "--dest", "/work", "--agents-md", "AGENTS.md", "--include-kodema-docs", "--output", "json",
	}, ports, deps)
	if code != cli.ExitSuccess {
		t.Fatalf("pull1 exit = %d; stderr=%q", code, errOut.String())
	}

	var summary struct {
		Skills []struct {
			Name        string `json:"name"`
			Source      string `json:"source"`
			Description string `json:"description"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatalf("decode pull1 json: %v\n%s", err, out.String())
	}
	byName := make(map[string]string, len(summary.Skills))
	for _, s := range summary.Skills {
		byName[s.Name] = s.Description
	}
	if byName["yaml-block"] != "Folded description of the skill spanning two lines." {
		t.Fatalf("yaml-block description = %q; want folded prose", byName["yaml-block"])
	}
	if !utf8.ValidString(byName["long-desc"]) || !strings.HasSuffix(byName["long-desc"], "...") {
		t.Fatalf("long-desc description invalid UTF-8 or not truncated: %q", byName["long-desc"])
	}
	if byName["no-desc"] != "Synthesize this summary." {
		t.Fatalf("no-desc description = %q", byName["no-desc"])
	}
	if noDescBytes, err := fs.ReadFile("/work/.agents/skills/no-desc/SKILL.md"); err != nil || !strings.Contains(string(noDescBytes), `description: "Synthesize this summary."`) {
		t.Fatalf("no-desc SKILL.md missing injected description: %q, %v", string(noDescBytes), err)
	}
	if longBytes, err := fs.ReadFile("/work/.agents/skills/long-desc/SKILL.md"); err != nil || !utf8.Valid(longBytes) {
		t.Fatalf("long-desc SKILL.md invalid UTF-8: %v", err)
	}
	if agentsBytes, err := fs.ReadFile("/work/AGENTS.md"); err != nil || !utf8.Valid(agentsBytes) {
		t.Fatalf("AGENTS.md invalid UTF-8: %v", err)
	}
	// Repo's kodema skill overrides --include-kodema-docs without mixing embedded references/.
	if _, err := fs.ReadFile("/work/.agents/skills/kodema/references/overview.md"); err == nil {
		t.Fatal("embedded kodema references were mixed into repo-overridden kodema skill")
	}
	// Executable supporting script preserves +x.
	if mode := fs.modes["/work/.agents/skills/tooling/scripts/run.sh"]; mode&0o111 == 0 {
		t.Fatalf("scripts/run.sh mode = %04o; want executable", mode)
	}

	// Upstream deletes docs/old.md and skills/tooling; re-pull cleans stale files
	// while keeping hand-authored skills untouched.
	runTestGit(t, work, "rm", "-r", "docs/old.md", "skills/tooling")
	runTestGit(t, work, "commit", "-m", "remove stale skill and doc")
	runTestGit(t, work, "push", bare, "main")

	out.Reset()
	errOut.Reset()
	code = cli.RunWithDeps(context.Background(), cli.Config{}, []string{
		"context", "pull", "--repo", "context", "--dest", "/work", "--agents-md", "AGENTS.md",
	}, ports, deps)
	if code != cli.ExitSuccess {
		t.Fatalf("pull2 exit = %d; stderr=%q", code, errOut.String())
	}
	for _, removed := range []string{
		"/work/.agents/skills/tooling/SKILL.md",
		"/work/.agents/skills/tooling/scripts/run.sh",
		"/work/.kodema/context/context/docs/old.md",
	} {
		if _, err := fs.ReadFile(removed); err == nil {
			t.Errorf("stale file %s was not removed on re-pull", removed)
		}
	}
	if _, err := fs.ReadFile("/work/.agents/skills/hand-authored/SKILL.md"); err != nil {
		t.Fatalf("hand-authored skill was removed by re-pull: %v", err)
	}

	// Missing pinned ref yields a clear ref-not-found error (not git checkout path error).
	out.Reset()
	errOut.Reset()
	code = cli.RunWithDeps(context.Background(), cli.Config{}, []string{
		"context", "pull", "--repo", "context@does-not-exist", "--dest", "/work",
	}, ports, deps)
	if code != cli.ExitFailure {
		t.Fatalf("missing ref exit = %d; want %d", code, cli.ExitFailure)
	}
	if strings.Contains(errOut.String(), "does not take a path argument") || !strings.Contains(errOut.String(), "not found") {
		t.Fatalf("missing ref error = %q; want clear ref-not-found message", errOut.String())
	}

	// Empty repo produces JSON [] rather than null for skills and docs.
	makeTestContextRepo(t, filepath.Join(gitRoot, "empty"), map[string]string{
		"notes.txt": "not markdown\n",
	})
	out.Reset()
	errOut.Reset()
	emptyPorts := cli.Ports{Files: newFakeFS(), Out: &out, Err: &errOut}
	code = cli.RunWithDeps(context.Background(), cli.Config{}, []string{
		"context", "pull", "--repo", "empty", "--dest", "/empty-work", "--output", "json",
	}, emptyPorts, deps)
	if code != cli.ExitSuccess {
		t.Fatalf("empty repo pull exit = %d; stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"skills":[]`) || !strings.Contains(out.String(), `"docs":[]`) {
		t.Fatalf("empty repo pull JSON emitted null instead of []: %s", out.String())
	}
}
