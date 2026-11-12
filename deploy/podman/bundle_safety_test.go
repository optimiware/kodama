package podman

// These tests pin the safety rules of the local Podman bundle: the
// documented Git credential-store snippet stores key=value lines (not a URL),
// the installer's admin header is invocation-private, the disposable smoke
// derives its identity only from KODAMA_SMOKE_* values and never reuses or
// purges another installation, a failing installation descriptor is refused
// instead of written empty, the TLS forwarders close inherited descriptors, and
// the release runbook describes the preflight and the limitations that the
// code and tests actually have. They need no Podman and no installation.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ---- the documented credential-store snippet --------------------------------

const sentinelGitToken = "quickstart-git-token-SENTINEL"

// credentialStoringBlock returns the exact runbook lines that write a
// credential-store file: from the first `creds...=` assignment through the
// `git credential-store` line. The clone commands that follow need a server and
// are not part of the credential proof.
func credentialStoringBlock(t *testing.T, block shellBlock) []string {
	t.Helper()
	store := -1
	start := -1
	for index, line := range block.lines {
		trimmed := strings.TrimSpace(line)
		if start < 0 && strings.HasPrefix(trimmed, "creds") && strings.Contains(trimmed, "=") {
			start = index
		}
		if strings.Contains(trimmed, "git credential-store") {
			store = index
			break
		}
	}
	if start < 0 || store < start {
		return nil
	}
	// Include the closing `)` of the private subshell when the block uses one.
	end := store
	for index := store + 1; index < len(block.lines); index++ {
		if strings.TrimSpace(block.lines[index]) == ")" {
			end = index
			break
		}
	}
	return block.lines[start : end+1]
}

// TestQuickstartCredentialStoreSnippetWorks proves the two documented
// credential-store steps really store the installation token. git
// credential-store reads key=value lines; a piped URL is refused with exit 128,
// so a URL form could never authenticate a clone.
func TestQuickstartCredentialStoreSnippetWorks(t *testing.T) {
	text := readFile(t, filepath.Join(repoRoot(t), "docs", "runbooks", "podman-quickstart.md"))
	var snippets [][]string
	for _, block := range shellBlocks(text) {
		if snippet := credentialStoringBlock(t, block); snippet != nil {
			snippets = append(snippets, snippet)
		}
	}
	if len(snippets) != 2 {
		t.Fatalf("the quickstart has %d credential-store blocks; want the admin and engineer examples", len(snippets))
	}
	if _, err := gitPath(); err != nil {
		t.Skip("git is not installed")
	}
	for index, snippet := range snippets {
		body := strings.Join(snippet, "\n")
		if !strings.Contains(body, "protocol=https") || !strings.Contains(body, "username=") ||
			!strings.Contains(body, "password=") {
			t.Fatalf("credential-store block %d does not send key=value credential lines:\n%s", index+1, body)
		}
		if regexp.MustCompile(`https://[^\s'"]*@`).MatchString(body) {
			t.Fatalf("credential-store block %d still pipes a URL instead of key=value lines:\n%s", index+1, body)
		}
		tokenMatch := regexp.MustCompile(`cat "\$STATE/secrets/([A-Za-z0-9._-]+)"`).FindStringSubmatch(body)
		preamble := ""
		tokenName := ""
		if tokenMatch != nil {
			tokenName = tokenMatch[1]
		} else {
			variableMatch := regexp.MustCompile(`cat "\$([A-Za-z_][A-Za-z0-9_]*)"`).FindStringSubmatch(body)
			if variableMatch == nil {
				t.Fatalf("credential-store block %d does not read a token file:\n%s", index+1, body)
			}
			tokenName = variableMatch[1] + "-token"
			preamble = variableMatch[1] + `="$STATE/secrets/` + tokenName + "\"\n"
		}
		helper := regexp.MustCompile(`--file "\$(creds[A-Za-z_]*)"`).FindStringSubmatch(body)
		if helper == nil {
			t.Fatalf("credential-store block %d does not use a credential file variable:\n%s", index+1, body)
		}
		state := t.TempDir()
		tokenPath := filepath.Join(state, "secrets", tokenName)
		if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tokenPath, []byte(sentinelGitToken+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		script := "set -euo pipefail\nexport STATE=" + shellQuote(state) + "\n" + preamble + body + "\n"
		stdout, stderr, code := runBash(t, map[string]string{"HOME": t.TempDir()}, script)
		if code != 0 {
			t.Fatalf("credential-store block %d failed (exit %d): %s %s\n%s", index+1, code, stdout, stderr, body)
		}
		fill := script +
			"\nprintf 'url=https://localhost:18445/admin\\n' | git -c credential.helper=\"store --file=$" +
			helper[1] + "\" credential fill\n"
		out, errOut, code := runBash(t, map[string]string{"HOME": t.TempDir()}, fill)
		if code != 0 {
			t.Fatalf("credential fill after block %d failed: %s", index+1, errOut)
		}
		if !strings.Contains(out, "password="+sentinelGitToken) {
			t.Fatalf("credential fill after block %d did not return the stored token: %s", index+1, out)
		}
	}
}

// gitPath reports whether the real git binary is usable for the snippet test.
func gitPath() (string, error) {
	for _, candidate := range []string{"/usr/bin/git", "/bin/git", "/usr/local/bin/git"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", os.ErrNotExist
}

// shellQuote quotes one value for the generated bash scripts.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// TestQuickstartDoesNotChangeDirectoryForTheInstall pins that the provisioning
// walk-through never leaves the repository root before a relative
// `./deploy/podman/install.sh`.
func TestQuickstartDoesNotChangeDirectoryForTheInstall(t *testing.T) {
	section := quickstartSection(t, "Provision a project repository")
	for offset, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "cd ") || strings.HasPrefix(trimmed, "cd\t") {
			t.Fatalf("the provisioning section changes directory at line %d: %q", offset+1, trimmed)
		}
	}
	if !strings.Contains(section, "./deploy/podman/install.sh") {
		t.Fatal("the provisioning section does not re-run the install from the repository root")
	}
}

