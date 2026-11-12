//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSmokeFixtureScripts runs the S2/S5 fixture scripts under /bin/sh against
// a disposable local bare Git repository with a real rejecting update hook.
// The product paths are overridden through the scripts' documented SMOKE_*
// environment, so the exact bytes the pipelines run are exercised hermetically:
// a wrong base, a wrong candidate/result, a missing marker, a missing task
// snapshot and the unapproved protected push all fail; the prepared path and
// the exact protected integration succeed.
func TestSmokeFixtureScripts(t *testing.T) {
	for _, tool := range []string{"git", "base64", "grep", "sed"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is unavailable; the workload images provide it", tool)
		}
	}
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	runHostGit(t, "", "init", "--bare", "--quiet", remote)
	runHostGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")

	seed := filepath.Join(root, "seed")
	runHostGit(t, "", "init", "--quiet", seed)
	writeFile(t, filepath.Join(seed, "README.md"), "seed\n")
	runHostGit(t, seed, "add", "--", "README.md")
	runHostGit(t, seed, "-c", "user.name=kodama", "-c", "user.email=kodama@example.invalid",
		"commit", "--quiet", "-m", "seed")
	base := strings.TrimSpace(runHostGit(t, seed, "rev-parse", "HEAD"))
	runHostGit(t, seed, "push", "--quiet", remote, base+":refs/heads/main")
	runHostGit(t, seed, "push", "--quiet", remote, base+":refs/heads/work/seed")

	// The protected ref refuses every update with the product's exact public
	// hook message; work refs are accepted.
	hook := filepath.Join(remote, "hooks", "update")
	writeFile(t, hook, "#!/bin/sh\ncase \"$1\" in\nrefs/heads/main) printf 'kodama git: update rejected\\n' >&2; exit 1 ;;\nesac\nexit 0\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatalf("make the update hook executable: %v", err)
	}

	environment := func(workspace string) []string {
		return mergeEnv(os.Environ(),
			"SMOKE_WORKSPACE="+workspace,
			"SMOKE_TOKEN_PATH="+filepath.Join(workspace, "token"),
			"SMOKE_TASK_FILE="+filepath.Join(workspace, "task.json"),
			"SMOKE_INPUTS_ROOT="+filepath.Join(workspace, "inputs"),
			"SMOKE_OUTPUTS_ROOT="+filepath.Join(workspace, "outputs"),
			"SMOKE_CA_FILE=",
			"KODAMA_EXECUTION=execution-hermetic",
			"KODAMA_TASK=task-hermetic",
			"SMOKE_RUN_MARKER=run-hermetic",
		)
	}
	prepareWorkspace := func(t *testing.T) string {
		t.Helper()
		workspace := filepath.Join(root, "workspace-"+strings.ReplaceAll(t.Name(), "/", "-"))
		for _, directory := range []string{workspace, filepath.Join(workspace, "inputs"), filepath.Join(workspace, "outputs", "test")} {
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatalf("create the fixture workspace: %v", err)
			}
		}
		writeFile(t, filepath.Join(workspace, "token"), "hermetic-execution-token\n")
		writeFile(t, filepath.Join(workspace, "task.json"), `{"task_id":"task-hermetic"}`+"\n")
		writeFile(t, filepath.Join(workspace, "inputs", "repository"), "w49-repo-hermetic\n")
		writeFile(t, filepath.Join(workspace, "inputs", "repository_path"), "remote.git\n")
		writeFile(t, filepath.Join(workspace, "inputs", "git_url"), "file://"+root+"\n")
		writeFile(t, filepath.Join(workspace, "inputs", "target_ref"), "refs/heads/main\n")
		writeFile(t, filepath.Join(workspace, "inputs", "expected_base"), base+"\n")
		writeFile(t, filepath.Join(workspace, "inputs", "work_ref"), "refs/heads/work/protected\n")
		return workspace
	}

	t.Run("prepare implements and tests the candidate", func(t *testing.T) {
		workspace := prepareWorkspace(t)
		env := environment(workspace)
		env = mergeEnv(env, "SMOKE_PREPARE_MARKER=prepared-ok", "SMOKE_IMPLEMENT_MARKER=implemented-ok", "SMOKE_TEST_MARKER=tested-ok", "SMOKE_INTEGRATE_MARKER=integrated-ok")
		output := runScript(t, env, smokeGitPrelude+"\n"+protectedPrepareScript)
		if !strings.Contains(output, "prepared-ok") {
			t.Fatalf("prepare did not print its marker: %s", output)
		}
		head := strings.TrimSpace(runHostGit(t, filepath.Join(workspace, "repo"), "rev-parse", "HEAD"))
		if head != base {
			t.Fatalf("prepared checkout HEAD = %q; want the base %q", head, base)
		}
		// A wrong expected base is refused before any marker.
		writeFile(t, filepath.Join(workspace, "inputs", "expected_base"), strings.Repeat("0", 40)+"\n")
		if err := runScriptErr(env, smokeGitPrelude+"\n"+protectedPrepareScript); err == nil {
			t.Fatal("prepare accepted a checkout that was not the expected base")
		}
		writeFile(t, filepath.Join(workspace, "inputs", "expected_base"), base+"\n")

		output = runScript(t, env, smokeGitPrelude+"\n"+protectedImplementScript)
		if !strings.Contains(output, "implemented-ok") {
			t.Fatalf("implement did not print its marker: %s", output)
		}
		for _, name := range []string{"answer.sh", "test.sh"} {
			if _, err := os.Stat(filepath.Join(workspace, "repo", name)); err != nil {
				t.Fatalf("implement did not write %s: %v", name, err)
			}
		}
		// The implement action must refuse a missing shared workspace marker.
		if err := os.Remove(filepath.Join(workspace, "run-marker")); err != nil {
			t.Fatalf("remove the run marker: %v", err)
		}
		if err := runScriptErr(env, smokeGitPrelude+"\n"+protectedImplementScript); err == nil {
			t.Fatal("implement accepted a missing workspace marker")
		}
		writeFile(t, filepath.Join(workspace, "run-marker"), "run-hermetic\n")

		runScript(t, env, smokeGitPrelude+"\n"+protectedTestScript)
		work := strings.TrimSpace(runHostGit(t, remote, "rev-parse", "refs/heads/work/protected"))
		if work == "" || work == base {
			t.Fatalf("the test action did not push a candidate to the work ref")
		}
		main := strings.TrimSpace(runHostGit(t, remote, "rev-parse", "refs/heads/main"))
		if main != base {
			t.Fatalf("the unapproved protected push moved main to %q; want %q", main, base)
		}
		writeFile(t, filepath.Join(workspace, "outputs", "test", "candidate"), work+"\n")
		writeFile(t, filepath.Join(workspace, "outputs", "test", "test_result"), "pass:"+work+"\n")

		// A wrong settled test result is refused before the protected push.
		writeFile(t, filepath.Join(workspace, "outputs", "test", "test_result"), "pass:"+base+"\n")
		if err := runScriptErr(env, smokeGitPrelude+"\n"+protectedIntegrateScript); err == nil {
			t.Fatal("integrate accepted a mismatched test result")
		}
		writeFile(t, filepath.Join(workspace, "outputs", "test", "test_result"), "pass:"+work+"\n")
		// A candidate equal to the base cannot move the protected ref.
		writeFile(t, filepath.Join(workspace, "inputs", "expected_base"), work+"\n")
		if err := runScriptErr(env, smokeGitPrelude+"\n"+protectedIntegrateScript); err == nil {
			t.Fatal("integrate accepted the candidate as its own base")
		}
		writeFile(t, filepath.Join(workspace, "inputs", "expected_base"), base+"\n")

		// The reviewer's pass is modelled by the disposable hook accepting the
		// protected ref; everything before it already proved the refusal.
		writeFile(t, hook, "#!/bin/sh\nexit 0\n")
		if err := os.Chmod(hook, 0o755); err != nil {
			t.Fatalf("replace the disposable update hook: %v", err)
		}
		output = runScript(t, env, smokeGitPrelude+"\n"+protectedIntegrateScript)
		if !strings.Contains(output, "integrated-ok") {
			t.Fatalf("integrate did not print its marker: %s", output)
		}
		main = strings.TrimSpace(runHostGit(t, remote, "rev-parse", "refs/heads/main"))
		if main != work {
			t.Fatalf("protected main = %q; want the candidate %q", main, work)
		}
	})

	t.Run("seed creates the absent protected ref", func(t *testing.T) {
		empty := filepath.Join(root, "empty.git")
		runHostGit(t, "", "init", "--bare", "--quiet", empty)
		seedRepo := filepath.Join(root, "empty-seed")
		runHostGit(t, "", "init", "--quiet", seedRepo)
		writeFile(t, filepath.Join(seedRepo, "seed.txt"), "seed\n")
		runHostGit(t, seedRepo, "add", "--", "seed.txt")
		runHostGit(t, seedRepo, "-c", "user.name=kodama", "-c", "user.email=kodama@example.invalid",
			"commit", "--quiet", "-m", "seed")
		seedCommit := strings.TrimSpace(runHostGit(t, seedRepo, "rev-parse", "HEAD"))
		runHostGit(t, seedRepo, "push", "--quiet", empty, seedCommit+":refs/heads/work/seed")

		workspace := filepath.Join(root, "workspace-seed")
		for _, directory := range []string{workspace, filepath.Join(workspace, "inputs")} {
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatalf("create the seed workspace: %v", err)
			}
		}
		writeFile(t, filepath.Join(workspace, "token"), "hermetic-execution-token\n")
		writeFile(t, filepath.Join(workspace, "inputs", "repository"), "w49-repo-hermetic-empty\n")
		writeFile(t, filepath.Join(workspace, "inputs", "repository_path"), "empty.git\n")
		writeFile(t, filepath.Join(workspace, "inputs", "git_url"), "file://"+root+"\n")
		writeFile(t, filepath.Join(workspace, "inputs", "target_ref"), "refs/heads/main\n")
		writeFile(t, filepath.Join(workspace, "inputs", "expected_base"), "")
		writeFile(t, filepath.Join(workspace, "inputs", "candidate"), seedCommit+"\n")
		writeFile(t, filepath.Join(workspace, "inputs", "work_ref"), "refs/heads/work/seed\n")
		env := mergeEnv(environment(workspace), "SMOKE_SEED_MARKER=seeded-ok")
		output := runScript(t, env, smokeGitPrelude+"\n"+protectedSeedIntegrateScript)
		if !strings.Contains(output, "seeded-ok") {
			t.Fatalf("the seed action did not print its marker: %s", output)
		}
		if got := strings.TrimSpace(runHostGit(t, empty, "rev-parse", "refs/heads/main")); got != seedCommit {
			t.Fatalf("seeded protected ref = %q; want %q", got, seedCommit)
		}
		// A non-empty expected base must not create a new ref.
		writeFile(t, filepath.Join(workspace, "inputs", "expected_base"), seedCommit+"\n")
		if err := runScriptErr(env, smokeGitPrelude+"\n"+protectedSeedIntegrateScript); err == nil {
			t.Fatal("the seed action accepted a non-empty base for an absent ref")
		}
	})

	t.Run("human push scripts prove the push inputs", func(t *testing.T) {
		workspace := prepareWorkspace(t)
		env := mergeEnv(environment(workspace),
			"SMOKE_GIT_URL=file://"+root,
			"SMOKE_REPOSITORY=remote.git",
			"SMOKE_EXECUTION_REF=refs/heads/work/execution-hermetic",
			"SMOKE_PUSHER_MARKER=execution-pushed",
		)
		output := runScript(t, env, smokeGitPrelude+"\n"+humanPushExecutionScript)
		if !strings.Contains(output, "execution-pushed") {
			t.Fatalf("the execution push did not print its marker: %s", output)
		}
		if pushed, ok := smokeLogValue([]byte(output), "w49-pushed-commit"); !ok || pushed == "" {
			t.Fatalf("the execution push did not report its commit: %s", output)
		}

		humanRef := "refs/heads/work/human-hermetic"
		humanRepo := filepath.Join(root, "human")
		runHostGit(t, "", "clone", "--quiet", remote, humanRepo)
		writeFile(t, filepath.Join(humanRepo, "human.txt"), "human\n")
		runHostGit(t, humanRepo, "add", "--", "human.txt")
		runHostGit(t, humanRepo, "-c", "user.name=kodama", "-c", "user.email=kodama@example.invalid",
			"commit", "--quiet", "-m", "human")
		humanCommit := strings.TrimSpace(runHostGit(t, humanRepo, "rev-parse", "HEAD"))
		runHostGit(t, humanRepo, "push", "--quiet", remote, "HEAD:"+humanRef)

		actionInputs := filepath.Join(workspace, "action-inputs")
		if err := os.MkdirAll(actionInputs, 0o700); err != nil {
			t.Fatalf("create the action inputs: %v", err)
		}
		writeFile(t, filepath.Join(actionInputs, "repository"), "w49-repo-hermetic\n")
		writeFile(t, filepath.Join(actionInputs, "ref"), humanRef+"\n")
		writeFile(t, filepath.Join(actionInputs, "commit"), humanCommit+"\n")
		writeFile(t, filepath.Join(actionInputs, "previous_commit"), "")
		actionEnv := mergeEnv(environment(workspace),
			"SMOKE_INPUTS_ROOT="+actionInputs,
			"SMOKE_EXPECT_REPOSITORY=w49-repo-hermetic",
			"SMOKE_EXPECT_REF="+humanRef,
			"SMOKE_EXPECT_COMMIT="+humanCommit,
			"SMOKE_EXPECT_PREVIOUS=",
			"SMOKE_PUSH_ACTION_MARKER=push-inputs-ok",
		)
		output = runScript(t, actionEnv, smokeGitPrelude+"\n"+humanPushActionScript)
		if !strings.Contains(output, "push-inputs-ok") {
			t.Fatalf("the subscribed action did not print its marker: %s", output)
		}
		// Another commit is refused, so the action cannot prove a push it did
		// not receive.
		wrong := mergeEnv(actionEnv, "SMOKE_EXPECT_COMMIT="+base)
		if err := runScriptErr(wrong, smokeGitPrelude+"\n"+humanPushActionScript); err == nil {
			t.Fatal("the subscribed action accepted another commit's inputs")
		}
	})
}

