package factory

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/test/support/infrastructure/messaging"
)

// TestGitWorkloadUsesPinnedFixtureImage pins the workload image identity: the
// fixture base resolved from the committed Containerfile, never a shared
// mutable tag, never Alpine, and fixture-unique local and registry names for
// every fixture. Cleanup names contain only names this fixture created.
func TestGitWorkloadUsesPinnedFixtureImage(t *testing.T) {
	pinned, err := messaging.PinnedFixtureBase()
	if err != nil {
		t.Fatalf("resolve the pinned fixture base: %v", err)
	}
	if !strings.Contains(pinned.Reference, "buildpack-deps") {
		t.Fatalf("pinned fixture base %q is not the Git-capable SCM base", pinned.Reference)
	}
	if strings.Contains(pinned.Reference, "alpine") {
		t.Fatalf("pinned fixture base %q is Alpine; the workload needs git and curl", pinned.Reference)
	}

	first, err := newGitWorkloadPlan("shard-alpha")
	if err != nil {
		t.Fatalf("build the first workload plan: %v", err)
	}
	second, err := newGitWorkloadPlan("shard-beta")
	if err != nil {
		t.Fatalf("build the second workload plan: %v", err)
	}
	if first.Base.Reference != pinned.Reference {
		t.Fatalf("workload plan base = %q; want the single pinned %q", first.Base.Reference, pinned.Reference)
	}
	if first.BaseDigest != pinned.Digest || first.BaseTag != pinned.Tag {
		t.Fatalf("workload plan base tag/digest = %q/%q; want %q/%q",
			first.BaseTag, first.BaseDigest, pinned.Tag, pinned.Digest)
	}
	if first.LocalTag == second.LocalTag {
		t.Fatalf("two fixtures share the local image name %q", first.LocalTag)
	}
	if first.Repository == second.Repository {
		t.Fatalf("two fixtures share the registry repository %q", first.Repository)
	}
	if !strings.HasPrefix(first.LocalTag, "localhost/kodama-fixture-") {
		t.Fatalf("workload local tag %q is outside the fixture image namespace", first.LocalTag)
	}
	if !strings.HasPrefix(first.Repository, "kodama/fixture-") {
		t.Fatalf("workload repository %q is outside the fixture image namespace", first.Repository)
	}
	if first.Marker == second.Marker {
		t.Fatalf("two fixtures share the workload marker %q", first.Marker)
	}

	image := GitWorkloadImage{
		LocalTag:          first.LocalTag,
		TagReference:      "registry.example.test:5000/" + first.Repository + ":workload",
		RunnableReference: "registry.example.test:5000/" + first.Repository + "@" + messaging.DigestOf([]byte("workload")),
		ManifestDigest:    messaging.DigestOf([]byte("workload")),
		BaseReference:     first.Base.Reference,
	}
	names := image.cleanupNames()
	if len(names) != 3 {
		t.Fatalf("cleanup names = %v; want the local tag, the push tag and the runnable reference", names)
	}
	for _, name := range names {
		if name == pinned.Reference || name == pinned.Tag || name == messaging.FixtureBaseTag {
			t.Fatalf("cleanup would remove the shared base %q", name)
		}
		if !strings.Contains(name, "kodama/fixture-") && !strings.HasPrefix(name, "localhost/kodama-fixture-") {
			t.Fatalf("cleanup name %q is outside the fixture image namespace", name)
		}
	}

	if _, err := newGitWorkloadPlan(" \t"); err == nil {
		t.Fatalf("an empty fixture identity produced a workload plan")
	}

	// A container can reach the host only through the workload alias that
	// rootless pasta maps to the relay bind. A loopback origin is the mutant
	// that makes a real workload clone/push fail.
	workloadURL, err := workloadGitURL(41000)
	if err != nil {
		t.Fatalf("build the workload Git URL: %v", err)
	}
	if workloadURL != "https://host.containers.internal:41000" {
		t.Fatalf("workload Git URL = %q; want the host workload alias", workloadURL)
	}
	if strings.Contains(workloadURL, "127.0.0.1") || strings.Contains(workloadURL, "localhost") {
		t.Fatalf("workload Git URL %q advertises loopback", workloadURL)
	}
	for _, port := range []int{0, -1, 65536} {
		if _, err := workloadGitURL(port); err == nil {
			t.Fatalf("workload Git URL accepted relay port %d", port)
		}
	}
}