// ---- the installer's admin header is invocation-private ---------------------

const headerTrapScript = `set -euo pipefail
. "$1"
shift
operator="$1"
outcome="$2"
mkdir -p "$(dirname "$operator")"
printf 'operator-header-SENTINEL\n' >"$operator"
chmod 600 "$operator"
header_dir="$(installer_header_dir)"
header_file="$header_dir/admin-header"
printf 'Authorization: Bearer installer\n' >"$header_file"
chmod 600 "$header_file"
[ "$(stat -c '%a' "$header_dir")" = "700" ] || { echo "header dir mode $(stat -c '%a' "$header_dir")" >&2; exit 1; }
[ "$(stat -c '%a' "$header_file")" = "600" ] || { echo "header file mode $(stat -c '%a' "$header_file")" >&2; exit 1; }
cleanup() {
	installer_header_cleanup "$header_dir"
}
trap cleanup EXIT
if [ "$outcome" = fail ]; then
	printf 'simulated install failure\n' >&2
	exit 7
fi
printf 'simulated install success\n'`

func TestInstallerHeaderIsInvocationPrivate(t *testing.T) {
	lib := filepath.Join(bundleRoot(t), "lib.sh")
	for _, outcome := range []string{"success", "fail"} {
		env := shellEnv(t, "header-test")
		state := env["KODAMA_LOCAL_STATE_DIR"]
		if err := os.MkdirAll(filepath.Join(state, "run"), 0o700); err != nil {
			t.Fatal(err)
		}
		operator := filepath.Join(state, "secrets", "admin-header")
		stdout, stderr, code := runBash(t, env, headerTrapScript, lib, operator, outcome)
		if outcome == "success" && code != 0 {
			t.Fatalf("the simulated successful install failed: %s %s", stdout, stderr)
		}
		if outcome == "fail" && code == 0 {
			t.Fatal("the simulated failing install exited zero")
		}
		data, err := os.ReadFile(operator)
		if err != nil {
			t.Fatalf("the operator header did not survive the %s path: %v", outcome, err)
		}
		if string(data) != "operator-header-SENTINEL\n" {
			t.Fatalf("the operator header was rewritten on the %s path: %q", outcome, data)
		}
		matches, err := filepath.Glob(filepath.Join(state, "run", "header.*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) > 0 {
			t.Fatalf("the %s path left the private header directory behind: %v", outcome, matches)
		}
	}
}

func TestInstallScriptUsesItsOwnHeader(t *testing.T) {
	install := readFile(t, filepath.Join(bundleRoot(t), "install.sh"))
	if strings.Contains(install, `>"$admin_header_file"`) || strings.Contains(install, `rm -f -- "$admin_header_file"`) {
		t.Fatal("install.sh still writes or removes the operator's secrets/admin-header")
	}
	for _, phrase := range []string{"installer_header_dir", "installer_header_cleanup", `--header @"$installer_header_file"`} {
		if !strings.Contains(install, phrase) {
			t.Fatalf("install.sh does not use its invocation-private header: missing %q", phrase)
		}
	}
	runbook := readFile(t, filepath.Join(repoRoot(t), "docs", "runbooks", "podman-quickstart.md"))
	for _, phrase := range []string{"private header", "Regenerate your header", "never reads or removes"} {
		if !strings.Contains(runbook, phrase) {
			t.Fatalf("the quickstart does not explain the installer header lifetime: missing %q", phrase)
		}
	}
}

// ---- the disposable smoke owns its identity ---------------------------------

// podmanStub installs a fake podman on PATH and returns the PATH value plus the
// file the stub appends its arguments to.
func podmanStub(t *testing.T, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "podman.log")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>" + shellQuote(log) + "\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "podman"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir + ":" + os.Getenv("PATH"), log
}

