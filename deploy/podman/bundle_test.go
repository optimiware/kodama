package podman

// These tests pin the static and pure-shell contract of the local Podman
// install bundle: the scripts exist and parse, the install keeps every
// secret out of argv, down removes only resources this bundle labelled and
// purges only a matching state directory, the state-path and marker rules
// refuse degenerate or foreign directories, image defaults are digest-pinned,
// the defaults are configurable, and the quickstart carries the release
// limitations and a resolvable link set. The real install proof is a scripted
// run on one host, not a unit test.

import (
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func bundleRoot(t *testing.T) string {
	t.Helper()
	return "."
}

func repoRoot(t *testing.T) string {
	t.Helper()
	absolute, err := filepath.Abs(filepath.Join(bundleRoot(t), "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func bashPath(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"/bin/bash", "/usr/bin/bash", "/usr/local/bin/bash"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	t.Skip("bash is not installed")
	return ""
}

// runBash runs one bash script with a controlled environment and returns its
// streams and exit code. It uses os.StartProcess because a neutral package may
// not import os/exec.
func runBash(t *testing.T, env map[string]string, script string, args ...string) (string, string, int) {
	t.Helper()
	bash := bashPath(t)
	merged := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			merged[key] = value
		}
	}
	delete(merged, "XDG_STATE_HOME")
	delete(merged, "KODAMA_LOCAL_STATE_DIR")
	maps.Copy(merged, env)
	environment := make([]string, 0, len(merged))
	for key, value := range merged {
		environment = append(environment, key+"="+value)
	}
	outReader, outWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errReader, errWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	argv := append([]string{"bash", "-c", script, "bash"}, args...)
	process, err := os.StartProcess(bash, argv, &os.ProcAttr{
		Env:   environment,
		Files: []*os.File{os.Stdin, outWriter, errWriter},
	})
	if err != nil {
		t.Fatalf("start bash: %v", err)
	}
	_ = outWriter.Close()
	_ = errWriter.Close()
	outBytes, _ := io.ReadAll(outReader)
	errBytes, _ := io.ReadAll(errReader)
	state, waitErr := process.Wait()
	if waitErr != nil {
		t.Fatalf("wait bash: %v", waitErr)
	}
	return string(outBytes), string(errBytes), state.ExitCode()
}

// shellEnv returns a minimal environment for sourcing lib.sh in tests.
func shellEnv(t *testing.T, prefix string) map[string]string {
	t.Helper()
	home := t.TempDir()
	state := filepath.Join(home, "state")
	return map[string]string{
		"HOME":                   home,
		"KODAMA_LOCAL_PREFIX":    prefix,
		"KODAMA_LOCAL_STATE_DIR": state,
	}
}

func TestBundleScriptsExistAndAreExecutable(t *testing.T) {
	for _, name := range []string{"install.sh", "status.sh", "down.sh", "lib.sh", "smoke.sh"} {
		path := filepath.Join(bundleRoot(t), name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s is missing: %v", name, err)
		}
		if info.IsDir() {
			t.Fatalf("%s is a directory", name)
		}
		if name != "lib.sh" && info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s is not executable (mode %04o)", name, info.Mode().Perm())
		}
	}
}

func TestBundleScriptsParseWithBash(t *testing.T) {
	bash := bashPath(t)
	for _, name := range []string{"install.sh", "status.sh", "down.sh", "lib.sh", "smoke.sh"} {
		path := filepath.Join(bundleRoot(t), name)
		process, err := os.StartProcess(bash, []string{"bash", "-n", path}, &os.ProcAttr{
			Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		})
		if err != nil {
			t.Fatalf("start bash -n %s: %v", name, err)
		}
		state, err := process.Wait()
		if err != nil {
			t.Fatalf("wait bash -n %s: %v", name, err)
		}
		if !state.Success() {
			t.Fatalf("%s does not parse (exit %d)", name, state.ExitCode())
		}
	}
}