// TestGitWorkloadAuthenticationDoesNotPersistCredentials pins the authored
// workload commands: the public CA is materialized in /workspace through the
// commands themselves, every Git authentication reads the mounted token and
// supplies it only through --config-env, TLS verification is never disabled,
// redirects and tracing are off, and no token is written into a URL, Git
// configuration or the pipeline YAML.
func TestGitWorkloadAuthenticationDoesNotPersistCredentials(t *testing.T) {
	ca := newTLSFixture(t, "host.containers.internal").CAPEM
	commands, err := workloadClonePushCommands(workloadGitSpec{
		URL:       "https://host.containers.internal:41000/" + GitProjectRepository + ".git",
		CAPEM:     ca,
		WorkRef:   GitWorkloadRef,
		Directory: "/workspace/git-workload-repository",
	})
	if err != nil {
		t.Fatalf("author the workload commands: %v", err)
	}
	joined := strings.Join(commands, "\n")

	required := []string{
		"--config-env=http.extraHeader=KODAMA_GIT_AUTHORIZATION",
		"http.followRedirects=false",
		"GIT_SSL_CAINFO=",
		"--cacert",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_TRACE=0",
		"GIT_CURL_VERBOSE=0",
		gitWorkloadTokenPath,
		"base64 -w0",
		"https://host.containers.internal:41000/",
		"repository-config-clean",
		"protected-ref-refused",
		"--exclude=",
		"scan_workspace_for_credential",
	}
	for _, value := range required {
		if !strings.Contains(joined, value) {
			t.Fatalf("authored workload commands do not contain %q:\n%s", value, joined)
		}
	}

	forbidden := []string{
		"http.sslVerify=false",
		"GIT_SSL_NO_VERIFY",
		"curl -k",
		"curl --insecure",
		"GIT_TRACE=1",
		"GIT_CURL_VERBOSE=1",
		"set -x",
		"git clone",
	}
	for _, value := range forbidden {
		if strings.Contains(joined, value) {
			t.Fatalf("authored workload commands contain unsafe control %q:\n%s", value, joined)
		}
	}
	if strings.Contains(joined, "KODAMA_TOKEN=") {
		t.Fatalf("authored workload commands assign a token value:\n%s", joined)
	}
	if strings.Contains(joined, "Authorization: Basic ") && !strings.Contains(joined, "Authorization: Basic $(printf") {
		t.Fatalf("authored workload commands carry a literal Basic header:\n%s", joined)
	}
	if !strings.Contains(string(ca), "BEGIN CERTIFICATE") || !strings.Contains(joined, "BEGIN CERTIFICATE") {
		t.Fatalf("the public CA certificate was not materialized by the commands")
	}

	unsafe := []struct {
		name string
		spec workloadGitSpec
	}{
		{
			name: "plain http",
			spec: workloadGitSpec{URL: "http://host.containers.internal:41000/repository.git", CAPEM: ca, WorkRef: GitWorkloadRef, Directory: "/workspace/repository"},
		},
		{
			name: "url credentials",
			spec: workloadGitSpec{URL: "https://kodama:secret@host.containers.internal:41000/repository.git", CAPEM: ca, WorkRef: GitWorkloadRef, Directory: "/workspace/repository"},
		},
		{
			name: "protected ref",
			spec: workloadGitSpec{URL: "https://host.containers.internal:41000/repository.git", CAPEM: ca, WorkRef: "refs/heads/main", Directory: "/workspace/repository"},
		},
		{
			name: "missing CA",
			spec: workloadGitSpec{URL: "https://host.containers.internal:41000/repository.git", WorkRef: GitWorkloadRef, Directory: "/workspace/repository"},
		},
		{
			name: "relative directory",
			spec: workloadGitSpec{URL: "https://host.containers.internal:41000/repository.git", CAPEM: ca, WorkRef: GitWorkloadRef, Directory: "repository"},
		},
	}
	for _, testCase := range unsafe {
		if _, err := workloadClonePushCommands(testCase.spec); err == nil {
			t.Fatalf("%s: authored workload commands; want a refusal", testCase.name)
		}
	}
}