func smokeEnv(t *testing.T, run string) map[string]string {
	t.Helper()
	root := t.TempDir()
	return map[string]string{
		"HOME":                             root,
		"KODAMA_SLOT":                      "stack-1",
		"KODAMA_SMOKE_RUN":                 run,
		"KODAMA_SMOKE_STATE_DIR":           filepath.Join(root, "nested", "state"),
		"Q01_EVIDENCE_DIR":                 filepath.Join(root, "evidence"),
		"KODAMA_LOCAL_HTTPS_PORT":          "27443",
		"KODAMA_LOCAL_OIDC_PORT":           "27444",
		"KODAMA_LOCAL_GIT_PORT":            "27081",
		"KODAMA_LOCAL_GIT_HTTPS_PORT":      "27445",
		"KODAMA_LOCAL_POSTGRES_PORT":       "25432",
		"KODAMA_LOCAL_NATS_PORT":           "24222",
		"KODAMA_LOCAL_NATS_MONITOR_PORT":   "28222",
		"KODAMA_LOCAL_OAUTH_UPSTREAM_PORT": "28181",
	}
}

func TestSmokeRequiresAnEmptyStateDirectory(t *testing.T) {
	lib := filepath.Join(bundleRoot(t), "lib.sh")
	env := shellEnv(t, "fresh-state")
	script := sourceLibScript(`require_fresh_state_dir "$1" "$2"`)
	root := t.TempDir()
	owned := filepath.Join(root, "owned")
	if err := os.Mkdir(owned, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owned, ".kodama-local"), []byte("prefix=fresh-state\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runBash(t, env, script, lib, owned, "fresh-state"); code == 0 {
		t.Fatal("a non-empty state directory was reused because its marker matched the prefix")
	}
	empty := filepath.Join(root, "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runBash(t, env, script, lib, empty, "fresh-state"); code != 0 {
		t.Fatal("an empty state directory was refused")
	}
	if _, _, code := runBash(t, env, script, lib, filepath.Join(root, "absent"), "fresh-state"); code != 0 {
		t.Fatal("an absent state directory was refused")
	}
}