// TestInstallKeepsSecretsOutOfArgv pins that the install writes secrets to
// private files and hands them to Podman only through --env-file or stdin,
// never as an argument. An argument is visible in `ps` and /proc/<pid>/cmdline.
func TestInstallKeepsSecretsOutOfArgv(t *testing.T) {
	install := readFile(t, filepath.Join(bundleRoot(t), "install.sh"))
	if strings.Contains(install, "set -x") {
		t.Fatal("install.sh enables shell tracing, which would print secrets")
	}
	forbidden := []string{
		"POSTGRES_PASSWORD=",
		"KODAMA_ADMIN_DATABASE_URL=",
		"KODAMA_DATABASE_URL=",
		"OAUTHSONAS_ACCESS_CODE=",
	}
	for _, pattern := range forbidden {
		for line := range strings.SplitSeq(install, "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "podman ") {
				continue
			}
			if strings.Contains(trimmed, pattern) {
				t.Fatalf("install.sh passes a secret assignment on a podman argv: %s", trimmed)
			}
		}
	}
	if !strings.Contains(install, "--env-file") {
		t.Fatal("install.sh does not hand secrets to containers through --env-file")
	}
	if !strings.Contains(install, "umask 077") {
		t.Fatal("install.sh does not make generated files private by default")
	}
	if !strings.Contains(install, "chmod 600") {
		t.Fatal("install.sh does not pin 0600 on its credential files")
	}
}

// TestDownRemovesOnlyOwnResources pins the cleanup scope: every container this
// bundle starts carries the installation label, down removes by that label and
// its own pid file, purges only a matching marker and known children, and never
// prunes the host or removes a computed path with rm -rf.
func TestDownRemovesOnlyOwnResources(t *testing.T) {
	down := readFile(t, filepath.Join(bundleRoot(t), "down.sh"))
	if !strings.Contains(down, "label=kodama.local.prefix=") {
		t.Fatal("down.sh does not select containers by the installation label")
	}
	for _, forbidden := range []string{
		"system prune",
		"container prune",
		"volume prune",
		"image prune",
		"podman rm -a",
		"podman rm --all",
		"podman stop -a",
		"podman volume rm -a",
		"podman volume prune",
		`rm -rf -- "$STATE_DIR"`,
		`rm -rf "$STATE_DIR"`,
	} {
		if strings.Contains(down, forbidden) {
			t.Fatalf("down.sh contains a host-wide or whole-state cleanup command: %q", forbidden)
		}
	}
	if !strings.Contains(down, `check_state_dir "$STATE_DIR" "$PREFIX" purge`) {
		t.Fatal("down.sh does not require the purge marker to match the prefix")
	}
	if !strings.Contains(down, "rmdir -- \"$STATE_DIR\"") {
		t.Fatal("down.sh does not remove the state directory with rmdir after known children")
	}
	if !strings.Contains(down, "OPENSSL-LISTEN:${OIDC_PORT},") {
		t.Fatal("down.sh does not anchor the TLS terminator check to this installation's listen port")
	}
}

func TestInstallAndStatusAreConfigurable(t *testing.T) {
	install := readFile(t, filepath.Join(bundleRoot(t), "install.sh"))
	status := readFile(t, filepath.Join(bundleRoot(t), "status.sh"))
	for _, name := range []string{
		"KODAMA_LOCAL_PREFIX",
		"KODAMA_LOCAL_STATE_DIR",
		"KODAMA_LOCAL_HTTPS_PORT",
		"KODAMA_LOCAL_OIDC_PORT",
		"KODAMA_LOCAL_GIT_PORT",
		"KODAMA_LOCAL_GIT_HTTPS_PORT",
		"KODAMA_LOCAL_POSTGRES_PORT",
		"KODAMA_LOCAL_NATS_PORT",
		"KODAMA_LOCAL_TENANT",
		"KODAMA_LOCAL_WORKLOAD_IMAGE",
	} {
		if !strings.Contains(install, name) || !strings.Contains(status, name) {
			t.Fatalf("%s is not documented as configurable by install.sh and status.sh", name)
		}
	}
	if !strings.Contains(status, "/readyz") {
		t.Fatal("status.sh does not probe /readyz")
	}
}