// mergeEnv replaces the named variables in base, so a fixture override never
// reaches a script twice with two different values.
func mergeEnv(base []string, overrides ...string) []string {
	replaced := map[string]bool{}
	for _, entry := range overrides {
		replaced[strings.SplitN(entry, "=", 2)[0]] = true
	}
	merged := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		if !replaced[strings.SplitN(entry, "=", 2)[0]] {
			merged = append(merged, entry)
		}
	}
	return append(merged, overrides...)
}

// runHostGit runs one real host Git command in a disposable directory.
func runHostGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	if directory != "" {
		command.Dir = directory
	}
	command.Env = mergeEnv(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

// runScript runs one prelude+script under /bin/sh and returns its combined
// output.
func runScript(t *testing.T, env []string, script string) string {
	t.Helper()
	command := exec.Command("/bin/sh", "-c", script)
	command.Env = env
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture script failed: %v\n%s", err, output)
	}
	return string(output)
}

// runScriptErr runs one prelude+script and returns its error without failing.
func runScriptErr(env []string, script string) error {
	command := exec.Command("/bin/sh", "-c", script)
	command.Env = env
	return command.Run()
}

// writeFile writes one hermetic fixture file.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestSmokeDynamicOutputSubmission proves the Python output helper is reused
// with the workload-computed candidate substituted after the candidate exists,
// and that the generated helper still runs through python3.
func TestSmokeDynamicOutputSubmission(t *testing.T) {
	commands := smokeOutputSubmissionCommands(t,
		map[string]string{"candidate": "__W49_CANDIDATE__", "test_result": "pass:__W49_CANDIDATE__"},
		"",
		[]smokeSubstitution{{Placeholder: "__W49_CANDIDATE__", Shell: "$candidate"}},
	)
	if len(commands) != 3 {
		t.Fatalf("submission commands = %d; want write, substitution and run", len(commands))
	}
	if !strings.Contains(commands[0], "__W49_CANDIDATE__") {
		t.Fatal("the generated helper does not carry the placeholder")
	}
	if !strings.Contains(commands[1], "sed -i") || !strings.Contains(commands[1], `$candidate`) {
		t.Fatal("the substitution command does not rewrite the placeholder from the candidate")
	}
	if !strings.HasPrefix(commands[2], "python3 ") {
		t.Fatal("the final command does not run the helper")
	}

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is unavailable")
	}
	bin := t.TempDir()
	tmpDir := t.TempDir()
	fake := filepath.Join(bin, "python3")
	writeFile(t, fake, "#!/bin/sh\ngrep -q 'candidate-value' \"$1\"\n")
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatalf("make the fake python3 executable: %v", err)
	}
	script := "candidate=candidate-value\n" + strings.Join(commands, "\n")
	command := exec.Command("/bin/sh", "-c", script)
	command.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "TMPDIR="+tmpDir)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("the generated submission command failed: %v\n%s", err, output)
	}
	if entries, err := os.ReadDir(tmpDir); err != nil || len(entries) != 0 {
		t.Fatalf("submission helper left files in TMPDIR: entries=%v err=%v", entries, err)
	}
}