// TestGitWorkloadCleanupRunsAfterShellAndOwnedNamesOnly pins the lifecycle
// order Close must use: fixture workload resources first, then the Git relay
// while the Shell still serves, then the Shell, and only then the image names.
// A failed earlier phase never skips a later cleanup phase.
func TestGitWorkloadCleanupRunsAfterShellAndOwnedNamesOnly(t *testing.T) {
	want := []string{"workload", "edge", "shell", "images"}
	var order []string
	shutdown := gitFixtureShutdown{
		StopWorkload: func(context.Context) error {
			order = append(order, "workload")
			return nil
		},
		CloseEdge: func() error {
			order = append(order, "edge")
			return nil
		},
		CloseShell: func(context.Context) error {
			order = append(order, "shell")
			return nil
		},
		RemoveImages: func(context.Context) error {
			order = append(order, "images")
			return nil
		},
	}
	completed, err := shutdown.run(context.Background())
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if !reflect.DeepEqual(completed, want) {
		t.Fatalf("cleanup order = %v; want %v", completed, want)
	}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("cleanup ran %v; want %v", order, want)
	}

	order = nil
	shutdown.CloseEdge = func() error {
		order = append(order, "edge")
		return errors.New("git run did not join")
	}
	completed, err = shutdown.run(context.Background())
	if err == nil {
		t.Fatalf("a failed edge join was reported as a successful cleanup")
	}
	if !reflect.DeepEqual(completed, want) || !reflect.DeepEqual(order, want) {
		t.Fatalf("cleanup after a failed join ran %v; want every phase in %v", order, want)
	}
}