// TestBundleDefaultsAreDigestPinned pins every credential-bearing image to
// tag@sha256 and the prefix-dependent default state directory.
func TestBundleDefaultsAreDigestPinned(t *testing.T) {
	lib := readFile(t, filepath.Join(bundleRoot(t), "lib.sh"))
	pinPattern := regexp.MustCompile(`@sha256:[a-f0-9]{64}`)
	for _, name := range []string{
		"docker.io/library/postgres:18-alpine@sha256:",
		"docker.io/library/nats:2.15-alpine@sha256:",
		"ghcr.io/optimiware/oauthsonas:1.5.0@sha256:",
		"docker.io/library/buildpack-deps:trixie-scm@sha256:",
		"docker.io/library/alpine:3.24@sha256:",
	} {
		index := strings.Index(lib, name)
		if index < 0 {
			t.Fatalf("lib.sh does not default to the pinned image %q", name)
		}
		line := lib[index:]
		if newline := strings.IndexByte(line, '\n'); newline >= 0 {
			line = line[:newline]
		}
		if !pinPattern.MatchString(line) {
			t.Fatalf("image default is not digest-pinned: %s", line)
		}
	}
	if strings.Contains(lib, ":latest") {
		t.Fatal("lib.sh uses a :latest image tag")
	}
	if !strings.Contains(lib, "kodama/podman/${PREFIX}") {
		t.Fatal("the default state directory does not include the installation prefix")
	}
	for _, portVariable := range []string{
		"KODAMA_LOCAL_HTTPS_PORT",
		"KODAMA_LOCAL_OIDC_PORT",
		"KODAMA_LOCAL_GIT_PORT",
		"KODAMA_LOCAL_POSTGRES_PORT",
		"KODAMA_LOCAL_NATS_PORT",
	} {
		if !strings.Contains(lib, portVariable) {
			t.Fatalf("lib.sh does not declare %s", portVariable)
		}
	}
}

// TestImagePinRules exercises validate_image_pin through bash.
func TestImagePinRules(t *testing.T) {
	lib := filepath.Join(bundleRoot(t), "lib.sh")
	env := shellEnv(t, "pin-test")
	script := `set -euo pipefail
. "$1"
shift
if validate_image_pin "$1"; then echo pinned; else echo refused; exit 1; fi`
	cases := []struct {
		reference string
		ok        bool
	}{
		{"docker.io/library/nats:2.15-alpine@sha256:" + strings.Repeat("a", 64), true},
		{"docker.io/library/nats@sha256:" + strings.Repeat("b", 64), true},
		{"docker.io/library/nats:2.15-alpine", false},
		{"docker.io/library/nats:latest", false},
		{"docker.io/library/nats:2.15-alpine@sha256:short", false},
		{"nats@sha256:" + strings.Repeat("c", 63), false},
	}
	for _, testCase := range cases {
		out, errOut, code := runBash(t, env, script, lib, testCase.reference)
		if testCase.ok && code != 0 {
			t.Fatalf("%q was refused: %s %s", testCase.reference, out, errOut)
		}
		if !testCase.ok && code == 0 {
			t.Fatalf("%q was accepted as pinned", testCase.reference)
		}
	}
}

// TestGitResourcesValidation pins the comma-separated mapping parser.
func TestGitResourcesValidation(t *testing.T) {
	lib := filepath.Join(bundleRoot(t), "lib.sh")
	script := `set -euo pipefail
. "$1"`
	cases := []struct {
		value string
		ok    bool
	}{
		{"admin=git-admin", true},
		{"admin=git-admin,demo=git-demo", true},
		{",a=b", false},
		{"a=b,", false},
		{"a=b,,c=d", false},
		{"admin", false},
	}
	for _, testCase := range cases {
		env := shellEnv(t, "resources-test")
		env["KODAMA_LOCAL_GIT_RESOURCES"] = testCase.value
		_, _, code := runBash(t, env, script, lib)
		if testCase.ok && code != 0 {
			t.Fatalf("valid GIT_RESOURCES %q was refused", testCase.value)
		}
		if !testCase.ok && code == 0 {
			t.Fatalf("invalid GIT_RESOURCES %q was accepted", testCase.value)
		}
	}
}

// TestStatePathValidation pins every degenerate state directory refusal.
func TestStatePathValidation(t *testing.T) {
	lib := filepath.Join(bundleRoot(t), "lib.sh")
	env := shellEnv(t, "path-test")
	home := env["HOME"]
	script := `set -euo pipefail
. "$1"
shift
if validate_state_path "$1"; then exit 0; else exit 1; fi`
	refused := []string{"/", "//", "", "relative/dir", home, filepath.Dir(home)}
	for _, path := range refused {
		if _, _, code := runBash(t, env, script, lib, path); code == 0 {
			t.Fatalf("validate_state_path accepted the degenerate path %q", path)
		}
	}
	valid := filepath.Join(home, "state", "install")
	out, errOut, code := runBash(t, env, script, lib, valid)
	if code != 0 {
		t.Fatalf("validate_state_path refused %q: %s %s", valid, out, errOut)
	}
}