func TestSmokeRefusesExistingPodmanResourcesBeforeInstalling(t *testing.T) {
	lib := filepath.Join(bundleRoot(t), "lib.sh")
	script := sourceLibScript(`smoke_require_no_podman_resources "$1"`)
	cases := []struct {
		name string
		body string
		ok   bool
		want string
	}{
		{"clean", "case \"$1\" in ps) exit 0;; volume) exit 1;; esac\nexit 0", true, ""},
		{"container", "case \"$1\" in ps) echo smoke-fix1-server; exit 0;; volume) exit 1;; esac\nexit 0", false, "smoke-fix1-server"},
		{"pgdata volume", "case \"$1\" in ps) exit 0;; volume) [ \"$3\" = smoke-fix1-pgdata ] && exit 0; exit 1;; esac\nexit 0", false, "smoke-fix1-pgdata"},
		{"natsdata volume", "case \"$1\" in ps) exit 0;; volume) [ \"$3\" = smoke-fix1-natsdata ] && exit 0; exit 1;; esac\nexit 0", false, "smoke-fix1-natsdata"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			pathValue, _ := podmanStub(t, testCase.body)
			env := shellEnv(t, "smoke-fix1")
			env["PATH"] = pathValue
			out, errOut, code := runBash(t, env, script, lib, "smoke-fix1")
			if testCase.ok {
				if code != 0 {
					t.Fatalf("a clean prefix was refused: %s %s", out, errOut)
				}
				return
			}
			if code == 0 {
				t.Fatal("an existing Podman resource was accepted")
			}
			if !strings.Contains(out+errOut, testCase.want) {
				t.Fatalf("the refusal does not name %q: %s %s", testCase.want, out, errOut)
			}
		})
	}
}

func TestSmokeCleanupOnlyPurgesItsOwnRun(t *testing.T) {
	lib := filepath.Join(bundleRoot(t), "lib.sh")
	env := shellEnv(t, "smoke-owned")
	write := sourceLibScript(`write_smoke_marker "$1" "$2"`)
	allowed := sourceLibScript(`smoke_cleanup_allowed "$1" "$2" "$3"`)
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, ".kodama-local"), []byte("prefix=smoke-owned\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := runBash(t, env, write, lib, state, "run-a"); code != 0 {
		t.Fatalf("writing the smoke marker failed: %s", errOut)
	}
	if _, _, code := runBash(t, env, allowed, lib, state, "run-a", "smoke-owned"); code != 0 {
		t.Fatal("the run that recorded the state was not allowed to purge it")
	}
	if _, _, code := runBash(t, env, allowed, lib, state, "run-b", "smoke-owned"); code == 0 {
		t.Fatal("a different run was allowed to purge this run's state")
	}
	unmarked := filepath.Join(root, "unmarked")
	if err := os.MkdirAll(unmarked, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runBash(t, env, allowed, lib, unmarked, "run-a", "smoke-owned"); code == 0 {
		t.Fatal("a state directory without this run's marker was purgeable")
	}
	foreign := filepath.Join(root, "foreign")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, ".kodama-local"), []byte("prefix=other-install\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := runBash(t, env, write, lib, foreign, "run-a"); code != 0 {
		t.Fatalf("writing a marker next to a foreign state failed: %s", errOut)
	}
	if _, _, code := runBash(t, env, allowed, lib, foreign, "run-a", "smoke-owned"); code == 0 {
		t.Fatal("a foreign state directory was purgeable")
	}
}