// TestGitWorkloadScanFailsClosedOnCredentialLeak proves the in-workload scan
// really fails the action under the product's `#!/bin/sh` + `set -e` contract.
// It runs the rendered scan commands under every POSIX shell this host offers
// against a clean workspace and four planted leaks: a persisted extraHeader,
// the raw token in the clone URL, the Basic encoding in a workspace file and
// the raw token copied to a nested workspace file. A clean tree prints the
// positive control and exits zero; every leak exits 1 without printing it.
// A `grep` shim on PATH records every argument, and no invocation may see the
// token or its Basic encoding.
func TestGitWorkloadScanFailsClosedOnCredentialLeak(t *testing.T) {
	const token = "i04-g6b-scan-token-0123456789abcdef"
	basic := base64.StdEncoding.EncodeToString([]byte("kodama:" + token))
	if basic == token {
		t.Fatalf("the test token has no Basic encoding")
	}

	testCases := []struct {
		name        string
		leak        func(t *testing.T, workspace, repository string)
		wantFailure bool
	}{
		{name: "clean"},
		{
			name: "persisted extraHeader",
			leak: func(t *testing.T, workspace, repository string) {
				writeScanFile(t, filepath.Join(repository, ".git", "config"),
					"[core]\n\trepositoryformatversion = 0\n"+
						"[http]\n\textraHeader = Authorization: Basic "+basic+"\n")
			},
			wantFailure: true,
		},
		{
			// Isolate the marker scan: this value is neither the token nor its
			// Basic encoding, but a persisted extraHeader is itself the defect.
			name: "persisted extraHeader marker",
			leak: func(t *testing.T, workspace, repository string) {
				writeScanFile(t, filepath.Join(repository, ".git", "config"),
					"[core]\n\trepositoryformatversion = 0\n"+
						"[http]\n\textraHeader = Authorization: Basic ZmFrZS1ub3QtdGhlLXRva2Vu\n")
			},
			wantFailure: true,
		},
		{
			name: "raw token in clone URL",
			leak: func(t *testing.T, workspace, repository string) {
				writeScanFile(t, filepath.Join(repository, ".git", "config"),
					"[remote \"origin\"]\n\turl = https://kodama:"+token+
						"@host.containers.internal:41000/project-i04/web.git\n")
			},
			wantFailure: true,
		},
		{
			name: "basic encoding in workspace file",
			leak: func(t *testing.T, workspace, repository string) {
				writeScanFile(t, filepath.Join(workspace, "leak.txt"),
					"Authorization: Basic "+basic+"\n")
			},
			wantFailure: true,
		},
		{
			name: "raw token in nested workspace file",
			leak: func(t *testing.T, workspace, repository string) {
				writeScanFile(t, filepath.Join(workspace, "nested", "leak.txt"), token+"\n")
			},
			wantFailure: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			for _, shell := range availableScanShells(t) {
				t.Run(shell.name, func(t *testing.T) {
					tokenPath, workspace, repository := newScanWorkspace(t, token)
					if testCase.leak != nil {
						testCase.leak(t, workspace, repository)
					}
					shimDir, argvLog := newGrepArgvShim(t)
					script := filepath.Join(t.TempDir(), "scan.sh")
					commands := workloadScanCommands(tokenPath, workspace)
					if err := os.WriteFile(script, []byte(strings.Join(commands, "\n")+"\n"), 0o600); err != nil {
						t.Fatalf("write rendered scan script: %v", err)
					}

					output, code := runScanScript(t, shell, script, shimDir)
					control := strings.Contains(output, "repository-config-clean")
					if testCase.wantFailure {
						if code == 0 {
							t.Fatalf("scan accepted a planted credential leak; want a failing action\n%s", output)
						}
						if code != 1 {
							t.Fatalf("scan of a planted leak exited %d; want the leak status 1\n%s", code, output)
						}
						if control {
							t.Fatalf("scan of a planted leak printed the clean control\n%s", output)
						}
					} else {
						if code != 0 {
							t.Fatalf("clean scan exited %d; want 0\n%s", code, output)
						}
						if !control {
							t.Fatalf("clean scan did not print the positive control\n%s", output)
						}
					}

					logged, err := os.ReadFile(argvLog)
					if err != nil {
						t.Fatalf("read grep argv log: %v", err)
					}
					if len(bytes.TrimSpace(logged)) == 0 {
						t.Fatalf("the scan never invoked the PATH grep shim; the argv proof did not run")
					}
					for _, secret := range []string{token, basic} {
						if bytes.Contains(logged, []byte(secret)) {
							t.Fatalf("a secret entered a process argument (%q):\nthe token must reach grep only on stdin:\n%s",
								secret, logged)
						}
					}
				})
			}
		})
	}
}

// TestGitWorkloadCAHeredocTerminatesWithoutTrailingNewline proves the CA
// materialization closes its heredoc even when the fixture certificate carries
// no trailing newline; otherwise the terminator would be concatenated onto the
// final PEM line and the shell would read the rest of the action as CA bytes.
func TestGitWorkloadCAHeredocTerminatesWithoutTrailingNewline(t *testing.T) {
	ca := newTLSFixture(t, "host.containers.internal").CAPEM
	trimmed := bytes.TrimRight(ca, "\n")
	if len(trimmed) == len(ca) {
		t.Fatalf("the TLS fixture CA already ends without a newline; the guard cannot distinguish the append")
	}
	commands, err := workloadClonePushCommands(workloadGitSpec{
		URL:       "https://host.containers.internal:41000/" + GitProjectRepository + ".git",
		CAPEM:     trimmed,
		WorkRef:   GitWorkloadRef,
		Directory: "/workspace/git-workload-repository",
	})
	if err != nil {
		t.Fatalf("author workload commands with a certificate lacking a trailing newline: %v", err)
	}
	if !strings.HasSuffix(commands[0], "\nKODAMA_WORKLOAD_CA") {
		t.Fatalf("the CA heredoc terminator is not on its own line: %q", commands[0])
	}
}

// scanShell is one POSIX shell the rendered scan commands run under.
type scanShell struct {
	name    string
	command string
	args    []string
}