// TestStateMarkerRules pins the marker prefix and purge rules.
func TestStateMarkerRules(t *testing.T) {
	lib := filepath.Join(bundleRoot(t), "lib.sh")
	env := shellEnv(t, "marker-test")
	script := `set -euo pipefail
. "$1"
shift
if check_state_dir "$1" "$2" "$3"; then exit 0; else exit 1; fi`

	empty := t.TempDir()
	if _, _, code := runBash(t, env, script, lib, empty, "marker-test", "install"); code != 0 {
		t.Fatal("an empty directory is not accepted for install")
	}
	if _, _, code := runBash(t, env, script, lib, empty, "marker-test", "purge"); code == 0 {
		t.Fatal("an empty directory is accepted for purge")
	}

	foreign := t.TempDir()
	if err := os.WriteFile(filepath.Join(foreign, ".kodama-local"), []byte("prefix=other-install\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runBash(t, env, script, lib, foreign, "marker-test", "install"); code == 0 {
		t.Fatal("a foreign marker is accepted for install")
	}
	if _, _, code := runBash(t, env, script, lib, foreign, "marker-test", "purge"); code == 0 {
		t.Fatal("a foreign marker is accepted for purge")
	}

	matching := t.TempDir()
	if err := os.WriteFile(filepath.Join(matching, ".kodama-local"), []byte("prefix=marker-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runBash(t, env, script, lib, matching, "marker-test", "install"); code != 0 {
		t.Fatal("a matching marker is refused for install")
	}
	if _, _, code := runBash(t, env, script, lib, matching, "marker-test", "purge"); code != 0 {
		t.Fatal("a matching marker is refused for purge")
	}

	unmarked := t.TempDir()
	if err := os.WriteFile(filepath.Join(unmarked, "data"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runBash(t, env, script, lib, unmarked, "marker-test", "install"); code == 0 {
		t.Fatal("a non-empty directory without a marker is accepted for install")
	}

	multi := t.TempDir()
	if err := os.WriteFile(filepath.Join(multi, ".kodama-local"), []byte("prefix=marker-test\nextra\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runBash(t, env, script, lib, multi, "marker-test", "install"); code == 0 {
		t.Fatal("a marker with extra content is accepted for install")
	}
	if _, _, code := runBash(t, env, script, lib, multi, "marker-test", "purge"); code == 0 {
		t.Fatal("a marker with extra content is accepted for purge")
	}
}

// TestInstallConfiguresDiscoveryEndpoints pins that install.sh wires every
// KODAMA_DISCOVERY_* setting into the server environment so
// GET /.well-known/kodama-configuration is served and interactive login works.
func TestInstallConfiguresDiscoveryEndpoints(t *testing.T) {
	install := readFile(t, filepath.Join(bundleRoot(t), "install.sh"))
	for _, entry := range []string{
		"KODAMA_DISCOVERY_ISSUER=${OIDC_ISSUER}",
		"KODAMA_DISCOVERY_CLIENT_ID=${OIDC_CLIENT_ID}",
		"KODAMA_DISCOVERY_AUDIENCE=${OIDC_AUDIENCE}",
		"KODAMA_DISCOVERY_AUTHORIZATION_ENDPOINT=${DISCOVERY_AUTHORIZATION_ENDPOINT}",
		"KODAMA_DISCOVERY_TOKEN_ENDPOINT=${DISCOVERY_TOKEN_ENDPOINT}",
		"KODAMA_DISCOVERY_DEVICE_AUTHORIZATION_ENDPOINT=${DISCOVERY_DEVICE_AUTHORIZATION_ENDPOINT}",
		"KODAMA_DISCOVERY_REVOCATION_ENDPOINT=${DISCOVERY_REVOCATION_ENDPOINT}",
		"KODAMA_DISCOVERY_SCOPES=${DISCOVERY_SCOPES}",
		"KODAMA_DISCOVERY_API_URL=${PUBLIC_URL}",
	} {
		if !strings.Contains(install, entry) {
			t.Fatalf("install.sh omits %q from the server environment", entry)
		}
	}

	lib := filepath.Join(bundleRoot(t), "lib.sh")
	env := shellEnv(t, "discovery-test")
	script := `set -euo pipefail
. "$1"
printf '%s\n%s\n%s\n%s\n%s\n%s\n' \
  "$OIDC_ISSUER" \
  "$DISCOVERY_AUTHORIZATION_ENDPOINT" \
  "$DISCOVERY_TOKEN_ENDPOINT" \
  "$DISCOVERY_DEVICE_AUTHORIZATION_ENDPOINT" \
  "$DISCOVERY_REVOCATION_ENDPOINT" \
  "$DISCOVERY_SCOPES"`
	out, errOut, code := runBash(t, env, script, lib)
	if code != 0 {
		t.Fatalf("sourcing lib.sh failed: %s %s", out, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 6 {
		t.Fatalf("expected 6 discovery lines, got %v", lines)
	}
	issuer := lines[0]
	for index, endpoint := range lines[1:5] {
		if !strings.HasPrefix(endpoint, issuer+"/") {
			t.Fatalf("discovery endpoint %d (%q) does not share issuer origin %q", index, endpoint, issuer)
		}
	}
	if lines[1] != issuer+"/oauth2/auth" || lines[2] != issuer+"/oauth2/token" {
		t.Fatalf("unexpected OAuthSonas authorization/token endpoints: %v", lines[1:3])
	}
	if lines[5] == "" {
		t.Fatal("DISCOVERY_SCOPES is empty")
	}
}

// TestDefaultStateDirIncludesPrefix pins the prefix-dependent default.
func TestDefaultStateDirIncludesPrefix(t *testing.T) {
	lib := filepath.Join(bundleRoot(t), "lib.sh")
	home := t.TempDir()
	env := map[string]string{
		"HOME":                home,
		"KODAMA_LOCAL_PREFIX": "prefix-test",
	}
	script := `set -euo pipefail
. "$1"
printf '%s\n' "$STATE_DIR"`
	out, errOut, code := runBash(t, env, script, lib)
	if code != 0 {
		t.Fatalf("sourcing lib.sh failed: %s %s", out, errOut)
	}
	want := filepath.Join(home, ".local/state/kodama/podman/prefix-test")
	if strings.TrimSpace(out) != want {
		t.Fatalf("default state directory = %q; want %q", strings.TrimSpace(out), want)
	}
}

// TestCredentialRefreshIsSupported pins the rotation path and its documentation.
func TestCredentialRefreshIsSupported(t *testing.T) {
	install := readFile(t, filepath.Join(bundleRoot(t), "install.sh"))
	for _, phrase := range []string{
		"--refresh-credentials",
		"service.credential.rotate",
		"token_valid",
		"28800",
	} {
		if !strings.Contains(install, phrase) {
			t.Fatalf("install.sh does not implement the credential refresh path: missing %q", phrase)
		}
	}
	lib := readFile(t, filepath.Join(bundleRoot(t), "lib.sh"))
	if !strings.Contains(lib, "service-versions") {
		t.Fatal("lib.sh does not place the service-version state under the state directory")
	}
	runbook := readFile(t, filepath.Join(repoRoot(t), "docs", "runbooks", "podman-quickstart.md"))
	for _, phrase := range []string{
		"install.sh --refresh-credentials",
		"8 h",
		"rotate",
	} {
		if !strings.Contains(runbook, phrase) {
			t.Fatalf("the quickstart does not document credential refresh: missing %q", phrase)
		}
	}
}

func TestBundleIsLinkedFromDeployAndKodama(t *testing.T) {
	deployReadme := readFile(t, filepath.Join(repoRoot(t), "deploy", "README.md"))
	if !strings.Contains(deployReadme, "docs/runbooks/podman-quickstart.md") {
		t.Fatal("deploy/README.md does not link the Podman quickstart")
	}
	product := readFile(t, filepath.Join(repoRoot(t), "KODAMA.md"))
	if !strings.Contains(product, "docs/runbooks/podman-quickstart.md") {
		t.Fatal("KODAMA.md does not link the Podman quickstart from the test environment")
	}
}

// quickstartSection returns the exact text of one top-level quickstart section
// (heading through the line before the next `## ` heading).
func quickstartSection(t *testing.T, title string) string {
	t.Helper()
	text := readFile(t, filepath.Join(repoRoot(t), "docs", "runbooks", "podman-quickstart.md"))
	heading := "## " + title
	start := strings.Index(text, heading)
	if start < 0 {
		t.Fatalf("the quickstart has no %q section", heading)
	}
	rest := text[start+len(heading):]
	if next := regexp.MustCompile(`\n## `).FindStringIndex(rest); next != nil {
		rest = rest[:next[0]+1]
	}
	return heading + rest
}

var repositoryDeclarationPattern = regexp.MustCompile(`^repo[[:space:]]+([^[:space:]]+)[[:space:]]*$`)

// repositoryRedeclarationFindings reports fenced commands that write a second
// `repo <name>` block after `git provision-repository` already declared it.
// A plain quotation of the committed file (for example `cat conf/git.conf`)
// is inspection, not a second declaration, and is not reported.
func repositoryRedeclarationFindings(text string) []string {
	provision := strings.Index(text, "git provision-repository")
	if provision < 0 {
		return nil
	}
	rest := text[provision:]
	repository := regexp.MustCompile(`--repository[[:space:]]+([A-Za-z0-9._/-]+)`).FindStringSubmatch(rest)
	if repository == nil {
		return nil
	}
	name := repository[1]
	var findings []string
	for _, block := range shellBlocks(rest) {
		declares, writes := false, false
		for offset, line := range block.lines {
			trimmed := strings.TrimSpace(line)
			if match := repositoryDeclarationPattern.FindStringSubmatch(trimmed); match != nil && match[1] == name {
				declares = true
			}
			if (strings.Contains(trimmed, "echo") || strings.Contains(trimmed, "printf") || strings.Contains(trimmed, "tee")) &&
				strings.Contains(trimmed, "repo "+name) {
				findings = append(findings, fmt.Sprintf("block at line %d echoes a second repo %q declaration", block.start+offset+1, name))
			}
		}
		blockText := strings.Join(block.lines, "\n")
		if strings.Contains(blockText, "<<") ||
			(strings.Contains(blockText, "conf/git.conf") && strings.Contains(blockText, ">")) {
			writes = true
		}
		if declares && writes {
			findings = append(findings, fmt.Sprintf("block at line %d writes a second repo %q declaration", block.start, name))
		}
	}
	return findings
}

// TestQuickstartNeverRedeclaresTheProvisionedRepository pins that
// `git provision-repository` already appends the default `repo <name>` block to
// the admin policy, so the quickstart must never show a second declaration of
// the same repository (heredoc or echo) after the provisioning step.
// git.ParseConfig refuses a duplicate repository declaration, so such a push
// could never be accepted.
func TestQuickstartNeverRedeclaresTheProvisionedRepository(t *testing.T) {
	section := quickstartSection(t, "Provision a project repository")
	if findings := repositoryRedeclarationFindings(section); len(findings) > 0 {
		t.Fatalf("the quickstart declares the provisioned repository twice: %s", strings.Join(findings, "; "))
	}
	// Prove the detector catches the exact defect: a heredoc that appends
	// a second block for the repository provision-repository just declared.
	broken := "```sh\n" +
		"podman exec server kodama git provision-repository --tenant t --storage-root s --repository demo --resource git-demo\n" +
		"```\n\n" +
		"```sh\n" +
		"cat >> conf/git.conf <<'EOF'\n\nrepo demo\n    RW+ = @admin\nEOF\n" +
		"```\n"
	if findings := repositoryRedeclarationFindings(broken); len(findings) == 0 {
		t.Fatal("the duplicate-repository detector did not catch the heredoc sample")
	}
}

// TestQuickstartDoesNotAttributeDefaultRulesToAdmin pins that the section never
// claims the default demo block grants @admin anything. `@admin` is not named
// in defaultRepositoryPolicy; the rule that lets the admin role push the policy
// repository belongs to the admin repository's own `repo admin` block.
func TestQuickstartDoesNotAttributeDefaultRulesToAdmin(t *testing.T) {
	section := quickstartSection(t, "Provision a project repository")
	writeClaim := regexp.MustCompile(`\b(write|writes|grant|grants)\b`)
	for offset, line := range strings.Split(section, "\n") {
		if !strings.Contains(line, "@admin") {
			continue
		}
		if !strings.Contains(line, "RW") && !writeClaim.MatchString(line) {
			continue
		}
		if strings.Contains(line, "admin repository") || strings.Contains(line, "repo admin") ||
			strings.Contains(line, "policy repository") || strings.Contains(line, "trusted policy ref") {
			continue
		}
		t.Fatalf("the quickstart attributes a default demo rule to @admin at line %d: %q", offset+1, strings.TrimSpace(line))
	}
}

// shellBlock is one fenced sh/bash block with the fence indentation removed.
type shellBlock struct {
	start int
	lines []string
}

var fencePattern = regexp.MustCompile("^(\\s*)```(?:sh|bash)\\s*$")
var closingFencePattern = regexp.MustCompile("^\\s*```\\s*$")

// shellBlocks extracts every fenced sh/bash block. The fence indentation is
// removed so an indented block inside a list item parses exactly as written.
func shellBlocks(text string) []shellBlock {
	lines := strings.Split(text, "\n")
	var blocks []shellBlock
	for index := 0; index < len(lines); index++ {
		match := fencePattern.FindStringSubmatch(lines[index])
		if match == nil {
			continue
		}
		indent := match[1]
		block := shellBlock{start: index + 1}
		index++
		for ; index < len(lines); index++ {
			if closingFencePattern.MatchString(lines[index]) {
				break
			}
			line := lines[index]
			if indent != "" && strings.HasPrefix(line, indent) {
				line = line[len(indent):]
			}
			block.lines = append(block.lines, line)
		}
		blocks = append(blocks, block)
	}
	return blocks
}

// Joined-assignment defect: a closing double quote in an assignment is
// followed immediately by a command word, so the shell joins the command into
// the assignment (for example IMAGE="...digest)"cat > file).
var joinedAssignmentPattern = regexp.MustCompile(`"(?:cat|git|curl|jq|podman|docker|printf|echo|sed|awk)[[:space:]]+[^[:space:]]`)

func joinedAssignmentFindings(text string) []string {
	var findings []string
	for _, block := range shellBlocks(text) {
		for offset, line := range block.lines {
			if joinedAssignmentPattern.MatchString(line) {
				findings = append(findings, fmt.Sprintf("line %d: %s", block.start+offset+1, strings.TrimSpace(line)))
			}
		}
	}
	return findings
}

var heredocOpenPattern = regexp.MustCompile(`<<-?[[:space:]]*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)

func heredocFindings(text string) []string {
	var findings []string
	for _, block := range shellBlocks(text) {
		var open []string
		for _, line := range block.lines {
			if match := heredocOpenPattern.FindStringSubmatch(line); match != nil {
				open = append(open, match[1])
				continue
			}
			if len(open) > 0 && strings.TrimSpace(line) == open[len(open)-1] {
				open = open[:len(open)-1]
			}
		}
		if len(open) > 0 {
			findings = append(findings, fmt.Sprintf("block at line %d leaves heredocs open: %s", block.start, strings.Join(open, ", ")))
		}
	}
	return findings
}

func bashParseFile(t *testing.T, path string) (bool, string) {
	t.Helper()
	bash := bashPath(t)
	process, err := os.StartProcess(bash, []string{"bash", "-n", path}, &os.ProcAttr{
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		t.Fatalf("start bash -n %s: %v", path, err)
	}
	state, err := process.Wait()
	if err != nil {
		t.Fatalf("wait bash -n %s: %v", path, err)
	}
	return state.Success(), path
}

// TestRunbookShellBlocksParse parses every fenced sh/bash block with bash -n.
func TestRunbookShellBlocksParse(t *testing.T) {
	runbook := filepath.Join(repoRoot(t), "docs", "runbooks", "podman-quickstart.md")
	text := readFile(t, runbook)
	blocks := shellBlocks(text)
	if len(blocks) < 10 {
		t.Fatalf("extracted only %d shell blocks from the runbook", len(blocks))
	}
	directory := t.TempDir()
	for index, block := range blocks {
		path := filepath.Join(directory, fmt.Sprintf("block-%02d.sh", index))
		if err := os.WriteFile(path, []byte(strings.Join(block.lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if ok, failed := bashParseFile(t, path); !ok {
			t.Fatalf("runbook shell block at line %d does not parse (bash -n %s)", block.start, failed)
		}
	}
}

// TestRunbookHeredocsAreClosed pins that every heredoc opened in a fenced block
// is closed in that same block.
func TestRunbookHeredocsAreClosed(t *testing.T) {
	text := readFile(t, filepath.Join(repoRoot(t), "docs", "runbooks", "podman-quickstart.md"))
	if findings := heredocFindings(text); len(findings) > 0 {
		t.Fatalf("unclosed heredoc: %s", strings.Join(findings, "; "))
	}
}

// TestJoinedAssignmentShapeIsDetected proves the detector catches the exact
// defect shape: a closing quote followed by a command word, with the next
// command joined onto the assignment line.
func TestJoinedAssignmentShapeIsDetected(t *testing.T) {
	broken := "```sh\n" +
		`IMAGE="docker.io/library/buildpack-deps@sha256:712f...)"cat > "$STATE/first-pipeline.yaml" <<EOF` + "\n" +
		"actions:\n" +
		"EOF\n" +
		"```\n"
	findings := joinedAssignmentFindings(broken)
	if len(findings) == 0 {
		t.Fatal("the joined-assignment detector did not catch the broken sample")
	}
	runbook := readFile(t, filepath.Join(repoRoot(t), "docs", "runbooks", "podman-quickstart.md"))
	if findings := joinedAssignmentFindings(runbook); len(findings) > 0 {
		t.Fatalf("the runbook contains a joined assignment/command line: %s", strings.Join(findings, "; "))
	}
}

// TestRunbookRelativeLinksResolve duplicates the docs checker for the
// quickstart runbook: every relative Markdown link must name an existing path.
func TestRunbookRelativeLinksResolve(t *testing.T) {
	runbook := filepath.Join(repoRoot(t), "docs", "runbooks", "podman-quickstart.md")
	text := readFile(t, runbook)
	linkPattern := regexp.MustCompile(`\]\(([^)\s]+)\)`)
	for _, match := range linkPattern.FindAllStringSubmatch(text, -1) {
		target := match[1]
		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "mailto:") {
			continue
		}
		if index := strings.IndexByte(target, '#'); index >= 0 {
			target = target[:index]
		}
		if target == "" {
			continue
		}
		path := filepath.Join(filepath.Dir(runbook), filepath.FromSlash(target))
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("broken relative link %q: %v", match[1], err)
		}
	}
}

// TestRunbookListsReleaseLimitations requires the quickstart to carry every
// release limitation, not a paraphrase that drops one.
func TestRunbookListsReleaseLimitations(t *testing.T) {
	runbook := readFile(t, filepath.Join(repoRoot(t), "docs", "runbooks", "podman-quickstart.md"))
	for _, phrase := range []string{
		"Single host",
		"local keyfile",
		"No live NATS subscriptions",
		"webhook",
		"email",
		"merge coordinator",
		"Git ref deletes",
		"retention",
		"metrics",
		"`git serve` restart",
		"AWS and GCP",
		"live-validated",
	} {
		if !strings.Contains(runbook, phrase) {
			t.Fatalf("the quickstart omits the release limitation phrase %q", phrase)
		}
	}
}

// TestRunbookCoversTheInstallSequence pins the operator sequence: install,
// migrate, bootstrap, sign-in, provision, first pipeline, logs and uninstall.
func TestRunbookCoversTheInstallSequence(t *testing.T) {
	runbook := readFile(t, filepath.Join(repoRoot(t), "docs", "runbooks", "podman-quickstart.md"))
	for _, command := range []string{
		"./deploy/podman/install.sh",
		"kodama migrate",
		"bootstrap-admin --profile test --init-keyfile",
		"scripts/oauthsonas-token",
		"git provision-tenant",
		"git serve",
		"--resource",
		"resource.binding.register",
		"pipeline.start",
		"execution.logs.read",
		"./deploy/podman/status.sh",
		"./deploy/podman/down.sh",
		"--purge",
	} {
		if !strings.Contains(runbook, command) {
			t.Fatalf("the quickstart omits %q", command)
		}
	}
}