// TestSmokeIgnoresInheritedOperatorIdentity proves an exported operator
// environment (KODAMA_LOCAL_PREFIX/STATE_DIR/GIT_RESOURCES from a sourced
// secrets/operator.env) cannot steer the disposable run into another
// installation's prefix or state.
func TestSmokeIgnoresInheritedOperatorIdentity(t *testing.T) {
	smoke := filepath.Join(bundleRoot(t), "smoke.sh")
	env := smokeEnv(t, "conta")
	sentinelState := filepath.Join(t.TempDir(), "real-install")
	if err := os.MkdirAll(sentinelState, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sentinelState, ".kodama-local"), []byte("prefix=real-install\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(sentinelState, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env["KODAMA_LOCAL_PREFIX"] = "real-install"
	env["KODAMA_LOCAL_STATE_DIR"] = sentinelState
	env["KODAMA_LOCAL_GIT_RESOURCES"] = "admin=git-admin,operator=git-operator"
	pathValue, log := podmanStub(t, "case \"$1\" in ps) exit 0;; volume) exit 1;; info) echo 'cannot use podman' >&2; exit 1;; esac\nexit 1")
	env["PATH"] = pathValue

	_, errOut, code := runBash(t, env, `bash "$1"`, smoke)
	if code == 0 {
		t.Fatal("smoke.sh succeeded against a fake podman")
	}
	if strings.Contains(errOut, sentinelState) {
		t.Fatalf("smoke.sh used the inherited operator state directory:\n%s", errOut)
	}
	if !strings.Contains(errOut, env["KODAMA_SMOKE_STATE_DIR"]) {
		t.Fatalf("smoke.sh did not name its own state directory in its failure report:\n%s", errOut)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "keep me\n" {
		t.Fatalf("the unrelated installation's sentinel did not survive: %v %q", err, data)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "ps -a --filter label=kodama.local.prefix=smoke-conta") {
		t.Fatalf("smoke.sh did not scope its container check to its own prefix:\n%s", calls)
	}
	if strings.Contains(string(calls), "real-install") {
		t.Fatalf("smoke.sh passed the inherited prefix to podman:\n%s", calls)
	}
}

// TestSmokeDerivesIdentityOnlyFromSmokeValues pins the documented overrides and
// the identity rule in the script and the runbooks.
func TestSmokeDerivesIdentityOnlyFromSmokeValues(t *testing.T) {
	smoke := readFile(t, filepath.Join(bundleRoot(t), "smoke.sh"))
	for _, phrase := range []string{
		"KODAMA_SMOKE_PREFIX",
		"KODAMA_SMOKE_STATE_DIR",
		"KODAMA_LOCAL_GIT_RESOURCES",
		"unset ",
		"smoke_require_no_podman_resources",
		"smoke_cleanup_allowed",
		"write_smoke_marker",
	} {
		if !strings.Contains(smoke, phrase) {
			t.Fatalf("smoke.sh omits %q", phrase)
		}
	}
	if strings.Contains(smoke, `export KODAMA_LOCAL_PREFIX="${KODAMA_LOCAL_PREFIX:-`) {
		t.Fatal("smoke.sh still inherits KODAMA_LOCAL_PREFIX")
	}
	if strings.Contains(smoke, `export KODAMA_LOCAL_STATE_DIR="${KODAMA_LOCAL_STATE_DIR:-`) {
		t.Fatal("smoke.sh still inherits KODAMA_LOCAL_STATE_DIR")
	}
	runbook := readFile(t, filepath.Join(repoRoot(t), "docs", "runbooks", "podman-quickstart.md"))
	flat := strings.Join(strings.Fields(runbook), " ")
	for _, phrase := range []string{"KODAMA_SMOKE_PREFIX", "KODAMA_SMOKE_STATE_DIR", "never inherits"} {
		if !strings.Contains(flat, phrase) {
			t.Fatalf("the quickstart does not document the smoke identity rule: missing %q", phrase)
		}
	}
	// The documented smoke example must not use a real install's default ports.
	section := quickstartSection(t, "Smoke test")
	for _, port := range []string{"18443", "18444", "18081", "18445", "15432", "14222", "18222", "18181"} {
		if regexp.MustCompile(`[=:]` + port + `\b`).MatchString(section) {
			t.Fatalf("the documented smoke example uses the real install default port %s", port)
		}
	}
}

// ---- a failing descriptor generator is refused ------------------------------

func TestInstallationDescriptorWriteChecksTheGenerator(t *testing.T) {
	lib := filepath.Join(bundleRoot(t), "lib.sh")
	env := shellEnv(t, "descriptor-fault")
	state := env["KODAMA_LOCAL_STATE_DIR"]
	if err := os.MkdirAll(filepath.Join(state, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(state, "run", "installation.json")
	if err := os.WriteFile(target, []byte("{\"previous\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		body string
	}{
		{"failing generator", `installation_descriptor_json() { return 1; }
if write_installation_descriptor "$1"; then exit 0; else exit 1; fi`},
		{"empty generator", `installation_descriptor_json() { :; }
if write_installation_descriptor "$1"; then exit 0; else exit 1; fi`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			script := sourceLibScript(testCase.body)
			if _, errOut, code := runBash(t, env, script, lib, target); code == 0 {
				t.Fatalf("a %s was written as the installation descriptor: %s", testCase.name, errOut)
			}
			data, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "{\"previous\":true}\n" {
				t.Fatalf("the existing descriptor was disturbed: %q", data)
			}
		})
	}
	install := readFile(t, filepath.Join(bundleRoot(t), "install.sh"))
	if strings.Contains(install, `write_private_file "$installation_file" "$(installation_descriptor_json`) ||
		strings.Contains(install, "$(installation_descriptor_json") {
		t.Fatal("install.sh still masks a failing descriptor generator behind a command substitution")
	}
	if !strings.Contains(install, `write_installation_descriptor "$installation_file"`) {
		t.Fatal("install.sh does not check the descriptor generator before writing")
	}
	writeIndex := strings.Index(install, "write_installation_descriptor")
	readyIndex := strings.Index(install, "installed and ready")
	if writeIndex < 0 || readyIndex < 0 || writeIndex > readyIndex {
		t.Fatal("install.sh prints the success banner before writing the descriptor")
	}
}

// ---- forwarders do not inherit slot descriptors -----------------------------

func TestCloseInheritedFileDescriptors(t *testing.T) {
	lib := filepath.Join(bundleRoot(t), "lib.sh")
	env := shellEnv(t, "fd-test")
	script := `set -euo pipefail
. "$1"
shift
lock="$1"
exec 10>>"$lock"
flock -n 10
(
	close_inherited_fds
	if [ -e /proc/self/fd/10 ]; then
		echo "fd 10 survived" >&2
		exit 1
	fi
	exec sleep 20
) &
pid=$!
for attempt in $(seq 1 200); do
	[ -e "/proc/$pid" ] || { echo "forwarder stand-in did not start" >&2; exit 1; }
	case "$(tr '\0' ' ' </proc/$pid/cmdline 2>/dev/null)" in
	sleep*) break ;;
	esac
	if [ -e "/proc/$pid/fd/10" ]; then
		echo "the running forwarder inherited fd 10" >&2
		kill "$pid" 2>/dev/null || true
		exit 1
	fi
	sleep 0.05
done
if [ -e "/proc/$pid/fd/10" ]; then
	echo "the running forwarder inherited fd 10" >&2
	kill "$pid" 2>/dev/null || true
	exit 1
fi
kill "$pid" 2>/dev/null || true
wait "$pid" 2>/dev/null || true
printf 'closed\n'`
	lock := filepath.Join(t.TempDir(), "slot.lock")
	out, errOut, code := runBash(t, env, script, lib, lock)
	if code != 0 {
		t.Fatalf("a forwarder kept an inherited descriptor: %s %s", out, errOut)
	}
	if !strings.Contains(out, "closed") {
		t.Fatalf("the descriptor test did not complete: %s %s", out, errOut)
	}
	install := readFile(t, filepath.Join(bundleRoot(t), "install.sh"))
	if !strings.Contains(install, "close_inherited_fds") {
		t.Fatal("install.sh does not close inherited descriptors before starting a forwarder")
	}
	if !regexp.MustCompile(`(?s)close_inherited_fds.*?exec socat`).MatchString(install) {
		t.Fatal("install.sh does not close the descriptors in the forwarder's own process before exec")
	}
}

// ---- the release runbook matches the preflight test -------------------------

func TestReleaseSmokePreflightMatchesTheTest(t *testing.T) {
	runbook := readFile(t, filepath.Join(repoRoot(t), "docs", "runbooks", "release-smoke.md"))
	preflight := runbook
	if start := strings.Index(runbook, "## Preflight"); start >= 0 {
		preflight = runbook[start:]
		if next := regexp.MustCompile(`\n## `).FindStringIndex(preflight[1:]); next != nil {
			preflight = preflight[:next[0]+1]
		}
	}
	for _, wrong := range []string{"read through the public `membership.list`", "engineer cannot\n   list memberships", "engineer cannot list memberships"} {
		if strings.Contains(preflight, wrong) {
			t.Fatalf("the preflight section still claims %q", wrong)
		}
	}
	for _, phrase := range []string{"resource.binding.read", "execution.list", "pipeline.read", "public `forbidden`"} {
		if !strings.Contains(preflight, phrase) {
			t.Fatalf("the preflight section does not describe the real refusal check %q", phrase)
		}
	}
	if strings.Contains(runbook, "not claimed here") {
		t.Fatal("the runbook still carries the stale 'not claimed here' text")
	}
	source := readFile(t, filepath.Join(repoRoot(t), "test", "e2e", "installation_access_test.go"))
	if strings.Contains(source, "declared public secret") {
		t.Fatal("the installation access test still claims a declared public secret")
	}
}

// TestReleaseSmokeScenarioListMatchesTheSmokeSelection proves the runbook and
// the wrapper name the same 12-test release selection.
func TestReleaseSmokeScenarioListMatchesTheSmokeSelection(t *testing.T) {
	smoke := readFile(t, filepath.Join(bundleRoot(t), "smoke.sh"))
	// The wrapper builds one expected-name array for the release selection and
	// adds the preflight's single expected name on its own line.
	arrayBlock := regexp.MustCompile(`(?s)expected=\((.*?)\n\)`).FindStringSubmatch(smoke)
	if arrayBlock == nil {
		t.Fatal("smoke.sh has no expected-name array")
	}
	expected := regexp.MustCompile(`(?m)^\s*(Test[A-Za-z0-9_/]+)$`).FindAllStringSubmatch(arrayBlock[1], -1)
	want := map[string]bool{
		"TestE2E_AC01InstalledPipeline": true, "TestE2E_AC02ImmutableAdmission": true,
		"TestE2E_AC09CleanupAttributable": true, "TestE2E_AC11Materialization": true,
		"TestE2E_AC12Outputs": true, "TestE2E_AC13OrderedActions": true, "TestE2E_AC16PlannedWait": true,
		"TestReleaseSmoke/RequestWait": true, "TestReleaseSmoke/OperatorRetry": true,
		"TestReleaseSmoke/ConversationHandoff": true, "TestReleaseSmoke/ProtectedTask": true,
		"TestReleaseSmoke/HumanPush": true,
	}
	if len(expected) != len(want) {
		t.Fatalf("smoke.sh expects %d names; want %d", len(expected), len(want))
	}
	for _, match := range expected {
		if !want[match[1]] {
			t.Fatalf("smoke.sh expects an unexpected test %q", match[1])
		}
	}
	runbook := readFile(t, filepath.Join(repoRoot(t), "docs", "runbooks", "release-smoke.md"))
	for name := range want {
		if !strings.Contains(runbook, "`"+name+"`") && !strings.Contains(runbook, "--expect "+name) {
			t.Fatalf("the release-smoke runbook does not name the selected test %s", name)
		}
	}
	if !strings.Contains(runbook, "12 selected") {
		t.Fatal("the release-smoke runbook does not state the 12-test default selection")
	}
}

// ---- verified release limitations -------------------------------------------

func TestRunbookDocumentsVerifiedLimitations(t *testing.T) {
	runbook := readFile(t, filepath.Join(repoRoot(t), "docs", "runbooks", "podman-quickstart.md"))
	for _, phrase := range []string{
		"0.0.0.0",
		"firewall",
		"deny-by-default",
		"not configurable",
		"`/workspace`", "Retained logs are collected", "submit declared outputs over HTTPS",
	} {
		if !strings.Contains(runbook, phrase) {
			t.Fatalf("the quickstart limitations do not document %q", phrase)
		}
	}
	for _, wrong := range []string{"has no CA material to submit an output", "no CA material"} {
		if strings.Contains(runbook, wrong) {
			t.Fatalf("the quickstart still claims %q", wrong)
		}
	}
}

// ---- the preflight uses the verifying wrapper -------------------------------

func TestSmokePreflightSelectionGoesThroughTheVerifier(t *testing.T) {
	smoke := filepath.Join(bundleRoot(t), "smoke.sh")
	env := smokeEnv(t, "select")
	for key := range env {
		if strings.HasPrefix(key, "KODAMA_LOCAL_") && strings.HasSuffix(key, "_PORT") {
			delete(env, key)
		}
	}
	delete(env, "KODAMA_SLOT")
	stdout, stderr, code := runBash(t, env, `bash "$1" --print-selection --preflight`, smoke)
	if code != 0 {
		t.Fatalf("printing the preflight selection failed: %s %s", stdout, stderr)
	}
	for _, phrase := range []string{
		"verify-go-test.sh",
		"--expect\nTestE2E_InstallationAccess\n",
		"-json",
		"-run\n^TestE2E_InstallationAccess$",
		"selection=preflight",
	} {
		if !strings.Contains(stdout, phrase) {
			t.Fatalf("the preflight selection omits %q:\n%s", phrase, stdout)
		}
	}
	if strings.Contains(stdout, "TestE2E_AC01InstalledPipeline") {
		t.Fatalf("the preflight selection also selects the release tests:\n%s", stdout)
	}

	stdout, stderr, code = runBash(t, env, `bash "$1" --print-selection`, smoke)
	if code != 0 {
		t.Fatalf("printing the full selection failed: %s %s", stdout, stderr)
	}
	for _, name := range []string{
		"TestE2E_AC01InstalledPipeline", "TestE2E_AC02ImmutableAdmission", "TestE2E_AC09CleanupAttributable",
		"TestE2E_AC11Materialization", "TestE2E_AC12Outputs", "TestE2E_AC13OrderedActions", "TestE2E_AC16PlannedWait",
		"TestReleaseSmoke/RequestWait", "TestReleaseSmoke/OperatorRetry", "TestReleaseSmoke/ConversationHandoff",
		"TestReleaseSmoke/ProtectedTask", "TestReleaseSmoke/HumanPush",
	} {
		if !strings.Contains(stdout, "--expect\n"+name+"\n") {
			t.Fatalf("the full selection omits %s:\n%s", name, stdout)
		}
	}
	if !strings.Contains(stdout, "selection=full") {
		t.Fatalf("the full selection is not marked full:\n%s", stdout)
	}

	narrowed := smokeEnv(t, "narrow")
	narrowed["KODAMA_SMOKE_TEST_RUN"] = "^$"
	narrowed["KODAMA_SMOKE_EXPECT"] = "TestE2E_AC01InstalledPipeline"
	stdout, stderr, code = runBash(t, narrowed, `bash "$1" --print-selection`, smoke)
	if code != 0 {
		t.Fatalf("printing the narrowed selection failed: %s %s", stdout, stderr)
	}
	if !strings.Contains(stdout, "selection=narrowed") {
		t.Fatalf("a narrowed selection is not marked non-release:\n%s", stdout)
	}
	if !strings.Contains(stdout, "verify-go-test.sh") || !strings.Contains(stdout, "--expect\nTestE2E_AC01InstalledPipeline\n") {
		t.Fatalf("a narrowed selection bypasses the verifier:\n%s", stdout)
	}

	// A '^$' selection can therefore never exit zero as release evidence: the
	// wrapper always runs the verifier, and the verifier fails a zero-test run.
	verifier := filepath.Join(repoRoot(t), "test", "e2e", "internal", "harness", "verify-go-test.sh")
	result := filepath.Join(t.TempDir(), "go-test.json")
	if err := os.WriteFile(result, []byte(`{"Action":"pass","Package":"github.com/optimiware/kodama/test/e2e"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := runBash(t, map[string]string{"HOME": t.TempDir()},
		`exec bash "$1" --expect TestE2E_AC01InstalledPipeline -- cat "$2"`, verifier, result); code == 0 {
		t.Fatalf("the verifier accepted a zero-test success: %s", errOut)
	}
}

// TestSmokeSelectionPrintIsDiagnosticsOnly pins that printing a selection
// starts nothing.
func TestSmokeSelectionPrintIsDiagnosticsOnly(t *testing.T) {
	smoke := readFile(t, filepath.Join(bundleRoot(t), "smoke.sh"))
	printIndex := strings.Index(smoke, "--print-selection")
	installIndex := strings.Index(smoke, `log "installing the disposable installation`)
	if printIndex < 0 || installIndex < 0 || printIndex > installIndex {
		t.Fatal("smoke.sh does not handle --print-selection before it installs anything")
	}
}