// availableScanShells returns /bin/sh plus the alternate POSIX shells this host
// provides. The product runs the action script with /bin/sh and set -e; /bin/sh
// is dash on the Debian workload image and may be another POSIX shell here, so
// every available interpreter must fail the same way.
func availableScanShells(t *testing.T) []scanShell {
	t.Helper()
	shells := []scanShell{{name: "sh", command: "/bin/sh", args: []string{"-e"}}}
	for _, candidate := range []scanShell{
		{name: "dash", command: "dash", args: []string{"-e"}},
		{name: "bash-posix", command: "bash", args: []string{"--posix", "-e"}},
	} {
		path, err := exec.LookPath(candidate.command)
		if err != nil {
			continue
		}
		candidate.command = path
		shells = append(shells, candidate)
	}
	return shells
}

// newScanWorkspace builds a clean container-shaped workspace: a repository
// whose configuration has a credential-free origin, the public CA file and the
// token mounted outside the workspace at its product path.
func newScanWorkspace(t *testing.T, token string) (tokenPath, workspace, repository string) {
	t.Helper()
	root := t.TempDir()
	tokenPath = filepath.Join(root, "kodama", "secrets", "KODAMA_TOKEN")
	writeScanFile(t, tokenPath, token+"\n")
	workspace = filepath.Join(root, "workspace")
	repository = filepath.Join(workspace, "git-workload-repository")
	writeScanFile(t, filepath.Join(repository, ".git", "config"),
		"[core]\n\trepositoryformatversion = 0\n"+
			"[remote \"origin\"]\n\turl = https://host.containers.internal:41000/project-i04/web.git\n")
	writeScanFile(t, filepath.Join(repository, "workload-commit.txt"),
		"git workload commit for refs/heads/work/i04-g6b\n")
	writeScanFile(t, filepath.Join(workspace, filepath.Base(gitWorkloadCAFile)),
		"-----BEGIN CERTIFICATE-----\npublic\n-----END CERTIFICATE-----\n")
	return tokenPath, workspace, repository
}

// writeScanFile writes one fixture file, creating its parent directories.
func writeScanFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create scan fixture directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write scan fixture %s: %v", path, err)
	}
}

// newGrepArgvShim installs a logging `grep` shim on PATH and returns its
// directory and argv log. The shim records every argument and then executes the
// real grep, so a token passed as an argument would be visible in the log.
func newGrepArgvShim(t *testing.T) (shimDir, argvLog string) {
	t.Helper()
	realGrep, err := exec.LookPath("grep")
	if err != nil {
		t.Fatalf("find the real grep: %v", err)
	}
	realGrep, err = filepath.Abs(realGrep)
	if err != nil {
		t.Fatalf("resolve the real grep path: %v", err)
	}
	shimDir = filepath.Join(t.TempDir(), "bin")
	argvLog = filepath.Join(t.TempDir(), "grep-argv.log")
	script := "#!/bin/sh\n" +
		"for argument in \"$@\"; do printf '%s\\n' \"$argument\" >> " + shellQuote(argvLog) + "; done\n" +
		"exec " + shellQuote(realGrep) + " \"$@\"\n"
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatalf("create grep shim directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(shimDir, "grep"), []byte(script), 0o755); err != nil {
		t.Fatalf("write grep shim: %v", err)
	}
	return shimDir, argvLog
}

// runScanScript runs one rendered scan script under the given shell with the
// shim first on PATH and returns its combined output and exit status.
func runScanScript(t *testing.T, shell scanShell, script, shimDir string) (string, int) {
	t.Helper()
	args := append(append([]string{}, shell.args...), script)
	command := exec.Command(shell.command, args...)
	command.Env = scanEnv(shimDir)
	output, err := command.CombinedOutput()
	if err == nil {
		return string(output), 0
	}
	if exitError, ok := errors.AsType[*exec.ExitError](err); ok {
		return string(output), exitError.ExitCode()
	}
	t.Fatalf("run the scan under %s: %v\n%s", shell.name, err, output)
	return "", 0
}

// scanEnv returns the process environment with PATH replaced by the shim
// directory followed by the original search path.
func scanEnv(shimDir string) []string {
	environment := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "PATH=") {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment, "PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
